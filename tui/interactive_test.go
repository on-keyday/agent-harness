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
	task.SetDetachable(true)
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
