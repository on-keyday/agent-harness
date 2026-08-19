package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/on-keyday/agent-harness/runner/protocol"
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

// mkGridTask builds a TaskInfo with the four fields gridLiveTasks reads.
func mkGridTask(id byte, kind protocol.TaskKind, st protocol.TaskStatus, lastOutput uint64) protocol.TaskInfo {
	var t protocol.TaskInfo
	t.Id.Id[0] = id
	t.Kind = kind
	t.Status = st
	t.LastOutputAt = lastOutput
	return t
}

func TestGridLiveTasks_KeepsLiveInteractiveActivityDesc(t *testing.T) {
	got := gridLiveTasks([]protocol.TaskInfo{
		mkGridTask('a', protocol.TaskKind_Interactive, protocol.TaskStatus_Running, 10),
		mkGridTask('b', protocol.TaskKind_Oneshot, protocol.TaskStatus_Running, 99),      // not tileable: no PTY to watch
		mkGridTask('c', protocol.TaskKind_Interactive, protocol.TaskStatus_Succeeded, 5), // session is gone
		mkGridTask('d', protocol.TaskKind_Interactive, protocol.TaskStatus_Detached, 30), // alive, just unattached
		mkGridTask('e', protocol.TaskKind_Interactive, protocol.TaskStatus_Running, 0),   // never produced output
	})
	var ids []byte
	for _, task := range got {
		ids = append(ids, task.Id.Id[0])
	}
	if string(ids) != "dae" {
		t.Errorf("gridLiveTasks = %q, want %q (activity-desc, never-active last)", ids, "dae")
	}
}

func TestGridModel_ScopeLabelNamesTheNarrowing(t *testing.T) {
	// "all" is a claim, not a blank: a narrowed grid that lost its label would
	// otherwise be indistinguishable from an unfiltered one.
	var m GridModel
	if got := m.scopeLabel(); got != "all" {
		t.Errorf("unlabelled scopeLabel = %q, want %q", got, "all")
	}
	m.scope = "01234567+desc"
	if got := m.scopeLabel(); got != "01234567+desc" {
		t.Errorf("scopeLabel = %q, want %q", got, "01234567+desc")
	}
	m.open = true
	m.SetSize(200, 40)
	if line := m.statusLine(); !strings.Contains(line, "scope:01234567+desc") {
		t.Errorf("status bar must carry the scope, got:\n%s", line)
	}
	// An emptied narrowed grid says which set is empty, not that the whole
	// fleet is.
	if view := m.View(); !strings.Contains(view, "in scope 01234567+desc") {
		t.Errorf("empty narrowed grid must name its scope, got:\n%s", view)
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
