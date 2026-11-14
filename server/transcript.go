package server

import (
	"encoding/hex"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// handleTranscript answers transcript_list and transcript_get (pillar B). The
// bit (transcript_read) is enforced centrally via requiredCap; the TARGET is
// gated here against the bit's own scope, so a task out of scope answers
// no_such_task, as git_query does.
//
// Task 1 stub: gated, but answers internal_error until the runner path lands.
func (h *TaskHandler) handleTranscript(cid string, conn ConnHandle, req *protocol.TaskControlRequest) {
	resp := protocol.TaskControlResponse{Kind: req.Header.Kind, RequestId: req.Header.RequestId}
	var taskID protocol.TaskID
	switch req.Header.Kind {
	case protocol.TaskControlKind_TranscriptList:
		if r := req.TranscriptList(); r != nil {
			taskID = r.TaskId
		}
	case protocol.TaskControlKind_TranscriptGet:
		if r := req.TranscriptGet(); r != nil {
			taskID = r.TaskId
		}
	default:
		return
	}
	out := protocol.TranscriptResponse{Status: protocol.TranscriptStatus_InternalError}
	if !h.inScope(cid, protocol.Capability_TranscriptRead, hex.EncodeToString(taskID.Id[:])) {
		out.Status = protocol.TranscriptStatus_NoSuchTask
	}
	if req.Header.Kind == protocol.TaskControlKind_TranscriptList {
		resp.SetTranscriptList(out)
	} else {
		resp.SetTranscriptGet(out)
	}
	conn.SendMessage(resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})) //nolint:errcheck
}
