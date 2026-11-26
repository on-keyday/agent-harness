package server

import (
	"log/slog"
	"sync"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// StartFunc starts a queued background session task already Assigned and
// bound to the runner (TaskHandler.StartQueuedSession). A non-nil error means
// the start failed and the task is already Failed.
//
// Tick calls it synchronously and assumes it is cheap: it opens streams and
// sends one request, never waiting on the runner.
type StartFunc func(runnerID objproto.ConnectionID, taskID string) error

// Scheduler places Queued tasks on runners that can take them.
// It is the orchestration glue between Registry and TaskStore.
//
// Concurrency: Tick takes an internal mutex so concurrent callers (OnChange
// fires from per-connection goroutines: open/cancel/runner-control/
// disconnect) cannot observe the same Queued task between the start and the
// subsequent state mutation. Without this serialization a task could be
// started twice — the runner would then create the same worktree branch
// twice and the second attempt would fail with "branch already exists".
type Scheduler struct {
	mu    sync.Mutex
	reg   *Registry
	store *TaskStore
	// start starts a queued background session task. Nil = background tasks
	// are not placed (they stay Queued).
	start StartFunc
}

// NewScheduler constructs a Scheduler that uses reg and store for state and
// start to begin each placed background session (pillar A spec, A-D12).
func NewScheduler(reg *Registry, store *TaskStore, start StartFunc) *Scheduler {
	return &Scheduler{reg: reg, store: store, start: start}
}

// Tick places Queued background session tasks, oldest first. For each one it
// asks Registry.Candidates — the same answer an interactive open uses (G17 of
// docs/superpowers/specs/2026-11-12-run-modes-and-records-grand-design.md) —
// and gives the task to the first candidate that:
//  1. is Idle (connected, below capacity) and not a displaced connection
//     (Registry.displaced: a takeover's predecessor, which Candidates lists
//     first because it connected earlier),
//  2. advertises the task's agent profile, if it names one, and runs
//     --oneshot if the run is one,
//  3. takes the reservation: reg.BindTask re-checks capacity under the
//     registry lock, which matters because handleOpenInteractive and readopt
//     reserve slots without holding s.mu.
//
// Having reserved, it re-reads the task: a task cancelled since the
// QueuedTasks snapshot is released and skipped. It then marks the task
// Running and starts its session (s.start). A task no candidate takes stays
// Queued, and the walk goes on to the next task rather than stopping: stopping
// would let one task pinned to an offline runner stall every later task.
//
// Candidates applies the task's Selector, which is the operator's actual
// intent and survives reconnects (ByHostname especially). BoundRunnerID is
// deliberately NOT enforced here: a runner that reconnects comes back under a
// new ConnectionID, and enforcing it would strand queued tasks across a
// runner restart.
//
// Tick serializes itself via s.mu so concurrent callers run one at a time.
// No goroutines are spawned. Oneshot tasks are no longer placed (A3): replay
// fails any left Queued.
func (s *Scheduler) Tick() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, task := range s.store.QueuedTasks() {
		// Only background sessions (A-D12). A foreground session is briefly
		// Queued between its own create and Assign in handleOpenInteractive and
		// must never be started a second time here (F3).
		if task.RunMode != protocol.RunMode_Background || s.start == nil {
			continue
		}
		for _, c := range s.reg.Candidates(task.RepoPath, task.Selector, task.ID) {
			if c.Status() != protocol.RunnerStatus_Idle || s.reg.displaced(c.ID) {
				continue
			}
			if task.AgentProfile != "" && !c.HasProfile(task.AgentProfile) {
				continue
			}
			// A runner that did not say it runs --oneshot would start the
			// interactive argv instead (A3, A-D29).
			if task.Oneshot && !c.OneshotSessions {
				continue
			}
			if !s.reg.BindTask(c.ID, task.ID) {
				continue
			}
			// Cancel does not take s.mu: the task may have been cancelled since
			// the QueuedTasks snapshot. Starting it now would let Assign turn
			// Cancelled back into Running, with no CancelTask ever sent.
			if cur, ok := s.store.Get(task.ID); !ok || cur.Status != protocol.TaskStatus_Queued {
				s.reg.UnbindTask(c.ID, task.ID)
				break
			}
			// Assign first, as handleOpenInteractive does: the session starts
			// on a Running task. A failed start has already failed the task
			// (finishWithError told the runner and the board).
			if !s.store.AssignIfQueued(task.ID, c.Identity, c.SkillsInjected) {
				// Cancelled since the re-read above.
				s.reg.UnbindTask(c.ID, task.ID)
				break
			}
			if err := s.start(c.ID, task.ID); err != nil {
				slog.Error("background start failed", "runner", c.ID, "task", task.ID, "error", err)
			}
			break
		}
	}
}
