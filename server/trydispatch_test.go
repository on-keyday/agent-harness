package server

import (
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// errConnHandle is a fakeConn variant whose SendMessage always errors.
type errConnHandle struct {
	fakeConn
}

func (e *errConnHandle) SendMessage(_ []byte) (int, uint64, error) {
	return 0, 0, errors.New("send failed")
}

// newTestDispatcher builds a Dispatcher wired to fresh Registry and TaskStore.
func newTestDispatcher() (*Dispatcher, *Registry, *TaskStore) {
	reg := NewRegistry()
	tasks := NewTaskStore()
	d := &Dispatcher{
		Registry: reg,
		Tasks:    tasks,
	}
	return d, reg, tasks
}

// registerRunner adds a runner entry with the given conn to the registry.
func registerRunner(reg *Registry, id string, conn ConnHandle, roots []string, maxTasks int) {
	reg.Add(&RunnerEntry{
		ID:           id,
		Hostname:     "host",
		AllowedRoots: roots,
		MaxTasks:     maxTasks,
		ActiveTasks:  map[string]struct{}{},
		ConnectedAt:  time.Unix(1, 0),
		LastSeen:     time.Unix(1, 0),
		Conn:         conn,
	})
}

// TestTryDispatch_HappyPath verifies that tryDispatch binds the runner, sends an
// AssignTask wire message, and transitions the task to Running.
func TestTryDispatch_HappyPath(t *testing.T) {
	d, reg, tasks := newTestDispatcher()
	fc := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:8539-10")}
	fc.nextSendStreamID = 11 // dispatcher opens body stream
	runnerID := fc.id.String()
	registerRunner(reg, runnerID, fc, []string{"/repo"}, 2)

	taskID := tasks.Create("/repo", "do work", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, "", protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any}, nil)
	task, _ := tasks.Get(taskID)

	ok := d.TryDispatch(task)
	if !ok {
		t.Fatal("expected TryDispatch to return true on happy path")
	}

	// Runner must have the task bound.
	entry, _ := reg.Get(runnerID)
	if _, has := entry.ActiveTasks[taskID]; !has {
		t.Errorf("expected task %q in runner ActiveTasks, got %v", taskID, entry.ActiveTasks)
	}

	// Task must be Running.
	te, _ := tasks.Get(taskID)
	if te.Status != protocol.TaskStatus_Running {
		t.Errorf("expected task status Running, got %v", te.Status)
	}
	if te.AssignedTo != runnerID {
		t.Errorf("expected AssignedTo=%q, got %q", runnerID, te.AssignedTo)
	}

	// Exactly one message must have been sent, prefixed with RunnerControl kind.
	if len(fc.sent) != 1 {
		t.Fatalf("expected 1 sent message, got %d", len(fc.sent))
	}
	if fc.sent[0][0] != byte(wire.ApplicationPayloadKind_RunnerControl) {
		t.Errorf("expected RunnerControl prefix byte, got %d", fc.sent[0][0])
	}

	// Decode and verify it is an AssignTask with correct repo_path.
	var req protocol.RunnerRequest
	if _, err := req.Decode(fc.sent[0][1:]); err != nil {
		t.Fatalf("decode RunnerRequest: %v", err)
	}
	if req.Kind != protocol.RunnerRequestType_AssignTask {
		t.Errorf("expected AssignTask kind, got %v", req.Kind)
	}
	at := req.AssignTask()
	if at == nil {
		t.Fatal("AssignTask() returned nil")
	}
	// Body (incl. Prompt + RepoPath) is on the recorded send stream now;
	// envelope only carries TaskID + StreamId.
	if len(fc.sendStreams) != 1 {
		t.Fatalf("expected 1 send stream, got %d", len(fc.sendStreams))
	}
	body := &protocol.AssignTaskBody{}
	if err := body.DecodeExact(fc.sendStreams[0].bytes); err != nil {
		t.Fatalf("decode AssignTaskBody: %v", err)
	}
	if string(body.Prompt) != "do work" {
		t.Errorf("prompt mismatch: got %q", body.Prompt)
	}
	if string(body.RepoPath) != "/repo" {
		t.Errorf("repo_path mismatch: got %q", body.RepoPath)
	}
}

// TestTryDispatch_NoCapacity verifies that tryDispatch returns false when all
// candidates are at capacity, and sends no messages.
func TestTryDispatch_NoCapacity(t *testing.T) {
	d, reg, tasks := newTestDispatcher()
	fc := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:8539-11")}
	runnerID := fc.id.String()

	// Runner at full capacity (MaxTasks=1, 1 active task).
	reg.Add(&RunnerEntry{
		ID:           runnerID,
		Hostname:     "host",
		AllowedRoots: []string{"/repo"},
		MaxTasks:     1,
		ActiveTasks:  map[string]struct{}{"existingtask": {}},
		ConnectedAt:  time.Unix(1, 0),
		LastSeen:     time.Unix(1, 0),
		Conn:         fc,
	})

	taskID := tasks.Create("/repo", "do work", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, "", protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any}, nil)
	task, _ := tasks.Get(taskID)

	ok := d.TryDispatch(task)
	if ok {
		t.Fatal("expected TryDispatch to return false when no capacity")
	}
	if len(fc.sent) != 0 {
		t.Errorf("expected no messages sent, got %d", len(fc.sent))
	}
	// Task must remain Queued.
	te, _ := tasks.Get(taskID)
	if te.Status != protocol.TaskStatus_Queued {
		t.Errorf("expected task to remain Queued, got %v", te.Status)
	}
}

// TestTryDispatch_SendError verifies that tryDispatch rolls back the BindTask
// reservation when SendMessage fails, and returns false.
func TestTryDispatch_SendError(t *testing.T) {
	d, reg, tasks := newTestDispatcher()

	fc := &errConnHandle{
		fakeConn: fakeConn{
			id:               objproto.MustParseConnectionID("ws:127.0.0.1:8539-12"),
			nextSendStreamID: 13, // dispatcher allocates body stream before SendMessage
		},
	}
	runnerID := fc.id.String()
	registerRunner(reg, runnerID, fc, []string{"/repo"}, 2)

	taskID := tasks.Create("/repo", "work", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, "", protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any}, nil)
	task, _ := tasks.Get(taskID)

	ok := d.TryDispatch(task)
	if ok {
		t.Fatal("expected TryDispatch to return false on send error")
	}

	// Runner slot must have been rolled back (UnbindTask called).
	entry, _ := reg.Get(runnerID)
	if _, has := entry.ActiveTasks[taskID]; has {
		t.Errorf("expected task to be unbound after send error, ActiveTasks=%v", entry.ActiveTasks)
	}

	// Task must remain Queued (not Running).
	te, _ := tasks.Get(taskID)
	if te.Status != protocol.TaskStatus_Queued {
		t.Errorf("expected task to remain Queued after send error, got %v", te.Status)
	}
}

// boardRunnerID builds an agentboard.RunnerID from a connection ID string so
// tests can call board.Registry().Validate without going through the protocol
// wire. The format mirrors runnerIDFromConnID (same underlying string).
func boardRunnerID(t *testing.T, connIDStr string) agentboard.RunnerID {
	t.Helper()
	cid, err := objproto.ParseConnectionID(connIDStr, 0)
	if err != nil {
		t.Fatalf("boardRunnerID: parse %q: %v", connIDStr, err)
	}
	var rid agentboard.RunnerID
	rid.SetTransport([]byte(cid.Transport))
	ip := cid.Addr.Addr().AsSlice()
	rid.SetIpAddr(ip)
	rid.Port = uint16(cid.Addr.Port())
	rid.UniqueNumber = cid.ID
	return rid
}

// boardTaskID converts a hex task ID string to agentboard.TaskID.
func boardTaskID(taskIDHex string) agentboard.TaskID {
	var tid agentboard.TaskID
	raw, _ := hex.DecodeString(taskIDHex)
	copy(tid.Id[:], raw)
	return tid
}

// TestTryDispatch_RegistersTicket verifies the full ticket lifecycle:
//  1. TryDispatch registers a non-zero ticket in the Board.
//  2. The AssignTask sent to the runner contains that ticket.
//  3. board.Registry().Validate returns HelloStatusOk.
//  4. After TaskFinished is handled, Validate returns HelloStatusUnknownTask.
func TestTryDispatch_RegistersTicket(t *testing.T) {
	board := agentboard.New(agentboard.Config{
		RingN:      8,
		TopicTTL:   time.Hour,
		MaxTopics:  16,
		MaxPayload: 1024,
	})
	defer board.Close()

	reg := NewRegistry()
	tasks := NewTaskStore()
	d := &Dispatcher{
		Registry: reg,
		Tasks:    tasks,
		Board:    board,
	}

	fc := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:8539-20")}
	fc.nextSendStreamID = 21 // dispatcher opens body stream
	runnerID := fc.id.String()
	registerRunner(reg, runnerID, fc, []string{"/repo"}, 2)

	taskIDHex := tasks.Create("/repo", "ticket-test", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, "", protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any}, nil)
	task, _ := tasks.Get(taskIDHex)

	ok := d.TryDispatch(task)
	if !ok {
		t.Fatal("expected TryDispatch to return true")
	}

	// 1. Exactly one message was sent; decode the AssignTask.
	if len(fc.sent) != 1 {
		t.Fatalf("expected 1 sent message, got %d", len(fc.sent))
	}
	var req protocol.RunnerRequest
	if _, err := req.Decode(fc.sent[0][1:]); err != nil {
		t.Fatalf("decode RunnerRequest: %v", err)
	}
	at := req.AssignTask()
	if at == nil {
		t.Fatal("AssignTask() returned nil")
	}

	// 2. AuthTicket lives on the streamed body now, not the envelope.
	if len(fc.sendStreams) != 1 {
		t.Fatalf("expected 1 send stream, got %d", len(fc.sendStreams))
	}
	body := &protocol.AssignTaskBody{}
	if err := body.DecodeExact(fc.sendStreams[0].bytes); err != nil {
		t.Fatalf("decode AssignTaskBody: %v", err)
	}
	var zero [16]byte
	if body.AuthTicket == zero {
		t.Error("expected non-zero AuthTicket in AssignTaskBody, got all-zero")
	}

	// 3. board.Registry().Validate must return HelloStatusOk for the registered ticket.
	brid := boardRunnerID(t, runnerID)
	btid := boardTaskID(taskIDHex)
	status := board.Registry().Validate(brid, btid, body.AuthTicket)
	if status != agentboard.HelloStatusOk {
		t.Errorf("expected HelloStatusOk after TryDispatch, got %v", status)
	}

	// 4. Simulate TaskFinished via RunnerHandler — the ticket must be revoked.
	rh := &RunnerHandler{
		Registry: reg,
		Tasks:    tasks,
		Now:      time.Now,
		Board:    board,
	}

	var tfTaskID protocol.TaskID
	raw, _ := hex.DecodeString(taskIDHex)
	copy(tfTaskID.Id[:], raw)
	tfMsg := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_TaskFinished}
	tfMsg.SetTaskFinished(protocol.TaskFinished{TaskId: tfTaskID, ExitCode: 0})
	payload, err := tfMsg.Append(nil)
	if err != nil {
		t.Fatalf("encode TaskFinished: %v", err)
	}
	rh.Handle(fc, payload)

	// After revocation, Validate must return HelloStatusUnknownTask.
	status = board.Registry().Validate(brid, btid, body.AuthTicket)
	if status != agentboard.HelloStatusUnknownTask {
		t.Errorf("expected HelloStatusUnknownTask after TaskFinished, got %v", status)
	}
}
