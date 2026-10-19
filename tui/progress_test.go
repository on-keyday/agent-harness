package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func progressTask(kind protocol.TaskKind, progress string, at uint64, live bool, title string) protocol.TaskInfo {
	var t protocol.TaskInfo
	t.Id.Id[0] = 0x22
	t.Kind = kind
	t.Status = protocol.TaskStatus_Running
	t.SetPrompt([]byte("the original ask"))
	t.SetProgress([]byte(progress))
	t.ProgressAt = at
	t.ProgressAgeMs = 720000
	t.SetLiveTerminal(live)
	t.SetTitle([]byte(title))
	return t
}

// The last column says what the task is doing now: its own line first, then
// its program's title (marked, so the two are told apart), then the prompt.
func TestDoingCell(t *testing.T) {
	if got := doingCell(progressTask(protocol.TaskKind_Interactive, "stage 2", 1, true, "✳ topic")); got != "stage 2 (12m ago)" {
		t.Errorf("with progress = %q, want the line with its age (D4: the reader judges staleness)", got)
	}
	if got := doingCell(progressTask(protocol.TaskKind_Interactive, "", 0, true, "✳ topic")); got != "(title) ✳ topic" {
		t.Errorf("title only = %q, want it marked", got)
	}
	if got := doingCell(progressTask(protocol.TaskKind_Oneshot, "", 0, false, "")); got != "the original ask" {
		t.Errorf("neither = %q, want the prompt", got)
	}
	// A cleared line is a write: it says "nothing to report", and the cell
	// falls through to the title rather than resurrecting a stale line.
	if got := doingCell(progressTask(protocol.TaskKind_Interactive, "", 9, true, "✳ topic")); got != "(title) ✳ topic" {
		t.Errorf("cleared = %q, want the title", got)
	}
}

func TestApplyEventProgress(t *testing.T) {
	var ti protocol.TaskInfo
	ev := protocol.TaskStatusEvent{TaskStatus: protocol.TaskStatus_Running, ProgressAt: 7, ProgressAgeMs: 3}
	ev.SetProgress([]byte("s3"))
	ev.SetLiveTerminal(true)
	ev.SetTitle([]byte("t"))
	New(Config{}).applyEventProgress(&ti, "x", ev)
	if string(ti.Progress) != "s3" || ti.ProgressAt != 7 || ti.ProgressAgeMs != 3 || !ti.LiveTerminal() || string(ti.Title) != "t" {
		t.Fatalf("row after event = progress %q@%d age %d live %v title %q", ti.Progress, ti.ProgressAt, ti.ProgressAgeMs, ti.LiveTerminal(), ti.Title)
	}
}

func TestTaskDetail_ProgressAndTitle(t *testing.T) {
	d := formatTaskDetail(progressTask(protocol.TaskKind_Interactive, "stage 2", 1, true, ""), nil)
	if !strings.Contains(d, "progress:") || !strings.Contains(d, `"stage 2" (12m ago)`) {
		t.Errorf("detail lacks the progress line:\n%s", d)
	}
	if !strings.Contains(d, "title:") {
		t.Errorf("a live terminal's detail lacks the title line even when empty:\n%s", d)
	}
}

func TestIdleWatcherRow_Condition(t *testing.T) {
	now := time.Now()
	idle := idleWatcherRow(&protocol.AwaitIdleWatcherInfo{WatcherId: 1, Condition: protocol.AwaitCondition_Idle, ThresholdMs: 2500}, now)
	prog := idleWatcherRow(&protocol.AwaitIdleWatcherInfo{WatcherId: 2, Condition: protocol.AwaitCondition_Progress}, now)
	if idle[3] != "idle 2500ms" || prog[3] != "progress" {
		t.Errorf("condition cells = %q / %q", idle[3], prog[3])
	}
}

// flexColumn falls back to the last column when the title is missing, so a
// renamed header would keep "working" by accident; both column sets must
// actually carry the title the resize code asks for.
func TestTasksFlexColumnIsNamed(t *testing.T) {
	for name, cols := range map[string]func() []table.Column{"flat": flatCols, "tree": treeCols} {
		found := false
		for _, c := range cols() {
			if c.Title == "Doing" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s columns have no \"Doing\" column for flexColumn to find", name)
		}
	}
}

// A forged task event can carry any bytes (pubsub lets a subscriber publish),
// and bubbles passes a cell's bytes to the terminal as they are: the cell
// strips control characters itself rather than trusting the server's rules.
func TestDoingCell_StripsControls(t *testing.T) {
	if got := doingCell(progressTask(protocol.TaskKind_Interactive, "\x1b[2Jx\u009b", 1, true, "")); got != "[2Jx (12m ago)" {
		t.Errorf("progress cell = %q, want the ESC and C1 gone", got)
	}
	if got := doingCell(progressTask(protocol.TaskKind_Interactive, "", 0, true, "\x1b]0;t\x07")); got != "(title) ]0;t" {
		t.Errorf("title cell = %q, want the controls gone", got)
	}
}

// The age arrives frozen in a snapshot or event; the TUI fetches nothing on
// its own, so the row must be aged locally as the idle badge is.
func TestProgressAgeAdvancesLocally(t *testing.T) {
	a := New(Config{})
	var ti protocol.TaskInfo
	ti.Id.Id[0] = 0x33
	ti.Status = protocol.TaskStatus_Running
	ti.SetProgress([]byte("stage"))
	ti.ProgressAt = 5
	ti.ProgressAgeMs = 1000
	a.Update(SnapshotMsg{Tasks: []protocol.TaskInfo{ti}})
	id := FormatTaskID(ti.Id)
	rt, ok := a.progressRecvAt[id]
	if !ok {
		t.Fatal("snapshot did not stamp the progress receipt time")
	}
	a.progressRecvAt[id] = rt.Add(-10 * time.Minute)
	a.refreshTasksTable()
	sel := a.tasks.SelectedTask()
	if sel == nil || sel.ProgressAgeMs < 600000 {
		t.Fatalf("rendered age = %v, want >= 10m after 10m locally", sel)
	}
}

// X empties the selected task's line (the server checks cancel + scope); the
// key exists in the tasks pane and says what it does when nothing is selected.
func TestProgressClearKey(t *testing.T) {
	found := false
	for _, b := range mainKeyBindings {
		for _, k := range b.Keys {
			if k == mainKeys.ProgressClear && b.Scope == scopeTasks {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("no tasks-pane binding for the progress-clear key")
	}
	a := New(Config{})
	if _, handled := a.onProgressClear(tea.KeyMsg{}); !handled {
		t.Fatal("onProgressClear did not handle the key")
	}
	if !strings.Contains(strings.Join(a.cmdresult.lines, "\n"), "no task selected") {
		t.Errorf("no selection should say so, got %q", a.cmdresult.lines)
	}
}

// A cleared line shows in the detail popup as an empty quoted line with its
// age — the quotes keep it apart from a task that wrote "(cleared)".
func TestTaskDetail_ClearedLineShownQuoted(t *testing.T) {
	d := formatTaskDetail(progressTask(protocol.TaskKind_Interactive, "", 9, false, ""), nil)
	if !strings.Contains(d, `progress:      "" (12m ago)`) {
		t.Errorf("detail lacks the cleared line:\n%s", d)
	}
}
