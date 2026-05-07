package server

import (
	"testing"
	"time"

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
		ID:           fc.id.String(),
		Hostname:     "testhost",
		AllowedRoots: []string{"/r"},
		MaxTasks:     1,
		ActiveTasks:  map[string]struct{}{},
		Conn:         fc,
	})
	taskID := s.tasks.Create("/r", "do-the-thing", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, "", protocol.RunnerSelector{}, nil)
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

// TestSweepIdleDetached_CancelsExpiredSessions verifies that sweepIdleDetached
// cancels a Detached task whose DetachedAt timestamp is past the cutoff.
func TestSweepIdleDetached_CancelsExpiredSessions(t *testing.T) {
	s := New(Config{DetachIdleTimeout: time.Minute})

	taskID := s.tasks.Create("/r", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Unspecified, "", protocol.RunnerSelector{}, nil)
	// Transition through Running → Detached manually.
	s.tasks.Assign(taskID, "runner-1", "/wt")
	if err := s.tasks.SetDetached(taskID); err != nil {
		t.Fatalf("SetDetached: %v", err)
	}

	// Back-date DetachedAt so the task appears idle beyond the timeout.
	func() {
		s.tasks.mu.Lock()
		defer s.tasks.mu.Unlock()
		e := s.tasks.tasks[taskID]
		e.DetachedAt = uint64(time.Now().Add(-2 * time.Minute).UnixNano())
	}()

	s.sweepIdleDetached(time.Now())

	got, ok := s.tasks.Get(taskID)
	if !ok {
		t.Fatal("task disappeared")
	}
	if got.Status != protocol.TaskStatus_Cancelled {
		t.Fatalf("want Cancelled, got %v", got.Status)
	}
}

// TestSweepIdleDetached_KeepsRecentSessions verifies that sweepIdleDetached
// does NOT cancel a Detached task whose DetachedAt is within the idle timeout.
func TestSweepIdleDetached_KeepsRecentSessions(t *testing.T) {
	s := New(Config{DetachIdleTimeout: time.Hour})

	taskID := s.tasks.Create("/r", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Unspecified, "", protocol.RunnerSelector{}, nil)
	s.tasks.Assign(taskID, "runner-1", "/wt")
	if err := s.tasks.SetDetached(taskID); err != nil {
		t.Fatalf("SetDetached: %v", err)
	}
	// DetachedAt is just-now (set by SetDetached), well within the 1-hour timeout.

	s.sweepIdleDetached(time.Now())

	got, ok := s.tasks.Get(taskID)
	if !ok {
		t.Fatal("task disappeared")
	}
	if got.Status != protocol.TaskStatus_Detached {
		t.Fatalf("want Detached, got %v", got.Status)
	}
}

// TestRestartCancelsDetached verifies that the restart loop marks Detached
// tasks as Cancelled (simulating what Run does after WAL replay).
func TestRestartCancelsDetached(t *testing.T) {
	s := New(Config{})

	taskID := s.tasks.Create("/r", "p", protocol.TaskKind_Interactive, protocol.ClientKind_Unspecified, "", protocol.RunnerSelector{}, nil)
	s.tasks.Assign(taskID, "runner-1", "/wt")
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
