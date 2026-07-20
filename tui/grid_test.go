package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// keyMsg builds a tea.KeyMsg carrying the given rune(s) as printable input.
func keyMsg(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

func TestGridModel_LayoutAndFocusMove(t *testing.T) {
	m := NewGridModel()
	// Inject panes directly (no live streams needed for layout).
	m.panes = []*PaneStreamer{
		NewPaneStreamer("aaaaaaaa", 24, 80),
		NewPaneStreamer("bbbbbbbb", 24, 80),
		NewPaneStreamer("cccccccc", 24, 80),
	}
	m.open = true
	m.SetSize(120, 40)

	if got := m.FocusedTaskID(); got != "aaaaaaaa" {
		t.Fatalf("initial focus should be first pane, got %q", got)
	}
	// Move focus right/down; with 3 panes and a 2-col grid, "l" then "j".
	m2, _ := m.Update(keyMsg("l"))
	if m2.FocusedTaskID() != "bbbbbbbb" {
		t.Fatalf("after 'l' focus should be pane 2, got %q", m2.FocusedTaskID())
	}
	view := m2.View()
	if !strings.Contains(view, "aaaaaaa") || !strings.Contains(view, "bbbbbbb") {
		t.Fatalf("view must show pane task-id labels, got:\n%s", view)
	}
}

func TestGridModel_DismissFocusedPane(t *testing.T) {
	m := NewGridModel()
	m.panes = []*PaneStreamer{
		NewPaneStreamer("aaaaaaaa", 24, 80),
		NewPaneStreamer("bbbbbbbb", 24, 80),
	}
	m.open = true
	m.SetSize(120, 40)
	m2, _ := m.Update(keyMsg("x"))
	if len(m2.panes) != 1 {
		t.Fatalf("dismiss should drop the focused pane, have %d", len(m2.panes))
	}
	if m2.FocusedTaskID() != "bbbbbbbb" {
		t.Fatalf("after dismissing pane 1, focus should land on pane 2, got %q", m2.FocusedTaskID())
	}
}
