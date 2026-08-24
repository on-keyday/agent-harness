package tui

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func testRunnerID(t *testing.T) protocol.RunnerID {
	t.Helper()
	var rid protocol.RunnerID
	rid.SetTransport([]byte("ws"))
	rid.SetIpAddr([]byte{127, 0, 0, 1})
	rid.Port = 8540
	rid.UniqueNumber = 7
	return rid
}

// A workspace with runner = any must not pin, and with runner = assigned must
// pin to the task's own runner. The selector choice is the whole difference
// between the two values, so it is what this pins.
func TestResumeSelectorOptsForWorkspace(t *testing.T) {
	rid := testRunnerID(t)

	if got := workspaceResumeOpts(rid, false); got.Runner == "" {
		t.Error("assigned: want a pinned selector, got the Any selector")
	}
	if got := workspaceResumeOpts(rid, true); got.Runner != "" {
		t.Errorf("any: want the Any selector, got Runner=%q", got.Runner)
	}
	// A task that was never assigned has no runner to pin to; assigned must
	// then degrade to Any rather than building a selector out of a zero id.
	var never protocol.RunnerID
	if got := workspaceResumeOpts(never, false); got.Runner != "" {
		t.Errorf("assigned with no AssignedTo: want the Any selector, got Runner=%q", got.Runner)
	}
}
