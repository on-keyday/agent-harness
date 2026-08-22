//go:build !js

package cli

// Throughput and footprint of feeding PTY output through the headless VT
// emulator. This is a sizing measurement for the open question of whether the
// SERVER can afford to hold a live emulator per session (today only clients
// build one — cli/snapshot_native.go and tui/pane_streamer.go — while the
// server keeps a byte ring plus two partial scanners: server/mode_tracker.go
// and server/replay_ground_scan.go).
//
// The two knobs that matter are the corpus shape (a shell scrolling text vs a
// full-screen TUI repainting absolute-positioned frames) and the scrollback
// size — a server-side emulator would run with scrollback disabled, because
// the 1 MiB replay ring already IS the scrollback.
//
// VTBENCH_CORPUS points at a captured PTY byte stream (harness-cli session
// snapshot --raw); the TUI cases skip without it.

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/charmbracelet/x/vt"
)

// benchCols/benchRows match a typical attached terminal, and the size the
// herdr comparison was measured at.
const (
	benchCols = 146
	benchRows = 35

	// scrollbackOff is the smallest scrollback x/vt will accept. Scrollback
	// cannot be turned OFF: NewScrollback substitutes DefaultScrollbackSize
	// for any maxLines <= 0, and SetMaxLines returns without doing anything
	// for the same range (scrollback.go). Passing 0 therefore measures the
	// 10k default while claiming not to — 1 is the real floor.
	scrollbackOff = 1
)

// plainCorpus is a shell scrolling plain text: `seq 1 200000` as a PTY writes
// it (bare CR before each LF). No escape sequences at all — the cheap end.
func plainCorpus() []byte {
	var b strings.Builder
	b.Grow(1 << 21)
	for i := 1; i <= 200000; i++ {
		fmt.Fprintf(&b, "%d\r\n", i)
	}
	return []byte(b.String())
}

// tuiCorpus is real captured PTY bytes — the expensive end, where every frame
// is absolute cursor positioning plus SGR runs.
func tuiCorpus(tb testing.TB) []byte {
	path := os.Getenv("VTBENCH_CORPUS")
	if path == "" {
		tb.Skip("VTBENCH_CORPUS unset; capture with `harness-cli session snapshot --raw <id> > corpus`")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		tb.Fatalf("read corpus: %v", err)
	}
	return b
}

// newBenchEmulator builds an emulator with the given scrollback budget and a
// drain on its response side. Full-screen apps emit DA1/DA2/DSR queries and
// x/vt answers by writing to its own output; with nobody reading, Write blocks
// forever (same reason collectScreen drains).
func newBenchEmulator(scrollback int) (*vt.Emulator, func()) {
	emu := vt.NewEmulator(benchCols, benchRows)
	emu.Scrollback().SetMaxLines(scrollback)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = io.Copy(io.Discard, emu)
	}()
	return emu, func() {
		_ = emu.Close()
		<-done
	}
}

func benchFeed(b *testing.B, corpus []byte, scrollback int) {
	emu, stop := newBenchEmulator(scrollback)
	defer stop()
	b.SetBytes(int64(len(corpus)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := emu.Write(corpus); err != nil {
			b.Fatalf("write: %v", err)
		}
	}
}

func BenchmarkVTFeedPlainMinScrollback(b *testing.B) { benchFeed(b, plainCorpus(), scrollbackOff) }
func BenchmarkVTFeedPlainScrollback10k(b *testing.B) {
	benchFeed(b, plainCorpus(), vt.DefaultScrollbackSize)
}
func BenchmarkVTFeedTUIMinScrollback(b *testing.B) { benchFeed(b, tuiCorpus(b), scrollbackOff) }
func BenchmarkVTFeedTUIScrollback10k(b *testing.B) {
	benchFeed(b, tuiCorpus(b), vt.DefaultScrollbackSize)
}

// TestVTEmulatorFootprint reports the retained heap cost of holding N live
// emulators, which is what a server would pay per detachable session. It
// asserts nothing — the number is the output.
func TestVTEmulatorFootprint(t *testing.T) {
	// Opt-in: the 10k-scrollback case retains ~3 GiB at once, which is a bad
	// thing to hand an unattended `make test` on a swapless box. It is a
	// measurement, not a regression guard — nothing here asserts.
	if os.Getenv("VTBENCH_FOOTPRINT") == "" {
		t.Skip("set VTBENCH_FOOTPRINT=1 to run (allocates ~3 GiB)")
	}
	// Filling costs ~60 µs per scrolled line (BenchmarkVTFeedShape), so the
	// filled cases use a smaller fleet and a corpus sized just past the 10k
	// scrollback bound instead of the 200k-line one.
	corpus := linesCorpus(12000, 100)

	measure := func(label string, n int, fill bool, scrollback int) {
		emus := make([]*vt.Emulator, 0, n)
		stops := make([]func(), 0, n)
		defer func() {
			for _, s := range stops {
				s()
			}
		}()

		runtime.GC()
		var before runtime.MemStats
		runtime.ReadMemStats(&before)

		for i := 0; i < n; i++ {
			emu, stop := newBenchEmulator(scrollback)
			if fill {
				if _, err := emu.Write(corpus); err != nil {
					t.Fatalf("write: %v", err)
				}
			}
			emus = append(emus, emu)
			stops = append(stops, stop)
		}

		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		runtime.KeepAlive(emus)

		delta := int64(after.HeapAlloc) - int64(before.HeapAlloc)
		t.Logf("%-28s %d emulators %dx%d: %.1f MiB total, %.1f kiB each",
			label, n, benchCols, benchRows,
			float64(delta)/(1<<20), float64(delta)/float64(n)/1024)
	}

	measure("empty, scrollback=1", 200, false, scrollbackOff)
	measure("empty, scrollback=10k", 200, false, vt.DefaultScrollbackSize)
	measure("fed 12k lines, scrollback=1", 25, true, scrollbackOff)
	measure("fed 12k lines, scrollback=10k", 25, true, vt.DefaultScrollbackSize)
}

// linesCorpus builds n lines of the given width (plus CRLF) — the same total
// byte count across variants with different scrolled-line counts, to separate
// per-byte cost from per-scroll cost.
func linesCorpus(n, width int) []byte {
	line := strings.Repeat("x", width)
	var b strings.Builder
	b.Grow(n * (width + 2))
	for i := 0; i < n; i++ {
		b.WriteString(line)
		b.WriteString("\r\n")
	}
	return []byte(b.String())
}

func BenchmarkVTFeedShape(b *testing.B) {
	// ~1.4 MB each way; only the number of screen scrolls differs.
	for _, c := range []struct {
		name         string
		lines, width int
	}{
		{"200000x5", 200000, 5},
		{"20000x68", 20000, 68},
		{"10000x138", 10000, 138},
	} {
		b.Run(c.name, func(b *testing.B) { benchFeed(b, linesCorpus(c.lines, c.width), scrollbackOff) })
	}
}
