package tui

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli/verb"
)

// The `reconnect` word parses on the TUI surface into a ScreenAction whose Sub
// is "reconnect" — the declaration is what routes it to tuiVerbs.Reconnect, so a
// missing table row would surface here rather than as a silent no-op.
func TestParseCommand_Reconnect(t *testing.T) {
	act, err := ParseCommand("reconnect", "")
	if err != nil {
		t.Fatalf("parse `reconnect`: %v", err)
	}
	sa, ok := act.(verb.ScreenAction)
	if !ok {
		t.Fatalf("`reconnect` parsed to %T, want verb.ScreenAction", act)
	}
	if sa.Sub != "reconnect" {
		t.Fatalf("ScreenAction.Sub = %q, want \"reconnect\"", sa.Sub)
	}
}

// With no client (initial dial pending / link down), reconnect has nothing to
// re-dial: it must self-guard with a notice and dispatch no command, never
// nil-panic reaching into a nil client's peer.
func TestReconnect_NilClientGuarded(t *testing.T) {
	a := New(Config{}) // client is nil (BindClient never called)
	_, cmd := a.runAction(verb.ScreenAction{Sub: "reconnect"})
	if cmd != nil {
		t.Fatalf("reconnect with a nil client must dispatch no cmd, got one")
	}
	if !strings.Contains(strings.Join(a.cmdresult.lines, "\n"), "not connected") {
		t.Errorf("reconnect with a nil client must post a 'not connected' notice, got:\n%s",
			strings.Join(a.cmdresult.lines, "\n"))
	}
}
