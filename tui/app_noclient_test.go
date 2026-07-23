package tui

import (
	"strings"
	"testing"
)

// TestRunAction_NilClientGuarded verifies that client-requiring cmdline
// actions do NOT dispatch a Do* command when the client is nil (initial dial
// still pending / failed under --persist). Before the guard, runAction returned
// the Do* closure verbatim; bubbletea then executed it and cli.(*Client).<RPC>
// nil-panicked (observed as a runtime panic in the TUI on `prune <id>` while
// disconnected). The guard must turn each into a no-op + a "not connected" line.
func TestRunAction_NilClientGuarded(t *testing.T) {
	id := "0123456789abcdef0123456789abcdef"
	cases := []struct {
		name string
		act  Action
	}{
		{"prune-id", PruneAction{TaskIDs: []string{id}}},
		{"prune-time", PruneAction{Before: 0}},
		{"cancel", CancelAction{IDPrefix: id}},
		{"submit", SubmitAction{Repo: "/r", Prompt: "hi"}},
		{"notify", NotifyAction{Title: "t", Text: "x"}},
		{"session-ls", SessionLsAction{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := New(Config{}) // client is nil (BindClient never called)
			_, cmd := a.runAction(tc.act)
			if cmd != nil {
				t.Fatalf("%s: expected no dispatched cmd with a nil client, got one (would nil-panic on execute)", tc.name)
			}
			if !strings.Contains(strings.Join(a.cmdresult.lines, "\n"), "not connected") {
				t.Errorf("%s: expected a 'not connected' notice, got:\n%s", tc.name, strings.Join(a.cmdresult.lines, "\n"))
			}
		})
	}
}

// TestRunAction_NilClientAllowsLocalActions verifies the guard does NOT block
// actions that need no client, so the TUI stays usable while disconnected.
func TestRunAction_NilClientAllowsLocalActions(t *testing.T) {
	for _, tc := range []struct {
		name string
		act  Action
	}{
		{"help", HelpAction{}},
		{"clear", ClearAction{}},
		{"caps-show", CapsAction{Show: true}},
		{"repo", RepoAction{Path: "/tmp"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := New(Config{})
			// Must not panic and must not emit the not-connected notice.
			a.runAction(tc.act)
			if strings.Contains(strings.Join(a.cmdresult.lines, "\n"), "not connected") {
				t.Errorf("%s: client-free action was wrongly gated as not-connected", tc.name)
			}
		})
	}
}
