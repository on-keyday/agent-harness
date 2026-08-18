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
	taskID := store.Create("/x", "prompt-a", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, Scope{}, "")

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
	taskID := store.Create("/x", "prompt-a", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, Scope{}, "")

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
	taskID := store.Create("/x", "prompt-a", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, Scope{}, "")

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
	taskID := store.Create("/x", "prompt-a", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, Scope{}, "")

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
	taskA := store.Create("/x", "a", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, Scope{}, "")
	taskB := store.Create("/x", "b", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, Scope{}, "")
	taskC := store.Create("/x", "c", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, Scope{}, "")

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

// hostnameSelector builds a ByHostname RunnerSelector for the tests below.
func hostnameSelector(t *testing.T, name string) protocol.RunnerSelector {
	t.Helper()
	var sel protocol.RunnerSelector
	sel.Kind = protocol.RunnerSelectorKind_ByHostname
	var h protocol.Hostname
	if !h.SetName([]byte(name)) {
		t.Fatalf("hostname %q too long", name)
	}
	sel.SetHostname(h)
	return sel
}

func idleRunner(id, hostname string, profiles []string) *RunnerEntry {
	return &RunnerEntry{
		ID:            id,
		Hostname:      hostname,
		AllowedRoots:  []string{"/x"},
		AgentProfiles: profiles,
		MaxTasks:      4,
		ActiveTasks:   map[string]struct{}{},
		ConnectedAt:   time.Unix(1, 0),
		LastSeen:      time.Unix(1, 0),
		Conn:          &fakeConn{},
	}
}

// TestSchedulerHonorsSelector: the assignment step must respect the selector the
// task was submitted with. handleSubmit only VALIDATES the pin (PinnedNotFound /
// AmbiguousRunner) — without this, Tick handed the task to whichever idle runner
// served a matching repo path, so `submit --host h2` ran on h1 roughly half the
// time. Interactive opens never had the bug: they assign synchronously to the
// candidate they resolved.
func TestSchedulerHonorsSelector(t *testing.T) {
	reg := NewRegistry()
	reg.Add(idleRunner("r1", "h1", nil))
	reg.Add(idleRunner("r2", "h2", nil))

	store := NewTaskStore()
	taskID := store.Create("/x", "pinned", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified,
		protocol.TaskID{}, "r2", hostnameSelector(t, "h2"), nil, protocol.Capability_All, Scope{}, "")

	var captured []string
	s := NewScheduler(reg, store, func(runnerID, tID string) error {
		captured = append(captured, runnerID+":"+tID)
		return nil
	})
	s.Tick()

	if len(captured) != 1 {
		t.Fatalf("expected exactly one assignment, got %v", captured)
	}
	if captured[0] != "r2:"+taskID {
		t.Fatalf("task pinned to h2 was assigned to %q, want r2:%s", captured[0], taskID)
	}
}

// TestSchedulerHonorsAgentProfile: a task naming a profile must not be handed to
// a runner that does not advertise it. That combination failed at the RUNNER
// with `agent_profile: unknown agent profile "x" (have [...])` — a wrong-runner
// symptom reported as a profile error.
func TestSchedulerHonorsAgentProfile(t *testing.T) {
	reg := NewRegistry()
	reg.Add(idleRunner("r1", "h1", []string{"claude"}))
	reg.Add(idleRunner("r2", "h2", []string{"codex"}))

	store := NewTaskStore()
	taskID := store.Create("/x", "codex task", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified,
		protocol.TaskID{}, "r2", protocol.RunnerSelector{}, nil, protocol.Capability_All, Scope{}, "codex")

	var captured []string
	s := NewScheduler(reg, store, func(runnerID, tID string) error {
		captured = append(captured, runnerID+":"+tID)
		return nil
	})
	s.Tick()

	if len(captured) != 1 || captured[0] != "r2:"+taskID {
		t.Fatalf("task requesting profile codex assigned to %v, want r2:%s", captured, taskID)
	}
}

// TestSchedulerPinnedTaskDoesNotBlockQueue: skipping ineligible tasks must not
// turn into head-of-line blocking. An older task pinned to an absent runner has
// to be stepped over, not allowed to stall every later task on that root.
func TestSchedulerPinnedTaskDoesNotBlockQueue(t *testing.T) {
	reg := NewRegistry()
	reg.Add(idleRunner("r1", "h1", nil))

	store := NewTaskStore()
	blocked := store.Create("/x", "pinned elsewhere", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified,
		protocol.TaskID{}, "", hostnameSelector(t, "h-absent"), nil, protocol.Capability_All, Scope{}, "")
	runnable := store.Create("/x", "any runner", protocol.TaskKind_Oneshot, protocol.ClientKind_Unspecified,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, Scope{}, "")

	var captured []string
	s := NewScheduler(reg, store, func(runnerID, tID string) error {
		captured = append(captured, runnerID+":"+tID)
		return nil
	})
	s.Tick()

	if len(captured) != 1 || captured[0] != "r1:"+runnable {
		t.Fatalf("expected r1 to skip the pinned task and take %s, got %v", runnable, captured)
	}
	if got, _ := store.Get(blocked); got.Status != protocol.TaskStatus_Queued {
		t.Fatalf("pinned task should stay Queued, got %v", got.Status)
	}
}
