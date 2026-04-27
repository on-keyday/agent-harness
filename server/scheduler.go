package server

import (
	"log/slog"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// AssignFunc dispatches an AssignTask message to a runner.
// Returns an error if the runner cannot be reached; the scheduler must NOT mark
// the task Running in that case.
//
// The scheduler calls AssignFunc synchronously and assumes it is cheap (i.e.,
// it enqueues one bgn-encoded message into a buffered channel or socket write,
// and returns quickly). If AssignFunc is slow, Tick will block for that duration.
// Callers should ensure AssignFunc does not block indefinitely.
type AssignFunc func(runnerID, taskID string) error

// Scheduler matches Queued tasks to available runners sharing a compatible repo root.
// It is the orchestration glue between Registry and TaskStore.
//
// Atomicity note: the state mutation (store.Assign + reg.BindTask) is NOT
// atomic across both stores. A tiny window exists where the task is Running but
// the runner slot is not yet bound. For v1 single-process operation this is
// acceptable because Tick is driven by a single goroutine in practice, and the
// window only affects concurrent Tick callers seeing a partially-updated state.
type Scheduler struct {
	reg    *Registry
	store  *TaskStore
	assign AssignFunc
}

// NewScheduler constructs a Scheduler that uses reg and store for state and
// assign for dispatching work to runners.
func NewScheduler(reg *Registry, store *TaskStore, assign AssignFunc) *Scheduler {
	return &Scheduler{
		reg:   reg,
		store: store,
		assign: assign,
	}
}

// Tick performs one pass over all runners in the Registry. For each runner with
// available capacity it tries to find a Queued task on a compatible repo and
// dispatch it.
//
// Order of operations per available runner:
//  1. Skip runners that are offline (Conn == nil) or at capacity.
//  2. Find the next Queued task for any of the runner's AllowedRoots via store.NextQueuedForRoot.
//  3. If none, skip this runner.
//  4. Call s.assign(runner.ID, task.ID).
//  5. On error: log via slog.Error and continue; no state change.
//  6. On success: store.Assign(task.ID, runner.ID, "") and reg.BindTask(runner.ID, task.ID).
//
// Tick is safe to call concurrently; it relies on Registry and TaskStore's
// internal RWMutexes for concurrency safety. No goroutines are spawned.
// The call returns as soon as all runners have been processed.
func (s *Scheduler) Tick() {
	runners := s.reg.List()
	for _, runner := range runners {
		// Skip runners that are offline or at capacity.
		if runner.Status() != protocol.RunnerStatus_Idle {
			continue
		}

		// Find a queued task for any root this runner serves.
		var task *TaskEntry
		var foundRepo string
		for _, root := range runner.AllowedRoots {
			if t, ok := s.store.NextQueuedForRepo(root); ok {
				task = &t
				foundRepo = root
				break
			}
		}
		if task == nil {
			continue
		}

		if err := s.assign(runner.ID, task.ID); err != nil {
			slog.Error("assign failed",
				"runner", runner.ID,
				"task", task.ID,
				"repo", foundRepo,
				"error", err,
			)
			continue
		}

		// WorktreeDir is left empty here; it will be filled in by TaskStarted
		// when the runner reports back that it has started the task.
		s.store.Assign(task.ID, runner.ID, "")
		s.reg.BindTask(runner.ID, task.ID)
	}
}
