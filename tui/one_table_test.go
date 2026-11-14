package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func oneTableApp(w, h int) *App {
	a := New(Config{Server: "ws://test:8080"})
	a.width, a.height = w, h
	a.layout()
	return a
}

func titles(cols []string) string { return strings.Join(cols, ",") }

func tasksColTitles(m *TasksModel) []string {
	var out []string
	for _, c := range m.table.Columns() {
		out = append(out, c.Title)
	}
	return out
}

// The visible table follows focus; notify/cmdresult/cmdline keep the last one.
func TestOnlyTheFocusedTableIsShown(t *testing.T) {
	for _, sz := range [][2]int{{80, 24}, {200, 50}} {
		a := oneTableApp(sz[0], sz[1])
		a.setFocus(focusTasks)
		v := a.View()
		if !strings.Contains(v, "Repo") || strings.Contains(v, "Roots") {
			t.Errorf("%v tasks focused: want Repo and not Roots:\n%s", sz, v)
		}
		a.setFocus(focusRunners)
		v = a.View()
		if strings.Contains(v, "Repo") || !strings.Contains(v, "Roots") {
			t.Errorf("%v runners focused: want Roots and not Repo:\n%s", sz, v)
		}
		a.setFocus(focusNotify)
		if v = a.View(); !strings.Contains(v, "Roots") || strings.Contains(v, "Repo") {
			t.Errorf("%v notify after runners must keep runners:\n%s", sz, v)
		}
		a.setFocus(focusTasks)
		a.setFocus(focusCmdline)
		if v = a.View(); !strings.Contains(v, "Repo") || strings.Contains(v, "Roots") {
			t.Errorf("%v cmdline after tasks must keep tasks:\n%s", sz, v)
		}
	}
}

func TestStartupShowsTasksTable(t *testing.T) {
	a := oneTableApp(120, 40)
	if v := a.View(); !strings.Contains(v, "Repo") || strings.Contains(v, "Roots") {
		t.Errorf("startup must show the tasks table:\n%s", v)
	}
}

// Both tables are sized to the full width: the visible table's frame spans the
// terminal, and the tasks table gets the room for a readable Repo.
func TestVisibleTableUsesFullWidth(t *testing.T) {
	a := oneTableApp(200, 50)
	if got := a.tasks.width; got != 198 {
		t.Errorf("tasks panel width = %d, want 198", got)
	}
	if got := a.runners.width; got != 198 {
		t.Errorf("runners panel width = %d, want 198", got)
	}
	for i, line := range strings.Split(a.View(), "\n") {
		if n := lipgloss.Width(line); n > 200 {
			t.Errorf("line %d is %d wide", i, n)
		}
	}
}

func TestRunnerSummaryCounts(t *testing.T) {
	mk := func(s protocol.RunnerStatus) protocol.RunnerInfo { return protocol.RunnerInfo{Status: s} }
	got := runnerSummary([]protocol.RunnerInfo{
		mk(protocol.RunnerStatus_Idle), mk(protocol.RunnerStatus_Busy),
		mk(protocol.RunnerStatus_Busy), mk(protocol.RunnerStatus_Busy),
		mk(protocol.RunnerStatus_Offline),
	})
	if want := "runners 5: 1 idle 3 busy 1 offline"; got != want {
		t.Errorf("summary = %q, want %q", got, want)
	}
	// idle and busy keep their zero; an absent status is omitted.
	if got, want := runnerSummary(nil), "runners 0: 0 idle 0 busy"; got != want {
		t.Errorf("empty summary = %q, want %q", got, want)
	}
}

func TestSummaryAndTabStripVisibleOnBothTables(t *testing.T) {
	a := oneTableApp(80, 24)
	a.Update(SnapshotMsg{Runners: []protocol.RunnerInfo{{Status: protocol.RunnerStatus_Busy}}})
	for _, f := range []focus{focusTasks, focusRunners} {
		a.setFocus(f)
		v := a.View()
		if !strings.Contains(v, "runners 1: 0 idle 1 busy") {
			t.Errorf("focus %v: summary missing:\n%s", f, v)
		}
		if len(strings.Split(v, "\n")) != 24 {
			t.Errorf("focus %v: view is %d rows, want 24", f, len(strings.Split(v, "\n")))
		}
	}
}

func bgTask(id byte) protocol.TaskInfo {
	ti := liveTask(id, 0, 0)
	ti.RunMode = protocol.RunMode_Background
	ti.DeadlineNs = 1
	ti.DeadlineRemainingMs = 600000
	return ti
}

func assertRowsMatchColumns(t *testing.T, m *TasksModel) {
	t.Helper()
	for i, r := range m.table.Rows() {
		if len(r) != len(m.table.Columns()) {
			t.Fatalf("row %d has %d cells, table has %d columns (%s)", i, len(r), len(m.table.Columns()), titles(tasksColTitles(m)))
		}
	}
}

func hasCol(m *TasksModel, title string) bool {
	for _, c := range m.table.Columns() {
		if c.Title == title {
			return true
		}
	}
	return false
}

// The Run column exists only while some row has a Run cell.
func TestRunColumnFollowsData(t *testing.T) {
	a := oneTableApp(120, 40)
	a.Update(SnapshotMsg{Tasks: []protocol.TaskInfo{liveTask(1, 0, 0), liveTask(2, 0, 0)}})
	if hasCol(&a.tasks, "Run") {
		t.Fatalf("plain interactive rows must not carry Run: %s", titles(tasksColTitles(&a.tasks)))
	}
	assertRowsMatchColumns(t, &a.tasks)

	a.Update(SnapshotMsg{Tasks: []protocol.TaskInfo{liveTask(1, 0, 0), bgTask(3)}})
	if !hasCol(&a.tasks, "Run") {
		t.Fatalf("a background row must bring Run: %s", titles(tasksColTitles(&a.tasks)))
	}
	assertRowsMatchColumns(t, &a.tasks)
	_ = a.View() // must not panic

	// Back to none, via a task event this time (prune of the bg row).
	a.Update(TaskEventMsg{Event: protocol.TaskStatusEvent{Kind: protocol.StatusEventKind_TaskPruned, TaskId: bgTask(3).Id}})
	if hasCol(&a.tasks, "Run") {
		t.Fatalf("Run must go once the last background row is pruned: %s", titles(tasksColTitles(&a.tasks)))
	}
	assertRowsMatchColumns(t, &a.tasks)
	_ = a.View()
}

// Below the width threshold Run stays out even with data.
func TestRunColumnStillNeedsWidth(t *testing.T) {
	m := NewTasks()
	m.SetSize(runColumnMinWidth-1, 10)
	m.SetRows([]protocol.TaskInfo{bgTask(1)}, nil)
	if hasCol(&m, "Run") {
		t.Fatal("Run must respect runColumnMinWidth")
	}
	assertRowsMatchColumns(t, &m)
}
