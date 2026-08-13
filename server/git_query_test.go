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
		nil, protocol.Capability_All, Scope{}, "")
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

// Every field a client can set has to reach the runner. This relay is one site
// for a struct that keeps growing, which is the shape that has silently
// dropped a new field on this project before — so assert them all, not a
// representative one.
func TestRunnerGitRequestRelaysEveryField(t *testing.T) {
	var req protocol.GitQueryRequest
	req.TaskId = protocol.TaskID{Id: [16]byte{1, 2, 3}}
	req.Kind = protocol.GitQueryKind_Show
	req.Target = protocol.GitDiffTarget_Index
	req.MaxCommits = 11
	req.MaxBytes = 4242
	req.SetBaseRev([]byte("HEAD~2"))
	req.SetTargetRev([]byte("abc123"))
	req.SetPath([]byte("src/x.go"))
	req.SetSubrepo([]byte("pkg/inner"))
	req.SetSubmoduleDiff(true)

	got := runnerGitRequest(&req, "/srv/repo", 77)

	if got.TaskId != req.TaskId {
		t.Errorf("task id: %v", got.TaskId)
	}
	if got.StreamId != 77 {
		t.Errorf("stream id: %d", got.StreamId)
	}
	if string(got.RepoPath) != "/srv/repo" {
		t.Errorf("repo path: %q", got.RepoPath)
	}
	if got.Kind != protocol.GitQueryKind_Show {
		t.Errorf("kind: %v", got.Kind)
	}
	if got.Target != protocol.GitDiffTarget_Index {
		t.Errorf("target: %v", got.Target)
	}
	if got.MaxCommits != 11 || got.MaxBytes != 4242 {
		t.Errorf("caps: %d %d", got.MaxCommits, got.MaxBytes)
	}
	if string(got.BaseRev) != "HEAD~2" {
		t.Errorf("base rev: %q", got.BaseRev)
	}
	if string(got.TargetRev) != "abc123" {
		t.Errorf("target rev: %q", got.TargetRev)
	}
	if string(got.Path) != "src/x.go" {
		t.Errorf("path: %q", got.Path)
	}
	if string(got.Subrepo) != "pkg/inner" {
		t.Errorf("subrepo: %q", got.Subrepo)
	}
	if !got.SubmoduleDiff() {
		t.Error("submodule_diff dropped")
	}
}
