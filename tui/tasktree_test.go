package tui

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func tuiTask(id, parent byte, createdAt uint64) protocol.TaskInfo {
	var t protocol.TaskInfo
	t.Id.Id[0] = id
	t.CreatorTaskId.Id[0] = parent
	t.CreatedAt = createdAt
	t.Status = protocol.TaskStatus_Running
	t.SetRepoPath([]byte("/repo"))
	return t
}

// TestTasksTree_ParallelSlicesFollowTheReordering is the bug this feature could
// most easily ship: rowIDs / rowTasks are indexed by TABLE POSITION, so a tree
// ordering applied to the cells but not to them would put the cursor on one row
// and open the detail popup on another.
func TestTasksTree_ParallelSlicesFollowTheReordering(t *testing.T) {
	m := NewTasks()
	// Input order is deliberately not tree order: 'c' (child of a) comes first.
	in := []protocol.TaskInfo{
		tuiTask('c', 'a', 30),
		tuiTask('a', 0, 10),
		tuiTask('b', 'a', 20),
	}
	m.SetTree(true)
	m.SetSize(120, 20)
	m.SetRows(in, nil)

	rows := m.Rows()
	if len(rows) != 3 {
		t.Fatalf("got %d rows, want 3", len(rows))
	}
	wantOrder := []byte{'a', 'b', 'c'}
	for i, want := range wantOrder {
		if got := rows[i].Id.Id[0]; got != want {
			t.Errorf("rowTasks[%d] = %c, want %c (tree order)", i, got, want)
		}
	}
	// SelectedID must agree with rowTasks at the same index.
	for i := range rows {
		m.table.SetCursor(i)
		if got, want := m.SelectedID()[:2], hexPrefix(rows[i]); got != want {
			t.Errorf("cursor %d: SelectedID=%s, rowTasks=%s — parallel slices disagree", i, got, want)
		}
	}
}

func hexPrefix(t protocol.TaskInfo) string {
	const hexd = "0123456789abcdef"
	b := t.Id.Id[0]
	return string([]byte{hexd[b>>4], hexd[b&0xf]})
}

func TestTasksTree_ToggleRestoresFlatOrderAndColumns(t *testing.T) {
	m := NewTasks()
	in := []protocol.TaskInfo{
		tuiTask('c', 'a', 30),
		tuiTask('a', 0, 10),
	}
	m.SetSize(120, 20)
	m.SetRows(in, nil)
	if m.TreeMode() {
		t.Fatal("tree mode should be off by default")
	}
	if got := m.Rows()[0].Id.Id[0]; got != 'c' {
		t.Errorf("flat mode reordered rows: first = %c, want c (input order)", got)
	}

	m.SetTree(true)
	m.SetRows(in, nil)
	if got := m.Rows()[0].Id.Id[0]; got != 'a' {
		t.Errorf("tree mode did not reorder: first = %c, want a", got)
	}

	m.SetTree(false)
	m.SetRows(in, nil)
	if got := m.Rows()[0].Id.Id[0]; got != 'c' {
		t.Errorf("toggling back did not restore input order: first = %c, want c", got)
	}
}

func TestTasksTree_IDCellCarriesTheGutter(t *testing.T) {
	m := NewTasks()
	m.SetTree(true)
	m.SetSize(160, 20)
	m.SetRows([]protocol.TaskInfo{
		tuiTask('a', 0, 10),
		tuiTask('b', 'a', 20),
	}, nil)
	view := m.View()
	if !strings.Contains(view, "└─") {
		t.Errorf("tree view has no connector in the ID column:\n%s", view)
	}
	if !strings.Contains(view, "by creator") {
		t.Errorf("tree view does not label its ID column:\n%s", view)
	}
}

// D18-2: in tree order the ID cell ends with the task's tier mark; flat order
// is unchanged.
func TestTreeIDCellCarriesTierMark(t *testing.T) {
	m := NewTasks()
	task := tuiTask('a', 0, 10)
	task.Capabilities = protocol.Capability_Spawn | protocol.Capability_Purge
	m.SetSize(160, 20)
	m.SetRows([]protocol.TaskInfo{task}, nil)
	if cell := m.table.Rows()[0][1]; strings.Contains(cell, "強") {
		t.Errorf("flat ID cell %q carries a tier mark", cell)
	}
	m.SetTree(true)
	m.SetRows([]protocol.TaskInfo{task}, nil)
	if cell := m.table.Rows()[0][1]; !strings.HasPrefix(cell, "強2") {
		t.Errorf("tree ID cell %q does not lead with the tier mark 強2", cell)
	}
}

// Review Focus 1: no granular bit, no mark — only the blank tier slot that
// keeps the gutters aligned with the rows that have one.
func TestTreeIDCellNoMarkForNoCaps(t *testing.T) {
	m := NewTasks()
	m.SetTree(true)
	m.SetSize(160, 20)
	m.SetRows([]protocol.TaskInfo{tuiTask('a', 0, 10)}, nil)
	if cell := m.table.Rows()[0][1]; cell != "     61000000" {
		t.Errorf("tree ID cell = %q, want the blank tier slot and the id8", cell)
	}
}

// The marks must survive the panel's real width: bubbles truncates a cell from
// the right, so marks after a deep gutter + id were cut to "…" at 120–160
// columns and a marked holder read as unmarked.
func TestTreeMarksSurviveNarrowPanels(t *testing.T) {
	root := tuiTask('a', 0, 10)
	root.Capabilities = protocol.Capability_Spawn | protocol.Capability_Purge
	child := tuiTask('b', 'a', 20)
	child.Capabilities = protocol.Capability_Spawn
	grand := tuiTask('c', 'b', 30)
	grand.Capabilities = protocol.Capability_Spawn
	for _, panel := range []int{58, 78} { // a 120- and a 160-column terminal's half
		m := NewTasks()
		m.SetTree(true)
		m.SetSize(panel, 20)
		m.SetMark(protocol.Capability_Spawn)
		m.SetRows([]protocol.TaskInfo{root, child, grand}, nil)
		view := m.View()
		if n := strings.Count(view, "◆"); n != 3+1 { // three rows + the header
			t.Errorf("panel %d: %d ◆ in the view, want 3 rows and the header:\n%s", panel, n, view)
		}
		if n := strings.Count(view, "強"); n != 3 {
			t.Errorf("panel %d: %d tier marks in the view, want 3:\n%s", panel, n, view)
		}
	}
}

// `T` on a selected task is the TUI's "show this task in the tree": the
// selection follows the task across the reorder, not the row number it had.
func TestTreeToggleKeepsTheSelectedTask(t *testing.T) {
	a := New(Config{})
	a.width, a.height = 160, 40
	// Flat order is newest first: c, o, p. Tree order puts p before its child c.
	setAuditTasks(a, tuiTask('p', 0, 10), tuiTask('o', 0, 20), tuiTask('c', 'p', 30))
	a.setFocus(focusTasks)
	a.tasks.table.SetCursor(0)
	want := a.tasks.SelectedID()
	if want[:2] != "63" { // 'c'
		t.Fatalf("flat cursor 0 is %s, want c's row", want[:2])
	}
	a = press(t, a, runes("T"))
	if !a.tasks.TreeMode() || a.tasks.SelectedID() != want {
		t.Fatalf("tree=%v selected=%s; want tree order still on %s", a.tasks.TreeMode(), a.tasks.SelectedID()[:2], want[:2])
	}
	a = press(t, a, runes("T"))
	if a.tasks.TreeMode() || a.tasks.SelectedID() != want {
		t.Fatalf("tree=%v selected=%s; want flat order still on %s", a.tasks.TreeMode(), a.tasks.SelectedID()[:2], want[:2])
	}
}
