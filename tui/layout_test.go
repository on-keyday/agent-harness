package tui

import (
	"fmt"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// TestViewHeightFitsTerminal verifies that View() never produces more rows than
// the terminal height. This is a regression test for the off-by-one in
// logHeight := max(a.height-27, 5) — the correct reserved count is 30 (28
// fixed non-log rows + 2 log-panel border rows), so logHeight must be
// a.height-30, not a.height-27.
//
// Note: the layout has 28 fixed non-log rows plus a minimum 5-row log content
// + 2 log-panel border rows = minimum 35 total rows. Terminals smaller than 35
// rows will always overflow because the fixed panels cannot be collapsed; the
// layout() guard (height<24) only prevents layout() from running, not View()
// from rendering the panels at their natural sizes. The regression test
// therefore only covers heights >= 35 where the layout can actually fit.
func TestViewHeightFitsTerminal(t *testing.T) {
	// Minimum usable height: 28 fixed non-log rows + 5 min log content + 2 log
	// border = 35. Test at the minimum achievable and a comfortable height.
	heights := []int{35, 40, 50}
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
		})
	}
}
