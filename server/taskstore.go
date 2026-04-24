package server

import (
	"crypto/rand"
	"encoding/hex"
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
// Get, NextQueuedForRepo, and List return pointers into the internal map.
// Callers must not mutate the returned pointers; all state transitions must go
// through Assign, Finish, or Cancel. This matches the aliasing contract used
// by Registry.
//
// WAL persistence will be retrofitted in Task 2.8 — the wal field is reserved
// as a nil-checkable hook point (not yet present).
type TaskStore struct {
	mu    sync.RWMutex
	tasks map[string]*TaskEntry
	order []string // insertion order; used by List and NextQueuedForRepo
}

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
	e := &TaskEntry{
		ID:        id,
		RepoPath:  repo,
		Prompt:    prompt,
		Status:    protocol.TaskStatus_Queued,
		CreatedAt: time.Now(),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks[id] = e
	s.order = append(s.order, id)
	return id
}

// Get returns the TaskEntry for id. The returned pointer aliases the stored
// entry; callers must not mutate it.
func (s *TaskStore) Get(id string) (*TaskEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.tasks[id]
	return e, ok
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
}

// NextQueuedForRepo returns the earliest-created Queued task whose RepoPath
// equals repo. Returns nil if no such task exists.
func (s *TaskStore) NextQueuedForRepo(repo string) *TaskEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// order slice preserves insertion (creation) order; iterate to find first match.
	for _, id := range s.order {
		e := s.tasks[id]
		if e.RepoPath == repo && e.Status == protocol.TaskStatus_Queued {
			return e
		}
	}
	return nil
}

// List returns the N most-recent entries in insertion order. If limit <= 0,
// all entries are returned. The returned pointers alias the stored entries;
// callers must not mutate them.
func (s *TaskStore) List(limit int) []*TaskEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()
	total := len(s.order)
	start := 0
	if limit > 0 && limit < total {
		start = total - limit
	}
	slice := s.order[start:]
	result := make([]*TaskEntry, len(slice))
	for i, id := range slice {
		result[i] = s.tasks[id]
	}
	return result
}
