package server

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TestSchedulerAssignsOnePair verifies that Tick assigns a single available runner
// to a Queued task on a compatible root.
func TestSchedulerAssignsOnePair(t *testing.T) {
	reg := NewRegistry()
	reg.Add(&RunnerEntry{
		ID:           "r1",
		Hostname:     "h1",
		AllowedRoots: []string{"/x"},
		MaxTasks:     1,
		ActiveTasks:  map[string]struct{}{},
		ConnectedAt:  time.Unix(1, 0),
		LastSeen:     time.Unix(1, 0),
		Conn:         &fakeConn{},
	})

	store := NewTaskStore()
	taskID := store.Create("/x", "prompt-a", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, "")

	var captured []string
	assignFn := func(runnerID, tID string) error {
		captured = append(captured, runnerID+":"+tID)
		return nil
	}

	s := NewScheduler(reg, store, assignFn)
	s.Tick()

	if len(captured) != 1 {
		t.Fatalf("expected assignFn called once, got %d times: %v", len(captured), captured)
	}
	if !strings.HasPrefix(captured[0], "r1:") {
		t.Fatalf("expected pair starting with \"r1:\", got %q", captured[0])
	}

	// Runner must now have the task in ActiveTasks.
	entry, ok := reg.Get("r1")
	if !ok {
		t.Fatal("runner r1 not found after Tick")
	}
	if _, bound := entry.ActiveTasks[taskID]; !bound {
		t.Fatalf("expected task %q in runner ActiveTasks, got %v", taskID, entry.ActiveTasks)
	}
	if entry.Status() != protocol.RunnerStatus_Busy {
		t.Fatalf("expected runner Status=Busy (at capacity), got %v", entry.Status())
	}

	// Task must now be Running.
	task, ok := store.Get(taskID)
	if !ok {
		t.Fatal("task not found after Tick")
	}
	if task.Status != protocol.TaskStatus_Running {
		t.Fatalf("expected task Status=Running, got %v", task.Status)
	}
}

// TestSchedulerNoMatch verifies that Tick does not assign when runner's roots don't
// contain the task's repo.
func TestSchedulerNoMatch(t *testing.T) {
	reg := NewRegistry()
	reg.Add(&RunnerEntry{
		ID:           "r1",
		Hostname:     "h1",
		AllowedRoots: []string{"/y"},
		MaxTasks:     1,
		ActiveTasks:  map[string]struct{}{},
		ConnectedAt:  time.Unix(1, 0),
		LastSeen:     time.Unix(1, 0),
		Conn:         &fakeConn{},
	})

	store := NewTaskStore()
	taskID := store.Create("/x", "prompt-a", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, "")

	assignFn := func(runnerID, tID string) error {
		t.Fatal("assignFn must not be called when there is no repo match")
		return nil
	}

	s := NewScheduler(reg, store, assignFn)
	s.Tick()

	// Runner must remain Idle (no active tasks).
	entry, _ := reg.Get("r1")
	if entry.Status() != protocol.RunnerStatus_Idle {
		t.Fatalf("expected runner to remain Idle, got %v", entry.Status())
	}

	// Task must remain Queued.
	task, _ := store.Get(taskID)
	if task.Status != protocol.TaskStatus_Queued {
		t.Fatalf("expected task to remain Queued, got %v", task.Status)
	}
}

// TestSchedulerSkipsBusy verifies that Tick only assigns runners with capacity and
// ignores runners at capacity.
func TestSchedulerSkipsBusy(t *testing.T) {
	reg := NewRegistry()
	reg.Add(&RunnerEntry{
		ID:           "r1",
		Hostname:     "h1",
		AllowedRoots: []string{"/x"},
		MaxTasks:     1,
		ActiveTasks:  map[string]struct{}{},
		ConnectedAt:  time.Unix(1, 0),
		LastSeen:     time.Unix(1, 0),
		Conn:         &fakeConn{},
	})
	// r2 starts at capacity (1/1).
	reg.Add(&RunnerEntry{
		ID:           "r2",
		Hostname:     "h2",
		AllowedRoots: []string{"/x"},
		MaxTasks:     1,
		ActiveTasks:  map[string]struct{}{"existing": {}},
		ConnectedAt:  time.Unix(2, 0),
		LastSeen:     time.Unix(2, 0),
		Conn:         &fakeConn{},
	})

	store := NewTaskStore()
	taskID := store.Create("/x", "prompt-a", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, "")

	var assigned []string
	assignFn := func(runnerID, tID string) error {
		assigned = append(assigned, runnerID)
		return nil
	}

	s := NewScheduler(reg, store, assignFn)
	s.Tick()

	if len(assigned) != 1 {
		t.Fatalf("expected 1 assignment, got %d: %v", len(assigned), assigned)
	}
	if assigned[0] != "r1" {
		t.Fatalf("expected assignment to r1 (Idle), got %q", assigned[0])
	}

	// r1 must be Busy (task bound).
	r1, _ := reg.Get("r1")
	if _, bound := r1.ActiveTasks[taskID]; !bound {
		t.Fatalf("expected task %q bound to r1, got ActiveTasks=%v", taskID, r1.ActiveTasks)
	}
}

// TestSchedulerAssignErrorLeavesQueued verifies that when assignFn errors,
// neither the task nor the runner change state.
func TestSchedulerAssignErrorLeavesQueued(t *testing.T) {
	reg := NewRegistry()
	reg.Add(&RunnerEntry{
		ID:           "r1",
		Hostname:     "h1",
		AllowedRoots: []string{"/x"},
		MaxTasks:     1,
		ActiveTasks:  map[string]struct{}{},
		ConnectedAt:  time.Unix(1, 0),
		LastSeen:     time.Unix(1, 0),
		Conn:         &fakeConn{},
	})

	store := NewTaskStore()
	taskID := store.Create("/x", "prompt-a", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, "")

	assignFn := func(runnerID, tID string) error {
		return errors.New("boom")
	}

	s := NewScheduler(reg, store, assignFn)
	s.Tick() // must not panic; error is logged, not propagated

	// Runner must remain Idle (no active tasks added due to error).
	entry, ok := reg.Get("r1")
	if !ok {
		t.Fatal("runner r1 not found")
	}
	if entry.Status() != protocol.RunnerStatus_Idle {
		t.Fatalf("expected runner to remain Idle after assign error, got %v", entry.Status())
	}
	if len(entry.ActiveTasks) != 0 {
		t.Fatalf("expected runner ActiveTasks empty, got %v", entry.ActiveTasks)
	}

	// Task must remain Queued.
	task, ok := store.Get(taskID)
	if !ok {
		t.Fatal("task not found")
	}
	if task.Status != protocol.TaskStatus_Queued {
		t.Fatalf("expected task to remain Queued after assign error, got %v", task.Status)
	}
}

// TestSchedulerMultipleRunnersFIFO verifies that when multiple available runners and
// multiple Queued tasks exist on the same repo, Tick assigns one task per runner
// in FIFO order and leaves remaining tasks Queued.
func TestSchedulerMultipleRunnersFIFO(t *testing.T) {
	reg := NewRegistry()
	reg.Add(&RunnerEntry{
		ID:           "r1",
		Hostname:     "h1",
		AllowedRoots: []string{"/x"},
		MaxTasks:     1,
		ActiveTasks:  map[string]struct{}{},
		ConnectedAt:  time.Unix(2, 0),
		LastSeen:     time.Unix(2, 0),
		Conn:         &fakeConn{},
	})
	reg.Add(&RunnerEntry{
		ID:           "r2",
		Hostname:     "h2",
		AllowedRoots: []string{"/x"},
		MaxTasks:     1,
		ActiveTasks:  map[string]struct{}{},
		ConnectedAt:  time.Unix(1, 0),
		LastSeen:     time.Unix(1, 0),
		Conn:         &fakeConn{},
	})

	store := NewTaskStore()
	taskA := store.Create("/x", "a", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, "")
	taskB := store.Create("/x", "b", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, "")
	taskC := store.Create("/x", "c", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, "")

	var assigned []string
	assignFn := func(runnerID, tID string) error {
		assigned = append(assigned, runnerID+":"+tID)
		return nil
	}

	s := NewScheduler(reg, store, assignFn)
	s.Tick()

	// Exactly 2 assignments must have been made (one per available runner).
	if len(assigned) != 2 {
		t.Fatalf("expected 2 assignments, got %d: %v", len(assigned), assigned)
	}

	assignedTasks := make(map[string]bool)
	for _, pair := range assigned {
		parts := strings.SplitN(pair, ":", 2)
		if len(parts) != 2 {
			t.Fatalf("unexpected pair format %q", pair)
		}
		assignedTasks[parts[1]] = true
	}

	if !assignedTasks[taskA] {
		t.Errorf("expected task %q (a) to be assigned, assigned tasks: %v", taskA, assignedTasks)
	}
	if !assignedTasks[taskB] {
		t.Errorf("expected task %q (b) to be assigned, assigned tasks: %v", taskB, assignedTasks)
	}

	// Task c must remain Queued.
	taskCEntry, ok := store.Get(taskC)
	if !ok {
		t.Fatal("task c not found")
	}
	if taskCEntry.Status != protocol.TaskStatus_Queued {
		t.Fatalf("expected task c to remain Queued, got %v", taskCEntry.Status)
	}

	// Both runners must be at capacity (Busy).
	r1, _ := reg.Get("r1")
	if r1.Status() != protocol.RunnerStatus_Busy {
		t.Fatalf("expected r1 Status=Busy, got %v", r1.Status())
	}
	r2, _ := reg.Get("r2")
	if r2.Status() != protocol.RunnerStatus_Busy {
		t.Fatalf("expected r2 Status=Busy, got %v", r2.Status())
	}
}
