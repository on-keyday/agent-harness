package agentboard

import (
	"errors"
	"sort"
)

var (
	// ErrNoTopic is an operation on a topic the board does not hold.
	ErrNoTopic = errors.New("agentboard: no such topic")
	// ErrNotRetained is a flush of a topic without a retention: it has no
	// disk copy, so emptying its ring would destroy its messages.
	ErrNotRetained = errors.New("agentboard: topic is not retained")
)

// Flush empties a retained topic's ring and withdrawn list. Nothing leaves the
// disk: the store syncs the log, then records the seq the ring was emptied
// through, so a restart does not load those messages back. A later publish
// fills the ring as usual. through is that seq — the current floor when the
// ring already held nothing — and dropped how many entries left memory.
func (b *Board) Flush(topic string) (through uint64, dropped int, err error) {
	b.mu.Lock()
	t, ok := b.topics[topic]
	if !ok {
		b.mu.Unlock()
		return 0, 0, ErrNoTopic
	}
	if t.retention == nil {
		b.mu.Unlock()
		return 0, 0, ErrNotRetained
	}
	store, floor := b.store, t.ringFloor
	b.mu.Unlock()

	through = t.newestSeq()
	if through == 0 {
		// Nothing to empty. Writing a floor of 0 would undo an earlier flush
		// on the next restart.
		return floor, 0, nil
	}
	// The log first: the ring is emptied only once what it held is on disk.
	if err := store.FlushRing(topic, through); err != nil {
		return 0, 0, err
	}
	dropped = t.dropThrough(through)
	b.mu.Lock()
	if through > t.ringFloor {
		t.ringFloor = through
	}
	b.mu.Unlock()
	return through, dropped, nil
}

// RingFloor is the seq topic's ring was last flushed through, 0 when it was
// never flushed (or its retention has ended since).
func (b *Board) RingFloor(topic string) uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if t, ok := b.topics[topic]; ok {
		return t.ringFloor
	}
	return 0
}

// refillFromLog puts back into t's ring what a flush took out of it: the
// newest RingN live and withdrawn messages of the log. Called when a flushed
// topic's retention ends, before its log is deleted — a retention ending never
// deletes messages itself (retention spec §3.2), and after a flush the log is
// the only copy.
func (b *Board) refillFromLog(name string, t *topic, store Store) error {
	live, withdrawn, err := store.Tail(name, b.cfg.RingN)
	if err != nil {
		return err
	}
	t.mu.Lock()
	t.ring = mergeNewest(t.ring, live, t.cap)
	t.retracted = mergeNewest(t.retracted, withdrawn, t.cap)
	t.mu.Unlock()
	return nil
}

// mergeNewest merges two ascending lists by seq, one entry per seq, and keeps
// the newest n.
func mergeNewest(have, add []RetainedMessage, n int) []RetainedMessage {
	bySeq := make(map[uint64]RetainedMessage, len(have)+len(add))
	for _, m := range add {
		bySeq[m.Seq] = m
	}
	for _, m := range have {
		bySeq[m.Seq] = m
	}
	out := make([]RetainedMessage, 0, len(bySeq))
	for _, m := range bySeq {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	if len(out) > n {
		out = out[len(out)-n:]
	}
	return out
}
