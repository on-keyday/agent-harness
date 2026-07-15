package tui

import (
	"encoding/hex"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestResumeSelectorOpts(t *testing.T) {
	if got := resumeSelectorOpts(protocol.RunnerID{}); got != (cli.SelectorOpts{}) {
		t.Errorf("zero-value AssignedTo: want Any (empty SelectorOpts), got %+v", got)
	}

	var rid protocol.RunnerID
	rid.SetTransport([]byte("ws"))
	rid.SetIpAddr([]byte{192, 168, 3, 14})
	rid.Port = 37386
	rid.UniqueNumber = 6360
	got := resumeSelectorOpts(rid)
	if got.Runner == "" {
		t.Fatalf("non-zero AssignedTo: want a Runner pin, got empty SelectorOpts")
	}
	if got.Host != "" || got.IP != "" {
		t.Errorf("expected only Runner set, got %+v", got)
	}
	// Round-trips back through the same selector-building path used by
	// --runner on the CLI (cli/selector.go: buildRunnerIDSelector).
	if _, err := cli.BuildSelector(got); err != nil {
		t.Errorf("BuildSelector(%+v): %v", got, err)
	}
}

// TestInteractiveReadyMsg_PickerArmGate guards the fix for the "picker
// mis-arms on out-of-scope interactive-open paths" review finding: only the
// two dispatch sites that populate pendingInteractive (the `S` key and the
// actionResume case of `r`/`R`) may open the runner picker on an
// AmbiguousRunner error. Every other interactive-open path (`i`,
// InteractiveAction, SessionNewAction, X11) must fall back to the flat
// cmdresult error line instead, since pendingInteractive was never (re)armed
// for them and could carry a stale resumeTaskID from a prior r/R.
func TestInteractiveReadyMsg_PickerArmGate(t *testing.T) {
	ambigErr := &cli.AmbiguousRunnerError{Candidates: []cli.RunnerCandidate{
		{Cid: "ws:10.0.0.1:1-1", Hostname: "h1", ActiveTasks: 0, MaxTasks: 8},
		{Cid: "ws:10.0.0.2:1-1", Hostname: "h2", ActiveTasks: 0, MaxTasks: 8},
	}}

	t.Run("unarmed falls back to flat error", func(t *testing.T) {
		a := New(Config{})
		m, _ := a.Update(InteractiveReadyMsg{Err: ambigErr})
		a = m.(*App)
		if a.runnerPicker.IsOpen() {
			t.Fatal("runner picker should NOT open for an unarmed interactive-open path")
		}
		found := false
		for _, line := range a.cmdresult.lines {
			if strings.Contains(line, "open interactive failed") {
				found = true
			}
		}
		if !found {
			t.Fatal("expected a flat 'open interactive failed' cmdresult line")
		}
	})

	t.Run("armed opens the picker", func(t *testing.T) {
		a := New(Config{})
		a.pickerArmed = true
		m, _ := a.Update(InteractiveReadyMsg{Err: ambigErr})
		a = m.(*App)
		if !a.runnerPicker.IsOpen() {
			t.Fatal("runner picker should open when the in-flight open was armed")
		}
		if a.pickerArmed {
			t.Fatal("pickerArmed should be cleared after handling InteractiveReadyMsg")
		}
	})
}

// TestPickerSelection guards the (runner, profile) generalization of the
// ambiguous-runner picker (§4a of the multi-agent-profile design): choosing
// a candidate row must re-issue pinned to BOTH the chosen Cid (as
// SelectorOpts{Runner: cid}) AND the chosen Profile (as the agentProfile
// forwarded to the Do* funnels). Mirrors TestResumeSelectorOpts for the
// picker path.
func TestPickerSelection(t *testing.T) {
	c := &cli.RunnerCandidate{
		Cid:         "ws:10.0.0.2:1-1",
		Hostname:    "gmkhost-codex",
		MatchedRoot: "/home/x/repo",
		ActiveTasks: 1,
		MaxTasks:    8,
		Profile:     "codex",
	}
	sel, agentProfile := pickerSelection(c)
	wantSel := cli.SelectorOpts{Runner: "ws:10.0.0.2:1-1"}
	if sel != wantSel {
		t.Errorf("SelectorOpts = %+v, want %+v", sel, wantSel)
	}
	if agentProfile != c.Profile {
		t.Errorf("agentProfile = %q, want candidate.Profile = %q", agentProfile, c.Profile)
	}
	// Round-trips back through the same selector-building path used
	// elsewhere (--runner / u / U reissue).
	if _, err := cli.BuildSelector(sel); err != nil {
		t.Errorf("BuildSelector(%+v): %v", sel, err)
	}
}

func TestPickerSelectionEmptyProfile(t *testing.T) {
	c := &cli.RunnerCandidate{Cid: "ws:10.0.0.1:1-1", Hostname: "h1"}
	sel, agentProfile := pickerSelection(c)
	if sel.Runner != "ws:10.0.0.1:1-1" {
		t.Errorf("SelectorOpts.Runner = %q, want ws:10.0.0.1:1-1", sel.Runner)
	}
	if agentProfile != "" {
		t.Errorf("agentProfile = %q, want empty (candidate.Profile was empty)", agentProfile)
	}
}

func TestUnpinnedResumeKeyArmsPickerWithAnySelector(t *testing.T) {
	a := New(Config{})
	var tid protocol.TaskID
	for i := range tid.Id {
		tid.Id[i] = byte(i + 1)
	}
	task := protocol.TaskInfo{Id: tid, Status: protocol.TaskStatus_Succeeded, Kind: protocol.TaskKind_Interactive}
	var assigned protocol.RunnerID
	assigned.SetTransport([]byte("ws"))
	assigned.SetIpAddr([]byte{192, 168, 3, 14})
	assigned.Port = 37386
	assigned.UniqueNumber = 6360
	task.AssignedTo = assigned
	a.tasks.SetRows([]protocol.TaskInfo{task}, nil)

	m, cmd := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'u'}})
	a = m.(*App)
	if cmd == nil {
		t.Fatal("u should dispatch a resume command")
	}
	if !a.pickerArmed {
		t.Fatal("u should arm the ambiguous-runner picker")
	}
	wantID := hex.EncodeToString(tid.Id[:])
	if a.pendingInteractive.resumeTaskID != wantID {
		t.Fatalf("resumeTaskID = %q, want %q", a.pendingInteractive.resumeTaskID, wantID)
	}
	if a.pendingInteractive.resumeConversation != true {
		t.Fatal("u should request conversation resume")
	}
}

func TestUnpinnedResumeKeyDoesNotReattachLiveSession(t *testing.T) {
	a := New(Config{})
	var tid protocol.TaskID
	tid.Id[0] = 0xaa
	task := protocol.TaskInfo{Id: tid, Status: protocol.TaskStatus_Detached, Kind: protocol.TaskKind_Interactive}
	a.tasks.SetRows([]protocol.TaskInfo{task}, nil)

	m, cmd := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'u'}})
	a = m.(*App)
	if cmd != nil {
		t.Fatal("u on a live session should not dispatch reattach")
	}
	if a.pickerArmed {
		t.Fatal("u on a live session should not arm the picker")
	}
	found := false
	for _, line := range a.cmdresult.lines {
		if strings.Contains(line, "pick a finished task") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected warning about finished task, got %v", a.cmdresult.lines)
	}
}

// TestRunnerPickerPickDispatchesWithProfile drives the picker digit-press
// through App.Update end to end: opening the picker with (runner, profile)
// combos and pressing a digit must dispatch a command AND surface the picked
// profile in the cmdresult label, confirming app.go's Pick handler actually
// threads pickerSelection's agentProfile through (not just the pure
// function in isolation).
func TestRunnerPickerPickDispatchesWithProfile(t *testing.T) {
	a := New(Config{})
	a.pendingInteractive = pendingInteractive{repo: "/repo"}
	a.runnerPicker.Open([]cli.RunnerCandidate{
		{Cid: "ws:10.0.0.1:1-1", Hostname: "h1", Profile: "claude"},
		{Cid: "ws:10.0.0.2:1-1", Hostname: "h2", Profile: "codex"},
	})

	m, cmd := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
	a = m.(*App)
	if cmd == nil {
		t.Fatal("picking a candidate should dispatch a command")
	}
	if a.runnerPicker.IsOpen() {
		t.Fatal("picker should close after a pick")
	}
	found := false
	for _, line := range a.cmdresult.lines {
		if strings.Contains(line, "h2") && strings.Contains(line, "codex") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a cmdresult line naming h2 and codex, got %v", a.cmdresult.lines)
	}
}
