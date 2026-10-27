package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// LifecycleSet reports the caller's OWN lifecycle state. The request names no
// task: the server writes the connection's principal, so this only works from
// inside a task. reportedAt is the reporter's clock when the hook started; the
// server drops a report older than the last one it accepted (stale).
func (c *Client) LifecycleSet(ctx context.Context, st protocol.LifecycleState, reportedAt int64) (*protocol.LifecycleSetResponse, error) {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_LifecycleSet}
	req.SetLifecycleSet(protocol.LifecycleSetRequest{Lifecycle: st, ReportedAt: uint64(reportedAt)})
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, err
	}
	out := resp.LifecycleSet()
	if resp.Kind != protocol.TaskControlKind_LifecycleSet || out == nil {
		return nil, fmt.Errorf("unexpected response kind: %v", resp.Kind)
	}
	return out, nil
}

// LifecycleSet (package-level) dials an agent connection for one call — the
// short-lived harness-cli a hook runs.
func LifecycleSet(ctx context.Context, peerCID objproto.ConnectionID, st protocol.LifecycleState, reportedAt int64) (*protocol.LifecycleSetResponse, error) {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.LifecycleSet(ctx, st, reportedAt)
}

// ParseLifecycleState accepts the wire words only.
func ParseLifecycleState(s string) (protocol.LifecycleState, bool) {
	switch s {
	case "working":
		return protocol.LifecycleState_Working, true
	case "waiting":
		return protocol.LifecycleState_Waiting, true
	case "idle":
		return protocol.LifecycleState_Idle, true
	case "error":
		return protocol.LifecycleState_Error, true
	}
	return protocol.LifecycleState_Unset, false
}

// LifecycleWord is the wire word for st, "" for unset.
func LifecycleWord(st protocol.LifecycleState) string {
	switch st {
	case protocol.LifecycleState_Working:
		return "working"
	case protocol.LifecycleState_Waiting:
		return "waiting"
	case protocol.LifecycleState_Idle:
		return "idle"
	case protocol.LifecycleState_Error:
		return "error"
	}
	return ""
}

// lifecycleShown reports whether t's lifecycle is worth rendering: written,
// and the task not finished — a finished task must not read "working".
func lifecycleShown(t *protocol.TaskInfo) bool {
	return t.LifecycleAt != 0 && !isTerminalStatus(t.Status)
}

// LifecycleStr is `lifecycle=waiting (3m ago)` for the CLI `ls` row, or ""
// when the task never reported one or has ended.
func LifecycleStr(t *protocol.TaskInfo) string {
	if !lifecycleShown(t) {
		return ""
	}
	return fmt.Sprintf("lifecycle=%s (%s)", LifecycleWord(t.Lifecycle), AgeStr(t.LifecycleAgeMs))
}

// LifecycleBadge is the list badge: `wait:3m`, `ready:12m` or `err:30s`. The
// words differ from the wire names so `ready` cannot be confused with the
// output-activity badge's `idle:3m`. "" for working and unset, which keep the
// activity badge, and for a task that is not live.
func LifecycleBadge(t *protocol.TaskInfo) string {
	if !lifecycleShown(t) {
		return ""
	}
	var word string
	switch t.Lifecycle {
	case protocol.LifecycleState_Waiting:
		word = "wait"
	case protocol.LifecycleState_Idle:
		word = "ready"
	case protocol.LifecycleState_Error:
		word = "err"
	default:
		return ""
	}
	return word + ":" + shortAge(t.LifecycleAgeMs)
}

// shortAge is ActivityStr's minute/second form: "3m", "30s".
func shortAge(ageMs uint64) string {
	d := time.Duration(ageMs) * time.Millisecond
	if d >= time.Minute {
		return fmt.Sprintf("%dm", int(d/time.Minute))
	}
	return fmt.Sprintf("%ds", int(d/time.Second))
}

// ProgramWord is the wire word for a program state, "" for none.
func ProgramWord(s protocol.ProgramState) string {
	// Spelled out: the generated String() capitalizes ("Blocked"), and these
	// are the protocol's words, shared with ls --json and the WebUI.
	switch s {
	case protocol.ProgramState_Idle:
		return "idle"
	case protocol.ProgramState_Working:
		return "working"
	case protocol.ProgramState_Done:
		return "done"
	case protocol.ProgramState_Blocked:
		return "blocked"
	case protocol.ProgramState_Error:
		return "error"
	}
	return ""
}

// ProgramKindWord is the wire word for a blocked kind, "" for none.
func ProgramKindWord(k protocol.ProgramKind) string {
	switch k {
	case protocol.ProgramKind_Permission:
		return "permission"
	case protocol.ProgramKind_Question:
		return "question"
	case protocol.ProgramKind_Auth:
		return "auth"
	}
	return ""
}

// ProgramStr is `program=blocked:question` for the CLI `ls` row: the
// program's own OSC 7501 report, kind only with blocked, never the msg (a
// free-form line belongs in the detail views). "" when the task has no live
// terminal or its program never reported.
func ProgramStr(t *protocol.TaskInfo) string {
	if !t.LiveTerminal() || t.ProgramState == protocol.ProgramState_None {
		return ""
	}
	s := "program=" + ProgramWord(t.ProgramState)
	if k := ProgramKindWord(t.ProgramKind); k != "" {
		s += ":" + k
	}
	return s
}

// ProgramDetail is the detail views' program line: `blocked (question) — q?`,
// each part only when present; "" when the program never reported.
func ProgramDetail(t *protocol.TaskInfo) string {
	if t.ProgramState == protocol.ProgramState_None {
		return ""
	}
	s := ProgramWord(t.ProgramState)
	if k := ProgramKindWord(t.ProgramKind); k != "" {
		s += " (" + k + ")"
	}
	if len(t.ProgramMsg) > 0 {
		s += " — " + string(t.ProgramMsg)
	}
	return s
}

// lifecycleDedup lets a hook skip a report identical to the last one this
// task sent: PostToolUse fires on every tool call. The file is an
// optimization only — any error reading or writing it means "send", and the
// server orders reports by reporter stamp, so a stale file costs one
// redundant dial, never a wrong state. record writes the state once a send
// was answered.
func lifecycleDedup(taskHex string, st protocol.LifecycleState) (skip bool, record func()) {
	noop := func() {}
	if taskHex == "" {
		return false, noop
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return false, noop
	}
	path := filepath.Join(dir, "harness", "lifecycle-"+taskHex)
	word := LifecycleWord(st)
	if b, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(b)) == word {
		return true, noop
	}
	return false, func() {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return
		}
		_ = os.WriteFile(path, []byte(word), 0o600)
	}
}

// ReportLifecycle is the body of `harness-cli lifecycle <state>`. It stamps
// the report first (the stamp is what orders async hooks), skips a repeat of
// the last state this task sent, and sends with a short timeout. It never
// fails: it runs inside the agent's hook pipeline, so a down server is one
// line on errw, not an error the agent sees. It writes nothing to stdout.
func ReportLifecycle(ctx context.Context, cid func() objproto.ConnectionID, word string, errw interface{ Write([]byte) (int, error) }) {
	reportedAt := time.Now().UnixNano()
	st, ok := ParseLifecycleState(word)
	if !ok {
		fmt.Fprintf(errw, "lifecycle: unknown state %q\n", word)
		return
	}
	skip, record := lifecycleDedup(os.Getenv("HARNESS_TASK_ID"), st)
	if skip {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := LifecycleSet(ctx, cid(), st, reportedAt)
	if err != nil {
		fmt.Fprintf(errw, "lifecycle %s: %v\n", word, strings.ReplaceAll(err.Error(), "\n", " "))
		return
	}
	switch resp.Status {
	case protocol.LifecycleSetStatus_Ok, protocol.LifecycleSetStatus_Stale:
		record()
	default:
		fmt.Fprintf(errw, "lifecycle %s: %s\n", word, resp.Status.String())
	}
}
