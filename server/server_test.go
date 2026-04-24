package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

func mustParseCID(t *testing.T, s string) objproto.ConnectionID {
	t.Helper()
	return objproto.MustParseConnectionID(s)
}

func TestSendAssignReachesRunner(t *testing.T) {
	fc := &fakeConn{id: mustParseCID(t, "ws:127.0.0.1:8539-9")}
	s := New(Config{Addr: "localhost:0"})
	s.registry.Add(&RunnerEntry{
		ID:       fc.id.String(),
		RepoPath: "/r",
		Status:   protocol.RunnerStatus_Idle,
		Conn:     fc,
	})
	taskID := s.tasks.Create("/r", "do-the-thing")
	if err := s.sendAssign(fc.id.String(), taskID); err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(fc.sent) != 1 {
		t.Fatalf("want 1 message, got %d", len(fc.sent))
	}
	if fc.sent[0][0] != byte(wire.ApplicationPayloadKind_RunnerControl) {
		t.Fatalf("want kind=RunnerControl byte, got %d", fc.sent[0][0])
	}
	// Decode the runner request
	rr := &protocol.RunnerRequest{}
	if _, err := rr.Decode(fc.sent[0][1:]); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rr.Kind != protocol.RunnerRequestType_AssignTask {
		t.Fatalf("kind: %v", rr.Kind)
	}
	at := rr.AssignTask()
	if at == nil || string(at.Prompt) != "do-the-thing" {
		t.Fatalf("assign-task: %+v", at)
	}
}

func TestSendAssignDisconnected(t *testing.T) {
	s := New(Config{})
	err := s.sendAssign("nonexistent-runner", "00000000")
	if err == nil {
		t.Fatal("expected error")
	}
}
