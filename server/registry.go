package server

import (
	"sort"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// RunnerEntry holds the current state of a connected runner.
//
// Read methods (Get, OldestIdleForRepo, List) return value snapshots; callers
// may freely read the returned values. All mutations go through the Set* /
// Add / Remove methods.
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
	mu      sync.RWMutex
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

// Get returns a value snapshot of the entry for id. The returned value is
// independent of the internal map; callers may read or copy it freely.
func (r *Registry) Get(id string) (RunnerEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.runners[id]
	if !ok {
		return RunnerEntry{}, false
	}
	return *e, true
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

// SetLastSeen updates the runner's LastSeen timestamp to ts.
// Returns false if the runner is not registered.
func (r *Registry) SetLastSeen(id string, ts time.Time) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.runners[id]
	if !ok {
		return false
	}
	e.LastSeen = ts
	return true
}

// OldestIdleForRepo returns a value snapshot of the Idle runner for repo with
// the earliest ConnectedAt time. Returns (zero, false) if no such runner exists.
// When two Idle runners share the same ConnectedAt, the one with the
// lexicographically smaller ID is returned to keep the result deterministic.
func (r *Registry) OldestIdleForRepo(repo string) (RunnerEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	var candidates []*RunnerEntry
	for _, e := range r.runners {
		if e.RepoPath == repo && e.Status == protocol.RunnerStatus_Idle {
			candidates = append(candidates, e)
		}
	}
	if len(candidates) == 0 {
		return RunnerEntry{}, false
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].ConnectedAt.Equal(candidates[j].ConnectedAt) {
			return candidates[i].ID < candidates[j].ID
		}
		return candidates[i].ConnectedAt.Before(candidates[j].ConnectedAt)
	})
	return *candidates[0], true
}

// List returns value snapshots of all entries in arbitrary order.
// The returned slice is independent of the internal map.
func (r *Registry) List() []RunnerEntry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	result := make([]RunnerEntry, 0, len(r.runners))
	for _, e := range r.runners {
		result = append(result, *e)
	}
	return result
}
