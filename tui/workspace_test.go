package tui

import (
	"testing"

	"github.com/on-keyday/agent-harness/cli/workspace"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// A workspace resume must fire only for a task in a terminal state. A live task
// is left alone, which is why a plain network blip cannot spawn anything: the
// task is still Running or Detached when the reconnect's snapshot arrives.
func TestWorkspaceResumeOnlyForTerminalTasks(t *testing.T) {
	cases := []struct {
		status protocol.TaskStatus
		want   bool
	}{
		{protocol.TaskStatus_Running, false},
		{protocol.TaskStatus_Detached, false},
		{protocol.TaskStatus_Queued, false},
		{protocol.TaskStatus_Failed, true},
		{protocol.TaskStatus_Succeeded, true},
		{protocol.TaskStatus_Cancelled, true},
	}
	for _, c := range cases {
		ti := protocol.TaskInfo{Status: c.status, Kind: protocol.TaskKind_Interactive}
		if got := workspaceWantsResume(&ti, workspace.Task{Resume: workspace.ResumeContinue}); got != c.want {
			t.Errorf("status %v: wantsResume = %v, want %v", c.status, got, c.want)
		}
	}

	ti := protocol.TaskInfo{Status: protocol.TaskStatus_Failed, Kind: protocol.TaskKind_Interactive}
	if workspaceWantsResume(&ti, workspace.Task{Resume: workspace.ResumeNo}) {
		t.Error("resume = no still resumed a terminal task")
	}
	// The zero Resume is what an omitted key parses to; it must behave as no.
	if workspaceWantsResume(&ti, workspace.Task{}) {
		t.Error("an omitted resume key still resumed a terminal task")
	}
	if workspaceWantsResume(nil, workspace.Task{Resume: workspace.ResumeContinue}) {
		t.Error("a task absent from the snapshot was resumed")
	}
}

// applyWorkspace must be inert without a workspace or without a client, so the
// arming path can call it unconditionally.
func TestApplyWorkspaceIsInertWhenUnconfigured(t *testing.T) {
	a := New(Config{})
	if cmd := a.applyWorkspace(); cmd != nil {
		t.Error("applyWorkspace with no workspace returned a command")
	}
	a.SetWorkspace(&workspace.Workspace{Name: "default"})
	if cmd := a.applyWorkspace(); cmd != nil {
		t.Error("applyWorkspace with no client returned a command")
	}
}

// A bad grid value is reported and does not panic. Validate rejects it earlier
// on every configured path, so this is the belt to that braces.
func TestWorkspaceGridCmdReportsABadValue(t *testing.T) {
	a := New(Config{})
	if _, err := a.workspaceGridCmd(&workspace.Workspace{
		Name: "w", Grid: "--nope", GridSet: true,
	}); err == nil {
		t.Error("a bad grid value produced no error")
	}
}

// A failed workspace resume arms exactly one re-apply for the next runner
// event. The reconnect after a server restart races the runner's own
// reconnect — the client is back first and the apply finds no runner — so
// without this the case the feature exists for fails by default.
func TestWorkspaceResumeFailureArmsOneRunnerRetry(t *testing.T) {
	a := New(Config{})
	a.SetWorkspace(&workspace.Workspace{Name: "default"})

	a.Update(SessionStartedMsg{Err: errNoRunnerForTest})
	if !a.workspaceRetryOnRunner {
		t.Fatal("a failed resume did not arm the retry")
	}

	a.Update(RunnerEventMsg{})
	if a.workspaceRetryOnRunner {
		t.Error("the runner event did not consume the armed retry")
	}
	if !a.workspaceArmed {
		t.Error("the runner event did not arm an apply")
	}

	// Not standing: a second runner event must not re-arm, or a workspace
	// would re-apply — and reopen the grid overlay — on every runner event.
	a.workspaceArmed = false
	a.Update(RunnerEventMsg{})
	if a.workspaceArmed {
		t.Error("a runner event re-armed with no failure to justify it")
	}
}

// Without a workspace installed there is nothing to retry.
func TestWorkspaceRetryNotArmedWithoutAWorkspace(t *testing.T) {
	a := New(Config{})
	a.Update(SessionStartedMsg{Err: errNoRunnerForTest})
	if a.workspaceRetryOnRunner {
		t.Error("armed a retry with no workspace installed")
	}
}

var errNoRunnerForTest = errTestString("interactive no_runner_for_repo: no idle runner for repo \"\"")

type errTestString string

func (e errTestString) Error() string { return string(e) }

// A task being resumed gets no forwards in the same pass: tea.Batch runs its
// commands concurrently, so a forward registered beside the resume races it and
// the server answers "no such task". Verified live before this test existed.
func TestWorkspaceDefersForwardsPastAResume(t *testing.T) {
	decl := workspace.Task{
		ID: wsTaskA, Resume: workspace.ResumeContinue, Runner: workspace.RunnerAssigned,
		Forwards: []string{"-L 3000:127.0.0.1:3000"},
	}
	terminal := protocol.TaskInfo{Status: protocol.TaskStatus_Failed, Kind: protocol.TaskKind_Interactive}
	if !workspaceWantsResume(&terminal, decl) {
		t.Fatal("fixture is wrong: the task must be resumable")
	}
	// The pass that resumes must plan no forward start for that task, and the
	// pass after it (task alive) must plan one.
	live := protocol.TaskInfo{Status: protocol.TaskStatus_Detached, Kind: protocol.TaskKind_Interactive}
	if workspaceWantsResume(&live, decl) {
		t.Fatal("a live task must not be resumed again — the re-arm would not terminate")
	}
	plan := planForwards(decl.Forwards, map[int]*PortForwardSession{}, wsTaskA)
	if len(plan.Start) != 1 {
		t.Errorf("the pass after the resume must start the forward, got %q", plan.Start)
	}
}

// Only a task the workspace names may trigger a re-apply; a hand-run
// `session new -d` must not.
func TestWorkspaceDeclares(t *testing.T) {
	a := New(Config{})
	if a.workspaceDeclares(wsTaskA) {
		t.Error("declared a task with no workspace installed")
	}
	a.SetWorkspace(&workspace.Workspace{Name: "w", Tasks: []workspace.Task{{ID: wsTaskA}}})
	if !a.workspaceDeclares(wsTaskA) {
		t.Error("did not recognise its own task")
	}
	if a.workspaceDeclares(wsTaskB) {
		t.Error("claimed a task it does not name")
	}
	if a.workspaceDeclares("") {
		t.Error("claimed the empty task id")
	}
}
