package termmode

import (
	"bytes"
	"strings"
	"testing"
)

func TestModeTracker_BasicAndLastValueWins(t *testing.T) {
	tr := New()
	tr.Feed([]byte("hello\x1b[?25lworld")) // hide cursor amid normal output
	if got := tr.Preamble(); !bytes.Equal(got, []byte("\x1b[?25l")) {
		t.Fatalf("preamble = %q, want ESC[?25l", got)
	}
	tr.Feed([]byte("\x1b[?25h")) // show again → most recent value wins
	if got := tr.Preamble(); !bytes.Equal(got, []byte("\x1b[?25h")) {
		t.Fatalf("preamble after show = %q, want ESC[?25h", got)
	}
}

func TestModeTracker_AltScreenLiveReentered(t *testing.T) {
	tr := New()
	tr.Feed([]byte("\x1b[?25l\x1b[?1049h\x1b[?2026h")) // cursor + alt-screen + sync
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
	if got := tr.Preamble(); !bytes.Equal(got, want) {
		t.Fatalf("preamble = %q, want %q (leave, hide, enter; sync excluded)", got, want)
	}
}

func TestModeTracker_AltScreenExitedNotReentered(t *testing.T) {
	tr := New()
	// Full-screen app entered then exited the alt screen; cursor shown again.
	tr.Feed([]byte("\x1b[?1049h\x1b[?25l\x1b[?25h\x1b[?1049l"))
	// On the primary screen the preamble must NOT emit ESC[?1049h (nor a stray
	// ESC[?1049l) — only the surviving content-independent mode (cursor shown).
	if got := tr.Preamble(); !bytes.Equal(got, []byte("\x1b[?25h")) {
		t.Fatalf("preamble = %q, want only ESC[?25h (no alt-screen re-entry on primary)", got)
	}
}

func TestModeTracker_MultiParamAndAscendingOrder(t *testing.T) {
	tr := New()
	tr.Feed([]byte("\x1b[?1000;1006h\x1b[?7l")) // 1000=set, 1006=set, 7=reset
	want := []byte("\x1b[?7l\x1b[?1000h\x1b[?1006h")
	if got := tr.Preamble(); !bytes.Equal(got, want) {
		t.Fatalf("preamble = %q, want %q", got, want)
	}
}

func TestModeTracker_SequenceSplitAcrossFeeds(t *testing.T) {
	tr := New()
	tr.Feed([]byte("\x1b[?2")) // sequence cut mid-parameter
	tr.Feed([]byte("5l"))      // completed on the next chunk
	if got := tr.Preamble(); !bytes.Equal(got, []byte("\x1b[?25l")) {
		t.Fatalf("split-feed preamble = %q, want ESC[?25l", got)
	}
}

func TestModeTracker_NonPrivateCSIIgnored(t *testing.T) {
	tr := New()
	tr.Feed([]byte("\x1b[1;31m\x1b[2J")) // SGR + erase: not DEC private modes
	if got := tr.Preamble(); got != nil {
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
		tr := New()
		tr.Feed([]byte("\x1b[?1049h\x1b[?25l"))
		pre := string(tr.Preamble())
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
		tr := New()
		tr.Feed([]byte("\x1b[?1049h"))
		pre := tr.Preamble()
		if bytes.Contains(pre, []byte(leave)) {
			t.Errorf("preamble = %q emitted a screen switch with no cursor bit to carry", pre)
		}
	})
}

func TestMode_UnseenSetAndReset(t *testing.T) {
	tr := New()
	if set, seen := tr.Mode(BracketedPaste); set || seen {
		t.Fatalf("Mode before any output = (%v, %v), want (false, false)", set, seen)
	}
	tr.Feed([]byte("\x1b[?2004h"))
	if set, seen := tr.Mode(BracketedPaste); !set || !seen {
		t.Fatalf("Mode after ESC[?2004h = (%v, %v), want (true, true)", set, seen)
	}
	tr.Feed([]byte("\x1b[?2004l"))
	if set, seen := tr.Mode(BracketedPaste); set || !seen {
		t.Fatalf("Mode after ESC[?2004l = (%v, %v), want (false, true)", set, seen)
	}
}

// Modes is what follows a vtgrid Repaint, which already selects the screen and
// sets DECTCEM: neither may appear in it, whatever the program last did.
func TestModes_IsPreambleWithoutTheScreenHead(t *testing.T) {
	tr := New()
	tr.Feed([]byte("\x1b[?1049h\x1b[?25l\x1b[?2004h\x1b[?1h"))
	want := []byte("\x1b[?1h\x1b[?2004h")
	if got := tr.Modes(); !bytes.Equal(got, want) {
		t.Fatalf("Modes = %q, want %q", got, want)
	}
	if pre := tr.Preamble(); !bytes.HasSuffix(pre, want) {
		t.Fatalf("Preamble %q does not end with Modes %q", pre, want)
	}
	if got := New().Modes(); got != nil {
		t.Fatalf("Modes with nothing tracked = %q, want nil", got)
	}
}
