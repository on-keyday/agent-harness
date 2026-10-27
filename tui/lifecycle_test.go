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

// The Life cell shows every lifecycle state, and lc:- for a live task that
// never reported (D28). `wait` is marked by case, not by an ANSI style: bubbles
// truncates cells by width and a style can be cut mid-sequence.
func TestLifeCell(t *testing.T) {
	cases := []struct {
		st   protocol.LifecycleState
		age  uint64
		want string
	}{
		{protocol.LifecycleState_Working, 1000, "work:1s"},
		{protocol.LifecycleState_Waiting, 180000, "WAIT:3m"},
		{protocol.LifecycleState_Idle, 720000, "ready:12m"},
		{protocol.LifecycleState_Error, 30000, "err:30s"},
	}
	for _, c := range cases {
		if got := lifeCell(lifecycleRow(c.st, c.age)); got != c.want {
			t.Errorf("%v = %q, want %q", c.st, got, c.want)
		}
	}
	never := lifecycleRow(protocol.LifecycleState_Unset, 0)
	never.LifecycleAt = 0
	if got := lifeCell(never); got != "lc:-" {
		t.Errorf("never reported = %q, want lc:-", got)
	}
	done := lifecycleRow(protocol.LifecycleState_Waiting, 1000)
	done.Status = protocol.TaskStatus_Succeeded
	if got := lifeCell(done); got != "" {
		t.Errorf("finished = %q, want blank", got)
	}
}

// With its own Life column the Act cell is the activity badge alone; merged
// into a narrow panel, the lifecycle badge takes it.
func TestActCell(t *testing.T) {
	ti := lifecycleRow(protocol.LifecycleState_Working, 1000)
	if got, want := actCell(ti, false), cli.ActivityStr(90000); got != want {
		t.Errorf("own column = %q, want the activity badge %q", got, want)
	}
	if got := actCell(ti, true); got != "work:1s" {
		t.Errorf("merged = %q, want work:1s", got)
	}
	ended := lifecycleRow(protocol.LifecycleState_Unset, 0)
	ended.LastOutputAt, ended.Status = 0, protocol.TaskStatus_Succeeded
	if got := actCell(ended, true); got != "" {
		t.Errorf("ended, no session = %q, want blank", got)
	}
}

func TestLifeColumn(t *testing.T) {
	m := NewTasks()
	m.SetRows([]protocol.TaskInfo{lifecycleRow(protocol.LifecycleState_Idle, 720000)}, nil)
	m.SetSize(120, 10)
	var titles []string
	for _, c := range m.baseCols {
		titles = append(titles, c.Title)
		if c.Title == "Life" && c.Width != 10 {
			t.Errorf("Life width = %d, want 10 (fits ready:999m)", c.Width)
		}
	}
	if titles[4] != "Life" || titles[5] != "Act" {
		t.Fatalf("Life must sit just before Act; columns = %v", titles)
	}
	if got := m.table.Rows()[0][4]; got != "ready:12m" {
		t.Errorf("Life cell = %q, want ready:12m", got)
	}
	m.SetSize(40, 10)
	for _, c := range m.baseCols {
		if c.Title == "Life" {
			t.Error("Life column survived below the threshold")
		}
	}
	if got := m.table.Rows()[0][4]; got != "ready:12m" {
		t.Errorf("narrow Act cell = %q, want the lifecycle badge", got)
	}
}

func TestActColumnWidth(t *testing.T) {
	for name, cols := range map[string]func() []table.Column{"flat": flatCols, "tree": treeCols} {
		for _, c := range cols() {
			if c.Title == "Act" && c.Width != 10 {
				t.Errorf("%s Act width = %d, want 10 (fits ready:999m when merged)", name, c.Width)
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
	if d := formatTaskDetail(never, nil); !strings.Contains(d, "lifecycle:     - (never reported)") {
		t.Errorf("a never-reported lifecycle must read as such (D28):\n%s", d)
	}
	// D14: a finished task must not read "working" — the review found the
	// popup ignored the terminal status the badge and the WebUI respect.
	done := lifecycleRow(protocol.LifecycleState_Working, 1000)
	done.Status = protocol.TaskStatus_Succeeded
	if d := formatTaskDetail(done, nil); strings.Contains(d, "lifecycle:") {
		t.Errorf("a finished task printed a lifecycle line:\n%s", d)
	}
}

func TestTaskDetail_Program(t *testing.T) {
	ti := lifecycleRow(protocol.LifecycleState_Waiting, 1000)
	ti.ProgramState, ti.ProgramKind = protocol.ProgramState_Blocked, protocol.ProgramKind_Question
	ti.SetProgramMsg([]byte("q?"))
	if d := formatTaskDetail(ti, nil); !strings.Contains(d, "program:       blocked (question) — q?") {
		t.Errorf("detail lacks the program line:\n%s", d)
	}
	ti.ProgramState, ti.ProgramKind = protocol.ProgramState_None, protocol.ProgramKind_None
	if d := formatTaskDetail(ti, nil); strings.Contains(d, "program:") {
		t.Errorf("a program that never reported printed a line:\n%s", d)
	}
}
