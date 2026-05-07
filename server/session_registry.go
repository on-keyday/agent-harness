package server

import "sync"

// SessionRegistry maps taskID -> *SessionMux for active detachable sessions.
// Safe for concurrent use.
type SessionRegistry struct {
	mu sync.RWMutex
	m  map[string]*SessionMux
}

func NewSessionRegistry() *SessionRegistry {
	return &SessionRegistry{m: map[string]*SessionMux{}}
}

func (r *SessionRegistry) Add(taskID string, mux *SessionMux) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[taskID] = mux
}

func (r *SessionRegistry) Get(taskID string) *SessionMux {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.m[taskID]
}

func (r *SessionRegistry) Remove(taskID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, taskID)
}

// Snapshot returns a shallow copy of the current map. Safe for the caller to
// iterate without holding the registry lock.
func (r *SessionRegistry) Snapshot() map[string]*SessionMux {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]*SessionMux, len(r.m))
	for k, v := range r.m {
		out[k] = v
	}
	return out
}
