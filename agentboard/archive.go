package agentboard

import (
	"sort"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Archive freezes topic's history up to now into an archive and returns it.
// A retained topic is copied from its log, which reaches past the ring; any
// other from its ring and withdrawn list. The topic itself is not changed.
// See docs/superpowers/specs/2026-11-02-agentboard-flush-archive-design.md.
func (b *Board) Archive(topic string, by protocol.TaskID) (ArchiveInfo, error) {
	if !Retainable(topic) {
		return ArchiveInfo{}, ErrNotRetainable
	}
	b.mu.Lock()
	store := b.store
	t, ok := b.topics[topic]
	retained := ok && t.retention != nil
	b.mu.Unlock()
	switch {
	case store == nil:
		return ArchiveInfo{}, ErrNoStore
	case !ok:
		return ArchiveInfo{}, ErrNoTopic
	case retained:
		// Publishes append to the ring and the log under t.mu in seq order
		// (Board.Send); holding it across the copy means every seq up to the
		// newest in the log is in the log.
		t.mu.Lock()
		defer t.mu.Unlock()
		return store.ArchiveLog(topic, by, time.Now())
	}
	msgs := append(t.snapshot(), t.snapshotRetracted()...)
	sort.Slice(msgs, func(i, j int) bool { return msgs[i].Seq < msgs[j].Seq })
	return store.ArchiveMessages(topic, msgs, by, time.Now())
}

// Archives lists the archives of topic, or every archive when topic is empty,
// newest first.
func (b *Board) Archives(topic string) ([]ArchiveInfo, error) {
	store := b.storeOrNil()
	if store == nil {
		return nil, ErrNoStore
	}
	all, err := store.Archives()
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, a := range all {
		if topic == "" || a.Topic == topic {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// ArchiveOf reports one archive; ok is false for an unknown id.
func (b *Board) ArchiveOf(id string) (ArchiveInfo, bool, error) {
	store := b.storeOrNil()
	if store == nil {
		return ArchiveInfo{}, false, nil
	}
	return store.ArchiveOf(id)
}

// ArchivePage returns up to limit messages of an archive, live and withdrawn,
// with seq below beforeSeq (0 = from the newest), newest first.
func (b *Board) ArchivePage(id string, beforeSeq uint64, limit int) ([]RetainedMessage, error) {
	store := b.storeOrNil()
	if store == nil {
		return nil, ErrNoStore
	}
	return store.ArchivePage(id, beforeSeq, limit)
}

// ArchivePageLive is ArchivePage for an agent path: withdrawn messages are
// left out and older rows read until the page holds limit, as PageLive does.
func (b *Board) ArchivePageLive(id string, beforeSeq uint64, limit int) ([]RetainedMessage, error) {
	return pageLive(func(before uint64) ([]RetainedMessage, error) {
		return b.ArchivePage(id, before, limit)
	}, beforeSeq, limit)
}

// ArchivePurge removes one message from an archive, or the whole archive for
// seq 0. found reports whether the archive existed.
func (b *Board) ArchivePurge(id string, seq uint64) (removed, found bool, err error) {
	store := b.storeOrNil()
	if store == nil {
		return false, false, nil
	}
	if _, ok, err := store.ArchiveOf(id); err != nil || !ok {
		return false, false, err
	}
	removed, err = store.ArchiveRemove(id, seq)
	return removed, true, err
}

func (b *Board) storeOrNil() Store {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.store
}
