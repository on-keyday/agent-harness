# Remote Port Forwarding Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `ssh -R`-style remote port forwarding (runner listens, client dials) across protocol, server, runner, and CLI/TUI, and rework TUI forward tracking to manage multiple concurrent forwards per task.

**Architecture:** Approach A from the spec — the client opens one persistent control bidi stream at registration; the server writes connection-arrival notifications onto it; per-connection data streams remain server-created and are picked up by the client via the existing `WaitForBidirectionalStream(id)` path (no client accept-loop → avoids the accept-queue-wedge class). The runner runs a TCP listener per registration and reports each accepted connection to the server.

**Tech Stack:** Go; brgen `.bgn` schema regenerated via `make protoregen`; trsf bidi streams; bubbletea TUI.

Spec: `docs/superpowers/specs/2026-06-06-remote-port-forwarding-design.md`.

---

## File Structure

- `runner/protocol/message.bgn` — schema source (edit), regenerated to `runner/protocol/message.go` (generated, DO NOT hand-edit).
- `runner/protocol/port_forward_test.go` — protocol round-trip tests (extend).
- `server/port_forward.go` — remote registration + RemoteForwardConn handling (extend).
- `server/remote_forward_registry.go` — NEW: forwardId → registration map on TaskHandler.
- `server/runner_handler.go` — dispatch `RemoteForwardConn` (extend).
- `server/port_forward_test.go` — server tests (extend).
- `runner/port_forward.go` — remote listener + accept loop + ClosePortForward (extend).
- `runner/connect.go` — dispatch `ClosePortForward` (extend, near line 352).
- `runner/port_forward_test.go` — NEW or extend: runner listener tests.
- `cli/port_forward.go` — `ParseRemoteForwardSpec`, `OpenRemoteForward`, `RunRemoteForward` (extend).
- `cli/port_forward_test.go` — CLI parse + run tests (extend).
- `cmd/harness-cli/main.go` — wire `-R` flag (extend, near line 340).
- `tui/portforward.go` — modal mode + id-keyed session selection helpers (extend).
- `tui/app.go` — `activeForwards` rework, `b`/`B` keys, picker (extend).
- `tui/portforward_test.go` — selection-logic unit tests (extend).
- `integration/port_forward_test.go` — e2e remote forward test (extend).

---

## Task 1: Protocol schema

**Files:**
- Modify: `runner/protocol/message.bgn` (enums + 3 formats + 2 new formats)
- Regenerate: `runner/protocol/message.go` (via `make protoregen`)
- Test: `runner/protocol/port_forward_test.go`

- [ ] **Step 1: Edit `message.bgn` — enums**

`PortForwardDirection` (lines ~719-722): keep `local`/`remote`, update the `remote` comment to `# runner listens, client dials (SSH -R).`

`OpenPortForwardStatus` (after `internal_error`, line ~729) add:
```
    bind_failed    = "bind_failed"
```

`RunnerRequestType` (after `open_port_forward`, line ~24) add:
```
    close_port_forward
```

`RunnerMessageType` (after `request_chained_relay`, line ~11) add:
```
    remote_forward_conn
```

- [ ] **Step 2: Edit `message.bgn` — extend existing formats**

`OpenPortForwardRequest` (lines ~731-736) becomes:
```
format OpenPortForwardRequest:
    task_id           :TaskID
    direction         :PortForwardDirection
    remote_host_len   :u16
    remote_host       :[remote_host_len]u8
    remote_port       :u16
    bind_addr_len     :u16
    bind_addr         :[bind_addr_len]u8
    bind_port         :u16
    control_stream_id :u64
```

`OpenPortForwardResponse` (lines ~738-740) becomes:
```
format OpenPortForwardResponse:
    status     :OpenPortForwardStatus
    stream_id  :u64
    forward_id :u64
```

`RunnerOpenPortForwardRequest` (lines ~742-748) becomes:
```
format RunnerOpenPortForwardRequest:
    task_id           :TaskID
    stream_id         :u64
    direction         :PortForwardDirection
    remote_host_len   :u16
    remote_host       :[remote_host_len]u8
    remote_port       :u16
    bind_addr_len     :u16
    bind_addr         :[bind_addr_len]u8
    bind_port         :u16
    forward_id        :u64
```

- [ ] **Step 3: Edit `message.bgn` — new formats + dispatch wiring**

Add two formats near the other port-forward formats (after `RunnerOpenPortForwardRequest`):
```
format ClosePortForwardRequest:
    forward_id :u64

format RemoteForwardConn:
    forward_id :u64
    stream_id  :u64

format RemoteForwardConnNotify:
    stream_id :u64
```

In `format RunnerRequest` match block (after line 165) add:
```
        RunnerRequestType.close_port_forward => close_port_forward :ClosePortForwardRequest
```

In `format RunnerMessage` match block (after line 150) add:
```
        RunnerMessageType.remote_forward_conn => remote_forward_conn :RemoteForwardConn
```

- [ ] **Step 4: Regenerate Go from schema**

Run: `make protoregen ARGS='runner/protocol/message.bgn'`
Expected: regenerates `runner/protocol/message.go`. First run downloads `~/.cache/brgen-kit` (~20 MB; needs network). Then:
Run: `go build ./runner/protocol/` → Expected: PASS.

(If `make protoregen` cannot reach the network, STOP and report — the schema cannot be regenerated offline and the rest of the plan depends on the generated accessors.)

- [ ] **Step 5: Write failing round-trip test for the new remote fields**

Append to `runner/protocol/port_forward_test.go`:
```go
func TestOpenPortForwardRequest_RemoteRoundTrip(t *testing.T) {
	in := OpenPortForwardRequest{
		Direction:       PortForwardDirection_Remote,
		RemotePort:      5432,
		BindPort:        15432,
		ControlStreamId: 77,
	}
	in.SetRemoteHost([]byte("127.0.0.1"))
	in.SetBindAddr([]byte("127.0.0.1"))
	b, err := in.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	var got OpenPortForwardRequest
	if _, err := got.Parse(b); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Direction != PortForwardDirection_Remote || got.RemotePort != 5432 ||
		got.BindPort != 15432 || got.ControlStreamId != 77 ||
		string(got.RemoteHost) != "127.0.0.1" || string(got.BindAddr) != "127.0.0.1" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestRemoteForwardConn_RoundTrip(t *testing.T) {
	in := RemoteForwardConn{ForwardId: 9, StreamId: 42}
	b, err := in.Append(nil)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	var got RemoteForwardConn
	if _, err := got.Parse(b); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.ForwardId != 9 || got.StreamId != 42 {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}
```

(Confirm the exact method names — `Append`/`Parse`/`SetRemoteHost` — against the existing test in this file; match whatever the generated code emits. The existing `TestRunnerOpenPortForwardRequest...` test shows the right calls.)

- [ ] **Step 6: Run tests**

Run: `go test ./runner/protocol/ -run PortForward -v`
Expected: PASS (generated accessors exist and round-trip).

- [ ] **Step 7: Commit**

```bash
git add runner/protocol/message.bgn runner/protocol/message.go runner/protocol/port_forward_test.go
git commit -m "feat(protocol): remote port-forward schema (bind/dial/forwardId, ClosePortForward, RemoteForwardConn)"
```

---

## Task 2: Server — remote-forward registry + registration

**Files:**
- Create: `server/remote_forward_registry.go`
- Modify: `server/port_forward.go`
- Test: `server/port_forward_test.go`

- [ ] **Step 1: Write the registry (no test yet; pure data holder used by later tested handlers)**

Create `server/remote_forward_registry.go`:
```go
package server

import (
	"sync"

	"github.com/on-keyday/agent-harness/trsf"
)

// remoteForward is one active ssh -R registration: the client control stream the
// server pushes connection-arrival notifications onto, plus the conn it allocates
// per-connection client data streams against.
type remoteForward struct {
	forwardID uint64
	taskIDHex string
	runnerID  string
	control   trsf.BidirectionalStream
	clientCxn ConnHandle
}

// remoteForwardRegistry maps server-assigned forwardId → registration. Safe for
// concurrent use. Lives on TaskHandler.
type remoteForwardRegistry struct {
	mu   sync.Mutex
	next uint64
	m    map[uint64]*remoteForward
}

func newRemoteForwardRegistry() *remoteForwardRegistry {
	return &remoteForwardRegistry{m: map[uint64]*remoteForward{}}
}

func (r *remoteForwardRegistry) add(rf *remoteForward) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	rf.forwardID = r.next
	r.m[r.next] = rf
	return r.next
}

func (r *remoteForwardRegistry) get(id uint64) (*remoteForward, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rf, ok := r.m[id]
	return rf, ok
}

func (r *remoteForwardRegistry) remove(id uint64) (*remoteForward, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rf, ok := r.m[id]
	if ok {
		delete(r.m, id)
	}
	return rf, ok
}
```

- [ ] **Step 2: Add the registry field to `TaskHandler`**

Find the `TaskHandler` struct definition (grep `type TaskHandler struct` in `server/`). Add a field:
```go
	remoteForwards *remoteForwardRegistry
```
and initialise it wherever `TaskHandler` is constructed (grep `TaskHandler{` in `server/`): add `remoteForwards: newRemoteForwardRegistry(),`. If construction is field-by-field after a `&TaskHandler{...}`, lazy-init at first use instead: add a helper
```go
func (h *TaskHandler) rforwards() *remoteForwardRegistry {
	if h.remoteForwards == nil {
		h.remoteForwards = newRemoteForwardRegistry()
	}
	return h.remoteForwards
}
```
(Pick whichever matches the existing construction style; prefer constructor init if there is a single constructor.)

- [ ] **Step 3: Write the failing test for remote registration**

Append to `server/port_forward_test.go` (mirror the existing local test's harness — reuse its fakes for `ConnHandle`/runner conn). The test drives `handleOpenPortForward` with `Direction_Remote` and asserts (a) status Ok + a non-zero ForwardId, (b) a `RunnerRequest{OpenPortForward, Direction=Remote, BindPort}` was sent to the runner, (c) the registration is stored.
```go
func TestHandleOpenPortForward_RemoteRegisters(t *testing.T) {
	h, clientConn, runnerConn := newPortForwardTestHandler(t) // existing helper; adapt name
	// register a running task assigned to the runner (reuse existing helper)
	taskID := seedRunningTask(t, h, runnerConn)

	// client opens a control stream first; pass its id in the request.
	ctrl := clientConn.CreateBidirectionalStream()

	req := &protocol.OpenPortForwardRequest{
		TaskId:          mustTaskID(taskID),
		Direction:       protocol.PortForwardDirection_Remote,
		RemotePort:      5432,
		BindPort:        15432,
		ControlStreamId: uint64(ctrl.ID()),
	}
	req.SetRemoteHost([]byte("127.0.0.1"))
	req.SetBindAddr([]byte("127.0.0.1"))

	resp := h.handleOpenPortForward(clientConn, req)
	if resp.Status != protocol.OpenPortForwardStatus_Ok {
		t.Fatalf("status = %v, want Ok", resp.Status)
	}
	if resp.ForwardId == 0 {
		t.Fatal("ForwardId should be non-zero")
	}
	if _, ok := h.rforwards().get(resp.ForwardId); !ok {
		t.Fatal("registration not stored")
	}
	// assert a listen RunnerRequest reached the runner (decode runnerConn's last sent msg)
	got := decodeLastRunnerRequest(t, runnerConn) // existing/added helper
	if got.Kind != protocol.RunnerRequestType_OpenPortForward {
		t.Fatalf("runner req kind = %v", got.Kind)
	}
	body := got.OpenPortForward()
	if body.Direction != protocol.PortForwardDirection_Remote || body.BindPort != 15432 || body.ForwardId != resp.ForwardId {
		t.Fatalf("runner req body = %+v", body)
	}
}
```
(The helper names `newPortForwardTestHandler`/`seedRunningTask`/`decodeLastRunnerRequest`/`mustTaskID` may already exist under different names in `server/port_forward_test.go` / `server/fakes_test.go`; reuse the real ones. Do not invent fakes that already exist — grep first.)

- [ ] **Step 4: Run test to verify it fails**

Run: `go test ./server/ -run TestHandleOpenPortForward_RemoteRegisters -v`
Expected: FAIL (handleOpenPortForward has no remote branch yet).

- [ ] **Step 5: Implement the remote branch in `handleOpenPortForward`**

In `server/port_forward.go`, near the top of `handleOpenPortForward` (after the task/runner lookup that yields `task`, `runner`, and validates them — reuse the existing lookup code, do not duplicate the error paths), branch on direction before the existing local stream-pair allocation:
```go
	if req.Direction == protocol.PortForwardDirection_Remote {
		return h.registerRemoteForward(conn, req, taskIDHex, runner)
	}
```
Add the method:
```go
// registerRemoteForward records the client control stream, asks the runner to
// open a listener, and returns the assigned forwardId. Per-connection data
// streams are created later, in handleRemoteForwardConn.
func (h *TaskHandler) registerRemoteForward(conn ConnHandle, req *protocol.OpenPortForwardRequest, taskIDHex string, runner RunnerEntry) protocol.OpenPortForwardResponse {
	errResp := func(s protocol.OpenPortForwardStatus) protocol.OpenPortForwardResponse {
		return protocol.OpenPortForwardResponse{Status: s}
	}
	ctrl := peer.WaitForBidirectionalStream(context.Background(), conn, trsf.StreamID(req.ControlStreamId))
	if ctrl == nil {
		return errResp(protocol.OpenPortForwardStatus_InternalError)
	}
	rf := &remoteForward{taskIDHex: taskIDHex, runnerID: runner.ID, control: ctrl, clientCxn: conn}
	fid := h.rforwards().add(rf)

	rreq := protocol.RunnerRequest{Kind: protocol.RunnerRequestType_OpenPortForward}
	body := protocol.RunnerOpenPortForwardRequest{
		TaskId:    req.TaskId,
		Direction: protocol.PortForwardDirection_Remote,
		BindPort:  req.BindPort,
		ForwardId: fid,
	}
	body.SetBindAddr(req.BindAddr)
	rreq.SetOpenPortForward(body)
	data := rreq.MustAppend([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)})
	if _, _, err := runner.Conn.SendMessage(data); err != nil {
		h.rforwards().remove(fid)
		_ = ctrl.CloseBoth()
		return errResp(protocol.OpenPortForwardStatus_InternalError)
	}

	// Tear down the listener when the client control stream closes.
	go h.watchRemoteForwardControl(rf)

	return protocol.OpenPortForwardResponse{Status: protocol.OpenPortForwardStatus_Ok, ForwardId: fid}
}
```
(Match `RunnerEntry` / `runner.ID` / `conn` types to the real signatures used in the existing local branch — adapt names to the actual ones in this file.)

- [ ] **Step 6: Run test to verify it passes**

Run: `go test ./server/ -run TestHandleOpenPortForward_RemoteRegisters -v`
Expected: PASS. (`watchRemoteForwardControl` is added in Task 3; for this step, add a temporary no-op `func (h *TaskHandler) watchRemoteForwardControl(rf *remoteForward) {}` so it compiles, replaced in Task 3.)

- [ ] **Step 7: Commit**

```bash
git add server/remote_forward_registry.go server/port_forward.go server/port_forward_test.go
git commit -m "feat(server): register remote port-forward + ask runner to listen"
```

---

## Task 3: Server — connection arrival + teardown

**Files:**
- Modify: `server/port_forward.go`, `server/runner_handler.go`
- Test: `server/port_forward_test.go`

- [ ] **Step 1: Write failing test for RemoteForwardConn handling**

Append to `server/port_forward_test.go`: after registering (reuse Task 2 setup), simulate the runner sending `RemoteForwardConn{forwardId, runnerStreamId}` and assert (a) a client data stream was created on the client conn, (b) a `RemoteForwardConnNotify{StreamId}` was written to the control stream.
```go
func TestHandleRemoteForwardConn_NotifiesClient(t *testing.T) {
	h, clientConn, runnerConn := newPortForwardTestHandler(t)
	taskID := seedRunningTask(t, h, runnerConn)
	ctrl := clientConn.CreateBidirectionalStream()
	req := &protocol.OpenPortForwardRequest{
		TaskId: mustTaskID(taskID), Direction: protocol.PortForwardDirection_Remote,
		RemotePort: 5432, BindPort: 15432, ControlStreamId: uint64(ctrl.ID()),
	}
	req.SetRemoteHost([]byte("127.0.0.1"))
	req.SetBindAddr([]byte("127.0.0.1"))
	resp := h.handleOpenPortForward(clientConn, req)

	runnerStream := runnerConn.CreateBidirectionalStream()
	h.handleRemoteForwardConn(runnerConn, &protocol.RemoteForwardConn{
		ForwardId: resp.ForwardId, StreamId: uint64(runnerStream.ID()),
	})

	// The control stream should now carry a RemoteForwardConnNotify.
	notifyBytes := ctrl.(*fakeBidiStream).WaitWritten(t, 1) // adapt to real fake
	var n protocol.RemoteForwardConnNotify
	if _, err := n.Parse(notifyBytes); err != nil {
		t.Fatalf("parse notify: %v", err)
	}
	if n.StreamId == 0 {
		t.Fatal("notify StreamId should be the new client data stream id")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./server/ -run TestHandleRemoteForwardConn_NotifiesClient -v`
Expected: FAIL (no `handleRemoteForwardConn`).

- [ ] **Step 3: Implement `handleRemoteForwardConn` + control watcher**

In `server/port_forward.go`:
```go
// handleRemoteForwardConn fires when a runner reports a new connection on a
// remote-forward listener. It allocates a client-side data stream, splices it
// to the runner's stream, and tells the client (over the control stream) to
// dial its local target and pick up the stream by id.
func (h *TaskHandler) handleRemoteForwardConn(runnerConn ConnHandle, msg *protocol.RemoteForwardConn) {
	rf, ok := h.rforwards().get(msg.ForwardId)
	if !ok {
		return // registration gone; runner stream will EOF and clean up
	}
	runnerStream := peer.WaitForBidirectionalStream(context.Background(), runnerConn, trsf.StreamID(msg.StreamId))
	if runnerStream == nil {
		return
	}
	clientStream := rf.clientCxn.CreateBidirectionalStream()
	if clientStream == nil {
		_ = runnerStream.CloseBoth()
		return
	}
	notify := protocol.RemoteForwardConnNotify{StreamId: uint64(clientStream.ID())}
	if err := rf.control.AppendData(false, notify.MustAppend(nil)); err != nil {
		_ = clientStream.CloseBoth()
		_ = runnerStream.CloseBoth()
		return
	}
	go spliceBidi(clientStream, runnerStream, rf.taskIDHex)
}

// watchRemoteForwardControl tears the forward down when the client closes the
// control stream: it tells the runner to stop listening and drops the registration.
func (h *TaskHandler) watchRemoteForwardControl(rf *remoteForward) {
	for {
		_, eof, err := rf.control.ReadDirect(4096) // client never sends; we wait for EOF/err
		if eof || err != nil {
			break
		}
	}
	if _, ok := h.rforwards().remove(rf.forwardID); !ok {
		return
	}
	runner, ok := h.Registry.Get(rf.runnerID)
	if !ok || runner.Conn == nil {
		return
	}
	rreq := protocol.RunnerRequest{Kind: protocol.RunnerRequestType_ClosePortForward}
	rreq.SetClosePortForward(protocol.ClosePortForwardRequest{ForwardId: rf.forwardID})
	data := rreq.MustAppend([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)})
	_, _, _ = runner.Conn.SendMessage(data)
}
```
(Replace the temporary no-op `watchRemoteForwardControl` from Task 2 with this. Adapt `h.Registry.Get`/`runner.Conn` to the real accessors used in the existing local handler.)

- [ ] **Step 4: Wire RemoteForwardConn dispatch in `server/runner_handler.go`**

In the `switch msg.Kind` block (around line 64), add a case mirroring the others:
```go
	case protocol.RunnerMessageType_RemoteForwardConn:
		if rfc := msg.RemoteForwardConn(); rfc != nil && h.taskHandler != nil {
			h.taskHandler.handleRemoteForwardConn(conn, rfc)
		}
```
(Match the real way `RunnerHandler` reaches the `TaskHandler` and the conn — grep how existing cases call into handlers; adapt the receiver/field names. If `RunnerHandler` does not currently hold a `*TaskHandler`/conn, add the minimal wiring following how `ChainedRelay` is plumbed.)

- [ ] **Step 5: Run tests**

Run: `go test ./server/ -run 'TestHandleRemoteForwardConn|TestHandleOpenPortForward_Remote' -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add server/port_forward.go server/runner_handler.go server/port_forward_test.go
git commit -m "feat(server): relay remote-forward connections + teardown on control close"
```

---

## Task 4: Runner — listener + accept loop + close

**Files:**
- Modify: `runner/port_forward.go`, `runner/connect.go`
- Test: `runner/port_forward_test.go` (create if absent)

- [ ] **Step 1: Write failing test for the remote listener accept→notify path**

Create/extend `runner/port_forward_test.go`. Test `startRemoteForwardListener` (added below): start it on `127.0.0.1:0`, dial the bound port, and assert the runner sends a `RunnerMessage{RemoteForwardConn}` with the registration's forwardId. Use a fake `Sender`/`Streams` mirroring existing runner tests (grep `runner/*_test.go` for the harness).
```go
func TestRemoteForwardListener_AcceptsAndNotifies(t *testing.T) {
	s, sent := newRunnerSessionTestHarness(t) // existing/added: captures sent RunnerMessages + creates streams
	ln, err := s.startRemoteForwardListener(context.Background(), "task-x", 7 /*forwardId*/, "127.0.0.1", 0)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	msg := waitForRunnerMessage(t, sent, protocol.RunnerMessageType_RemoteForwardConn)
	if msg.RemoteForwardConn().ForwardId != 7 {
		t.Fatalf("forwardId = %d, want 7", msg.RemoteForwardConn().ForwardId)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./runner/ -run TestRemoteForwardListener_AcceptsAndNotifies -v`
Expected: FAIL (no `startRemoteForwardListener`).

- [ ] **Step 3: Implement remote branch + listener in `runner/port_forward.go`**

Extend `handleOpenPortForward` to branch on direction at the top (before the existing wait-for-stream + dial, which is the local path):
```go
func (s *Session) handleOpenPortForward(ctx context.Context, req *protocol.RunnerOpenPortForwardRequest) {
	if req.Direction == protocol.PortForwardDirection_Remote {
		s.startRemoteForward(ctx, req)
		return
	}
	// ... existing local path unchanged ...
}
```
Add the listener + accept loop, and a per-(task,forwardId) listener registry on the Session:
```go
// startRemoteForward opens a TCP listener; each accepted connection becomes a
// new stream to the server, announced via RunnerMessage{RemoteForwardConn}.
func (s *Session) startRemoteForward(ctx context.Context, req *protocol.RunnerOpenPortForwardRequest) {
	log := s.logger()
	taskIDHex := hex.EncodeToString(req.TaskId.Id[:])
	if s.worktreeDirFor(taskIDHex) == "" {
		log.Error("remote_forward: unknown task", "task_id", taskIDHex)
		return
	}
	ln, err := s.startRemoteForwardListener(ctx, taskIDHex, req.ForwardId, string(req.BindAddr), int(req.BindPort))
	if err != nil {
		log.Info("remote_forward: listen failed", "err", err)
		return // (BindFailed response path: see note in Step 5)
	}
	s.trackRemoteForwardListener(req.ForwardId, ln)
}

func (s *Session) startRemoteForwardListener(ctx context.Context, taskIDHex string, forwardID uint64, bindAddr string, bindPort int) (net.Listener, error) {
	if bindAddr == "" {
		bindAddr = "127.0.0.1"
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(bindAddr, strconv.Itoa(bindPort)))
	if err != nil {
		return nil, err
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			go s.onRemoteForwardConn(ctx, forwardID, conn)
		}
	}()
	return ln, nil
}

// onRemoteForwardConn makes a stream for one accepted connection, tells the
// server, and splices.
func (s *Session) onRemoteForwardConn(ctx context.Context, forwardID uint64, conn net.Conn) {
	stream := s.Streams.CreateBidirectionalStream()
	if stream == nil {
		_ = conn.Close()
		return
	}
	msg := protocol.RunnerMessage{Kind: protocol.RunnerMessageType_RemoteForwardConn}
	msg.SetRemoteForwardConn(protocol.RemoteForwardConn{ForwardId: forwardID, StreamId: uint64(stream.ID())})
	if err := s.Sender.Send(msg.MustAppend([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)})); err != nil {
		_ = stream.CloseBoth()
		_ = conn.Close()
		return
	}
	spliceConnStream(conn, stream) // existing helper in this file
}
```
Add the listener registry to `Session` (struct field + methods). Grep the `Session` struct (likely in `runner/session.go`); add:
```go
	rforwardMu  sync.Mutex
	rforwardLns map[uint64]net.Listener
```
and:
```go
func (s *Session) trackRemoteForwardListener(id uint64, ln net.Listener) {
	s.rforwardMu.Lock()
	defer s.rforwardMu.Unlock()
	if s.rforwardLns == nil {
		s.rforwardLns = map[uint64]net.Listener{}
	}
	s.rforwardLns[id] = ln
}

func (s *Session) closeRemoteForwardListener(id uint64) {
	s.rforwardMu.Lock()
	ln := s.rforwardLns[id]
	delete(s.rforwardLns, id)
	s.rforwardMu.Unlock()
	if ln != nil {
		_ = ln.Close()
	}
}
```
(Match `s.Streams.CreateBidirectionalStream()` and `s.Sender.Send` to the real accessors — grep how `runner/session.go` sends RunnerMessages; line ~504 shows `s.Sender.Send(m.MustAppend(...))`.)

- [ ] **Step 4: Handle ClosePortForward in `runner/connect.go`**

In the dispatch switch (after the `RunnerRequestType_OpenPortForward` case near line 352) add:
```go
	case protocol.RunnerRequestType_ClosePortForward:
		cpf := req.ClosePortForward()
		if cpf == nil {
			return
		}
		session.closeRemoteForwardListener(cpf.ForwardId)
```

- [ ] **Step 5: Run tests**

Run: `go test ./runner/ -run TestRemoteForwardListener -v`
Expected: PASS.

Note (BindFailed): wiring the runner→server `BindFailed` *response* needs a runner→server message; for v1 the listener error is logged and the client simply never gets connections (the OpenPortForwardResponse already returned Ok at registration time, before the runner attempted to listen — see Task 2). A precise BindFailed requires making registration await the runner's listen result. **Decision for this plan: keep registration fire-and-forget (Ok on send), log listen failures on the runner.** If tighter feedback is wanted, that is a follow-up (add a runner→server ack). Update the spec's error-handling note accordingly if you change this.

- [ ] **Step 6: Commit**

```bash
git add runner/port_forward.go runner/connect.go runner/session.go runner/port_forward_test.go
git commit -m "feat(runner): remote port-forward listener + accept→notify + close"
```

---

## Task 5: CLI — parse, register, run

**Files:**
- Modify: `cli/port_forward.go`, `cmd/harness-cli/main.go`
- Test: `cli/port_forward_test.go`

- [ ] **Step 1: Write failing test for `ParseRemoteForwardSpec`**

Append to `cli/port_forward_test.go`:
```go
func TestParseRemoteForwardSpec(t *testing.T) {
	got, err := ParseRemoteForwardSpec("8080:localhost:3000")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.BindAddr != "127.0.0.1" || got.RunnerPort != 8080 || got.DialHost != "localhost" || got.DialPort != 3000 {
		t.Fatalf("got %+v", got)
	}
	got2, err := ParseRemoteForwardSpec("0.0.0.0:8080:localhost:3000")
	if err != nil || got2.BindAddr != "0.0.0.0" || got2.RunnerPort != 8080 {
		t.Fatalf("bind form: %+v err=%v", got2, err)
	}
	if _, err := ParseRemoteForwardSpec("nope"); err == nil {
		t.Fatal("expected error on bad spec")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cli/ -run TestParseRemoteForwardSpec -v`
Expected: FAIL (undefined).

- [ ] **Step 3: Implement `RemoteForwardSpec` + parser**

In `cli/port_forward.go`:
```go
// RemoteForwardSpec is one parsed -R forward: the runner listens on
// BindAddr:RunnerPort, and for each accepted connection the client dials
// DialHost:DialPort.
type RemoteForwardSpec struct {
	BindAddr   string
	RunnerPort int
	DialHost   string
	DialPort   int
}

// ParseRemoteForwardSpec parses "[bind:]runnerport:dialhost:dialport".
// bind defaults to 127.0.0.1 (on the runner). IPv6 literals unsupported.
func ParseRemoteForwardSpec(s string) (RemoteForwardSpec, error) {
	parts := strings.Split(s, ":")
	var bind, dhost, rportS, dportS string
	switch len(parts) {
	case 3:
		bind, rportS, dhost, dportS = "127.0.0.1", parts[0], parts[1], parts[2]
	case 4:
		bind, rportS, dhost, dportS = parts[0], parts[1], parts[2], parts[3]
	default:
		return RemoteForwardSpec{}, fmt.Errorf("forward: bad -R spec %q (want [bind:]runnerport:dialhost:dialport)", s)
	}
	rport, err := strconv.Atoi(rportS)
	if err != nil || rport <= 0 || rport > 65535 {
		return RemoteForwardSpec{}, fmt.Errorf("forward: bad runner port in %q", s)
	}
	dport, err := strconv.Atoi(dportS)
	if err != nil || dport <= 0 || dport > 65535 {
		return RemoteForwardSpec{}, fmt.Errorf("forward: bad dial port in %q", s)
	}
	if dhost == "" {
		return RemoteForwardSpec{}, fmt.Errorf("forward: empty dial host in %q", s)
	}
	return RemoteForwardSpec{BindAddr: bind, RunnerPort: rport, DialHost: dhost, DialPort: dport}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./cli/ -run TestParseRemoteForwardSpec -v`
Expected: PASS.

- [ ] **Step 5: Implement `OpenRemoteForward` + `RunRemoteForward`**

In `cli/port_forward.go`:
```go
// OpenRemoteForward registers a remote forward: it opens a control stream,
// sends the registration, and returns the control stream + forwardId. The
// caller reads the control stream for RemoteForwardConnNotify and dials per conn.
func (c *Client) OpenRemoteForward(ctx context.Context, taskIDHex string, sp RemoteForwardSpec) (trsf.BidirectionalStream, uint64, error) {
	tid, err := parseTaskIDHex(taskIDHex)
	if err != nil {
		return nil, 0, fmt.Errorf("forward: parse task id: %w", err)
	}
	ctrl := c.Transport().CreateBidirectionalStream()
	if ctrl == nil {
		return nil, 0, errors.New("forward: cannot open control stream")
	}
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_OpenPortForward}
	body := protocol.OpenPortForwardRequest{
		TaskId:          tid,
		Direction:       protocol.PortForwardDirection_Remote,
		RemotePort:      uint16(sp.DialPort),
		BindPort:        uint16(sp.RunnerPort),
		ControlStreamId: uint64(ctrl.ID()),
	}
	body.SetRemoteHost([]byte(sp.DialHost))
	body.SetBindAddr([]byte(sp.BindAddr))
	req.SetOpenPortForward(body)

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		_ = ctrl.CloseBoth()
		return nil, 0, err
	}
	r := resp.OpenPortForward()
	if r == nil || resp.Kind != protocol.TaskControlKind_OpenPortForward {
		_ = ctrl.CloseBoth()
		return nil, 0, errors.New("forward: bad response")
	}
	switch r.Status {
	case protocol.OpenPortForwardStatus_Ok:
	case protocol.OpenPortForwardStatus_NoSuchTask:
		_ = ctrl.CloseBoth()
		return nil, 0, errors.New("forward: no such task")
	case protocol.OpenPortForwardStatus_RunnerOffline:
		_ = ctrl.CloseBoth()
		return nil, 0, errors.New("forward: runner offline")
	case protocol.OpenPortForwardStatus_BindFailed:
		_ = ctrl.CloseBoth()
		return nil, 0, errors.New("forward: runner failed to bind the port")
	default:
		_ = ctrl.CloseBoth()
		return nil, 0, fmt.Errorf("forward: server error (status=%d)", r.Status)
	}
	return ctrl, r.ForwardId, nil
}

// RunRemoteForward registers each spec and reads its control stream, dialing the
// client-side target per arriving connection. Blocks until ctx is cancelled.
func RunRemoteForward(ctx context.Context, c *Client, taskIDHex string, specs []RemoteForwardSpec, logf func(string)) error {
	if logf == nil {
		logf = func(s string) { slog.Info(s) }
	}
	var wg sync.WaitGroup
	for _, sp := range specs {
		ctrl, fid, err := c.OpenRemoteForward(ctx, taskIDHex, sp)
		if err != nil {
			return err
		}
		logf(fmt.Sprintf("remote-forwarding runner:%s:%d -> %s:%d (task %s, fwd %d)",
			sp.BindAddr, sp.RunnerPort, sp.DialHost, sp.DialPort, taskIDHex[:min(12, len(taskIDHex))], fid))
		wg.Add(1)
		go func(sp RemoteForwardSpec, ctrl trsf.BidirectionalStream) {
			defer wg.Done()
			c.readRemoteForwardControl(ctx, sp, ctrl, logf)
		}(sp, ctrl)
	}
	<-ctx.Done()
	wg.Wait()
	return nil
}

// readRemoteForwardControl parses RemoteForwardConnNotify frames off the control
// stream and, for each, dials the client-side target and splices.
func (c *Client) readRemoteForwardControl(ctx context.Context, sp RemoteForwardSpec, ctrl trsf.BidirectionalStream, logf func(string)) {
	defer ctrl.CloseBoth()
	for {
		data, eof, err := ctrl.ReadDirect(64 * 1024)
		if len(data) > 0 {
			var n protocol.RemoteForwardConnNotify
			if _, perr := n.Parse(data); perr != nil {
				logf("remote-forward: bad notify: " + perr.Error())
				continue
			}
			go c.dialAndSplice(ctx, sp, trsf.StreamID(n.StreamId), logf)
		}
		if eof || err != nil {
			return
		}
	}
}

func (c *Client) dialAndSplice(ctx context.Context, sp RemoteForwardSpec, streamID trsf.StreamID, logf func(string)) {
	st := peer.WaitForBidirectionalStream(ctx, c.Transport(), streamID)
	if st == nil {
		return
	}
	conn, err := net.Dial("tcp", net.JoinHostPort(sp.DialHost, strconv.Itoa(sp.DialPort)))
	if err != nil {
		logf(fmt.Sprintf("remote-forward: dial %s:%d failed: %v", sp.DialHost, sp.DialPort, err))
		_ = st.CloseBoth() // EOF propagates → runner-side conn closes (refused semantics)
		return
	}
	spliceConnStream(conn, st) // existing helper
}
```
NOTE on control-stream framing: `ctrl.ReadDirect` may coalesce/split frames. `RemoteForwardConnNotify` is fixed-size (8 bytes). If `ReadDirect` can return partial/multiple notifies, buffer to 8-byte boundaries before parsing. Implement a tiny accumulator in `readRemoteForwardControl` (append to a buffer; while `len(buf) >= 8`, parse one, advance). Add a test for the split case (Step 6).

- [ ] **Step 6: Write a test for `readRemoteForwardControl` framing**

Append to `cli/port_forward_test.go`: feed a fake control stream two notifies in one chunk and split across chunks; assert two dial attempts result (use a fake dialer hook or assert via a counter — adapt to whatever seam is cleanest; if `dialAndSplice` is hard to fake, extract the "parse notifies from a byte stream → []StreamID" pure function and test that).
```go
func TestParseConnNotifies_Framing(t *testing.T) {
	mk := func(id uint64) []byte {
		return protocol.RemoteForwardConnNotify{StreamId: id}.MustAppend(nil)
	}
	one := mk(11)
	two := append(mk(22), mk(33)...)
	ids, rest := parseConnNotifies(append([]byte{}, two...))
	if len(ids) != 2 || ids[0] != 22 || ids[1] != 33 || len(rest) != 0 {
		t.Fatalf("coalesced: ids=%v rest=%d", ids, len(rest))
	}
	// split: first 4 bytes only → no complete notify
	ids, rest = parseConnNotifies(one[:4])
	if len(ids) != 0 || len(rest) != 4 {
		t.Fatalf("partial: ids=%v rest=%d", ids, len(rest))
	}
}
```
Implement the pure helper and use it inside `readRemoteForwardControl`:
```go
// parseConnNotifies consumes as many whole RemoteForwardConnNotify records from
// buf as possible, returning the stream ids and the unconsumed remainder.
func parseConnNotifies(buf []byte) (ids []uint64, rest []byte) {
	const sz = 8 // one u64
	for len(buf) >= sz {
		var n protocol.RemoteForwardConnNotify
		if _, err := n.Parse(buf[:sz]); err != nil {
			break
		}
		ids = append(ids, n.StreamId)
		buf = buf[sz:]
	}
	return ids, buf
}
```
(Confirm the encoded size of `RemoteForwardConnNotify` is 8 bytes after regen; if brgen adds framing, set `sz` to the actual `len(MustAppend(nil))`.)

- [ ] **Step 7: Wire `-R` into the CLI `forward` subcommand**

In `cmd/harness-cli/main.go` `case "forward":` (line ~340): add a second repeatable flag and run both:
```go
		var rspecs multiFlag // same string-slice flag type as -L
		fs.Var(&rspecs, "R", "remote forward [bind:]runnerport:dialhost:dialport (repeatable)")
```
After parsing local `specs`, parse remote:
```go
		parsedR := make([]cli.RemoteForwardSpec, 0, len(rspecs))
		for _, s := range rspecs {
			sp, err := cli.ParseRemoteForwardSpec(s)
			if err != nil { fmt.Fprintln(os.Stderr, err); os.Exit(2) }
			parsedR = append(parsedR, sp)
		}
		if len(parsed) == 0 && len(parsedR) == 0 {
			fmt.Fprintln(os.Stderr, "usage: harness-cli forward <task-id> [-L ...] [-R ...]")
			os.Exit(2)
		}
```
Run both in parallel under the same ctx:
```go
		go func() {
			if len(parsedR) > 0 {
				if err := cli.RunRemoteForward(fctx, c, taskID, parsedR, func(s string){ fmt.Fprintln(os.Stderr, s) }); err != nil {
					fmt.Fprintln(os.Stderr, err)
				}
			}
		}()
		if len(parsed) > 0 {
			if err := cli.RunForward(fctx, c, taskID, parsed, func(s string){ fmt.Fprintln(os.Stderr, s) }); err != nil { ... }
		} else {
			<-fctx.Done()
		}
```
Update the usage strings (lines ~342, ~513-514) to mention `-R`.

- [ ] **Step 8: Run tests + build**

Run: `go test ./cli/ -run 'RemoteForward|ParseConnNotifies' -v` → Expected: PASS.
Run: `go build ./cmd/harness-cli/` → Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add cli/port_forward.go cli/port_forward_test.go cmd/harness-cli/main.go
git commit -m "feat(cli): remote port forward (-R) registration, control-stream dial loop"
```

---

## Task 6: TUI — multi-forward management + b/B

**Files:**
- Modify: `tui/portforward.go`, `tui/app.go`
- Test: `tui/portforward_test.go`

- [ ] **Step 1: Write failing test for id-keyed selection logic**

Append to `tui/portforward_test.go`:
```go
func TestSelectForwards_ByTaskAndDirection(t *testing.T) {
	fs := map[int]*PortForwardSession{
		1: {ID: 1, TaskID: "a", Direction: ForwardLocal, Spec: "8080:h:80"},
		2: {ID: 2, TaskID: "a", Direction: ForwardLocal, Spec: "9090:h:90"},
		3: {ID: 3, TaskID: "a", Direction: ForwardRemote, Spec: "1:h:2"},
		4: {ID: 4, TaskID: "b", Direction: ForwardLocal, Spec: "7:h:7"},
	}
	got := selectForwards(fs, "a", ForwardLocal)
	if len(got) != 2 {
		t.Fatalf("want 2 local forwards for task a, got %d", len(got))
	}
	if len(selectForwards(fs, "a", ForwardRemote)) != 1 {
		t.Fatal("want 1 remote forward for task a")
	}
	if len(selectForwards(fs, "z", ForwardLocal)) != 0 {
		t.Fatal("want 0 for unknown task")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./tui/ -run TestSelectForwards_ByTaskAndDirection -v`
Expected: FAIL (undefined types/func).

- [ ] **Step 3: Implement session model + selection helper**

In `tui/portforward.go`:
```go
type ForwardDirection int

const (
	ForwardLocal ForwardDirection = iota
	ForwardRemote
)

// PortForwardSession is one active forward (local or remote), tracked by a
// client-side unique ID so a task can hold several at once.
type PortForwardSession struct {
	ID        int
	TaskID    string
	Direction ForwardDirection
	Spec      string
	Cancel    context.CancelFunc
}

// selectForwards returns the active sessions for a task in one direction,
// sorted by ID for stable picker ordering.
func selectForwards(m map[int]*PortForwardSession, taskID string, dir ForwardDirection) []*PortForwardSession {
	var out []*PortForwardSession
	for _, s := range m {
		if s.TaskID == taskID && s.Direction == dir {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./tui/ -run TestSelectForwards_ByTaskAndDirection -v`
Expected: PASS.

- [ ] **Step 5: Rework `tui/app.go` state + keys (no new unit test; covered by Step 1 logic + manual)**

- Change the field (line ~60) from `activeForwards map[string]*PortForwardSession` to:
```go
	activeForwards map[int]*PortForwardSession
	nextForwardID  int
```
  and init (line ~94) `activeForwards: map[int]*PortForwardSession{}`.
- `PortForwardStartedMsg` gains `ID int` and `Direction ForwardDirection`. The start handler (line ~368) becomes:
```go
	case PortForwardStartedMsg:
		a.activeForwards[msg.ID] = &PortForwardSession{ID: msg.ID, TaskID: msg.TaskID, Direction: msg.Direction, Spec: msg.Spec, Cancel: msg.Cancel}
		short := msg.TaskID[:min(12, len(msg.TaskID))]
		flag := "-L"; if msg.Direction == ForwardRemote { flag = "-R" }
		a.cmdresult.Append(OKStyle.Render("forward started: ") + short + "  " + flag + " " + msg.Spec)
```
- Allocate the ID when dispatching start. In the modal-submit handler (line ~456) and the new `b` handler, do `a.nextForwardID++` and pass it into `DoStartPortForward`/`DoStartRemoteForward` so the goroutine reports it back in `PortForwardStartedMsg`.
- `p` (line ~570) opens the modal in local mode; add `b` opening it in remote mode:
```go
		if a.focus == focusTasks && msg.String() == "b" {
			t := a.tasks.SelectedTask()
			if t == nil { a.cmdresult.Append(WarnStyle.Render("forward: no task selected")); return a, nil }
			a.portForwardModal.OpenMode(t.ID, ForwardRemote)
			return a, nil
		}
```
  (`OpenMode` sets `m.taskID`, `m.mode`, placeholder; keep `Open` as `OpenMode(id, ForwardLocal)`.)
- `P`/`B` stop with picker. Replace the `P` handler (line ~580) and add `B`:
```go
		if a.focus == focusTasks && (msg.String() == "P" || msg.String() == "B") {
			dir := ForwardLocal; if msg.String() == "B" { dir = ForwardRemote }
			t := a.tasks.SelectedTask()
			if t == nil { a.cmdresult.Append(WarnStyle.Render("forward: no task selected")); return a, nil }
			sel := selectForwards(a.activeForwards, t.ID, dir)
			switch len(sel) {
			case 0:
				a.cmdresult.Append(WarnStyle.Render("forward: none active for selected task"))
			case 1:
				sel[0].Cancel(); delete(a.activeForwards, sel[0].ID)
				a.cmdresult.Append(OKStyle.Render("forward cancelled: ") + t.ID[:min(12,len(t.ID))])
			default:
				a.forwardPicker.Open(sel) // list modal; Enter cancels chosen
			}
			return a, nil
		}
```
- Add a `forwardPicker` list modal (small new type in `tui/portforward.go`) that renders `sel` and, on Enter, returns the chosen `*PortForwardSession`; the app then calls `.Cancel()` + `delete`. Intercept its keys when open (mirror how `portForwardModal` intercepts at line ~442).
- Update the hint line (line ~727) to append `· b rforward · B stop-rforward`.

- [ ] **Step 6: Add `DoStartRemoteForward` + modal mode in `tui/portforward.go`**

```go
func (m *PortForwardModal) OpenMode(taskID string, dir ForwardDirection) {
	m.open = true
	m.taskID = taskID
	m.mode = dir
	m.input.SetValue("")
	if dir == ForwardRemote {
		m.input.Placeholder = "[bind:]runnerport:dialhost:dialport"
	} else {
		m.input.Placeholder = "[bind:]localport:remotehost:remoteport"
	}
	m.input.Focus()
}

// DoStartRemoteForward parses the spec and runs a remote forward in the
// background, reporting back via PortForwardStartedMsg{Direction: ForwardRemote}.
func DoStartRemoteForward(c *cli.Client, taskID, spec string, id int, program *tea.Program) tea.Cmd {
	return func() tea.Msg {
		sp, err := cli.ParseRemoteForwardSpec(spec)
		if err != nil {
			return PortForwardStatusMsg{TaskID: taskID, Line: "forward: " + err.Error()}
		}
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			err := cli.RunRemoteForward(ctx, c, taskID, []cli.RemoteForwardSpec{sp}, func(s string) {
				program.Send(PortForwardStatusMsg{TaskID: taskID, Line: s})
			})
			if err != nil {
				program.Send(PortForwardStatusMsg{TaskID: taskID, Line: "forward: " + err.Error()})
			}
		}()
		return PortForwardStartedMsg{ID: id, TaskID: taskID, Direction: ForwardRemote, Spec: spec, Cancel: cancel}
	}
}
```
(Mirror the exact shape of the existing `DoStartPortForward` — message field names, `program.Send` usage — and add `mode` to the `PortForwardModal` struct + `Mode()`/dispatch in the submit handler so local vs remote dispatches to the right `Do*`.)

- [ ] **Step 7: Run tests + build**

Run: `go test ./tui/ -run 'SelectForwards' -v` → Expected: PASS.
Run: `go build ./tui/ ./cmd/harness-tui/` → Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add tui/portforward.go tui/app.go tui/portforward_test.go
git commit -m "feat(tui): multiple forwards per task (id-keyed) + b/B remote forward with stop picker"
```

---

## Task 7: End-to-end integration test

**Files:**
- Modify: `integration/port_forward_test.go`

- [ ] **Step 1: Write the e2e remote-forward test**

Mirror the existing local-forward e2e in this file (same server+runner+client wiring). The remote test: start a **client-side echo server** on an ephemeral port as the dial target; register a remote forward (`runner bind :0`-equivalent — use a fixed free port or have the runner pick one and report it; if the schema fixes bindPort, pick a free port via a throwaway `net.Listen` then close it); `net.Dial` the runner's bound port; write bytes; assert the echo returns through the tunnel.
```go
func TestRemotePortForward_EndToEnd(t *testing.T) {
	env := newPortForwardE2E(t) // existing harness used by the local test
	defer env.Close()

	// client-side echo server = the dial target
	echoLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil { t.Fatal(err) }
	defer echoLn.Close()
	go echoAccept(echoLn) // read→write loop; reuse local test's echo if present
	_, dialPortStr, _ := net.SplitHostPort(echoLn.Addr().String())
	dialPort, _ := strconv.Atoi(dialPortStr)

	runnerPort := freePort(t) // helper: listen :0, capture port, close
	sp := cli.RemoteForwardSpec{BindAddr: "127.0.0.1", RunnerPort: runnerPort, DialHost: "127.0.0.1", DialPort: dialPort}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go cli.RunRemoteForward(ctx, env.client, env.taskID, []cli.RemoteForwardSpec{sp}, func(string){})

	// give the runner a moment to bind, then connect to the runner-side port
	conn := dialWithRetry(t, net.JoinHostPort("127.0.0.1", strconv.Itoa(runnerPort)), 2*time.Second)
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil { t.Fatal(err) }
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil { t.Fatal(err) }
	if string(buf) != "ping" { t.Fatalf("echo = %q", buf) }
}
```
(Reuse the local test's helpers: server/runner/client setup, `echoAccept`, `dialWithRetry`, `freePort`. Add the small ones if missing. The runner and client are in-process in this test, both reachable on 127.0.0.1 — the directional distinction is exercised by which side listens vs dials.)

- [ ] **Step 2: Run it**

Run: `go test ./integration/ -run TestRemotePortForward_EndToEnd -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add integration/port_forward_test.go
git commit -m "test(integration): end-to-end remote port forward"
```

---

## Final verification

- [ ] `make check` → PASS (webui-build + `go build ./...`)
- [ ] `make wasm-check` → PASS
- [ ] `go test ./...` → PASS
- [ ] Manual smoke (optional, on the live harness): `harness-cli forward <bash-task> -R 18080:127.0.0.1:<local-svc-port>`, then from the bash session `curl 127.0.0.1:18080` reaches the client-side service.

## Self-review notes (from plan author)

- Spec coverage: registration (T2), conn arrival + notify (T3), runner listener + close (T4), CLI parse/register/run (T5), TUI multi-forward + b/B picker (T6), e2e (T7), schema (T1). All spec sections covered.
- BindFailed: spec lists it as a status, but Task 4 Step 5 deliberately keeps registration fire-and-forget (Ok before the runner attempts listen) and logs listen failures runner-side. The `bind_failed` enum value is still added (T1) for future use; the CLI handles it (T5) if ever returned. If precise bind feedback is required, add a runner→server listen-ack as a follow-up and update the spec.
- Generated-code dependency: every task after T1 needs the regenerated accessors; if `make protoregen` cannot run (offline), the whole plan blocks at T1 Step 4.
