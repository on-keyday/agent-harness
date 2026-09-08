package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/vtgrid"
)

// fillPane writes rows L01X..LnnX into a fresh rows-tall pane, one per row, so
// each row's text is a unique non-substring token (L01X is not a substring of
// L10X). The screen exactly fills, so nothing scrolls off and every LnnX sits
// on row n-1 — a deterministic ladder to read the crop window against.
func fillPane(cols, rows int) *PaneStreamer {
	p := &PaneStreamer{emu: vtgrid.New(cols, rows), cols: cols, rows: rows}
	lines := make([]string, rows)
	for i := 1; i <= rows; i++ {
		lines[i-1] = fmt.Sprintf("L%02dX", i)
	}
	p.emu.Write([]byte(strings.Join(lines, "\r\n")))
	return p
}

// Default (viewOff==0) is the bottom crop: a 5-row pane over 20 rows shows the
// last five, L16X..L20X. This pins the existing anchor so the scroll change
// cannot silently move the default.
func TestPaneStreamer_ScrollDefaultIsBottom(t *testing.T) {
	p := fillPane(80, 20)
	out := p.Render(80, 5)
	if !strings.Contains(out, "L20X") || !strings.Contains(out, "L16X") {
		t.Fatalf("default crop must be the bottom 5 rows L16X..L20X\ngot:\n%s", out)
	}
	if strings.Contains(out, "L15X") {
		t.Fatalf("default crop must not reach L15X\ngot:\n%s", out)
	}
}

// ScrollBy shifts the window UP by that many rows: from the bottom, +5 shows
// L11X..L15X and drops the bottom five (L16X..L20X). This is the CPU-meter case
// — pin content above the auto-followed bottom.
func TestPaneStreamer_ScrollUpShowsHigherRows(t *testing.T) {
	p := fillPane(80, 20)
	p.ScrollBy(5)
	if got := p.ScrollOffset(); got != 5 {
		t.Fatalf("ScrollOffset after ScrollBy(5) = %d, want 5", got)
	}
	out := p.Render(80, 5)
	if !strings.Contains(out, "L11X") || !strings.Contains(out, "L15X") {
		t.Fatalf("scroll up 5 must show L11X..L15X\ngot:\n%s", out)
	}
	if strings.Contains(out, "L16X") || strings.Contains(out, "L20X") {
		t.Fatalf("scroll up 5 must drop the bottom rows L16X/L20X\ngot:\n%s", out)
	}
}

// The window clamps at the top: scrolling far past the top shows the topmost
// rows (L01X..L05X) and never a negative startY. The requested offset may be
// larger than the reachable one — Render clamps the effect, not the request.
func TestPaneStreamer_ScrollClampsAtTop(t *testing.T) {
	p := fillPane(80, 20)
	p.ScrollBy(1000)
	out := p.Render(80, 5)
	if !strings.Contains(out, "L01X") || !strings.Contains(out, "L05X") {
		t.Fatalf("scroll past the top must show L01X..L05X\ngot:\n%s", out)
	}
	if strings.Contains(out, "L06X") {
		t.Fatalf("the top crop must not reach L06X\ngot:\n%s", out)
	}
}

// ResetScroll returns to the bottom-following default.
func TestPaneStreamer_ResetScrollReturnsToBottom(t *testing.T) {
	p := fillPane(80, 20)
	p.ScrollBy(8)
	p.ResetScroll()
	if got := p.ScrollOffset(); got != 0 {
		t.Fatalf("ScrollOffset after ResetScroll = %d, want 0", got)
	}
	out := p.Render(80, 5)
	if !strings.Contains(out, "L20X") {
		t.Fatalf("reset must return to the bottom crop (L20X)\ngot:\n%s", out)
	}
}

// ScrollBy(-n) floors at zero: scrolling down while already at the bottom does
// not underflow into a negative offset (which would push the window below the
// content).
func TestPaneStreamer_ScrollDownFloorsAtZero(t *testing.T) {
	p := fillPane(80, 20)
	p.ScrollBy(-5)
	if got := p.ScrollOffset(); got != 0 {
		t.Fatalf("ScrollOffset after ScrollBy(-5) from 0 = %d, want 0 (no underflow)", got)
	}
}

// The grid routes shift+up/shift+down to the focused pane's scroll and `0`
// resets it — the wiring, so a test sees the key path and not only the method.
func TestGrid_ScrollKeysDriveFocusedPane(t *testing.T) {
	p := fillPane(80, 20)
	m := GridModel{panes: []*PaneStreamer{p}, focus: 0}

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyShiftUp})
	if got := p.ScrollOffset(); got != 1 {
		t.Fatalf("shift+up must scroll the focused pane up by 1, got offset %d", got)
	}
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyShiftUp})
	if got := p.ScrollOffset(); got != 2 {
		t.Fatalf("a second shift+up must reach offset 2, got %d", got)
	}
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyShiftDown})
	if got := p.ScrollOffset(); got != 1 {
		t.Fatalf("shift+down must scroll back to offset 1, got %d", got)
	}
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'0'}})
	if got := p.ScrollOffset(); got != 0 {
		t.Fatalf("`0` must reset the focused pane's scroll to 0, got %d", got)
	}
}
