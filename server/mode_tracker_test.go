package server

import (
	"bytes"
	"context"
	"strings"
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

func TestModeTracker_AltScreenLiveReentered(t *testing.T) {
	tr := newModeTracker()
	tr.feed([]byte("\x1b[?25l\x1b[?1049h\x1b[?2026h")) // cursor + alt-screen + sync
	// Session is currently in the alt screen → the preamble must re-enter it (so
	// a reattach lands the live app's frames on the alt buffer even when the
	// original ESC[?1049h was evicted). Sync (2026) stays excluded (transient
	// framing).
	//
	// The ORDER around that entry is the load-bearing part, and it is asserted
	// here rather than described in a comment because it looks wrong: the leave
	// is not redundant and the cursor mode is not misplaced. A real Windows
	// console freezes the alternate buffer's cursor visibility at whatever the
	// primary buffer held when ESC[?1049h ran, and DECTCEM issued from inside
	// the alternate buffer never reaches it. So the cursor bit has to be set
	// while the primary buffer is current and then carried across a switch that
	// actually happens — hence leave, set, enter. Against a client already on
	// the alternate buffer (a reattach into the same terminal) the leading
	// ESC[?1049l is what keeps the entry from degenerating into a no-op; against
	// one on the primary buffer it is inert.
	want := []byte("\x1b[?1049l\x1b[?25l\x1b[?1049h")
	if got := tr.preamble(); !bytes.Equal(got, want) {
		t.Fatalf("preamble = %q, want %q (leave, hide, enter; sync excluded)", got, want)
	}
}

func TestModeTracker_AltScreenExitedNotReentered(t *testing.T) {
	tr := newModeTracker()
	// Full-screen app entered then exited the alt screen; cursor shown again.
	tr.feed([]byte("\x1b[?1049h\x1b[?25l\x1b[?25h\x1b[?1049l"))
	// On the primary screen the preamble must NOT emit ESC[?1049h (nor a stray
	// ESC[?1049l) — only the surviving content-independent mode (cursor shown).
	if got := tr.preamble(); !bytes.Equal(got, []byte("\x1b[?25h")) {
		t.Fatalf("preamble = %q, want only ESC[?25h (no alt-screen re-entry on primary)", got)
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

// TestPreambleEmitsTheLeaveOnlyWhenItCarriesSomething pins the guarantee that
// the fix costs nothing when it buys nothing: the forced ESC[?1049l exists only
// to give a tracked cursor bit a real switch to ride, so a preamble with no
// DECTCEM to hand over must not drag the client through a screen switch.
func TestPreambleEmitsTheLeaveOnlyWhenItCarriesSomething(t *testing.T) {
	const leave = "\x1b[?1049l"

	t.Run("alt screen with a tracked cursor bit", func(t *testing.T) {
		tr := newModeTracker()
		tr.feed([]byte("\x1b[?1049h\x1b[?25l"))
		pre := string(tr.preamble())
		if !strings.Contains(pre, leave) {
			t.Fatalf("preamble = %q, want it to contain the leave %q", pre, leave)
		}
		// And the leave has to come before the cursor bit, which has to come
		// before the entry. That is the whole mechanism.
		iLeave := strings.Index(pre, leave)
		iCur := strings.Index(pre, "\x1b[?25l")
		iEnter := strings.Index(pre, "\x1b[?1049h")
		if !(iLeave < iCur && iCur < iEnter) {
			t.Errorf("order is leave=%d cursor=%d enter=%d in %q; want leave < cursor < enter",
				iLeave, iCur, iEnter, pre)
		}
	})

	t.Run("alt screen with no tracked cursor bit", func(t *testing.T) {
		tr := newModeTracker()
		tr.feed([]byte("\x1b[?1049h"))
		pre := tr.preamble()
		if bytes.Contains(pre, []byte(leave)) {
			t.Errorf("preamble = %q emitted a screen switch with no cursor bit to carry", pre)
		}
	})
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
