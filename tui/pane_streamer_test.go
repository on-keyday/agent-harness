package tui

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/vtgrid"
)

// bottomLeftCrop renders the bottom-left region: given a 3-row emulator with
// text on the last line, a 1-row crop must show that last line.
func TestPaneStreamer_RenderBottomLeftCrop(t *testing.T) {
	p := &PaneStreamer{emu: vtgrid.New(20, 3), cols: 20, rows: 3}
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
	p := &PaneStreamer{emu: vtgrid.New(5, 2), cols: 5, rows: 2}
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
// emu/cols/rows snapshot, because the screen model has no internal locking of its
// own. The pump path writes to the emulator while holding p.mu (mirrored here
// by the writer goroutine); an unlocked Render loop racing that Write is only
// visible under -race. This test is meaningful ONLY with `go test -race`.
func TestPaneStreamer_RenderRaceWithWrite(t *testing.T) {
	p := &PaneStreamer{emu: vtgrid.New(20, 3), cols: 20, rows: 3}

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

// The cumulative counters cannot answer "is anything arriving NOW" — reading
// them needs two samples and a subtraction, on exactly the pane you are staring
// at because it looks stuck. These pin the window arithmetic that turns them
// into a rate; time is passed in, so none of it needs a clock.
func TestPaneStreamerRateRollsOnAFullWindow(t *testing.T) {
	p := NewPaneStreamer("t", 4, 20)
	t0 := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

	p.mu.Lock()
	defer p.mu.Unlock()

	// Nothing has ever arrived: zero, and it is a measurement rather than a gap.
	if bps, fps := p.rateLocked(t0); bps != 0 || fps != 0 {
		t.Fatalf("rate before any arrival = %v/%v, want 0/0", bps, fps)
	}

	// The window anchors on the FIRST arrival, not on attach: a pane that has
	// been silent for a minute must not have that minute averaged into its first
	// measurement. So these four 100-byte chunks span exactly one second FROM
	// the first of them, and the fourth is what rolls the window.
	for _, at := range []time.Duration{0, 250 * time.Millisecond, 500 * time.Millisecond, time.Second} {
		p.recordArrivalLocked(100, t0.Add(at))
	}
	bps, fps := p.rateLocked(t0.Add(time.Second))
	if bps != 400 || fps != 4 {
		t.Fatalf("rate = %vB/s %vrd/s, want 400/4", bps, fps)
	}
}

// A pane that STOPS producing must READ zero, and it must get there in one step.
//
// The first implementation divided the open window's count by a growing elapsed,
// which does reach zero — one render at a time, so a silent pane showed a number
// counting down forever and the operator saw the grid animating while nothing
// was happening. The value must therefore be STILL between rolls: a diagnostic
// that moves on its own cannot be read by eye.
func TestPaneStreamerRateHoldsStillThenDropsToZero(t *testing.T) {
	p := NewPaneStreamer("t", 4, 20)
	t0 := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

	p.mu.Lock()
	defer p.mu.Unlock()

	p.recordArrivalLocked(1000, t0)
	p.recordArrivalLocked(1000, t0.Add(time.Second)) // rolls: 2000 B over 1 s

	// Still, for every render inside the window after the last arrival. This is
	// the assertion the countdown bug fails: sampling at four instants must give
	// one value, not four.
	for _, at := range []time.Duration{0, 200 * time.Millisecond, 500 * time.Millisecond, 999 * time.Millisecond} {
		bps, fps := p.rateLocked(t0.Add(time.Second + at))
		if bps != 2000 || fps != 2 {
			t.Fatalf("at +%v the rate read %vB/s %vrd/s, want a still 2000/2", at, bps, fps)
		}
	}

	// Then one step to zero, and it stays there rather than drifting.
	for _, at := range []time.Duration{time.Second, 3 * time.Second, 60 * time.Second} {
		bps, fps := p.rateLocked(t0.Add(time.Second + at))
		if bps != 0 || fps != 0 {
			t.Errorf("after %v of silence = %v/%v, want 0/0", at, bps, fps)
		}
	}
}

// The countdown, exactly. It needs a burst SHORTER than a window: the window
// never rolls, so its byte count freezes while its elapsed keeps growing, and
// anything derived from the open window renders a smaller number on every tick.
// A window that HAS rolled hides this — its count is reset to zero, so the same
// arithmetic yields a steady 0 and the bug looks fixed.
//
// The invariant is the general one and does not name a value: between two
// arrivals, what the pane reports MUST NOT CHANGE. Nothing happened, so nothing
// may move.
func TestPaneStreamerRateDoesNotDriftBetweenArrivals(t *testing.T) {
	p := NewPaneStreamer("t", 4, 20)
	t0 := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

	p.mu.Lock()
	defer p.mu.Unlock()

	// A 50 KB burst delivered in 20 chunks over 100 ms — a replay burst, or one
	// screen repaint. Well under rateWindow, so no window completes.
	for i := 0; i < 20; i++ {
		p.recordArrivalLocked(2500, t0.Add(time.Duration(i)*5*time.Millisecond))
	}

	wantB, wantF := p.rateLocked(t0.Add(time.Second))
	for _, at := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 30 * time.Second} {
		bps, fps := p.rateLocked(t0.Add(at))
		if bps != wantB || fps != wantF {
			t.Fatalf("at +%v the rate moved to %vB/s %vrd/s from %v/%v with no arrival in between; "+
				"a value derived from the open window counts down on every render", at, bps, fps, wantB, wantF)
		}
	}
}

// Until the open window is a full rateWindow long, the last completed one
// stands: 2 frames in 40 ms is not 50 frames per second, and reporting it as one
// would make every brief burst look like a runaway pane.
func TestPaneStreamerRateDoesNotExtrapolateAShortWindow(t *testing.T) {
	p := NewPaneStreamer("t", 4, 20)
	t0 := time.Date(2026, 8, 23, 12, 0, 0, 0, time.UTC)

	p.mu.Lock()
	defer p.mu.Unlock()

	p.recordArrivalLocked(10, t0)
	p.recordArrivalLocked(10, t0.Add(time.Second)) // rolls: 20 B/s, 2 rd/s
	p.recordArrivalLocked(2, t0.Add(time.Second+40*time.Millisecond))

	bps, fps := p.rateLocked(t0.Add(time.Second + 40*time.Millisecond))
	if bps != 20 || fps != 2 {
		t.Fatalf("rate during a 40 ms window = %v/%v, want the completed window 20/2", bps, fps)
	}
}

// The overlay is what a screenshot carries, so the rate has to be IN it.
func TestPaneStreamerDiagLineCarriesTheRate(t *testing.T) {
	p := NewPaneStreamer("t", 4, 20)
	line := p.DiagLine()
	for _, want := range []string{"rx=", "rd=", "B/s", "rd/s"} {
		if !strings.Contains(line, want) {
			t.Errorf("DiagLine %q is missing %q", line, want)
		}
	}
}
