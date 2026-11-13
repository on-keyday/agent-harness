package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestBgFormPrefillsOneHourDeadline(t *testing.T) {
	f := NewBgForm()
	f.OpenNew("/r", "claude")
	f.prompt.SetValue("triage")
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

func TestRunCell(t *testing.T) {
	old := time.Local
	time.Local = time.UTC
	defer func() { time.Local = old }()
	at := uint64(time.Date(2026, 11, 14, 15, 4, 0, 0, time.UTC).UnixNano())
	bg := func(ti protocol.TaskInfo) protocol.TaskInfo { ti.RunMode = protocol.RunMode_Background; return ti }
	for _, c := range []struct {
		name string
		t    protocol.TaskInfo
		want string
	}{
		{"foreground", protocol.TaskInfo{Status: protocol.TaskStatus_Running, DeadlineNs: at, DeadlineRemainingMs: 60_000}, ""},
		{"queued busy", bg(protocol.TaskInfo{Status: protocol.TaskStatus_Queued, WaitReason: protocol.WaitReason_AllBusy}), "all_busy"},
		{"queued none", bg(protocol.TaskInfo{Status: protocol.TaskStatus_Queued, WaitReason: protocol.WaitReason_NoCandidate}), "no_candidate"},
		{"queued placing", bg(protocol.TaskInfo{Status: protocol.TaskStatus_Queued}), "placing"},
		{"running", bg(protocol.TaskInfo{Status: protocol.TaskStatus_Running, DeadlineNs: at, DeadlineRemainingMs: 65*60_000 + 30_000}), "1h05m 15:04"},
		{"under a minute", bg(protocol.TaskInfo{Status: protocol.TaskStatus_Detached, DeadlineNs: at, DeadlineRemainingMs: 20_000}), "0m 15:04"},
		{"days out", bg(protocol.TaskInfo{Status: protocol.TaskStatus_Running, DeadlineNs: at, DeadlineRemainingMs: (51*60 + 7) * 60_000}), "2d03h 11-14"},
		{"ended", bg(protocol.TaskInfo{Status: protocol.TaskStatus_Cancelled, EndedAt: 1, EndReason: protocol.EndReason_BlockedTimeout}), "blocked_timeout"},
		{"ended before end reasons", bg(protocol.TaskInfo{Status: protocol.TaskStatus_Failed, EndedAt: 1}), "unknown"},
	} {
		if got := runCell(c.t); got != c.want {
			t.Errorf("%s: runCell = %q, want %q", c.name, got, c.want)
		}
	}
}

// A-D14 on the task list itself, at the widths terminals actually have: the
// run facts used to lead the About column, which is about 0 cells wide below
// ~240 terminal columns, so they were readable only in the `d` popup.
func TestTaskListShowsRunFactsAtCommonWidths(t *testing.T) {
	old := time.Local
	time.Local = time.UTC
	defer func() { time.Local = old }()
	at := uint64(time.Date(2026, 11, 14, 15, 4, 0, 0, time.UTC).UnixNano())
	running := protocol.TaskInfo{Status: protocol.TaskStatus_Running, RunMode: protocol.RunMode_Background,
		DeadlineNs: at, DeadlineRemainingMs: 65 * 60_000, Kind: protocol.TaskKind_Interactive}
	running.Id.Id[0] = 1
	queued := protocol.TaskInfo{Status: protocol.TaskStatus_Queued, RunMode: protocol.RunMode_Background,
		WaitReason: protocol.WaitReason_NoCandidate, Kind: protocol.TaskKind_Interactive}
	queued.Id.Id[0] = 2
	for _, w := range []int{144, 160, 200} {
		a := New(Config{Server: "ws://test:8080"})
		a.width, a.height = w, 40
		a.layout()
		a.tasks.SetRows([]protocol.TaskInfo{running, queued}, nil)
		view := a.tasks.View()
		for _, want := range []string{"1h05m 15:04", "no_candidate"} {
			if !strings.Contains(view, want) {
				t.Errorf("terminal %d: task list does not show %q:\n%s", w, want, view)
			}
		}
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

// Review I3: the cap is 16384 BYTES (protocol.PromptMax), so the form neither
// cuts a long paste down to it silently nor lets a multi-byte prompt under the
// rune count through; it refuses at Ctrl+J, with the cap in the message.
func TestBgFormRefusesAnOversizePromptInBytes(t *testing.T) {
	f := NewBgForm()
	f.OpenNew("/r", "claude")
	f.prompt.SetValue(strings.Repeat("a", protocol.PromptMax+100))
	if _, _, err := f.Values(); err == nil || !strings.Contains(err.Error(), "16384") {
		t.Fatalf("an over-cap paste: err %v, want a refusal naming the cap", err)
	}
	f.prompt.SetValue(strings.Repeat("あ", 6000)) // 6000 runes, 18000 bytes
	if _, _, err := f.Values(); err == nil {
		t.Fatal("a multi-byte prompt over the byte cap passed the form")
	}
}

// What a background open cannot run without is refused in the form, not after
// a round trip that would lose what was typed.
func TestBgFormRequiresPromptAndAgentOnANewOpen(t *testing.T) {
	f := NewBgForm()
	f.OpenNew("/r", "")
	f.prompt.SetValue("triage")
	if _, _, err := f.Values(); err == nil || !strings.Contains(err.Error(), "agent") {
		t.Fatalf("no agent: err %v", err)
	}
	f.fields[bgFieldAgent].SetValue("claude")
	f.prompt.SetValue("")
	if _, _, err := f.Values(); err == nil || !strings.Contains(err.Error(), "prompt") {
		t.Fatalf("no prompt: err %v", err)
	}
}

// Review I3: the form stays open until the server answers, so a refusal leaves
// the typed prompt in place with the reason shown; a success closes it.
func TestBgFormStaysOpenUntilTheServerAnswers(t *testing.T) {
	a := New(Config{})
	a.bgform.OpenNew("/r", "claude")
	a.bgform.prompt.SetValue("triage new issues")
	m, cmd := a.Update(tea.KeyMsg{Type: tea.KeyCtrlJ})
	a = m.(*App)
	if cmd == nil || !a.bgform.IsOpen() {
		t.Fatalf("after Ctrl+J: cmd %v, form open %v; want a request and the form still open", cmd != nil, a.bgform.IsOpen())
	}
	m, _ = a.Update(SessionStartedMsg{Err: errors.New("profile_unavailable")})
	a = m.(*App)
	if !a.bgform.IsOpen() || a.bgform.prompt.Value() != "triage new issues" || !strings.Contains(a.bgform.View(), "profile_unavailable") {
		t.Fatalf("after a refusal: open %v prompt %q", a.bgform.IsOpen(), a.bgform.prompt.Value())
	}
	m, _ = a.Update(tea.KeyMsg{Type: tea.KeyCtrlJ})
	a = m.(*App)
	m, _ = a.Update(SessionStartedMsg{TaskID: "abc"})
	a = m.(*App)
	if a.bgform.IsOpen() {
		t.Fatal("the form stayed open after the server accepted it")
	}
}

// Review I4: r/R resume where the task last ran, as they do for a foreground
// task; u/U leave it to placement. A pin to a runner that is no longer
// connected would queue forever (a background open is never refused for no
// runner), so it is dropped then.
func TestBackgroundResumePinsTheAssignedRunnerWhileConnected(t *testing.T) {
	var rid protocol.RunnerID
	rid.Id[0] = 0x7e
	withRunner := func(connected bool) *App {
		a := bgTaskApp(t, protocol.TaskStatus_Cancelled, protocol.RunMode_Background)
		info := *a.tasks.SelectedTask()
		info.AssignedTo = rid
		a.tasks.SetRows([]protocol.TaskInfo{info}, nil)
		if connected {
			a.runners.SetRows([]protocol.RunnerInfo{{Id: rid}})
		}
		return a
	}
	a := withRunner(true)
	m, _ := a.Update(keyRune('r'))
	if got := m.(*App).bgform.Pin(); got != rid {
		t.Fatalf("r: pin %v, want the assigned runner", got)
	}
	a = withRunner(true)
	m, _ = a.Update(keyRune('u'))
	if got := m.(*App).bgform.Pin(); !got.IsZero() {
		t.Fatalf("u: pin %v, want none", got)
	}
	a = withRunner(false)
	m, _ = a.Update(keyRune('r'))
	if got := m.(*App).bgform.Pin(); !got.IsZero() {
		t.Fatalf("r with the runner gone: pin %v, want none", got)
	}
}
