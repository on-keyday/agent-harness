package server

import (
	"encoding/hex"
	"log/slog"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// handleSetParent re-points a live task's parent link (its creator_task_id) on
// the operator's behalf, or — swap — inverts the task with its current parent.
//
// Gate and rationale mirror handleSetCaps: operator identity (no principal
// task) is the only ungrantable predicate the server has. A capability bit
// authorising "re-point parents" would be self-amplifying — its holder adopts
// any victim task under itself and inherits that victim's whole subtree as a
// target set, and spawn-time intersection can never claw that back.
//
// Deliberately NOT here (spec 2026-08-14-task-reparent-design):
//   - no caps/scope mutation — reparenting moves the target set, not the
//     granted verbs; `caps set` is the verb that changes authority
//   - no connection teardown — nothing is revoked, and the primary use case
//     (role inversion) wants the former parent's live attach kept
//   - no descendant list in the response — descendants follow implicitly,
//     their own links are untouched
func (h *TaskHandler) handleSetParent(conn ConnHandle, requestID uint32, cid string, req *protocol.SetParentRequest) {
	respond := func(status protocol.SetParentStatus, oldP, newP, swapped protocol.TaskID) {
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_SetParent, RequestId: requestID}
		resp.SetSetParent(protocol.SetParentResponse{Status: status, OldParent: oldP, NewParent: newP, SwappedId: swapped})
		out := resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck
	}
	var zero protocol.TaskID

	if h.lookupPrincipal(cid).Id != ([16]byte{}) {
		slog.Warn("set_parent denied: caller is not an operator", "cid", cid)
		respond(protocol.SetParentStatus_NotOperator, zero, zero, zero)
		return
	}
	targetHex := hex.EncodeToString(req.TaskId.Id[:])

	if req.Swap() {
		if req.ParentId.Id != ([16]byte{}) {
			respond(protocol.SetParentStatus_SwapTakesNoParent, zero, zero, zero)
			return
		}
		target, former, err := h.Tasks.SwapWithParent(targetHex)
		switch err {
		case nil:
		case ErrSwapNotFound:
			respond(protocol.SetParentStatus_NotFound, zero, zero, zero)
			return
		case ErrSwapNoParent:
			respond(protocol.SetParentStatus_NoParent, zero, zero, zero)
			return
		case ErrSwapParentMissing:
			respond(protocol.SetParentStatus_ParentNotFound, zero, zero, zero)
			return
		default:
			respond(protocol.SetParentStatus_InternalError, zero, zero, zero)
			return
		}
		formerID := taskIDFromHex(former.ID)
		slog.Info("set_parent swap applied", "task", targetHex,
			"now_under", hexOrRoot(target.CreatorTaskID), "former_parent", former.ID)
		respond(protocol.SetParentStatus_Ok, formerID, target.CreatorTaskID, formerID)
		if h.OnChange != nil {
			h.OnChange()
		}
		return
	}

	before, ok := h.Tasks.Get(targetHex)
	if !ok {
		respond(protocol.SetParentStatus_NotFound, zero, zero, zero)
		return
	}
	if req.ParentId.Id != ([16]byte{}) {
		parentHex := hex.EncodeToString(req.ParentId.Id[:])
		if _, ok := h.Tasks.Get(parentHex); !ok {
			respond(protocol.SetParentStatus_ParentNotFound, zero, zero, zero)
			return
		}
		// Cycle check — this is what keeps listVisibleToCaller's "only
		// reachable through a creator cycle, which task creation cannot
		// produce" true. descendantsOf seeds the walk with the root it is
		// asked about (allowed[targetHex] = true), so parent == target is
		// inside this predicate too; no separate self-parent branch.
		descendants := make(map[string]bool)
		descendantsOf(h.childIndex(), targetHex, descendants)
		if descendants[parentHex] {
			respond(protocol.SetParentStatus_WouldCycle, zero, zero, zero)
			return
		}
	}
	after, ok := h.Tasks.SetParent(targetHex, req.ParentId)
	if !ok {
		respond(protocol.SetParentStatus_NotFound, zero, zero, zero)
		return
	}
	slog.Info("set_parent applied", "task", targetHex,
		"old", hexOrRoot(before.CreatorTaskID), "new", hexOrRoot(after.CreatorTaskID))
	respond(protocol.SetParentStatus_Ok, before.CreatorTaskID, after.CreatorTaskID, zero)
	if h.OnChange != nil {
		h.OnChange()
	}
}

// hexOrRoot renders a parent link for logs: "(root)" for the zero id.
func hexOrRoot(id protocol.TaskID) string {
	if id.Id == ([16]byte{}) {
		return "(root)"
	}
	return hex.EncodeToString(id.Id[:])
}
