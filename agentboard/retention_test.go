package agentboard

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

var (
	retRid = protocol.RunnerID{Id: [16]byte{0x7e}}
	retTid = protocol.TaskID{Id: [16]byte{0x7a}}
)

func newRetainBoard(t *testing.T, cfg Config, dir string) (*Board, *FileStore) {
	t.Helper()
	if cfg.RingN == 0 {
		cfg.RingN = 64
	}
	if cfg.TopicTTL == 0 {
		cfg.TopicTTL = time.Hour
	}
	if cfg.MaxTopics == 0 {
		cfg.MaxTopics = 16
	}
	if cfg.MaxPayload == 0 {
		cfg.MaxPayload = 1024
	}
	b := New(cfg)
	s, err := OpenFileStore(dir, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.AttachStore(s); err != nil {
		t.Fatal(err)
	}
	return b, s
}

func publish(t *testing.T, b *Board, topic, body string) uint64 {
	t.Helper()
	seq, _, err := b.Send(topic, []byte(body), retRid, retTid, "h", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

func TestRetain_RefusesChat(t *testing.T) {
	b, s := newRetainBoard(t, Config{}, t.TempDir())
	defer b.Close()
	defer s.Close()
	for _, name := range []string{"chat.0123abcd", "chat.0123abcd.x", OperatorTopic, ""} {
		if err := b.Retain(name, Retention{Forever: true}); !errors.Is(err, ErrNotRetainable) {
			t.Errorf("Retain(%q) = %v, want ErrNotRetainable", name, err)
		}
	}
}

func TestRetain_OutlivesTTL(t *testing.T) {
	b, s := newRetainBoard(t, Config{TopicTTL: time.Millisecond}, t.TempDir())
	defer b.Close()
	defer s.Close()
	if err := b.Retain("kept", Retention{TTL: time.Hour, SetAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	publish(t, b, "kept", "x")
	publish(t, b, "plain", "x")
	time.Sleep(10 * time.Millisecond)
	b.evictExpiredTopics()
	if _, ok := b.ListRetained("kept"); !ok {
		t.Fatal("retained topic evicted by the board TTL")
	}
	if _, ok := b.ListRetained("plain"); ok {
		t.Fatal("plain topic survived the board TTL")
	}
}

func TestRetain_ExpiresFromRetainCall(t *testing.T) {
	dir := t.TempDir()
	b, s := newRetainBoard(t, Config{}, dir)
	defer b.Close()
	defer s.Close()
	if err := b.Retain("quiet", Retention{TTL: 50 * time.Millisecond, SetAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	b.evictExpiredTopics()
	if _, ok := b.ListRetained("quiet"); !ok {
		t.Fatal("unpublished retained topic expired before its period")
	}
	time.Sleep(90 * time.Millisecond)
	b.evictExpiredTopics()
	if _, _, ok := b.RetentionOf("quiet"); ok {
		t.Fatal("retention outlived its period")
	}
	if _, err := os.Stat(filepath.Join(dir, storeFileName("quiet"))); !os.IsNotExist(err) {
		t.Fatalf("expired topic's log still on disk: %v", err)
	}
	// Expiry only stops keeping it: the board TTL then applies, and a topic
	// never published to is past it at once.
	b.evictExpiredTopics()
	if _, ok := b.ListRetained("quiet"); ok {
		t.Fatal("the topic did not fall back to the board TTL")
	}
}

func TestRetain_MaxTopicsSkipsRetained(t *testing.T) {
	b, s := newRetainBoard(t, Config{MaxTopics: 2}, t.TempDir())
	defer b.Close()
	defer s.Close()
	for _, n := range []string{"r1", "r2"} {
		if err := b.Retain(n, Retention{Forever: true}); err != nil {
			t.Fatal(err)
		}
		publish(t, b, n, "x")
	}
	if _, _, err := b.Send("third", []byte("x"), retRid, retTid, "h", "", 0); !errors.Is(err, ErrTooManyTopics) {
		t.Fatalf("third topic Send = %v, want ErrTooManyTopics", err)
	}
	for _, n := range []string{"r1", "r2"} {
		if _, ok := b.ListRetained(n); !ok {
			t.Fatalf("%s evicted under MaxTopics", n)
		}
	}
}

func TestRetain_RestartRestores(t *testing.T) {
	dir := t.TempDir()
	b, s := newRetainBoard(t, Config{}, dir)
	conn := b.Attach(retRid, retTid, "h", "")
	if err := b.Subscribe(conn, "keep"); err != nil {
		t.Fatal(err)
	}
	if err := b.Retain("keep", Retention{Forever: true}); err != nil {
		t.Fatal(err)
	}
	var last uint64
	for i := 0; i < 100; i++ {
		last = publish(t, b, "keep", "m")
	}
	if got := b.InboxAdvance(conn); len(got) != 64 {
		t.Fatalf("first advance = %d rows, want the 64 in the ring", len(got))
	}
	b.Close()
	_ = s.Close()

	b2, s2 := newRetainBoard(t, Config{}, dir)
	defer b2.Close()
	defer s2.Close()
	msgs, ok := b2.ListRetained("keep")
	if !ok || len(msgs) != 64 || msgs[63].Seq != last {
		t.Fatalf("restored ring = %d rows (ok %v), want 64 ending at %d", len(msgs), ok, last)
	}
	conn2 := b2.Attach(retRid, retTid, "h", "")
	if err := b2.Subscribe(conn2, "keep"); err != nil {
		t.Fatal(err)
	}
	if got := b2.InboxAdvance(conn2); len(got) != 0 {
		t.Fatalf("advance after restart re-delivered %d rows", len(got))
	}
	page, err := b2.Page("keep", 0, 200)
	if err != nil || len(page) != 100 {
		t.Fatalf("Page = %d rows, %v; want 100", len(page), err)
	}
	if r, _, ok := b2.RetentionOf("keep"); !ok || !r.Forever {
		t.Fatalf("RetentionOf after restart = %+v, %v", r, ok)
	}
}

func TestRetain_SeqAboveDisk(t *testing.T) {
	dir := t.TempDir()
	b, s := newRetainBoard(t, Config{SeqSeed: 1 << 62}, dir)
	if err := b.Retain("s", Retention{Forever: true}); err != nil {
		t.Fatal(err)
	}
	high := publish(t, b, "s", "x")
	b.Close()
	_ = s.Close()

	b2, s2 := newRetainBoard(t, Config{SeqSeed: 5}, dir)
	defer b2.Close()
	defer s2.Close()
	if next := publish(t, b2, "s", "y"); next <= high {
		t.Fatalf("seq after a restart with a clock seed behind = %d, want > %d", next, high)
	}
}

func TestRetain_PurgeDiskOnlySeq(t *testing.T) {
	dir := t.TempDir()
	b, s := newRetainBoard(t, Config{}, dir)
	defer b.Close()
	defer s.Close()
	_ = b.Retain("p", Retention{Forever: true})
	var third uint64
	for i := 0; i < 100; i++ {
		body := "m"
		if i == 2 {
			body = "SECRET-DISK-ONLY"
		}
		seq := publish(t, b, "p", body)
		if i == 2 {
			third = seq
		}
	}
	removed, found := b.PurgeSeq("p", third)
	if !removed || !found {
		t.Fatalf("PurgeSeq of a disk-only seq = %v, %v", removed, found)
	}
	_ = s.Close() // flush, so the file is what a reader would find
	raw, _ := os.ReadFile(filepath.Join(dir, storeFileName("p")))
	if bytes.Contains(raw, []byte("SECRET-DISK-ONLY")) || bytes.Contains(raw, []byte("U0VDUkVULURJU0stT05MWQ")) {
		t.Fatal("purged payload still on disk")
	}
}

func TestRetain_PurgeTopicKeepsRetention(t *testing.T) {
	b, s := newRetainBoard(t, Config{}, t.TempDir())
	defer b.Close()
	defer s.Close()
	_ = b.Retain("pt", Retention{Forever: true})
	publish(t, b, "pt", "old")
	if _, found := b.PurgeTopic("pt"); !found {
		t.Fatal("PurgeTopic did not find the topic")
	}
	if _, _, ok := b.RetentionOf("pt"); !ok {
		t.Fatal("PurgeTopic dropped the retention")
	}
	publish(t, b, "pt", "new")
	page, _ := b.Page("pt", 0, 10)
	if len(page) != 1 || string(page[0].Payload) != "new" {
		t.Fatalf("page after purge = %+v, want only the new message", page)
	}
}

func TestRetain_ClearDeletesFile(t *testing.T) {
	dir := t.TempDir()
	b, s := newRetainBoard(t, Config{}, dir)
	defer b.Close()
	defer s.Close()
	_ = b.Retain("c", Retention{Forever: true})
	publish(t, b, "c", "x")
	if err := b.ClearRetain("c"); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := b.RetentionOf("c"); ok {
		t.Fatal("retention still set after ClearRetain")
	}
	if _, err := os.Stat(filepath.Join(dir, storeFileName("c"))); !os.IsNotExist(err) {
		t.Fatalf("cleared topic's log still on disk: %v", err)
	}
	if _, ok := b.ListRetained("c"); !ok {
		t.Fatal("ClearRetain removed the in-memory ring; it should age out normally")
	}
}

// Retaining an existing topic carries what it already holds onto disk.
func TestRetain_ExistingRingPersisted(t *testing.T) {
	dir := t.TempDir()
	b, s := newRetainBoard(t, Config{}, dir)
	publish(t, b, "late", "before-retain")
	_ = b.Retain("late", Retention{Forever: true})
	b.Close()
	_ = s.Close()
	b2, s2 := newRetainBoard(t, Config{}, dir)
	defer b2.Close()
	defer s2.Close()
	msgs, _ := b2.ListRetained("late")
	if len(msgs) != 1 || string(msgs[0].Payload) != "before-retain" {
		t.Fatalf("restored = %+v, want the message published before Retain", msgs)
	}
}
