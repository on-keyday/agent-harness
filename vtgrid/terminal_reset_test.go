//go:build !js

package vtgrid

import (
	"testing"

	agentexec "github.com/on-keyday/objtrsf/exec"
)

// The reset a client writes when it hands the terminal back (objtrsf
// WriteTerminalReset) must leave the cursor where it was. Its scroll-region and
// origin-mode resets each home the cursor as a side effect, so on a terminal at
// defaults — `exec -t`'s child exited, a shell session was detached — the next
// thing the caller prints landed on row 1, over what was already there.
func TestTerminalResetLeavesTheCursorWhereItWas(t *testing.T) {
	term := New(40, 10)
	_, _ = term.Write([]byte("one\r\ntwo\r\n$ "))
	x0, y0, _ := term.Cursor()

	agentexec.WriteTerminalReset(term)

	if x, y, _ := term.Cursor(); x != x0 || y != y0 {
		t.Fatalf("cursor moved from (%d,%d) to (%d,%d): the next prompt would overwrite row %d", x0, y0, x, y, y+1)
	}
}

// And it still does what it is for: a scroll region an app left behind is
// cleared, so cursor addressing is absolute again.
//
// Origin mode is checked by its EFFECT, not its flag. The cursor restore that
// keeps the position also restores the origin mode saved with it, so a mode an
// app left on stays on — and with the scroll region back to the full screen, on
// and off address the same cells. What must hold is that `CUP 1;1` lands on the
// real top-left.
func TestTerminalResetStillClearsALeftoverScrollRegion(t *testing.T) {
	term := New(40, 10)
	_, _ = term.Write([]byte("\x1b[3;6r\x1b[?6h"))

	agentexec.WriteTerminalReset(term)

	if term.scr.top != 0 || term.scr.bottom != 9 {
		t.Errorf("scroll region = %d..%d, want the full 0..9", term.scr.top, term.scr.bottom)
	}
	_, _ = term.Write([]byte("\x1b[1;1H"))
	if x, y, _ := term.Cursor(); x != 0 || y != 0 {
		t.Errorf("CUP 1;1 after the reset went to (%d,%d), want (0,0)", x, y)
	}
}
