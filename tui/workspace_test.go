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
