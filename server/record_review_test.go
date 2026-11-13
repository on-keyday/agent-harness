package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/exec/frame"
)

// Findings of the A1 store/capture checkpoint review (I1..I6).

// I1: a live read survives a rotation that gzips the open segment under it.
func TestLiveReadSurvivesARotation(t *testing.T) {
	s := newRecStore(t, 64<<20, 1<<30)
	w, _ := s.OpenRun(recTask, 1, time.Unix(0, 1))
	_ = w.Append(recEntry(1, "a"))
	_ = w.Rotate(nil)
	_ = w.Append(recEntry(2, "b"))
	_ = w.Flush()
	var got []string
	rotated := false
	err := s.Read(recTask, 1, func(e protocol.RecordEntry) error {
		got = append(got, string(e.Payload))
		if !rotated {
			rotated = true
			if err := w.Rotate(nil); err != nil { // segment 1 becomes .seg.gz now
				t.Fatal(err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read failed across a rotation: %v", err)
	}
	if strings.Join(got, ",") != "a,b" {
		t.Fatalf("read %v, want a,b", got)
	}
}

// I2: a switch-on racing the session's end must not leave a writer pinned.
func TestStartRecordingAfterStopIsRefused(t *testing.T) {
	mux, _, _ := newStampMux(t)
	mux.Stop()
	if err := mux.StartRecording(newMemSink(1<<20), recordAgain); err == nil {
		t.Fatal("StartRecording on a stopped mux succeeded")
	}
	if mux.Recording() {
		t.Fatal("a stopped mux reports recording")
	}
}

// I3: a crash between the rename to .seg.gz and the .seg's removal leaves the
// same segment twice; the complete .gz wins and the .seg goes.
func TestDuplicateSegmentPrefersTheGzip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "records")
	s, _ := NewRecordStore(RecordConfig{Dir: dir})
	w, _ := s.OpenRun(recTask, 1, time.Unix(0, 1))
	_ = w.Append(recEntry(1, "a"))
	_ = w.Append(recEntry(2, "b"))
	_ = w.Flush()
	run := s.runDir(recTask, 1)
	raw, _ := os.ReadFile(filepath.Join(run, segName(0, false)))
	_ = w.Close(time.Unix(0, 3))
	if err := os.WriteFile(filepath.Join(run, segName(0, false)), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	s2, err := NewRecordStore(RecordConfig{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(payloads(readAll(t, s2, recTask, 1)), ","); got != "a,b" {
		t.Fatalf("read %q, want each entry once", got)
	}
	if _, err := os.Stat(filepath.Join(run, segName(0, false))); !os.IsNotExist(err) {
		t.Fatal("the duplicate .seg is still there")
	}
}

// I4: a record started from the ring carries the size and the modes an
// attach would put in front of the same frames.
func TestRingSeedCarriesSizeAndModes(t *testing.T) {
	mux, runner, _ := newStampMux(t)
	if err := mux.applyWinSizeFrame(winSizeFrame(30, 100)); err != nil {
		t.Fatal(err)
	}
	runner.QueueRead(encodeFrame(frame.FrameType_Stdout, []byte("\x1b[?25lhidden-cursor")))
	waitFor(t, func() bool { return mux.LastOutputUnixNano() != 0 })
	sink := newMemSink(1 << 20)
	if err := mux.StartRecording(sink, recordFromRing); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return len(sink.all()) >= 2 })
	all := sink.all()
	if all[0].FrameType != uint8(frame.FrameType_Control) {
		t.Fatalf("first entry %+v, want the size", all[0])
	}
	if cols, rows, ok := winSizeOf(encodeFrame(frame.FrameType_Control, all[0].Payload)); !ok || cols != 100 || rows != 30 {
		t.Fatalf("size entry %dx%d ok=%v, want 100x30", cols, rows, ok)
	}
	if all[1].FrameType != uint8(frame.FrameType_Synth) {
		t.Fatalf("second entry %+v, want the mode preamble", all[1])
	}
}

// I5: one unreadable file costs that file, not the run.
func TestUnreadableFilesDegradePerSegment(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "records")
	s, _ := NewRecordStore(RecordConfig{Dir: dir})
	w, _ := s.OpenRun(recTask, 1, time.Unix(0, 1))
	_ = w.Append(recEntry(1, "kept"))
	_ = w.Close(time.Unix(0, 2))
	run := s.runDir(recTask, 1)
	if err := os.WriteFile(filepath.Join(run, segName(5, true)), nil, 0o600); err != nil { // an empty .gz
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(run, "meta.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	s2, err := NewRecordStore(RecordConfig{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if infos := s2.List(recTask); len(infos) != 1 {
		t.Fatalf("list %+v, want the run still listed", infos)
	}
	if got := strings.Join(payloads(readAll(t, s2, recTask, 1)), ","); got != "kept" {
		t.Fatalf("read %q, want the readable entry", got)
	}
	if ok, err := s2.Delete(recTask, 1); !ok || err != nil {
		t.Fatalf("delete %v %v", ok, err)
	}
}

// I6: retention counts from the run's end (A-D26), not from a switch-off.
func TestSwitchOffDoesNotStartTheRetentionClock(t *testing.T) {
	s := newRecStore(t, 64<<20, 1<<30)
	now := time.Now()
	w, _ := s.OpenRun(recTask, 1, now.Add(-time.Hour))
	_ = w.Append(recEntry(1, "x"))
	_ = w.Close(now.Add(-time.Minute)) // switched off a minute ago; the run goes on
	if err := s.SetRetention(recTask, 1, false, time.Second, now); err != nil {
		t.Fatal(err)
	}
	s.sweepExpired(now)
	if len(s.List(recTask)) != 1 {
		t.Fatal("a switched-off record of a run still going expired")
	}
	if err := s.FinishRun(recTask, 1, now.Add(-2*time.Second), protocol.EndReason_ProcessExit); err != nil {
		t.Fatal(err)
	}
	s.sweepExpired(now)
	if len(s.List(recTask)) != 0 {
		t.Fatal("the record outlived its retention after the run ended")
	}
}

// Final review (re-graded Important): open segments cannot be evicted, so
// they must not count against the server-wide cap — otherwise live sessions
// alone over the cap made it delete every closed record on the server, kept
// forever or not.
func TestOpenSegmentsDoNotEvictClosedRecords(t *testing.T) {
	s := newRecStore(t, 64<<20, 1<<30)
	old, _ := s.OpenRun(recTask, 1, time.Unix(0, 1))
	_ = old.Append(recEntry(1, "kept"))
	_ = old.Close(time.Unix(0, 2))
	s.mu.Lock()
	s.cfg.TotalMax = s.diskTotalLocked() + 4096 // the closed record fits with room to spare
	s.mu.Unlock()
	live, _ := s.OpenRun(recTask2, 1, time.Unix(0, 10))
	_ = live.Append(recEntry(11, strings.Repeat("z", 64<<10))) // an open segment far over the cap
	_ = live.Flush()
	other, _ := s.OpenRun(recTask2, 2, time.Unix(0, 20))
	_ = other.Append(recEntry(21, "t"))
	_ = other.Rotate(nil) // a close: the caps run
	if got := payloads(readAll(t, s, recTask, 1)); len(got) != 1 || got[0] != "kept" {
		t.Fatalf("the closed record was evicted for open segments' bytes: %v", got)
	}
}
