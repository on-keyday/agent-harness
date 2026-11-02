package agentboard

import (
	"errors"
	"os"
	"testing"
	"time"
)

// retainedWith100 retains topic and publishes 100 messages, withdrawing the
// 50th. It returns the seqs in publish order.
func retainedWith100(t *testing.T, b *Board, topic string, r Retention) []uint64 {
	t.Helper()
	if err := b.Retain(topic, r); err != nil {
		t.Fatal(err)
	}
	seqs := make([]uint64, 0, 100)
	for i := 0; i < 100; i++ {
		seqs = append(seqs, publish(t, b, topic, "m"))
	}
	if _, ok := b.RetireSeq(seqs[99], retTid); !ok {
		t.Fatal("retract failed")
	}
	return seqs
}

func TestFlush_EmptiesRingKeepsLog(t *testing.T) {
	b, s := newRetainBoard(t, Config{}, t.TempDir())
	defer b.Close()
	defer s.Close()
	seqs := retainedWith100(t, b, "g", Retention{Forever: true})
	through, dropped, err := b.Flush("g")
	if err != nil {
		t.Fatal(err)
	}
	if through != seqs[99] || dropped != 64 {
		t.Errorf("through=%d dropped=%d, want %d/64 (63 live + 1 withdrawn)", through, dropped, seqs[99])
	}
	if msgs, _ := b.ListRetained("g"); len(msgs) != 0 {
		t.Errorf("ring holds %d", len(msgs))
	}
	if msgs, _ := b.ListRetracted("g"); len(msgs) != 0 {
		t.Errorf("withdrawn list holds %d", len(msgs))
	}
	if page, err := b.Page("g", 0, 200); err != nil || len(page) != 100 {
		t.Errorf("Page = %d, %v", len(page), err)
	}
	if page, err := b.PageLive("g", 0, 200); err != nil || len(page) != 99 {
		t.Errorf("PageLive = %d, %v", len(page), err)
	}
	if got := b.RingFloor("g"); got != through {
		t.Errorf("RingFloor = %d, want %d", got, through)
	}
}

func TestFlush_EmptyRingKeepsFloor(t *testing.T) {
	b, s := newRetainBoard(t, Config{}, t.TempDir())
	defer b.Close()
	defer s.Close()
	retainedWith100(t, b, "g", Retention{Forever: true})
	first, _, err := b.Flush("g")
	if err != nil {
		t.Fatal(err)
	}
	through, dropped, err := b.Flush("g")
	if err != nil || through != first || dropped != 0 {
		t.Errorf("second flush = %d %d %v, want %d 0 nil", through, dropped, err, first)
	}
}

func TestFlush_NextPublishDelivered(t *testing.T) {
	b, s := newRetainBoard(t, Config{}, t.TempDir())
	defer b.Close()
	defer s.Close()
	conn := b.Attach(retRid, retTid, "h", "")
	if err := b.Subscribe(conn, "g"); err != nil {
		t.Fatal(err)
	}
	retainedWith100(t, b, "g", Retention{Forever: true})
	if _, _, err := b.Flush("g"); err != nil {
		t.Fatal(err)
	}
	seq := publish(t, b, "g", "after")
	got := b.InboxAdvance(conn)
	if len(got) != 1 || got[0].Seq != seq {
		t.Errorf("advance = %v, want [%d]", seqsOf(got), seq)
	}
}

func TestFlush_Refusals(t *testing.T) {
	b, s := newRetainBoard(t, Config{}, t.TempDir())
	defer b.Close()
	defer s.Close()
	publish(t, b, "plain", "m")
	if _, _, err := b.Flush("plain"); !errors.Is(err, ErrNotRetained) {
		t.Errorf("unretained: %v", err)
	}
	if _, _, err := b.Flush("missing"); !errors.Is(err, ErrNoTopic) {
		t.Errorf("missing: %v", err)
	}
}

func TestFlush_SurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	b, s := newRetainBoard(t, Config{}, dir)
	retainedWith100(t, b, "g", Retention{Forever: true})
	through, _, err := b.Flush("g")
	if err != nil {
		t.Fatal(err)
	}
	after := publish(t, b, "g", "after")
	b.Close()
	_ = s.Close()

	b2, s2 := newRetainBoard(t, Config{}, dir)
	defer b2.Close()
	defer s2.Close()
	if msgs, _ := b2.ListRetained("g"); len(msgs) != 1 || msgs[0].Seq != after {
		t.Errorf("ring after restart = %v, want [%d]", seqsOf(msgs), after)
	}
	if msgs, _ := b2.ListRetracted("g"); len(msgs) != 0 {
		t.Errorf("withdrawn after restart = %v", seqsOf(msgs))
	}
	if got := b2.RingFloor("g"); got != through {
		t.Errorf("RingFloor after restart = %d, want %d", got, through)
	}
}

func assertRefilled(t *testing.T, b *Board, dir, topic string, seqs []uint64) {
	t.Helper()
	msgs, ok := b.ListRetained(topic)
	if !ok || len(msgs) != 64 || msgs[63].Seq != seqs[98] || msgs[0].Seq != seqs[35] {
		t.Errorf("ring after retention end = %d rows (ok %v), want the 64 live ending at %d", len(msgs), ok, seqs[98])
	}
	if w, _ := b.ListRetracted(topic); len(w) != 1 || w[0].Seq != seqs[99] {
		t.Errorf("withdrawn after retention end = %v", seqsOf(w))
	}
	if b.RingFloor(topic) != 0 {
		t.Errorf("RingFloor = %d after retention end", b.RingFloor(topic))
	}
	if _, err := os.Stat(logFileOf(t, dir, topic)); !os.IsNotExist(err) {
		t.Errorf("log still on disk: %v", err)
	}
}

func TestFlush_ClearRefillsRing(t *testing.T) {
	dir := t.TempDir()
	b, s := newRetainBoard(t, Config{}, dir)
	defer b.Close()
	defer s.Close()
	seqs := retainedWith100(t, b, "g", Retention{Forever: true})
	if _, _, err := b.Flush("g"); err != nil {
		t.Fatal(err)
	}
	if err := b.ClearRetain("g"); err != nil {
		t.Fatal(err)
	}
	assertRefilled(t, b, dir, "g", seqs)
}

func TestFlush_ExpiryRefillsRing(t *testing.T) {
	dir := t.TempDir()
	b, s := newRetainBoard(t, Config{}, dir)
	defer b.Close()
	defer s.Close()
	seqs := retainedWith100(t, b, "g", Retention{TTL: 20 * time.Millisecond, SetAt: time.Now()})
	if _, _, err := b.Flush("g"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)
	b.evictExpiredTopics()
	if _, _, ok := b.RetentionOf("g"); ok {
		t.Fatal("still retained after its period")
	}
	assertRefilled(t, b, dir, "g", seqs)
}

func TestFlush_RestartThenClear(t *testing.T) {
	dir := t.TempDir()
	b, s := newRetainBoard(t, Config{}, dir)
	seqs := retainedWith100(t, b, "g", Retention{Forever: true})
	if _, _, err := b.Flush("g"); err != nil {
		t.Fatal(err)
	}
	b.Close()
	_ = s.Close()
	b2, s2 := newRetainBoard(t, Config{}, dir)
	defer b2.Close()
	defer s2.Close()
	if err := b2.ClearRetain("g"); err != nil {
		t.Fatal(err)
	}
	assertRefilled(t, b2, dir, "g", seqs)
}

// An emptied ring is not an unpublished topic: the listing's last seq stays
// the newest message the topic has had, which the flush moved to disk.
func TestFlush_ListTopicsKeepsLastSeq(t *testing.T) {
	b, s := newRetainBoard(t, Config{}, t.TempDir())
	defer b.Close()
	defer s.Close()
	seqs := retainedWith100(t, b, "g", Retention{Forever: true})
	if _, _, err := b.Flush("g"); err != nil {
		t.Fatal(err)
	}
	for _, r := range b.ListTopics() {
		if r.Name == "g" && (r.LastSeq != seqs[99] || r.LastPublishedAt.IsZero()) {
			t.Errorf("row after flush = %+v, want LastSeq %d", r, seqs[99])
		}
	}
}
