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
| D4 | The budget is a fixed constant, about 1170 bytes, and an over-budget message on a `udp` connection is an ERROR, not sent. `ws`/`wss` are not checked | this spec, presented and not objected to |
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

Why an error and not the kernel's EMSGSIZE: `transport/udp.go` (objtrsf)
swallows EMSGSIZE deliberately, because trsf's MTU probes are oversize on
purpose, so the harness cannot see it. A fixed budget is what the harness CAN
check. It is conservative: a 1300-byte message that fits a 1500-MTU LAN today
becomes an error. Such a message is already dropped on a tailnet path
(WireGuard MTU 1280), so the error makes an existing defect visible.

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
// ControlMessageBudget is the largest control message that fits one UDP
// datagram on every path the harness runs over: trsf's DefaultInitialMTU (1200)
// minus objproto's 8-byte header and AEAD tag.
const ControlMessageBudget = 1170

// CheckControlMessage refuses a control message that would be dropped
// silently: over budget on a udp connection. ws/wss ride TCP and are not
// checked.
func CheckControlMessage(transport string, n int) error
```

The error names the size, the budget, the transport, and that the request has
to stream its body (or the connection has to be ws).

- **Client**: `RoundTripTaskControl` and `BeginTaskControl` call it with
  `c.conn.Connection().ConnectionID().Transport` and the encoded length, after
  encoding and before `SendMessage`; an error is returned as the send error and
  the pending entry is removed, exactly as a failed send is now.
- **Server → runner**: `sendRunnerRequest(conn objproto.Connection, req
  *protocol.RunnerRequest) error` encodes with the `RunnerControl` app kind,
  checks, and sends. The 15 sites that build a `RunnerRequest` today
  (`agent_wake.go`, `dispatch.go` ×2, `exec_run.go` ×2, `file_transfer.go` ×2,
  `git_query.go`, `port_forward.go` ×4, `runner_handler.go` ×2,
  `task_handler.go`) call it. A test fails when a `RunnerRequest` is encoded
  and sent anywhere else.

## Testing

- **Envelope size**: an `ExecRunRequest` and a `RunnerExecRunRequest` built
  for a 4 KiB argv with `term` set encode within `ControlMessageBudget`.
- **The check**: `CheckControlMessage("udp", budget+1)` errors and names the
  size; `("udp", budget)`, `("ws", 64<<10)` and `("wss", 64<<10)` pass.
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
