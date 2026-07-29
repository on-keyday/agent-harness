package server

import (
	"context"
	"encoding/hex"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// TestHandleOpenPortForward_NoSuchTask exercises the early-exit branch
// when the requested task id is not present in the TaskStore (or is not
// in the Running/Detached state). The handler must return NoSuchTask without
// touching streams — passing nil for ConnHandle is safe here precisely
// because the lookup fails before any stream-allocation step runs.
func TestHandleOpenPortForward_NoSuchTask(t *testing.T) {
	h := &TaskHandler{
		Tasks:    NewTaskStore(),
		Registry: NewRegistry(),
	}
	req := &protocol.OpenPortForwardRequest{TaskId: protocol.TaskID{Id: [16]byte{9, 9, 9}}}
	req.SetRemoteHost([]byte("127.0.0.1"))
	resp := h.handleOpenPortForward(nil, req)
	if resp.Status != protocol.OpenPortForwardStatus_NoSuchTask {
		t.Fatalf("got status %v, want NoSuchTask", resp.Status)
	}
}

// TestHandleOpenPortForward_DetachedTaskAccepted verifies that the status
// gate accepts Detached as well as Running. Mirrors the equivalent test in
// file_transfer_test.go. The runner is intentionally NOT registered: the
// expected outcome is RunnerOffline (proving the status gate let us through),
// not NoSuchTask (which would prove the gate rejected Detached).
func TestHandleOpenPortForward_DetachedTaskAccepted(t *testing.T) {
	h := &TaskHandler{
		Tasks:    NewTaskStore(),
		Registry: NewRegistry(),
	}
	var rawID [16]byte
	rawID[0] = 0xD3
	idHex := hex.EncodeToString(rawID[:])
	h.Tasks.mu.Lock()
	h.Tasks.tasks[idHex] = &TaskEntry{
		ID:         idHex,
		RepoPath:   "/repo",
		Status:     protocol.TaskStatus_Detached,
		Kind:       protocol.TaskKind_Interactive,
		AssignedTo: "fake-runner-id",
	}
	h.Tasks.order = append(h.Tasks.order, idHex)
	h.Tasks.mu.Unlock()

	req := &protocol.OpenPortForwardRequest{TaskId: protocol.TaskID{Id: rawID}}
	req.SetRemoteHost([]byte("127.0.0.1"))
	req.RemotePort = 8080
	resp := h.handleOpenPortForward(nil, req)
	if resp.Status == protocol.OpenPortForwardStatus_NoSuchTask {
		t.Fatalf("detached task must not be rejected as NoSuchTask")
	}
	// Runner not registered → RunnerOffline.
	if resp.Status != protocol.OpenPortForwardStatus_RunnerOffline {
		t.Fatalf("expected RunnerOffline (no runner registered), got %v", resp.Status)
	}
}

// TestHandleOpenPortForward_LocalDialsRunner verifies the per-connection local
// forward path end to end: on a Running task with an online runner,
// handleOpenPortForward returns Ok, and the RunnerOpenPortForwardRequest the
// runner receives decodes with Direction == Local, the right
// RemoteHost/RemotePort dial target, and a StreamId matching the runner-side
// stream. Direction is hard-coded to Local inside handleOpenPortForward (it
// can no longer be copied from the request — OpenPortForwardRequest dropped
// the field in this task's schema slimming), so this is the test that pins
// the "the runner protocol did not move" claim for the -L path, mirroring how
// TestHandleOpenPortForward_RemoteRegisters pins it for -R.
func TestHandleOpenPortForward_LocalDialsRunner(t *testing.T) {
	h := &TaskHandler{Tasks: NewTaskStore(), Registry: NewRegistry()}
	var rawID [16]byte
	rawID[0] = 0x22
	idHex := hex.EncodeToString(rawID[:])
	h.Tasks.mu.Lock()
	h.Tasks.tasks[idHex] = &TaskEntry{ID: idHex, Status: protocol.TaskStatus_Running, AssignedTo: "runner-1"}
	h.Tasks.order = append(h.Tasks.order, idHex)
	h.Tasks.mu.Unlock()

	runnerConn := &fakeConn{nextStreamID: 900}
	h.Registry.Add(&RunnerEntry{ID: "runner-1", Conn: runnerConn})

	clientConn := &fakeConn{nextStreamID: 501}
	req := &protocol.OpenPortForwardRequest{
		TaskId:     protocol.TaskID{Id: rawID},
		RemotePort: 3306,
	}
	req.SetRemoteHost([]byte("db.internal"))

	resp := h.handleOpenPortForward(clientConn, req)
	if resp.Status != protocol.OpenPortForwardStatus_Ok {
		t.Fatalf("status = %v, want Ok", resp.Status)
	}
	if resp.StreamId != 501 {
		t.Fatalf("StreamId = %d, want client stream id 501", resp.StreamId)
	}

	runnerSent := runnerConn.Sent()
	if len(runnerSent) == 0 {
		t.Fatal("no request sent to runner")
	}
	var rr protocol.RunnerRequest
	if _, err := rr.Decode(runnerSent[0][1:]); err != nil { // strip ApplicationPayloadKind byte
		t.Fatalf("decode runner req: %v", err)
	}
	if rr.Kind != protocol.RunnerRequestType_OpenPortForward {
		t.Fatalf("runner req kind = %v", rr.Kind)
	}
	body := rr.OpenPortForward()
	if body == nil {
		t.Fatal("runner req body missing")
	}
	if body.Direction != protocol.PortForwardDirection_Local {
		t.Fatalf("runner req Direction = %v, want Local (byte-identical-to-pre-migration claim)", body.Direction)
	}
	if string(body.RemoteHost) != "db.internal" || body.RemotePort != 3306 {
		t.Fatalf("runner req dial target = %s:%d, want db.internal:3306", body.RemoteHost, body.RemotePort)
	}
	if body.StreamId != 900 {
		t.Fatalf("runner req StreamId = %d, want runner-side stream id 900", body.StreamId)
	}
}

// TestHandleOpenPortForward_RemoteRegisters verifies the ssh -R registration,
// now via RegisterPortForwardRequest/handleRegisterPortForward: the server
// creates a control stream (returned as StreamId), assigns a ForwardId,
// stores the registration, and sends the runner a listen request. The
// runner-facing assertions (rr.Kind, body.Direction, body.BindPort,
// body.ForwardId) are unchanged from before this task's migration — that is
// the evidence the runner protocol did not move.
func TestHandleOpenPortForward_RemoteRegisters(t *testing.T) {
	h := &TaskHandler{Tasks: NewTaskStore(), Registry: NewRegistry()}
	var rawID [16]byte
	rawID[0] = 0x5A
	idHex := hex.EncodeToString(rawID[:])
	h.Tasks.mu.Lock()
	h.Tasks.tasks[idHex] = &TaskEntry{ID: idHex, Status: protocol.TaskStatus_Running, AssignedTo: "runner-1"}
	h.Tasks.order = append(h.Tasks.order, idHex)
	h.Tasks.mu.Unlock()

	runnerConn := &fakeConn{}
	h.Registry.Add(&RunnerEntry{ID: "runner-1", Conn: runnerConn})

	clientConn := &fakeConn{nextStreamID: 555}
	req := &protocol.RegisterPortForwardRequest{
		TaskId:     protocol.TaskID{Id: rawID},
		Direction:  protocol.PortForwardDirection_Remote,
		TargetPort: 5432,
		BindPort:   15432,
	}
	req.SetTargetHost([]byte("127.0.0.1"))
	req.SetBindAddr([]byte("127.0.0.1"))

	resp := runRemoteRegister(t, h, clientConn, req, runnerConn, true)
	if resp.Status != protocol.OpenPortForwardStatus_Ok {
		t.Fatalf("status = %v, want Ok", resp.Status)
	}
	if resp.ForwardId == 0 {
		t.Fatal("ForwardId should be non-zero")
	}
	if resp.StreamId != 555 {
		t.Fatalf("StreamId = %d, want control stream id 555", resp.StreamId)
	}
	if _, ok := h.pforwards().get(resp.ForwardId); !ok {
		t.Fatal("registration not stored")
	}
	runnerSent := runnerConn.Sent()
	if len(runnerSent) == 0 {
		t.Fatal("no listen request sent to runner")
	}
	var rr protocol.RunnerRequest
	if _, err := rr.Decode(runnerSent[0][1:]); err != nil { // strip ApplicationPayloadKind byte
		t.Fatalf("decode runner req: %v", err)
	}
	if rr.Kind != protocol.RunnerRequestType_OpenPortForward {
		t.Fatalf("runner req kind = %v", rr.Kind)
	}
	body := rr.OpenPortForward()
	if body == nil || body.Direction != protocol.PortForwardDirection_Remote ||
		body.BindPort != 15432 || body.ForwardId != resp.ForwardId {
		t.Fatalf("runner req body = %+v", body)
	}
}

// TestHandleRegisterPortForward_LocalNoSuchTask verifies the status gate on
// the register path for a local registration: an unknown task id must return
// NoSuchTask without touching conn or the registry — passing nil for
// ConnHandle and an empty cid is safe here precisely because the lookup fails
// before any stream-allocation step runs. Mirrors
// TestHandleOpenPortForward_NoSuchTask for the new RPC.
func TestHandleRegisterPortForward_LocalNoSuchTask(t *testing.T) {
	h := &TaskHandler{
		Tasks:    NewTaskStore(),
		Registry: NewRegistry(),
	}
	req := &protocol.RegisterPortForwardRequest{
		TaskId:    protocol.TaskID{Id: [16]byte{9, 9, 9}},
		Direction: protocol.PortForwardDirection_Local,
	}
	req.SetTargetHost([]byte("db.internal"))
	resp := h.handleRegisterPortForward(nil, req, "")
	if resp.Status != protocol.OpenPortForwardStatus_NoSuchTask {
		t.Fatalf("got status %v, want NoSuchTask", resp.Status)
	}
}

// TestHandleRegisterPortForward_LocalRegisters verifies the -L registration
// path added by this task: on a Running task, handleRegisterPortForward with
// Direction=Local returns Ok with a non-zero ForwardId and the client
// control-stream id, stores the registration under h.pforwards() with
// direction == Local — and, the point of the test, never contacts the
// runner. A runner IS registered (so a bug that made this path consult the
// runner would still get a live Conn, not an early RunnerOffline masking the
// real assertion); runnerConn.Sent() must stay empty throughout.
//
// The control stream is a recordingBidiStream (not the default
// nextStreamID-driven noopBidiStream) because noopBidiStream.ReadDirect
// returns EOF immediately, which would race the background
// watchRemoteForwardControl goroutine's registry-removal against this test's
// own h.pforwards().get check.
func TestHandleRegisterPortForward_LocalRegisters(t *testing.T) {
	h := &TaskHandler{Tasks: NewTaskStore(), Registry: NewRegistry()}
	var rawID [16]byte
	rawID[0] = 0x33
	idHex := hex.EncodeToString(rawID[:])
	h.Tasks.mu.Lock()
	h.Tasks.tasks[idHex] = &TaskEntry{ID: idHex, Status: protocol.TaskStatus_Running, AssignedTo: "runner-1"}
	h.Tasks.order = append(h.Tasks.order, idHex)
	h.Tasks.mu.Unlock()

	runnerConn := &fakeConn{}
	h.Registry.Add(&RunnerEntry{ID: "runner-1", Conn: runnerConn})

	ctrl := newRecordingBidiStream(777)
	defer ctrl.CloseBoth() // let the watchRemoteForwardControl goroutine exit
	clientConn := &fakeConn{nextBidi: ctrl}
	req := &protocol.RegisterPortForwardRequest{
		TaskId:     protocol.TaskID{Id: rawID},
		Direction:  protocol.PortForwardDirection_Local,
		TargetPort: 5432,
		BindPort:   18080,
	}
	req.SetTargetHost([]byte("db.internal"))
	req.SetBindAddr([]byte("127.0.0.1"))

	resp := h.handleRegisterPortForward(clientConn, req, clientConn.ConnectionID().String())
	if resp.Status != protocol.OpenPortForwardStatus_Ok {
		t.Fatalf("status = %v, want Ok", resp.Status)
	}
	if resp.ForwardId == 0 {
		t.Fatal("ForwardId should be non-zero")
	}
	if resp.StreamId != 777 {
		t.Fatalf("StreamId = %d, want control stream id 777", resp.StreamId)
	}
	pf, ok := h.pforwards().get(resp.ForwardId)
	if !ok {
		t.Fatal("registration not stored")
	}
	if pf.direction != protocol.PortForwardDirection_Local {
		t.Fatalf("stored direction = %v, want Local", pf.direction)
	}
	if sent := runnerConn.Sent(); len(sent) != 0 {
		t.Fatalf("local registration must not contact the runner; runner.Sent() = %d messages", len(sent))
	}
}

// addRunningTask inserts a Running task assigned to runnerID and returns its hex id.
func addRunningTask(t *testing.T, h *TaskHandler, first byte, runnerID string) string {
	t.Helper()
	var raw [16]byte
	raw[0] = first
	idHex := hex.EncodeToString(raw[:])
	h.Tasks.mu.Lock()
	h.Tasks.tasks[idHex] = &TaskEntry{ID: idHex, Status: protocol.TaskStatus_Running, AssignedTo: runnerID}
	h.Tasks.order = append(h.Tasks.order, idHex)
	h.Tasks.mu.Unlock()
	return idHex
}

func TestVisiblePortForwards_ReapsDeadTaskAndListsRest(t *testing.T) {
	h := &TaskHandler{Tasks: NewTaskStore(), Registry: NewRegistry()}
	liveHex := addRunningTask(t, h, 0x11, "runner-1")
	deadHex := addRunningTask(t, h, 0x22, "runner-1")

	conn := &fakeConn{nextStreamID: 100}
	live := &portForward{direction: protocol.PortForwardDirection_Local, taskIDHex: liveHex,
		control: newRecordingBidiStream(1), clientCxn: conn, bindAddr: "127.0.0.1", bindPort: 8080,
		targetHost: "db", targetPort: 5432}
	dead := &portForward{direction: protocol.PortForwardDirection_Local, taskIDHex: deadHex,
		control: newRecordingBidiStream(2), clientCxn: conn, bindAddr: "127.0.0.1", bindPort: 8081,
		targetHost: "db", targetPort: 5432}
	liveID := h.pforwards().add(live)
	deadID := h.pforwards().add(dead)

	// The dead task leaves Running only AFTER both forwards were registered.
	h.Tasks.Cancel(deadHex)

	got := h.visiblePortForwards("", protocol.TaskID{})
	if len(got) != 1 || got[0].ForwardId != liveID {
		t.Fatalf("expected only forward %d, got %+v", liveID, got)
	}
	if got[0].BindPort != 8080 || string(got[0].TargetHost) != "db" {
		t.Fatalf("info fields not populated: %+v", got[0])
	}
	if _, ok := h.pforwards().get(deadID); ok {
		t.Fatal("the dead task's forward should have been reaped by the list call")
	}
}

func TestKillPortForward_UnknownIDAndDoubleKill(t *testing.T) {
	h := &TaskHandler{Tasks: NewTaskStore(), Registry: NewRegistry()}
	if st := h.killPortForward("", 9999); st != protocol.KillPortForwardStatus_NoSuchForward {
		t.Fatalf("unknown id: status = %v, want no_such_forward", st)
	}

	taskHex := addRunningTask(t, h, 0x33, "runner-1")
	pf := &portForward{direction: protocol.PortForwardDirection_Local, taskIDHex: taskHex,
		control: newRecordingBidiStream(3), clientCxn: &fakeConn{nextStreamID: 200}}
	id := h.pforwards().add(pf)

	if st := h.killPortForward("", id); st != protocol.KillPortForwardStatus_Ok {
		t.Fatalf("first kill: status = %v, want ok", st)
	}
	// Exactly one caller may win: the second kill must not also report ok.
	if st := h.killPortForward("", id); st != protocol.KillPortForwardStatus_NoSuchForward {
		t.Fatalf("second kill: status = %v, want no_such_forward", st)
	}
}

// TestPortForwardControlEOFDeregisters covers the stray-terminal case: the
// client process dies, its control stream EOFs, and the registration goes away
// with no RPC involved.
func TestPortForwardControlEOFDeregisters(t *testing.T) {
	h := &TaskHandler{Tasks: NewTaskStore(), Registry: NewRegistry()}
	taskHex := addRunningTask(t, h, 0x44, "runner-1")
	ctrl := newRecordingBidiStream(4)
	pf := &portForward{direction: protocol.PortForwardDirection_Local, taskIDHex: taskHex,
		control: ctrl, clientCxn: &fakeConn{nextStreamID: 300}}
	id := h.pforwards().add(pf)
	go h.watchRemoteForwardControl(pf)

	_ = ctrl.CloseBoth() // client went away
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := h.pforwards().get(id); !ok {
			return // deregistered
		}
		if time.Now().After(deadline) {
			t.Fatal("registration survived control-stream EOF")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// recordingBidiStream captures AppendData/Write payloads and blocks ReadDirect
// until CloseBoth. Used as a remote-forward control stream so a test can inspect
// the notify written to it while keeping the server's control watcher parked.
type recordingBidiStream struct {
	streamID trsf.StreamID
	mu       sync.Mutex
	written  []byte
	closeCh  chan struct{}
	closed   atomic.Bool
}

func newRecordingBidiStream(id trsf.StreamID) *recordingBidiStream {
	return &recordingBidiStream{streamID: id, closeCh: make(chan struct{})}
}

func (s *recordingBidiStream) ID() trsf.StreamID { return s.streamID }
func (s *recordingBidiStream) append(p []byte) int {
	s.mu.Lock()
	s.written = append(s.written, p...)
	s.mu.Unlock()
	return len(p)
}
func (s *recordingBidiStream) Written() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte{}, s.written...)
}
func (s *recordingBidiStream) Write(p []byte) (int, error) { return s.append(p), nil }
func (s *recordingBidiStream) WriteContext(_ context.Context, p []byte) (int, error) {
	return s.append(p), nil
}
func (s *recordingBidiStream) Close() error      { return nil }
func (s *recordingBidiStream) HasSendData() bool { return false }
func (s *recordingBidiStream) Completed() bool   { return false }
func (s *recordingBidiStream) AppendData(_ bool, payloads ...[]byte) error {
	for _, p := range payloads {
		s.append(p)
	}
	return nil
}
func (s *recordingBidiStream) AppendDataContext(_ context.Context, eof bool, payloads ...[]byte) error {
	return s.AppendData(eof, payloads...)
}
func (s *recordingBidiStream) Read(_ []byte) (int, error) { <-s.closeCh; return 0, io.EOF }
func (s *recordingBidiStream) ReadContext(_ context.Context, _ []byte) (int, error) {
	<-s.closeCh
	return 0, io.EOF
}
func (s *recordingBidiStream) ReadDirect(_ uint64) ([]byte, bool, error) {
	<-s.closeCh
	return nil, true, nil
}
func (s *recordingBidiStream) ReadDirectContext(_ context.Context, _ uint64) ([]byte, bool, error) {
	<-s.closeCh
	return nil, true, nil
}
func (s *recordingBidiStream) HasRecvData() bool { return false }
func (s *recordingBidiStream) EOF() bool         { return s.closed.Load() }
func (s *recordingBidiStream) Cancel()           { _ = s.CloseBoth() }
func (s *recordingBidiStream) CloseBoth() error {
	if s.closed.CompareAndSwap(false, true) {
		close(s.closeCh)
	}
	return nil
}

// runRemoteRegister runs handleRegisterPortForward (which now blocks for the
// runner's bind result) in a goroutine and feeds it that result. A fresh handler
// assigns forwardId 1, so we signal id 1; the signal is retried until the
// registration consumes it, then the response is returned.
func runRemoteRegister(t *testing.T, h *TaskHandler, clientConn *fakeConn, req *protocol.RegisterPortForwardRequest, runnerConn *fakeConn, bindOK bool) protocol.RegisterPortForwardResponse {
	t.Helper()
	respCh := make(chan protocol.RegisterPortForwardResponse, 1)
	cid := clientConn.ConnectionID().String()
	go func() { respCh <- h.handleRegisterPortForward(clientConn, req, cid) }()
	br := &protocol.RemoteForwardBindResult{ForwardId: 1}
	br.SetOk(bindOK)
	deadline := time.Now().Add(2 * time.Second)
	for {
		h.handleRemoteForwardBindResult(runnerConn, br)
		select {
		case r := <-respCh:
			return r
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("registration did not complete (bind result not consumed)")
		}
	}
}

// registerRemoteForwardForTest sets up a running task + runner and registers a
// remote forward whose control stream is ctrl, feeding the given bind result.
// Returns the handler, the two fake conns, and the registration response.
func registerRemoteForwardForTest(t *testing.T, ctrl trsf.BidirectionalStream, bindOK bool) (*TaskHandler, *fakeConn, *fakeConn, protocol.RegisterPortForwardResponse) {
	t.Helper()
	h := &TaskHandler{Tasks: NewTaskStore(), Registry: NewRegistry()}
	var rawID [16]byte
	rawID[0] = 0x7C
	idHex := hex.EncodeToString(rawID[:])
	h.Tasks.mu.Lock()
	h.Tasks.tasks[idHex] = &TaskEntry{ID: idHex, Status: protocol.TaskStatus_Running, AssignedTo: "runner-1"}
	h.Tasks.order = append(h.Tasks.order, idHex)
	h.Tasks.mu.Unlock()
	runnerConn := &fakeConn{}
	h.Registry.Add(&RunnerEntry{ID: "runner-1", Conn: runnerConn})
	clientConn := &fakeConn{nextBidi: ctrl}
	req := &protocol.RegisterPortForwardRequest{
		TaskId:     protocol.TaskID{Id: rawID},
		Direction:  protocol.PortForwardDirection_Remote,
		TargetPort: 5432,
		BindPort:   15432,
	}
	req.SetTargetHost([]byte("127.0.0.1"))
	req.SetBindAddr([]byte("127.0.0.1"))
	resp := runRemoteRegister(t, h, clientConn, req, runnerConn, bindOK)
	return h, clientConn, runnerConn, resp
}

// TestRegisterRemoteForward_BindFailed verifies that a runner bind failure makes
// registration return BindFailed and clean up (no leaked registration; control
// stream closed).
func TestRegisterRemoteForward_BindFailed(t *testing.T) {
	ctrl := newRecordingBidiStream(555)
	h, _, _, resp := registerRemoteForwardForTest(t, ctrl, false)
	if resp.Status != protocol.OpenPortForwardStatus_BindFailed {
		t.Fatalf("status = %v, want BindFailed", resp.Status)
	}
	if _, ok := h.pforwards().get(1); ok {
		t.Fatal("registration should be removed after bind failure")
	}
	if !ctrl.closed.Load() {
		t.Fatal("control stream should be closed after bind failure")
	}
}

// TestHandleRemoteForwardConn_NotifiesClient verifies a runner-reported
// connection produces a tagged PortForwardEvent (kind conn_notify) on the
// control stream carrying the new client data-stream id.
func TestHandleRemoteForwardConn_NotifiesClient(t *testing.T) {
	ctrl := newRecordingBidiStream(555)
	h, clientConn, runnerConn, resp := registerRemoteForwardForTest(t, ctrl, true)
	if resp.Status != protocol.OpenPortForwardStatus_Ok {
		t.Fatalf("register status = %v, want Ok", resp.Status)
	}
	// The runner-created data stream (id 900) must resolve on the runner conn.
	runnerConn.bidiByID = map[trsf.StreamID]trsf.BidirectionalStream{900: &noopBidiStream{streamID: 900}}
	// The next client stream (the data stream) is assigned id 556.
	clientConn.nextStreamID = 556

	h.handleRemoteForwardConn(runnerConn, &protocol.RemoteForwardConn{ForwardId: resp.ForwardId, StreamId: 900})

	var ev protocol.PortForwardEvent
	if _, err := ev.Decode(ctrl.Written()); err != nil {
		t.Fatalf("decode event: %v (written %d bytes)", err, len(ctrl.Written()))
	}
	if ev.Kind != protocol.PortForwardEventKind_ConnNotify {
		t.Fatalf("event kind = %v, want ConnNotify", ev.Kind)
	}
	n := ev.ConnNotify()
	if n == nil || n.StreamId != 556 {
		t.Fatalf("notify StreamId = %+v, want 556", n)
	}
}

// TestRemoteForwardControlClose_TearsDownRegistration verifies that closing the
// control stream makes the watcher drop the registration (and signal the runner).
func TestRemoteForwardControlClose_TearsDownRegistration(t *testing.T) {
	ctrl := newRecordingBidiStream(555)
	h, _, _, resp := registerRemoteForwardForTest(t, ctrl, true)
	if _, ok := h.pforwards().get(resp.ForwardId); !ok {
		t.Fatal("registration missing after register")
	}
	ctrl.CloseBoth() // client closes the control stream
	deadline := time.Now().Add(time.Second)
	for {
		if _, ok := h.pforwards().get(resp.ForwardId); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("registration not torn down after control-stream close")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
