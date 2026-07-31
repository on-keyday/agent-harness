package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/charmbracelet/bubbles/table"
)

func baseSet() []table.Column {
	return []table.Column{
		{Title: "Status", Width: 9},
		{Title: "ID", Width: 12},
		{Title: "Repo", Width: 28},
		{Title: "Prompt", Width: 20},
	}
}

// TestFitColumnsFillsExactly — the whole point is that the table renders in the
// space it was given, no more and no less.
func TestFitColumnsFillsExactly(t *testing.T) {
	for _, w := range []int{20, 38, 40, 60, 89, 100, 200} {
		got := fitColumns(baseSet(), w, 3)
		if n := tableRenderWidth(got); n != w && w >= minColWidth*len(baseSet())+tableCellPadding*len(baseSet()) {
			t.Errorf("width=%d: rendered %d cells, want %d (%+v)", w, n, w, got)
		}
		for _, c := range got {
			if c.Width < minColWidth {
				t.Errorf("width=%d: column %q shrank to %d, below the %d floor", w, c.Title, c.Width, minColWidth)
			}
		}
	}
}

// TestFitColumnsClampsBelowTheFloor — under the floor the table cannot fit, and
// the columns must stop shrinking rather than go to zero or negative (bubbles
// renders those as ragged rows).
func TestFitColumnsClampsBelowTheFloor(t *testing.T) {
	base := baseSet()
	got := fitColumns(base, 4, 3)
	floor := minColWidth * len(base)
	if n := tableRenderWidth(got); n != floor+tableCellPadding*len(base) {
		t.Errorf("rendered %d cells, want the clamped %d", n, floor+tableCellPadding*len(base))
	}
}

// TestFitColumnsDoesNotRatchet — SetSize runs on every resize, so computing
// from the previous result instead of from the natural widths would shrink the
// table a bit more each time and never recover when the terminal grows.
func TestFitColumnsDoesNotRatchet(t *testing.T) {
	base := baseSet()
	narrow := fitColumns(base, 40, 3)
	for i := 0; i < 5; i++ {
		narrow = fitColumns(base, 40, 3)
	}
	if n := tableRenderWidth(narrow); n != 40 {
		t.Errorf("after repeated fits: %d cells, want 40", n)
	}
	wide := fitColumns(base, 200, 3)
	if n := tableRenderWidth(wide); n != 200 {
		t.Errorf("re-widened to %d cells, want 200 — base was mutated somewhere", n)
	}
}

// TestFitColumnsLeavesBaseAlone is the mechanism behind the no-ratchet
// property, asserted directly so a future change that mutates in place fails
// here rather than as a confusing resize bug.
func TestFitColumnsLeavesBaseAlone(t *testing.T) {
	base := baseSet()
	want := baseSet()
	fitColumns(base, 40, 3)
	fitColumns(base, 300, 3)
	for i := range base {
		if base[i].Width != want[i].Width {
			t.Errorf("base[%d] %q was mutated: %d, want %d", i, base[i].Title, base[i].Width, want[i].Width)
		}
	}
}

func TestFlexColumnResolvesByTitle(t *testing.T) {
	base := baseSet()
	if got := flexColumn(base, "Repo"); got != 2 {
		t.Errorf("flexColumn(Repo) = %d, want 2", got)
	}
	if got := flexColumn(base, "nope"); got != len(base)-1 {
		t.Errorf("unknown title = %d, want the last column %d", got, len(base)-1)
	}
}

// TestFitColumnsGivesSurplusToFlex — on a wide terminal the unbounded column
// is the one that should grow, not an arbitrary one.
func TestFitColumnsGivesSurplusToFlex(t *testing.T) {
	base := baseSet()
	got := fitColumns(base, 300, flexColumn(base, "Prompt"))
	for i := range got {
		if got[i].Title == "Prompt" {
			if got[i].Width <= base[i].Width {
				t.Errorf("flex column did not absorb the surplus: %d", got[i].Width)
			}
			continue
		}
		if got[i].Width != base[i].Width {
			t.Errorf("non-flex column %q changed: %d, want %d", got[i].Title, got[i].Width, base[i].Width)
		}
	}
}

// TestViewFitsTerminalWidth is the regression test for the frame being
// shredded on any terminal narrower than the tables' natural width: the
// runners/tasks panels rendered at a fixed ~182 cells because bubbles'
// SetWidth does not resize columns.
func TestViewFitsTerminalWidth(t *testing.T) {
	for _, w := range []int{80, 100, 137, 160, 240} {
		t.Run(fmt.Sprintf("width=%d", w), func(t *testing.T) {
			a := New(Config{Server: "ws://test:8080", DefaultRepo: ""})
			a.width = w
			a.height = 40
			a.layout()

			for i, line := range strings.Split(a.View(), "\n") {
				if n := lipgloss.Width(line); n > w {
					t.Errorf("line %d is %d cells wide (terminal is %d): %q", i, n, w, line)
				}
			}
		})
	}
}
