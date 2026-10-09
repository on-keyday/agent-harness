package server

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/exec/frame"
)

// feed runs bytes through the same path the relay does, one direction.
func feed(e *execRun, s *frameScanner, b []byte) {
	s.scan(b, e.observeFrame, e.observeEmptyFrame)
}

func TestCountersArePerChannelAndSkipControl(t *testing.T) {
	e := &execRun{}
	var up, down frameScanner
	feed(e, &up, frameBytes(frame.FrameType_Stdin, []byte("req")))
	feed(e, &up, frameBytes(frame.FrameType_Control, []byte("sig")))
	feed(e, &down, frameBytes(frame.FrameType_Stdout, []byte("resp!")))
	feed(e, &down, frameBytes(frame.FrameType_Synth, []byte("+")))
	feed(e, &down, frameBytes(frame.FrameType_Stderr, []byte("w")))

	in, out, errb, last := e.counters()
	if in != 3 || out != 6 || errb != 1 {
		t.Fatalf("stdin=%d stdout=%d stderr=%d, want 3/6/1 (control counted nowhere, synth as stdout)", in, out, errb)
	}
	if last == 0 {
		t.Fatal("last activity not stamped")
	}
	info := execRunInfo(e)
	if info.StdinBytes != 3 || info.StdoutBytes != 6 || info.StderrBytes != 1 || info.LastActivityUnixMs == 0 {
		t.Fatalf("listing row: %+v", info)
	}
}

// A tap opened after N stdout bytes starts at N.
func TestExecTapOpenedMidExecStartsAtTheChannelsOffset(t *testing.T) {
	e := &execRun{}
	var down frameScanner
	feed(e, &down, frameBytes(frame.FrameType_Stdout, []byte("12345")))

	tap, recs := newTestExecTap(t, e, protocol.ExecTapFilter_All, 0)
	defer e.removeTap(tap)
	feed(e, &down, frameBytes(frame.FrameType_Stdout, []byte("ab")))

	d := drainExec(t, recs, 1)[0].Data()
	if d == nil || d.StreamOffset != 5 || string(d.Data) != "ab" {
		t.Fatalf("data %+v, want offset 5 payload ab", d)
	}
}

func TestExecTapSeesStdinEOF(t *testing.T) {
	e := &execRun{}
	tap, recs := newTestExecTap(t, e, protocol.ExecTapFilter_All, 0)
	defer e.removeTap(tap)
	var up frameScanner
	feed(e, &up, frameBytes(frame.FrameType_Stdin, []byte("x")))
	feed(e, &up, frameBytes(frame.FrameType_Stdin, nil))

	got := drainExec(t, recs, 2)
	if got[1].Kind != protocol.ExecTapRecordKind_Eof || got[1].Eof().Channel != protocol.ExecTapChannel_Stdin {
		t.Fatalf("second record: kind %v", got[1].Kind)
	}
}

func TestExecTapFilterAndTruncation(t *testing.T) {
	e := &execRun{}
	tap, recs := newTestExecTap(t, e, protocol.ExecTapFilter_Stdout, 4)
	defer e.removeTap(tap)
	var up, down frameScanner
	feed(e, &up, frameBytes(frame.FrameType_Stdin, []byte("ignored")))
	feed(e, &down, frameBytes(frame.FrameType_Stdout, []byte("0123456789")))
	feed(e, &down, frameBytes(frame.FrameType_Stdout, []byte("ab")))

	got := drainExec(t, recs, 2)
	if d := got[0].Data(); string(d.Data) != "0123" || d.TruncatedBytes != 6 || d.Channel != protocol.ExecTapChannel_Stdout {
		t.Fatalf("first: %+v", d)
	}
	if d := got[1].Data(); d.StreamOffset != 10 {
		t.Fatalf("offset must count the cut bytes, got %d", d.StreamOffset)
	}
}

// The relay must not wait on a tap that is not draining, and the tap survives
// with a gap.
func TestSlowExecTapGetsAGapAndTheRelayIsNotBlocked(t *testing.T) {
	e := &execRun{}
	sink := &execChanSink{out: make(chan *protocol.ExecTapRecord, 1)}
	tap := newExecTap(sink, protocol.ExecTapFilter_All, 0)
	e.addTap(tap)
	defer e.removeTap(tap)
	var down frameScanner
	for i := 0; i < recordTapQueueDepth*4; i++ {
		feed(e, &down, frameBytes(frame.FrameType_Stdout, []byte("0123456789")))
	}
	if tap.q.missedBytes() == 0 {
		t.Fatal("overflow not counted")
	}
	if e.tapCount() != 1 {
		t.Fatal("a slow tap was dropped")
	}
}

// The runner reports an exec's end on its control stream and the last output on
// the data stream, and nothing orders the two at the server. A tap must still
// show output that arrives after the end was reported: exec_ended waits for
// the runner→client relay to finish.
func TestExecTapShowsOutputThatArrivesAfterTheEndIsReported(t *testing.T) {
	e := &execRun{}
	e.beginOutput()
	tap, recs := newTestExecTap(t, e, protocol.ExecTapFilter_All, 0)
	defer e.removeTap(tap)

	e.endTaps(protocol.ExecEventKind_Exited, 0)
	var down frameScanner
	feed(e, &down, frameBytes(frame.FrameType_Stdout, []byte("reply")))
	e.outputDone()

	got := drainExec(t, recs, 2)
	if d := got[0].Data(); d == nil || string(d.Data) != "reply" {
		t.Fatalf("first record kind %v: the late reply was lost", got[0].Kind)
	}
	if got[1].Kind != protocol.ExecTapRecordKind_ExecEnded {
		t.Fatalf("second record kind %v, want exec_ended", got[1].Kind)
	}
}

// A relay that never finishes (a runner stream that lingers after a kill) must
// not keep the tap open forever: exec_ended goes out after a grace period.
func TestExecTapEndsAfterTheGraceWhenOutputNeverFinishes(t *testing.T) {
	old := execTapEndGrace
	execTapEndGrace = 50 * time.Millisecond
	defer func() { execTapEndGrace = old }()

	e := &execRun{}
	e.beginOutput()
	_, recs := newTestExecTap(t, e, protocol.ExecTapFilter_All, 0)
	e.endTaps(protocol.ExecEventKind_Killed, -1)
	if rec := drainExec(t, recs, 1)[0]; rec.Kind != protocol.ExecTapRecordKind_ExecEnded {
		t.Fatalf("kind %v", rec.Kind)
	}
}
