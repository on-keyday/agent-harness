package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/verb"
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

// detachedCall is one startDetachedSession call, as a test stub saw it.
type detachedCall struct {
	repo    string
	sel     cli.SelectorOpts
	agent   string
	runMode cli.RunModeFlags
}

// stubDetachedStart records every startDetachedSession call. The first one's
// Cmd answers ambiguous_runner over twoRunners; later ones answer a task id.
func stubDetachedStart(t *testing.T) *[]detachedCall {
	t.Helper()
	var calls []detachedCall
	old := startDetachedSession
	startDetachedSession = func(_ *cli.Client, repo string, sel cli.SelectorOpts, _ []string, _ string, _ Authority, _ bool, _ bool, _ bool, agent string, _ TermSize, _ bool, runMode cli.RunModeFlags) tea.Cmd {
		calls = append(calls, detachedCall{repo: repo, sel: sel, agent: agent, runMode: runMode})
		first := len(calls) == 1
		return func() tea.Msg {
			if first {
				return SessionStartedMsg{Err: &cli.AmbiguousRunnerError{Candidates: twoRunners}}
			}
			return SessionStartedMsg{TaskID: "t"}
		}
	}
	t.Cleanup(func() { startDetachedSession = old })
	return &calls
}

// An ambiguous answer to a detached start opens the runner picker, and a pick
// re-issues the SAME start pinned to the picked runner and agent: the
// --oneshot flag and the prompt survive the re-issue. Both surfaces that open
// a one-shot — the `s` popup and the command line's `session new -d
// --oneshot` — go through it (A3 retired submit, whose result carried this
// retry).
func TestAmbiguousDetachedStartReissuesFromThePicker(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start func(a *App) tea.Cmd
	}{
		{"s popup", func(a *App) tea.Cmd {
			a.popup.SetRepoChoices([]string{"/r"}, "/r")
			a.popup.Open()
			a.popup.ta.SetValue("answer this")
			return a.inSubmitPopup(tea.KeyMsg{Type: tea.KeyCtrlJ})
		}},
		{"session new -d", func(a *App) tea.Cmd {
			return (tuiVerbs{a}).SessionNew(verb.SpawnAction{Repo: "/r", Detach: true, Oneshot: true, Prompt: "answer this"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := stubDetachedStart(t)
			a := New(Config{})
			cmd := tc.start(a)
			if cmd == nil || len(*calls) != 1 {
				t.Fatalf("start: cmd=%v, %d call(s), want one", cmd != nil, len(*calls))
			}
			m, _ := a.Update(cmd())
			a = m.(*App)
			if !a.runnerPicker.IsOpen() {
				t.Fatalf("ambiguous detached start: picker not open; result pane %q", a.cmdresult.lines)
			}
			a.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'2'}}) // runner b: one agent, fires
			if len(*calls) != 2 {
				t.Fatalf("pick re-issued %d time(s), want once", len(*calls)-1)
			}
			first, re := (*calls)[0], (*calls)[1]
			if re.sel != twoRunners[1].Pin() || re.agent != "claude" {
				t.Fatalf("re-issue pinned to %+v / %q, want %+v / claude", re.sel, re.agent, twoRunners[1].Pin())
			}
			if re.repo != first.repo || re.runMode != first.runMode || !re.runMode.Oneshot || re.runMode.Prompt != "answer this" {
				t.Fatalf("re-issue %+v, want the first start's repo and run mode %+v (oneshot, the prompt)", re, first)
			}
		})
	}
}
