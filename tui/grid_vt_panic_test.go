package tui

import (
	"testing"

	"github.com/charmbracelet/x/vt"
)

// The reported Windows crash: a scroll region whose bottom margin exceeds the
// buffer height, then reverseIndex (ESC M), makes raw emu.Write panic with
// index-out-of-range inside x/vt ScrollDown. feed() must swallow it so a single
// pane's bad byte stream never crashes the whole TUI.
func TestPaneStreamer_FeedRecoversVTPanic(t *testing.T) {
	p := &PaneStreamer{emu: vt.NewEmulator(80, 24), cols: 80, rows: 24}
	defer p.emu.Close()

	nasty := []byte("\x1b[1;58r\x1b[1;1H")
	for i := 0; i < 70; i++ {
		nasty = append(nasty, "\x1bM"...) // reverseIndex, scrolls the oversized region
	}
	if alive := p.feed(nasty, 0, 0, false); !alive {
		t.Fatal("feed wrongly reported a torn-down emulator")
	}
	// still usable after the recovered panic
	if alive := p.feed([]byte("still alive"), 0, 0, false); !alive {
		t.Fatal("feed dead after recovering a VT panic")
	}
	// Reaching here means no panic escaped feed() — that is the assertion.
}

// A resize resets the scroll region so a stale (too-tall) region can't later
// panic on scroll.
func TestPaneStreamer_FeedResizeResetsScrollRegion(t *testing.T) {
	p := &PaneStreamer{emu: vt.NewEmulator(80, 60), cols: 80, rows: 60}
	defer p.emu.Close()

	p.feed([]byte("\x1b[1;58r"), 0, 0, false) // region valid at 60 rows
	if !p.feed([]byte("x"), 24, 80, true) {    // shrink to 24 (feed emits ESC[r)
		t.Fatal("feed reported dead on resize")
	}
	if p.rows != 24 || p.cols != 80 {
		t.Fatalf("resize not applied: %dx%d", p.cols, p.rows)
	}
	// reverseIndex now must not blow up (region was reset to 1..24)
	if !p.feed([]byte("\x1b[1;1H\x1bM\x1bM\x1bM"), 0, 0, false) {
		t.Fatal("feed reported dead after resize+reverseIndex")
	}
}
