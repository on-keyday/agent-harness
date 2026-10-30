package agentboard

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

var fsTask = protocol.TaskID{Id: [16]byte{0xfa}}

func openTestStore(t *testing.T, dir string, flush time.Duration) *FileStore {
	t.Helper()
	s, err := OpenFileStore(dir, flush)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	return s
}

func storeMsg(seq uint64, body string) RetainedMessage {
	return RetainedMessage{
		Seq: seq, Payload: []byte(body), FromTask: fsTask,
		FromRunner: protocol.RunnerID{Id: [16]byte{0x0b}}, FromHostname: "h", FromAgentProfile: "claude",
		SenderKind: protocol.SenderKind_Operator, ReplyToTopic: "chat.operator",
		ReceivedAt: time.UnixMilli(int64(1_700_000_000_000 + seq)), NoContact: true, ExcludeSelf: true,
	}
}

func logFileOf(t *testing.T, dir, topic string) string {
	t.Helper()
	return filepath.Join(dir, storeFileName(topic))
}

func TestFileStore_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir, time.Hour)
	ret := Retention{TTL: 2 * time.Hour, SetBy: fsTask, SetAt: time.UnixMilli(1_700_000_000_000)}
	if err := s.SetRetention("tree.x.d", ret); err != nil {
		t.Fatal(err)
	}
	for seq := uint64(1); seq <= 100; seq++ {
		if err := s.Append("tree.x.d", storeMsg(seq, "m")); err != nil {
			t.Fatal(err)
		}
	}
	w := storeMsg(50, "m")
	w.RetractedAt = time.UnixMilli(1_700_000_000_999)
	w.RetractedBy = protocol.RetractedBy_PurgeCap
	w.RetractedByTask = fsTask
	if err := s.Retract("tree.x.d", w); err != nil {
		t.Fatal(err)
	}
	if err := s.SetShown("tree.x.d", fsTask, 90); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := openTestStore(t, dir, time.Hour)
	defer s2.Close()
	topics, err := s2.Load(64)
	if err != nil {
		t.Fatal(err)
	}
	if len(topics) != 1 {
		t.Fatalf("loaded %d topics, want 1", len(topics))
	}
	got := topics[0]
	if got.Name != "tree.x.d" || got.Retention.TTL != ret.TTL || got.Retention.Forever ||
		got.Retention.SetBy != ret.SetBy || !got.Retention.SetAt.Equal(ret.SetAt) {
		t.Fatalf("retention = %+v (name %q), want %+v", got.Retention, got.Name, ret)
	}
	if len(got.Ring) != 64 || got.Ring[63].Seq != 100 {
		t.Fatalf("ring len %d, last %d; want 64 ending at 100", len(got.Ring), got.Ring[len(got.Ring)-1].Seq)
	}
	for _, m := range got.Ring {
		if m.Seq == 50 {
			t.Fatal("withdrawn seq 50 is in the live ring")
		}
	}
	if len(got.Retracted) != 1 || got.Retracted[0].Seq != 50 || got.Retracted[0].RetractedBy != protocol.RetractedBy_PurgeCap ||
		got.Retracted[0].RetractedByTask != fsTask || !got.Retracted[0].RetractedAt.Equal(w.RetractedAt) {
		t.Fatalf("retracted = %+v", got.Retracted)
	}
	if got.Shown[fsTask] != 90 || got.MaxSeq != 100 {
		t.Fatalf("shown %v maxSeq %d", got.Shown, got.MaxSeq)
	}
	want := storeMsg(100, "m")
	m := got.Ring[63]
	if !bytes.Equal(m.Payload, want.Payload) || m.FromTask != want.FromTask || m.FromRunner != want.FromRunner ||
		m.FromHostname != want.FromHostname || m.FromAgentProfile != want.FromAgentProfile || m.SenderKind != want.SenderKind ||
		m.ReplyToTopic != want.ReplyToTopic || !m.ReceivedAt.Equal(want.ReceivedAt) || !m.NoContact || !m.ExcludeSelf ||
		m.Topic != "tree.x.d" {
		t.Fatalf("message did not round-trip: %+v", m)
	}
}

func TestFileStore_PageNewestFirst(t *testing.T) {
	s := openTestStore(t, t.TempDir(), time.Hour)
	defer s.Close()
	_ = s.SetRetention("p", Retention{Forever: true})
	for seq := uint64(1); seq <= 100; seq++ {
		_ = s.Append("p", storeMsg(seq, "m"))
	}
	page, err := s.Page("p", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 10 || page[0].Seq != 100 || page[9].Seq != 91 {
		t.Fatalf("first page = %d rows %d..%d, want 100..91", len(page), page[0].Seq, page[len(page)-1].Seq)
	}
	page, _ = s.Page("p", 91, 10)
	if len(page) != 10 || page[0].Seq != 90 || page[9].Seq != 81 {
		t.Fatalf("second page = %d rows, want 90..81", len(page))
	}
}

func TestFileStore_RemoveDestroysBytes(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir, time.Hour)
	defer s.Close()
	_ = s.SetRetention("r", Retention{Forever: true})
	for seq := uint64(1); seq <= 10; seq++ {
		body := "m"
		if seq == 7 {
			body = "SECRET-XYZ"
		}
		_ = s.Append("r", storeMsg(seq, body))
	}
	removed, err := s.Remove("r", 7)
	if err != nil || !removed {
		t.Fatalf("Remove = %v, %v", removed, err)
	}
	raw, err := os.ReadFile(logFileOf(t, dir, "r"))
	if err != nil {
		t.Fatal(err)
	}
	// base64 of the payload is what a msg record holds; check both spellings.
	if bytes.Contains(raw, []byte("SECRET-XYZ")) || bytes.Contains(raw, []byte("U0VDUkVULVhZWg")) {
		t.Fatal("the purged payload is still in the log")
	}
	if removed, _ := s.Remove("r", 7); removed {
		t.Fatal("a second Remove of the same seq reported removed")
	}
	page, _ := s.Page("r", 0, 100)
	if len(page) != 9 {
		t.Fatalf("page after remove has %d rows, want 9", len(page))
	}
}

func TestFileStore_DropAndTruncate(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir, time.Hour)
	_ = s.SetRetention("gone", Retention{Forever: true})
	_ = s.Append("gone", storeMsg(1, "m"))
	_ = s.SetRetention("empty", Retention{TTL: time.Hour})
	_ = s.Append("empty", storeMsg(2, "m"))
	if err := s.Drop("gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(logFileOf(t, dir, "gone")); !os.IsNotExist(err) {
		t.Fatalf("dropped log still exists: %v", err)
	}
	if err := s.Truncate("empty"); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	s2 := openTestStore(t, dir, time.Hour)
	defer s2.Close()
	topics, _ := s2.Load(64)
	if len(topics) != 1 || topics[0].Name != "empty" || len(topics[0].Ring) != 0 || topics[0].Retention.TTL != time.Hour {
		t.Fatalf("after drop+truncate loaded %+v", topics)
	}
}

func TestFileStore_UnsafeTopicNames(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir, time.Hour)
	names := []string{"a/b", "..", strings.Repeat("x", 300)}
	for i, n := range names {
		_ = s.SetRetention(n, Retention{Forever: true})
		if err := s.Append(n, storeMsg(uint64(i+1), n)); err != nil {
			t.Fatalf("Append(%q): %v", n, err)
		}
	}
	_ = s.Close()
	s2 := openTestStore(t, dir, time.Hour)
	defer s2.Close()
	topics, err := s2.Load(64)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, tp := range topics {
		if len(tp.Ring) == 1 {
			got[tp.Name] = string(tp.Ring[0].Payload)
		}
	}
	for _, n := range names {
		if got[n] != n {
			t.Errorf("topic %q did not round-trip (got %q)", n[:min(len(n), 10)], got[n])
		}
	}
}

func TestFileStore_TornLastLine(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir, time.Hour)
	_ = s.SetRetention("torn", Retention{Forever: true})
	for seq := uint64(1); seq <= 3; seq++ {
		_ = s.Append("torn", storeMsg(seq, "m"))
	}
	_ = s.Close()
	f, err := os.OpenFile(logFileOf(t, dir, "torn"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString(`{"t":"msg","se`)
	_ = f.Close()

	s2 := openTestStore(t, dir, time.Hour)
	defer s2.Close()
	topics, err := s2.Load(64)
	if err != nil {
		t.Fatalf("Load after a torn line: %v", err)
	}
	if len(topics) != 1 || len(topics[0].Ring) != 3 {
		t.Fatalf("loaded %+v, want the 3 whole messages", topics)
	}
}

func TestFileStore_BadMiddleLineSkipped(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir, time.Hour)
	_ = s.SetRetention("bad", Retention{Forever: true})
	for seq := uint64(1); seq <= 5; seq++ {
		_ = s.Append("bad", storeMsg(seq, "m"))
	}
	_ = s.Close()
	path := logFileOf(t, dir, "bad")
	raw, _ := os.ReadFile(path)
	lines := strings.Split(string(raw), "\n")
	// lines[0] is the header; lines[3] is the third msg record.
	lines[3] = "not json"
	_ = os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644)

	s2 := openTestStore(t, dir, time.Hour)
	defer s2.Close()
	topics, err := s2.Load(64)
	if err != nil {
		t.Fatalf("Load with a bad middle line: %v", err)
	}
	if len(topics) != 1 || len(topics[0].Ring) != 4 {
		t.Fatalf("loaded %+v, want the 4 readable messages", topics)
	}
}

func TestFileStore_FlushBatches(t *testing.T) {
	dir := t.TempDir()
	s := openTestStore(t, dir, time.Hour)
	_ = s.SetRetention("b", Retention{Forever: true})
	st, _ := os.Stat(logFileOf(t, dir, "b"))
	before := st.Size()
	_ = s.Append("b", storeMsg(1, "m"))
	st, _ = os.Stat(logFileOf(t, dir, "b"))
	if st.Size() != before {
		t.Fatalf("Append wrote through (size %d -> %d) with an hour-long flush interval", before, st.Size())
	}
	_ = s.Close()
	st, _ = os.Stat(logFileOf(t, dir, "b"))
	if st.Size() <= before {
		t.Fatal("Close did not flush the buffered record")
	}
}
