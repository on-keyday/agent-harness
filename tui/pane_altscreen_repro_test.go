package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/vt"
)

// GROUND TRUTH, not a theory: does Render() (CellAt-based crop) show content
// that a full-screen (alt-screen) app drew, the same content emu.String()
// shows? snapshot uses emu.String() and renders fine; the grid pane uses
// Render() and goes black. If CellAt reads a different buffer than String when
// on the alt screen, Render is empty while String is not — that's the black.
func TestPaneStreamer_AltScreenRenderVsString(t *testing.T) {
	emu := vt.NewEmulator(80, 24)
	go drainEmu(emu)

	// Mimic a full-screen app (claude): enter alt screen, home, draw content.
	emu.Write([]byte("\x1b[?1049h")) // enter alternate screen buffer
	emu.Write([]byte("\x1b[2J\x1b[H"))
	emu.Write([]byte("HELLO_ALT_SCREEN"))

	full := emu.String()
	t.Logf("emu.String() =\n%q", full)

	p := &PaneStreamer{emu: emu, cols: 80, rows: 24}
	rendered := p.Render(80, 24)
	t.Logf("Render() =\n%q", rendered)

	lc := lastContentRow(emu, 80, 24)
	cur := emu.CursorPosition()
	t.Logf("lastContentRow=%d cursor=(%d,%d)", lc, cur.X, cur.Y)

	if strings.Contains(full, "HELLO_ALT_SCREEN") && !strings.Contains(rendered, "HELLO_ALT_SCREEN") {
		t.Fatalf("REPRO: String() shows alt-screen content but Render()/CellAt does NOT — this is the black pane")
	}
	if !strings.Contains(full, "HELLO_ALT_SCREEN") {
		t.Fatalf("setup wrong: even String() lacks the content: %q", full)
	}
}

// Control: primary-screen content must render (this is bash / the case that
// never went black on Linux).
func TestPaneStreamer_PrimaryScreenRenders(t *testing.T) {
	emu := vt.NewEmulator(80, 24)
	go drainEmu(emu)
	emu.Write([]byte("HELLO_PRIMARY"))
	p := &PaneStreamer{emu: emu, cols: 80, rows: 24}
	if !strings.Contains(p.Render(80, 24), "HELLO_PRIMARY") {
		t.Fatalf("primary-screen content must render, got %q", p.Render(80, 24))
	}
}
