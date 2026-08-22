package tui

import (
	"testing"

	"github.com/on-keyday/agent-harness/vtgrid"
)

// The reported Windows crash: a scroll region whose bottom margin exceeds the
// buffer height, then reverseIndex (ESC M), used to panic with
// index-out-of-range inside the emulator's ScrollDown. The screen model this
// pane now uses bounds the region instead, so the sequence is simply handled —
// but the assertion is unchanged and still worth making: whatever the model
// does with a hostile byte stream, nothing may escape feed() and take the TUI
// with it.
func TestPaneStreamer_FeedRecoversVTPanic(t *testing.T) {
	p := &PaneStreamer{emu: vtgrid.New(80, 24), cols: 80, rows: 24}

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

// A resize must leave no stale (too-tall) scroll region behind. The pane used
// to write `ESC[r` itself after resizing because the old emulator did not
// clamp; the model does, and this holds the outcome rather than the mechanism.
func TestPaneStreamer_FeedResizeResetsScrollRegion(t *testing.T) {
	p := &PaneStreamer{emu: vtgrid.New(80, 60), cols: 80, rows: 60}

	p.feed([]byte("\x1b[1;58r"), 0, 0, false) // region valid at 60 rows
	if !p.feed([]byte("x"), 24, 80, true) {   // shrink to 24
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
