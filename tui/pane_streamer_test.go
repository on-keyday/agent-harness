package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/vt"
)

// bottomLeftCrop renders the bottom-left region: given a 3-row emulator with
// text on the last line, a 1-row crop must show that last line.
func TestPaneStreamer_RenderBottomLeftCrop(t *testing.T) {
	p := &PaneStreamer{emu: vt.NewEmulator(20, 3), cols: 20, rows: 3}
	go drainEmu(p.emu) // mirror the production io.Copy(io.Discard, emu) drain
	p.emu.Write([]byte("top\r\nmid\r\nbottom"))

	out := p.Render(20, 1)
	if !strings.Contains(out, "bottom") {
		t.Fatalf("1-row crop must show the bottom line, got %q", out)
	}
	if strings.Contains(out, "top") {
		t.Fatalf("1-row crop must NOT show the top line, got %q", out)
	}
}

// A crop wider/taller than the grid renders the whole grid without panicking on
// out-of-range CellAt (nil cells render as blanks).
func TestPaneStreamer_RenderLargerThanGrid(t *testing.T) {
	p := &PaneStreamer{emu: vt.NewEmulator(5, 2), cols: 5, rows: 2}
	go drainEmu(p.emu)
	p.emu.Write([]byte("ab\r\ncd"))
	out := p.Render(10, 5) // larger than 5x2
	if !strings.Contains(out, "ab") || !strings.Contains(out, "cd") {
		t.Fatalf("full grid must be visible, got %q", out)
	}
}

func drainEmu(emu *vt.Emulator) {
	buf := make([]byte, 4096)
	for {
		if _, err := emu.Read(buf); err != nil {
			return
		}
	}
}
