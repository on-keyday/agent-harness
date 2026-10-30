package agentboard

// Regression tests for the mid-plan review of the retention store (F1–F6).

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// F1: a restart must not count a retained topic's period from an older live
// message when a newer one was withdrawn since.
func TestRetainReview_RestartKeepsLastPublishOfWithdrawn(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir, time.Hour)
	now := time.Now()
	_ = s.SetRetention("t", Retention{TTL: time.Hour, SetAt: now.Add(-3 * time.Hour)})
	old := storeMsg(1, "old")
	old.ReceivedAt = now.Add(-2 * time.Hour)
	fresh := storeMsg(2, "fresh")
	fresh.ReceivedAt = now.Add(-time.Minute)
	_ = s.Append("t", old)
	_ = s.Append("t", fresh)
	fresh.RetractedAt = now
	_ = s.Retract("t", fresh)
	_ = s.Close()

	b, s2 := newRetainBoard(t, Config{}, dir)
	defer b.Close()
	defer s2.Close()
	b.evictExpiredTopics()
	if _, ok := b.ListRetained("t"); !ok {
		t.Fatal("topic expired on restart: its last publish (now withdrawn) was ignored")
	}
}

// F1: a whole-topic purge keeps the retention, and that must survive a
// restart — the emptied topic's period counts from the purge.
func TestRetainReview_PurgedTopicSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	b, s := newRetainBoard(t, Config{}, dir)
	_ = b.Retain("pt", Retention{TTL: time.Hour, SetAt: time.Now().Add(-3 * time.Hour)})
	publish(t, b, "pt", "x")
	b.PurgeTopic("pt")
	b.Close()
	_ = s.Close()

	b2, s2 := newRetainBoard(t, Config{}, dir)
	defer b2.Close()
	defer s2.Close()
	b2.evictExpiredTopics()
	if _, _, ok := b2.RetentionOf("pt"); !ok {
		t.Fatal("purged retained topic expired on restart")
	}
}

// F2: purging the newest message must not lower the seq floor a restart
// seeds from, or a clock-behind boot reuses the seq under a delivery mark.
func TestRetainReview_SeqFloorSurvivesPurge(t *testing.T) {
	dir := t.TempDir()
	b, s := newRetainBoard(t, Config{}, dir)
	conn := b.Attach(retRid, retTid, "h", "")
	_ = b.Subscribe(conn, "t")
	_ = b.Retain("t", Retention{Forever: true})
	var last uint64
	for i := 0; i < 3; i++ {
		last = publish(t, b, "t", "m")
	}
	b.InboxAdvance(conn)
	b.PurgeSeq("t", last)
	b.Close()
	_ = s.Close()

	b2, s2 := newRetainBoard(t, Config{}, dir)
	defer b2.Close()
	defer s2.Close()
	conn2 := b2.Attach(retRid, retTid, "h", "")
	_ = b2.Subscribe(conn2, "t")
	if next := publish(t, b2, "t", "new"); next <= last {
		t.Fatalf("seq after purge+restart = %d, want > %d", next, last)
	}
	if got := b2.InboxAdvance(conn2); len(got) != 1 {
		t.Fatalf("advance = %d rows, want the new message", len(got))
	}
}

// F3: a torn tail is cut on reopen, so the next record is readable and the
// torn bytes are gone.
func TestRetainReview_AppendAfterTornTail(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir, time.Hour)
	_ = s.SetRetention("torn", Retention{Forever: true})
	for seq := uint64(1); seq <= 3; seq++ {
		_ = s.Append("torn", storeMsg(seq, "m"))
	}
	_ = s.Close()
	path := logFileOf(t, dir, "torn")
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(`{"t":"msg","seq":9,"payload":"VE9STi1TRUNSRVQ`)
	_ = f.Close()

	s2 := openTestStore(t, dir, time.Hour)
	if err := s2.Append("torn", storeMsg(4, "m")); err != nil {
		t.Fatal(err)
	}
	_ = s2.Close()
	raw, _ := os.ReadFile(path)
	if bytes.Contains(raw, []byte("VE9STi1TRUNSRVQ")) {
		t.Fatal("torn record's bytes are still in the log")
	}
	s3 := openTestStore(t, dir, time.Hour)
	defer s3.Close()
	topics, _ := s3.Load(64)
	if len(topics) != 1 || len(topics[0].Ring) != 4 {
		t.Fatalf("loaded %+v, want 4 messages", topics)
	}
}

// F4: writes for a topic that is no longer retained create no log, and an
// orphan log left behind is removed on load.
func TestRetainReview_NoOrphanLogs(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir, time.Hour)
	_ = s.SetRetention("gone", Retention{Forever: true})
	_ = s.Drop("gone")
	_ = s.Append("gone", storeMsg(1, "late"))
	_ = s.Retract("gone", storeMsg(1, "late"))
	_ = s.SetShown("gone", fsTask, 1)
	_ = s.Close()
	if _, err := os.Stat(logFileOf(t, dir, "gone")); !os.IsNotExist(err) {
		t.Fatalf("a write after Drop recreated the log: %v", err)
	}

	orphan := filepath.Join(dir, storeFileName("orphan"))
	_ = os.WriteFile(orphan, append(headerLine("orphan"), []byte(`{"t":"msg","seq":5,"payload":"eA=="}`+"\n")...), 0o644)
	s2 := openTestStore(t, dir, time.Hour)
	defer s2.Close()
	if _, err := s2.Load(64); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatalf("orphan log with no retention survived Load: %v", err)
	}
}

// F5: an Append that arrives after the Remove of its seq — a purge racing the
// publish's mirror — must not put the purged message on disk.
func TestRetainReview_AppendAfterRemoveIsDropped(t *testing.T) {
	s := openTestStore(t, t.TempDir(), time.Hour)
	defer s.Close()
	_ = s.SetRetention("r", Retention{Forever: true})
	_, _ = s.Remove("r", 5)
	_ = s.Append("r", storeMsg(5, "purged-before-mirror"))
	page, _ := s.Page("r", 0, 10)
	if len(page) != 0 {
		t.Fatalf("page = %+v, want the purged seq absent", page)
	}
}

// F6: retaining a topic persists the delivery marks it already holds.
func TestRetainReview_RetainPersistsExistingMarks(t *testing.T) {
	dir := t.TempDir()
	b, s := newRetainBoard(t, Config{}, dir)
	conn := b.Attach(retRid, retTid, "h", "")
	_ = b.Subscribe(conn, "late")
	for i := 0; i < 20; i++ {
		publish(t, b, "late", "m")
	}
	b.InboxAdvance(conn)
	_ = b.Retain("late", Retention{Forever: true})
	b.Close()
	_ = s.Close()

	b2, s2 := newRetainBoard(t, Config{}, dir)
	defer b2.Close()
	defer s2.Close()
	conn2 := b2.Attach(retRid, retTid, "h", "")
	_ = b2.Subscribe(conn2, "late")
	if got := b2.InboxAdvance(conn2); len(got) != 0 {
		t.Fatalf("restart re-handed %d messages delivered before Retain", len(got))
	}
}

var _ = protocol.TaskID{}
