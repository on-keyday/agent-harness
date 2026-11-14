package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
)

// A3 (A-D31): `s` and its popup open a detached `session new --oneshot`
// with the typed prompt, where they used to submit a oneshot task.
func TestSKeyOpensOneshotSession(t *testing.T) {
	a := New(Config{})
	var gotRepo string
	var gotMode cli.RunModeFlags
	var called bool
	old := startDetachedSession
	startDetachedSession = func(_ *cli.Client, repo string, _ cli.SelectorOpts, _ []string, _ string, _ Authority, _ bool, _ bool, _ bool, _ string, _ TermSize, eventStream bool, runMode cli.RunModeFlags) tea.Cmd {
		called, gotRepo, gotMode = true, repo, runMode
		if eventStream {
			t.Error("a one-shot was opened as an event stream")
		}
		return func() tea.Msg { return nil }
	}
	t.Cleanup(func() { startDetachedSession = old })

	a.popup.SetRepoChoices([]string{"/r"}, "/r")
	a.popup.Open()
	a.popup.ta.SetValue("answer this")
	if cmd := a.inSubmitPopup(tea.KeyMsg{Type: tea.KeyCtrlJ}); cmd == nil || !called {
		t.Fatal("Ctrl+J did not open a session")
	}
	if gotRepo != "/r" || !gotMode.Oneshot || gotMode.Prompt != "answer this" {
		t.Fatalf("opened repo %q with %+v, want /r, oneshot, the prompt", gotRepo, gotMode)
	}
}
