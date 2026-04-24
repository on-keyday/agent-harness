package server

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TaskEntry holds the current state of a task throughout its lifecycle.
type TaskEntry struct {
	ID          string
	RepoPath    string
	Prompt      string
	Status      protocol.TaskStatus
	AssignedTo  string
	WorktreeDir string
	CreatedAt   time.Time
	StartedAt   *time.Time
	EndedAt     *time.Time
	ExitCode    *int32
	DiffInfo    []byte
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

// Create adds a new Queued task for the given repo and prompt. It returns the
// new task's ID (32 lowercase hex characters).
func (s *TaskStore) Create(repo, prompt string) string {
	id := newTaskID()
	now := time.Now()
	e := &TaskEntry{
		ID:        id,
		RepoPath:  repo,
		Prompt:    prompt,
		Status:    protocol.TaskStatus_Queued,
		CreatedAt: now,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[id] = e
	s.order = append(s.order, id)
	if s.wal != nil {
		if err := s.wal.Write(WALEvent{Type: "task_created", TaskID: id, RepoPath: repo, Prompt: prompt, Ts: now.UnixNano()}); err != nil {
			slog.Error("WAL write failed", "op", "task_created", "task_id", id, "err", err)
		}
	}
	return id
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
	defer s.mu.Unlock()
	e, ok := s.tasks[id]
	if !ok {
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
}

// Finish marks the task terminal. exit==0 → Succeeded; non-zero → Failed.
// It records ExitCode, DiffInfo, and EndedAt.
func (s *TaskStore) Finish(id string, exit int32, diff []byte) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.tasks[id]
	if !ok {
		return
	}
	if exit == 0 {
		e.Status = protocol.TaskStatus_Succeeded
	} else {
		e.Status = protocol.TaskStatus_Failed
	}
	exitCopy := exit
	e.ExitCode = &exitCopy
	e.DiffInfo = diff
	e.EndedAt = &now
	if s.wal != nil {
		if err := s.wal.Write(WALEvent{Type: "task_finished", TaskID: id, ExitCode: &exitCopy, DiffInfo: diff, Ts: now.UnixNano()}); err != nil {
			slog.Error("WAL write failed", "op", "task_finished", "task_id", id, "err", err)
		}
	}
}

// Cancel sets the task to Cancelled and records EndedAt. Idempotent: if the
// task is already in a terminal state, the call is a no-op.
func (s *TaskStore) Cancel(id string) {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.tasks[id]
	if !ok {
		return
	}
	// Idempotent: skip if already terminal.
	switch e.Status {
	case protocol.TaskStatus_Succeeded, protocol.TaskStatus_Failed, protocol.TaskStatus_Cancelled:
		return
	}
	e.Status = protocol.TaskStatus_Cancelled
	e.EndedAt = &now
	if s.wal != nil {
		if err := s.wal.Write(WALEvent{Type: "task_cancelled", TaskID: id, Ts: now.UnixNano()}); err != nil {
			slog.Error("WAL write failed", "op", "task_cancelled", "task_id", id, "err", err)
		}
	}
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
			s.tasks[ev.TaskID] = &TaskEntry{
				ID:        ev.TaskID,
				RepoPath:  ev.RepoPath,
				Prompt:    ev.Prompt,
				Status:    protocol.TaskStatus_Queued,
				CreatedAt: time.Unix(0, ev.Ts),
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
				t.DiffInfo = ev.DiffInfo
				ts := time.Unix(0, ev.Ts)
				t.EndedAt = &ts
			}
		case "task_cancelled":
			if t, ok := s.tasks[ev.TaskID]; ok {
				t.Status = protocol.TaskStatus_Cancelled
				ts := time.Unix(0, ev.Ts)
				t.EndedAt = &ts
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
