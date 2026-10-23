package server

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/exec/frame"
)

// TestSessionMux_AttachRestoresEvictedCursorMode is the regression for the
// real symptom: a mode-setting sequence (cursor hide) scrolls out of the ring
// window, yet a reattach must still re-establish it so the new emulator doesn't
// show a stray cursor.
func TestSessionMux_AttachRestoresEvictedCursorMode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	runner := newFakeStream(t)
	// Ring small enough that the bulk frame evicts the cursor-hide frame.
	mux := NewSessionMux(ctx, "task", protocol.TaskKind_Interactive, runner, NewRingBuffer(16), SessionHooks{})

	hide := makeWireFrame(1, []byte("\x1b[?25l"))  // 6-byte payload → 11-byte frame
	bulk := makeWireFrame(1, []byte("AAAAAAAAAA")) // 10-byte payload → 15-byte frame

	runner.QueueRead(hide)
	waitFor(t, func() bool { return mux.RingBufferLen() == len(hide) })
	runner.QueueRead(bulk)
	// bulk (15) + hide (11) > cap (16) → hide is evicted, ring holds only bulk.
	waitFor(t, func() bool { return mux.RingBufferLen() == len(bulk) })

	tui := newFakeStream(t)
	if err := mux.Attach(ctx, tui); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// Replay must re-hide the cursor and then deliver the surviving bulk frame.
	//
	// The preamble is a SYNTH frame, not a Stdout one, and that is the point of
	// asserting its type here rather than filtering it out: it is a byte
	// sequence this server invented, and `session snapshot --raw` promises to
	// show only what the PTY produced.
	wantPreamble := makeWireFrame(byte(frame.FrameType_Synth), []byte("\x1b[?25l"))
	got := tui.WaitWritten(t, len(wantPreamble)+len(bulk))
	if !bytes.HasPrefix(got, wantPreamble) {
		t.Fatalf("replay does not start with a synth-framed cursor preamble\n got=%q\nwant prefix=%q", got, wantPreamble)
	}
	if pty := stripSynth(t, got); !bytes.Equal(pty, bulk) {
		t.Fatalf("PTY bytes in the replay\n got=%q\nwant=%q", pty, bulk)
	}
}
