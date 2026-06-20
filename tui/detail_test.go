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
