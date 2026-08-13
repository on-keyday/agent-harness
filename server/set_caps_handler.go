package server

import (
	"encoding/hex"
	"log/slog"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// handleSetCaps rewrites a live task's authority on the operator's behalf.
//
// The gate is the caller having NO principal task, not a capability bit. A bit
// authorising "rewrite capabilities" is self-amplifying: whoever holds it
// grants itself all on first use and intersectCaps can never claw it back.
// Operator identity is the only ungrantable predicate the server has, and it
// is established at the PSK gate (server/psk.go) where a non-agent client must
// prove operatorPSK — a secret never injected into an agent task environment.
//
// not_operator is a real answer rather than a not-found: it says something
// about the caller, nothing about the target, so it is not an existence oracle.
func (h *TaskHandler) handleSetCaps(conn ConnHandle, requestID uint32, cid string, req *protocol.SetCapsRequest) {
	respond := func(status protocol.SetCapsStatus, affected []string, connsClosed uint32) {
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_SetCaps, RequestId: requestID}
		body := protocol.SetCapsResponse{Status: status, ConnsClosed: connsClosed}
		for _, hexID := range affected {
			raw, err := hex.DecodeString(hexID)
			if err != nil || len(raw) != taskIDHexLen/2 {
				continue
			}
			var tid protocol.TaskID
			copy(tid.Id[:], raw)
			body.Affected = append(body.Affected, tid)
		}
		body.AffectedLen = uint16(len(body.Affected))
		resp.SetSetCaps(body)
		out := resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck
	}

	if h.lookupPrincipal(cid).Id != ([16]byte{}) {
		slog.Warn("set_caps denied: caller is not an operator", "cid", cid)
		respond(protocol.SetCapsStatus_NotOperator, nil, 0)
		return
	}

	targetHex := hex.EncodeToString(req.TaskId.Id[:])
	before, ok := h.Tasks.Get(targetHex)
	if !ok {
		respond(protocol.SetCapsStatus_NotFound, nil, 0)
		return
	}

	newCaps, newScope := before.Capabilities, before.Scope
	if req.CapsPresent() {
		newCaps = req.Caps
	}
	if req.ScopePresent() {
		newScope = scopeFromWire(req.Scope)
	}

	after, ok := h.Tasks.SetCaps(targetHex, req.CapsPresent(), newCaps, req.ScopePresent(), newScope)
	if !ok {
		respond(protocol.SetCapsStatus_NotFound, nil, 0)
		return
	}
	affected := []string{targetHex}
	narrowed := isNarrowing(before, after)

	if req.Cascade() {
		// Attenuation happens at spawn, so a task already spawned keeps what it
		// was given: revoking a parent without cascading leaves it able to act
		// through a child it created while still wide. The cascade re-imposes
		// the monotonic invariant over the whole subtree.
		descendants := make(map[string]bool)
		descendantsOf(h.childIndex(), targetHex, descendants)
		for childHex := range descendants {
			if childHex == targetHex {
				continue
			}
			cBefore, ok := h.Tasks.Get(childHex)
			if !ok {
				continue
			}
			clampedCaps := cBefore.Capabilities & after.Capabilities
			clampedScope := Scope{
				Base: minScopeBase(cBefore.Scope.Base, after.Scope.Base),
				IDs:  filterIDs(cBefore.Scope.IDs, after),
			}
			cAfter, ok := h.Tasks.SetCaps(childHex, true, clampedCaps, true, clampedScope)
			if !ok {
				continue
			}
			affected = append(affected, childHex)
			narrowed = narrowed || isNarrowing(cBefore, cAfter)
		}
	}

	// A narrowing has to reach in-flight work or it is advisory: an attach, a
	// file transfer and a port forward opened under the old grant all outlive
	// it. Closing the connection tears down every stream on it at once, which
	// is why this is done at the connection layer rather than per stream type.
	// The blast radius is correct by construction — these are the connections
	// the revoked task opened OUTWARD; its own PTY rides the runner connection
	// and survives. A pure widening drops nothing.
	var connsClosed uint32
	if narrowed && !req.KeepConns() && h.DropConnsForPrincipal != nil {
		for _, id := range affected {
			connsClosed += uint32(h.DropConnsForPrincipal(id))
		}
	}

	slog.Info("set_caps applied", "task", targetHex, "caps", after.Capabilities.String(),
		"scope", after.Scope.String(), "cascade", req.Cascade(),
		"affected", len(affected), "conns_closed", connsClosed, "narrowed", narrowed)
	respond(protocol.SetCapsStatus_Ok, affected, connsClosed)

	if h.OnChange != nil {
		h.OnChange()
	}
}

// isNarrowing reports whether the change removed authority in any direction:
// a cap bit cleared, a base ranked down, or an explicit id dropped. Only a
// narrowing justifies tearing down live connections.
func isNarrowing(before, after TaskEntry) bool {
	if before.Capabilities&^after.Capabilities != 0 {
		return true
	}
	if scopeBaseRank(after.Scope.Base) < scopeBaseRank(before.Scope.Base) {
		return true
	}
	kept := make(map[string]bool, len(after.Scope.IDs))
	for _, id := range after.Scope.IDs {
		kept[id] = true
	}
	for _, id := range before.Scope.IDs {
		if !kept[id] {
			return true
		}
	}
	return false
}

// filterIDs keeps only the ids that are still inside the parent's post-change
// effective set. A child's explicit grant cannot outlive the grant it came
// from.
func filterIDs(ids []string, parent TaskEntry) []string {
	if len(ids) == 0 || parent.Scope.Base == protocol.ScopeBase_Global {
		return ids
	}
	allowed := map[string]bool{parent.ID: true}
	for _, id := range parent.Scope.IDs {
		allowed[id] = true
	}
	out := ids[:0:0]
	for _, id := range ids {
		if allowed[id] {
			out = append(out, id)
		}
	}
	return out
}
