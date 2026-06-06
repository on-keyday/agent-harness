package server

import (
	"bytes"
	"context"
	"testing"
	"time"
)

func TestModeTracker_BasicAndLastValueWins(t *testing.T) {
	tr := newModeTracker()
	tr.feed([]byte("hello\x1b[?25lworld")) // hide cursor amid normal output
	if got := tr.preamble(); !bytes.Equal(got, []byte("\x1b[?25l")) {
		t.Fatalf("preamble = %q, want ESC[?25l", got)
	}
	tr.feed([]byte("\x1b[?25h")) // show again → most recent value wins
	if got := tr.preamble(); !bytes.Equal(got, []byte("\x1b[?25h")) {
		t.Fatalf("preamble after show = %q, want ESC[?25h", got)
	}
}

func TestModeTracker_AltScreenAndSyncExcluded(t *testing.T) {
	tr := newModeTracker()
	tr.feed([]byte("\x1b[?25l\x1b[?1049h\x1b[?2026h")) // cursor + alt-screen + sync
	// Only the cursor bit is content-independent and replayable.
	if got := tr.preamble(); !bytes.Equal(got, []byte("\x1b[?25l")) {
		t.Fatalf("preamble = %q, want only ESC[?25l (alt-screen/sync excluded)", got)
	}
}

func TestModeTracker_MultiParamAndAscendingOrder(t *testing.T) {
	tr := newModeTracker()
	tr.feed([]byte("\x1b[?1000;1006h\x1b[?7l")) // 1000=set, 1006=set, 7=reset
	want := []byte("\x1b[?7l\x1b[?1000h\x1b[?1006h")
	if got := tr.preamble(); !bytes.Equal(got, want) {
		t.Fatalf("preamble = %q, want %q", got, want)
	}
}

func TestModeTracker_SequenceSplitAcrossFeeds(t *testing.T) {
	tr := newModeTracker()
	tr.feed([]byte("\x1b[?2")) // sequence cut mid-parameter
	tr.feed([]byte("5l"))      // completed on the next chunk
	if got := tr.preamble(); !bytes.Equal(got, []byte("\x1b[?25l")) {
		t.Fatalf("split-feed preamble = %q, want ESC[?25l", got)
	}
}

func TestModeTracker_NonPrivateCSIIgnored(t *testing.T) {
	tr := newModeTracker()
	tr.feed([]byte("\x1b[1;31m\x1b[2J")) // SGR + erase: not DEC private modes
	if got := tr.preamble(); got != nil {
		t.Fatalf("non-private CSI produced preamble %q, want nil", got)
	}
}

// TestSessionMux_AttachRestoresEvictedCursorMode is the regression for the
// real symptom: a mode-setting sequence (cursor hide) scrolls out of the ring
// window, yet a reattach must still re-establish it so the new emulator doesn't
// show a stray cursor.
func TestSessionMux_AttachRestoresEvictedCursorMode(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	runner := newFakeStream(t)
	// Ring small enough that the bulk frame evicts the cursor-hide frame.
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(16), SessionHooks{})

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

	// Replay must be: a synthesised stdout frame re-hiding the cursor, then the
	// surviving bulk frame.
	wantPreamble := makeWireFrame(1, []byte("\x1b[?25l"))
	want := append(append([]byte{}, wantPreamble...), bulk...)
	got := tui.WaitWritten(t, len(want))
	if !bytes.Equal(got, want) {
		t.Fatalf("replay\n got=%q\nwant=%q", got, want)
	}
}
