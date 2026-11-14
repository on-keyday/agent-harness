package tui

import (
	"fmt"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// TestViewHeightFitsTerminal verifies that View() fills the terminal exactly,
// from the 24-row minimum layout() accepts upward: more rows overflow it, fewer
// leave rows the tables could have used. The panels other than the runners +
// tasks row are fixed (16 rows, see layout), so the tables absorb the rest.
func TestViewHeightFitsTerminal(t *testing.T) {
	heights := []int{24, 30, 35, 40, 50}
	for _, h := range heights {
		t.Run(fmt.Sprintf("height=%d", h), func(t *testing.T) {
			a := New(Config{Server: "ws://test:8080", DefaultRepo: ""})
			a.width = 120
			a.height = h
			a.layout()

			view := a.View()
			got := lipgloss.Height(view)
			if got > h {
				t.Errorf("View() height = %d, want <= %d (overflows terminal by %d row(s))",
					got, h, got-h)
			}
			if got < h {
				t.Errorf("View() height = %d at a %d-row terminal: %d row(s) left unused", got, h, h-got)
			}
		})
	}
}

// TestTaskTableTakesFreedRows: the task log pane is gone (A3, A-D30), and the
// rows it held go to the task table, which used to be fixed at 10.
func TestTaskTableTakesFreedRows(t *testing.T) {
	a := New(Config{Server: "ws://test:8080", DefaultRepo: ""})
	a.width, a.height = 120, 40
	a.layout()
	if got := a.tasks.table.Height(); got <= 10 {
		t.Fatalf("task table shows %d rows at a 40-row terminal, want more than 10", got)
	}
}
