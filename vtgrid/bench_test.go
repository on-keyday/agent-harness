package vtgrid

// Cost of this model against the general-purpose emulator it could replace.
//
// The two numbers that decided the design are throughput on SCROLLING output
// and bytes retained per live screen. Scrolling is the one that bites: a
// terminal that repaints in place touches a bounded number of cells per frame,
// while a shell printing lines touches the whole grid once per line, and
// x/vt's measured 40-87 µs per scrolled line is per-cell work.
//
//	go test ./vtgrid/ -bench . -benchtime 3x -cpu 1
//	VTGRID_FOOTPRINT=1 go test ./vtgrid/ -run Footprint -v

import (
	"io"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/charmbracelet/x/vt"
)

const (
	benchCols = 150
	benchRows = 40
)

// scrollCorpus is n lines of the given width as a PTY writes them. Same total
// bytes at different line counts separates per-byte cost from per-scroll cost.
func scrollCorpus(n, width int) []byte {
	line := strings.Repeat("x", width)
	var b strings.Builder
	b.Grow(n * (width + 2))
	for i := 0; i < n; i++ {
		b.WriteString(line)
		b.WriteString("\r\n")
	}
	return []byte(b.String())
}

func benchOurs(b *testing.B, data []byte) {
	t := New(benchCols, benchRows)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = t.Write(data)
	}
}

func benchOracle(b *testing.B, data []byte) {
	emu := vt.NewEmulator(benchCols, benchRows)
	emu.Scrollback().SetMaxLines(1)
	done := make(chan struct{})
	go func() { defer close(done); _, _ = io.Copy(io.Discard, emu) }()
	defer func() { _ = emu.Close(); <-done }()
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = emu.Write(data)
	}
}

func BenchmarkScroll(b *testing.B) {
	for _, c := range []struct {
		name         string
		lines, width int
	}{
		{"200000x5", 200000, 5},
		{"20000x68", 20000, 68},
		{"10000x138", 10000, 138},
	} {
		data := scrollCorpus(c.lines, c.width)
		b.Run(c.name+"/vtgrid", func(b *testing.B) { benchOurs(b, data) })
		b.Run(c.name+"/x-vt", func(b *testing.B) { benchOracle(b, data) })
	}
}

func BenchmarkCorpus(b *testing.B) {
	for _, c := range vtCorpora {
		data := loadVTCorpus(b, c.Name)
		b.Run(c.Name+"/vtgrid", func(b *testing.B) { benchOurs(b, data) })
		b.Run(c.Name+"/x-vt", func(b *testing.B) { benchOracle(b, data) })
	}
}

// TestFootprint reports retained heap per live screen — what a server holding
// one per detachable session would pay. It asserts nothing; the number is the
// output. Opt-in because the x/vt fleet retains over a gigabyte.
func TestFootprint(t *testing.T) {
	if os.Getenv("VTGRID_FOOTPRINT") == "" {
		t.Skip("set VTGRID_FOOTPRINT=1 to run (the x/vt case retains ~1 GiB)")
	}
	const n = 200
	fill := scrollCorpus(2000, 100)

	measure := func(label string, build func() (any, func())) {
		held := make([]any, 0, n)
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
			v, stop := build()
			held = append(held, v)
			stops = append(stops, stop)
		}
		runtime.GC()
		var after runtime.MemStats
		runtime.ReadMemStats(&after)
		runtime.KeepAlive(held)
		d := int64(after.HeapAlloc) - int64(before.HeapAlloc)
		t.Logf("%-22s %d screens %dx%d: %7.1f MiB total, %8.1f kiB each",
			label, n, benchCols, benchRows, float64(d)/(1<<20), float64(d)/float64(n)/1024)
	}

	measure("vtgrid empty", func() (any, func()) {
		return New(benchCols, benchRows), func() {}
	})
	measure("vtgrid fed 2k lines", func() (any, func()) {
		t := New(benchCols, benchRows)
		_, _ = t.Write(fill)
		return t, func() {}
	})
	measure("x/vt empty", func() (any, func()) {
		emu := vt.NewEmulator(benchCols, benchRows)
		emu.Scrollback().SetMaxLines(1)
		done := make(chan struct{})
		go func() { defer close(done); _, _ = io.Copy(io.Discard, emu) }()
		return emu, func() { _ = emu.Close(); <-done }
	})
	measure("x/vt fed 2k lines", func() (any, func()) {
		emu := vt.NewEmulator(benchCols, benchRows)
		emu.Scrollback().SetMaxLines(1)
		done := make(chan struct{})
		go func() { defer close(done); _, _ = io.Copy(io.Discard, emu) }()
		_, _ = emu.Write(fill)
		return emu, func() { _ = emu.Close(); <-done }
	})
}
