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
	m.panes = []gridPane{
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
	m.panes = []gridPane{
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

// A pane's header names the task's repo, not only its id: with several
// sessions tiled, eight hex digits say nothing about which checkout each one
// is working in. The repo is shown by its last path element; the task table
// already carries the full path.
func TestGridModel_PaneHeaderNamesTheRepo(t *testing.T) {
	m := NewGridModel()
	m.panes = []gridPane{
		NewPaneStreamer("aaaaaaaa11", 24, 80),
		NewPaneStreamer("bbbbbbbb22", 24, 80),
	}
	m.repos = map[string]string{
		"aaaaaaaa11": "/home/u/workspace/alpha-service",
		"bbbbbbbb22": "C:/Users/u/src/beta-tool",
	}
	m.open = true
	m.SetSize(120, 40)
	view := m.View()
	for _, want := range []string{"aaaaaaaa alpha-service", "bbbbbbbb beta-tool"} {
		if !strings.Contains(view, want) {
			t.Errorf("pane header lacks %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "/home/u/workspace") {
		t.Errorf("pane header carries the full path, not the repo name:\n%s", view)
	}
}

// In a narrow cell the repo name gives way before the id does: the id is what
// the grid's keys and the other surfaces refer to.
func TestGridModel_NarrowPaneKeepsTheIDAndCutsTheRepo(t *testing.T) {
	m := NewGridModel()
	m.panes = []gridPane{NewPaneStreamer("aaaaaaaa11", 24, 80)}
	m.repos = map[string]string{"aaaaaaaa11": "/w/a-very-long-repository-name-indeed"}
	m.open = true
	head := strings.SplitN(m.renderPane(0, 20, 5), "\n", 3)[1]
	if !strings.Contains(head, "aaaaaaaa") {
		t.Errorf("narrow header lost the id: %q", head)
	}
	if !strings.Contains(head, "…") {
		t.Errorf("narrow header should cut the repo name with an ellipsis: %q", head)
	}
}

// The header carries the agent's lifecycle badge (work / wait / ready / err,
// cli.LifecycleBadge) so a tiled screen says which sessions want a look, and
// it outlives the repo name in a narrow cell. A task that never reported a
// lifecycle shows none rather than `lc:-`.
func TestGridModel_PaneHeaderShowsTheLifecycleBadge(t *testing.T) {
	m := NewGridModel()
	m.panes = []gridPane{
		NewPaneStreamer("aaaaaaaa11", 24, 80),
		NewPaneStreamer("bbbbbbbb22", 24, 80),
	}
	m.repos = map[string]string{"aaaaaaaa11": "/w/alpha", "bbbbbbbb22": "/w/beta"}
	m.SetLifecycle(map[string]string{"aaaaaaaa11": "ready:3m"})
	m.open = true
	m.SetSize(120, 40)
	view := m.View()
	if !strings.Contains(view, "aaaaaaaa alpha ready:3m") {
		t.Errorf("pane header lacks the lifecycle badge:\n%s", view)
	}
	if strings.Contains(view, "lc:-") {
		t.Errorf("a task with no lifecycle report shows lc:-:\n%s", view)
	}
	narrow := strings.SplitN(m.renderPane(0, 18, 5), "\n", 3)[1]
	if !strings.Contains(narrow, "ready:3m") || !strings.Contains(narrow, "aaaaaaaa") {
		t.Errorf("narrow header must keep the id and the badge: %q", narrow)
	}
}

func TestGridLifecycleBadges_OnlyReportedLiveTasks(t *testing.T) {
	working := mkGridTask(1, protocol.TaskKind_Interactive, protocol.TaskStatus_Running, 1)
	working.Lifecycle, working.LifecycleAt, working.LifecycleAgeMs = protocol.LifecycleState_Working, 1, 5000
	silent := mkGridTask(2, protocol.TaskKind_Interactive, protocol.TaskStatus_Running, 1)
	got := gridLifecycleBadges([]protocol.TaskInfo{working, silent})
	if got[FormatTaskID(working.Id)] != "work:5s" {
		t.Errorf("working badge = %q, want work:5s", got[FormatTaskID(working.Id)])
	}
	if _, ok := got[FormatTaskID(silent.Id)]; ok {
		t.Errorf("a task with no report got a badge: %q", got[FormatTaskID(silent.Id)])
	}
}
