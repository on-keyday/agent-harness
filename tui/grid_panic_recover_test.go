package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/vt"
)

// A VT panic during one feed (stale scroll region taller than the buffer) must
// not leave the pane permanently black: feed resets the scroll region on
// recover, so the NEXT chunk of output renders. Regression for the reported
// "reopen shows black panes that never recover".
func TestPaneStreamer_NotStuckBlackAfterPanic(t *testing.T) {
	p := &PaneStreamer{emu: vt.NewEmulator(80, 24), cols: 80, rows: 24}
	defer p.emu.Close()

	// Trigger the panic (region 1..58 on a 24-row buffer, then reverseIndex).
	nasty := []byte("\x1b[1;58r\x1b[1;1H")
	for i := 0; i < 70; i++ {
		nasty = append(nasty, "\x1bM"...)
	}
	if !p.feed(nasty, 0, 0, false) {
		t.Fatal("feed reported the emulator torn down")
	}
	// Subsequent output must render (emulator not stuck).
	p.feed([]byte("\r\nAFTER_PANIC_VISIBLE line\r\nsecond line\r\n"), 0, 0, false)
	out := p.Render(80, 10)
	if !strings.Contains(out, "AFTER_PANIC_VISIBLE") {
		t.Fatalf("pane stuck black after a recovered panic (no scroll-region reset): %q", out)
	}
}
