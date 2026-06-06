package server

import (
	"sync"

	"github.com/on-keyday/agent-harness/trsf"
)

// remoteForward is one active ssh -R registration. The server creates the
// control stream and pushes RemoteForwardConnNotify records onto it; it
// allocates a per-connection client data stream against clientCxn each time the
// runner reports a new accepted connection.
type remoteForward struct {
	forwardID uint64
	taskIDHex string
	runnerID  string // = TaskEntry.AssignedTo; used to re-find the runner at teardown
	control   trsf.BidirectionalStream
	clientCxn ConnHandle
}

// remoteForwardRegistry maps server-assigned forwardId → registration. Safe for
// concurrent use. Lives on TaskHandler.
type remoteForwardRegistry struct {
	mu   sync.Mutex
	next uint64
	m    map[uint64]*remoteForward
}

func newRemoteForwardRegistry() *remoteForwardRegistry {
	return &remoteForwardRegistry{m: map[uint64]*remoteForward{}}
}

// rforwards returns the handler's remote-forward registry, lazily creating it.
// Not safe to call concurrently with itself on first use; in practice the first
// call happens on a single request goroutine before any teardown goroutine runs.
func (h *TaskHandler) rforwards() *remoteForwardRegistry {
	if h.remoteForwards == nil {
		h.remoteForwards = newRemoteForwardRegistry()
	}
	return h.remoteForwards
}

// add assigns the next forwardId, stores rf under it, and returns the id.
func (r *remoteForwardRegistry) add(rf *remoteForward) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	rf.forwardID = r.next
	r.m[r.next] = rf
	return r.next
}

func (r *remoteForwardRegistry) get(id uint64) (*remoteForward, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rf, ok := r.m[id]
	return rf, ok
}

func (r *remoteForwardRegistry) remove(id uint64) (*remoteForward, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rf, ok := r.m[id]
	if ok {
		delete(r.m, id)
	}
	return rf, ok
}
