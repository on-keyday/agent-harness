package tui

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Bound enrollment (C2): the runner detail shows the binding of the key the
// runner authenticated with, in full, and "unbound" rather than nothing.
func TestRunnerDetailShowsBinding(t *testing.T) {
	var r protocol.RunnerInfo
	if d := formatRunnerDetail(r); !strings.Contains(d, "binding:       unbound\n") {
		t.Errorf("unbound runner detail lacks the binding line:\n%s", d)
	}
	var id protocol.TaskID
	for i := range id.Id {
		id.Id[i] = 0xa1
	}
	r.SetBound([]protocol.TaskID{id})
	if d := formatRunnerDetail(r); !strings.Contains(d, "binding:       "+strings.Repeat("a1", 16)+"\n") {
		t.Errorf("bound runner detail lacks the full bound id:\n%s", d)
	}
}
