package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/table"
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
	if got := doingCell(progressTask(protocol.TaskKind_Interactive, "stage 2", 1, true, "✳ topic")); got != "stage 2" {
		t.Errorf("with progress = %q, want the line", got)
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
	applyEventProgress(&ti, ev)
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
