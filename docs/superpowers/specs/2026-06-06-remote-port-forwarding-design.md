# Remote port forwarding (ssh -R equivalent) — design

Status: approved (2026-06-06). Scope: CLI + TUI.

## Problem

The harness has **local** port forwarding (`ssh -L` equivalent): the client
listens on a local port and, per accepted connection, the runner dials a
target reachable from the runner side. There is no **remote** forward
(`ssh -R` equivalent): the runner listens on a port and, per accepted
connection, the *client* dials a target reachable from the client side. The
canonical use case is exposing a service on (or reachable from) the operator's
machine to the agent running on the runner — symmetric completion of the
forwarding feature.

The protocol already anticipates this: `PortForwardDirection_Remote = 1`
exists, but only the `_Local` path is wired through the layers.

## Existing local flow (reference)

`client listens → per accepted conn: OpenPortForward(Local) round-trip → server
allocates clientStream+runnerStream pair → runner dials RemoteHost:RemotePort →
server splices the pair`. Client splices its accepted conn ↔ clientStream.
(`cli/port_forward.go`, `server/port_forward.go`, `runner/port_forward.go`,
`tui/portforward.go`, wired in `cmd/harness-cli/main.go` `forward`.)

## Reversed flow needed (remote)

`runner listens → per accepted conn: runner notifies server → server pushes to
client → client dials DialHost:DialPort → splice end-to-end`.

The one genuinely missing primitive is a **server→client push**: today the
client is purely request/response (`RoundTripTaskControl`) and learns of
server-created streams by id via `WaitForBidirectionalStream`. Remote forward
needs the client to be told *asynchronously* that a connection arrived.

## Chosen approach: A — persistent control stream

At registration the client opens one long-lived bidi **control stream** to the
server. The server writes connection-arrival notifications onto it. Per-conn
data streams are still server-created and picked up by the client via the
existing `WaitForBidirectionalStream(id)` path.

Rationale (vs. "B: client accepts server-initiated streams"): approach A reuses
the proven wait-by-id mechanism and adds no client-side `AcceptBidirectionalStream`
loop, sidestepping the accept-queue-wedge failure class previously hit on this
project (a queue nobody drains wedging the streams layer). The control stream
is client-initiated, so no new server→client stream primitive is required — the
server merely writes onto a bidi the client already opened.

## Detailed flow

### Registration
CLI: `harness-cli forward <task-id> -R [bind:]runnerPort:dialHost:dialPort`
(bind defaults to `127.0.0.1` on the **runner**).

1. Client opens a persistent control bidi stream to the server.
2. Client → server: `TaskControl/OpenPortForward{ Direction=Remote,
   BindAddr, BindPort=runnerPort, RemoteHost=dialHost, RemotePort=dialPort,
   ControlStreamId }`. `RemoteHost`/`RemotePort` are reused as "the dial
   target"; Direction decides who listens and who dials.
3. Server validates task (Running/Detached) + runner online, assigns a
   `forwardId`, records the registration (`forwardId → {controlStream,
   clientConn, task, runner}`), and sends the runner
   `RunnerRequest/OpenPortForward{ Direction=Remote, BindAddr, BindPort,
   ForwardId }`.
4. Runner opens `net.Listen("tcp", bind:runnerPort)`, stores it under
   `(task, forwardId)`. On success the server returns
   `OpenPortForwardResponse{ Ok, ForwardId }` to the client; on listen failure
   `OpenPortForwardResponse{ BindFailed }`. The client then blocks reading the
   control stream.

### Connection arrival
5. Runner listener `Accept()` → runner creates a bidi `runnerStream` to the
   server → sends `RunnerMessage/RemoteForwardConn{ ForwardId, StreamId=runnerStream }`.
6. Server looks up the registration by `forwardId`, allocates a `clientStream`
   on the registering client's conn, starts `spliceBidi(clientStream,
   runnerStream)`, and writes `RemoteForwardConnNotify{ StreamId=clientStream }`
   onto the control stream.
7. Client reads the notify → `WaitForBidirectionalStream(clientStream)` →
   `net.Dial(dialHost:dialPort)` → `spliceConnStream(conn, clientStream)`. On
   dial failure the client closes `clientStream`, whose EOF propagates back to
   the runner-side accepted connection (connection-refused semantics, mirroring
   the local path's dial-failure behaviour).

### Teardown
8. Client Ctrl-C → ctx cancel → close control stream. Server detects the
   control-stream EOF → sends `RunnerRequest/ClosePortForward{ ForwardId }` and
   drops the registration. Runner closes the listener. In-flight connections are
   left to drain and tear down naturally when their own streams close (we do not
   forcibly kill active relays on listener close — simplest correct behaviour;
   matches how a closed `ssh -R` listener stops new accepts without killing
   established tunnels).

## Schema changes (`runner/protocol/message.bgn` — every wire byte is in schema)

- `OpenPortForwardRequest` (client→server): add `BindAddr bytes`,
  `BindPort uint16`, `ControlStreamId uint64`. Reuse `RemoteHost`/`RemotePort`
  as the dial target. For `_Local` the new fields are zero/unused.
- `RunnerOpenPortForwardRequest` (server→runner): add `BindAddr bytes`,
  `BindPort uint16`, `ForwardId uint64`. For `_Local` these are unused; for
  `_Remote` `StreamId`/`RemoteHost`/`RemotePort` are unused (the runner listens,
  it does not dial).
- `OpenPortForwardResponse`: add `ForwardId uint64`; add status `BindFailed`.
- New `RunnerRequestType_ClosePortForward` + body `{ ForwardId uint64 }`.
- New `RunnerMessageType_RemoteForwardConn` + body
  `{ ForwardId uint64, StreamId uint64 }`.
- New `RemoteForwardConnNotify { StreamId uint64 }` — the control-stream
  notification, schema'd so no convention-only bytes hit the wire.

`runner/protocol/message.go` is regenerated from the `.bgn`; do not hand-edit.

## Components by layer

- **cli** (`cli/port_forward.go`): `ParseRemoteForwardSpec` (parse
  `[bind:]runnerPort:dialHost:dialPort`, bind default `127.0.0.1`);
  `OpenRemoteForward` (open control stream, send registration, return control
  stream + forwardId + dial target); `RunRemoteForward` (read the control
  stream; per `RemoteForwardConnNotify`, dial + `spliceConnStream`); teardown on
  ctx cancel. Wire `-R` into `cmd/harness-cli/main.go` `forward`. Reuse the
  existing `spliceConnStream`.
- **server** (`server/port_forward.go`): extend `handleOpenPortForward` with a
  `Direction_Remote` branch (register, instruct runner to listen, retain the
  control stream keyed by forwardId); add a handler for
  `RunnerMessageType_RemoteForwardConn` (allocate clientStream, splice, notify
  on control stream); on control-stream EOF send `ClosePortForward` and drop the
  registration. Add a remote-forward registry on the `TaskHandler`.
- **runner** (`runner/port_forward.go`): `Direction_Remote` branch (start
  listener, accept loop → create stream + send `RemoteForwardConn`); handle
  `ClosePortForward` (close listener). Track listeners per `(task, forwardId)`.
  Reuse the existing `spliceConnStream`.
- **tui** (`tui/portforward.go`, `tui/app.go`): see "TUI forward management"
  below — this work also fixes the existing single-forward-per-task limitation
  for local forwards. Add a `mode` (local/remote) to `PortForwardModal`
  switching the placeholder + dispatch target; `DoStartRemoteForward` /
  `RemoteForwardStartedMsg` / `RemoteForwardStatusMsg` mirror the local ones.
  TUI calls run on the long-lived `a.client` (no fresh dial), per the existing
  convention.

## TUI forward management (local + remote)

Today `activeForwards map[string]*PortForwardSession` is keyed by task id, so a
task can hold only one forward and starting a second silently overwrites the
first (its cancel handle is lost). Replace this with **id-keyed tracking that
supports multiple concurrent forwards** per task and overall:

- `activeForwards map[int]*PortForwardSession`, keyed by a client-side monotonic
  `nextForwardID int`. `PortForwardSession` carries `{ ID int, TaskID string,
  Direction (local|remote), Spec string, Cancel context.CancelFunc }`. One map
  holds both directions, discriminated by `Direction`.
- **Start:** `p` opens the modal in local mode, `b` in remote mode (b = bind).
  Each successful start allocates a fresh `ID` and inserts a session — multiple
  per task allowed. `PortForwardStartedMsg` carries the assigned `ID`.
- **Stop (picker):** `P` = stop-local, `B` = stop-remote. The key filters
  `activeForwards` by the selected task **and** the matching direction:
  - 0 matches → "no active {local|remote} forward for selected task".
  - exactly 1 → cancel it immediately (preserves current one-forward UX).
  - >1 → open a **picker** listing the matching sessions (spec text) for
    selection; Enter cancels the chosen one, Esc dismisses. The picker reuses a
    simple list modal alongside `PortForwardModal`.
- On cancel / forward exit, delete the session from the map by `ID` (no more
  lost cancel handles).
- Picker scope is the **selected task** (consistent with `p`/`b` being
  per-selected-task). A global cross-task forwards view is a possible future
  extension, out of scope here.
- Hint line: append `· b rforward · B stop-rforward` (local `p`/`P` already
  shown).

## Error handling

- Client dial failure → close clientStream (refused semantics).
- Runner `net.Listen` failure → `OpenPortForwardResponse{ BindFailed }`.
- Runner offline / task not Running|Detached → existing `RunnerOffline` /
  `NoSuchTask`.
- Control-stream drop (client gone) → server must close the runner listener
  (ClosePortForward) so no orphan listener lingers on the runner.

## Testing (TDD; mirror existing local-forward tests)

- **protocol**: round-trip for the new fields + new messages (extend
  `runner/protocol/port_forward_test.go`).
- **server**: Remote registration → emits listen RunnerRequest;
  `RemoteForwardConn` → allocates clientStream + writes notify; control EOF →
  emits `ClosePortForward` + drops registration.
- **runner**: Remote → starts listener; accept → creates stream + sends
  `RemoteForwardConn`; `ClosePortForward` → closes listener.
- **cli**: `ParseRemoteForwardSpec` cases; `RunRemoteForward` dial+splice driven
  by a fake control stream emitting a notify.
- **integration**: end-to-end remote forward mirroring the local e2e test
  (commit `95886c2`): runner listens, a client-side echo server is the dial
  target, bytes round-trip through the tunnel.
- **tui management**: extract the session-selection logic (filter `activeForwards`
  by task + direction → 0 / 1 / many) as a pure function and unit-test it
  (allocate ids, multiple per task, stop-by-id removes the right one, picker
  filtering). Keep the bubbletea wiring thin around the tested function.

## Out of scope (YAGNI)

- WebUI (wasm) surface — CLI + TUI only for now.
- IPv6 literal hosts (matches local-forward dogfood scope).
- A global, cross-task forwards list view in the TUI (the picker is scoped to
  the selected task). Multiple forwards per task IS now supported (both
  directions); the CLI also accepts repeated `-L` / `-R`.
- Forcibly tearing down in-flight relays when a listener closes.
