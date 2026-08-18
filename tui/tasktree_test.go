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
