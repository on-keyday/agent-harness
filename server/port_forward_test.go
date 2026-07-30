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
//
// The control stream is a recordingBidiStream (not the default
// nextStreamID-driven noopBidiStream) because noopBidiStream.ReadDirect
// returns EOF immediately, which races the background
// watchRemoteForwardControl goroutine's registry-removal against this test's
// own h.pforwards().get check — flaky under -race (~7/20), invisible under
// plain `make test` which never passes -race. See
// TestHandleRegisterPortForward_LocalRegisters for the same fix applied
// earlier to the -L sibling.
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

	ctrl := newRecordingBidiStream(555)
	defer ctrl.CloseBoth() // let the watchRemoteForwardControl goroutine exit
	clientConn := &fakeConn{nextBidi: ctrl}
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
	deadCtrl := newRecordingBidiStream(2)
	live := &portForward{direction: protocol.PortForwardDirection_Local, taskIDHex: liveHex,
		control: newRecordingBidiStream(1), clientCxn: conn, bindAddr: "127.0.0.1", bindPort: 8080,
		targetHost: "db", targetPort: 5432}
	dead := &portForward{direction: protocol.PortForwardDirection_Local, taskIDHex: deadHex,
		control: deadCtrl, clientCxn: conn, bindAddr: "127.0.0.1", bindPort: 8081,
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

	// The reaped forward's client must have actually been told: the whole
	// mechanism this feature rests on is an explicit `closed` record, not
	// just a registry entry disappearing (a client watching only its own
	// side would otherwise keep forwarding into nothing).
	var ev protocol.PortForwardEvent
	if _, err := ev.Decode(deadCtrl.Written()); err != nil {
		t.Fatalf("decode closed event: %v (written %d bytes)", err, len(deadCtrl.Written()))
	}
	if ev.Kind != protocol.PortForwardEventKind_Closed {
		t.Fatalf("event kind = %v, want Closed", ev.Kind)
	}
	if c := ev.Closed(); c == nil || c.Reason != protocol.PortForwardCloseReason_TaskGone {
		t.Fatalf("closed reason = %+v, want TaskGone", c)
	}
}

func TestKillPortForward_UnknownIDAndDoubleKill(t *testing.T) {
	h := &TaskHandler{Tasks: NewTaskStore(), Registry: NewRegistry()}
	if st := h.killPortForward("", 9999); st != protocol.KillPortForwardStatus_NoSuchForward {
		t.Fatalf("unknown id: status = %v, want no_such_forward", st)
	}

	taskHex := addRunningTask(t, h, 0x33, "runner-1")
	ctrl := newRecordingBidiStream(3)
	pf := &portForward{direction: protocol.PortForwardDirection_Local, taskIDHex: taskHex,
		control: ctrl, clientCxn: &fakeConn{nextStreamID: 200}}
	id := h.pforwards().add(pf)

	if st := h.killPortForward("", id); st != protocol.KillPortForwardStatus_Ok {
		t.Fatalf("first kill: status = %v, want ok", st)
	}

	// The killed forward's client must have actually been told: the whole
	// mechanism this feature rests on is an explicit `closed` record, not
	// just a registry entry disappearing.
	var ev protocol.PortForwardEvent
	if _, err := ev.Decode(ctrl.Written()); err != nil {
		t.Fatalf("decode closed event: %v (written %d bytes)", err, len(ctrl.Written()))
	}
	if ev.Kind != protocol.PortForwardEventKind_Closed {
		t.Fatalf("event kind = %v, want Closed", ev.Kind)
	}
	if c := ev.Closed(); c == nil || c.Reason != protocol.PortForwardCloseReason_Killed {
		t.Fatalf("closed reason = %+v, want Killed", c)
	}

	// Exactly one caller may win: the second kill must not also report ok.
	if st := h.killPortForward("", id); st != protocol.KillPortForwardStatus_NoSuchForward {
		t.Fatalf("second kill: status = %v, want no_such_forward", st)
	}
}

// addRunningTaskWithCreator is addRunningTask plus a CreatorTaskID, so
// subtree-visibility tests can build a caller -> child relationship (the BFS
// in visibleToCaller walks CreatorTaskID edges).
func addRunningTaskWithCreator(t *testing.T, h *TaskHandler, first byte, runnerID string, creator protocol.TaskID) string {
	t.Helper()
	var raw [16]byte
	raw[0] = first
	idHex := hex.EncodeToString(raw[:])
	h.Tasks.mu.Lock()
	h.Tasks.tasks[idHex] = &TaskEntry{ID: idHex, Status: protocol.TaskStatus_Running, AssignedTo: runnerID, CreatorTaskID: creator}
	h.Tasks.order = append(h.Tasks.order, idHex)
	h.Tasks.mu.Unlock()
	return idHex
}

// TestVisiblePortForwards_SubtreeGatingAndTaskFilter covers the confined-caller
// branch of visiblePortForwards, which every other test in this file skips by
// passing connID="" (operator, all=true). That gap is exactly what let
// Finding 1 (kill gate checking the capability before visibility) through
// review undetected. A non-operator, non-InfoGlobal caller must see forwards
// for its own task and its descendants, must NOT see a forward belonging to
// an unrelated task, and the --task filter must narrow further within that
// visible set (not replace it — filtering to a task outside the subtree must
// not resurrect it).
func TestVisiblePortForwards_SubtreeGatingAndTaskFilter(t *testing.T) {
	h := &TaskHandler{Tasks: NewTaskStore(), Registry: NewRegistry()}
	ownHex := addRunningTask(t, h, 0x51, "runner-1") // the caller's own task
	var ownTID protocol.TaskID
	copyHexToID(t, ownHex, &ownTID)
	childHex := addRunningTaskWithCreator(t, h, 0x52, "runner-1", ownTID) // descendant
	otherHex := addRunningTask(t, h, 0x53, "runner-1")                    // unrelated task

	const callerCID = "confined-conn-subtree"
	h.principals = map[string]protocol.TaskID{callerCID: ownTID}

	conn := &fakeConn{nextStreamID: 400}
	mk := func(taskHex string, bindPort uint16) *portForward {
		return &portForward{direction: protocol.PortForwardDirection_Local, taskIDHex: taskHex,
			control: newRecordingBidiStream(trsf.StreamID(bindPort)), clientCxn: conn,
			bindAddr: "127.0.0.1", bindPort: bindPort, targetHost: "db", targetPort: 5432}
	}
	h.pforwards().add(mk(ownHex, 9001))
	childID := h.pforwards().add(mk(childHex, 9002))
	h.pforwards().add(mk(otherHex, 9003))

	// No filter: own + child visible; the unrelated task's forward is filtered out.
	got := h.visiblePortForwards(callerCID, protocol.TaskID{})
	if len(got) != 2 {
		t.Fatalf("confined caller: expected 2 visible forwards (own+child), got %d: %+v", len(got), got)
	}
	for _, fi := range got {
		gotHex := hex.EncodeToString(fi.TaskId.Id[:])
		if gotHex != ownHex && gotHex != childHex {
			t.Errorf("confined caller saw forward for out-of-subtree task %s", gotHex)
		}
	}

	// --task filter narrows to just the child, even though both own and child
	// are independently visible without it.
	var filter protocol.TaskID
	copyHexToID(t, childHex, &filter)
	filtered := h.visiblePortForwards(callerCID, filter)
	if len(filtered) != 1 || filtered[0].ForwardId != childID {
		t.Fatalf("--task filter: expected only forward %d, got %+v", childID, filtered)
	}

	// --task filter to the out-of-subtree task must not resurrect it: still invisible.
	var otherFilter protocol.TaskID
	copyHexToID(t, otherHex, &otherFilter)
	if got := h.visiblePortForwards(callerCID, otherFilter); len(got) != 0 {
		t.Fatalf("--task filter to an out-of-subtree task should stay empty, got %+v", got)
	}
}

// TestKillPortForward_InvisibleForwardNoSuchForward proves the enumeration-
// oracle fix at the killPortForward level (as opposed to
// TestDirectionGate/kill_invisible_forward_no_such_forward_not_denied in
// capabilities_test.go, which proves it at the Handle() dispatch level): a
// forward that exists but belongs to a task outside the caller's subtree
// answers no_such_forward, identical to an unknown id, and — the point of
// asserting on the registry afterward — is NOT removed. A caller who cannot
// see a forward must not be able to kill it via a lucky guess.
func TestKillPortForward_InvisibleForwardNoSuchForward(t *testing.T) {
	h := &TaskHandler{Tasks: NewTaskStore(), Registry: NewRegistry()}
	ownHex := addRunningTask(t, h, 0x61, "runner-1")
	otherHex := addRunningTask(t, h, 0x62, "runner-1")
	var ownTID protocol.TaskID
	copyHexToID(t, ownHex, &ownTID)

	const callerCID = "confined-conn-kill"
	h.principals = map[string]protocol.TaskID{callerCID: ownTID}

	pf := &portForward{direction: protocol.PortForwardDirection_Local, taskIDHex: otherHex,
		control: newRecordingBidiStream(10), clientCxn: &fakeConn{nextStreamID: 700}}
	id := h.pforwards().add(pf)

	if st := h.killPortForward(callerCID, id); st != protocol.KillPortForwardStatus_NoSuchForward {
		t.Fatalf("invisible forward: status = %v, want no_such_forward", st)
	}
	if _, ok := h.pforwards().get(id); !ok {
		t.Fatal("invisible forward must NOT be removed by a caller who cannot see it")
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

// TestKillPortForward_Remote verifies killing a -R registration through the
// same killPortForward path exercised for -L by
// TestKillPortForward_UnknownIDAndDoubleKill. Before this test, killing a
// Direction_Remote registration had zero coverage anywhere: the live
// verification for this feature drove -L only, so a wrong kill tail (leaving
// the runner's listener bound on a port with no registration left to reclaim
// it — recoverable only by restarting the runner) could ship silently.
// Asserts both halves of closePortForward's Remote tail: (a) the client's
// control stream gets the closed{killed} record, and (b) the runner gets a
// ClosePortForward for this forward id so its listener is released.
func TestKillPortForward_Remote(t *testing.T) {
	ctrl := newRecordingBidiStream(555)
	h, _, runnerConn, resp := registerRemoteForwardForTest(t, ctrl, true)
	if resp.Status != protocol.OpenPortForwardStatus_Ok {
		t.Fatalf("register status = %v, want Ok", resp.Status)
	}

	if st := h.killPortForward("", resp.ForwardId); st != protocol.KillPortForwardStatus_Ok {
		t.Fatalf("kill: status = %v, want ok", st)
	}
	if _, ok := h.pforwards().get(resp.ForwardId); ok {
		t.Fatal("registration should be removed after kill")
	}

	// (a) the client's control stream got the closed{killed} record.
	var ev protocol.PortForwardEvent
	if _, err := ev.Decode(ctrl.Written()); err != nil {
		t.Fatalf("decode closed event: %v (written %d bytes)", err, len(ctrl.Written()))
	}
	if ev.Kind != protocol.PortForwardEventKind_Closed {
		t.Fatalf("event kind = %v, want Closed", ev.Kind)
	}
	if c := ev.Closed(); c == nil || c.Reason != protocol.PortForwardCloseReason_Killed {
		t.Fatalf("closed reason = %+v, want Killed", c)
	}

	// (b) the runner was told to stop listening, so it can reclaim the port.
	found := false
	for _, raw := range runnerConn.Sent() {
		var rr protocol.RunnerRequest
		if _, err := rr.Decode(raw[1:]); err != nil { // strip ApplicationPayloadKind byte
			continue
		}
		if rr.Kind != protocol.RunnerRequestType_ClosePortForward {
			continue
		}
		cp := rr.ClosePortForward()
		if cp != nil && cp.ForwardId == resp.ForwardId {
			found = true
		}
	}
	if !found {
		t.Fatal("runner never received a ClosePortForward for the killed forward")
	}
}

// TestDropPortForwardsForConn covers the abrupt-client-death path. A client that
// is SIGKILLed — or SIGHUPed because its terminal window closed — never closes
// its control stream, so watchRemoteForwardControl stays parked and only the
// server's connection teardown can drop the registration. Verified live before
// this hook existed: a SIGKILLed `harness-cli forward -L` outlived its own
// connection's removal from activeConns and would never have been reclaimed.
func TestDropPortForwardsForConn(t *testing.T) {
	h := &TaskHandler{Tasks: NewTaskStore(), Registry: NewRegistry()}
	taskHex := addRunningTask(t, h, 0x71, "runner-1")

	deadCtrl := newRecordingBidiStream(9)
	dying := &portForward{direction: protocol.PortForwardDirection_Local, taskIDHex: taskHex,
		control: deadCtrl, clientCxn: &fakeConn{nextStreamID: 900}, clientCID: "ws:127.0.0.1:1-dead"}
	survivor := &portForward{direction: protocol.PortForwardDirection_Local, taskIDHex: taskHex,
		control: newRecordingBidiStream(10), clientCxn: &fakeConn{nextStreamID: 901}, clientCID: "ws:127.0.0.1:2-live"}
	dyingID := h.pforwards().add(dying)
	survivorID := h.pforwards().add(survivor)

	h.DropPortForwardsForConn("ws:127.0.0.1:1-dead")

	if _, ok := h.pforwards().get(dyingID); ok {
		t.Error("the dead connection's registration should have been dropped")
	}
	if _, ok := h.pforwards().get(survivorID); !ok {
		t.Error("a different connection's registration must survive")
	}
	// notify=false is load-bearing: pushing a closed event onto a stream whose
	// transport is already gone can only fail, and there is no one to tell.
	if w := deadCtrl.Written(); len(w) != 0 {
		t.Errorf("no closed event should be pushed to a dead client, got %d bytes", len(w))
	}
}
