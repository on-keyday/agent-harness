package tui

import (
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
	rid.Id = [16]byte{216, 24}
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

// An ambiguous result opens the picker only when it carries a retry; one
// without (the `i`, InteractiveAction, X11 paths) stays a flat error. The
// retry rides the failing request's own message, so there is no armed flag
// that a slow, unrelated open could find set.
func TestAmbiguousResultOpensPickerOnlyWithRetry(t *testing.T) {
	are := &cli.AmbiguousRunnerError{Candidates: twoRunners}
	var r pickRec

	a := New(Config{})
	m, _ := a.Update(InteractiveReadyMsg{Err: are})
	a = m.(*App)
	if a.runnerPicker.IsOpen() {
		t.Fatal("no retry: picker must not open")
	}
	found := false
	for _, line := range a.cmdresult.lines {
		if strings.Contains(line, "open interactive failed") {
			found = true
		}
	}
	if !found {
		t.Fatal("no retry: expected the flat 'open interactive failed' line")
	}

	m, _ = a.Update(InteractiveReadyMsg{Err: are, Retry: r.retry})
	a = m.(*App)
	if !a.runnerPicker.IsOpen() {
		t.Fatal("retry present: picker must open")
	}
	a.runnerPicker.Close()

	m, _ = a.Update(SubmitResultMsg{Err: are, Retry: r.retry})
	a = m.(*App)
	if !a.runnerPicker.IsOpen() {
		t.Fatal("submit result with a retry must open the picker")
	}
}

// A pick re-issues over the client the TUI holds WHEN THE PICK HAPPENS. A
// reconnect between the ambiguous answer and the keypress replaces a.client
// (BindClientMsg); the old one is dead.
func TestPickerRetryUsesTheClientAtPickTime(t *testing.T) {
	first, second := &cli.Client{}, &cli.Client{}
	var r pickRec
	a := New(Config{})
	a.client = first
	m, _ := a.Update(InteractiveReadyMsg{Err: &cli.AmbiguousRunnerError{Candidates: twoRunners}, Retry: r.retry})
	a = m.(*App)
	m, _ = a.Update(BindClientMsg{Client: second})
	a = m.(*App)
	a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}}) // runner b: one agent, fires
	if r.n != 1 || r.client != second {
		t.Fatalf("retry ran %d time(s) with client %p, want the rebound %p", r.n, r.client, second)
	}
}

// withRetry stamps the retry onto the result of the request it wraps.
func TestWithRetryStampsTheResult(t *testing.T) {
	var r pickRec
	cmd := withRetry(func() tea.Msg { return InteractiveReadyMsg{TaskID: "t"} }, r.retry)
	msg, ok := cmd().(InteractiveReadyMsg)
	if !ok || msg.Retry == nil || msg.TaskID != "t" {
		t.Fatalf("msg=%+v", msg)
	}
}

func TestUnpinnedResumeKeyDispatchesAResume(t *testing.T) {
	a := New(Config{})
	var tid protocol.TaskID
	for i := range tid.Id {
		tid.Id[i] = byte(i + 1)
	}
	task := protocol.TaskInfo{Id: tid, Status: protocol.TaskStatus_Succeeded, Kind: protocol.TaskKind_Interactive}
	var assigned protocol.RunnerID
	assigned.Id = [16]byte{216, 24}
	task.AssignedTo = assigned
	a.tasks.SetRows([]protocol.TaskInfo{task}, nil)

	m, cmd := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'u'}})
	a = m.(*App)
	if cmd == nil {
		t.Fatal("u should dispatch a resume command")
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

// TestRunnerPickerPickDispatchesWithProfile drives a pick through App.Update:
// the digit reaches the picker, the retry is called with the chosen agent,
// and the result line names the runner and the agent.
func TestRunnerPickerPickDispatchesWithProfile(t *testing.T) {
	a := New(Config{})
	var r pickRec
	a.runnerPicker.Open([]cli.RunnerCandidate{
		{Cid: "ws:10.0.0.1:1-1", Hostname: "h1", Profiles: []string{"claude"}},
		{Cid: "ws:10.0.0.2:1-1", Hostname: "h2", Profiles: []string{"codex"}},
	}, r.retry, nil)

	m, cmd := a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}})
	a = m.(*App)
	if cmd == nil || r.agent != "codex" || r.sel.Runner != "ws:10.0.0.2:1-1" {
		t.Fatalf("cmd=%v pick=%+v", cmd != nil, r)
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
