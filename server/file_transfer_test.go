package server

import (
	"encoding/hex"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TestHandleOpenFileTransfer_NoSuchTask exercises the early-exit branch
// when the requested task id is not present in the TaskStore (or is not
// in the Running state). The handler must return NoSuchTask without
// touching streams — passing nil for ConnHandle is safe here precisely
// because the lookup fails before any stream-allocation step runs.
func TestHandleOpenFileTransfer_NoSuchTask(t *testing.T) {
	h := &TaskHandler{
		Tasks:    NewTaskStore(),
		Registry: NewRegistry(),
	}
	req := &protocol.OpenFileTransferRequest{
		Direction: protocol.FileTransferDirection_Push,
	}
	// task_id remains zero; not registered in store.
	resp := h.handleOpenFileTransfer(nil, req)
	if resp.Status != protocol.OpenFileTransferStatus_NoSuchTask {
		t.Fatalf("status = %v want no_such_task", resp.Status)
	}
}

// TestHandleListFiles_NoSuchTask is the symmetric case for list_files.
func TestHandleListFiles_NoSuchTask(t *testing.T) {
	h := &TaskHandler{
		Tasks:    NewTaskStore(),
		Registry: NewRegistry(),
	}
	req := &protocol.ListFilesRequest{}
	resp := h.handleListFiles(nil, req)
	if resp.Status != protocol.ListFilesStatus_NoSuchTask {
		t.Fatalf("status = %v want no_such_task", resp.Status)
	}
}

// TestHandleOpenFileTransfer_DetachedTaskAccepted verifies that the status
// gate accepts Detached as well as Running. Detachable interactive tasks
// transition to Detached when their TUI/CLI client disconnects but the
// runner-side worktree remains reachable, so file ops MUST stay available.
//
// We bypass the asserting TaskStore mutators (SetDetached requires Running)
// by injecting the TaskEntry directly under the store's lock — same pattern
// used elsewhere in this package (see task_handler_test.go AttachSession
// tests). The runner is intentionally NOT registered: the expected outcome
// is RunnerOffline (proving the status gate let us through), not NoSuchTask
// (which would prove the gate rejected Detached).
func TestHandleOpenFileTransfer_DetachedTaskAccepted(t *testing.T) {
	h := &TaskHandler{
		Tasks:    NewTaskStore(),
		Registry: NewRegistry(),
	}
	var rawID [16]byte
	rawID[0] = 0xD1
	idHex := hex.EncodeToString(rawID[:])
	h.Tasks.mu.Lock()
	h.Tasks.tasks[idHex] = &TaskEntry{
		ID:         idHex,
		RepoPath:   "/repo",
		Status:     protocol.TaskStatus_Detached,
		Kind:       protocol.TaskKind_Interactive,
		AssignedTo: "fake-runner-id",
	}
	h.Tasks.order = append(h.Tasks.order, idHex)
	h.Tasks.mu.Unlock()

	req := &protocol.OpenFileTransferRequest{
		TaskId:    protocol.TaskID{Id: rawID},
		Direction: protocol.FileTransferDirection_Push,
	}
	resp := h.handleOpenFileTransfer(nil, req)
	if resp.Status == protocol.OpenFileTransferStatus_NoSuchTask {
		t.Fatalf("detached task must not be rejected as NoSuchTask")
	}
	// Runner not registered → RunnerOffline. (We rely on this preceding the
	// nil-conn InternalError check in the handler; see file_transfer.go.)
	if resp.Status != protocol.OpenFileTransferStatus_RunnerOffline {
		t.Fatalf("expected RunnerOffline (no runner registered), got %v", resp.Status)
	}
}

// TestHandleListFiles_DetachedTaskAccepted is the symmetric case for ls.
func TestHandleListFiles_DetachedTaskAccepted(t *testing.T) {
	h := &TaskHandler{
		Tasks:    NewTaskStore(),
		Registry: NewRegistry(),
	}
	var rawID [16]byte
	rawID[0] = 0xD2
	idHex := hex.EncodeToString(rawID[:])
	h.Tasks.mu.Lock()
	h.Tasks.tasks[idHex] = &TaskEntry{
		ID:         idHex,
		RepoPath:   "/repo",
		Status:     protocol.TaskStatus_Detached,
		Kind:       protocol.TaskKind_Interactive,
		AssignedTo: "fake-runner-id",
	}
	h.Tasks.order = append(h.Tasks.order, idHex)
	h.Tasks.mu.Unlock()

	req := &protocol.ListFilesRequest{TaskId: protocol.TaskID{Id: rawID}}
	resp := h.handleListFiles(nil, req)
	if resp.Status == protocol.ListFilesStatus_NoSuchTask {
		t.Fatalf("detached task must not be rejected as NoSuchTask")
	}
	if resp.Status != protocol.ListFilesStatus_RunnerOffline {
		t.Fatalf("expected RunnerOffline (no runner registered), got %v", resp.Status)
	}
}
