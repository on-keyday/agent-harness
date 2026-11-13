package server

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/vtgrid"
	"github.com/on-keyday/objtrsf/exec/frame"
)

// memSink is a ptyRecordSink that keeps entries in memory, segment by segment,
// optionally slowly.
type memSink struct {
	mu       sync.Mutex
	segs     [][]protocol.RecordEntry
	limit    int64
	delay    time.Duration
	detached bool
	closed   bool
}

func newMemSink(limit int64) *memSink {
	return &memSink{segs: [][]protocol.RecordEntry{nil}, limit: limit}
}

func (s *memSink) SegmentLimit() int64 { return s.limit }
func (s *memSink) Append(e protocol.RecordEntry) error {
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.segs[len(s.segs)-1] = append(s.segs[len(s.segs)-1], e)
	return nil
}
func (s *memSink) Rotate(head []protocol.RecordEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.segs = append(s.segs, append([]protocol.RecordEntry(nil), head...))
	return nil
}
func (s *memSink) Flush() error  { return nil }
func (s *memSink) Detach() error { s.mu.Lock(); s.detached = true; s.mu.Unlock(); return nil }
func (s *memSink) Close(time.Time) error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return nil
}
func (s *memSink) snapshot() [][]protocol.RecordEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([][]protocol.RecordEntry, len(s.segs))
	for i, seg := range s.segs {
		out[i] = append([]protocol.RecordEntry(nil), seg...)
	}
	return out
}
func (s *memSink) all() []protocol.RecordEntry {
	var out []protocol.RecordEntry
	for _, seg := range s.snapshot() {
		out = append(out, seg...)
	}
	return out
}

// The record format names objtrsf's frame types by number (RecordEntry's
// comment in message.bgn); a renumbering there would misread every segment on
// disk.
func TestRecordEntryFrameTypesMatchObjtrsf(t *testing.T) {
	if frame.FrameType_Stdin != 0 || frame.FrameType_Stdout != 1 || frame.FrameType_Stderr != 2 ||
		frame.FrameType_Control != 3 || frame.FrameType_Synth != 4 {
		t.Fatal("objtrsf FrameType numbering changed; RecordEntry.frame_type on disk depends on it")
	}
}

func TestRecordingCapturesOutputAndSizeNotInput(t *testing.T) {
	mux, runner, ctx := newStampMux(t)
	sink := newMemSink(1 << 20)
	if err := mux.StartRecording(sink, recordFresh); err != nil {
		t.Fatal(err)
	}
	control := newFakeStream(t)
	if err := mux.Attach(ctx, control); err != nil {
		t.Fatal(err)
	}
	control.QueueRead(encodeFrame(frame.FrameType_Stdin, []byte("typed-secret")))
	control.QueueRead(makeWinSizeFrame(40, 150))
	runner.QueueRead(encodeFrame(frame.FrameType_Stdout, []byte("out")))
	waitFor(t, func() bool {
		var out, size bool
		for _, e := range sink.all() {
			out = out || (e.FrameType == uint8(frame.FrameType_Stdout) && string(e.Payload) == "out")
			size = size || e.FrameType == uint8(frame.FrameType_Control)
		}
		return out && size
	})
	for _, e := range sink.all() {
		if strings.Contains(string(e.Payload), "typed-secret") || e.FrameType == uint8(frame.FrameType_Stdin) {
			t.Fatalf("input reached the record: %+v", e)
		}
	}
}

// replayText feeds entries to a fresh screen model the way a player would.
func replayText(entries []protocol.RecordEntry) string {
	g := vtgrid.New(80, 24)
	for _, e := range entries {
		switch frame.FrameType(e.FrameType) {
		case frame.FrameType_Control:
			if cols, rows, ok := winSizeOf(encodeFrame(frame.FrameType_Control, e.Payload)); ok && cols > 0 && rows > 0 {
				g.Resize(cols, rows)
			}
		default:
			_, _ = g.Write(e.Payload)
		}
	}
	return g.String()
}

func TestRotatedSegmentHeadRebuildsTheScreen(t *testing.T) {
	mux, runner, _ := newStampMux(t)
	sink := newMemSink(200)
	if err := mux.StartRecording(sink, recordFresh); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		runner.QueueRead(encodeFrame(frame.FrameType_Stdout, []byte(fmt.Sprintf("line %02d\r\n", i))))
	}
	runner.QueueRead(encodeFrame(frame.FrameType_Stdout, []byte("\x1b[2;5Hmoved")))
	waitFor(t, func() bool {
		all := sink.all()
		return len(all) > 0 && strings.Contains(string(all[len(all)-1].Payload), "moved")
	})
	segs := sink.snapshot()
	if len(segs) < 3 {
		t.Fatalf("%d segments, want rotations at a 200-byte limit", len(segs))
	}
	if len(segs[0]) == 0 || segs[0][0].FrameType != uint8(frame.FrameType_Stdout) {
		t.Fatal("a fresh record's first segment must start with the PTY's own first frame, not a head")
	}
	last := segs[len(segs)-1]
	if len(last) == 0 || last[0].FrameType != uint8(frame.FrameType_Synth) {
		t.Fatalf("a rotated segment does not start with a Synth head: %+v", last)
	}
	mux.screenMu.Lock()
	live := mux.screen.String()
	mux.screenMu.Unlock()
	if got := replayText(last); got != live {
		t.Fatalf("the last segment alone replays to\n%s\nwant the live screen\n%s", got, live)
	}
}

func TestStartRecordingSeedsFromRing(t *testing.T) {
	mux, runner, _ := newStampMux(t)
	runner.QueueRead(encodeFrame(frame.FrameType_Stdout, []byte("before")))
	waitFor(t, func() bool { return mux.LastOutputUnixNano() != 0 })
	sink := newMemSink(1 << 20)
	start := time.Now().UnixNano()
	if err := mux.StartRecording(sink, recordFromRing); err != nil {
		t.Fatal(err)
	}
	runner.QueueRead(encodeFrame(frame.FrameType_Stdout, []byte("after")))
	waitFor(t, func() bool { return len(sink.all()) >= 2 })
	all := sink.all()
	if string(all[0].Payload) != "before" || int64(all[0].TsNs) < start {
		t.Fatalf("first entry %q at %d, want the ring's output stamped at the start (%d)", all[0].Payload, all[0].TsNs, start)
	}
	if string(all[len(all)-1].Payload) != "after" {
		t.Fatalf("last entry %q, want the live output after the seed", all[len(all)-1].Payload)
	}
}

func TestFreshRecordHasNoHead(t *testing.T) {
	mux, runner, _ := newStampMux(t)
	sink := newMemSink(1 << 20)
	_ = mux.StartRecording(sink, recordFresh)
	runner.QueueRead(encodeFrame(frame.FrameType_Stdout, []byte("first")))
	waitFor(t, func() bool { return len(sink.all()) == 1 })
	if e := sink.all()[0]; e.FrameType != uint8(frame.FrameType_Stdout) || string(e.Payload) != "first" {
		t.Fatalf("first entry %+v, want the PTY's first frame", e)
	}
}

func TestStopThenStartMakesNewSegment(t *testing.T) {
	mux, runner, _ := newStampMux(t)
	sink := newMemSink(1 << 20)
	_ = mux.StartRecording(sink, recordFresh)
	runner.QueueRead(encodeFrame(frame.FrameType_Stdout, []byte("one")))
	waitFor(t, func() bool { return len(sink.all()) == 1 })
	mux.StopRecording()
	if mux.Recording() {
		t.Fatal("still recording after StopRecording")
	}
	sink.mu.Lock()
	closed := sink.closed
	sink.closed = false
	sink.mu.Unlock()
	if !closed {
		t.Fatal("StopRecording did not Close the writer")
	}
	if err := mux.StartRecording(sink, recordAgain); err != nil {
		t.Fatal(err)
	}
	runner.QueueRead(encodeFrame(frame.FrameType_Stdout, []byte("two")))
	waitFor(t, func() bool {
		segs := sink.snapshot()
		return len(segs) == 2 && len(segs[1]) > 0 && string(segs[1][len(segs[1])-1].Payload) == "two"
	})
	if segs := sink.snapshot(); segs[1][0].FrameType != uint8(frame.FrameType_Synth) {
		t.Fatalf("the restarted segment begins with %+v, want the Synth head", segs[1][0])
	}
}

func TestMuxStopDetachesWithoutClosing(t *testing.T) {
	mux, _, _ := newStampMux(t)
	sink := newMemSink(1 << 20)
	_ = mux.StartRecording(sink, recordFresh)
	mux.Stop()
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if !sink.detached || sink.closed {
		t.Fatalf("after Stop: detached=%v closed=%v, want detached and not closed", sink.detached, sink.closed)
	}
}

// Review Focus 5: a slow disk stalls the pump at the queue's bound, never
// reorders or drops a frame.
func TestRecorderKeepsOrderUnderSlowWriter(t *testing.T) {
	mux, runner, _ := newStampMux(t)
	sink := newMemSink(1 << 30)
	sink.delay = 20 * time.Microsecond
	_ = mux.StartRecording(sink, recordFresh)
	const n = 5000
	for i := 0; i < n; i++ {
		runner.QueueRead(encodeFrame(frame.FrameType_Stdout, []byte(fmt.Sprintf("%d,", i))))
	}
	deadline := time.Now().Add(30 * time.Second)
	for len(sink.all()) < n && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	all := sink.all()
	if len(all) != n {
		t.Fatalf("recorded %d of %d frames", len(all), n)
	}
	for i, e := range all {
		if string(e.Payload) != fmt.Sprintf("%d,", i) {
			t.Fatalf("entry %d is %q: out of order", i, e.Payload)
		}
	}
}
