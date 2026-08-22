package vtgrid

// The window title, which is not on the grid and so is not covered by the
// parity suite's row comparison.
//
// These cases moved here from cli when the snapshot path switched renderers.
// They used to test a hand-written byte scan that existed only because x/vt
// truncates a title at its first multi-byte character; the scan is gone, and
// what they pin now is that the parser gets the same answers directly.

import (
	"io"
	"testing"

	"github.com/charmbracelet/x/vt"
)

func titleOf(s string) string {
	t := New(80, 24)
	_, _ = t.Write([]byte(s))
	return t.Title()
}

func TestTitle(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"none", "plain text\r\n", ""},
		{"osc 2 as well as 0", "\x1b]2;from two\x07", "from two"},
		{"ST terminator", "\x1b]0;via st\x1b\\", "via st"},
		{"last one wins", "\x1b]0;first\x07junk\x1b]0;second\x07", "second"},
		// A capture is cut on a timer, so the tail can be half a sequence. Half
		// a title is worse than none: a rule would match half a glyph.
		{"unterminated tail is ignored", "\x1b]0;good\x07\x1b]0;cut off", "good"},
		// OSC 1 sets the ICON name, not the window title.
		{"osc 1 is not a title", "\x1b]0;title\x07\x1b]1;icon\x07", "title"},
		{"other osc kinds ignored", "\x1b]0;title\x07\x1b]52;c;Zm9v\x07", "title"},
		{"invalid utf-8 payload is dropped", "\x1b]0;ok\x07\x1b]0;\xe2\x07", "ok"},
		{"empty title is a title", "\x1b]0;x\x07\x1b]0;\x07", ""},
		{"no command digits", "\x1b];nope\x07", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := titleOf(tc.in); got != tc.want {
				t.Errorf("Title() after %q = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// realTitleOSC is the byte sequence that exposed the defect, lifted from a live
// capture: a complete, BEL-terminated 36-byte title whose first glyph is
// U+2733 ✳ — whose second byte is 0x9C.
const realTitleOSC = "\x1b]0;✳ herdr agent harness 参考要素\x07"

// TestTitleSurvivesAnEightBitSTLookalike is the regression test for the reason
// this parser refuses 0x9C as a terminator: in a UTF-8 stream that byte is a
// continuation byte, not a C1 control, and honouring it ends the sequence one
// byte into the title and prints the rest onto the grid.
//
// The oracle half is a negative control, so the test keeps meaning something:
// x/vt still gets it wrong. If that ever starts passing, x/vt was fixed and the
// entry in knownOracleDefects should go with it.
func TestTitleSurvivesAnEightBitSTLookalike(t *testing.T) {
	want := "✳ herdr agent harness 参考要素"
	if got := titleOf("\x1b[?25h" + realTitleOSC + "\x1b[?25l\x1b[2D"); got != want {
		t.Fatalf("Title() = %q, want %q", got, want)
	}

	emu := vt.NewEmulator(80, 24)
	done := make(chan struct{})
	go func() { defer close(done); _, _ = io.Copy(io.Discard, emu) }()
	var viaCallback string
	emu.SetCallbacks(vt.Callbacks{Title: func(s string) { viaCallback = s }})
	_, _ = emu.Write([]byte(realTitleOSC))
	_ = emu.Close()
	<-done

	switch {
	case viaCallback == want:
		t.Logf("x/vt now returns the whole title (%q) — check whether the "+
			"knownOracleDefects entry in diff_test.go can go too", viaCallback)
	case len(viaCallback) >= len(want):
		t.Errorf("x/vt returned %q — neither the old truncation nor the right "+
			"answer; the assumption behind knownOracleDefects needs re-checking", viaCallback)
	}
}
