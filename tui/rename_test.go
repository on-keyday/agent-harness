package tui

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli/verb"
)

// session rename reaches the TUI's cmdline dispatch and runs on the
// long-lived client (pitfall 3): with no client it reports that, rather than
// dialing a fresh connection.
func TestSessionRenameRunsOnTheLongLivedClient(t *testing.T) {
	a := credApp(120, 40)
	a.client = nil
	cmd := (tuiVerbs{a}).SessionRename(verb.SessionRenameAction{TaskID: strings.Repeat("a", 32), Name: "w"})
	if cmd != nil {
		t.Fatal("a rename with no client produced a command instead of the not-connected line")
	}
	if !strings.Contains(cmdresultText(a), "not connected") {
		t.Fatalf("result pane = %q", cmdresultText(a))
	}
}
