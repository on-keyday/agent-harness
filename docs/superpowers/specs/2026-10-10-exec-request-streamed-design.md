# Exec requests carry their command on a stream — design

## Problem

Zed's remote terminal does not open through the ssh gateway when the TUI hosting
the gateway is connected to the server over UDP. It opens over WS.

The terminal is `ssh -t <host> 'cd …; exec env <every variable> bash -l'`, a
**2802-byte** command line on the operator's Windows machine (measured with
`(Get-CimInstance Win32_Process -Filter "name='ssh.exe'" | Where-Object
CommandLine -match 'exec env').CommandLine.Length`). The gateway hands that line
to `cli.ExecRun`, which puts it inside `ExecRunRequest` — a task-control
message. Every task-control message is ONE objproto application message, which
over UDP is ONE datagram, and a datagram that does not fit is dropped with no
error at either end (`docs/superpowers/specs/2026-05-09-udp-dualstack-design.md`
§12.1). The budget that fits every path is about **1170 bytes** (trsf's
`DefaultInitialMTU = 1200`, minus objproto's header and AEAD tag).

What was observed, in order:
- No exec was created: a probe exec started afterwards got the id right after
  Zed's last one, so the terminal's open never reached the server's registry.
- The terminal showed a cursor and nothing else; no log at any end said why.
- The same TUI reconnected over WS: the terminal printed a prompt (operator,
  2026-10-10).
- A dummy harness did NOT reproduce it: a 2000-byte line went through over UDP,
  because the dummy runs on loopback (MTU 65536).

§12.1 of the dualstack spec already migrated the large control messages it knew
of (the snapshot, `AssignTask`, agentboard payloads) to the pattern "small
envelope + body on a trsf stream". `exec` arrived on 2026-08-25, after that
sweep, and was never put through it. `RunnerExecRunRequest` (server → runner)
has the same shape and the same exposure: several runners connect over UDP.

Its §12.2 proposed warning at the send site when an application message is over
budget. Nothing like it was built — a grep of objtrsf's `objproto` and the
harness send paths found no size check below objproto's 64 KiB packet limit —
so this failure produced no error and no log line anywhere.

## Decisions taken

**operator** = the human chose it in conversation; **this spec** = the author
chose it while writing.

| # | Decision | Decided by |
| --- | --- | --- |
| D1 | `exec` carries its argv and `term` on a stream, on BOTH hops (client → server, server → runner) | operator |
| D2 | A size check at the send site of the two request paths that carry caller-supplied lengths: the client's task-control send, and the server's `RunnerRequest` send. Not every send | operator |
| D3 | The check is NOT in objtrsf's `objproto` | operator |
| D4 | The budget is what fits one datagram on THAT connection right now — trsf's `MaxDatagramSize()`, which moves with PLPMTUD — and an over-budget message on a `udp` connection is an ERROR, not sent. `ws`/`wss` are not checked | operator (the live MTU replaced a fixed 1170) |
| D5 | The server reads an exec body up to **2 MiB** and refuses beyond it — Linux's usual total `ARG_MAX`, so no command a runner's OS would run is refused by the server first | operator |
| D6 | The four exec flags (`stdin_enabled`, `shell_line`, `sshd_parent`, `pty`) stay in the envelope; only variable-length fields move to the body | this spec |
| D7 | Every server → runner `RunnerRequest` send goes through one function, `sendRunnerRequest`, with a test that fails on a send outside it | this spec (the "obligation in a function + grep guard" shape this repo already uses) |
| D8 | A runner that cannot read an exec body reports the exec `failed` with the reason, instead of returning silently | this spec |

## Why not objproto (D3), and what that costs

objproto's `activeConnection.SendMessage` is the one place every message
passes, which is where §12.2 put the check; the operator ruled it out. In the
harness there is no such single place: 77 call sites in 31 files call
`SendMessage` on an `objproto.Connection` directly. D2 picks the two paths
whose messages carry lengths a CALLER chooses — a command line, a prompt, a
path — because those are the ones that grow past the budget without anyone
editing a schema. Responses and server-originated notifications stay
unchecked; a later incident there is the trigger to extend D7's shape.

Why not the kernel's EMSGSIZE: `transport/udp.go` (objtrsf) swallows it
deliberately, because trsf's MTU probes are oversize on purpose, so the harness
cannot see it.

What the harness CAN see is trsf's own path-MTU estimate. trsf runs PLPMTUD per
connection and exports the result as `MaxDatagramSize()` — "the largest payload
that fits one packet RIGHT NOW", already on both the server's `ConnHandle` and
the client's `peer.Conn`. Control messages do not go through trsf, so nothing
clamps them to that estimate; reading it as the budget is new. It is the
estimate for the same UDP 5-tuple, so it applies to these messages too
(inferred, not measured). It starts at trsf's floor (1200 − 30 = 1170) and
rises as probes succeed, so a LAN connection gets about 1470 once discovery has
run, and nothing that fits the path today is refused. `MaxDatagramSize()`
subtracts trsf's 30-byte packet overhead where an objproto control message
spends 24, so the budget is 6 bytes conservative; using the exported value
rather than recomputing the overhead in the harness keeps that arithmetic in
the one place its comment says it lives.

What it does not cover: right after the path narrows (a LAN → tailnet move) the
estimate is still the old, larger value until PLPMTUD notices, and an
over-sized message in that window is still dropped silently.

## Wire

```
format ExecRunRequest:
    task_id :TaskID
    stdin_enabled :u1
    shell_line :u1
    sshd_parent :u1
    pty :u1
    reserved :u4
    # A client-initiated send-stream carrying ExecRunBody until EOF. The command
    # rides here rather than inline because an argv has no bound that fits one
    # UDP datagram (Zed's remote terminal: 2802 bytes). Same pattern as
    # AgentSendRequest.payload_stream_id.
    payload_stream_id :u64

format ExecRunBody:
    argv :ExecArgv
    term_len :u8
    term :[term_len]u8

format RunnerExecRunRequest:
    exec_id :u64
    task_id :TaskID
    stream_id :u64       # bidi stream for the frame protocol, as before
    stdin_enabled :u1
    shell_line :u1
    sshd_parent :u1
    pty :u1
    reserved :u4
    # A server-initiated send-stream carrying RunnerExecRunBody until EOF.
    # AssignTask.stream_id's pattern.
    body_stream_id :u64

format RunnerExecRunBody:
    auth_ticket :[16]u8
    repo_path_len :u16
    repo_path :[repo_path_len]u8
    argv :ExecArgv
    term_len :u8
    term :[term_len]u8
```

The comments on the moved fields (`term`, the TERM rule, `repo_path`'s
provenance) move with them. `ExecArgv` and `ExecArg` are unchanged: one
argument stays at most 65535 bytes.

### Skew

All four formats change layout, and `RunnerExecRunRequest` is decoded by the
runner. As with the PTY change: restart the server, every runner and every
client together. A mixed pair fails exec rather than running it wrongly — the
envelopes no longer contain an argv at all, so neither side can mistake one
for the other — except where a decode failure has no reply path (an old runner
receiving the new envelope logs and returns, so that exec hangs until the
runner is restarted).

## Client (`cli.ExecRun`)

`cli.ExecRun` builds `ExecRunBody` and sends the request through
`(*Client).TaskControlWithPayload`, which announces the request, then writes the
body and EOF. Everything else about `ExecRun` — the refusals, the data stream,
the outcome stream, the terminal mode — is unchanged.

## Server

`handleOpenExecRun` reads the body with `readAgentPayloadStream(conn, id,
2<<20)` and decodes `ExecRunBody`. A body over 2 MiB is refused with a status
that names the size; a body that does not decode is refused as malformed. The
`execRun` registration (argv for `exec ls`, `pty`) is built from the body.

To the runner: the server creates a send-stream, sends the
`RunnerExecRunRequest` envelope through `sendRunnerRequest`, then writes the
encoded `RunnerExecRunBody` and EOF — `AssignTask`'s order (`server/dispatch.go`
`buildAssignMsg` and its caller).

## Runner

`handleExecRun` first reads the body: `waitForAssignTaskBody` is generalized to
`waitForStreamBody(ctx, lookup, id) ([]byte, error)`, and both `AssignTask` and
exec use it, each decoding its own format. A body that cannot be read or decoded
is reported as `ExecRunFinished{failed, -1, <reason>}` — the exec id is in the
envelope, so the server can still deliver an outcome (D8). Then everything
continues as today, reading `argv`, `term`, `repo_path` and `auth_ticket` from
the body.

## The send-site check (D2, D4, D7)

`runner/protocol/control_budget.go`:

```go
// CheckControlMessage refuses a control message that would be dropped
// silently: on a udp connection, larger than what fits one datagram right now
// (budget = the connection's trsf MaxDatagramSize()). ws/wss ride TCP and are
// not checked.
func CheckControlMessage(transport string, budget, n int) error
```

The error names the size, the budget, the transport, and that the request has
to stream its body (or the connection has to be ws).

- **Client**: `RoundTripTaskControl` and `BeginTaskControl` call it with
  `c.conn.Connection().ConnectionID().Transport`, `c.conn.MaxDatagramSize()`
  and the encoded length, after encoding and before `SendMessage`; an error is
  returned as the send error and the pending entry is removed, exactly as a
  failed send is now.
- **Server → runner**: `sendRunnerRequest(conn ConnHandle, req
  *protocol.RunnerRequest) error` encodes with the `RunnerControl` app kind,
  checks against `conn.ConnectionID().Transport` and `conn.MaxDatagramSize()`,
  and sends. The 15 sites that build a `RunnerRequest` today
  (`agent_wake.go`, `dispatch.go` ×2, `exec_run.go` ×2, `file_transfer.go` ×2,
  `git_query.go`, `port_forward.go` ×4, `runner_handler.go` ×2,
  `task_handler.go`) call it. A test fails when a `RunnerRequest` is encoded
  and sent anywhere else.

## Testing

- **Envelope size**: an `ExecRunRequest` and a `RunnerExecRunRequest` built
  for a 4 KiB argv with `term` set encode within 1170 bytes (trsf's floor).
- **The check**: `CheckControlMessage("udp", 1170, 1171)` errors and names the
  size and budget; `("udp", 1170, 1170)`, `("udp", 1470, 1300)`,
  `("ws", 1170, 64<<10)` and `("wss", 1170, 64<<10)` pass.
- **Client**: a request encoded over budget on a udp client is refused before
  `SendMessage`, and its pending entry is gone.
- **Server**: a body over 2 MiB is refused; a malformed body is refused.
- **Runner**: an unreadable body produces `ExecRunFinished{failed}` with the
  reason.
- **The guard**: `sendRunnerRequest` is the only place a `RunnerRequest` is sent
  (verified by reintroducing a direct send and watching the test fail).
- **Integration**: an exec whose shell line is 8 KiB runs and returns its
  output, over the dummy's WS and UDP addresses — loopback does not drop
  oversize datagrams, so this proves the stream path, not the drop.
- **Live (operator)**: with the TUI back on UDP, Zed's terminal opens and prints
  a prompt.
- `scripts/wire-skew-check.sh`.

## Non-goals

- A check in objtrsf's `objproto` (D3).
- Checking server responses and server-originated notifications (D2).
- Streaming any other request. The check will name one if it is over budget;
  that is the trigger to move it.
- The `scp` extension upload Zed attempts (refused by the gateway); separate.

## Amendment — what shipped, 2026-10-10

Commits `a6393208..da20a438` (plan
`docs/superpowers/plans/2026-10-10-exec-request-streamed.md`).

### Where the shipped code differs from the text above

- **The server opens an exec OFF the receive loop.** The spec said the server
  reads the body with `readAgentPayloadStream`; it did not say where. Run inline
  in the task-control dispatch, the read waited out its own 2 s timeout — the
  stream's frames are delivered by the same loop — and every exec came back
  `bad_body`. The `OpenExecRun` case now runs in a goroutine on a value copy of
  the envelope, `board_send`'s shape (`server/task_handler.go`).
- **`sendRunnerRequest` has 20 call sites, not 15.** The spec's list came from a
  grep for `protocol.RunnerRequest{`; five more requests are built as
  `var rr protocol.RunnerRequest` (`hold.go`, `dataplane.go` ×2, `readopt.go`,
  `server.go`'s EstablishRelay). The guard test found them. `psk.go` is exempt
  from the guard: it re-encodes the handshake's RunnerHello as a
  `RunnerMessage` for the in-process dispatcher, and nothing goes on the wire.
- `buildAssignMsg` returns the `*protocol.RunnerRequest` rather than encoded
  bytes, so both AssignTask senders go through `sendRunnerRequest`.
- The empty-argv refusal moved behind the body read; it lives in
  `openExecRun(conn, req, body)`, which `handleOpenExecRun` calls once the body
  is in hand.

### Verification

- `make test`, `make check`, `make wasm-check`, `make test-integration` green.
- `TestExecRunLongCommandOverBothTransports`: an 8 KiB shell line runs from a
  client on the WS leg and one on the UDP leg, against a runner on UDP. On
  loopback this proves the stream path, not the drop (MTU 65536).
- The send-site check: `TestCheckControlMessage`,
  `TestTaskControlSendsCheckTheSizeFirst`,
  `TestSendRunnerRequestRefusesOverBudgetOnUDP`, and the guard
  `TestRunnerRequestsAreSentOnlyThroughSendRunnerRequest` — falsified by adding
  a direct `RunnerRequest` encode to `git_query.go`, then restored.
- `scripts/wire-skew-check.sh` PASS (handshake only, as before).
- **Not yet done:** the operator's live check — Zed's remote terminal through a
  TUI-hosted gateway on UDP.
