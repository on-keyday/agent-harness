package server

import (
	"bytes"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// RecordStore keeps PTY records (pillar A spec, A-D23..A-D26).

const recTask = "00112233445566778899aabbccddeeff"
const recTask2 = "ffeeddccbbaa99887766554433221100"

func recEntry(ts int64, payload string) protocol.RecordEntry {
	e := protocol.RecordEntry{TsNs: uint64(ts), FrameType: 1}
	e.SetPayload([]byte(payload))
	return e
}

// entrySize is one entry's encoded size, for sizing caps in tests.
func entrySize(payload string) int64 {
	e := recEntry(1, payload)
	return int64(len(e.MustAppend(nil)))
}

func newRecStore(t *testing.T, runMax, totalMax int64) *RecordStore {
	t.Helper()
	s, err := NewRecordStore(RecordConfig{Dir: filepath.Join(t.TempDir(), "records"), RunMax: runMax, TotalMax: totalMax})
	if err != nil {
		t.Fatalf("NewRecordStore: %v", err)
	}
	return s
}

func readAll(t *testing.T, s *RecordStore, task string, run uint32) []protocol.RecordEntry {
	t.Helper()
	var out []protocol.RecordEntry
	if err := s.Read(task, run, func(e protocol.RecordEntry) error {
		out = append(out, e)
		return nil
	}); err != nil {
		t.Fatalf("Read: %v", err)
	}
	return out
}

func payloads(es []protocol.RecordEntry) []string {
	var out []string
	for _, e := range es {
		out = append(out, string(e.Payload))
	}
	return out
}

func segFiles(t *testing.T, s *RecordStore, task string, run uint32) []string {
	t.Helper()
	ents, err := os.ReadDir(s.runDir(task, run))
	if err != nil {
		t.Fatalf("read run dir: %v", err)
	}
	var out []string
	for _, e := range ents {
		if strings.Contains(e.Name(), ".seg") {
			out = append(out, e.Name())
		}
	}
	return out
}

func TestRunWriterRoundTrip(t *testing.T) {
	s := newRecStore(t, 64<<20, 1<<30)
	w, err := s.OpenRun(recTask, 1, time.Unix(0, 1))
	if err != nil {
		t.Fatal(err)
	}
	for i, p := range []string{"a", "bb", "ccc"} {
		if err := w.Append(recEntry(int64(i+1), p)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(time.Unix(0, 9)); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(payloads(readAll(t, s, recTask, 1)), ","); got != "a,bb,ccc" {
		t.Fatalf("read back %q", got)
	}
}

func TestSegmentGzippedOnClose(t *testing.T) {
	s := newRecStore(t, 64<<20, 1<<30)
	w, _ := s.OpenRun(recTask, 1, time.Unix(0, 1))
	_ = w.Append(recEntry(1, "first"))
	if err := w.Rotate([]protocol.RecordEntry{recEntry(2, "head")}); err != nil {
		t.Fatal(err)
	}
	_ = w.Append(recEntry(3, "second"))
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	files := segFiles(t, s, recTask, 1)
	if len(files) != 2 || !strings.HasSuffix(files[0], ".seg.gz") || !strings.HasSuffix(files[1], ".seg") || strings.HasSuffix(files[1], ".gz") {
		t.Fatalf("segment files %v, want one closed .seg.gz then one open .seg", files)
	}
	if got := strings.Join(payloads(readAll(t, s, recTask, 1)), ","); got != "first,head,second" {
		t.Fatalf("read back %q", got)
	}
}

func TestRunCapDropsOldestAndMarksTruncated(t *testing.T) {
	seg := entrySize("x") // one entry fills a segment
	s := newRecStore(t, 8*seg, 1<<30)
	w, _ := s.OpenRun(recTask, 1, time.Unix(0, 1))
	if w.SegmentLimit() != seg {
		t.Fatalf("segment limit %d, want run max / 8 = %d", w.SegmentLimit(), seg)
	}
	for i := 1; i <= 11; i++ {
		_ = w.Append(recEntry(int64(i), "x"))
		if err := w.Rotate(nil); err != nil {
			t.Fatal(err)
		}
	}
	_ = w.Close(time.Unix(0, 99))
	got := readAll(t, s, recTask, 1)
	if len(got) != 8 || got[0].TsNs != 4 {
		t.Fatalf("kept %d entries starting at ts %d, want the newest 8 starting at 4", len(got), got[0].TsNs)
	}
	infos := s.List(recTask)
	if len(infos) != 1 || infos[0].TruncatedFromNs != 4 {
		t.Fatalf("list %+v, want truncated_from_ns 4", infos)
	}
}

func TestTotalCapEvictsOldestAcrossTasks(t *testing.T) {
	// Incompressible payloads, so a closed segment's size on disk is about
	// its entries' size and the cap below needs exactly one eviction.
	rnd := rand.New(rand.NewSource(1))
	noise := func() string {
		b := make([]byte, 4096)
		rnd.Read(b)
		return string(b)
	}
	s := newRecStore(t, 64<<20, 1<<30)
	wa, _ := s.OpenRun(recTask, 1, time.Unix(0, 1))
	for i := 1; i <= 3; i++ {
		_ = wa.Append(recEntry(int64(i), noise()))
		_ = wa.Rotate(nil)
	}
	_ = wa.Close(time.Unix(0, 10))
	wb, _ := s.OpenRun(recTask2, 1, time.Unix(0, 20))
	_ = wb.Append(recEntry(21, noise()))
	_ = wb.Flush()

	// Half a segment under what is on disk now: closing B's segment must push
	// out exactly the oldest closed segment on the server, task A's first.
	s.mu.Lock()
	s.cfg.TotalMax = s.diskTotalLocked() - 2048
	s.mu.Unlock()
	if err := wb.Rotate(nil); err != nil {
		t.Fatal(err)
	}
	a := readAll(t, s, recTask, 1)
	if len(a) != 2 || a[0].TsNs != 2 {
		t.Fatalf("task A keeps %d entries (first ts %v); want the newest 2", len(a), a)
	}
	if b := readAll(t, s, recTask2, 1); len(b) != 1 || b[0].TsNs != 21 {
		t.Fatalf("task B lost its segment: %d entries", len(b))
	}
}

// Review Focus 2: a crash leaves a half-written entry at the end of the open
// segment. Reading stops at the last whole entry; the startup pass compresses
// the segment without the partial one.
func TestReadStopsAtTruncatedTail(t *testing.T) {
	s := newRecStore(t, 64<<20, 1<<30)
	w, _ := s.OpenRun(recTask, 1, time.Unix(0, 1))
	_ = w.Append(recEntry(1, "whole"))
	_ = w.Append(recEntry(2, "cut"))
	_ = w.Detach()
	seg := filepath.Join(s.runDir(recTask, 1), segFiles(t, s, recTask, 1)[0])
	b, _ := os.ReadFile(seg)
	if err := os.WriteFile(seg, b[:len(b)-2], 0o600); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(payloads(readAll(t, s, recTask, 1)), ","); got != "whole" {
		t.Fatalf("read %q, want only the whole entry", got)
	}
	if err := s.CompressLeftovers(func(string) bool { return false }); err != nil {
		t.Fatal(err)
	}
	files := segFiles(t, s, recTask, 1)
	if len(files) != 1 || !strings.HasSuffix(files[0], ".seg.gz") {
		t.Fatalf("after the startup pass: %v, want one .seg.gz", files)
	}
	if got := strings.Join(payloads(readAll(t, s, recTask, 1)), ","); got != "whole" {
		t.Fatalf("after compression read %q", got)
	}
}

func TestCompressLeftoversSkipsHeld(t *testing.T) {
	s := newRecStore(t, 64<<20, 1<<30)
	w, _ := s.OpenRun(recTask, 1, time.Unix(0, 1))
	_ = w.Append(recEntry(1, "held"))
	_ = w.Detach()
	if err := s.CompressLeftovers(func(task string) bool { return task == recTask }); err != nil {
		t.Fatal(err)
	}
	if files := segFiles(t, s, recTask, 1); len(files) != 1 || strings.HasSuffix(files[0], ".gz") {
		t.Fatalf("a held run's open segment was closed: %v", files)
	}
}

// Review Focus 1 (store half): a re-adopting server reopens the run and
// continues its open segment; the record reads back as one sequence.
func TestReopenRunAppends(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "records")
	s1, _ := NewRecordStore(RecordConfig{Dir: dir, RunMax: 64 << 20, TotalMax: 1 << 30})
	w1, _ := s1.OpenRun(recTask, 2, time.Unix(0, 1))
	_ = w1.Append(recEntry(1, "before"))
	_ = w1.Detach()

	s2, err := NewRecordStore(RecordConfig{Dir: dir, RunMax: 64 << 20, TotalMax: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	w2, err := s2.OpenRun(recTask, 2, time.Unix(0, 5))
	if err != nil {
		t.Fatal(err)
	}
	_ = w2.Append(recEntry(6, "after"))
	_ = w2.Flush()
	if got := strings.Join(payloads(readAll(t, s2, recTask, 2)), ","); got != "before,after" {
		t.Fatalf("read %q", got)
	}
	if files := segFiles(t, s2, recTask, 2); len(files) != 1 {
		t.Fatalf("segments %v, want the one continued segment", files)
	}
	infos := s2.List(recTask)
	if len(infos) != 1 || infos[0].StartedNs != 1 || !infos[0].Recording() {
		t.Fatalf("list %+v, want one recording run started at 1", infos)
	}
}

func TestRetentionExpiresRecordNotRun(t *testing.T) {
	s := newRecStore(t, 64<<20, 1<<30)
	now := time.Now()
	for run, keep := range map[uint32]struct {
		forever bool
		ttl     time.Duration
		end     time.Time
	}{
		1: {ttl: time.Second, end: now.Add(-2 * time.Second)},
		2: {forever: true, end: now.Add(-2 * time.Second)},
		3: {ttl: time.Second},
	} {
		w, _ := s.OpenRun(recTask, run, now.Add(-time.Hour))
		_ = w.Append(recEntry(1, "x"))
		_ = w.Detach()
		if !keep.end.IsZero() {
			_ = s.FinishRun(recTask, run, keep.end, protocol.EndReason_ProcessExit)
		}
		if err := s.SetRetention(recTask, run, keep.forever, keep.ttl, now); err != nil {
			t.Fatal(err)
		}
	}
	s.sweepExpired(now)
	var runs []uint32
	for _, i := range s.List(recTask) {
		runs = append(runs, i.Run)
	}
	if len(runs) != 2 || runs[0] != 2 || runs[1] != 3 {
		t.Fatalf("runs left %v, want [2 3]: the expired one goes, forever and unended stay", runs)
	}
}

func TestDeleteAllRuns(t *testing.T) {
	s := newRecStore(t, 64<<20, 1<<30)
	for _, run := range []uint32{1, 2} {
		w, _ := s.OpenRun(recTask, run, time.Unix(0, 1))
		_ = w.Append(recEntry(1, "x"))
		_ = w.Close(time.Unix(0, 2))
	}
	if ok, err := s.Delete(recTask, 0); err != nil || !ok {
		t.Fatalf("Delete: %v %v", ok, err)
	}
	if _, err := os.Stat(filepath.Join(s.cfg.Dir, recTask)); !os.IsNotExist(err) {
		t.Fatalf("task directory still there: %v", err)
	}
	if len(s.List(recTask)) != 0 {
		t.Fatal("list still shows records")
	}
}

// A record's metadata round-trips through a restart: the end reason a run
// finished with is what a later server lists.
func TestFinishRunStampsEndAndCompresses(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "records")
	s, _ := NewRecordStore(RecordConfig{Dir: dir, RunMax: 64 << 20, TotalMax: 1 << 30})
	w, _ := s.OpenRun(recTask, 1, time.Unix(0, 1))
	_ = w.Append(recEntry(1, "x"))
	_ = w.Detach()
	if err := s.FinishRun(recTask, 1, time.Unix(0, 7), protocol.EndReason_Deadline); err != nil {
		t.Fatal(err)
	}
	s2, _ := NewRecordStore(RecordConfig{Dir: dir, RunMax: 64 << 20, TotalMax: 1 << 30})
	infos := s2.List(recTask)
	if len(infos) != 1 || infos[0].EndedNs != 7 || infos[0].EndReason != protocol.EndReason_Deadline || infos[0].Recording() {
		t.Fatalf("after restart: %+v", infos)
	}
	if files := segFiles(t, s2, recTask, 1); len(files) != 1 || !strings.HasSuffix(files[0], ".seg.gz") {
		t.Fatalf("segments %v, want the finished run compressed", files)
	}
	if !bytes.Equal(readAll(t, s2, recTask, 1)[0].Payload, []byte("x")) {
		t.Fatal("payload lost")
	}
}
