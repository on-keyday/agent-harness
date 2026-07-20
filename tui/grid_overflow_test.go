package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/vt"
)

// widePaneStatic fills every emulator row with full-width CJK glyphs — the
// content that used to make each crop line overshoot the cell width, wrap, and
// inflate the pane past its budgeted height until the whole grid overflowed the
// terminal (top rows clipped by lipgloss.Place). Static text triggers no VT
// query responses, so no drain goroutine is needed; the emulator is Closed by
// the caller to avoid leaks.
func widePaneStatic(cols, rows int) *PaneStreamer {
	p := &PaneStreamer{emu: vt.NewEmulator(cols, rows), cols: cols, rows: rows}
	var ls []string
	for i := 0; i < rows; i++ {
		ls = append(ls, strings.Repeat("あ", cols/2))
	}
	p.emu.Write([]byte(strings.Join(ls, "\r\n")))
	return p
}

func TestGridView_NeverExceedsTerminal(t *testing.T) {
	var fails []string
	for _, W := range []int{80, 120, 160} {
		for _, H := range []int{24, 40, 50} {
			for _, n := range []int{2, 5, 9} {
				panes := make([]*PaneStreamer, n)
				for i := range panes {
					panes[i] = widePaneStatic(100, 30)
				}
				m := GridModel{open: true, panes: panes}
				m.SetSize(W, H)
				v := m.View()
				if gh, gw := lipgloss.Height(v), lipgloss.Width(v); gh > H || gw > W {
					fails = append(fails, fmt.Sprintf("W=%d H=%d n=%d -> view %dx%d (over h+%d w+%d)", W, H, n, gw, gh, gh-H, gw-W))
				}
				for _, p := range panes {
					_ = p.emu.Close()
				}
			}
		}
	}
	if len(fails) > 0 {
		t.Fatalf("%d combos overflow the terminal:\n%s", len(fails), strings.Join(fails, "\n"))
	}
}

func TestPaneStreamer_RenderWidthGuaranteeAscii(t *testing.T) {
	p := &PaneStreamer{emu: vt.NewEmulator(20, 1), cols: 20, rows: 1}
	defer p.emu.Close()
	p.emu.Write([]byte("hello world"))
	out := p.Render(20, 1)
	if !strings.Contains(out, "hello world") {
		t.Fatalf("ascii content corrupted: %q", out)
	}
	if lipgloss.Width(out) > 20 {
		t.Fatalf("line visual width %d exceeds requested 20: %q", lipgloss.Width(out), out)
	}
}
