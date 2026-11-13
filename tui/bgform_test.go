package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestBgFormPrefillsOneHourDeadline(t *testing.T) {
	f := NewBgForm()
	f.OpenNew("/r", "claude")
	flags, agent, err := f.Values()
	if err != nil {
		t.Fatal(err)
	}
	if !flags.Background || flags.Deadline != time.Hour || agent != "claude" {
		t.Fatalf("flags %+v agent %q", flags, agent)
	}
}

func bgTaskApp(t *testing.T, status protocol.TaskStatus, mode protocol.RunMode) *App {
	t.Helper()
	a := New(Config{})
	var tid protocol.TaskID
	tid.Id[0] = 0xbc
	info := protocol.TaskInfo{Id: tid, Status: status, RunMode: mode, Kind: protocol.TaskKind_Interactive,
		Conditions: protocol.BackgroundConditions{DeadlineMs: 7200000, QuietMs: 60000, TurnEnd: protocol.TurnEndSignal_Lifecycle}}
	info.SetPrompt([]byte("triage new issues"))
	info.SetAgentProfile([]byte("codex"))
	a.tasks.SetRows([]protocol.TaskInfo{info}, nil)
	return a
}

// Review Focus 1: a resume key on a background task opens the background form
// prefilled from the task, never an attached open that would be refused
// prompt_required.
func TestResumeKeyOnBackgroundTaskOpensPrefilledForm(t *testing.T) {
	a := bgTaskApp(t, protocol.TaskStatus_Cancelled, protocol.RunMode_Background)
	m, cmd := a.Update(keyRune('r'))
	a = m.(*App)
	if !a.bgform.IsOpen() {
		t.Fatalf("form not open (cmd %v)", cmd != nil)
	}
	flags, agent, err := a.bgform.Values()
	if err != nil {
		t.Fatal(err)
	}
	if flags.Prompt != "triage new issues" || agent != "codex" || flags.Deadline != 2*time.Hour ||
		flags.Quiet != time.Minute || flags.TurnEnd != protocol.TurnEndSignal_Lifecycle || a.bgform.ResumeID() == "" {
		t.Fatalf("prefill: %+v agent %q resume %q", flags, agent, a.bgform.ResumeID())
	}
}

// E on a running foreground session opens the form for the conditions only;
// on a background one it switches to foreground at once.
func TestRunModeKey(t *testing.T) {
	a := bgTaskApp(t, protocol.TaskStatus_Detached, protocol.RunMode_Foreground)
	m, _ := a.Update(keyRune('E'))
	a = m.(*App)
	if !a.bgform.IsOpen() || !a.bgform.IsSwitch() {
		t.Fatal("E on a foreground session must open the switch form")
	}
	b := bgTaskApp(t, protocol.TaskStatus_Detached, protocol.RunMode_Background)
	m, cmd := b.Update(keyRune('E'))
	b = m.(*App)
	if b.bgform.IsOpen() || cmd == nil {
		t.Fatalf("E on a background session must switch at once (form open %v, cmd %v)", b.bgform.IsOpen(), cmd != nil)
	}
}

func TestTaskRowShowsRunFields(t *testing.T) {
	info := protocol.TaskInfo{Status: protocol.TaskStatus_Queued, RunMode: protocol.RunMode_Background, WaitReason: protocol.WaitReason_AllBusy}
	if got := aboutCell(info); !strings.Contains(got, "wait=all_busy") {
		t.Fatalf("about cell %q", got)
	}
	ended := protocol.TaskInfo{Status: protocol.TaskStatus_Cancelled, EndedAt: 1, RunMode: protocol.RunMode_Background, EndReason: protocol.EndReason_BlockedTimeout}
	if got := aboutCell(ended); !strings.Contains(got, "end=blocked_timeout") {
		t.Fatalf("about cell %q", got)
	}
}

// While attached in the grid (A-D15, A-D19): E on the focused pane switches
// its run mode as the task-row key does, the form drawn over the grid.
func TestGridRunModeKeyOpensSwitchFormOverGrid(t *testing.T) {
	a := bgTaskApp(t, protocol.TaskStatus_Detached, protocol.RunMode_Foreground)
	info := *a.tasks.SelectedTask()
	a.tasksByID = map[string]protocol.TaskInfo{FormatTaskID(info.Id): info}
	a.grid.panes = []gridPane{NewPaneStreamer(FormatTaskID(info.Id), 24, 80)}
	a.grid.open = true
	a.grid.SetSize(100, 30)
	_, cmd := a.grid.Update(keyMsg("E"))
	if cmd == nil {
		t.Fatal("E in the grid returned no message")
	}
	m, _ := a.Update(cmd())
	a = m.(*App)
	if !a.bgform.IsOpen() || !a.bgform.IsSwitch() || a.bgform.SwitchID() != FormatTaskID(info.Id) {
		t.Fatalf("form open %v switch %v id %q", a.bgform.IsOpen(), a.bgform.IsSwitch(), a.bgform.SwitchID())
	}
	// The form, not the grid, takes the next key.
	m, _ = a.Update(tea.KeyMsg{Type: tea.KeyEsc})
	a = m.(*App)
	if a.bgform.IsOpen() || !a.grid.IsOpen() {
		t.Fatalf("esc: form open %v, grid open %v", a.bgform.IsOpen(), a.grid.IsOpen())
	}
}
