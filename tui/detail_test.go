package tui

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TestFormatTaskDetailCapsLine verifies the "caps:" label line is rendered
// inside the detail popup for a task with Spawn|FileRead capabilities.
func TestFormatTaskDetailCapsLine(t *testing.T) {
	var taskID protocol.TaskID
	taskID.Id[0] = 0xab
	task := protocol.TaskInfo{
		Id:           taskID,
		Status:       protocol.TaskStatus_Running,
		Kind:         protocol.TaskKind_Oneshot,
		Capabilities: protocol.Capability_Spawn | protocol.Capability_FileRead,
	}
	task.SetRepoPath([]byte("/home/user/repo"))
	task.SetPrompt([]byte("fix the bug"))

	got := formatTaskDetail(task)

	if !strings.Contains(got, "caps:") {
		t.Errorf("expected caps: label in detail output:\n%s", got)
	}
	if !strings.Contains(got, "spawn,file_read") {
		t.Errorf("expected spawn,file_read in detail output:\n%s", got)
	}
}

// TestFormatTaskDetailActLine verifies the "act:" busy/idle badge and the
// "last output:" timestamp are rendered for a task with a live interactive
// session (LastOutputAt > 0), mirroring the task table's Act column.
func TestFormatTaskDetailActLine(t *testing.T) {
	var taskID protocol.TaskID
	taskID.Id[0] = 0xef
	task := protocol.TaskInfo{
		Id:           taskID,
		Status:       protocol.TaskStatus_Running,
		Kind:         protocol.TaskKind_Interactive,
		LastOutputAt: 1770000000000000000,
		OutputIdleMs: 500,
	}
	task.SetRepoPath([]byte("/repo"))

	got := formatTaskDetail(task)

	if !strings.Contains(got, "act:") {
		t.Errorf("expected act: label in detail output:\n%s", got)
	}
	if !strings.Contains(got, "busy") {
		t.Errorf("expected busy badge in detail output:\n%s", got)
	}
	if !strings.Contains(got, "last output:") {
		t.Errorf("expected last output: label in detail output:\n%s", got)
	}

	task.OutputIdleMs = 10_000
	got = formatTaskDetail(task)
	if !strings.Contains(got, "idle:10s") {
		t.Errorf("expected idle:10s badge in detail output:\n%s", got)
	}
}

// TestFormatTaskDetailNoActWithoutLiveSession verifies no act/last-output
// lines appear when the task has no live interactive session
// (LastOutputAt == 0 — the server leaves it zero for those).
func TestFormatTaskDetailNoActWithoutLiveSession(t *testing.T) {
	var taskID protocol.TaskID
	taskID.Id[0] = 0x11
	task := protocol.TaskInfo{
		Id:     taskID,
		Status: protocol.TaskStatus_Succeeded,
		Kind:   protocol.TaskKind_Oneshot,
	}
	task.SetRepoPath([]byte("/repo"))

	got := formatTaskDetail(task)

	if strings.Contains(got, "act:") {
		t.Errorf("did not expect act: label for a task without live session:\n%s", got)
	}
	if strings.Contains(got, "last output:") {
		t.Errorf("did not expect last output: label for a task without live session:\n%s", got)
	}
}

// TestFormatTaskDetailCapsNone verifies the "caps:" line renders "none" when
// Capabilities is the zero value (Capability_None).
func TestFormatTaskDetailCapsNone(t *testing.T) {
	var taskID protocol.TaskID
	taskID.Id[0] = 0xcd
	task := protocol.TaskInfo{
		Id:     taskID,
		Status: protocol.TaskStatus_Queued,
		// Capabilities zero-value == Capability_None
	}
	task.SetRepoPath([]byte("/repo"))

	got := formatTaskDetail(task)

	if !strings.Contains(got, "caps:") {
		t.Errorf("expected caps: label in detail output:\n%s", got)
	}
	if !strings.Contains(got, "none") {
		t.Errorf("expected none in caps line of detail output:\n%s", got)
	}
}
