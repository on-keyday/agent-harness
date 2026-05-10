package server

import (
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
