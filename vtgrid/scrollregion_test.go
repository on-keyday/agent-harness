package vtgrid

// The stale-scroll-region hazard, which is the reason tui/pane_streamer.go
// carries a recover() around every write, a vtPanics counter, and an `ESC [ r`
// injection after each resize: x/vt does not clamp a scroll region when the
// buffer shrinks under it, and the next scroll indexes past the end. Its
// comment there records the consequence — left alone the emulator panics on
// every subsequent scroll and "the pane goes PERMANENTLY BLACK".
//
// A pane in the TUI's grid resizes whenever the operator resizes their
// terminal, and a full-screen app sets a scroll region as a matter of course,
// so the two meet often. These tests pin the behaviour of both implementations
// so that replacing one with the other is a decision made against evidence.

import (
	"io"
	"testing"

	"github.com/charmbracelet/x/vt"
)

// staleRegionThenScroll is the sequence: set a scroll region, shrink the
// buffer under it, then force a scroll inside what is now out of bounds.
func staleRegionThenScroll(w io.Writer) {
	_, _ = w.Write([]byte("\x1b[5;20r")) // DECSTBM rows 5..20
	_, _ = w.Write([]byte("\x1b[10;1H")) // park the cursor inside it
}

func TestResizeClampsScrollRegion(t *testing.T) {
	term := New(80, 24)
	staleRegionThenScroll(term)
	term.Resize(80, 10) // bottom margin 19 is now past the last row, 9

	// Scroll hard enough that an unclamped bottom margin would index out of
	// range, and print afterwards so a silent no-op cannot pass either.
	for i := 0; i < 40; i++ {
		_, _ = term.Write([]byte("line\r\n"))
	}
	_, _ = term.Write([]byte("\x1b[1;1Halive"))

	if got := term.Lines()[0]; got[:5] != "alive" {
		t.Fatalf("row 0 = %q, want it to start with \"alive\"", got)
	}
	if term.scr.bottom != term.rows-1 || term.scr.top != 0 {
		t.Errorf("scroll region survived the resize as [%d,%d]; resize must reset it to [0,%d]",
			term.scr.top, term.scr.bottom, term.rows-1)
	}
}

// TestOracleResizeLeavesStaleScrollRegion documents what the other
// implementation does with the same input.
//
// It does NOT reproduce the panic. Seven variations were tried against x/vt
// v0.0.0-20260622092256 — reverse index at the region top and at row 1, SU,
// SD, insert-line inside the stale region, a region ending at the last row,
// and a region taller than the buffer with no resize at all — and none of them
// panicked. So the hazard tui/pane_streamer.go guards against is either fixed
// upstream since that code was written or has a trigger not found here, and
// this test cannot be used to argue that vtgrid removes a live danger.
//
// It is kept as a tripwire rather than deleted: it records what was tried, so
// the next person does not repeat the search, and it will start reporting a
// panic again if one returns.
func TestOracleResizeLeavesStaleScrollRegion(t *testing.T) {
	emu := vt.NewEmulator(80, 24)
	done := make(chan struct{})
	go func() { defer close(done); _, _ = io.Copy(io.Discard, emu) }()
	defer func() { _ = emu.Close(); <-done }()

	staleRegionThenScroll(emu)
	emu.Resize(80, 10)

	panicked := func() (p bool) {
		defer func() {
			if r := recover(); r != nil {
				p = true
			}
		}()
		for i := 0; i < 40; i++ {
			_, _ = emu.Write([]byte("line\r\n"))
		}
		return false
	}()

	if !panicked {
		t.Log("x/vt survived a shrink under a stale scroll region, as it did for " +
			"the other six variations tried — the pane_streamer.go hazard is not " +
			"reproducible at this version by this route")
		return
	}
	t.Log("x/vt panicked — the hazard tui/pane_streamer.go recovers from is live " +
		"again; vtgrid clamps the region on resize and does not share it")
}
