package server

import (
	"sort"
	"sync"
	"sync/atomic"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// portForward is one active port-forward registration (either -L or -R). The
// server creates the control stream; for -R it also pushes tagged
// PortForwardEvent records onto it (conn_notify per accepted connection,
// closed on teardown) and allocates a per-connection client data stream
// against clientCxn each time the runner reports a new accepted connection.
type portForward struct {
	forwardID  uint64
	direction  protocol.PortForwardDirection
	taskIDHex  string
	runnerID   string // = TaskEntry.AssignedTo; used to re-find the runner at teardown
	control    trsf.BidirectionalStream
	clientCxn  ConnHandle
	clientCID  string
	clientKind protocol.ClientKind
	bindAddr   string
	bindPort   uint16
	targetHost string
	targetPort uint16
	// clientEndpoint records whether the client side of this forward is a real
	// socket or lives inside the client process. It changes nothing about the
	// byte path — it exists so the listing does not report a bind address that
	// was never bound.
	clientEndpoint protocol.ClientEndpointKind

	// Always-on traffic accounting. Atomics rather than the registry mutex:
	// these are written from the relay goroutines of every connection under
	// this forward, and a relay must never wait on a lock a listing holds.
	bytesToTarget   atomic.Uint64
	bytesFromTarget atomic.Uint64
	connsTotal      atomic.Uint64
	connsOpen       atomic.Int64
	lastActivityMs  atomic.Int64
	nextConnSeq     atomic.Uint64

	// Per-connection halves, for the conn_close record a tap receives. Its own
	// mutex, not the registry's: it is touched on every relayed chunk, and
	// taking the registry lock there would put every listing behind the
	// traffic.
	connMu sync.Mutex
	conns  map[uint64]connBytes

	// Taps reading this forward right now, and the count the listing reports so
	// a forward cannot be watched invisibly.
	tapMu sync.Mutex
	taps  []*forwardTap

	// What the stats sweep last published, so an unchanged forward costs a
	// comparison and no event.
	statsMu       sync.Mutex
	lastPublished publishedCounters
}

// portForwardRegistry maps server-assigned forwardId → registration. Safe for
// concurrent use. Lives on TaskHandler. It also tracks pending bind-result
// channels so a registration can block until the runner reports whether its
// listener bound.
type portForwardRegistry struct {
	mu      sync.Mutex
	next    uint64
	m       map[uint64]*portForward
	pending map[uint64]chan bool
}

func newPortForwardRegistry() *portForwardRegistry {
	return &portForwardRegistry{m: map[uint64]*portForward{}, pending: map[uint64]chan bool{}}
}

// addPending registers a buffered channel the registration waits on for the
// runner's bind result. Caller must removePending when done.
func (r *portForwardRegistry) addPending(id uint64) chan bool {
	ch := make(chan bool, 1)
	r.mu.Lock()
	r.pending[id] = ch
	r.mu.Unlock()
	return ch
}

func (r *portForwardRegistry) removePending(id uint64) {
	r.mu.Lock()
	delete(r.pending, id)
	r.mu.Unlock()
}

// signalBind delivers a runner bind result to the waiting registration, if any.
// Non-blocking: a missing or already-signalled entry is a no-op.
func (r *portForwardRegistry) signalBind(id uint64, ok bool) {
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

// pforwards returns the handler's port-forward registry, creating it exactly
// once. Safe for concurrent callers: sync.Once serializes the init and
// establishes the happens-before so the subsequent field read is race-free.
func (h *TaskHandler) pforwards() *portForwardRegistry {
	h.portForwardsOnce.Do(func() {
		h.portForwards = newPortForwardRegistry()
	})
	return h.portForwards
}

// add assigns the next forwardId, stores pf under it, and returns the id.
func (r *portForwardRegistry) add(pf *portForward) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	pf.forwardID = r.next
	r.m[r.next] = pf
	return r.next
}

// remoteForwardInfo is a debug-dump snapshot of one registration.
type remoteForwardInfo struct {
	forwardID uint64
	direction protocol.PortForwardDirection
	taskIDHex string
	runnerID  string
	clientCID string
}

// snapshot returns a copy of the active registrations for debug dumps.
func (r *portForwardRegistry) snapshot() []remoteForwardInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]remoteForwardInfo, 0, len(r.m))
	for _, pf := range r.m {
		cid := ""
		if pf.clientCxn != nil {
			cid = pf.clientCxn.ConnectionID().String()
		}
		out = append(out, remoteForwardInfo{forwardID: pf.forwardID, direction: pf.direction, taskIDHex: pf.taskIDHex, runnerID: pf.runnerID, clientCID: cid})
	}
	return out
}

// list returns the registrations ordered by forwardID (== creation order).
func (r *portForwardRegistry) list() []*portForward {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*portForward, 0, len(r.m))
	for _, pf := range r.m {
		out = append(out, pf)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].forwardID < out[j].forwardID })
	return out
}

func (r *portForwardRegistry) get(id uint64) (*portForward, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pf, ok := r.m[id]
	return pf, ok
}

func (r *portForwardRegistry) remove(id uint64) (*portForward, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	pf, ok := r.m[id]
	if ok {
		delete(r.m, id)
	}
	return pf, ok
}
