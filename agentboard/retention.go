package agentboard

import (
	"errors"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

var (
	// ErrNotRetainable is a topic retention is refused on: an empty name or
	// any chat.* topic. Direct messages stay temporary.
	ErrNotRetainable = errors.New("agentboard: topic cannot be retained")
	// ErrNoStore is a retention request on a board with no store attached.
	ErrNoStore = errors.New("agentboard: no store attached")
)

// Retainable reports whether topic may be retained: not an empty name and not
// any chat.* topic. The one rule every surface asks, so a control the server
// would refuse is never offered.
func Retainable(topic string) bool {
	return topic != "" && !strings.HasPrefix(topic, SelfTopicPrefix)
}

// ExpiresAt is when a retained topic's period runs out, counted from its last
// publish or from the retain call, whichever is later. Zero for forever. The
// board evicts by it, and board topics reports it, so the two cannot differ.
func (r Retention) ExpiresAt(lastPublishedAt time.Time) time.Time {
	if r.Forever {
		return time.Time{}
	}
	base := lastPublishedAt
	if r.SetAt.After(base) {
		base = r.SetAt
	}
	return base.Add(r.TTL)
}

// AttachStore loads every retained topic from s and mirrors retained topics
// into it from now on. Call once, before the board serves anything. The
// publish seq is raised above every seq the logs hold, so a boot whose clock
// seed is behind cannot reuse one.
func (b *Board) AttachStore(s Store) error {
	stored, err := s.Load(b.cfg.RingN)
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.store = s
	var high uint64
	for _, st := range stored {
		t := newTopic(st.Name, b.cfg.RingN)
		t.ring = append(t.ring, st.Ring...)
		t.retracted = append(t.retracted, st.Retracted...)
		for tid, seq := range st.Shown {
			if seq == 0 {
				continue
			}
			if t.shown == nil {
				t.shown = make(map[protocol.TaskID]uint64)
			}
			t.shown[tid] = seq
		}
		t.lastPublishedAt = st.LastPublishedAt
		for _, tid := range st.Members {
			b.addMemberLocked(st.Name, tid)
		}
		r := st.Retention
		t.retention = &r
		b.topics[st.Name] = t
		if st.MaxSeq > high {
			high = st.MaxSeq
		}
	}
	if high > b.seq.Load() {
		b.seq.Store(high)
	}
	return nil
}

// Retain sets topic's retention, creating the topic empty when it does not
// exist. On a topic that was not retained yet, what it already holds is
// written to the store first. Calling it again replaces the setting.
func (b *Board) Retain(topic string, r Retention) error {
	if !Retainable(topic) {
		return ErrNotRetainable
	}
	b.mu.Lock()
	if b.store == nil {
		b.mu.Unlock()
		return ErrNoStore
	}
	t, ok := b.topics[topic]
	if !ok {
		if len(b.topics) >= b.cfg.MaxTopics {
			b.evictOldestTopicLocked()
			if len(b.topics) >= b.cfg.MaxTopics {
				b.mu.Unlock()
				return ErrTooManyTopics
			}
		}
		t = newTopic(topic, b.cfg.RingN)
		b.topics[topic] = t
	}
	wasRetained := t.retention != nil
	t.retention = &r
	store := b.store
	var members []protocol.TaskID
	for tid := range b.members[topic] {
		members = append(members, tid)
	}
	b.mu.Unlock()

	if err := store.SetRetention(topic, r); err != nil {
		return err
	}
	if wasRetained {
		return nil
	}
	for _, m := range t.snapshot() {
		if err := store.Append(topic, m); err != nil {
			return err
		}
	}
	for _, m := range t.snapshotRetracted() {
		if err := store.Append(topic, m); err != nil {
			return err
		}
		if err := store.Retract(topic, m); err != nil {
			return err
		}
	}
	// The marks too, or the first hook after a restart hands every
	// subscriber what it was already given before the retain.
	for tid, seq := range t.snapshotShown() {
		if err := store.SetShown(topic, tid, seq); err != nil {
			return err
		}
	}
	for _, tid := range members {
		if err := store.SetMember(topic, tid, true); err != nil {
			return err
		}
	}
	return nil
}

// ClearRetain returns topic to the board's TTL and deletes its disk copy. The
// in-memory ring stays and ages out like any topic's. Clearing a topic that
// is not retained is a no-op.
func (b *Board) ClearRetain(topic string) error {
	b.mu.Lock()
	t, ok := b.topics[topic]
	if !ok || t.retention == nil {
		b.mu.Unlock()
		return nil
	}
	t.retention = nil
	store := b.store
	// The board TTL counts from the last publish; a topic retained without one
	// starts its ordinary countdown now rather than being evicted at once.
	// Under t.mu, where topic.append writes it.
	t.mu.Lock()
	if t.lastPublishedAt.IsZero() {
		t.lastPublishedAt = time.Now()
	}
	t.mu.Unlock()
	b.mu.Unlock()
	return store.Drop(topic)
}

// RetentionOf reports topic's retention and the bytes its log uses on disk.
func (b *Board) RetentionOf(topic string) (r Retention, diskBytes int64, ok bool) {
	b.mu.Lock()
	t, found := b.topics[topic]
	if !found || t.retention == nil {
		b.mu.Unlock()
		return Retention{}, 0, false
	}
	r = *t.retention
	store := b.store
	b.mu.Unlock()
	return r, store.Bytes(topic), true
}

// Page returns up to limit messages of topic, live and withdrawn, with seq
// below beforeSeq (0 = from the newest), newest first. A retained topic is
// read from its log, which reaches past the ring; any other topic from memory.
func (b *Board) Page(topic string, beforeSeq uint64, limit int) ([]RetainedMessage, error) {
	b.mu.Lock()
	t, ok := b.topics[topic]
	retained := ok && t.retention != nil
	store := b.store
	b.mu.Unlock()
	if !ok {
		return nil, nil
	}
	if retained {
		return store.Page(topic, beforeSeq, limit)
	}
	all := append(t.snapshot(), t.snapshotRetracted()...)
	sort.Slice(all, func(i, j int) bool { return all[i].Seq > all[j].Seq })
	out := make([]RetainedMessage, 0, limit)
	for _, m := range all {
		if beforeSeq != 0 && m.Seq >= beforeSeq {
			continue
		}
		if len(out) == limit {
			break
		}
		out = append(out, m)
	}
	return out, nil
}

// PageLive is Page for an agent path: withdrawn messages are left out, and
// older rows are read until the page holds limit live ones or the history
// ends, so a run of withdrawals does not shorten it.
func (b *Board) PageLive(topic string, beforeSeq uint64, limit int) ([]RetainedMessage, error) {
	out := make([]RetainedMessage, 0, limit)
	for len(out) < limit {
		page, err := b.Page(topic, beforeSeq, limit)
		if err != nil {
			return nil, err
		}
		for _, m := range page {
			if m.RetractedAt.IsZero() && len(out) < limit {
				out = append(out, m)
			}
		}
		if len(page) < limit {
			break
		}
		beforeSeq = page[len(page)-1].Seq
	}
	return out, nil
}

// storeLog reports a store error. A failed mirror never fails the board
// operation that caused it: the message is on the board either way.
func storeLog(op, topic string, err error) {
	if err != nil {
		slog.Error("agentboard store", "op", op, "topic", topic, "err", err)
	}
}
