package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// LifecycleShown reports whether t's lifecycle is rendered at all: not for a
// finished task, which must not read "working" (D14). A live task that never
// reported IS rendered, as "-": hooks that do not fire (an agent without them,
// a runner that predates them) must not look like a quiet task (D28).
func LifecycleShown(t *protocol.TaskInfo) bool {
	return !isTerminalStatus(t.Status)
}

// LifecycleStr is `lifecycle=waiting (3m ago)` for the CLI `ls` row,
// `lifecycle=-` when the task never reported one, "" when it has ended.
func LifecycleStr(t *protocol.TaskInfo) string {
	if !LifecycleShown(t) {
		return ""
	}
	if t.LifecycleAt == 0 {
		return "lifecycle=-"
	}
	return fmt.Sprintf("lifecycle=%s (%s)", LifecycleWord(t.Lifecycle), AgeStr(t.LifecycleAgeMs))
}

// LifecycleBadge is the list badge: `work:5s`, `wait:3m`, `ready:12m` or
// `err:30s`, and `lc:-` for a live task that never reported. The words differ
// from the wire names so `ready` cannot be confused with the output-activity
// badge's `idle:3m`, which is shown beside it. "" for a finished task.
func LifecycleBadge(t *protocol.TaskInfo) string {
	if !LifecycleShown(t) {
		return ""
	}
	var word string
	switch {
	case t.LifecycleAt == 0:
		return "lc:-"
	case t.Lifecycle == protocol.LifecycleState_Working:
		word = "work"
	case t.Lifecycle == protocol.LifecycleState_Waiting:
		word = "wait"
	case t.Lifecycle == protocol.LifecycleState_Idle:
		word = "ready"
	case t.Lifecycle == protocol.LifecycleState_Error:
		word = "err"
	default:
		return "lc:-"
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

// hookEvent is what a hook's stdin says about the event that fired it. Only
// the fields the lifecycle needs are read; the zero value means "no hook
// input" (a hand-run command, or an agent whose hook JSON this does not know),
// and then the argv word is the state.
//
// The field names are Claude Code's hook schema. Another agent's hooks need
// their own reading here and their own event table in hookState.next.
type hookEvent struct {
	Name      string `json:"hook_event_name"`
	AgentID   string `json:"agent_id"`
	Interrupt bool   `json:"is_interrupt"`
}

// hookEventFor reads the hook input for the agent the hook command names
// (`--hook claude`). Which agent's JSON is on stdin is never guessed from the
// JSON: a nested agent inherits the env, and one agent can replay another's
// hook config. No --hook (a hand-run command) or no stdin is no input.
// Another agent is added here with its own reading.
func hookEventFor(hook string, stdin io.Reader) hookEvent {
	if stdin == nil {
		return hookEvent{}
	}
	switch hook {
	case "claude":
		return parseHookInput(stdin)
	}
	return hookEvent{}
}

// parseHookInput reads a hook's stdin JSON. PostToolUse carries the tool's
// whole output, so the read is bounded; anything unreadable is "no input".
func parseHookInput(r io.Reader) hookEvent {
	var ev hookEvent
	if err := json.NewDecoder(io.LimitReader(r, 16<<20)).Decode(&ev); err != nil {
		return hookEvent{}
	}
	return ev
}

// hookState is what one task's hooks remember between invocations: the
// subagents still running and whether the parent is inside a turn. It exists
// because claude 2.1.311 ends the parent's turn (Stop) right after launching
// a subagent, which then works on its own and hands back in a new turn
// (measured 2026-10-27): a Stop with live subagents is not idle.
type hookState struct {
	Subagents map[string]bool `json:"subagents,omitempty"`
	InTurn    bool            `json:"in_turn"`
	// LastSent / LastSentAt are the newest report the server ACCEPTED (Ok),
	// so a repeat of it can be skipped. A stale answer records nothing: the
	// server then holds something newer than this report.
	LastSent   string `json:"last_sent,omitempty"`
	LastSentAt int64  `json:"last_sent_at,omitempty"`
}

// next applies one hook event and returns the task's state after it. argv is
// the state the hook's command line names, used as-is for events this table
// does not interpret.
func (st *hookState) next(ev hookEvent, argv protocol.LifecycleState) protocol.LifecycleState {
	if st.Subagents == nil {
		st.Subagents = map[string]bool{}
	}
	idleUnlessSubagents := func() protocol.LifecycleState {
		if len(st.Subagents) > 0 {
			return protocol.LifecycleState_Working
		}
		return protocol.LifecycleState_Idle
	}
	switch ev.Name {
	case "SessionStart":
		// A new process: nothing of the old one's subagents survives.
		st.Subagents, st.InTurn = map[string]bool{}, false
		return protocol.LifecycleState_Idle
	case "UserPromptSubmit":
		st.InTurn = true
		return protocol.LifecycleState_Working
	case "SubagentStart":
		if ev.AgentID != "" {
			st.Subagents[ev.AgentID] = true
		}
		return protocol.LifecycleState_Working
	case "SubagentStop":
		// claude also stops subagents it never announced; forgetting one we
		// do not hold changes nothing.
		delete(st.Subagents, ev.AgentID)
		if st.InTurn {
			return protocol.LifecycleState_Working
		}
		return idleUnlessSubagents()
	case "Stop":
		if ev.AgentID == "" {
			st.InTurn = false
		}
		return idleUnlessSubagents()
	case "StopFailure":
		st.InTurn = false
		return protocol.LifecycleState_Error
	case "PostToolUse":
		return protocol.LifecycleState_Working
	case "PostToolUseFailure":
		// Esc during a tool ends the parent's turn with no Stop hook; any
		// other failure is a tool that failed inside a turn that goes on.
		if ev.Interrupt && ev.AgentID == "" {
			st.InTurn = false
			return idleUnlessSubagents()
		}
		return protocol.LifecycleState_Working
	case "PermissionRequest", "Elicitation", "PreToolUse":
		// The human must answer, whoever asked (a subagent's prompt too).
		return protocol.LifecycleState_Waiting
	}
	return argv
}

// hookResult is one hook's computed report: the state, the reporter stamp it
// carries, and whether it repeats the last accepted report and can be skipped.
type hookResult struct {
	state      protocol.LifecycleState
	reportedAt int64
	skip       bool
}

func hookStatePath(taskHex string) (string, bool) {
	if taskHex == "" {
		return "", false
	}
	dir, err := os.UserCacheDir()
	if err != nil {
		return "", false
	}
	return filepath.Join(dir, "harness", "lifecycle-"+taskHex+".json"), true
}

// withHookState runs fn on the task's state under an exclusive lock and saves
// the result. Hooks are async, so several run at once; without the lock two
// of them read the same state and one update is lost. The lock is a file
// created with O_EXCL (portable across the fleet's OSes); one older than a few
// seconds belongs to a hook that died and is taken over. Any file error runs
// fn on a fresh state and saves nothing: the report is then still sent, and
// only subagent tracking and the repeat-skip are lost.
func withHookState(taskHex string, fn func(st *hookState)) {
	path, ok := hookStatePath(taskHex)
	if !ok || os.MkdirAll(filepath.Dir(path), 0o700) != nil {
		fn(&hookState{})
		return
	}
	lock := path + ".lock"
	locked := false
	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); {
		f, err := os.OpenFile(lock, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			f.Close()
			locked = true
			break
		}
		if fi, serr := os.Stat(lock); serr == nil && time.Since(fi.ModTime()) > 5*time.Second {
			os.Remove(lock)
			continue
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !locked {
		fn(&hookState{})
		return
	}
	defer os.Remove(lock)
	var st hookState
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &st)
	}
	fn(&st)
	if b, err := json.Marshal(&st); err == nil {
		tmp := path + ".tmp"
		if os.WriteFile(tmp, b, 0o600) == nil {
			_ = os.Rename(tmp, path)
		}
	}
}

// applyHookEvent advances the task's state by one event and stamps the report
// INSIDE the lock: the order hooks take the lock is the order their states were
// computed, so stamping there makes the server's stamp order agree with it.
func applyHookEvent(taskHex string, ev hookEvent, argv protocol.LifecycleState) hookResult {
	var r hookResult
	withHookState(taskHex, func(st *hookState) {
		r.state = st.next(ev, argv)
		r.reportedAt = time.Now().UnixNano()
		r.skip = taskHex != "" && st.LastSent == LifecycleWord(r.state)
	})
	return r
}

// recordSent remembers an accepted report so a repeat of it can be skipped,
// unless a newer accepted one is already recorded. A stale answer clears the
// record: the server holds a newer state than this hook knows.
func recordSent(taskHex string, r hookResult, status protocol.LifecycleSetStatus) {
	withHookState(taskHex, func(st *hookState) {
		switch status {
		case protocol.LifecycleSetStatus_Ok:
			if r.reportedAt > st.LastSentAt {
				st.LastSent, st.LastSentAt = LifecycleWord(r.state), r.reportedAt
			}
		default:
			st.LastSent, st.LastSentAt = "", 0
		}
	})
}

// ReportLifecycle is the body of `harness-cli lifecycle <state>`. It reads the
// hook's stdin when there is one, folds the event into the task's state (see
// hookState), skips a repeat of the last accepted report, and sends with a
// short timeout. It never fails: it runs inside the agent's hook pipeline, so
// a down server is one line on errw, not an error the agent sees. It writes
// nothing to stdout.
func ReportLifecycle(ctx context.Context, cid func() objproto.ConnectionID, word, hook string, stdin io.Reader, errw io.Writer) {
	argv, ok := ParseLifecycleState(word)
	if !ok {
		fmt.Fprintf(errw, "lifecycle: unknown state %q\n", word)
		return
	}
	ev := hookEventFor(hook, stdin)
	task := os.Getenv("HARNESS_TASK_ID")
	r := applyHookEvent(task, ev, argv)
	if r.skip {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	resp, err := LifecycleSet(ctx, cid(), r.state, r.reportedAt)
	if err != nil {
		recordSent(task, r, protocol.LifecycleSetStatus_NotFound) // unknown outcome: send next time
		fmt.Fprintf(errw, "lifecycle %s: %v\n", LifecycleWord(r.state), strings.ReplaceAll(err.Error(), "\n", " "))
		return
	}
	recordSent(task, r, resp.Status)
	if resp.Status != protocol.LifecycleSetStatus_Ok && resp.Status != protocol.LifecycleSetStatus_Stale {
		fmt.Fprintf(errw, "lifecycle %s: %s\n", LifecycleWord(r.state), resp.Status.String())
	}
}
