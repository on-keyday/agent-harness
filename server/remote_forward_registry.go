package server

import (
	"sync"

	"github.com/on-keyday/objtrsf/trsf"
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
// concurrent use. Lives on TaskHandler. It also tracks pending bind-result
// channels so a registration can block until the runner reports whether its
// listener bound.
type remoteForwardRegistry struct {
	mu      sync.Mutex
	next    uint64
	m       map[uint64]*remoteForward
	pending map[uint64]chan bool
}

func newRemoteForwardRegistry() *remoteForwardRegistry {
	return &remoteForwardRegistry{m: map[uint64]*remoteForward{}, pending: map[uint64]chan bool{}}
}

// addPending registers a buffered channel the registration waits on for the
// runner's bind result. Caller must removePending when done.
func (r *remoteForwardRegistry) addPending(id uint64) chan bool {
	ch := make(chan bool, 1)
	r.mu.Lock()
	r.pending[id] = ch
	r.mu.Unlock()
	return ch
}

func (r *remoteForwardRegistry) removePending(id uint64) {
	r.mu.Lock()
	delete(r.pending, id)
	r.mu.Unlock()
}

// signalBind delivers a runner bind result to the waiting registration, if any.
// Non-blocking: a missing or already-signalled entry is a no-op.
func (r *remoteForwardRegistry) signalBind(id uint64, ok bool) {
	r.mu.Lock()
	ch := r.pending[id]
	r.mu.Unlock()
	if ch != nil {
		select {
		case ch <- ok:
		default:
		}
	}
}

// rforwards returns the handler's remote-forward registry, creating it exactly
// once. Safe for concurrent callers: sync.Once serializes the init and
// establishes the happens-before so the subsequent field read is race-free.
func (h *TaskHandler) rforwards() *remoteForwardRegistry {
	h.remoteForwardsOnce.Do(func() {
		h.remoteForwards = newRemoteForwardRegistry()
	})
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

// remoteForwardInfo is a debug-dump snapshot of one registration.
type remoteForwardInfo struct {
	forwardID uint64
	taskIDHex string
	runnerID  string
	clientCID string
}

// snapshot returns a copy of the active registrations for debug dumps.
func (r *remoteForwardRegistry) snapshot() []remoteForwardInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]remoteForwardInfo, 0, len(r.m))
	for _, rf := range r.m {
		cid := ""
		if rf.clientCxn != nil {
			cid = rf.clientCxn.ConnectionID().String()
		}
		out = append(out, remoteForwardInfo{forwardID: rf.forwardID, taskIDHex: rf.taskIDHex, runnerID: rf.runnerID, clientCID: cid})
	}
	return out
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
