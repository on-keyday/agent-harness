package server

import (
	"encoding/hex"
	"log/slog"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/authority"
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
			if err != nil || len(raw) != authority.TaskIDHexLen/2 {
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
		newScope = authority.ScopeFromWire(req.Scope, req.Overrides)
		// Operator identity is a licence to grant any AUTHORITY, not to store
		// an incoherent value: an empty or overlapping override mask would
		// make ForCap's "at most one entry matches" false, and a non-canonical
		// vis_base gives one authority two encodings. Attenuation is still
		// skipped here on purpose — an operator may widen past its own reach.
		//
		// This path had no validation at all, which is how live tasks ended up
		// carrying scopes the spawn path would have refused.
		if err := authority.Validate(newScope); err != nil {
			slog.Warn("set_caps denied: inconsistent scope", "cid", cid, "err", err)
			respond(protocol.SetCapsStatus_InvalidScope, nil, 0)
			return
		}
	}

	after, ok := h.Tasks.SetCaps(targetHex, req.CapsPresent(), newCaps, req.ScopePresent(), newScope,
		AuthorityWrite{Source: protocol.AuthoritySource_OperatorCapsSet})
	if !ok {
		respond(protocol.SetCapsStatus_NotFound, nil, 0)
		return
	}
	affected := []string{targetHex}
	narrowed := authority.IsNarrowing(before.Capabilities, before.Scope, after.Capabilities, after.Scope)

	if req.Cascade() {
		// Attenuation happens at spawn, so a task already spawned keeps what it
		// was given: revoking a parent without cascading leaves it able to act
		// through a child it created while still wide. The cascade re-imposes
		// the monotonic invariant over the whole subtree.
		children := h.childIndex()
		descendants := make(map[string]bool)
		authority.DescendantsOf(children, targetHex, descendants)
		for childHex := range descendants {
			if childHex == targetHex {
				continue
			}
			cBefore, ok := h.Tasks.Get(childHex)
			if !ok {
				continue
			}
			clampedCaps := cBefore.Capabilities & after.Capabilities
			clampedScope := authority.ClampUnder(cBefore.Scope, clampedCaps, authority.NewReach(after.ID, after.Scope, children))
			cAfter, ok := h.Tasks.SetCaps(childHex, true, clampedCaps, true, clampedScope,
				AuthorityWrite{Source: protocol.AuthoritySource_Cascade, Actor: req.TaskId})
			if !ok {
				continue
			}
			affected = append(affected, childHex)
			narrowed = narrowed || authority.IsNarrowing(cBefore.Capabilities, cBefore.Scope, cAfter.Capabilities, cAfter.Scope)
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
	if narrowed && !req.KeepConns() {
		for _, id := range affected {
			if h.DropConnsForPrincipal != nil {
				connsClosed += uint32(h.DropConnsForPrincipal(id))
			}
			// A data-plane transfer is not among those connections: it runs
			// between the client and the RUNNER, with this process forwarding
			// packets it cannot read. Dropping the task's outward connections
			// leaves it running, so it is withdrawn explicitly.
			if h.RevokeDataPlaneForTask != nil {
				connsClosed += uint32(h.RevokeDataPlaneForTask(id))
			}
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
