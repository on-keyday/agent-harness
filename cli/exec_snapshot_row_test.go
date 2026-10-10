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
