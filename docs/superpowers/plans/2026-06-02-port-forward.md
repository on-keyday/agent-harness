# Port forwarding (`-L`) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add SSH `-L` style TCP port forwarding so a client can reach a `host:port` dialled from the runner side, over server-relayed `trsf` streams, exposed via both `harness-cli forward` and the TUI.

**Architecture:** Reuse the file-transfer skeleton verbatim: a `TaskControl` `OpenPortForward` RPC (client→server) makes the server allocate a `trsf` bidirectional stream pair and forward a `RunnerOpenPortForward` request to the runner; the server splices the two streams. One accepted TCP connection = one stream pair. The client splices its accepted `net.Conn`↔stream; the runner `net.Dial`s the target and splices target conn↔stream. Teardown uses the `spliceBidi` (either-side-closes-tears-down-both) variant. Direction is carried as a schema enum so `-R` is a later addition, not a reshape.

**Tech Stack:** Go, the in-tree `runner/protocol` brgen-generated wire types (`.bgn` → `make protoregen`), `trsf` streams, Bubble Tea (`tui`).

---

## Controller pre-flight (read before dispatching any subagent)

This repo's worktree paths route to the parent checkout. **All subagents MUST work in `/home/kforfk/workspace/remote-agent-harness/`, NOT in any `.harness-worktrees/<hash>/` dir, and verify `git rev-parse --abbrev-ref HEAD` before writing.** Each implementer/reviewer prompt MUST also include: (1) read `.claude/skills/implementation-pitfalls/SKILL.md` first; (2) quote the spec Problem statement and report coverage; (3) grep sibling code in the layer before adding a new entry. See the spec: `docs/superpowers/specs/2026-06-02-port-forward-design.md`.

## File structure

- `runner/protocol/message.bgn` — **all** new wire types in one task (then `make protoregen` regenerates `message.go`).
- `cli/port_forward.go` — `parseForwardSpec`, `(*Client).OpenPortForward`, `RunForward` engine, `spliceConnStream`. (Methods on `*Client` are the long-lived-client form the TUI calls directly — same as the file-transfer helpers.)
- `cmd/harness-cli/main.go` — wire the `forward` subcommand.
- `server/port_forward.go` — `handleOpenPortForward`; dispatch arm added in `server/task_handler.go`.
- `runner/port_forward.go` — `handleOpenPortForward`, `spliceConnStream`; dispatch arm added in `runner/connect.go`.
- `tui/portforward.go` — `PortForwardModal`, `PortForwardSession`, `DoStartPortForward`, status Msgs; key wiring in `tui/app.go`.

---

### Task 1: Wire schema (all messages, one place)

**Files:**
- Modify: `runner/protocol/message.bgn`
- Regenerate: `runner/protocol/message.go` (via `make protoregen`)
- Test: `runner/protocol/port_forward_test.go` (new)

- [ ] **Step 1: Add the schema.** In `runner/protocol/message.bgn`:

Add `open_port_forward` to the `RunnerRequestType` enum (after `chained_relay_response`):
```
enum RunnerRequestType:
    :u8
    assign_task
    cancel_task
    open_exec
    runner_hello_response
    task_wake
    open_file_transfer
    list_files
    establish_relay
    chained_relay_response
    open_port_forward
```

Add the match arm in `format RunnerRequest` (after the `chained_relay_response` arm):
```
        RunnerRequestType.open_port_forward => open_port_forward :RunnerOpenPortForwardRequest
```

Add `open_port_forward` to the `TaskControlKind` enum (after `dial_runner`):
```
    dial_runner
    open_port_forward
```

Add the match arm in `format TaskControlRequest` (before the `.. => error("Unexpected task")` line):
```
        TaskControlKind.open_port_forward => open_port_forward :OpenPortForwardRequest
```

Add the match arm in `format TaskControlResponse` (after the `dial_runner` arm):
```
        TaskControlKind.open_port_forward => open_port_forward :OpenPortForwardResponse
```

Add the new formats next to the file-transfer block (after `RunnerListFilesRequest`, around line 710):
```
# === Port forwarding (client ↔ runner; see specs/2026-06-02-port-forward-design.md) ===

enum PortForwardDirection:
    :u8
    local    # client listens, runner dials remote_host:remote_port (SSH -L).
    remote   # reserved: runner listens, client dials. Not yet implemented.

enum OpenPortForwardStatus:
    :u8
    ok             = "ok"
    no_such_task   = "no_such_task"
    runner_offline = "runner_offline"
    internal_error = "internal_error"

format OpenPortForwardRequest:
    task_id         :TaskID
    direction       :PortForwardDirection
    remote_host_len :u16
    remote_host     :[remote_host_len]u8
    remote_port     :u16

format OpenPortForwardResponse:
    status    :OpenPortForwardStatus
    stream_id :u64

format RunnerOpenPortForwardRequest:
    task_id         :TaskID
    stream_id       :u64
    direction       :PortForwardDirection
    remote_host_len :u16
    remote_host     :[remote_host_len]u8
    remote_port     :u16
```

- [ ] **Step 2: Regenerate.**

Run: `make protoregen ARGS='runner/protocol/message.bgn'`
Expected: `runner/protocol/message.go` updated; `git diff --stat` shows it changed.

- [ ] **Step 3: Write the round-trip test.** Create `runner/protocol/port_forward_test.go`:
```go
package protocol

import "testing"

func TestOpenPortForwardRequest_RoundTrip(t *testing.T) {
	req := OpenPortForwardRequest{
		TaskId:     TaskID{Id: [16]byte{1, 2, 3}},
		Direction:  PortForwardDirection_Local,
		RemotePort: 3000,
	}
	req.SetRemoteHost([]byte("127.0.0.1"))
	enc, err := req.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	got := &OpenPortForwardRequest{}
	if _, err := got.Decode(enc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.RemotePort != 3000 || string(got.RemoteHost) != "127.0.0.1" ||
		got.Direction != PortForwardDirection_Local {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}
```

- [ ] **Step 4: Run the test.**

Run: `go test ./runner/protocol/ -run TestOpenPortForward -v`
Expected: PASS.

- [ ] **Step 5: Commit.**
```bash
git add runner/protocol/message.bgn runner/protocol/message.go runner/protocol/port_forward_test.go
git commit -m "feat(proto): port-forward wire types (OpenPortForward + runner variant)"
```

---

### Task 2: `-L` spec parser

**Files:**
- Create: `cli/port_forward.go`
- Test: `cli/port_forward_test.go`

- [ ] **Step 1: Write the failing test.** Create `cli/port_forward_test.go`:
```go
package cli

import "testing"

func TestParseForwardSpec(t *testing.T) {
	cases := []struct {
		in              string
		bind            string
		lport           int
		rhost           string
		rport           int
		wantErr         bool
	}{
		{"3000:127.0.0.1:3000", "127.0.0.1", 3000, "127.0.0.1", 3000, false},
		{"0.0.0.0:8080:10.0.0.5:80", "0.0.0.0", 8080, "10.0.0.5", 80, false},
		{"3000:localhost:3000", "127.0.0.1", 3000, "localhost", 3000, false},
		{"badspec", "", 0, "", 0, true},
		{"3000:host", "", 0, "", 0, true},
		{"notaport:host:80", "", 0, "", 0, true},
		{"3000:host:notaport", "", 0, "", 0, true},
	}
	for _, c := range cases {
		got, err := parseForwardSpec(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("%q: expected error, got %+v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%q: unexpected error: %v", c.in, err)
			continue
		}
		if got.BindAddr != c.bind || got.LocalPort != c.lport ||
			got.RemoteHost != c.rhost || got.RemotePort != c.rport {
			t.Errorf("%q: got %+v", c.in, got)
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails.**

Run: `go test ./cli/ -run TestParseForwardSpec -v`
Expected: FAIL (undefined: parseForwardSpec).

- [ ] **Step 3: Implement.** Create `cli/port_forward.go` with the parser (rest of the engine is added in later tasks):
```go
package cli

import (
	"fmt"
	"strconv"
	"strings"
)

// ForwardSpec is one parsed -L forward: listen on BindAddr:LocalPort, and
// for each accepted connection have the runner dial RemoteHost:RemotePort.
type ForwardSpec struct {
	BindAddr   string
	LocalPort  int
	RemoteHost string
	RemotePort int
}

// parseForwardSpec parses "[bind:]localport:remotehost:remoteport".
// bind defaults to 127.0.0.1 (do not expose the local port externally).
// IPv6 literal hosts are not supported (dogfood scope).
func parseForwardSpec(s string) (ForwardSpec, error) {
	parts := strings.Split(s, ":")
	var bind, rhost, lportS, rportS string
	switch len(parts) {
	case 3:
		bind = "127.0.0.1"
		lportS, rhost, rportS = parts[0], parts[1], parts[2]
	case 4:
		bind, lportS, rhost, rportS = parts[0], parts[1], parts[2], parts[3]
	default:
		return ForwardSpec{}, fmt.Errorf("forward: bad spec %q (want [bind:]localport:remotehost:remoteport)", s)
	}
	lport, err := strconv.Atoi(lportS)
	if err != nil || lport <= 0 || lport > 65535 {
		return ForwardSpec{}, fmt.Errorf("forward: bad local port in %q", s)
	}
	rport, err := strconv.Atoi(rportS)
	if err != nil || rport <= 0 || rport > 65535 {
		return ForwardSpec{}, fmt.Errorf("forward: bad remote port in %q", s)
	}
	if rhost == "" {
		return ForwardSpec{}, fmt.Errorf("forward: empty remote host in %q", s)
	}
	return ForwardSpec{BindAddr: bind, LocalPort: lport, RemoteHost: rhost, RemotePort: rport}, nil
}
```

- [ ] **Step 4: Run to verify it passes.**

Run: `go test ./cli/ -run TestParseForwardSpec -v`
Expected: PASS.

- [ ] **Step 5: Commit.**
```bash
git add cli/port_forward.go cli/port_forward_test.go
git commit -m "feat(cli): parse -L forward spec"
```

---

### Task 3: Client `OpenPortForward` + `spliceConnStream` + `RunForward` engine

**Files:**
- Modify: `cli/port_forward.go`

This task mirrors `cli/file_transfer.go`'s `OpenFileTransfer` (the client-side open + `peer.WaitForBidirectionalStream`). No unit test here — exercised by the integration test in Task 6. Compile-check only.

- [ ] **Step 1: Add the imports and the three functions to `cli/port_forward.go`.**

Add to the import block:
```go
import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/trsf"
)
```

Append:
```go
// OpenPortForward asks the server to wire a relayed stream to the runner
// for taskIDHex, which will dial remoteHost:remotePort. Returns the bidi
// stream the caller splices its accepted TCP connection against. Mirrors
// (*Client).OpenFileTransfer. This is a method on the long-lived *Client,
// so the TUI calls it directly on a.client (no fresh dial).
func (c *Client) OpenPortForward(ctx context.Context, taskIDHex, remoteHost string, remotePort int) (trsf.BidirectionalStream, error) {
	tid, err := parseTaskIDHex(taskIDHex)
	if err != nil {
		return nil, fmt.Errorf("forward: parse task id: %w", err)
	}
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_OpenPortForward}
	body := protocol.OpenPortForwardRequest{
		TaskId:     tid,
		Direction:  protocol.PortForwardDirection_Local,
		RemotePort: uint16(remotePort),
	}
	body.SetRemoteHost([]byte(remoteHost))
	req.SetOpenPortForward(body)

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.Kind != protocol.TaskControlKind_OpenPortForward {
		return nil, fmt.Errorf("forward: unexpected response kind %v", resp.Kind)
	}
	r := resp.OpenPortForward()
	if r == nil {
		return nil, errors.New("forward: response variant missing")
	}
	switch r.Status {
	case protocol.OpenPortForwardStatus_Ok:
	case protocol.OpenPortForwardStatus_NoSuchTask:
		return nil, errors.New("forward: no such task (id unknown or task not running)")
	case protocol.OpenPortForwardStatus_RunnerOffline:
		return nil, errors.New("forward: runner offline")
	default:
		return nil, fmt.Errorf("forward: server error (status=%d)", r.Status)
	}
	st := peer.WaitForBidirectionalStream(ctx, c.Transport(), trsf.StreamID(r.StreamId))
	if st == nil {
		return nil, fmt.Errorf("forward: stream %d not visible", r.StreamId)
	}
	return st, nil
}

// spliceConnStream pumps bytes between a net.Conn and a trsf bidi stream
// until either direction closes or errors, then tears down both. Same
// either-side-wins teardown as server.spliceBidi (correct for TCP, where a
// half-closed/RST peer must not leave the reverse relay blocked forever).
func spliceConnStream(conn net.Conn, st trsf.BidirectionalStream) {
	var once sync.Once
	teardown := func() {
		once.Do(func() {
			_ = conn.Close()
			_ = st.CloseBoth()
		})
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // conn -> stream
		defer wg.Done()
		defer teardown()
		buf := make([]byte, 64*1024)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				if werr := st.AppendData(false, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				_ = st.AppendData(true)
				return
			}
		}
	}()
	go func() { // stream -> conn
		defer wg.Done()
		defer teardown()
		for {
			data, eof, err := st.ReadDirect(64 * 1024)
			if err != nil {
				return
			}
			if len(data) > 0 {
				if _, werr := conn.Write(data); werr != nil {
					return
				}
			}
			if eof {
				return
			}
		}
	}()
	wg.Wait()
}

// RunForward listens for each spec and bridges accepted connections to the
// runner via OpenPortForward. Blocks until ctx is cancelled, then closes all
// listeners. Per-connection errors are logged and isolated; the listener and
// sibling connections are unaffected.
func RunForward(ctx context.Context, c *Client, taskIDHex string, specs []ForwardSpec, logf func(string)) error {
	if logf == nil {
		logf = func(s string) { slog.Info(s) }
	}
	var lns []net.Listener
	for _, sp := range specs {
		ln, err := net.Listen("tcp", net.JoinHostPort(sp.BindAddr, strconv.Itoa(sp.LocalPort)))
		if err != nil {
			for _, l := range lns {
				_ = l.Close()
			}
			return fmt.Errorf("forward: listen %s:%d: %w", sp.BindAddr, sp.LocalPort, err)
		}
		lns = append(lns, ln)
		logf(fmt.Sprintf("forwarding %s:%d -> %s:%d (task %s)", sp.BindAddr, sp.LocalPort, sp.RemoteHost, sp.RemotePort, taskIDHex[:min(12, len(taskIDHex))]))
		go acceptLoop(ctx, c, taskIDHex, sp, ln, logf)
	}
	<-ctx.Done()
	for _, l := range lns {
		_ = l.Close()
	}
	return nil
}

func acceptLoop(ctx context.Context, c *Client, taskIDHex string, sp ForwardSpec, ln net.Listener, logf func(string)) {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			return // listener closed (ctx done) or fatal accept error
		}
		go func() {
			st, err := c.OpenPortForward(ctx, taskIDHex, sp.RemoteHost, sp.RemotePort)
			if err != nil {
				logf("forward: " + err.Error())
				_ = conn.Close()
				return
			}
			spliceConnStream(conn, st)
		}()
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
```

- [ ] **Step 2: Compile-check.**

Run: `go build ./cli/...`
Expected: builds clean.

- [ ] **Step 3: Commit.**
```bash
git add cli/port_forward.go
git commit -m "feat(cli): OpenPortForward client helper + RunForward engine"
```

---

### Task 4: Server handler + dispatch

**Files:**
- Create: `server/port_forward.go`
- Modify: `server/task_handler.go` (add dispatch arm)
- Test: `server/port_forward_test.go`

- [ ] **Step 1: Write the failing test.** Create `server/port_forward_test.go` (mirror `server/file_transfer_test.go`'s nil-conn / no-such-task assertions):
```go
package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestHandleOpenPortForward_NoSuchTask(t *testing.T) {
	h := &TaskHandler{Tasks: NewTaskStore(), Registry: NewRegistry()}
	req := &protocol.OpenPortForwardRequest{TaskId: protocol.TaskID{Id: [16]byte{9, 9, 9}}}
	req.SetRemoteHost([]byte("127.0.0.1"))
	resp := h.handleOpenPortForward(nil, req)
	if resp.Status != protocol.OpenPortForwardStatus_NoSuchTask {
		t.Fatalf("got status %v, want NoSuchTask", resp.Status)
	}
}
```
(If `NewTaskStore` / `NewRegistry` constructors differ, copy the exact setup from the top of `server/file_transfer_test.go` — read it first.)

- [ ] **Step 2: Run to verify it fails.**

Run: `go test ./server/ -run TestHandleOpenPortForward -v`
Expected: FAIL (undefined: handleOpenPortForward).

- [ ] **Step 3: Implement the handler.** Create `server/port_forward.go`:
```go
package server

import (
	"encoding/hex"
	"log/slog"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// handleOpenPortForward mirrors handleOpenFileTransfer: it allocates a
// client/runner stream pair, forwards a RunnerOpenPortForward request, and
// splices the two streams. Unlike file transfer it uses spliceBidi
// (tear-down-on-either-close) because a TCP forward is not a guaranteed
// both-EOF request/response. The actual net.Dial happens on the runner.
func (h *TaskHandler) handleOpenPortForward(conn ConnHandle, req *protocol.OpenPortForwardRequest) protocol.OpenPortForwardResponse {
	errResp := func(s protocol.OpenPortForwardStatus) protocol.OpenPortForwardResponse {
		return protocol.OpenPortForwardResponse{Status: s}
	}
	taskIDHex := hex.EncodeToString(req.TaskId.Id[:])
	task, ok := h.Tasks.Get(taskIDHex)
	if !ok || (task.Status != protocol.TaskStatus_Running && task.Status != protocol.TaskStatus_Detached) {
		return errResp(protocol.OpenPortForwardStatus_NoSuchTask)
	}
	runner, ok := h.Registry.Get(task.AssignedTo)
	if !ok || runner.Conn == nil {
		return errResp(protocol.OpenPortForwardStatus_RunnerOffline)
	}
	if conn == nil {
		slog.Error("port_forward: nil client conn (programmer error)")
		return errResp(protocol.OpenPortForwardStatus_InternalError)
	}
	clientStream := conn.CreateBidirectionalStream()
	if clientStream == nil {
		return errResp(protocol.OpenPortForwardStatus_InternalError)
	}
	runnerStream := runner.Conn.CreateBidirectionalStream()
	if runnerStream == nil {
		_ = clientStream.CloseBoth()
		return errResp(protocol.OpenPortForwardStatus_InternalError)
	}

	rreq := protocol.RunnerRequest{Kind: protocol.RunnerRequestType_OpenPortForward}
	body := protocol.RunnerOpenPortForwardRequest{
		TaskId:     req.TaskId,
		StreamId:   uint64(runnerStream.ID()),
		Direction:  req.Direction,
		RemotePort: req.RemotePort,
	}
	body.SetRemoteHost(req.RemoteHost)
	rreq.SetOpenPortForward(body)
	data := rreq.MustAppend([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)})
	if _, _, err := runner.Conn.SendMessage(data); err != nil {
		_ = clientStream.CloseBoth()
		_ = runnerStream.CloseBoth()
		slog.Error("port_forward: send to runner failed", "task_id", taskIDHex, "err", err)
		return errResp(protocol.OpenPortForwardStatus_InternalError)
	}
	go spliceBidi(clientStream, runnerStream, taskIDHex)
	return protocol.OpenPortForwardResponse{
		Status:   protocol.OpenPortForwardStatus_Ok,
		StreamId: uint64(clientStream.ID()),
	}
}
```

- [ ] **Step 4: Add the dispatch arm.** In `server/task_handler.go`, after the `TaskControlKind_ListFiles` case (around line 220), add:
```go
	case protocol.TaskControlKind_OpenPortForward:
		pf := req.OpenPortForward()
		if pf == nil {
			slog.Error("TaskHandler: OpenPortForward variant is nil")
			return
		}
		presp := h.handleOpenPortForward(conn, pf)
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_OpenPortForward, RequestId: req.RequestId}
		resp.SetOpenPortForward(presp)
		out := resp.MustAppend([]byte{byte(wire.ApplicationPayloadKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck
```

- [ ] **Step 5: Run to verify it passes.**

Run: `go test ./server/ -run TestHandleOpenPortForward -v`
Expected: PASS.

- [ ] **Step 6: Commit.**
```bash
git add server/port_forward.go server/task_handler.go server/port_forward_test.go
git commit -m "feat(server): handleOpenPortForward relay + dispatch"
```

---

### Task 5: Runner handler + dispatch

**Files:**
- Create: `runner/port_forward.go`
- Modify: `runner/connect.go` (add dispatch arm)

Exercised end-to-end by Task 6; compile-check here.

- [ ] **Step 1: Implement the handler.** Create `runner/port_forward.go`:
```go
package runner

import (
	"context"
	"encoding/hex"
	"net"
	"strconv"
	"sync"

	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/trsf"
)

// handleOpenPortForward waits for the relayed stream, dials the requested
// TCP target from the runner host, and splices the two. On dial failure it
// closes the stream (the server splice propagates EOF to the client, which
// closes the accepted local connection — connection-refused semantics).
func (s *Session) handleOpenPortForward(ctx context.Context, req *protocol.RunnerOpenPortForwardRequest) {
	log := s.logger()
	stream := peer.WaitForBidirectionalStream(ctx, s.Streams, trsf.StreamID(req.StreamId))
	if stream == nil {
		log.Error("port_forward: stream not visible", "stream_id", req.StreamId)
		return
	}
	taskIDHex := hex.EncodeToString(req.TaskId.Id[:])
	if s.worktreeDirFor(taskIDHex) == "" {
		log.Error("port_forward: unknown task", "task_id", taskIDHex)
		_ = stream.CloseBoth()
		return
	}
	addr := net.JoinHostPort(string(req.RemoteHost), strconv.Itoa(int(req.RemotePort)))
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		log.Info("port_forward: dial failed", "addr", addr, "err", err)
		_ = stream.CloseBoth()
		return
	}
	spliceConnStream(conn, stream)
}

// spliceConnStream pumps bytes between a net.Conn and a trsf bidi stream
// until either direction closes or errors, then tears down both. Mirrors
// cli.spliceConnStream (kept per-package; the file-transfer handlers follow
// the same no-cross-package-sharing convention).
func spliceConnStream(conn net.Conn, st trsf.BidirectionalStream) {
	var once sync.Once
	teardown := func() {
		once.Do(func() {
			_ = conn.Close()
			_ = st.CloseBoth()
		})
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer teardown()
		buf := make([]byte, 64*1024)
		for {
			n, err := conn.Read(buf)
			if n > 0 {
				if werr := st.AppendData(false, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				_ = st.AppendData(true)
				return
			}
		}
	}()
	go func() {
		defer wg.Done()
		defer teardown()
		for {
			data, eof, err := st.ReadDirect(64 * 1024)
			if err != nil {
				return
			}
			if len(data) > 0 {
				if _, werr := conn.Write(data); werr != nil {
					return
				}
			}
			if eof {
				return
			}
		}
	}()
	wg.Wait()
}
```

- [ ] **Step 2: Add the dispatch arm.** In `runner/connect.go`, after the `RunnerRequestType_ListFiles` case (around line 351), add:
```go
	case protocol.RunnerRequestType_OpenPortForward:
		pf := req.OpenPortForward()
		if pf == nil {
			return
		}
		go session.handleOpenPortForward(ctx, pf)
```

- [ ] **Step 3: Compile-check.**

Run: `go build ./runner/...`
Expected: builds clean.

- [ ] **Step 4: Commit.**
```bash
git add runner/port_forward.go runner/connect.go
git commit -m "feat(runner): handleOpenPortForward dial + splice + dispatch"
```

---

### Task 6: CLI `forward` subcommand + end-to-end integration test

**Files:**
- Modify: `cmd/harness-cli/main.go`
- Test: `integration/port_forward_test.go` (mirror an existing test under `integration/` that spins up server+runner+task — read one first for the exact harness setup)

- [ ] **Step 1: Wire the subcommand.** In `cmd/harness-cli/main.go`, add a `case "forward":` next to `case "file":` (around line 243):
```go
	case "forward":
		fs := flag.NewFlagSet("forward", flag.ExitOnError)
		var specs multiStringFlag
		fs.Var(&specs, "L", "local forward [bind:]localport:remotehost:remoteport (repeatable)")
		fs.Parse(args)
		rest := fs.Args()
		if len(rest) != 1 || len(specs) == 0 {
			fmt.Fprintln(os.Stderr, "usage: harness-cli forward <task-id> -L [bind:]localport:remotehost:remoteport [-L ...]")
			os.Exit(2)
		}
		taskID := rest[0]
		parsed := make([]cli.ForwardSpec, 0, len(specs))
		for _, s := range specs {
			sp, err := cli.ParseForwardSpec(s)
			if err != nil {
				die(err)
			}
			parsed = append(parsed, sp)
		}
		c, err := cli.Dial(ctx, parseCID())
		if err != nil {
			die(err)
		}
		defer c.Close()
		if err := c.SayHello(ctx, protocol.ClientKind_Cli); err != nil {
			die(err)
		}
		fctx, cancel := signal.NotifyContext(ctx, os.Interrupt)
		defer cancel()
		if err := cli.RunForward(fctx, c, taskID, parsed, func(s string) { fmt.Fprintln(os.Stderr, s) }); err != nil {
			die(err)
		}
```
Ensure `os/signal` is imported in `cmd/harness-cli/main.go`. If a repeatable-string flag type does not already exist in the package, add it (the `--claude-arg` flag at main.go:65 already uses a `flag.Var` slice type — reuse that type; if its name differs from `multiStringFlag`, use the existing name).

- [ ] **Step 2: Export the parser.** In `cli/port_forward.go`, rename `parseForwardSpec` → `ParseForwardSpec` (exported, since the CLI binary calls it). Update the call site in `cli/port_forward_test.go` (`parseForwardSpec` → `ParseForwardSpec`) and inside `acceptLoop`/anywhere else in `cli/port_forward.go`.

- [ ] **Step 3: Run existing unit tests to confirm no regressions.**

Run: `go test ./cli/ -run TestParseForwardSpec -v && go build ./cmd/...`
Expected: PASS + clean build.

- [ ] **Step 4: Write the integration test.** Create `integration/port_forward_test.go`. Read an existing `integration/*_test.go` that starts a server + runner + submits/attaches a task to copy the exact bootstrap, then:
```go
// Pseudocode shape — fill bootstrap from the sibling integration test:
// 1. Start server, start runner, submit an interactive/session task so a
//    Running task with a worktree exists; capture its task ID.
// 2. Start a local echo TCP server: ln, _ := net.Listen("tcp","127.0.0.1:0");
//    accept in a goroutine, io.Copy(conn, conn). echoPort := ln.Addr().(*net.TCPAddr).Port
//    (server is reachable from the runner because both run in-process here.)
// 3. spec := cli.ForwardSpec{BindAddr:"127.0.0.1", LocalPort:0-or-fixed,
//    RemoteHost:"127.0.0.1", RemotePort: echoPort}.
//    Because RunForward listens on a fixed local port, pick a free one via
//    a throwaway net.Listen then Close, or extend RunForward usage to your need.
// 4. ctx, cancel := context.WithCancel(...); go cli.RunForward(ctx, client, taskID, []cli.ForwardSpec{spec}, nil)
// 5. Dial 127.0.0.1:localport, write "ping\n", read back, assert "ping\n".
// 6. Open a SECOND connection concurrently, assert independent echo (multiplex).
// 7. Dial-failure case: a spec whose RemotePort points at a closed port —
//    assert the local accepted conn closes promptly (Read returns EOF/err).
// 8. cancel(); assert the local listener stops accepting.
```
Assertions must check: byte round-trip, two concurrent connections each echo independently, dial-failure closes the local conn, ctx-cancel stops the listener.

- [ ] **Step 5: Run the integration test.**

Run: `go test ./integration/ -run PortForward -v`
Expected: PASS.

- [ ] **Step 6: Commit.**
```bash
git add cmd/harness-cli/main.go cli/port_forward.go cli/port_forward_test.go integration/port_forward_test.go
git commit -m "feat(cli): forward subcommand + end-to-end integration test"
```

---

### Task 7: TUI surface (`p` start / `P` stop)

**Files:**
- Create: `tui/portforward.go`
- Modify: `tui/app.go` (key dispatch + modal interception + App fields)
- Test: `tui/portforward_test.go` (modal state only)

Read `tui/popup.go`, `tui/app.go:370-554`, and `tui/cmdresult.go` first — match their patterns exactly (sibling-code obligation).

- [ ] **Step 1: Write the failing modal test.** Create `tui/portforward_test.go`:
```go
package tui

import "testing"

func TestPortForwardModal_OpenClose(t *testing.T) {
	var m PortForwardModal
	if m.IsOpen() {
		t.Fatal("new modal should be closed")
	}
	m.Open("abc123")
	if !m.IsOpen() || m.TaskID() != "abc123" {
		t.Fatalf("after Open: open=%v task=%q", m.IsOpen(), m.TaskID())
	}
	m.Close()
	if m.IsOpen() {
		t.Fatal("after Close: should be closed")
	}
}
```

- [ ] **Step 2: Run to verify it fails.**

Run: `go test ./tui/ -run TestPortForwardModal -v`
Expected: FAIL (undefined: PortForwardModal).

- [ ] **Step 3: Implement the modal + session + Cmd.** Create `tui/portforward.go`:
```go
package tui

import (
	"context"
	"fmt"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/on-keyday/agent-harness/cli"
)

// PortForwardModal prompts for one -L spec for a selected task.
type PortForwardModal struct {
	open   bool
	taskID string
	input  textinput.Model
}

func (m *PortForwardModal) IsOpen() bool   { return m.open }
func (m *PortForwardModal) TaskID() string { return m.taskID }

func (m *PortForwardModal) Open(taskID string) {
	m.taskID = taskID
	if m.input.Prompt == "" {
		m.input = textinput.New()
		m.input.Placeholder = "[bind:]localport:remotehost:remoteport"
	}
	m.input.SetValue("")
	m.input.Focus()
	m.open = true
}

func (m *PortForwardModal) Close() {
	m.open = false
	m.input.Blur()
}

func (m *PortForwardModal) Spec() string { return m.input.Value() }

func (m *PortForwardModal) Update(msg tea.Msg) (PortForwardModal, tea.Cmd) {
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return *m, cmd
}

func (m *PortForwardModal) View() string {
	if !m.open {
		return ""
	}
	return "port-forward " + m.taskID[:min(12, len(m.taskID))] + "  -L " + m.input.View()
}

// PortForwardSession tracks a running forward so it can be cancelled.
type PortForwardSession struct {
	TaskID string
	Spec   string
	Cancel context.CancelFunc
}

// PortForwardStatusMsg carries a line to append to cmdresult.
type PortForwardStatusMsg struct{ Line string }

// PortForwardStartedMsg registers a started forward in the App.
type PortForwardStartedMsg struct {
	TaskID  string
	Spec    string
	Cancel  context.CancelFunc
}

// DoStartPortForward parses the spec and starts a background forward using
// the long-lived a.client (NOT a fresh dial — file-transfer Do* convention).
func DoStartPortForward(c *cli.Client, taskID, spec string, send func(tea.Msg)) tea.Cmd {
	return func() tea.Msg {
		sp, err := cli.ParseForwardSpec(spec)
		if err != nil {
			return PortForwardStatusMsg{Line: "forward: " + err.Error()}
		}
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			err := cli.RunForward(ctx, c, taskID, []cli.ForwardSpec{sp}, func(s string) {
				send(PortForwardStatusMsg{Line: s})
			})
			if err != nil {
				send(PortForwardStatusMsg{Line: "forward: " + err.Error()})
			}
			send(PortForwardStatusMsg{Line: fmt.Sprintf("forward stopped: %s", taskID[:min(12, len(taskID))])})
		}()
		return PortForwardStartedMsg{TaskID: taskID, Spec: spec, Cancel: cancel}
	}
}
```
(If the package already defines a `min` helper, drop this file's reliance on a new one and reuse it; otherwise add a small `min` in this file.)

- [ ] **Step 4: Wire into `tui/app.go`.**

Add fields to the `App` struct:
```go
	portForwardModal PortForwardModal
	activeForwards   map[string]*PortForwardSession
```
Initialize `activeForwards` where the App is constructed (next to other map/model initializers): `a.activeForwards = map[string]*PortForwardSession{}`.

In `App.Update`, in the modal-intercept region (near the popup/filepicker checks ~line 386), add before the generic key handling:
```go
	if a.portForwardModal.IsOpen() {
		switch m := msg.(type) {
		case tea.KeyMsg:
			switch m.Type {
			case tea.KeyEsc:
				a.portForwardModal.Close()
				return a, nil
			case tea.KeyEnter:
				spec := a.portForwardModal.Spec()
				taskID := a.portForwardModal.TaskID()
				a.portForwardModal.Close()
				send := a.program.Send // capture the *tea.Program send fn the App already holds
				return a, DoStartPortForward(a.client, taskID, spec, send)
			}
		}
		var cmd tea.Cmd
		a.portForwardModal, cmd = a.portForwardModal.Update(msg)
		return a, cmd
	}
```
(Use whatever the App's existing handle to `program.Send` is — the interactive flow already posts messages back via `program.Send` per `tui/interactive.go`; reuse that exact field/closure. If the App stores `*tea.Program`, use it; do not introduce a new global.)

In the `tea.KeyMsg` per-key block (after the `r`/`R` case ~line 530), add:
```go
		if a.focus == focusTasks && msg.String() == "p" {
			taskID := a.tasks.SelectedID()
			if taskID == "" {
				a.cmdresult.Append(WarnStyle.Render("no task selected"))
				return a, nil
			}
			a.portForwardModal.Open(taskID)
			return a, nil
		}
		if a.focus == focusTasks && msg.String() == "P" {
			taskID := a.tasks.SelectedID()
			if sess, ok := a.activeForwards[taskID]; ok {
				sess.Cancel()
				delete(a.activeForwards, taskID)
				a.cmdresult.Append(OKStyle.Render("forward stopping: ") + taskID[:min(12, len(taskID))])
			} else {
				a.cmdresult.Append(WarnStyle.Render("no active forward for selected task"))
			}
			return a, nil
		}
```

Add Msg handling in `App.Update`'s message switch (next to other custom Msg cases):
```go
	case PortForwardStartedMsg:
		a.activeForwards[msg.TaskID] = &PortForwardSession{TaskID: msg.TaskID, Spec: msg.Spec, Cancel: msg.Cancel}
		a.cmdresult.Append(OKStyle.Render("forward started: ") + msg.Spec)
		return a, nil
	case PortForwardStatusMsg:
		a.cmdresult.Append(msg.Line)
		return a, nil
```

If the modal is open, render `a.portForwardModal.View()` in the View() output near where the popup/filepicker overlays render.

- [ ] **Step 5: Run the modal test + build.**

Run: `go test ./tui/ -run TestPortForwardModal -v && go build ./cmd/harness-tui/`
Expected: PASS + clean build.

- [ ] **Step 6: Commit.**
```bash
git add tui/portforward.go tui/portforward_test.go tui/app.go
git commit -m "feat(tui): p/P port-forward start/stop"
```

---

### Task 8: Full build, vet, and test sweep

**Files:** none (verification task).

- [ ] **Step 1: Build everything (incl. WASM/WebUI compile path).**

Run: `make check`
Expected: clean build of `./...` and the WASM target. (The WebUI does not get a forward UI in this plan; it only needs to keep compiling — `cli.OpenPortForward` is a `*Client` method available to it but unused.)

- [ ] **Step 2: Vet.**

Run: `make vet`
Expected: clean.

- [ ] **Step 3: Full test sweep.**

Run: `make test`
Expected: all PASS, including `runner/protocol`, `cli`, `server`, `tui`, `integration`.

- [ ] **Step 4: Commit (if any fixups were needed).**
```bash
git add -A
git commit -m "chore: port-forward build/vet/test fixups"
```

---

## Self-Review

**Spec coverage:**
- `-L` semantics, client-side listener, runner-side dial → Tasks 3/5/6. ✓
- Server-relayed sideband stream, file-transfer skeleton reuse → Task 4. ✓
- Direction-neutral schema, `Remote` reserved → Task 1. ✓
- Arbitrary `host:port` dial target, client-specified → schema (Task 1) + runner `net.Dial` (Task 5). ✓
- `bind` defaults to loopback → Task 2 parser. ✓
- `Ok` ≠ reachable; dial-fail closes stream → Task 5 + Task 3 client closes local conn on stream EOF. ✓
- No per-connection ack frame → Tasks 3/4/5 (no ack in the path). ✓
- Non-terminal-task requirement → Task 4 (`Running`/`Detached` check). ✓
- Per-connection error isolation → Task 3 `acceptLoop` (per-conn goroutine). ✓
- Teardown via `spliceBidi` (either-side) → Task 4 server splice + Tasks 3/5 `spliceConnStream`. ✓
- Shared engine over long-lived `*cli.Client`, TUI reuses it → Task 3 methods on `*Client`, Task 7 `DoStartPortForward(a.client,...)`. ✓
- TUI `p` start modal / `P` stop / `cmdresult` status → Task 7. ✓
- Tests: spec parser unit, schema round-trip, server status, integration echo/concurrent/dial-fail → Tasks 1/2/4/6. ✓

**Placeholder scan:** Integration test (Task 6 Step 4) is given as a shaped skeleton because the exact bootstrap must be copied from an existing `integration/*_test.go`; the assertions it must make are fully enumerated. No other placeholders.

**Type consistency:** `ParseForwardSpec` (exported from Task 6 onward), `ForwardSpec{BindAddr,LocalPort,RemoteHost,RemotePort}`, `RunForward(ctx, *Client, taskID, []ForwardSpec, logf)`, `(*Client).OpenPortForward(ctx, taskIDHex, remoteHost, remotePort)`, `spliceConnStream(net.Conn, trsf.BidirectionalStream)` (both `cli` and `runner`), `handleOpenPortForward` (server + runner), generated `protocol.PortForwardDirection_Local`, `protocol.OpenPortForwardStatus_*`, `protocol.TaskControlKind_OpenPortForward`, `protocol.RunnerRequestType_OpenPortForward`, setters `SetRemoteHost`/`SetOpenPortForward` — used consistently across tasks.
