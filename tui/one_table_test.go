package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"
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

// The tab bar's keys are pressed against the real cycle in TestTabBarFocusOffHintReturnsFocus.

// The Repo cell is cut to the column's FITTED width. Cut to the declared width
// instead, a column fitColumns has narrowed gets cut a second time by bubbles,
// at the end — the repo name — leaving "…aku-security/sisakulint-…".
func TestRepoCellFitsItsFittedColumn(t *testing.T) {
	eastAsian(t)
	const repo = "/home/someone/workspace/an-org-security/sisakulint-long-name"
	for _, w := range []int{80, 120, 160} {
		a := oneTableApp(w, 40)
		tk := liveTask(1, 0, 0)
		tk.RepoPath = []byte(repo)
		a.Update(SnapshotMsg{Tasks: []protocol.TaskInfo{tk}})
		repoW, ri := -1, -1
		for i, c := range a.tasks.table.Columns() {
			if c.Title == "Repo" {
				repoW, ri = c.Width, i
			}
		}
		if ri < 0 {
			t.Fatalf("w=%d: no Repo column", w)
		}
		cell := a.tasks.table.Rows()[0][ri]
		if runewidth.StringWidth(cell) > repoW {
			t.Errorf("w=%d: Repo cell %q is %d wide, column is %d", w, cell, runewidth.StringWidth(cell), repoW)
		}
		if !strings.HasSuffix(cell, "-long-name") {
			t.Errorf("w=%d: Repo cell %q lost the repo name's tail", w, cell)
		}
	}
}

// A resize that keeps the column set still changes the Repo column's fitted
// width, so the cells must follow it.
func TestRepoCellFollowsAResize(t *testing.T) {
	eastAsian(t)
	const repo = "/home/someone/workspace/an-org-security/sisakulint-long-name"
	a := oneTableApp(160, 40)
	tk := liveTask(1, 0, 0)
	tk.RepoPath = []byte(repo)
	a.Update(SnapshotMsg{Tasks: []protocol.TaskInfo{tk}})
	before := len(a.tasks.table.Columns())
	a.width = 100
	a.layout()
	if len(a.tasks.table.Columns()) != before {
		t.Fatalf("the column set changed (%d → %d); pick widths that keep it", before, len(a.tasks.table.Columns()))
	}
	for i, c := range a.tasks.table.Columns() {
		if c.Title == "Repo" {
			if cell := a.tasks.table.Rows()[0][i]; runewidth.StringWidth(cell) > c.Width {
				t.Errorf("after resize: Repo cell %q is %d wide, column is %d", cell, runewidth.StringWidth(cell), c.Width)
			}
		}
	}
}

// eastAsian measures as bubbles does under a CJK locale (LANG=ja_JP…), where
// go-runewidth counts the ambiguous-width "…" as two cells. bubbles truncates
// every cell with runewidth, so a cell must fit by THAT measure.
func eastAsian(t *testing.T) {
	t.Helper()
	old := runewidth.DefaultCondition.EastAsianWidth
	runewidth.DefaultCondition.EastAsianWidth = true
	t.Cleanup(func() { runewidth.DefaultCondition.EastAsianWidth = old })
}

func TestTruncateLeftFitsUnderEastAsianWidth(t *testing.T) {
	eastAsian(t)
	for _, w := range []int{5, 14, 28} {
		got := truncateLeft("/home/someone/workspace/an-org/sisakulint-long-name", w)
		if runewidth.StringWidth(got) > w || !strings.HasSuffix(got, "ame") || runewidth.StringWidth(got) < w-1 {
			t.Errorf("w=%d: %q is %d wide", w, got, runewidth.StringWidth(got))
		}
	}
	if got := truncateLeft("/short", 28); got != "/short" {
		t.Errorf("a fitting string changed: %q", got)
	}
}
