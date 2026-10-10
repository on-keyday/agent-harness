package cli

import (
	"encoding/hex"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// ExecSnapshotRow converts one running exec into the object the WebUI's exec
// list renders — ForwardSnapshotRow's sibling, here for the same two reasons:
// the wasm package cannot be tested, and every value is already produced by a
// cli renderer. Counters go out raw AND as the rendered `traffic` line, which is
// exactly what `exec ls` prints.
func ExecSnapshotRow(e *protocol.ExecRunInfo) map[string]any {
	return map[string]any{
		"exec_id": float64(e.ExecId),
		"task":    hex.EncodeToString(e.TaskId.Id[:]),
		// RAW, not a rendered age: the page re-renders on every poll, so a
		// string formatted here would freeze the age while the row kept being
		// redrawn.
		"started_unix_ms":       float64(e.StartedUnixMs),
		"origin":                ExecRunOrigin(e),
		"command":               ExecRunArgvString(e.Argv),
		"stdin_bytes":           float64(e.StdinBytes),
		"stdout_bytes":          float64(e.StdoutBytes),
		"stderr_bytes":          float64(e.StderrBytes),
		"last_activity_unix_ms": float64(e.LastActivityUnixMs),
		"taps":                  float64(e.Taps),
		"pty":                   e.Pty(),
		"traffic":               ExecRunTrafficLine(e),
	}
}
