# Port forwarding (SSH `-L` style) — design

Date: 2026-06-02

## Problem

When an agent runs a server inside its task worktree (e.g. a dev server
on `127.0.0.1:3000` on the runner host), there is no way to reach it from
the client machine. We want SSH `-L` style local port forwarding: a
client-side TCP listener whose connections are tunnelled, via the server,
to a `host:port` dialled from the runner side.

`-R` (remote forward: listener on the runner, dial on the client) is
explicitly wanted later but out of scope for the first implementation.
The schema must not bake in `-L`-only assumptions.

## Approach decision

Bytes are carried over **server-relayed `trsf` bidirectional streams**,
reusing the proven file-transfer skeleton (`OpenFileTransferRequest` /
`RunnerOpenFileTransferRequest` two-stage request + `relayBytes`
bidirectional splice).

Rejected alternative: end-to-end `client↔runner peer.Conn` via
`SetProxy` / `EstablishRelay` (the "Phase B agent proxy" path). Its only
real advantage — the server never sees forwarded plaintext — buys little
here because the server is a trusted hub that already handles task logs,
file contents and PTY streams in plaintext. Its costs are real: the
ceremony is currently **agent-identity gated** (only a process holding a
valid `TaskID` can run `ProxyRequest`), so a plain `harness-cli` client
could not establish it without first un-gating client→runner peer.Conn;
and setup is 5–6 round trips (+ chained-relay hops). The server-relayed
path matches the `-L` topology (client-side listener) directly and needs
no `objproto` / `peer` changes.

Note: both paths ultimately move bytes over `trsf` bidirectional
streams, which are reliable / in-order per stream (QUIC-like). File
transfer already proves raw byte relay over them works; the
"`trsf` is datagram-only / not TCP-suitable" claim is false.

Dial target policy: **arbitrary `host:port` specified by the client**
(SSH `-L` equivalent). The server is trusted and this is an individual
dogfood tool, so the runner-as-stepping-stone exposure is accepted.

## CLI surface

```
harness-cli forward <task-id> -L [bind:]localport:remotehost:remoteport [-L ...]
```

- SSH `-L` syntax. `-L` repeatable for multiple forwards.
- Foreground: holds the listeners until `Ctrl-C`, then tears everything
  down.
- `bind` omitted ⇒ listen on `127.0.0.1` (do not expose the local port
  externally by default).
- Example: `harness-cli forward <id> -L 3000:127.0.0.1:3000` →
  `localhost:3000` on the client reaches `127.0.0.1:3000` on the runner
  host (the agent's dev server).

## Protocol / schema additions

All in `runner/protocol/message.bgn` (single source of truth, one place;
direction carried as a field so `-R` is a later addition, not a reshape):

- `OpenPortForwardRequest { TaskId, Direction (enum: Local|Remote),
  RemoteHost string, RemotePort uint16 }` — client→server, via
  `TaskControl`.
- `OpenPortForwardResponse { Status (enum: Ok|TaskNotFound|TaskTerminal|
  Rejected), StreamId }` — server→client.
- `RunnerOpenPortForwardRequest { TaskId, StreamId, Direction,
  RemoteHost, RemotePort }` — server→runner.

`Direction` ships with `Local` implemented; `Remote` is a reserved enum
value (no handler yet). Same two-stage shape as the `file` family.

## Data flow (one accepted TCP connection = one stream pair)

1. The client's local listener accepts a TCP connection.
2. Client → server: `OpenPortForwardRequest`. The server allocates a
   stream pair (one toward the client, one toward the runner), returns
   `OpenPortForwardResponse{Ok, StreamId}`, and sends
   `RunnerOpenPortForwardRequest` to the runner.
3. The server splices the two streams bidirectionally with the same
   `relayBytes` mechanism file transfer uses.
4. The runner `net.Dial("tcp", RemoteHost:RemotePort)`. On success it
   splices its stream ↔ the dialled `net.Conn`; **on failure it closes
   the stream**.
5. The client splices its accepted TCP conn ↔ its stream.

`Ok` means "relay wired", not "target reachable". A dial failure closes
the runner stream → the server splice propagates EOF → the client closes
the accepted TCP conn (matches TCP behaviour when a backend is down). No
ack frame is added, so there is no extra round trip per connection.

## Lifecycle / error handling

- A forward requires `TaskId` to be **non-terminal** (runner process
  alive). If terminal, `OpenPortForwardResponse` carries an error status;
  the client closes that one accepted conn, keeps the listener up, logs
  it.
- Per-connection errors (dial failure, relay error) tear down **only
  that one connection**; never propagate to siblings or the listener.
- TCP half-close is honoured: a one-directional EOF is propagated to the
  other direction before both ends close.
- Known characteristic: each new connection costs one client→server RPC
  round trip (in-flight transfers run at full speed). A later
  optimisation — establish a forward *session* once, then a lightweight
  per-connection open frame — is left open. Not built now (YAGNI).

## Shared forward engine

The forward engine (parse the `-L` spec, listen, accept-loop, per-conn
open + bidirectional splice) lives in the `cli` package as a helper that
takes a `*cli.Client`, so both the `harness-cli forward` binary command
and the TUI call the **same** code over a long-lived client (per the
"reuse long-lived `*cli.Client`" convention — no re-dial per forward).

- The binary runs it in the foreground; `Ctrl-C` cancels its context and
  tears down.
- The TUI runs it in a background goroutine with a cancel context (see
  below).

## TUI surface

- **Start key**: `p` in tasks focus opens a `PortForwardModal`
  (`textinput` for the `-L localport:remotehost:remoteport` spec),
  targeting the selected task. Submit (Enter / Ctrl+J) starts it; `Esc`
  cancels. Follows the existing popup modal pattern (Open/Close gate +
  key interception in `App.Update`).
- **Execution**: NOT `tea.Exec` (that suspends the TUI). A
  `DoStartPortForward(a.client, taskID, spec)` Cmd spawns a background
  goroutine running the shared forward engine, so the TUI stays usable.
  Active forwards are tracked in `a.activeForwards
  map[...]*PortForwardSession`, each holding its `context.CancelFunc`.
- **Stop key**: `P` in tasks focus cancels the selected task's forward
  and removes it from the map.
- **Status**: start / stop / per-connection errors are appended to the
  `cmdresult` panel (`OKStyle` / `ErrorStyle`) via status Msgs sent back
  to the program. A dedicated "active forwards" panel is deferred
  (YAGNI); `cmdresult` lines suffice initially.

## Components / files

- `runner/protocol/message.bgn` — new messages above; regenerate
  `message.go`.
- `cli/port_forward.go` — shared forward engine (`*cli.Client` based):
  spec parse, listener, accept loop, per-conn open + splice.
- `cli/forward.go` (binary command) — wires the `forward` subcommand to
  the engine, foreground, SIGINT teardown.
- `server` — `handleOpenPortForward` mirroring `handleOpenFileTransfer`:
  allocate the stream pair, send `RunnerOpenPortForwardRequest`, start
  the bidirectional `relayBytes` splice.
- `runner` — `handleOpenPortForward`: validate, `net.Dial` the target,
  splice stream ↔ conn, close stream on dial failure.
- `tui/portforward.go` — `PortForwardModal` + `PortForwardSession` +
  `DoStartPortForward` Cmd + status Msgs; key wiring in `tui/app.go`.

## Testing

- Unit: `-L` spec parser (`bind:lport:rhost:rport` and `lport:rhost:rport`
  forms; reject malformed).
- Integration: bring up server + runner + a task, stand up an echo TCP
  server the runner can reach, forward through it and assert:
  - byte round-trip through the local port,
  - multiple concurrent connections are independent,
  - dial failure (closed target port) closes the local accepted conn
    without killing the listener,
  - non-terminal-task requirement (terminal task ⇒ error status).
