package server

import (
	"encoding/hex"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func newGitQueryHandler() *TaskHandler {
	return &TaskHandler{Tasks: NewTaskStore(), Registry: NewRegistry(), Sessions: NewSessionRegistry()}
}

// seedGitTask creates a task assigned to a runner id that is NOT in the
// registry, then drives it to the given terminal-or-live status.
func seedGitTask(t *testing.T, h *TaskHandler, finish bool) protocol.TaskID {
	t.Helper()
	id := h.Tasks.Create("/srv/repo", "prompt", protocol.TaskKind_Oneshot,
		protocol.ClientKind_Cli, protocol.TaskID{}, "", protocol.RunnerSelector{},
		nil, protocol.Capability_All, "")
	h.Tasks.Assign(id, "runner-that-is-gone", "/srv/repo/.harness-worktrees/"+id)
	if finish {
		h.Tasks.Finish(id, 0, nil)
	}
	raw, err := hex.DecodeString(id)
	if err != nil {
		t.Fatalf("task id %q is not hex: %v", id, err)
	}
	var tid protocol.TaskID
	copy(tid.Id[:], raw)
	return tid
}

func TestGitQueryUnknownTask(t *testing.T) {
	h := newGitQueryHandler()
	var req protocol.GitQueryRequest
	req.TaskId = protocol.TaskID{Id: [16]byte{9, 9, 9}}
	req.Kind = protocol.GitQueryKind_Status

	if resp := h.handleGitQuery(nil, &req); resp.Status != protocol.GitQueryStatus_NoSuchTask {
		t.Fatalf("status = %v, want no_such_task", resp.Status)
	}
}

func TestGitQueryRunnerOffline(t *testing.T) {
	h := newGitQueryHandler()
	tid := seedGitTask(t, h, false)
	var req protocol.GitQueryRequest
	req.TaskId = tid
	req.Kind = protocol.GitQueryKind_Status

	if resp := h.handleGitQuery(nil, &req); resp.Status != protocol.GitQueryStatus_RunnerOffline {
		t.Fatalf("status = %v, want runner_offline", resp.Status)
	}
}

// A finished task must get past the task lookup. The file ops answer
// no_such_task for anything outside Running/Detached; git_query deliberately
// does not, because the retained harness/<taskID> branch still holds the work.
// Reaching the runner lookup (runner_offline) instead of stopping at the task
// lookup (no_such_task) is what proves the gate is absent.
func TestGitQueryDoesNotGateOnTaskStatus(t *testing.T) {
	h := newGitQueryHandler()
	tid := seedGitTask(t, h, true)

	hexID := hex.EncodeToString(tid.Id[:])
	task, ok := h.Tasks.Get(hexID)
	if !ok {
		t.Fatal("seeded task vanished")
	}
	if task.Status == protocol.TaskStatus_Running || task.Status == protocol.TaskStatus_Detached {
		t.Fatalf("fixture is not terminal (status %v); the test proves nothing", task.Status)
	}

	var req protocol.GitQueryRequest
	req.TaskId = tid
	req.Kind = protocol.GitQueryKind_Log

	resp := h.handleGitQuery(nil, &req)
	if resp.Status == protocol.GitQueryStatus_NoSuchTask {
		t.Fatal("a terminal task was rejected at the task lookup; the Running/Detached gate leaked in")
	}
	if resp.Status != protocol.GitQueryStatus_RunnerOffline {
		t.Fatalf("status = %v, want runner_offline (the runner is deliberately absent)", resp.Status)
	}
}

func TestGitQueryRequiresFileRead(t *testing.T) {
	got, gated := requiredCap[protocol.TaskControlKind_GitQuery]
	if !gated {
		t.Fatal("git_query is not in requiredCap; it would dispatch ungated")
	}
	if got != protocol.Capability_FileRead {
		t.Fatalf("git_query requires %v, want file_read", got)
	}
}
