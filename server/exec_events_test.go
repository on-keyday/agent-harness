package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/exec/frame"
)

func TestExecStatsSweepPublishesOnlyWhatMoved(t *testing.T) {
	h := &TaskHandler{}
	var kinds []protocol.StatusEventKind
	h.OnExecEvent = func(kind protocol.StatusEventKind, _ *execRun) { kinds = append(kinds, kind) }
	e := &execRun{taskIDHex: "aa"}
	h.execs().add(e)

	h.sweepExecStats()
	if len(kinds) != 1 || kinds[0] != protocol.StatusEventKind_ExecStats {
		t.Fatalf("first sweep: %v", kinds)
	}
	h.sweepExecStats()
	h.sweepExecStats()
	if len(kinds) != 1 {
		t.Fatalf("an idle exec published %d events, want 1", len(kinds))
	}
	var down frameScanner
	for i := 0; i < 1000; i++ {
		down.scan(frameBytes(frame.FrameType_Stdout, []byte("x")), e.observeFrame, e.observeEmptyFrame)
	}
	h.sweepExecStats()
	h.sweepExecStats()
	if len(kinds) != 2 {
		t.Fatalf("a burst produced %d events in total, want 2", len(kinds))
	}
	tap := newExecTap(nil, protocol.ExecTapFilter_All, 0)
	e.addTap(tap)
	h.sweepExecStats()
	if len(kinds) != 3 {
		t.Fatal("a new tap produced no event: a row would show taps=0 while someone reads it")
	}
}
