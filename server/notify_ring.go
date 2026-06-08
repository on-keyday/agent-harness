package server

import (
	"sync"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// notifyRing is a fixed-capacity in-memory ring of recent NotifyEvents.
// Lost on restart — no disk persistence (spec §8a). Consumed by Plan B's
// replay-on-subscribe.
type notifyRing struct {
	mu  sync.Mutex
	buf []protocol.NotifyEvent
	cap int
}

func newNotifyRing(capacity int) *notifyRing {
	if capacity < 1 {
		capacity = 1
	}
	return &notifyRing{cap: capacity}
}

func (r *notifyRing) append(ev protocol.NotifyEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, ev)
	if len(r.buf) > r.cap {
		r.buf = r.buf[len(r.buf)-r.cap:]
	}
}

// snapshot returns a copy of the current ring contents, oldest first.
func (r *notifyRing) snapshot() []protocol.NotifyEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]protocol.NotifyEvent, len(r.buf))
	copy(out, r.buf)
	return out
}
