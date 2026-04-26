package server

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TestSchedulerAssignsOnePair verifies that Tick assigns a single Idle runner
// to a Queued task on the same RepoPath.
func TestSchedulerAssignsOnePair(t *testing.T) {
	reg := NewRegistry()
	reg.Add(&RunnerEntry{
		ID:          "r1",
		RepoPath:    "/x",
		Status:      protocol.RunnerStatus_Idle,
		ConnectedAt: time.Unix(1, 0),
		LastSeen:    time.Unix(1, 0),
	})

	store := NewTaskStore()
	taskID := store.Create("/x", "prompt-a", protocol.TaskKind_Oneshot)

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

	// Runner must now be Busy with CurrentTask == taskID.
	entry, ok := reg.Get("r1")
	if !ok {
		t.Fatal("runner r1 not found after Tick")
	}
	if entry.Status != protocol.RunnerStatus_Busy {
		t.Fatalf("expected runner Status=Busy, got %v", entry.Status)
	}
	if entry.CurrentTask != taskID {
		t.Fatalf("expected CurrentTask=%q, got %q", taskID, entry.CurrentTask)
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

// TestSchedulerNoMatch verifies that Tick does not assign when runner and task
// are on different RepoPaths.
func TestSchedulerNoMatch(t *testing.T) {
	reg := NewRegistry()
	reg.Add(&RunnerEntry{
		ID:          "r1",
		RepoPath:    "/y",
		Status:      protocol.RunnerStatus_Idle,
		ConnectedAt: time.Unix(1, 0),
		LastSeen:    time.Unix(1, 0),
	})

	store := NewTaskStore()
	taskID := store.Create("/x", "prompt-a", protocol.TaskKind_Oneshot)

	assignFn := func(runnerID, tID string) error {
		t.Fatal("assignFn must not be called when there is no repo match")
		return nil
	}

	s := NewScheduler(reg, store, assignFn)
	s.Tick()

	// Runner must remain Idle.
	entry, _ := reg.Get("r1")
	if entry.Status != protocol.RunnerStatus_Idle {
		t.Fatalf("expected runner to remain Idle, got %v", entry.Status)
	}

	// Task must remain Queued.
	task, _ := store.Get(taskID)
	if task.Status != protocol.TaskStatus_Queued {
		t.Fatalf("expected task to remain Queued, got %v", task.Status)
	}
}

// TestSchedulerSkipsBusy verifies that Tick only assigns Idle runners and
// ignores Busy runners.
func TestSchedulerSkipsBusy(t *testing.T) {
	reg := NewRegistry()
	reg.Add(&RunnerEntry{
		ID:          "r1",
		RepoPath:    "/x",
		Status:      protocol.RunnerStatus_Idle,
		ConnectedAt: time.Unix(1, 0),
		LastSeen:    time.Unix(1, 0),
	})
	reg.Add(&RunnerEntry{
		ID:          "r2",
		RepoPath:    "/x",
		Status:      protocol.RunnerStatus_Busy,
		ConnectedAt: time.Unix(2, 0),
		LastSeen:    time.Unix(2, 0),
	})

	store := NewTaskStore()
	taskID := store.Create("/x", "prompt-a", protocol.TaskKind_Oneshot)

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

	// r1 must be Busy.
	r1, _ := reg.Get("r1")
	if r1.Status != protocol.RunnerStatus_Busy {
		t.Fatalf("expected r1 Status=Busy, got %v", r1.Status)
	}
	if r1.CurrentTask != taskID {
		t.Fatalf("expected r1 CurrentTask=%q, got %q", taskID, r1.CurrentTask)
	}

	// r2 must remain Busy (unchanged).
	r2, _ := reg.Get("r2")
	if r2.Status != protocol.RunnerStatus_Busy {
		t.Fatalf("expected r2 to remain Busy, got %v", r2.Status)
	}
}

// TestSchedulerAssignErrorLeavesQueued verifies that when assignFn errors,
// neither the task nor the runner change state.
func TestSchedulerAssignErrorLeavesQueued(t *testing.T) {
	reg := NewRegistry()
	reg.Add(&RunnerEntry{
		ID:          "r1",
		RepoPath:    "/x",
		Status:      protocol.RunnerStatus_Idle,
		ConnectedAt: time.Unix(1, 0),
		LastSeen:    time.Unix(1, 0),
	})

	store := NewTaskStore()
	taskID := store.Create("/x", "prompt-a", protocol.TaskKind_Oneshot)

	assignFn := func(runnerID, tID string) error {
		return errors.New("boom")
	}

	s := NewScheduler(reg, store, assignFn)
	s.Tick() // must not panic; error is logged, not propagated

	// Runner must remain Idle.
	entry, ok := reg.Get("r1")
	if !ok {
		t.Fatal("runner r1 not found")
	}
	if entry.Status != protocol.RunnerStatus_Idle {
		t.Fatalf("expected runner to remain Idle after assign error, got %v", entry.Status)
	}
	if entry.CurrentTask != "" {
		t.Fatalf("expected runner CurrentTask to remain empty, got %q", entry.CurrentTask)
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

// TestSchedulerMultipleRunnersFIFO verifies that when multiple Idle runners and
// multiple Queued tasks exist on the same repo, Tick assigns one task per runner
// in FIFO order and leaves remaining tasks Queued.
//
// Two Idle runners (r1: ConnectedAt=Unix(2,0), r2: ConnectedAt=Unix(1,0)) and
// three Queued tasks (a, b, c) are set up. After one Tick:
//   - Both a and b must be assigned to some runner (in any pairing).
//   - c must remain Queued.
//   - Both runners must be Busy.
func TestSchedulerMultipleRunnersFIFO(t *testing.T) {
	// Two Idle runners, three Queued tasks. After one Tick, two tasks should be
	// assigned (one per runner) and the third should remain Queued. We do NOT
	// assert which specific runner got which specific task because reg.List()
	// iterates an unordered map. Correctness depends on NextQueuedForRepo
	// re-reading TaskStatus on every call: when the first runner's pair is
	// committed via store.Assign, that task's Status flips to Running, so the
	// second runner's NextQueuedForRepo skips it and picks the next FIFO entry.
	// If TaskStore ever caches the queue separately from per-task status, this
	// test would silently degrade — keep that filter live.
	reg := NewRegistry()
	reg.Add(&RunnerEntry{
		ID:          "r1",
		RepoPath:    "/x",
		Status:      protocol.RunnerStatus_Idle,
		ConnectedAt: time.Unix(2, 0),
		LastSeen:    time.Unix(2, 0),
	})
	reg.Add(&RunnerEntry{
		ID:          "r2",
		RepoPath:    "/x",
		Status:      protocol.RunnerStatus_Idle,
		ConnectedAt: time.Unix(1, 0),
		LastSeen:    time.Unix(1, 0),
	})

	store := NewTaskStore()
	taskA := store.Create("/x", "a", protocol.TaskKind_Oneshot)
	taskB := store.Create("/x", "b", protocol.TaskKind_Oneshot)
	taskC := store.Create("/x", "c", protocol.TaskKind_Oneshot)

	var assigned []string
	assignFn := func(runnerID, tID string) error {
		assigned = append(assigned, runnerID+":"+tID)
		return nil
	}

	s := NewScheduler(reg, store, assignFn)
	s.Tick()

	// Exactly 2 assignments must have been made (one per Idle runner).
	if len(assigned) != 2 {
		t.Fatalf("expected 2 assignments, got %d: %v", len(assigned), assigned)
	}

	// Collect the task IDs that were assigned (order of runner→task pairing is
	// non-deterministic due to map iteration in reg.List()).
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

	// Both runners must be Busy.
	r1, _ := reg.Get("r1")
	if r1.Status != protocol.RunnerStatus_Busy {
		t.Fatalf("expected r1 Status=Busy, got %v", r1.Status)
	}
	r2, _ := reg.Get("r2")
	if r2.Status != protocol.RunnerStatus_Busy {
		t.Fatalf("expected r2 Status=Busy, got %v", r2.Status)
	}
}
