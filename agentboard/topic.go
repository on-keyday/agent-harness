package agentboard

import (
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// RetainedMessage is one entry in a topic ring buffer.
type RetainedMessage struct {
	Seq uint64
	// InReplyTo is the seq of the message this one answers, 0 when it is not a
	// reply. Validated by the server at publish time, so a non-zero value here
	// referred to a real message when it was accepted — the parent may since
	// have been evicted from its ring.
	InReplyTo    uint64
	Topic        string
	Payload      []byte
	FromRunner   protocol.RunnerID
	FromTask     protocol.TaskID
	FromHostname string
	// FromAgentProfile is the agent profile the sender was running under when
	// this message was published, frozen here because the ring outlives the
	// taskState that produced the entry: a task id resumed under a different
	// runtime must not retroactively re-label its old messages. Empty = the
	// server could not attribute a runtime (see agentboard.bgn
	// DeliveredMessage.from_agent_profile).
	FromAgentProfile string
	ReceivedAt       time.Time
	// RetractedAt is when the message's AUTHOR withdrew it, zero while it is
	// live. Set only on entries in topic.retracted — an entry in the live ring
	// always carries the zero value.
	RetractedAt time.Time
}

// topic holds a bounded ring of recent messages plus metadata used for TTL eviction.
//
// Withdrawn messages move OUT of ring and into retracted rather than being
// flagged in place. Two reasons, both load-bearing:
//
//   - Every agent-facing read reaches the ring (since / snapshot / summary,
//     hence Board.Inbox / Wait / Retained / ListRetained). Moving the entry
//     makes all of them stop returning it without a filter at any call site,
//     and a filter is what gets forgotten at the sixth site.
//   - The ring is a FIFO that drops its oldest entry on overflow. Leaving
//     withdrawn entries in it would let one task's send/retract loop push
//     OTHER senders' live messages out of the window — the operator's view
//     eroded at agent speed, which is the thing retraction is supposed not to
//     do. retracted therefore has its own capacity.
type topic struct {
	mu              sync.Mutex
	name            string
	cap             int
	ring            []RetainedMessage
	retracted       []RetainedMessage
	lastPublishedAt time.Time
}

func newTopic(name string, cap int) *topic {
	return &topic{name: name, cap: cap, ring: make([]RetainedMessage, 0, cap)}
}

func (t *topic) append(seq uint64, payload []byte, fromRid protocol.RunnerID, fromTid protocol.TaskID, fromHost, fromProfile string, inReplyTo uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()
	t.lastPublishedAt = now
	if len(t.ring) == t.cap {
		copy(t.ring, t.ring[1:])
		t.ring = t.ring[:t.cap-1]
	}
	t.ring = append(t.ring, RetainedMessage{
		Seq:              seq,
		InReplyTo:        inReplyTo,
		Topic:            t.name,
		Payload:          append([]byte(nil), payload...),
		FromRunner:       fromRid,
		FromTask:         fromTid,
		FromHostname:     fromHost,
		FromAgentProfile: fromProfile,
		ReceivedAt:       now,
	})
}

// removeSeq drops the single retained message with the given seq, preserving
// the order of the rest. Returns whether an entry was found and removed.
//
// It searches the withdrawn list as well as the live ring. Purge is the only
// way to make a payload's bytes stop existing (the escape hatch for something
// that must not survive at all), so a retract must not put a message beyond
// its reach.
func (t *topic) removeSeq(seq uint64) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := range t.ring {
		if t.ring[i].Seq == seq {
			t.ring = append(t.ring[:i], t.ring[i+1:]...)
			return true
		}
	}
	for i := range t.retracted {
		if t.retracted[i].Seq == seq {
			t.retracted = append(t.retracted[:i], t.retracted[i+1:]...)
			return true
		}
	}
	return false
}

// retract moves the live message with the given seq into the withdrawn list,
// but only when by published it. Returns whether the move happened; a seq that
// is absent, already withdrawn, or authored by somebody else all answer false,
// and the caller must not tell those cases apart on the wire (see
// RetractStatus.not_found in agentboard.bgn).
//
// now is passed in rather than read here so the server stamps one time for the
// whole operation.
func (t *topic) retract(seq uint64, by protocol.TaskID, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := range t.ring {
		if t.ring[i].Seq != seq {
			continue
		}
		if t.ring[i].FromTask.Id != by.Id {
			return false
		}
		m := t.ring[i]
		m.RetractedAt = now
		t.ring = append(t.ring[:i], t.ring[i+1:]...)
		if len(t.retracted) == t.cap {
			copy(t.retracted, t.retracted[1:])
			t.retracted = t.retracted[:t.cap-1]
		}
		t.retracted = append(t.retracted, m)
		return true
	}
	return false
}

// snapshotRetracted returns a copy of the withdrawn list in ascending seq
// order. Only operator-facing handlers call it; every agent-facing read goes
// through the live ring.
func (t *topic) snapshotRetracted() []RetainedMessage {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]RetainedMessage, len(t.retracted))
	copy(out, t.retracted)
	return out
}

// retractedCount is the number of withdrawn messages held for operator audit.
// Deliberately separate from summary()'s msgCount, which answers "how much
// would a subscriber receive".
func (t *topic) retractedCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.retracted)
}

// snapshot returns a copy of the ring (metadata + payload) in ascending seq
// order. Callers that only need metadata read Seq/From*/len(Payload)/ReceivedAt
// and never forward Payload onto the wire.
func (t *topic) snapshot() []RetainedMessage {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]RetainedMessage, len(t.ring))
	copy(out, t.ring)
	return out
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

// summary returns a snapshot of the topic's stats for ListTopics.
func (t *topic) summary() (lastSeq uint64, lastPublishedAt time.Time, msgCount int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.ring) > 0 {
		lastSeq = t.ring[len(t.ring)-1].Seq
	}
	return lastSeq, t.lastPublishedAt, len(t.ring)
}
