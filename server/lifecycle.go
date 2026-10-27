package server

import (
	"encoding/hex"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// handleLifecycleSet records the caller's OWN lifecycle state, as reported by
// the agent hooks the runner injects (spec 2026-10-27-task-lifecycle-design).
// The target is the connection's principal, as for progress_set, so no
// capability is needed and an operator connection is refused. A report whose
// reporter stamp is older than the stored one is answered stale and publishes
// nothing: async hooks may land out of order.
func (h *TaskHandler) handleLifecycleSet(conn ConnHandle, req *protocol.TaskControlRequest) {
	respond := func(status protocol.LifecycleSetStatus, at int64) {
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_LifecycleSet, RequestId: req.RequestId}
		resp.SetLifecycleSet(protocol.LifecycleSetResponse{Status: status, LifecycleAt: uint64(at)})
		conn.SendMessage(resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})) //nolint:errcheck
	}
	ls := req.LifecycleSet()
	if ls == nil {
		respond(protocol.LifecycleSetStatus_BadRequest, 0)
		return
	}
	pid := h.lookupPrincipal(conn.ConnectionID().String())
	if pid.Id == ([16]byte{}) {
		respond(protocol.LifecycleSetStatus_NoPrincipal, 0)
		return
	}
	switch ls.Lifecycle {
	case protocol.LifecycleState_Working, protocol.LifecycleState_Waiting,
		protocol.LifecycleState_Idle, protocol.LifecycleState_Error:
	default:
		respond(protocol.LifecycleSetStatus_BadRequest, 0)
		return
	}
	taskHex := hex.EncodeToString(pid.Id[:])
	snap, status := h.Tasks.SetLifecycle(taskHex, ls.Lifecycle, int64(ls.ReportedAt), time.Now().UnixNano())
	if status != protocol.LifecycleSetStatus_Ok {
		respond(status, 0)
		return
	}
	if h.OnTaskLifecycle != nil {
		h.OnTaskLifecycle(taskHex)
	}
	respond(protocol.LifecycleSetStatus_Ok, snap.LifecycleAt)
}
