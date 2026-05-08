package server

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// TestSessionMux_DetachReattach_ForwardsOutput exercises the user-reported
// scenario at the SessionMux layer with fake streams: detach (0 attached)
// → reattach must replay buffered frames AND forward subsequent live
// frames to the new tui. Takeover (>=1 attached → reattach) is covered by
// TestSessionMux_AttachTakeover; this test is specifically the
// "0-attached interlude" variant.
func TestSessionMux_DetachReattach_ForwardsOutput(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	runnerStream := newFakeStream(t)
	mux := NewSessionMux(ctx, "task-X", runnerStream, NewRingBuffer(4096), SessionHooks{})
	defer mux.Stop()

	frameAlpha := makeWireFrame(1, []byte("alpha"))
	frameBeta := makeWireFrame(1, []byte("beta"))
	frameGamma := makeWireFrame(1, []byte("gamma"))

	// Initial attach.
	tui1 := newFakeStream(t)
	if err := mux.Attach(ctx, tui1); err != nil {
		t.Fatalf("first Attach: %v", err)
	}

	// Pre-detach runner output reaches tui1 + ring.
	runnerStream.QueueRead(frameAlpha)
	tui1.WaitWritten(t, len(frameAlpha))

	// Detach.
	tui1.CloseRead()
	waitFor(t, func() bool { return !mux.IsAttached() })

	// Post-detach runner output goes to ring only (tui is nil).
	runnerStream.QueueRead(frameBeta)
	waitFor(t, func() bool { return mux.RingBufferLen() == len(frameAlpha)+len(frameBeta) })

	// Reattach with a fresh tui.
	tui2 := newFakeStream(t)
	if err := mux.Attach(ctx, tui2); err != nil {
		t.Fatalf("reattach: %v", err)
	}

	// Replay should land first: frameAlpha ++ frameBeta.
	expectedReplay := append(append([]byte{}, frameAlpha...), frameBeta...)
	got := tui2.WaitWritten(t, len(expectedReplay))
	if !bytes.Equal(got, expectedReplay) {
		t.Fatalf("replay got %q want %q", got, expectedReplay)
	}

	// Post-reattach LIVE frame must reach tui2.
	runnerStream.QueueRead(frameGamma)
	expectedAll := append(append([]byte{}, expectedReplay...), frameGamma...)
	got2 := tui2.WaitWritten(t, len(expectedAll))
	if !bytes.Equal(got2, expectedAll) {
		t.Fatalf("post-reattach forward: got %q want %q", got2, expectedAll)
	}
}
