package server

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

func mustParseCID(t *testing.T, s string) objproto.ConnectionID {
	t.Helper()
	return objproto.MustParseConnectionID(s)
}

func TestSendAssignReachesRunner(t *testing.T) {
	fc := &fakeConn{id: mustParseCID(t, "ws:127.0.0.1:8539-9")}
	fc.nextSendStreamID = 7 // sendAssign opens a body stream; allow it.
	s := New(Config{Addr: "localhost:0"})
	s.registry.Add(&RunnerEntry{
		ID:           fc.id,
		Hostname:     "testhost",
		AllowedRoots: []string{"/r"},
		MaxTasks:     1,
		ActiveTasks:  map[string]struct{}{},
		Conn:         fc,
	})
	taskID := s.tasks.Create("/r", "do-the-thing", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_All, Scope: authority.Scope{}}, "")
	if err := s.sendAssign(fc.id, taskID); err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(fc.sent) != 1 {
		t.Fatalf("want 1 message, got %d", len(fc.sent))
	}
	if fc.sent[0][0] != byte(appwire.AppKind_RunnerControl) {
		t.Fatalf("want kind=RunnerControl byte, got %d", fc.sent[0][0])
	}
	// Decode the runner request — envelope only carries TaskID + StreamId.
	rr := &protocol.RunnerRequest{}
	if _, err := rr.Decode(fc.sent[0][1:]); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rr.Kind != protocol.RunnerRequestType_AssignTask {
		t.Fatalf("kind: %v", rr.Kind)
	}
	at := rr.AssignTask()
	if at == nil {
		t.Fatalf("assign-task envelope nil")
	}
	if at.StreamId != 7 {
		t.Fatalf("envelope StreamId=%d want 7", at.StreamId)
	}
	// Body (incl. Prompt) is on the recorded send stream.
	if len(fc.sendStreams) != 1 {
		t.Fatalf("expected 1 send stream, got %d", len(fc.sendStreams))
	}
	body := &protocol.AssignTaskBody{}
	if err := body.DecodeExact(fc.sendStreams[0].bytes); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if string(body.Prompt) != "do-the-thing" {
		t.Fatalf("body.Prompt=%q want do-the-thing", body.Prompt)
	}
	// The runner's repo gate reads this; TestTryDispatch_HappyPath asserted it
	// until TryDispatch was deleted.
	if string(body.RepoPath) != "/r" {
		t.Fatalf("body.RepoPath=%q want /r", body.RepoPath)
	}
}

func TestSendAssignDisconnected(t *testing.T) {
	s := New(Config{})
	err := s.sendAssign(tcid("nonexistent-runner"), "00000000")
	if err == nil {
		t.Fatal("expected error")
	}
}

// TestRestartCancelsDetached verifies that the restart loop marks Detached
// tasks as Cancelled (simulating what Run does after WAL replay).
func TestRestartCancelsDetached(t *testing.T) {
	s := New(Config{})

	taskID := s.tasks.Create("/r", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_All, Scope: authority.Scope{}}, "")
	s.tasks.Assign(taskID, testRunnerID("runner-1"), "/wt", false)
	if err := s.tasks.SetDetached(taskID); err != nil {
		t.Fatalf("SetDetached: %v", err)
	}

	// Simulate the restart loop from Run.
	for _, task := range s.tasks.List(0) {
		if task.Status == protocol.TaskStatus_Detached {
			s.tasks.Cancel(task.ID, protocol.EndReason_Cancelled, protocol.TaskID{})
		}
	}

	got, ok := s.tasks.Get(taskID)
	if !ok {
		t.Fatal("task disappeared")
	}
	if got.Status != protocol.TaskStatus_Cancelled {
		t.Fatalf("want Cancelled, got %v", got.Status)
	}
}

// boardRunnerID builds an protocol.RunnerID from a connection ID string so
// tests can call board.Registry().Ticket without going through the protocol
// wire. It derives the identity the same way makeProtoRunnerID does, so a test
// that registers by connection-id string and validates by identity agrees with
// itself — the server itself derives nothing from an address any more.
func boardRunnerID(t *testing.T, connIDStr string) protocol.RunnerID {
	t.Helper()
	return makeProtoRunnerID(t, connIDStr)
}

// boardTaskID converts a hex task ID string to protocol.TaskID.
func boardTaskID(taskIDHex string) protocol.TaskID {
	var tid protocol.TaskID
	raw, _ := hex.DecodeString(taskIDHex)
	copy(tid.Id[:], raw)
	return tid
}

// TestSendAssignRegistersTicket verifies the ticket lifecycle on the path the
// scheduler dispatches through (Scheduler.Tick → Server.sendAssign):
//  1. sendAssign registers a non-zero ticket in the Board.
//  2. The AssignTask sent to the runner contains that ticket.
//  3. board.Registry().Ticket returns it.
//  4. After TaskFinished is handled, the board holds no ticket for the task.
func TestSendAssignRegistersTicket(t *testing.T) {
	board := agentboard.New(agentboard.Config{
		RingN:      8,
		TopicTTL:   time.Hour,
		MaxTopics:  16,
		MaxPayload: 1024,
	})
	defer board.Close()

	s := New(Config{})
	s.Board = board
	reg, tasks := s.registry, s.tasks

	fc := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:8539-20")}
	fc.nextSendStreamID = 21 // sendAssign opens a body stream
	runnerID := fc.id
	reg.Add(&RunnerEntry{
		ID:               runnerID,
		Identity:         testRunnerID(runnerID.String()),
		Hostname:         "host",
		AllowedRoots:     []string{"/repo"},
		MaxTasks:         2,
		ActiveTasks:      map[string]struct{}{},
		ConnectedAt:      time.Unix(1, 0),
		LastTaskActivity: time.Unix(1, 0),
		Conn:             fc,
	})

	taskIDHex := tasks.Create("/repo", "ticket-test", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{Kind: protocol.RunnerSelectorKind_Any}, nil, authority.Grant{Caps: protocol.Capability_All, Scope: authority.Scope{}}, "")

	// Through the scheduler, as production does: it reserves the slot (so the
	// runner owns the task when its TaskFinished arrives), then sendAssign.
	NewScheduler(reg, tasks, s.sendAssign).Tick()
	if task, _ := tasks.Get(taskIDHex); task.Status != protocol.TaskStatus_Running {
		t.Fatalf("status=%v want Running after Tick", task.Status)
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

	// 3. The board holds the ticket the runner was sent.
	brid := boardRunnerID(t, runnerID.String())
	btid := boardTaskID(taskIDHex)
	if got, ok := board.Registry().Ticket(brid, btid); !ok || got != body.AuthTicket {
		t.Errorf("board ticket after sendAssign = %x, %v; want the one in AssignTaskBody", got, ok)
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

	// After revocation the board holds no ticket for the task.
	if _, ok := board.Registry().Ticket(brid, btid); ok {
		t.Error("ticket still registered after TaskFinished")
	}
}
