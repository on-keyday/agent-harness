package server

import (
	"sort"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// idleWatcher is one armed await-idle watcher. The server holds it so
// `session await-idle ls` can report it and `kill` can reach it. Nothing is
// persisted: a server restart drops every watcher, as it always has.
//
// Exactly one of fire, session stop, kill or connection teardown ends a
// watcher, and the one that does is whichever removes it from the registry
// first.
type idleWatcher struct {
	id        uint64
	taskIDHex string
	sink      protocol.AwaitIdleSink
	topic     string
	threshold time.Duration
	armedAt   time.Time
	// requester is the arming principal, resolved at arm time because the
	// board/notify sinks outlive the arming connection. Zero = operator.
	requester  protocol.TaskID
	clientCID  string
	clientKind protocol.ClientKind
	// stop ends the SessionMux goroutine early. Closed only by the path that
	// won the remove, so it is closed at most once.
	stop chan struct{}
	// onCancel is what a kill owes the requester: the reply sink's deferred
	// response with status=cancelled. nil for board and notify, which owe
	// nothing — not waking the recipient is the point of killing them.
	onCancel func()
}

// idleWatcherRegistry maps a server-assigned id to its watcher. Shaped like
// execRegistry, for the same reason: something the operator wants to see and
// stop that belongs to no task row.
type idleWatcherRegistry struct {
	mu   sync.Mutex
	next uint64
	m    map[uint64]*idleWatcher
}

func newIdleWatcherRegistry() *idleWatcherRegistry {
	return &idleWatcherRegistry{m: map[uint64]*idleWatcher{}}
}

// idleWatchers returns the registry, created on first use so struct-literal
// handlers in tests need not set it — the same shape as execs().
func (h *TaskHandler) idleWatchers() *idleWatcherRegistry {
	h.idleWatchersOnce.Do(func() {
		h.idleWatcherReg = newIdleWatcherRegistry()
	})
	return h.idleWatcherReg
}

// add assigns the next id (from 1, so 0 is never real) and stores w.
func (r *idleWatcherRegistry) add(w *idleWatcher) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	w.id = r.next
	if w.armedAt.IsZero() {
		w.armedAt = time.Now()
	}
	r.m[w.id] = w
	return w.id
}

func (r *idleWatcherRegistry) get(id uint64) (*idleWatcher, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.m[id]
	return w, ok
}

// remove drops the watcher and reports whether it was there. The caller that
// gets true is the one that ended it.
func (r *idleWatcherRegistry) remove(id uint64) (*idleWatcher, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.m[id]
	if ok {
		delete(r.m, id)
	}
	return w, ok
}

// list returns the watchers ascending by id; taskFilter "" means all.
func (r *idleWatcherRegistry) list(taskFilter string) []*idleWatcher {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*idleWatcher, 0, len(r.m))
	for _, w := range r.m {
		if taskFilter != "" && w.taskIDHex != taskFilter {
			continue
		}
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}
