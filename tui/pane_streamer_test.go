package tui

import (
	"strings"
	"sync"
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

// A second Stop() must not panic. The pre-fix drainC channel was closed once
// in Stop and never re-nil'd, so a repeat Stop() called close() on an
// already-closed channel: panic: close of closed channel. Removing drainC in
// favor of the io.Copy(io.Discard, emu) sibling idiom (cli/snapshot_native.go)
// and nil-guarding cancel/stream/emu makes a repeat Stop() a no-op.
func TestPaneStreamer_StopIdempotent(t *testing.T) {
	p := NewPaneStreamer("x", 24, 80)
	p.Stop()
	p.Stop() // must not panic
}

// Render must hold p.mu across the whole CellAt scan, not just the
// emu/cols/rows snapshot, because vt.Emulator has no internal locking of its
// own. The pump path writes to the emulator while holding p.mu (mirrored here
// by the writer goroutine); an unlocked Render loop racing that Write is only
// visible under -race. This test is meaningful ONLY with `go test -race`.
func TestPaneStreamer_RenderRaceWithWrite(t *testing.T) {
	p := &PaneStreamer{emu: vt.NewEmulator(20, 3), cols: 20, rows: 3}
	go drainEmu(p.emu)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			p.mu.Lock()
			p.emu.Write([]byte("data\r\n"))
			p.mu.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			p.Render(20, 3)
		}
	}()
	wg.Wait()
}

func drainEmu(emu *vt.Emulator) {
	buf := make([]byte, 4096)
	for {
		if _, err := emu.Read(buf); err != nil {
			return
		}
	}
}
