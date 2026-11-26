package cli

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestExecSnapshotRowCarriesRawAndRendered(t *testing.T) {
	row := ExecSnapshotRow(&protocol.ExecRunInfo{ExecId: 4, StdinBytes: 10, Taps: 1})
	for _, k := range []string{"exec_id", "task", "started_unix_ms", "origin", "command",
		"stdin_bytes", "stdout_bytes", "stderr_bytes", "last_activity_unix_ms", "taps", "traffic"} {
		if _, ok := row[k]; !ok {
			t.Fatalf("row lacks %q", k)
		}
	}
	if row["traffic"] != ExecRunTrafficLine(&protocol.ExecRunInfo{ExecId: 4, StdinBytes: 10, Taps: 1}) {
		t.Fatalf("traffic must be exactly the CLI line: %v", row["traffic"])
	}
}

func TestExecSnapshotRowCarriesPty(t *testing.T) {
	var e protocol.ExecRunInfo
	e.SetPty(true)
	row := ExecSnapshotRow(&e)
	if row["pty"] != true || !strings.HasPrefix(row["traffic"].(string), "io=pty") {
		t.Fatalf("snapshot row: pty=%v traffic=%v", row["pty"], row["traffic"])
	}
}

// The browser's exec row names the action an exec runs, and says "" for a raw
// argv: the same label `exec ls` prints.
func TestExecSnapshotRowCarriesAction(t *testing.T) {
	var e protocol.ExecRunInfo
	e.SetActionPresent(true)
	e.SetActionId(protocol.ActionID{Id: [16]byte{1}})
	e.SetActionRevision(2)
	e.SetActionName([]byte("deploy"))
	if got := ExecSnapshotRow(&e)["action"]; got != "action=deploy@r2" {
		t.Fatalf("action = %v", got)
	}
	if got := ExecSnapshotRow(&protocol.ExecRunInfo{})["action"]; got != "" {
		t.Fatalf("raw argv action = %v, want \"\"", got)
	}
}
