package cli

// Shared by the native and wasm exec clients. cli/exec_run.go is !js, so
// anything the browser also needs — the result shape, the wire argv, the status
// mapping and every renderer — lives here rather than being mirrored in JS.
// A mirror has no way to fail loudly when the grammar grows.

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// ExecRunResult is how one exec ended.
type ExecRunResult struct {
	// ExitCode is the child's own for Kind==exited, and -1 otherwise — a death
	// by signal has no exit code, and inventing 127 would be a shell's
	// convention applied where there is no shell.
	ExitCode int32
	Kind     protocol.ExecEventKind
	Detail   string
}

// buildExecArgv wraps a command line for the wire, one element per argument.
func buildExecArgv(argv []string) protocol.ExecArgv {
	var out protocol.ExecArgv
	for _, a := range argv {
		var e protocol.ExecArg
		e.SetArg([]byte(a))
		out.Argv = append(out.Argv, e)
	}
	out.ArgvLen = uint16(len(out.Argv))
	return out
}

// ExecArgvStrings unwraps a wire argv back to a plain command line.
func ExecArgvStrings(a protocol.ExecArgv) []string {
	out := make([]string, 0, len(a.Argv))
	for i := range a.Argv {
		out = append(out, string(a.Argv[i].Arg))
	}
	return out
}

func execRunStatusError(taskID string, st protocol.ExecRunStatus) error {
	switch st {
	case protocol.ExecRunStatus_Ok:
		return nil
	case protocol.ExecRunStatus_NotFound:
		return fmt.Errorf("exec: task %q not found (pruned, wrong id, or outside your scope)", taskID)
	case protocol.ExecRunStatus_NoWorktree:
		return fmt.Errorf("exec: task %q has no worktree to run in — it ended clean and its tree was removed (a task that ended with uncommitted work keeps one)", taskID)
	case protocol.ExecRunStatus_RunnerUnreachable:
		return fmt.Errorf("exec: the runner hosting task %q is not connected", taskID)
	case protocol.ExecRunStatus_EmptyArgv:
		return errors.New("exec: empty command")
	case protocol.ExecRunStatus_Denied:
		return errors.New("exec: denied (needs the exec_run capability)")
	default:
		return fmt.Errorf("exec: %s", st.String())
	}
}

// ExecArgvString renders a command line for display. Arguments containing
// spaces are quoted so a two-word argument cannot be read as two arguments —
// a listing is what an operator reads to decide what to kill.
//
// The one implementation of that rule. Every surface that shows a command runs
// through here, so the CLI listing, the TUI result line and the WebUI row
// cannot disagree about what an argv looks like.
func ExecArgvString(argv []string) string {
	parts := make([]string, 0, len(argv))
	for _, s := range argv {
		if strings.ContainsAny(s, " \t\"") {
			s = fmt.Sprintf("%q", s)
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}

// ExecRunArgvString is the wire-form adapter for ExecArgvString.
func ExecRunArgvString(a protocol.ExecArgv) string {
	return ExecArgvString(ExecArgvStrings(a))
}

// ExecRunInfoLines renders the listing as a human-readable table.
func ExecRunInfoLines(es []protocol.ExecRunInfo) []string {
	if len(es) == 0 {
		return []string{"no running execs"}
	}
	out := make([]string, 0, len(es)+1)
	out = append(out, fmt.Sprintf("%-6s %-12s %-8s %-24s %s", "id", "task", "age", "origin", "command"))
	for i := range es {
		e := &es[i]
		age := "-"
		if e.StartedUnixMs > 0 {
			age = time.Since(time.UnixMilli(int64(e.StartedUnixMs))).Truncate(time.Second).String()
		}
		out = append(out, fmt.Sprintf("%-6d %-12s %-8s %-24s %s",
			e.ExecId,
			hex.EncodeToString(e.TaskId.Id[:])[:12],
			age,
			e.OriginKind.String()+" "+string(e.OriginCid),
			ExecRunArgvString(e.Argv)))
	}
	return out
}

// ExecRunInfoJSONLine renders one row as JSON, carrying everything the text
// form abbreviates — the full task id and the argv as a list.
func ExecRunInfoJSONLine(e *protocol.ExecRunInfo) string {
	row := struct {
		ExecID        uint64   `json:"exec_id"`
		TaskID        string   `json:"task_id"`
		StartedUnixMs uint64   `json:"started_unix_ms"`
		Argv          []string `json:"argv"`
		OriginKind    string   `json:"origin_kind"`
		OriginCID     string   `json:"origin_cid"`
	}{
		ExecID:        e.ExecId,
		TaskID:        hex.EncodeToString(e.TaskId.Id[:]),
		StartedUnixMs: e.StartedUnixMs,
		Argv:          ExecArgvStrings(e.Argv),
		OriginKind:    e.OriginKind.String(),
		OriginCID:     string(e.OriginCid),
	}
	b, err := json.Marshal(row)
	if err != nil {
		return "{}"
	}
	return string(b)
}
