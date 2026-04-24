package server

import (
	"sort"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// RunnerEntry holds the current state of a connected runner.
//
// Note: Get and OldestIdleForRepo return pointers to the same RunnerEntry
// stored in the map. Callers must not mutate the returned pointer;
// all mutations must go through SetStatus (or Remove/Add). This matches
// the intended use pattern where only the server reads entries after
// retrieval, and the scheduler calls SetStatus for state transitions.
type RunnerEntry struct {
	ID          string // = objproto.ConnectionID.String()
	RepoPath    string
	Status      protocol.RunnerStatus
	CurrentTask string // empty when Idle/Offline
	ConnectedAt time.Time
	LastSeen    time.Time
}

// Registry tracks connected runners. All public methods are concurrency-safe.
type Registry struct {
	mu      sync.Mutex
	runners map[string]*RunnerEntry
}

// NewRegistry creates an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		runners: make(map[string]*RunnerEntry),
	}
}

// Add inserts or replaces the entry keyed by e.ID.
func (r *Registry) Add(e *RunnerEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runners[e.ID] = e
}

// Remove deletes the entry with the given id. No-op if absent.
func (r *Registry) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.runners, id)
}

// Get returns the entry for id. The returned pointer aliases the stored entry;
// callers must not mutate it.
func (r *Registry) Get(id string) (*RunnerEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.runners[id]
	return e, ok
}

// SetStatus updates Status, CurrentTask, and LastSeen for the entry with id.
// No-op if the id is not found.
func (r *Registry) SetStatus(id string, s protocol.RunnerStatus, currentTask string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.runners[id]
	if !ok {
		return
	}
	e.Status = s
	e.CurrentTask = currentTask
	e.LastSeen = time.Now()
}

// OldestIdleForRepo returns the Idle runner for repo with the earliest
// ConnectedAt time, or nil if no such runner exists. When two Idle runners
// share the same ConnectedAt, the one with the lexicographically smaller ID
// is returned to keep the result deterministic.
func (r *Registry) OldestIdleForRepo(repo string) *RunnerEntry {
	r.mu.Lock()
	defer r.mu.Unlock()

	var candidates []*RunnerEntry
	for _, e := range r.runners {
		if e.RepoPath == repo && e.Status == protocol.RunnerStatus_Idle {
			candidates = append(candidates, e)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].ConnectedAt.Equal(candidates[j].ConnectedAt) {
			return candidates[i].ID < candidates[j].ID
		}
		return candidates[i].ConnectedAt.Before(candidates[j].ConnectedAt)
	})
	return candidates[0]
}

// List returns a snapshot of all entries in arbitrary order.
// The returned pointers alias the stored entries; callers must not mutate them.
func (r *Registry) List() []*RunnerEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]*RunnerEntry, 0, len(r.runners))
	for _, e := range r.runners {
		result = append(result, e)
	}
	return result
}
