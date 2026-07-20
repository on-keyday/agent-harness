package server

import (
	"testing"
	"time"

	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TestOnRemoveMarks verifies that removing a runner from the registry marks all
// tasks in its ActiveTasks snapshot as Failed with reason "runner_disconnected",
// using MarkFailed (idempotent on already-terminal tasks).
func TestOnRemoveMarks_ActiveTasksMarkedFailed(t *testing.T) {
	reg := NewRegistry()
	tasks := NewTaskStore()

	fc := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:8539-30")}
	runnerID := fc.id.String()

	// Create two tasks and manually set them to Running (simulating dispatch).
	taskA := tasks.Create("/repo", "a", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, "")
	taskB := tasks.Create("/repo", "b", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, "")
	tasks.Assign(taskA, runnerID, "")
	tasks.Assign(taskB, runnerID, "")

	// Register runner with both tasks active.
	reg.Add(&RunnerEntry{
		ID:           runnerID,
		Hostname:     "host",
		AllowedRoots: []string{"/repo"},
		MaxTasks:     2,
		ActiveTasks:  map[string]struct{}{taskA: {}, taskB: {}},
		ConnectedAt:  time.Unix(1, 0),
		LastSeen:     time.Unix(1, 0),
		Conn:         fc,
	})

	offlineEvents := 0
	// Wire OnRemove as server.go should: mark tasks failed, then publish event.
	reg.OnRemove = func(id string, snap RunnerEntry) {
		for taskID := range snap.ActiveTasks {
			tasks.MarkFailed(taskID, "runner_disconnected")
		}
		offlineEvents++
	}

	// Trigger removal.
	reg.Remove(runnerID)

	if offlineEvents != 1 {
		t.Errorf("expected OnRemove called once, got %d", offlineEvents)
	}

	// Both tasks must be Failed with reason "runner_disconnected".
	for _, taskID := range []string{taskA, taskB} {
		te, ok := tasks.Get(taskID)
		if !ok {
			t.Fatalf("task %q not found after runner removal", taskID)
		}
		if te.Status != protocol.TaskStatus_Failed {
			t.Errorf("task %q: expected Failed status, got %v", taskID, te.Status)
		}
		if string(te.ErrorMsg) != "runner_disconnected" {
			t.Errorf("task %q: expected reason 'runner_disconnected', got %q", taskID, te.ErrorMsg)
		}
	}
}

// TestOnRemoveMarks_AlreadyTerminalIsIdempotent verifies that MarkFailed is
// idempotent: tasks already in a terminal state (Succeeded/Failed/Cancelled)
// are not overwritten.
func TestOnRemoveMarks_AlreadyTerminalIsIdempotent(t *testing.T) {
	reg := NewRegistry()
	tasks := NewTaskStore()

	fc := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:8539-31")}
	runnerID := fc.id.String()

	// Create a task and manually mark it Succeeded (terminal).
	taskID := tasks.Create("/repo", "c", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, "")
	tasks.Assign(taskID, runnerID, "")
	tasks.Finish(taskID, 0, nil) // exit 0 → Succeeded

	// Register runner with the already-finished task still in ActiveTasks
	// (race condition snapshot).
	reg.Add(&RunnerEntry{
		ID:           runnerID,
		Hostname:     "host",
		AllowedRoots: []string{"/repo"},
		MaxTasks:     1,
		ActiveTasks:  map[string]struct{}{taskID: {}},
		ConnectedAt:  time.Unix(1, 0),
		LastSeen:     time.Unix(1, 0),
		Conn:         fc,
	})

	reg.OnRemove = func(id string, snap RunnerEntry) {
		for tid := range snap.ActiveTasks {
			tasks.MarkFailed(tid, "runner_disconnected")
		}
	}

	reg.Remove(runnerID)

	// Task must remain Succeeded (MarkFailed is idempotent on terminal tasks).
	te, _ := tasks.Get(taskID)
	if te.Status != protocol.TaskStatus_Succeeded {
		t.Errorf("expected task to remain Succeeded (terminal), got %v", te.Status)
	}
}

// TestOnRemoveMarks_EmptyActiveTasks verifies that removal of a runner with no
// active tasks is a no-op for the task store.
func TestOnRemoveMarks_EmptyActiveTasks(t *testing.T) {
	reg := NewRegistry()
	tasks := NewTaskStore()

	fc := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:8539-32")}
	runnerID := fc.id.String()

	reg.Add(&RunnerEntry{
		ID:           runnerID,
		Hostname:     "host",
		AllowedRoots: []string{"/repo"},
		MaxTasks:     1,
		ActiveTasks:  map[string]struct{}{}, // no active tasks
		ConnectedAt:  time.Unix(1, 0),
		LastSeen:     time.Unix(1, 0),
		Conn:         fc,
	})

	markFailedCalled := 0
	reg.OnRemove = func(id string, snap RunnerEntry) {
		for tid := range snap.ActiveTasks {
			tasks.MarkFailed(tid, "runner_disconnected")
			markFailedCalled++
		}
	}

	reg.Remove(runnerID)

	if markFailedCalled != 0 {
		t.Errorf("expected MarkFailed not called for empty ActiveTasks, got %d calls", markFailedCalled)
	}
	// Task store must be empty.
	if len(tasks.List(0)) != 0 {
		t.Errorf("expected no tasks, got %d", len(tasks.List(0)))
	}
}

// TestAfterMuxStopped_DetachedStaysBoundUntilOnRemove verifies the
// runner-death-while-detached path: when a Detached session's mux stops
// (runner died without sending TaskFinished), afterMuxStopped must NOT
// release the registry slot — the task has to stay in ActiveTasks so the
// later Registry.Remove → OnRemove sweep can MarkFailed it
// "runner_disconnected". Regression: an unconditional UnbindTask emptied the
// OnRemove snapshot and the task stayed Detached forever.
func TestAfterMuxStopped_DetachedStaysBoundUntilOnRemove(t *testing.T) {
	reg := NewRegistry()
	tasks := NewTaskStore()
	h := &TaskHandler{Tasks: tasks, Registry: reg, Sessions: NewSessionRegistry()}

	fc := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:8539-31")}
	runnerID := fc.id.String()

	id := tasks.Create("/repo", "", protocol.TaskKind_Interactive, protocol.ClientKind_Cli, protocol.TaskID{}, runnerID, protocol.RunnerSelector{}, nil, protocol.Capability_All, "")
	tasks.Assign(id, runnerID, "")
	if err := tasks.SetDetached(id); err != nil {
		t.Fatalf("SetDetached: %v", err)
	}

	reg.Add(&RunnerEntry{
		ID:          runnerID,
		MaxTasks:    2,
		ActiveTasks: map[string]struct{}{id: {}},
		ConnectedAt: time.Unix(1, 0),
		LastSeen:    time.Unix(1, 0),
		Conn:        fc,
	})
	reg.OnRemove = func(_ string, snap RunnerEntry) {
		for taskID := range snap.ActiveTasks {
			tasks.MarkFailed(taskID, "runner_disconnected")
		}
	}

	// Mux teardown fires first (observed ordering on abrupt runner death:
	// the stream dies before the conn-GC deregisters the runner).
	h.afterMuxStopped(id, runnerID)

	if e, ok := reg.Get(runnerID); !ok || len(e.ActiveTasks) != 1 {
		t.Fatalf("detached task must stay bound after afterMuxStopped; ActiveTasks=%v", e.ActiveTasks)
	}

	// Conn GC deregisters the runner ~a minute later.
	reg.Remove(runnerID)

	te, ok := tasks.Get(id)
	if !ok {
		t.Fatal("task vanished")
	}
	if te.Status != protocol.TaskStatus_Failed {
		t.Errorf("status = %v, want Failed", te.Status)
	}
	if string(te.ErrorMsg) != "runner_disconnected" {
		t.Errorf("reason = %q, want runner_disconnected", te.ErrorMsg)
	}
}

// TestAfterMuxStopped_RunningIsCancelledAndUnbound preserves the existing
// attached-session semantics: mux stop while the task is Running cancels it
// (a provisional terminal state a late TaskFinished may overwrite) and
// releases the slot immediately.
func TestAfterMuxStopped_RunningIsCancelledAndUnbound(t *testing.T) {
	reg := NewRegistry()
	tasks := NewTaskStore()
	h := &TaskHandler{Tasks: tasks, Registry: reg, Sessions: NewSessionRegistry()}

	fc := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:8539-32")}
	runnerID := fc.id.String()

	id := tasks.Create("/repo", "", protocol.TaskKind_Interactive, protocol.ClientKind_Cli, protocol.TaskID{}, runnerID, protocol.RunnerSelector{}, nil, protocol.Capability_All, "")
	tasks.Assign(id, runnerID, "")

	reg.Add(&RunnerEntry{
		ID:          runnerID,
		MaxTasks:    2,
		ActiveTasks: map[string]struct{}{id: {}},
		ConnectedAt: time.Unix(1, 0),
		LastSeen:    time.Unix(1, 0),
		Conn:        fc,
	})

	h.afterMuxStopped(id, runnerID)

	te, _ := tasks.Get(id)
	if te.Status != protocol.TaskStatus_Cancelled {
		t.Errorf("status = %v, want Cancelled", te.Status)
	}
	if e, _ := reg.Get(runnerID); len(e.ActiveTasks) != 0 {
		t.Errorf("slot must be released for a terminal task; ActiveTasks=%v", e.ActiveTasks)
	}
}
