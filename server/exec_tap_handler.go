package server

import (
	"log/slog"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// handleOpenExecTap attaches a tap to one exec and answers with the stream its
// records arrive on.
//
// The three gates of handleOpenForwardTap, in the same order and for the same
// reasons:
//
//  1. Holding exec_tap — checked before dispatch off requiredCap, answered with
//     PermissionDenied rather than a status here.
//  2. Visibility — an id the caller cannot see answers no_such_exec, the same
//     answer an unknown id gets. Exec ids come from a dense next++ counter, so a
//     distinguishable refusal would be an enumeration oracle.
//  3. Scope — an exec visible through a global visibility rank may still belong
//     to a task outside the caller's ACTION scope. inScope rather than
//     authorize: the hasCap half has already run in (1).
func (h *TaskHandler) handleOpenExecTap(conn ConnHandle, req *protocol.OpenExecTapRequest, connID string) protocol.OpenExecTapResponse {
	errResp := func(s protocol.OpenExecTapStatus) protocol.OpenExecTapResponse {
		return protocol.OpenExecTapResponse{Status: s}
	}
	e, ok := h.execs().get(req.ExecId)
	if !ok || !h.execVisibleTo(connID, e) {
		return errResp(protocol.OpenExecTapStatus_NoSuchExec)
	}
	if !h.inScope(connID, protocol.Capability_ExecTap, e.taskIDHex) {
		return errResp(protocol.OpenExecTapStatus_NoSuchExec)
	}
	if conn == nil {
		slog.Error("exec tap: nil client conn (programmer error)")
		return errResp(protocol.OpenExecTapStatus_InternalError)
	}
	stream := conn.CreateBidirectionalStream()
	if stream == nil {
		return errResp(protocol.OpenExecTapStatus_InternalError)
	}

	tap := newExecTap(&streamRecordSink[*protocol.ExecTapRecord]{stream: stream}, req.ChannelFilter, req.MaxRecordBytes)
	e.addTap(tap)
	serveTapStream(stream, tap.run, func() { e.removeTap(tap) })

	slog.Info("exec tap: attached", "exec_id", e.execID, "task_id", e.taskIDHex,
		"filter", req.ChannelFilter, "max_record_bytes", req.MaxRecordBytes)
	return protocol.OpenExecTapResponse{
		Status:   protocol.OpenExecTapStatus_Ok,
		StreamId: uint64(stream.ID()),
	}
}
