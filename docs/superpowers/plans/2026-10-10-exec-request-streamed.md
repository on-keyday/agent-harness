# Exec requests carry their command on a stream — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An exec's command line rides a trsf stream on both hops, so a long one (Zed's 2802-byte remote terminal) works over UDP; and an over-budget request on a UDP connection is an error at the send site instead of a silent drop.

**Architecture:** `ExecRunRequest` / `RunnerExecRunRequest` keep only fixed-size fields plus a stream id; `ExecRunBody` / `RunnerExecRunBody` carry argv, term (and repo path + ticket to the runner) on a stream — `AgentSendRequest`'s and `AssignTask`'s patterns. `protocol.CheckControlMessage(transport, budget, n)` is called by the client's two task-control send paths and by one new server function, `sendRunnerRequest`, through which every `RunnerRequest` send goes; the budget is the connection's trsf `MaxDatagramSize()`.

**Tech Stack:** Go, brgen `.bgn` (`make protoregen`), objtrsf trsf streams.

**Spec:** `docs/superpowers/specs/2026-10-10-exec-request-streamed-design.md`

## Global Constraints

- Body cap on the server: **2 MiB** (`2 << 20`).
- Budget: the connection's `MaxDatagramSize()`; only `udp` is checked; `ws` / `wss` never.
- The four exec flags stay in the envelopes, after `task_id` (client) / `stream_id` (runner), in the order `stdin_enabled, shell_line, sshd_parent, pty, reserved :u4`.
- New enum values are APPENDED to `ExecRunStatus`, never inserted.
- No check in objtrsf (spec D3).
- Repo is public: no LAN IPs, hostnames or private paths in committed files.
- Verify with make targets; commit trailer `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

1. **The body stream is announced after the envelope** (client `TaskControlWithPayload`, server→runner write after `sendRunnerRequest`) — the reader must poll for the stream rather than fail on first lookup. Pinned by the integration test's long-argv exec on both transports (Task 4).
2. **A runner that cannot read or decode the body** must still report the exec (`failed` + reason), never return silently. Pinned by `TestExecRunReportsAnUnreadableBody` (Task 1).
3. **A body over 2 MiB, or one that does not decode** — refused with a status naming why, not `internal_error`. Pinned by `TestOpenExecRunRefusesAnOversizeBody` / `…AMalformedBody` (Task 1).
4. **A udp client whose `MaxDatagramSize()` is the floor (1170) sending a request that is 1171 bytes** — refused before `SendMessage`, pending entry removed; a ws client with the same request — sent. Pinned in Task 2.
5. **A `RunnerRequest` sent anywhere but `sendRunnerRequest`** — fails the guard. Pinned in Task 3, verified by reintroducing a direct send.

---

### Task 1: Wire + all three consumers (atomic — the build is broken between them)

**Files:**
- Modify: `runner/protocol/message.bgn` (`format ExecRunRequest`, `format RunnerExecRunRequest`, `enum ExecRunStatus`), regenerate `runner/protocol/message.go`
- Modify: `cli/exec_run.go` (`ExecRun`, `execRunStatusError`)
- Modify: `server/exec_run.go` (`handleOpenExecRun`, `runnerExecRunRequest`)
- Modify: `runner/exec_run.go` (`handleExecRun`), `runner/connect.go` (`waitForAssignTaskBody` → `waitForStreamBody`)
- Test: `runner/protocol/exec_term_test.go` (adapt), `runner/protocol/exec_body_test.go` (create), `server/exec_run_test.go`, `server/mapper_completeness_test.go` (if it builds an `ExecRunRequest`), `runner/exec_run_test.go`, `cli/exec_run_test.go`

**Interfaces:**
- Produces: `protocol.ExecRunBody{Argv, TermLen, Term}` + `SetTerm`; `protocol.RunnerExecRunBody{AuthTicket, RepoPath, Argv, Term}` + setters; `ExecRunRequest.PayloadStreamId`; `RunnerExecRunRequest.BodyStreamId`; `ExecRunStatus_BodyTooLarge`, `ExecRunStatus_BadBody`; `runnerExecRunMessages(body *protocol.ExecRunBody, req *protocol.ExecRunRequest, execID uint64, repoPath string, dataStreamID, bodyStreamID uint64, ticket [16]byte) (protocol.RunnerExecRunRequest, protocol.RunnerExecRunBody)`; runner `waitForStreamBody(ctx, p peer.BidirectionalStreamLookup, id trsf.StreamID, what string) ([]byte, error)`.

- [ ] **Step 1: Schema**

In `format ExecRunRequest`: delete `argv :ExecArgv` and the `term_len`/`term` pair (keep their comment text for the body), and after `reserved :u4` add:

```
    # A client-initiated send-stream carrying ExecRunBody until EOF. The command
    # rides here rather than inline because an argv has no bound that fits one
    # UDP datagram — Zed's remote terminal is a 2802-byte line, and an
    # over-sized control message is dropped with no error at either end.
    # AgentSendRequest.payload_stream_id's pattern.
    payload_stream_id :u64

# ExecRunBody is what ExecRunRequest.payload_stream_id carries.
format ExecRunBody:
    argv :ExecArgv
    <the term comment moved from ExecRunRequest>
    term_len :u8
    term :[term_len]u8
```

In `format RunnerExecRunRequest`: delete `auth_ticket`, `repo_path_len`, `repo_path`, `argv`, `term_len`, `term`; keep `exec_id`, `task_id`, `stream_id` and the four flags + `reserved :u4`; add after `reserved :u4`:

```
    # A server-initiated send-stream carrying RunnerExecRunBody until EOF.
    # AssignTask.stream_id's pattern, for the same reason as
    # ExecRunRequest.payload_stream_id.
    body_stream_id :u64

format RunnerExecRunBody:
    auth_ticket :[16]u8
    <repo_path's provenance comment, moved>
    repo_path_len :u16
    repo_path :[repo_path_len]u8
    argv :ExecArgv
    # Relayed verbatim; see ExecRunBody.term.
    term_len :u8
    term :[term_len]u8
```

Append to `enum ExecRunStatus` (after `internal_error`):

```
    # The command body is larger than the server reads (2 MiB).
    body_too_large
    # The command body could not be read from its stream or did not decode.
    bad_body
```

Run: `make protoregen` → exit 0. Then `grep -n 'func (.*\*ExecRunBody) SetTerm\|func (.*\*RunnerExecRunBody) SetRepoPath\|PayloadStreamId \|BodyStreamId ' runner/protocol/message.go` → the setters and fields exist (ledger any naming difference).

- [ ] **Step 2: Failing tests (protocol)**

Create `runner/protocol/exec_body_test.go`:

```go
package protocol

import (
	"strings"
	"testing"
)

// The envelopes no longer grow with the command: a 4 KiB argv must leave both
// inside trsf's floor (1170 bytes), the smallest budget any UDP path gets.
func TestExecEnvelopesStaySmallForALongCommand(t *testing.T) {
	long := strings.Repeat("x", 4096)
	_ = long // the argv lives in the BODY now; the envelopes cannot carry it
	var c ExecRunRequest
	c.SetPty(true)
	c.PayloadStreamId = 1 << 40
	cb, err := c.Append(nil)
	if err != nil {
		t.Fatal(err)
	}
	var r RunnerExecRunRequest
	r.SetPty(true)
	r.ExecId, r.StreamId, r.BodyStreamId = 1<<40, 1<<40, 1<<40
	rb, err := r.Append(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(cb) > 1170 || len(rb) > 1170 {
		t.Fatalf("envelopes are %d / %d bytes, want <= 1170", len(cb), len(rb))
	}
}

func TestExecRunBodyRoundTripsALongArgvAndTerm(t *testing.T) {
	var b ExecRunBody
	var one ExecArg
	one.SetArg([]byte(strings.Repeat("x", 4096)))
	b.Argv.Argv = []ExecArg{one}
	b.Argv.ArgvLen = 1
	b.SetTerm([]byte("xterm-256color"))
	raw, err := b.EncodeCopy(nil)
	if err != nil {
		t.Fatal(err)
	}
	var out ExecRunBody
	if err := out.DecodeExactCopy(raw); err != nil {
		t.Fatal(err)
	}
	if len(out.Argv.Argv[0].Arg) != 4096 || string(out.Term) != "xterm-256color" {
		t.Fatalf("round trip lost data: argv0=%d term=%q", len(out.Argv.Argv[0].Arg), out.Term)
	}
}
```

Update `TestExecRunRequestRoundTripsTerm` in `exec_term_test.go` to round-trip `ExecRunBody` (term moved). Run `go test ./runner/protocol/` → PASS (the schema is already regenerated; these pin it).

- [ ] **Step 3: Client**

In `cli.ExecRun`, replace building `body := protocol.ExecRunRequest{TaskId: tid, Argv: buildExecArgv(argv)}` … `resp, err := c.RoundTripTaskControl(ctx, req)` with:

```go
	execBody := protocol.ExecRunBody{Argv: buildExecArgv(argv)}
	execBody.SetTerm([]byte(opts.Term))
	bodyBytes, err := execBody.EncodeCopy(nil)
	if err != nil {
		return ExecRunResult{}, fmt.Errorf("exec: encode command: %w", err)
	}
	// The command rides a stream, not the request: an argv has no bound that
	// fits one UDP datagram, and an over-sized control message is dropped with
	// no error at either end.
	tr, err := c.TaskControlWithPayload(ctx, func(streamID uint64) (*protocol.TaskControlRequest, error) {
		env := protocol.ExecRunRequest{TaskId: tid, PayloadStreamId: streamID}
		env.SetShellLine(opts.ShellLine)
		env.SetSshdParent(opts.SshdParent)
		env.SetStdinEnabled(opts.Stdin != nil || opts.Terminal)
		env.SetPty(opts.Pty)
		req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_OpenExecRun}
		req.SetOpenExecRun(env)
		return req, nil
	}, bodyBytes)
	if err != nil {
		return ExecRunResult{}, err
	}
	if tr.Err != nil {
		return ExecRunResult{}, tr.Err
	}
	resp := tr.Resp
```

(keep the following `r := resp.OpenExecRun()` handling). Add to `execRunStatusError`:

```go
	case protocol.ExecRunStatus_BodyTooLarge:
		return errors.New("exec: command too large (the server reads at most 2 MiB)")
	case protocol.ExecRunStatus_BadBody:
		return errors.New("exec: the server could not read the command")
```

- [ ] **Step 4: Server — failing tests**

In `server/exec_run_test.go`, replace `TestRunnerExecRunRequestCarriesEveryField`'s call with `runnerExecRunMessages` and assert: envelope keeps `ExecId 7`, `StreamId 42`, `BodyStreamId 43`, task id, all four flags; body carries `/repo`, the argv, `term`, and the ticket. Update `TestRunnerExecRunRequestCarriesTheTasksTicket` likewise (the ticket now in the body). Add:

```go
// A body past the cap is refused by name, not as internal_error.
func TestOpenExecRunRefusesAnOversizeBody(t *testing.T) {
	st := execBodyStatus(errPayloadTooLarge)
	if st != protocol.ExecRunStatus_BodyTooLarge {
		t.Fatalf("status = %v, want body_too_large", st)
	}
}

func TestOpenExecRunRefusesAMalformedBody(t *testing.T) {
	if st := execBodyStatus(errors.New("decode")); st != protocol.ExecRunStatus_BadBody {
		t.Fatalf("status = %v, want bad_body", st)
	}
}
```

Run `go test ./server/ -run 'ExecRun'` → FAIL (undefined `runnerExecRunMessages`, `execBodyStatus`).

- [ ] **Step 5: Server — implement**

In `server/exec_run.go`:

```go
// execBodyMax is the most the server reads of an exec's command: Linux's usual
// total ARG_MAX, so no command a runner's OS would run is refused here first,
// and a bound, because an unbounded read is a free allocation.
const execBodyMax = 2 << 20

// execBodyStatus maps a body read/decode failure to what the client is told.
func execBodyStatus(err error) protocol.ExecRunStatus {
	if errors.Is(err, errPayloadTooLarge) {
		return protocol.ExecRunStatus_BodyTooLarge
	}
	return protocol.ExecRunStatus_BadBody
}
```

At the top of `handleOpenExecRun`, before the empty-argv check:

```go
	raw, rerr := readAgentPayloadStream(conn, req.PayloadStreamId, execBodyMax)
	if rerr != nil {
		slog.Warn("exec_run: command body", "err", rerr)
		return errResp(execBodyStatus(rerr))
	}
	var body protocol.ExecRunBody
	if derr := body.DecodeExactCopy(raw); derr != nil {
		slog.Warn("exec_run: command body decode", "err", derr)
		return errResp(protocol.ExecRunStatus_BadBody)
	}
```

Replace `req.Argv` with `body.Argv` throughout the function. Replace the runner send block (`rreq := …` through the `SendMessage` failure branch) with: create `bodyStream := runner.Conn.CreateSendStream()` (nil → close the three streams, `removeExec`, `internal_error`); build `env, rbody := runnerExecRunMessages(&body, req, execID, task.RepoPath, uint64(runnerStream.ID()), uint64(bodyStream.ID()), ticket)`; send the envelope (Task 3 swaps this for `sendRunnerRequest`; for now `rreq.MustAppend` + `runner.Conn.SendMessage` as before); then

```go
	encoded, eerr := rbody.EncodeCopy(nil)
	if eerr == nil {
		eerr = bodyStream.AppendData(false, encoded)
	}
	if eerr == nil {
		eerr = bodyStream.AppendData(true)
	}
	if eerr != nil {
		slog.Error("exec_run: body to runner failed", "task_id", taskIDHex, "err", eerr)
		// The runner reports an unreadable body itself (it holds the exec id),
		// so the client still gets an outcome; nothing more to unwind here.
	}
```

`runnerExecRunRequest` becomes `runnerExecRunMessages`:

```go
func runnerExecRunMessages(body *protocol.ExecRunBody, req *protocol.ExecRunRequest, execID uint64, repoPath string, dataStreamID, bodyStreamID uint64, authTicket [16]byte) (protocol.RunnerExecRunRequest, protocol.RunnerExecRunBody) {
	env := protocol.RunnerExecRunRequest{
		ExecId:       execID,
		TaskId:       req.TaskId,
		StreamId:     dataStreamID,
		BodyStreamId: bodyStreamID,
	}
	env.SetShellLine(req.ShellLine())
	env.SetSshdParent(req.SshdParent())
	env.SetStdinEnabled(req.StdinEnabled())
	env.SetPty(req.Pty())
	rb := protocol.RunnerExecRunBody{AuthTicket: authTicket, Argv: body.Argv}
	rb.SetRepoPath([]byte(repoPath))
	rb.SetTerm(body.Term)
	return env, rb
}
```

Keep its doc comment (one relay site; the ticket is a parameter). Run `go test ./server/ -run 'ExecRun|MapsEveryField'` → PASS.

- [ ] **Step 6: Runner — failing test**

In `runner/exec_run_test.go`:

```go
// The exec id is in the envelope, so a body that never arrives is still
// reported — silence would leave the client waiting on the outcome forever.
func TestExecRunReportsAnUnreadableBody(t *testing.T) {
	var got *protocol.ExecRunFinished
	s := &Session{Sender: senderFunc(func(b []byte) error {
		var m protocol.RunnerMessage
		if err := m.DecodeExactCopy(b[1:]); err == nil {
			got = m.ExecRunFinished()
		}
		return nil
	}), Streams: noStreams{}}
	req := &protocol.RunnerExecRunRequest{ExecId: 5, StreamId: 9, BodyStreamId: 0}
	s.handleExecRun(context.Background(), req)
	if got == nil || got.ExecId != 5 || got.Kind != protocol.ExecEventKind_Failed || len(got.Detail) == 0 {
		t.Fatalf("finish = %+v, want failed with a reason for exec 5", got)
	}
}
```

Read `runner/session.go` for the `Session.Sender` and `Streams` field types and write `senderFunc` / `noStreams` (a lookup returning nil) to satisfy them; if `handleExecRun` needs the data stream first, order the body read BEFORE it (Step 7) so this test does not depend on one. Run `go test ./runner/ -run TestExecRunReportsAnUnreadableBody` → FAIL (build: `BodyStreamId` reads / missing behaviour).

- [ ] **Step 7: Runner — implement**

Rename `waitForAssignTaskBody` to return raw bytes:

```go
// waitForStreamBody resolves a server-initiated send-stream, reads it to EOF and
// returns the bytes. AssignTask and exec both ship their bodies this way; each
// caller decodes its own format. what names the body in errors.
func waitForStreamBody(ctx context.Context, p peer.BidirectionalStreamLookup, id trsf.StreamID, what string) ([]byte, error)
```

(its body is the old function's up to `raw`, with "AssignTask" replaced by `what`). Its AssignTask caller decodes `AssignTaskBody` from the bytes with the old error text.

In `handleExecRun`, first thing after `finish` is defined:

```go
	raw, berr := waitForStreamBody(ctx, s.Streams, trsf.StreamID(req.BodyStreamId), "exec body")
	var body protocol.RunnerExecRunBody
	if berr == nil {
		berr = body.DecodeExactCopy(raw)
	}
	if berr != nil {
		finish(protocol.ExecEventKind_Failed, -1, "exec body: "+berr.Error())
		return
	}
```

Then read `repoPath`, `argv`, the ticket and `term` from `body` (`string(body.RepoPath)`, `body.Argv`, `body.AuthTicket`, `body.Term`). `execPtyRefusal` / `execPtyEnv` take the term as a parameter now (`func execPtyRefusal(req *protocol.RunnerExecRunRequest, term []byte) string`, `func execPtyEnv(env []string, req *protocol.RunnerExecRunRequest, term []byte) []string`) — update their tests to build a body's term.

Run `go test ./runner/ ./runner/protocol/` → PASS.

- [ ] **Step 8: Whole build and suite**

Run: `go build ./... && make wasm-check` → exit 0.
Run: `go test -tags integration ./integration/ -run 'TestExecRunE2E|TestSSHGatewayE2E' -count=1 -timeout 400s` → ok (every existing exec path now goes through the stream).

- [ ] **Step 9: Commit**

```bash
git add runner/protocol/ cli/exec_run.go cli/exec_run_test.go server/exec_run.go server/exec_run_test.go server/mapper_completeness_test.go runner/exec_run.go runner/exec_run_test.go runner/connect.go
git commit -m "feat(exec): the command rides a stream on both hops, not the control message

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 2: The send-site check, and the client's two paths

**Files:**
- Create: `runner/protocol/control_budget.go`, `runner/protocol/control_budget_test.go`
- Modify: `cli/client.go` (`RoundTripTaskControl`, `BeginTaskControl`)
- Test: `cli/client_budget_test.go` (create)

**Interfaces:**
- Produces: `func CheckControlMessage(transport string, budget, n int) error`.

- [ ] **Step 1: Failing tests**

`runner/protocol/control_budget_test.go`:

```go
package protocol

import (
	"strings"
	"testing"
)

func TestCheckControlMessage(t *testing.T) {
	err := CheckControlMessage("udp", 1170, 1171)
	if err == nil || !strings.Contains(err.Error(), "1171") || !strings.Contains(err.Error(), "1170") {
		t.Fatalf("over budget on udp: %v, want an error naming 1171 and 1170", err)
	}
	for _, c := range []struct {
		tr        string
		budget, n int
	}{{"udp", 1170, 1170}, {"udp", 1470, 1300}, {"ws", 1170, 64 << 10}, {"wss", 1170, 64 << 10}} {
		if err := CheckControlMessage(c.tr, c.budget, c.n); err != nil {
			t.Errorf("%s budget=%d n=%d: %v, want nil", c.tr, c.budget, c.n, err)
		}
	}
}
```

Run `go test ./runner/protocol/ -run TestCheckControlMessage` → FAIL (undefined).

- [ ] **Step 2: Implement**

`runner/protocol/control_budget.go`:

```go
package protocol

import "fmt"

// CheckControlMessage refuses a control message that would be dropped
// silently. Every control message is ONE objproto message, which over UDP is
// ONE datagram, and a datagram that does not fit the path is dropped with no
// error at either end. budget is what fits right now: the connection's trsf
// MaxDatagramSize(), which follows PLPMTUD. ws and wss ride TCP and are not
// checked.
//
// The callers are the paths whose messages carry caller-chosen lengths — a
// client's task-control request and the server's RunnerRequest — not every
// send; see docs/superpowers/specs/2026-10-10-exec-request-streamed-design.md.
func CheckControlMessage(transport string, budget, n int) error {
	if transport != "udp" || n <= budget {
		return nil
	}
	return fmt.Errorf("control message is %d bytes; one udp datagram holds %d on this path, and the rest would be dropped silently — this request has to stream its body (or connect over ws)", n, budget)
}
```

Run → PASS.

- [ ] **Step 3: Client — failing test**

The `cli` tests build `&Client{pending: …}` with no connection (`cli/client_skew_test.go`), so the wiring is pinned by a source scan rather than a fake transport; the check's behaviour is pinned by Step 1. `cli/client_budget_test.go`:

```go
package cli

import (
	"os"
	"strings"
	"testing"
)

// Both task-control send paths check the size BEFORE SendMessage. A path that
// skipped it would send an over-budget request to be dropped silently over udp,
// which is how Zed's remote terminal hung with no error anywhere.
func TestTaskControlSendsCheckTheSizeFirst(t *testing.T) {
	src, err := os.ReadFile("client.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, fn := range []string{"func (c *Client) RoundTripTaskControl(", "func (c *Client) BeginTaskControl("} {
		i := strings.Index(string(src), fn)
		if i < 0 {
			t.Fatalf("%s not found", fn)
		}
		body := string(src[i:])
		if j := strings.Index(body[1:], "\nfunc "); j > 0 {
			body = body[:j+1]
		}
		check := strings.Index(body, "c.checkTaskControlSize(")
		send := strings.Index(body, ".SendMessage(")
		if check < 0 || send < 0 || check > send {
			t.Errorf("%s: checkTaskControlSize must come before SendMessage (check at %d, send at %d)", fn, check, send)
		}
	}
}
```

Run `go test ./cli/ -run TestTaskControlSendsCheckTheSizeFirst` → FAIL.

- [ ] **Step 4: Client — implement**

In both `RoundTripTaskControl` and `BeginTaskControl`, after `data := req.MustAppend(...)` and before `SendMessage`:

```go
	if err := c.checkTaskControlSize(data); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("send: %w", err)
	}
```

with

```go
// checkTaskControlSize refuses a request that would not fit one datagram on a
// udp connection, instead of sending it to be dropped silently.
func (c *Client) checkTaskControlSize(data []byte) error {
	return protocol.CheckControlMessage(c.conn.Connection().ConnectionID().Transport, c.conn.MaxDatagramSize(), len(data))
}
```

Run `go test ./cli/` → PASS; `make wasm-check` → exit 0 (`peer.Conn.MaxDatagramSize` must build for js; if it does not, ledger and gate the check to native).

- [ ] **Step 5: Commit**

```bash
git add runner/protocol/control_budget.go runner/protocol/control_budget_test.go cli/client.go cli/client_budget_test.go
git commit -m "feat(cli): a task-control request too big for one udp datagram is an error, not a silent drop

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: `sendRunnerRequest` — every server → runner send, checked, through one function

**Files:**
- Create: `server/runner_send.go`, `server/runner_send_test.go`
- Modify: `server/agent_wake.go`, `server/dispatch.go` (AssignTask + CancelTask), `server/exec_run.go` (open + close), `server/file_transfer.go` ×2, `server/git_query.go`, `server/port_forward.go` ×4, `server/runner_handler.go` ×2, `server/task_handler.go` (OpenExec)

**Interfaces:**
- Produces: `func sendRunnerRequest(conn runnerSender, req *protocol.RunnerRequest) error` where `type runnerSender interface { ConnectionID() objproto.ConnectionID; SendMessage([]byte) (int, uint64, error); MaxDatagramSize() int }` (`ConnHandle` satisfies it).

- [ ] **Step 1: Failing tests**

`server/runner_send_test.go`:

```go
package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

type recordingSender struct {
	transport string
	budget    int
	sent      [][]byte
}

func (r *recordingSender) ConnectionID() objproto.ConnectionID {
	return objproto.ConnectionID{Transport: r.transport}
}
func (r *recordingSender) SendMessage(b []byte) (int, uint64, error) {
	r.sent = append(r.sent, b)
	return len(b), 0, nil
}
func (r *recordingSender) MaxDatagramSize() int { return r.budget }

// A RunnerRequest that does not fit one udp datagram is refused before it is
// sent; the same request over ws goes out.
func TestSendRunnerRequestRefusesOverBudgetOnUDP(t *testing.T) {
	req := &protocol.RunnerRequest{Kind: protocol.RunnerRequestType_CloseExecRun}
	req.SetCloseExecRun(protocol.CloseExecRunRequest{ExecId: 1})
	udp := &recordingSender{transport: "udp", budget: 4}
	if err := sendRunnerRequest(udp, req); err == nil || len(udp.sent) != 0 {
		t.Fatalf("udp over budget: err=%v sends=%d, want an error and nothing sent", err, len(udp.sent))
	}
	ws := &recordingSender{transport: "ws", budget: 4}
	if err := sendRunnerRequest(ws, req); err != nil || len(ws.sent) != 1 {
		t.Fatalf("ws: err=%v sends=%d, want one send", err, len(ws.sent))
	}
}

// Every RunnerRequest leaves the server through sendRunnerRequest. The encode
// prefix is the tell: a RunnerRequest encoded anywhere else is one sent past
// the check.
func TestRunnerRequestsAreSentOnlyThroughSendRunnerRequest(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	const tell = "byte(appwire.AppKind_RunnerControl)})"
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "runner_send.go" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), tell) {
			t.Errorf("%s encodes a RunnerRequest itself; send it through sendRunnerRequest", f)
		}
	}
}
```

(If `CloseExecRunRequest`'s setter or field names differ, use the generated ones.) Run `go test ./server/ -run 'SendRunnerRequest|OnlyThroughSendRunnerRequest'` → FAIL (undefined `sendRunnerRequest`; the guard lists the 15 sites' files).

- [ ] **Step 2: Implement**

`server/runner_send.go`:

```go
package server

import (
	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// runnerSender is the part of a runner connection a send needs. ConnHandle
// satisfies it.
type runnerSender interface {
	ConnectionID() objproto.ConnectionID
	SendMessage([]byte) (int, uint64, error)
	MaxDatagramSize() int
}

// sendRunnerRequest encodes req and sends it to a runner — refusing one that
// would not fit one datagram on a udp connection, which would otherwise be
// dropped with no error at either end. Every RunnerRequest goes out here
// (TestRunnerRequestsAreSentOnlyThroughSendRunnerRequest), so the check cannot
// be skipped by a new call site.
func sendRunnerRequest(conn runnerSender, req *protocol.RunnerRequest) error {
	data, err := req.Append([]byte{byte(appwire.AppKind_RunnerControl)})
	if err != nil {
		return err
	}
	if err := protocol.CheckControlMessage(conn.ConnectionID().Transport, conn.MaxDatagramSize(), len(data)); err != nil {
		return err
	}
	_, _, err = conn.SendMessage(data)
	return err
}
```

Convert the 15 sites: each `x := req.(Must)Append(...RunnerControl...)` + `conn.SendMessage(x)` pair becomes `err := sendRunnerRequest(conn, &req)` keeping the site's existing error handling (log line, status, rollback). `buildAssignMsg` returns the `*protocol.RunnerRequest` and body bytes instead of encoded envelope bytes; its caller sends through `sendRunnerRequest`. A site whose connection is not a `ConnHandle` (read each) gets an adapter or a ruling in the ledger.

Run `go test ./server/` → PASS. Then reintroduce one direct `MustAppend(...RunnerControl...)` + `SendMessage` in any file, run the guard → FAIL, restore.

- [ ] **Step 3: Commit**

```bash
git add server/
git commit -m "feat(server): every RunnerRequest goes through sendRunnerRequest, which refuses one too big for a udp datagram

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: Integration over both transports, gates, skew, record

**Files:**
- Modify: `integration/exec_run_test.go` (or a new `integration/exec_long_argv_test.go`)
- Modify: the spec (Amendment), `.claude/skills/surface-parity-checklist/firing-log.md` (only if the walk finds a surface)

- [ ] **Step 1: Long argv over WS and UDP**

Write `TestExecRunLongCommandOverBothTransports`: `startDualStackServer` (`integration/udp_test.go`), a runner on the UDP leg with `runnerOpts{MaxTasks: 2, Roots: []string{repo}, ClaudeBin: fakeClaudeSlowPath(t)}` (read `startRunnerWithCID` / `startRunner` and pick the one that takes options), a live task via `openLiveSession`, then from a client dialled on EACH leg: `c.ExecRun(ctx, taskID, []string{"echo " + strings.Repeat("a", 8192) + " END"}, cli.ExecRunOpts{ShellLine: true, Stdout: &out})` → exited 0, `out` ends in `END`. The comment says loopback does not drop oversize datagrams, so this proves the stream path, not the drop.

Run: `go test -tags integration ./integration/ -run TestExecRunLongCommandOverBothTransports -count=1 -timeout 300s` → ok.

- [ ] **Step 2: Gates** (one at a time)

`make test`, `make check`, `make wasm-check`, `make test-integration`, `scripts/wire-skew-check.sh` → all exit 0.

- [ ] **Step 3: Live — operator**

Land (separate step, operator-triggered), restart everything, put the gateway's TUI back on UDP, open Zed's terminal → a prompt. Record the result in the spec Amendment.

- [ ] **Step 4: Amend the spec and commit**

Append `## Amendment — what shipped, <date>` (commit range, deviations as reversals, test results), commit:

```bash
git add docs/superpowers/specs/2026-10-10-exec-request-streamed-design.md
git commit -m "docs(exec): streamed exec — what shipped

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```
