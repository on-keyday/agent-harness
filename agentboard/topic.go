package agentboard

import (
	"sync"
	"time"
)

// RetainedMessage is one entry in a topic ring buffer.
type RetainedMessage struct {
	Seq     uint64
	Topic   string
	Payload []byte
}

// topic holds a bounded ring of recent messages plus metadata used for TTL eviction.
type topic struct {
	mu              sync.Mutex
	name            string
	cap             int
	ring            []RetainedMessage
	lastPublishedAt time.Time
}

func newTopic(name string, cap int) *topic {
	return &topic{name: name, cap: cap, ring: make([]RetainedMessage, 0, cap)}
}

func (t *topic) append(seq uint64, payload []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastPublishedAt = time.Now()
	if len(t.ring) == t.cap {
		copy(t.ring, t.ring[1:])
		t.ring = t.ring[:t.cap-1]
	}
	t.ring = append(t.ring, RetainedMessage{
		Seq:     seq,
		Topic:   t.name,
		Payload: append([]byte(nil), payload...),
	})
}

// since returns retained messages with Seq > sinceSeq, in ascending order.
func (t *topic) since(sinceSeq uint64) []RetainedMessage {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]RetainedMessage, 0, len(t.ring))
	for _, m := range t.ring {
		if m.Seq > sinceSeq {
			out = append(out, m)
		}
	}
	return out
}
