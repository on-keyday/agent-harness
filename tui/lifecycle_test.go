package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/table"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func lifecycleRow(st protocol.LifecycleState, ageMs uint64) protocol.TaskInfo {
	t := progressTask(protocol.TaskKind_Interactive, "", 0, true, "")
	t.LastOutputAt = 1
	t.OutputIdleMs = 90000
	t.Lifecycle = st
	t.LifecycleAt = 3
	t.LifecycleAgeMs = ageMs
	return t
}

// The Act cell shows the lifecycle badge when there is one, the activity badge
// otherwise. `wait` is marked by case, not by an ANSI style: bubbles truncates
// cells by width and a style can be cut mid-sequence.
func TestActCell_Lifecycle(t *testing.T) {
	if got := actCell(lifecycleRow(protocol.LifecycleState_Waiting, 180000)); got != "WAIT:3m" {
		t.Errorf("waiting = %q, want WAIT:3m", got)
	}
	if got := actCell(lifecycleRow(protocol.LifecycleState_Idle, 720000)); got != "ready:12m" {
		t.Errorf("idle = %q, want ready:12m", got)
	}
	if got, want := actCell(lifecycleRow(protocol.LifecycleState_Working, 1000)), cli.ActivityStr(90000); got != want {
		t.Errorf("working = %q, want the activity badge %q", got, want)
	}
	noSession := lifecycleRow(protocol.LifecycleState_Unset, 0)
	noSession.LastOutputAt, noSession.LifecycleAt = 0, 0
	if got := actCell(noSession); got != "" {
		t.Errorf("no session, no lifecycle = %q, want blank", got)
	}
}

func TestActColumnWidth(t *testing.T) {
	for name, cols := range map[string]func() []table.Column{"flat": flatCols, "tree": treeCols} {
		for _, c := range cols() {
			if c.Title == "Act" && c.Width != 10 {
				t.Errorf("%s Act width = %d, want 10 (fits ready:999m)", name, c.Width)
			}
		}
	}
}

func TestApplyEventProgress_CopiesLifecycleAndProgram(t *testing.T) {
	var ti protocol.TaskInfo
	ev := protocol.TaskStatusEvent{TaskStatus: protocol.TaskStatus_Running,
		Lifecycle: protocol.LifecycleState_Waiting, LifecycleAt: 7, LifecycleAgeMs: 3,
		ProgramState: protocol.ProgramState_Blocked, ProgramKind: protocol.ProgramKind_Question}
	ev.SetProgramMsg([]byte("q?"))
	New(Config{}).applyEventProgress(&ti, "x", ev)
	if ti.Lifecycle != protocol.LifecycleState_Waiting || ti.LifecycleAt != 7 || ti.LifecycleAgeMs != 3 ||
		ti.ProgramState != protocol.ProgramState_Blocked || ti.ProgramKind != protocol.ProgramKind_Question || string(ti.ProgramMsg) != "q?" {
		t.Fatalf("row after event = %+v", ti)
	}
}

func TestAgedTask_AgesLifecycle(t *testing.T) {
	a := New(Config{})
	ti := lifecycleRow(protocol.LifecycleState_Waiting, 1000)
	a.lifecycleRecvAt = map[string]time.Time{"x": time.Now().Add(-2 * time.Second)}
	if got := a.agedTask("x", ti, time.Now()); got.LifecycleAgeMs < 2900 {
		t.Fatalf("aged lifecycle age = %d, want >= ~3000", got.LifecycleAgeMs)
	}
}

func TestTaskDetail_Lifecycle(t *testing.T) {
	d := formatTaskDetail(lifecycleRow(protocol.LifecycleState_Waiting, 180000), nil)
	if !strings.Contains(d, "lifecycle:     waiting (3m ago)") {
		t.Errorf("detail lacks the lifecycle line:\n%s", d)
	}
	never := lifecycleRow(protocol.LifecycleState_Unset, 0)
	never.LifecycleAt = 0
	if d := formatTaskDetail(never, nil); strings.Contains(d, "lifecycle:") {
		t.Errorf("a never-reported lifecycle printed a line:\n%s", d)
	}
}
