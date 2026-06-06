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
	"github.com/on-keyday/agent-harness/trsf"
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
		Detachable: true,
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

// TestHandleOpenPortForward_RemoteRegisters verifies the ssh -R registration:
// the server creates a control stream (returned as StreamId), assigns a
// ForwardId, stores the registration, and sends the runner a listen request.
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
	req := &protocol.OpenPortForwardRequest{
		TaskId:     protocol.TaskID{Id: rawID},
		Direction:  protocol.PortForwardDirection_Remote,
		RemotePort: 5432,
		BindPort:   15432,
	}
	req.SetRemoteHost([]byte("127.0.0.1"))
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
	if _, ok := h.rforwards().get(resp.ForwardId); !ok {
		t.Fatal("registration not stored")
	}
	if len(runnerConn.sent) == 0 {
		t.Fatal("no listen request sent to runner")
	}
	var rr protocol.RunnerRequest
	if _, err := rr.Decode(runnerConn.sent[0][1:]); err != nil { // strip ApplicationPayloadKind byte
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

// runRemoteRegister runs handleOpenPortForward(Remote) (which now blocks for the
// runner's bind result) in a goroutine and feeds it that result. A fresh handler
// assigns forwardId 1, so we signal id 1; the signal is retried until the
// registration consumes it, then the response is returned.
func runRemoteRegister(t *testing.T, h *TaskHandler, clientConn *fakeConn, req *protocol.OpenPortForwardRequest, runnerConn *fakeConn, bindOK bool) protocol.OpenPortForwardResponse {
	t.Helper()
	respCh := make(chan protocol.OpenPortForwardResponse, 1)
	go func() { respCh <- h.handleOpenPortForward(clientConn, req) }()
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
func registerRemoteForwardForTest(t *testing.T, ctrl trsf.BidirectionalStream, bindOK bool) (*TaskHandler, *fakeConn, *fakeConn, protocol.OpenPortForwardResponse) {
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
	req := &protocol.OpenPortForwardRequest{
		TaskId:     protocol.TaskID{Id: rawID},
		Direction:  protocol.PortForwardDirection_Remote,
		RemotePort: 5432,
		BindPort:   15432,
	}
	req.SetRemoteHost([]byte("127.0.0.1"))
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
	if _, ok := h.rforwards().get(1); ok {
		t.Fatal("registration should be removed after bind failure")
	}
	if !ctrl.closed.Load() {
		t.Fatal("control stream should be closed after bind failure")
	}
}

// TestHandleRemoteForwardConn_NotifiesClient verifies a runner-reported
// connection produces a RemoteForwardConnNotify on the control stream carrying
// the new client data-stream id.
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

	var n protocol.RemoteForwardConnNotify
	if _, err := n.Decode(ctrl.Written()); err != nil {
		t.Fatalf("decode notify: %v (written %d bytes)", err, len(ctrl.Written()))
	}
	if n.StreamId != 556 {
		t.Fatalf("notify StreamId = %d, want 556", n.StreamId)
	}
}

// TestRemoteForwardControlClose_TearsDownRegistration verifies that closing the
// control stream makes the watcher drop the registration (and signal the runner).
func TestRemoteForwardControlClose_TearsDownRegistration(t *testing.T) {
	ctrl := newRecordingBidiStream(555)
	h, _, _, resp := registerRemoteForwardForTest(t, ctrl, true)
	if _, ok := h.rforwards().get(resp.ForwardId); !ok {
		t.Fatal("registration missing after register")
	}
	ctrl.CloseBoth() // client closes the control stream
	deadline := time.Now().Add(time.Second)
	for {
		if _, ok := h.rforwards().get(resp.ForwardId); !ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("registration not torn down after control-stream close")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
