package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func lifecycleTask(st protocol.LifecycleState, ageMs uint64, status protocol.TaskStatus) protocol.TaskInfo {
	t := protocol.TaskInfo{Status: status, Lifecycle: st, LifecycleAt: 5, LifecycleAgeMs: ageMs}
	return t
}

func TestLifecycleBadge(t *testing.T) {
	cases := []struct {
		name string
		t    protocol.TaskInfo
		want string
	}{
		{"waiting", lifecycleTask(protocol.LifecycleState_Waiting, 180000, protocol.TaskStatus_Running), "wait:3m"},
		{"idle", lifecycleTask(protocol.LifecycleState_Idle, 720000, protocol.TaskStatus_Detached), "ready:12m"},
		{"error", lifecycleTask(protocol.LifecycleState_Error, 30000, protocol.TaskStatus_Running), "err:30s"},
		{"working", lifecycleTask(protocol.LifecycleState_Working, 30000, protocol.TaskStatus_Running), ""},
		{"terminal", lifecycleTask(protocol.LifecycleState_Waiting, 180000, protocol.TaskStatus_Succeeded), ""},
		{"never written", protocol.TaskInfo{Status: protocol.TaskStatus_Running, Lifecycle: protocol.LifecycleState_Waiting}, ""},
	}
	for _, c := range cases {
		if got := LifecycleBadge(&c.t); got != c.want {
			t.Errorf("%s: LifecycleBadge = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestTaskLine_Lifecycle(t *testing.T) {
	ti := lifecycleTask(protocol.LifecycleState_Waiting, 180000, protocol.TaskStatus_Running)
	if line := taskLine(ti, nil); !strings.Contains(line, "lifecycle=waiting (3m ago)") {
		t.Fatalf("taskLine = %q, want lifecycle=waiting (3m ago)", line)
	}
	ti.LifecycleAt = 0
	if line := taskLine(ti, nil); strings.Contains(line, "lifecycle=") {
		t.Fatalf("taskLine = %q, want no lifecycle= for a never-written field", line)
	}
}

func TestTaskJSON_LifecycleFields(t *testing.T) {
	b, err := json.Marshal(newTaskJSON(&protocol.TaskInfo{}, nil))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"lifecycle", "lifecycle_at", "lifecycle_age_ms", "program_state", "program_kind", "program_msg"} {
		if _, ok := m[k]; !ok {
			t.Errorf("ls --json lacks %q at zero; present-at-zero is the contract", k)
		}
	}
	if m["lifecycle"] != "" {
		t.Errorf("unset lifecycle = %v, want \"\"", m["lifecycle"])
	}
}

// The hook sequence claude 2.1.311 produced for one subagent, measured on a
// dummy harness (2026-10-27): the parent's turn STOPS right after launching
// the subagent, which then works on its own and hands back in a new turn.
// While it works the task must read working, not idle.
func TestNextLifecycle_SubagentOutlivesParentStop(t *testing.T) {
	var st hookState
	steps := []struct {
		event, agent string
		argv         protocol.LifecycleState
		want         protocol.LifecycleState
	}{
		{"SessionStart", "", protocol.LifecycleState_Idle, protocol.LifecycleState_Idle},
		{"UserPromptSubmit", "", protocol.LifecycleState_Working, protocol.LifecycleState_Working},
		{"SubagentStart", "a1", protocol.LifecycleState_Working, protocol.LifecycleState_Working},
		{"PostToolUse", "", protocol.LifecycleState_Working, protocol.LifecycleState_Working},
		{"Stop", "", protocol.LifecycleState_Idle, protocol.LifecycleState_Working},           // a1 still running
		{"SubagentStop", "x9", protocol.LifecycleState_Idle, protocol.LifecycleState_Working}, // never started: ignored
		{"PostToolUse", "a1", protocol.LifecycleState_Working, protocol.LifecycleState_Working},
		{"SubagentStop", "a1", protocol.LifecycleState_Idle, protocol.LifecycleState_Idle},         // last one, parent stopped
		{"UserPromptSubmit", "", protocol.LifecycleState_Working, protocol.LifecycleState_Working}, // handback turn
		{"Stop", "", protocol.LifecycleState_Idle, protocol.LifecycleState_Idle},
	}
	for i, s := range steps {
		if got := st.next(hookEvent{Name: s.event, AgentID: s.agent}, s.argv); got != s.want {
			t.Fatalf("step %d %s(%s): got %v, want %v", i, s.event, s.agent, got, s.want)
		}
	}
}

func TestNextLifecycle_Rules(t *testing.T) {
	var st hookState
	st.next(hookEvent{Name: "UserPromptSubmit", AgentID: ""}, protocol.LifecycleState_Working)
	st.next(hookEvent{Name: "SubagentStart", AgentID: "a1"}, protocol.LifecycleState_Working)
	if got := st.next(hookEvent{Name: "PermissionRequest", AgentID: "a1"}, protocol.LifecycleState_Waiting); got != protocol.LifecycleState_Waiting {
		t.Errorf("a subagent's permission prompt = %v, want waiting (the human must answer it)", got)
	}
	if got := st.next(hookEvent{Name: "StopFailure", AgentID: ""}, protocol.LifecycleState_Error); got != protocol.LifecycleState_Error {
		t.Errorf("StopFailure = %v, want error", got)
	}
	// A new session forgets every subagent the old one had.
	if got := st.next(hookEvent{Name: "SessionStart", AgentID: ""}, protocol.LifecycleState_Idle); got != protocol.LifecycleState_Idle || len(st.Subagents) != 0 || st.InTurn {
		t.Errorf("SessionStart = %v, subagents=%v inTurn=%v; want idle and a reset", got, st.Subagents, st.InTurn)
	}
	// No hook event (a hand-run `harness-cli lifecycle waiting`): the argv word.
	if got := st.next(hookEvent{Name: "", AgentID: ""}, protocol.LifecycleState_Waiting); got != protocol.LifecycleState_Waiting {
		t.Errorf("no event = %v, want the argv state", got)
	}
}

// A failed tool still means the turn goes on (review finding: after an
// approved tool failed, nothing reported working). An interrupt (Esc during a
// tool) ends the parent's turn without a Stop hook.
func TestNextLifecycle_ToolFailureAndInterrupt(t *testing.T) {
	var st hookState
	st.next(hookEvent{Name: "UserPromptSubmit"}, protocol.LifecycleState_Working)
	st.next(hookEvent{Name: "PermissionRequest"}, protocol.LifecycleState_Waiting)
	if got := st.next(hookEvent{Name: "PostToolUseFailure"}, protocol.LifecycleState_Working); got != protocol.LifecycleState_Working {
		t.Errorf("failed tool = %v, want working", got)
	}
	if got := st.next(hookEvent{Name: "PostToolUseFailure", Interrupt: true}, protocol.LifecycleState_Working); got != protocol.LifecycleState_Idle || st.InTurn {
		t.Errorf("interrupted tool = %v inTurn=%v, want idle and the turn over", got, st.InTurn)
	}
}

// Review finding: recording the word of a report the server answered stale
// made the next real report of that word skip. Only an accepted report with a
// newer stamp than the recorded one may be recorded.
func TestRecordSent_OnlyNewerAccepted(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	const task = "00112233445566778899aabbccddeeff"
	idle := applyHookEvent(task, hookEvent{Name: "Stop"}, protocol.LifecycleState_Idle)
	recordSent(task, idle, protocol.LifecycleSetStatus_Ok)
	work := applyHookEvent(task, hookEvent{Name: "UserPromptSubmit"}, protocol.LifecycleState_Working)
	recordSent(task, work, protocol.LifecycleSetStatus_Stale) // the server holds something newer
	if r := applyHookEvent(task, hookEvent{Name: "PostToolUse"}, protocol.LifecycleState_Working); r.skip {
		t.Fatal("a report after a stale answer was skipped")
	}
	// An older Ok landing after a newer one must not overwrite it.
	newer := applyHookEvent(task, hookEvent{Name: "Stop"}, protocol.LifecycleState_Idle)
	recordSent(task, newer, protocol.LifecycleSetStatus_Ok)
	recordSent(task, work, protocol.LifecycleSetStatus_Ok)
	if r := applyHookEvent(task, hookEvent{Name: "Stop"}, protocol.LifecycleState_Idle); !r.skip {
		t.Fatal("an older Ok overwrote the newer recorded report")
	}
}

// Which agent's hook JSON is on stdin is named by the hook command
// (--hook claude), never guessed from the JSON: a nested agent inherits the
// env, and one agent may replay another's config. Without --hook the stdin is
// not read and the argv word is the state.
func TestHookEventFor(t *testing.T) {
	in := `{"hook_event_name":"Stop"}`
	if ev := hookEventFor("claude", strings.NewReader(in)); ev.Name != "Stop" {
		t.Errorf("claude = %+v, want Stop", ev)
	}
	if ev := hookEventFor("", strings.NewReader(in)); ev != (hookEvent{}) {
		t.Errorf("no --hook = %+v, want stdin ignored", ev)
	}
	if ev := hookEventFor("claude", nil); ev != (hookEvent{}) {
		t.Errorf("claude without stdin = %+v, want zero", ev)
	}
}

func TestParseHookInput(t *testing.T) {
	ev := parseHookInput(strings.NewReader(`{"hook_event_name":"PostToolUseFailure","agent_id":"afae","is_interrupt":true,"tool_response":{"big":"x"}}`))
	if ev != (hookEvent{Name: "PostToolUseFailure", AgentID: "afae", Interrupt: true}) {
		t.Errorf("= %+v", ev)
	}
	if ev := parseHookInput(strings.NewReader("not json")); ev != (hookEvent{}) {
		t.Errorf("garbage = %+v, want zero", ev)
	}
}

// Async hooks run concurrently; the state file is read-modify-written under a
// lock, so none of these concurrent starts may be lost.
func TestApplyHookEvent_ConcurrentStartsAllLand(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	const task = "00112233445566778899aabbccddeeff"
	var wg sync.WaitGroup
	const n = 200
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release them together so the read-modify-writes overlap
			applyHookEvent(task, hookEvent{Name: "SubagentStart", AgentID: fmt.Sprintf("a%d", i)}, protocol.LifecycleState_Working)
		}(i)
	}
	close(start)
	wg.Wait()
	var st hookState
	withHookState(task, func(s *hookState) { st = *s })
	if len(st.Subagents) != n {
		t.Fatalf("%d of %d concurrent SubagentStarts landed; the rest were lost", len(st.Subagents), n)
	}
	r := applyHookEvent(task, hookEvent{Name: "Stop", AgentID: ""}, protocol.LifecycleState_Idle)
	if r.state != protocol.LifecycleState_Working {
		t.Fatalf("Stop with live subagents = %v, want working", r.state)
	}
	for i := 0; i < n; i++ {
		r = applyHookEvent(task, hookEvent{Name: "SubagentStop", AgentID: fmt.Sprintf("a%d", i)}, protocol.LifecycleState_Idle)
	}
	if r.state != protocol.LifecycleState_Idle {
		t.Fatalf("after every subagent stopped = %v, want idle", r.state)
	}
}

// A repeat of the last state sent is skipped (PostToolUse fires per tool
// call); a state file that cannot be used costs a redundant send, never a skip.
func TestApplyHookEvent_SkipsRepeats(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	const task = "00112233445566778899aabbccddeeff"
	first := applyHookEvent(task, hookEvent{Name: "UserPromptSubmit", AgentID: ""}, protocol.LifecycleState_Working)
	if first.skip {
		t.Fatal("first report skipped")
	}
	recordSent(task, first, protocol.LifecycleSetStatus_Ok)
	if r := applyHookEvent(task, hookEvent{Name: "PostToolUse", AgentID: ""}, protocol.LifecycleState_Working); !r.skip {
		t.Fatal("a repeat of the last sent state was not skipped")
	}
	if r := applyHookEvent(task, hookEvent{Name: "Stop", AgentID: ""}, protocol.LifecycleState_Idle); r.skip || r.state != protocol.LifecycleState_Idle {
		t.Fatalf("a change was skipped: %+v", r)
	}
	if r := applyHookEvent("", hookEvent{Name: "PostToolUse", AgentID: ""}, protocol.LifecycleState_Working); r.skip {
		t.Fatal("an empty task id must never skip")
	}
	ro := t.TempDir()
	if err := os.WriteFile(filepath.Join(ro, "harness"), []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_CACHE_HOME", ro)
	recordSent(task, applyHookEvent(task, hookEvent{Name: "UserPromptSubmit", AgentID: ""}, protocol.LifecycleState_Working), protocol.LifecycleSetStatus_Ok)
	if r := applyHookEvent(task, hookEvent{Name: "PostToolUse", AgentID: ""}, protocol.LifecycleState_Working); r.skip {
		t.Fatal("an unusable cache dir skipped a report")
	}
}

func TestTaskLine_Program(t *testing.T) {
	live := func(st protocol.ProgramState, k protocol.ProgramKind) protocol.TaskInfo {
		ti := protocol.TaskInfo{Status: protocol.TaskStatus_Detached, ProgramState: st, ProgramKind: k}
		ti.SetLiveTerminal(true)
		ti.SetProgramMsg([]byte("not printed"))
		return ti
	}
	if line := taskLine(live(protocol.ProgramState_Blocked, protocol.ProgramKind_Question), nil); !strings.Contains(line, "program=blocked:question") || strings.Contains(line, "not printed") {
		t.Errorf("blocked = %q, want program=blocked:question and no msg", line)
	}
	if line := taskLine(live(protocol.ProgramState_Working, protocol.ProgramKind_None), nil); !strings.Contains(line, "program=working") || strings.Contains(line, "program=working:") {
		t.Errorf("working = %q, want program=working", line)
	}
	if line := taskLine(live(protocol.ProgramState_None, protocol.ProgramKind_None), nil); strings.Contains(line, "program=") {
		t.Errorf("none = %q, want no program=", line)
	}
	dead := live(protocol.ProgramState_Working, protocol.ProgramKind_None)
	dead.SetLiveTerminal(false)
	if line := taskLine(dead, nil); strings.Contains(line, "program=") {
		t.Errorf("no live terminal = %q, want no program=", line)
	}
}
