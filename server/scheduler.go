package server

import (
	"log/slog"
	"sync"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// AssignFunc dispatches an AssignTask message to a runner.
// Returns an error if the runner cannot be reached; the scheduler must NOT mark
// the task Running in that case.
//
// The scheduler calls AssignFunc synchronously and assumes it is cheap (i.e.,
// it enqueues one bgn-encoded message into a buffered channel or socket write,
// and returns quickly). If AssignFunc is slow, Tick will block for that duration.
// Callers should ensure AssignFunc does not block indefinitely.
type AssignFunc func(runnerID objproto.ConnectionID, taskID string) error

// Scheduler places Queued tasks on runners that can take them.
// It is the orchestration glue between Registry and TaskStore.
//
// Concurrency: Tick takes an internal mutex so concurrent callers (OnChange
// fires from per-connection goroutines: submit/cancel/runner-control/
// disconnect) cannot observe the same Queued task between the assign-send and
// the subsequent state mutation. Without this serialization a task could be
// AssignTask'd twice — the runner would then create the same worktree branch
// twice and the second attempt would fail with "branch already exists".
type Scheduler struct {
	mu     sync.Mutex
	reg    *Registry
	store  *TaskStore
	assign AssignFunc
	// start starts a queued background session task already Assigned and
	// bound to the runner (TaskHandler.StartQueuedSession). Nil = background
	// tasks are not placed (they stay Queued).
	start AssignFunc
}

// SetSessionStart wires the start of queued background session tasks
// (pillar A spec, A-D12). Set once, before the first Tick.
func (s *Scheduler) SetSessionStart(start AssignFunc) { s.start = start }

// NewScheduler constructs a Scheduler that uses reg and store for state and
// assign for dispatching work to runners.
func NewScheduler(reg *Registry, store *TaskStore, assign AssignFunc) *Scheduler {
	return &Scheduler{
		reg:    reg,
		store:  store,
		assign: assign,
	}
}

// Tick places Queued tasks, oldest first. For each one it asks
// Registry.Candidates — the same answer submit and an interactive open use
// (G17 of docs/superpowers/specs/2026-11-12-run-modes-and-records-grand-design.md)
// — and gives the task to the first candidate that:
//  1. is Idle (connected, below capacity) and not a displaced connection
//     (Registry.displaced: a takeover's predecessor, which Candidates lists
//     first because it connected earlier),
//  2. advertises the task's agent profile, if it names one,
//  3. takes the reservation: reg.BindTask re-checks capacity under the
//     registry lock, which matters because handleOpenInteractive and readopt
//     reserve slots without holding s.mu.
//
// Having reserved, it re-reads the task: a task cancelled since the
// QueuedTasks snapshot is released and skipped. It then sends AssignTask
// (s.assign). On a send error the reservation is
// released, the error logged and the next candidate tried. On success
// store.Assign records the runner. A task no candidate takes stays Queued, and
// the walk goes on to the next task rather than stopping: stopping would let
// one task pinned to an offline runner stall every later task.
//
// Candidates applies the task's Selector, which is the operator's actual
// intent and survives reconnects (ByHostname especially). BoundRunnerID is
// deliberately NOT enforced here. It is a snapshot of the candidate chosen at
// submit time, and a runner that reconnects comes back under a new
// ConnectionID — enforcing it would strand queued tasks across a runner
// restart.
//
// Tick serializes itself via s.mu so concurrent callers run one at a time.
// No goroutines are spawned.
func (s *Scheduler) Tick() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, task := range s.store.QueuedTasks() {
		// Only queued work Tick can start: a oneshot, or a background session
		// (A-D12). A foreground session is briefly Queued between its own
		// create and Assign in handleOpenInteractive and must never be handed
		// to the oneshot path (F3).
		background := task.RunMode == protocol.RunMode_Background
		if task.Kind != protocol.TaskKind_Oneshot && !background {
			continue
		}
		if background && s.start == nil {
			continue
		}
		for _, c := range s.reg.Candidates(task.RepoPath, task.Selector) {
			if c.Status() != protocol.RunnerStatus_Idle || s.reg.displaced(c.ID) {
				continue
			}
			if task.AgentProfile != "" && !c.HasProfile(task.AgentProfile) {
				continue
			}
			// Reserve BEFORE sending: the runner may answer the moment
			// AssignTask leaves, and its TaskAccepted and its log JOIN are both
			// authorized by ownsTask.
			if !s.reg.BindTask(c.ID, task.ID) {
				continue
			}
			// Cancel does not take s.mu: the task may have been cancelled since
			// the QueuedTasks snapshot. Sending it now would let Assign turn
			// Cancelled back into Running, with no CancelTask ever sent.
			if cur, ok := s.store.Get(task.ID); !ok || cur.Status != protocol.TaskStatus_Queued {
				s.reg.UnbindTask(c.ID, task.ID)
				break
			}
			if background {
				// Assign first, as handleOpenInteractive does: the session
				// starts on a Running task. A failed start has already failed
				// the task (finishWithError told the runner and the board).
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
			if err := s.assign(c.ID, task.ID); err != nil {
				s.reg.UnbindTask(c.ID, task.ID)
				slog.Error("assign failed",
					"runner", c.ID,
					"task", task.ID,
					"repo", task.RepoPath,
					"error", err,
				)
				continue
			}
			// WorktreeDir is left empty here; it will be filled in by
			// TaskStarted when the runner reports back that it has started.
			s.store.Assign(task.ID, c.Identity, "", c.SkillsInjected)
			break
		}
	}
}
