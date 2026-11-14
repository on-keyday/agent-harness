package server

import (
	"testing"

	"github.com/on-keyday/objtrsf/exec/frame"
)

func endControl(exit int32, reason string) []byte {
	ctrl := frame.Control{Type: frame.ControlType_End}
	e := frame.End{ExitCode: exit}
	e.SetReason([]byte(reason))
	ctrl.SetEnd(e)
	return ctrl.MustAppend(nil)
}

// A runner that ends a session (refusing to start it) sends an End frame
// before closing the stream; the mux keeps its reason, so an opener the
// server could not attach in time is told it rather than internal_error.
func TestMuxKeepsTheRunnersEndReason(t *testing.T) {
	mux, runner, _ := newStampMux(t)
	if _, ok := mux.EndReason(); ok {
		t.Fatal("a fresh mux reports an end")
	}
	runner.QueueRead(encodeFrame(frame.FrameType_Control, endControl(-1, "prompt_unsupported: profile bash has no {prompt} in its interactive template")))
	waitFor(t, func() bool { _, ok := mux.EndReason(); return ok })
	if reason, _ := mux.EndReason(); reason != "prompt_unsupported: profile bash has no {prompt} in its interactive template" {
		t.Fatalf("reason %q", reason)
	}
}
