package server

import (
	"encoding/hex"
	"errors"
	"log/slog"
	"time"

	"github.com/on-keyday/agent-harness/actiondef"
	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// Server-defined actions: the three verbs and the gate on an edit
// (docs/superpowers/specs/2026-11-26-grant-conditions-and-bound-enrollment-design.md,
// C11, C13, C17).

// actionBodyMax bounds the definition a put reads. The largest valid
// definition (64 literals of 4096 bytes, 16 params of 32 choices of 255
// bytes, a 1 KiB description) is under 400 KiB.
const actionBodyMax = 512 << 10

// actionMessageMax bounds ActionPutResponse.message: the response is inline,
// and a control message is one datagram.
const actionMessageMax = 256

// errHoldersUnreachable is actionEditRefusals' answer as an Edit gate error.
var errHoldersUnreachable = errors.New("holders unreachable")

// actionBy is how a record names who wrote it, as RunnerKeyRecord.EnrolledBy.
func (h *TaskHandler) actionBy(cid objproto.ConnectionID) string {
	if self := h.principalHex(cid); self != "" {
		return "task:" + self
	}
	return "operator"
}

func cutMessage(s string) []byte {
	if len(s) > actionMessageMax {
		s = s[:actionMessageMax]
	}
	return []byte(s)
}

// handleActionPut creates (zero id) or edits one action. The definition rides
// the request's send stream; it is read here, so this runs off the receive
// loop. An edit names the revision it edits (stale_revision otherwise) and is
// refused whole unless the editor could re-grant every holder (C11).
func (h *TaskHandler) handleActionPut(conn ConnHandle, cid objproto.ConnectionID, req protocol.ActionPutRequest) protocol.ActionPutResponse {
	answer := func(st protocol.ActionStatus, msg string) protocol.ActionPutResponse {
		r := protocol.ActionPutResponse{Status: st}
		r.SetMessage(cutMessage(msg))
		return r
	}
	raw, err := readAgentPayloadStream(conn, req.PayloadStreamId, actionBodyMax)
	if err != nil {
		slog.Warn("action_put: definition body", "err", err)
		if errors.Is(err, errPayloadTooLarge) {
			return answer(protocol.ActionStatus_BodyTooLarge, "the definition is larger than the server reads")
		}
		return answer(protocol.ActionStatus_BadBody, "the definition could not be read")
	}
	var def protocol.ActionDefinition
	if err := def.DecodeExactCopy(raw); err != nil {
		return answer(protocol.ActionStatus_BadBody, "the definition did not decode")
	}
	if h.Actions == nil {
		return answer(protocol.ActionStatus_InternalError, "this server keeps no actions")
	}
	create := req.Id.Id == ([16]byte{})
	switch {
	case create && req.ExpectRevision != 0:
		return answer(protocol.ActionStatus_InvalidDefinition, "a create names revision 0")
	case !create && req.ExpectRevision == 0:
		return answer(protocol.ActionStatus_InvalidDefinition, "an edit names the revision it edits")
	}
	if err := actiondef.Validate(&def); err != nil {
		return answer(protocol.ActionStatus_InvalidDefinition, err.Error())
	}
	by, now := h.actionBy(cid), time.Now()
	var rec protocol.ActionRecord
	var refused uint32
	var self bool
	if create {
		rec, err = h.Actions.Create(def, by, now)
	} else {
		// Lock order: ActionStore.mu, then the task and schedule stores
		// (read). Nothing takes ActionStore.mu while holding either.
		rec, err = h.Actions.Edit(req.Id.Id, req.ExpectRevision, def, by, now, func() error {
			refused, self = h.actionEditRefusals(cid, req.Id.Id)
			if self || refused > 0 {
				return errHoldersUnreachable
			}
			return nil
		})
	}
	switch {
	case err == nil:
	case errors.Is(err, ErrActionNotFound):
		return answer(protocol.ActionStatus_NotFound, "")
	case errors.Is(err, ErrActionNameTaken):
		return answer(protocol.ActionStatus_NameTaken, "another action is named "+string(def.Name))
	case errors.Is(err, ErrStaleRevision):
		return answer(protocol.ActionStatus_StaleRevision, "the action was edited since that revision; list it and edit the current one")
	case errors.Is(err, errHoldersUnreachable):
		r := answer(protocol.ActionStatus_HoldersUnreachable, holdersMessage(self, refused))
		r.HoldersRefused = refused
		return r
	default:
		slog.Error("action_put: store", "err", err)
		return answer(protocol.ActionStatus_InternalError, "could not save the action")
	}
	slog.Info("action_put", "action", hex.EncodeToString(rec.Id.Id[:]), "name", string(def.Name), "revision", rec.Revision, "by", by)
	return protocol.ActionPutResponse{Status: protocol.ActionStatus_Ok, Id: rec.Id, Revision: rec.Revision}
}

func holdersMessage(self bool, refused uint32) string {
	if self {
		return "your own grant names this action: editing it would change your own authority"
	}
	return "an edit changes what every holder runs, and you could not re-grant every holder of this action (as caps set would need)"
}

// handleActionRemove deletes one action. Deleting only narrows — a grant
// naming it then reaches nothing (C7) — so it needs action_admin alone (C11).
func (h *TaskHandler) handleActionRemove(cid objproto.ConnectionID, req protocol.ActionRemoveRequest) protocol.ActionRemoveResponse {
	if h.Actions == nil {
		return protocol.ActionRemoveResponse{Status: protocol.ActionStatus_NotFound}
	}
	switch err := h.Actions.Remove(req.Id.Id); {
	case err == nil:
		slog.Info("action_remove", "action", hex.EncodeToString(req.Id.Id[:]), "by", h.actionBy(cid))
		return protocol.ActionRemoveResponse{Status: protocol.ActionStatus_Ok}
	case errors.Is(err, ErrActionNotFound):
		return protocol.ActionRemoveResponse{Status: protocol.ActionStatus_NotFound}
	default:
		slog.Error("action_remove: store", "err", err)
		return protocol.ActionRemoveResponse{Status: protocol.ActionStatus_InternalError}
	}
}

// handleActionList writes every action on a send stream. Every caller that
// passed the gate (action_admin or exec_run) sees every action: a definition
// is server configuration, not a per-task object.
func (h *TaskHandler) handleActionList(conn ConnHandle, requestID uint32) {
	body := &protocol.ActionListBody{}
	if h.Actions != nil {
		body.SetActions(h.Actions.List())
	}
	respondWithBody(conn, "action_list", body, func(streamID uint64) {
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_ActionList, RequestId: requestID}
		resp.SetActionList(protocol.ActionListResponse{Status: protocol.ActionStatus_Ok, StreamId: streamID})
		respondAgent(conn, resp)
	})
}

// actionEditRefusals is C11's gate on an edit of id. The holders are every
// task in the store, whatever its status (a finished task can be resumed with
// its recorded caps), and every schedule, whose grant names id. The operator
// passes. A task editor passes a task holder through R1 (regrantReaches) and
// R2 (execRunCovers), and a schedule holder through scheduleRewritable.
// selfHolder: the editor's own grant names id, which is always refused —
// editing it would change the editor's own authority, as regrantReaches
// refuses a caller's own task.
func (h *TaskHandler) actionEditRefusals(cid objproto.ConnectionID, id [16]byte) (refused uint32, selfHolder bool) {
	self := h.principalHex(cid)
	if self == "" {
		return 0, false
	}
	children := h.childIndex()
	for _, t := range h.Tasks.List(0) {
		if !t.Scope.NamesAction(id) {
			continue
		}
		if t.ID == self {
			selfHolder = true
			continue
		}
		if !h.regrantReaches(cid, t.ID) || !h.execRunCovers(cid, t, children) {
			refused++
		}
	}
	if h.Sched != nil && h.Sched.Store != nil {
		for _, sc := range h.Sched.Store.List() {
			if sc.Grant.Scope.NamesAction(id) && !h.scheduleRewritable(cid, sc) {
				refused++
			}
		}
	}
	return refused, selfHolder
}

// execRunCovers is C11's R2: the caller holds exec_run with no condition, and
// its exec_run reaches every target holder's exec_run reaches (the holder's
// condition left unapplied: the edit changes what runs there).
func (h *TaskHandler) execRunCovers(cid objproto.ConnectionID, holder TaskEntry, children map[string][]string) bool {
	run := requiredCentral(protocol.TaskControlKind_OpenExecRun)
	if !hasCap(h.callerCaps(cid), run) {
		return false
	}
	eAll, eAllowed, cond := h.conditionedScopeSet(cid, run, children)
	if cond != nil {
		return false
	}
	if eAll {
		return true
	}
	hAll, hAllowed := authority.ActionSet(holder.ID, holder.Scope, run, children)
	if hAll {
		return false
	}
	for target, in := range hAllowed {
		if in && !eAllowed[target] {
			return false
		}
	}
	return true
}

// scheduleRewritable is what a flag-less `schedule edit` of sc by the caller
// needs (C11, the plan's decision (b)): reach, not the schedule's own task,
// the power the edit exercises, and the stored grant rewritten unchanged with
// the caller as granter (S-D16) — plus R2's other half, an unconditioned
// exec_run.
func (h *TaskHandler) scheduleRewritable(cid objproto.ConnectionID, sc Schedule) bool {
	callerCaps := h.callerCaps(cid)
	if !hasCap(callerCaps, requiredCentral(protocol.TaskControlKind_ScheduleEdit)) {
		return false
	}
	if !h.scheduleReach(cid, sc) || h.scheduleOwnTask(cid, sc) {
		return false
	}
	if c, refusal := h.schedulePower(cid, &sc, ""); c != 0 || refusal != nil {
		return false
	}
	if _, _, cond := h.conditionedScopeSet(cid, requiredCentral(protocol.TaskControlKind_OpenExecRun), h.childIndex()); cond != nil {
		return false
	}
	greq, err := grantRequestOver(sc.Grant, &protocol.ScheduleSpec{}, protocol.GrantConditions{})
	if err != nil {
		return false
	}
	boundHex := ""
	if sc.State == protocol.ScheduleTaskState_Bound {
		boundHex = sc.TaskID
	}
	_, err = h.storedGrant(h.granter(cid, callerCaps), greq, boundHex)
	return err == nil
}
