package server

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TaskEntry holds the current state of a task throughout its lifecycle.
type TaskEntry struct {
	ID       string
	RepoPath string
	Prompt   string
	Kind     protocol.TaskKind
	// OriginKind records which kind of caller submitted this task — set by
	// the server from the originating connection's last ClientHello. Stays
	// ClientKind_Unspecified when the connection didn't issue a hello (e.g.
	// orphan tasks replayed from an older WAL with no origin field).
	OriginKind  protocol.ClientKind
	Status      protocol.TaskStatus
	AssignedTo  string
	WorktreeDir string
	CreatedAt   time.Time
	StartedAt   *time.Time
	EndedAt     *time.Time
	ExitCode    *int32
	ErrorMsg    []byte

	// Selector is the runner-selection constraint supplied at task submission.
	// A zero value (Kind == RunnerSelectorKind_Any) means "any available runner".
	Selector protocol.RunnerSelector
	// BoundRunnerID, when non-empty, pins this task to a specific runner ID
	// (the registry's string key). Populated by the scheduler or submit handler
	// when the caller supplies a ByRunnerId selector.
	BoundRunnerID string
	// ExtraArgs are per-task CLI arguments that the runner appends to its
	// runner-global --claude-args baseline before exec'ing claude.
	// Sourced from SubmitRequest.ExtraArgs / OpenInteractiveRequest.ExtraArgs
	// at task creation time and forwarded verbatim through AssignTask /
	// OpenExecRunnerRequest to the runner.
	ExtraArgs []string
}

// TaskStore is the in-memory authority for task lifecycle.
//
// Read methods (Get, NextQueuedForRepo, List) return value snapshots; callers
// may freely read the returned values. All mutations go through Assign, Finish,
// Cancel, SetWorktreeDir, or Create.
//
// A WAL can be attached via SetWAL; subsequent mutations append events to it.
type TaskStore struct {
	mu    sync.RWMutex
	tasks map[string]*TaskEntry
	order []string // insertion order; used by List and NextQueuedForRepo
	wal   *WAL

	OnCreate func(id string)                                         // optional; called after each Create. Server uses this to register pubsub taps.
	OnAssign func(id, runnerID, worktreeDir string)                  // optional; called after Assign transitions a task to Running.
	OnFinish func(id string, exit int32, status protocol.TaskStatus) // optional; called after Finish marks a task terminal.
	OnCancel func(id string)                                         // optional; called after Cancel marks a task Cancelled.
}

// SetWAL attaches a WAL to which subsequent mutations append. nil disables WAL hooks.
// Not concurrency-safe; call once during server startup before Run.
func (s *TaskStore) SetWAL(w *WAL) { s.wal = w }

// NewTaskStore creates an empty TaskStore.
func NewTaskStore() *TaskStore {
	return &TaskStore{
		tasks: make(map[string]*TaskEntry),
	}
}

// newTaskID generates a task ID: 16 random bytes hex-encoded to 32 lowercase
// hex characters.
func newTaskID() string {
	var b [16]byte
	_, err := rand.Read(b[:])
	if err != nil {
		panic("crypto/rand.Read failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// Create adds a new Queued task for the given repo, prompt, kind, and
// origin. It returns the new task's ID (32 lowercase hex characters). Use
// TaskKind_Oneshot for prompt-driven submits and TaskKind_Interactive for
// PTY claude sessions opened via OpenInteractive. origin records which kind
// of caller (cli / tui / webui) submitted the task; pass
// ClientKind_Unspecified when the caller is unknown (e.g. tests, internal
// scheduler bookkeeping).
//
// boundRunnerID pins the task to a specific runner (empty = no pinning).
// selector is the runner-selection constraint; use a zero value for "any".
// extraArgs are forwarded verbatim to the runner and appended to
// --claude-args at exec time; pass nil for none.
func (s *TaskStore) Create(repo, prompt string, kind protocol.TaskKind, origin protocol.ClientKind, boundRunnerID string, selector protocol.RunnerSelector, extraArgs []string) string {
	s.mu.Lock()
	id := newTaskID()
	s.tasks[id] = &TaskEntry{
		ID:            id,
		RepoPath:      repo,
		Prompt:        prompt,
		Kind:          kind,
		OriginKind:    origin,
		Status:        protocol.TaskStatus_Queued,
		CreatedAt:     time.Now(),
		BoundRunnerID: boundRunnerID,
		Selector:      selector,
		ExtraArgs:     extraArgs,
	}
	s.order = append(s.order, id)
	if s.wal != nil {
		if err := s.wal.Write(WALEvent{
			Type:          "task_created",
			TaskID:        id,
			RepoPath:      repo,
			Prompt:        prompt,
			Kind:          uint8(kind),
			OriginKind:    uint8(origin),
			BoundRunnerID: boundRunnerID,
			Selector:      selector,
			ExtraArgs:     extraArgs,
		}); err != nil {
			slog.Error("WAL write failed", "op", "task_created", "task_id", id, "err", err)
		}
	}
	onCreate := s.OnCreate
	s.mu.Unlock()
	if onCreate != nil {
		onCreate(id)
	}
	return id
}

// ResumeError is the typed error TaskStore.Resume returns when the resume
// preconditions fail. The handler maps these to wire-level
// SubmitStatus_ResumeNotFound / SubmitStatus_ResumeNotTerminal codes.
type ResumeError uint8

const (
	ResumeErrNotFound ResumeError = iota + 1
	ResumeErrNotTerminal
)

func (e ResumeError) Error() string {
	switch e {
	case ResumeErrNotFound:
		return "resume_not_found"
	case ResumeErrNotTerminal:
		return "resume_not_terminal"
	default:
		return "resume_unknown_error"
	}
}

// Resume re-queues an existing terminal TaskEntry under the SAME id with a
// fresh prompt + extra-args + selector + bound-runner. The transition is
// performed under s.mu so two concurrent Resume calls cannot both observe
// the task as terminal — the second caller sees ResumeErrNotTerminal because
// the first has already flipped Status back to Queued. This is the
// multi-resume guard.
//
// Repo path is intentionally NOT mutable on resume: the whole point of
// reusing the id is that the runner re-attaches its worktree to the retained
// `harness/<id>` branch under the original repo, so claude's project key
// matches the previous run. Callers that pass a mismatching repo are wrong;
// the handler validates that before reaching this method.
//
// PeekRepo returns the existing entry's repo so the handler can resolve
// runner candidates without first holding the lock. Use it before Resume.
//
// Returns the post-reset TaskEntry snapshot on success.
func (s *TaskStore) Resume(id, prompt string, extraArgs []string, selector protocol.RunnerSelector, boundRunnerID string) (TaskEntry, error) {
	s.mu.Lock()
	e, ok := s.tasks[id]
	if !ok {
		s.mu.Unlock()
		return TaskEntry{}, ResumeErrNotFound
	}
	switch e.Status {
	case protocol.TaskStatus_Succeeded,
		protocol.TaskStatus_Failed,
		protocol.TaskStatus_Cancelled:
		// terminal — proceed
	default:
		s.mu.Unlock()
		return TaskEntry{}, ResumeErrNotTerminal
	}

	// Reset the per-run fields. CreatedAt is preserved (first submission
	// time stays meaningful in List output); StartedAt/EndedAt/ExitCode/
	// DiffInfo/AssignedTo/WorktreeDir all become "fresh run" defaults.
	e.Status = protocol.TaskStatus_Queued
	e.AssignedTo = ""
	e.WorktreeDir = ""
	e.StartedAt = nil
	e.EndedAt = nil
	e.ExitCode = nil
	e.ErrorMsg = nil
	e.Prompt = prompt
	e.ExtraArgs = extraArgs
	e.Selector = selector
	e.BoundRunnerID = boundRunnerID

	if s.wal != nil {
		if err := s.wal.Write(WALEvent{
			Type:          "task_resumed",
			TaskID:        id,
			Prompt:        prompt,
			ExtraArgs:     extraArgs,
			Selector:      selector,
			BoundRunnerID: boundRunnerID,
		}); err != nil {
			slog.Error("WAL write failed", "op", "task_resumed", "task_id", id, "err", err)
		}
	}

	snapshot := *e
	onCreate := s.OnCreate
	s.mu.Unlock()

	// Reuse OnCreate so downstream wiring (pubsub task_queued, log store
	// taps) treats the resumed task like a fresh queue arrival. The task is
	// already in s.tasks under the original id, so any per-id state these
	// hooks set up is idempotent (re-tap the same log topic, etc.).
	if onCreate != nil {
		onCreate(id)
	}
	return snapshot, nil
}

// PeekRepo returns the RepoPath + Kind for an existing TaskEntry without
// transitioning it. Used by the resume path in handleSubmit/handleOpenInteractive
// so candidate resolution runs against the original repo (the request's
// RepoPath is ignored on resume — only the original worktree is meaningful).
// Returns ok=false if the id does not exist.
func (s *TaskStore) PeekRepo(id string) (repo string, kind protocol.TaskKind, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, exists := s.tasks[id]
	if !exists {
		return "", 0, false
	}
	return e.RepoPath, e.Kind, true
}

// Get returns a value snapshot of the TaskEntry for id.
// The returned value is independent of the internal map.
func (s *TaskStore) Get(id string) (TaskEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.tasks[id]
	if !ok {
		return TaskEntry{}, false
	}
	return *e, true
}

// Assign transitions the task to Running, recording the runner and worktree.
func (s *TaskStore) Assign(id, runnerID, worktreeDir string) {
	now := time.Now()
	s.mu.Lock()
	e, ok := s.tasks[id]
	if !ok {
		s.mu.Unlock()
		return
	}
	e.Status = protocol.TaskStatus_Running
	e.AssignedTo = runnerID
	e.WorktreeDir = worktreeDir
	e.StartedAt = &now
	if s.wal != nil {
		if err := s.wal.Write(WALEvent{Type: "task_assigned", TaskID: id, RunnerID: runnerID, WorktreeDir: worktreeDir, Ts: now.UnixNano()}); err != nil {
			slog.Error("WAL write failed", "op", "task_assigned", "task_id", id, "err", err)
		}
	}
	onAssign := s.OnAssign
	s.mu.Unlock()
	if onAssign != nil {
		onAssign(id, runnerID, worktreeDir)
	}
}

// Finish marks the task terminal. exit==0 → Succeeded; non-zero → Failed.
// It records ExitCode, DiffInfo, and EndedAt.
func (s *TaskStore) Finish(id string, exit int32, errorMsg []byte) {
	now := time.Now()
	s.mu.Lock()
	e, ok := s.tasks[id]
	if !ok {
		s.mu.Unlock()
		return
	}
	var finalStatus protocol.TaskStatus
	if exit == 0 {
		finalStatus = protocol.TaskStatus_Succeeded
	} else {
		finalStatus = protocol.TaskStatus_Failed
	}
	e.Status = finalStatus
	exitCopy := exit
	e.ExitCode = &exitCopy
	e.ErrorMsg = errorMsg
	e.EndedAt = &now
	if s.wal != nil {
		if err := s.wal.Write(WALEvent{Type: "task_finished", TaskID: id, ExitCode: &exitCopy, DiffInfo: errorMsg, Ts: now.UnixNano()}); err != nil {
			slog.Error("WAL write failed", "op", "task_finished", "task_id", id, "err", err)
		}
	}
	onFinish := s.OnFinish
	s.mu.Unlock()
	if onFinish != nil {
		onFinish(id, exit, finalStatus)
	}
}

// Cancel sets the task to Cancelled and records EndedAt. Idempotent: if the
// task is already in a terminal state, the call is a no-op.
func (s *TaskStore) Cancel(id string) {
	now := time.Now()
	s.mu.Lock()
	e, ok := s.tasks[id]
	if !ok {
		s.mu.Unlock()
		return
	}
	// Idempotent: skip if already terminal.
	switch e.Status {
	case protocol.TaskStatus_Succeeded, protocol.TaskStatus_Failed, protocol.TaskStatus_Cancelled:
		s.mu.Unlock()
		return
	}
	e.Status = protocol.TaskStatus_Cancelled
	e.EndedAt = &now
	if s.wal != nil {
		if err := s.wal.Write(WALEvent{Type: "task_cancelled", TaskID: id, Ts: now.UnixNano()}); err != nil {
			slog.Error("WAL write failed", "op", "task_cancelled", "task_id", id, "err", err)
		}
	}
	onCancel := s.OnCancel
	s.mu.Unlock()
	if onCancel != nil {
		onCancel(id)
	}
}

// MarkFailed transitions a non-terminal task to Failed, recording reason in DiffInfo.
// Idempotent: if the task is already in a terminal state (Succeeded, Failed, Cancelled),
// the call is a no-op. Emits a "task_failed" WAL event with the reason string.
// This is the preferred path for server-internal failures (e.g. runner disconnected)
// where there is no meaningful exit code.
func (s *TaskStore) MarkFailed(id, reason string) {
	now := time.Now()
	s.mu.Lock()
	e, ok := s.tasks[id]
	if !ok {
		s.mu.Unlock()
		return
	}
	// Idempotent: skip if already terminal.
	switch e.Status {
	case protocol.TaskStatus_Succeeded, protocol.TaskStatus_Failed, protocol.TaskStatus_Cancelled:
		s.mu.Unlock()
		return
	}
	e.Status = protocol.TaskStatus_Failed
	e.EndedAt = &now
	e.ErrorMsg = []byte(reason)
	if s.wal != nil {
		if err := s.wal.Write(WALEvent{Type: "task_failed", TaskID: id, Reason: reason, Ts: now.UnixNano()}); err != nil {
			slog.Error("WAL write failed", "op", "task_failed", "task_id", id, "err", err)
		}
	}
	s.mu.Unlock()
}

// SetWorktreeDir updates the worktree path for a task (called when the runner reports TaskStarted).
// Returns false if the task is not present.
func (s *TaskStore) SetWorktreeDir(id, wt string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return false
	}
	t.WorktreeDir = wt
	return true
}

// NextQueuedForRepo returns a value snapshot of the earliest-created Queued
// task whose RepoPath equals repo. Returns (zero, false) if no such task exists.
func (s *TaskStore) NextQueuedForRepo(repo string) (TaskEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// order slice preserves insertion (creation) order; iterate to find first match.
	for _, id := range s.order {
		e := s.tasks[id]
		if e.RepoPath == repo && e.Status == protocol.TaskStatus_Queued {
			return *e, true
		}
	}
	return TaskEntry{}, false
}

// ReplayEvents reconstructs in-memory state from a WAL. Must be called BEFORE the TaskStore is used.
// Tasks left in Running state after replay are forced to Failed (the runner is presumed lost across restart).
func (s *TaskStore) ReplayEvents(events []WALEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ev := range events {
		switch ev.Type {
		case "task_created":
			// OriginKind is best-effort: legacy WAL entries (pre-ii) lack
			// the origin_kind field and unmarshal to 0 (Unspecified),
			// which is exactly the desired default.
			// Selector and BoundRunnerID default to zero/empty for pre-3.1
			// WAL entries, which is the correct "any runner" sentinel.
			s.tasks[ev.TaskID] = &TaskEntry{
				ID:            ev.TaskID,
				RepoPath:      ev.RepoPath,
				Prompt:        ev.Prompt,
				Kind:          protocol.TaskKind(ev.Kind),
				OriginKind:    protocol.ClientKind(ev.OriginKind),
				Status:        protocol.TaskStatus_Queued,
				CreatedAt:     time.Unix(0, ev.Ts),
				BoundRunnerID: ev.BoundRunnerID,
				Selector:      ev.Selector,
				ExtraArgs:     ev.ExtraArgs,
			}
			s.order = append(s.order, ev.TaskID)
		case "task_assigned":
			if t, ok := s.tasks[ev.TaskID]; ok {
				t.Status = protocol.TaskStatus_Running
				t.AssignedTo = ev.RunnerID
				t.WorktreeDir = ev.WorktreeDir
				ts := time.Unix(0, ev.Ts)
				t.StartedAt = &ts
			}
		case "task_finished":
			if t, ok := s.tasks[ev.TaskID]; ok {
				t.Status = protocol.TaskStatus_Succeeded
				if ev.ExitCode != nil {
					t.ExitCode = ev.ExitCode
					if *ev.ExitCode != 0 {
						t.Status = protocol.TaskStatus_Failed
					}
				}
				t.ErrorMsg = ev.DiffInfo
				ts := time.Unix(0, ev.Ts)
				t.EndedAt = &ts
			}
		case "task_cancelled":
			if t, ok := s.tasks[ev.TaskID]; ok {
				t.Status = protocol.TaskStatus_Cancelled
				ts := time.Unix(0, ev.Ts)
				t.EndedAt = &ts
			}
		case "task_failed":
			// MarkFailed events: move task to Failed and store reason in DiffInfo.
			if t, ok := s.tasks[ev.TaskID]; ok {
				t.Status = protocol.TaskStatus_Failed
				ts := time.Unix(0, ev.Ts)
				t.EndedAt = &ts
				t.ErrorMsg = []byte(ev.Reason)
			}
		case "task_resumed":
			// Resume on replay: reset the existing TaskEntry to Queued under
			// the same id, picking up the new prompt + extra args + selector
			// + bound runner from the event. If the entry doesn't exist
			// (corrupt WAL ordering), drop the event silently — the
			// task_created that should precede this would have already failed
			// to apply too.
			if t, ok := s.tasks[ev.TaskID]; ok {
				t.Status = protocol.TaskStatus_Queued
				t.AssignedTo = ""
				t.WorktreeDir = ""
				t.StartedAt = nil
				t.EndedAt = nil
				t.ExitCode = nil
				t.ErrorMsg = nil
				t.Prompt = ev.Prompt
				t.ExtraArgs = ev.ExtraArgs
				t.Selector = ev.Selector
				t.BoundRunnerID = ev.BoundRunnerID
			}
		case "task_pruned":
			if _, ok := s.tasks[ev.TaskID]; ok {
				delete(s.tasks, ev.TaskID)
				// Rebuild order to drop this id.
				kept := s.order[:0]
				for _, oid := range s.order {
					if oid != ev.TaskID {
						kept = append(kept, oid)
					}
				}
				s.order = kept
			}
		}
	}
	// Any task still Running after full replay had no Finished event — restart killed it.
	for _, t := range s.tasks {
		if t.Status == protocol.TaskStatus_Running {
			t.Status = protocol.TaskStatus_Failed
			now := time.Now()
			t.EndedAt = &now
		}
	}
}

// PruneTerminal removes terminal-status tasks whose EndedAt is before cutoff.
// For each pruned task, its log file at <logDir>/<id>.log is also deleted (errors are logged but non-fatal).
// A "task_pruned" WAL event is emitted so a subsequent replay applies the same removal.
// Returns the number of tasks removed. logDir may be "" to skip log-file deletion.
func (s *TaskStore) PruneTerminal(cutoff time.Time, logDir string) int {
	s.mu.Lock()
	var pruned []string
	keepOrder := s.order[:0]
	for _, id := range s.order {
		t := s.tasks[id]
		if t == nil {
			continue
		}
		terminal := false
		switch t.Status {
		case protocol.TaskStatus_Succeeded, protocol.TaskStatus_Failed, protocol.TaskStatus_Cancelled:
			terminal = true
		}
		if terminal && t.EndedAt != nil && t.EndedAt.Before(cutoff) {
			pruned = append(pruned, id)
			delete(s.tasks, id)
			continue
		}
		keepOrder = append(keepOrder, id)
	}
	s.order = keepOrder
	if s.wal != nil {
		now := time.Now().UnixNano()
		for _, id := range pruned {
			if err := s.wal.Write(WALEvent{Type: "task_pruned", TaskID: id, Ts: now}); err != nil {
				slog.Error("WAL write failed", "op", "task_pruned", "task_id", id, "err", err)
			}
		}
	}
	s.mu.Unlock()

	if logDir != "" {
		for _, id := range pruned {
			path := filepath.Join(logDir, id+".log")
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				slog.Warn("prune log file", "task_id", id, "err", err)
			}
		}
	}
	return len(pruned)
}

// List returns value snapshots of the N most-recent entries in insertion order.
// If limit <= 0, all entries are returned. The returned slice is independent
// of the internal map.
func (s *TaskStore) List(limit int) []TaskEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	total := len(s.order)
	start := 0
	if limit > 0 && limit < total {
		start = total - limit
	}
	slice := s.order[start:]
	result := make([]TaskEntry, len(slice))
	for i, id := range slice {
		result[i] = *s.tasks[id]
	}
	return result
}
