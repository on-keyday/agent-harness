package agentboard

import (
	"bytes"
	"os"
	"testing"
	"time"
)

func retainedStore(t *testing.T, dir, topic string, flush time.Duration) *FileStore {
	t.Helper()
	s := openTestStore(t, dir, flush)
	if err := s.SetRetention(topic, Retention{Forever: true, SetAt: time.UnixMilli(1_700_000_000_000)}); err != nil {
		t.Fatal(err)
	}
	return s
}

func seqsOf(ms []RetainedMessage) []uint64 {
	out := make([]uint64, len(ms))
	for i, m := range ms {
		out[i] = m.Seq
	}
	return out
}

func TestFileStore_RingFloorHidesFromLoad(t *testing.T) {
	dir := t.TempDir()
	s := retainedStore(t, dir, "g", time.Hour)
	for seq := uint64(1); seq <= 5; seq++ {
		if err := s.Append("g", storeMsg(seq, "m")); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.FlushRing("g", 5); err != nil {
		t.Fatal(err)
	}
	if err := s.Append("g", storeMsg(6, "m")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := openTestStore(t, dir, time.Hour)
	defer s2.Close()
	got, err := s2.Load(64)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("topics = %d", len(got))
	}
	if seqs := seqsOf(got[0].Ring); len(seqs) != 1 || seqs[0] != 6 {
		t.Errorf("ring = %v, want [6]", seqs)
	}
	if got[0].RingFloor != 5 {
		t.Errorf("RingFloor = %d, want 5", got[0].RingFloor)
	}
	if got[0].MaxSeq != 6 {
		t.Errorf("MaxSeq = %d, want 6", got[0].MaxSeq)
	}
}

func TestFileStore_RingFloorSyncsBufferedRecords(t *testing.T) {
	dir := t.TempDir()
	s := retainedStore(t, dir, "g", time.Hour)
	defer s.Close()
	for seq := uint64(1); seq <= 3; seq++ {
		if err := s.Append("g", storeMsg(seq, "body-on-disk")); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.FlushRing("g", 3); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(logFileOf(t, dir, "g"))
	if err != nil {
		t.Fatal(err)
	}
	if n := bytes.Count(raw, []byte(`"t":"msg"`)); n != 3 {
		t.Errorf("msg records on disk = %d, want 3", n)
	}
	if !bytes.Contains(raw, []byte(`"t":"ring_floor"`)) {
		t.Errorf("no ring_floor record on disk")
	}
}

func TestFileStore_TailIgnoresFloor(t *testing.T) {
	dir := t.TempDir()
	s := retainedStore(t, dir, "g", time.Hour)
	defer s.Close()
	for seq := uint64(1); seq <= 6; seq++ {
		if err := s.Append("g", storeMsg(seq, "m")); err != nil {
			t.Fatal(err)
		}
	}
	w := storeMsg(2, "m")
	w.RetractedAt = time.UnixMilli(1_700_000_000_500)
	if err := s.Retract("g", w); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushRing("g", 6); err != nil {
		t.Fatal(err)
	}
	live, withdrawn, err := s.Tail("g", 4)
	if err != nil {
		t.Fatal(err)
	}
	if got := seqsOf(live); len(got) != 4 || got[0] != 3 || got[3] != 6 {
		t.Errorf("live = %v, want [3 4 5 6]", got)
	}
	if got := seqsOf(withdrawn); len(got) != 1 || got[0] != 2 {
		t.Errorf("withdrawn = %v, want [2]", got)
	}
}

func TestFileStore_RingFloorAddsNoMessage(t *testing.T) {
	dir := t.TempDir()
	s := retainedStore(t, dir, "g", time.Hour)
	if err := s.Append("g", storeMsg(1, "m")); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushRing("g", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	meta, err := scanMeta(logFileOf(t, dir, "g"), "g")
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.msgs) != 1 || meta.maxSeq != 1 {
		t.Errorf("msgs=%d maxSeq=%d, want 1/1", len(meta.msgs), meta.maxSeq)
	}
}

// A lower ring_floor written after a higher one does not lower the floor:
// the messages the first flush took out stay on disk across a restart.
func TestFileStore_RingFloorNeverGoesDown(t *testing.T) {
	dir := t.TempDir()
	s := retainedStore(t, dir, "g", time.Hour)
	for seq := uint64(1); seq <= 12; seq++ {
		if err := s.Append("g", storeMsg(seq, "m")); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.FlushRing("g", 11); err != nil {
		t.Fatal(err)
	}
	if err := s.FlushRing("g", 10); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := openTestStore(t, dir, time.Hour)
	defer s2.Close()
	got, err := s2.Load(64)
	if err != nil {
		t.Fatal(err)
	}
	if got[0].RingFloor != 11 || len(got[0].Ring) != 1 || got[0].Ring[0].Seq != 12 {
		t.Errorf("floor %d ring %v, want 11 and [12]", got[0].RingFloor, seqsOf(got[0].Ring))
	}
}
