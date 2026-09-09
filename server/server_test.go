package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/appwire"
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
		ID:           fc.id.String(),
		Hostname:     "testhost",
		AllowedRoots: []string{"/r"},
		MaxTasks:     1,
		ActiveTasks:  map[string]struct{}{},
		Conn:         fc,
	})
	taskID := s.tasks.Create("/r", "do-the-thing", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, Scope{}, "")
	if err := s.sendAssign(fc.id.String(), taskID); err != nil {
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
}

func TestSendAssignDisconnected(t *testing.T) {
	s := New(Config{})
	err := s.sendAssign("nonexistent-runner", "00000000")
	if err == nil {
		t.Fatal("expected error")
	}
}

// TestRestartCancelsDetached verifies that the restart loop marks Detached
// tasks as Cancelled (simulating what Run does after WAL replay).
func TestRestartCancelsDetached(t *testing.T) {
	s := New(Config{})

	taskID := s.tasks.Create("/r", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, Scope{}, "")
	s.tasks.Assign(taskID, "runner-1", "/wt", false)
	if err := s.tasks.SetDetached(taskID); err != nil {
		t.Fatalf("SetDetached: %v", err)
	}

	// Simulate the restart loop from Run.
	for _, task := range s.tasks.List(0) {
		if task.Status == protocol.TaskStatus_Detached {
			s.tasks.Cancel(task.ID)
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
