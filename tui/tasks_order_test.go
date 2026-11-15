package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func lifeTask(id byte, created uint64, s protocol.LifecycleState) protocol.TaskInfo {
	t := tuiTask(id, 0, created)
	t.Lifecycle, t.LifecycleAt = s, 1
	return t
}

// r1, r2 are ready and w1 working: in the state order the rows are
// [── ready (2) ──, r2, r1, ── working (1) ──, w1] (inside a group, newest
// activity first; equal activity keeps created order, newest first).
func stateTasks() []protocol.TaskInfo {
	return []protocol.TaskInfo{
		lifeTask('w', 30, protocol.LifecycleState_Working),
		lifeTask('b', 20, protocol.LifecycleState_Idle),
		lifeTask('a', 10, protocol.LifecycleState_Idle),
	}
}

func orderedTasks(t *testing.T, o cli.TaskOrder, ts []protocol.TaskInfo) TasksModel {
	t.Helper()
	m := NewTasks()
	m.SetSize(120, 20)
	m.SetOrder(o)
	m.SetRows(ts, nil)
	m.Focus()
	return m
}

func selectedByte(m *TasksModel) byte {
	if st := m.SelectedTask(); st != nil {
		return st.Id.Id[0]
	}
	return 0
}

func TestTasksStateOrderDrawsHeaderRows(t *testing.T) {
	m := orderedTasks(t, cli.OrderState, stateTasks())
	v := stripANSI(m.View())
	ready, working := strings.Index(v, "── ready (2) ──"), strings.Index(v, "── working (1) ──")
	if ready < 0 || working < 0 || ready > working {
		t.Fatalf("headers missing or out of order:\n%s", v)
	}
	if strings.Contains(v, "#0") || strings.Contains(v, "#1") {
		t.Errorf("a header row's marker leaked into the view:\n%s", v)
	}
	var ids []byte
	for _, r := range m.Rows() {
		ids = append(ids, r.Id.Id[0])
	}
	if string(ids) != "baw" {
		t.Errorf("Rows() = %q, want the tasks only, in table order (baw)", ids)
	}
}

// Review Focus 3: the cursor never rests on a header, so the actions that read
// the selection always get a task.
func TestTasksCursorSkipsHeaderRows(t *testing.T) {
	m := orderedTasks(t, cli.OrderState, stateTasks())
	if got := selectedByte(&m); got != 'b' {
		t.Fatalf("initial selection = %q, want the first task under the first header (b)", got)
	}
	down, up := tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeyUp}
	m, _ = m.Update(down) // b → a
	m, _ = m.Update(down) // a → header → w
	if got := selectedByte(&m); got != 'w' {
		t.Fatalf("down across a header = %q, want w", got)
	}
	m, _ = m.Update(up) // w → header → a
	if got := selectedByte(&m); got != 'a' {
		t.Fatalf("up across a header = %q, want a", got)
	}
	m, _ = m.Update(up) // a → b
	m, _ = m.Update(up) // b → top header: nothing above, so back to b
	if got := selectedByte(&m); got != 'b' || m.SelectedID() == "" {
		t.Fatalf("up onto the top header = %q (id %q), want b", got, m.SelectedID())
	}
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyHome})
	if got := selectedByte(&m); got != 'b' {
		t.Fatalf("home = %q, want b", got)
	}
}

func TestTasksRefreshNeverLeavesTheCursorOnAHeader(t *testing.T) {
	m := orderedTasks(t, cli.OrderState, stateTasks())
	m.SelectID(hexID('a'))
	// a is gone: its row index (2) is now the working header.
	ts := stateTasks()
	m.SetRows([]protocol.TaskInfo{ts[0], ts[1]}, nil)
	if st := m.SelectedTask(); st == nil || m.SelectedID() == "" {
		t.Fatalf("cursor rests on a header after the refresh (cursor %d)", m.table.Cursor())
	}
}

// In a reordering mode the rows move on every poll; the cursor stays on its
// task, or `d` / Enter would act on whichever task moved under it.
func TestTasksRefreshKeepsTheSelectedTaskWhenRowsMove(t *testing.T) {
	m := orderedTasks(t, cli.OrderState, stateTasks())
	m.SelectID(hexID('w'))
	ts := stateTasks()
	ts[1].Lifecycle = protocol.LifecycleState_Working // b moves into working, above w
	m.SetRows(ts, nil)
	if got := selectedByte(&m); got != 'w' {
		t.Fatalf("selection after the reorder = %q, want w", got)
	}
}

func TestTasksDefaultOrderIsCreatedNewestFirst(t *testing.T) {
	a := New(Config{})
	a.width, a.height = 160, 40
	if got := a.tasks.Order(); got != cli.OrderCreated {
		t.Fatalf("default order = %q, want created", got)
	}
	setAuditTasks(a, tuiTask('a', 0, 10), tuiTask('c', 0, 30), tuiTask('b', 0, 20))
	var ids []byte
	for _, r := range a.tasks.Rows() {
		ids = append(ids, r.Id.Id[0])
	}
	if string(ids) != "cba" {
		t.Errorf("rows = %q, want cba", ids)
	}
	if strings.Contains(stripANSI(a.tasks.View()), "──  ") {
		t.Errorf("created mode drew a header")
	}
}

func TestOrderKeyCyclesAndReports(t *testing.T) {
	a := New(Config{})
	a.width, a.height = 160, 40
	setAuditTasks(a, stateTasks()...)
	a.setFocus(focusTasks)
	for _, want := range []cli.TaskOrder{cli.OrderActivity, cli.OrderLive, cli.OrderState, cli.OrderRepo, cli.OrderCreated} {
		a = press(t, a, runes("o"))
		if a.tasks.Order() != want {
			t.Fatalf("after o: order %q, want %q", a.tasks.Order(), want)
		}
		if got := strings.Join(a.cmdresult.lines, "\n"); !strings.HasSuffix(got, "order: "+string(want)) {
			t.Fatalf("result pane %q does not end with the mode", got)
		}
	}
}

// Review Focus 5: the tree is its own order.
func TestOrderKeyRefusedInTreeView(t *testing.T) {
	a := New(Config{})
	a.width, a.height = 160, 40
	setAuditTasks(a, stateTasks()...)
	a.setFocus(focusTasks)
	a = press(t, a, runes("T"), runes("o"))
	if a.tasks.Order() != cli.OrderCreated {
		t.Errorf("o in the tree view changed the order to %q", a.tasks.Order())
	}
	if got := strings.Join(a.cmdresult.lines, "\n"); !strings.HasSuffix(got, "order: not with the tree view (T)") {
		t.Errorf("result pane %q", got)
	}
}

func hexID(b byte) string {
	var id protocol.TaskID
	id.Id[0] = b
	return FormatTaskID(id)
}
