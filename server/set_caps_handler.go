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
		children := h.childIndex()
		descendants := make(map[string]bool)
		descendantsOf(children, targetHex, descendants)
		for childHex := range descendants {
			if childHex == targetHex {
				continue
			}
			cBefore, ok := h.Tasks.Get(childHex)
			if !ok {
				continue
			}
			clampedCaps := cBefore.Capabilities & after.Capabilities
			clampedScope := clampScopeUnder(cBefore.Scope, clampedCaps, after, children)
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

// isNarrowing reports whether the change removed authority in any direction:
// a cap bit cleared, the scope of a still-held bit narrowed, or the visible set
// narrowed. Only a narrowing justifies tearing down live connections.
//
// Judged per bit, because a cascade narrows per bit: comparing the base scope
// alone missed an override ranked down, so an attach opened under the wider
// grant outlived it.
func isNarrowing(before, after TaskEntry) bool {
	if before.Capabilities&^after.Capabilities != 0 {
		return true
	}
	if reachNarrowed(before.Scope.VisRank(), false, visibleIDs(before.Scope),
		after.Scope.VisRank(), false, visibleIDs(after.Scope)) {
		return true
	}
	exercised := exercisedBits(before.Capabilities)
	for bit := protocol.Capability(1); bit != 0 && bit <= protocol.Capability_All; bit <<= 1 {
		if exercised&bit == 0 {
			continue
		}
		bBase, bExcl, bIDs := before.Scope.ForCap(bit)
		aBase, aExcl, aIDs := after.Scope.ForCap(bit)
		if reachNarrowed(bBase, bExcl, bIDs, aBase, aExcl, aIDs) {
			return true
		}
	}
	return false
}

// reachNarrowed compares two descriptions of one target set: a rank ranked
// down, self newly excluded, or an explicit id no longer named. A global rank
// afterwards covers everything, so nothing it replaced was narrowed.
func reachNarrowed(bBase protocol.ScopeBase, bExcl bool, bIDs []string, aBase protocol.ScopeBase, aExcl bool, aIDs []string) bool {
	if aBase == protocol.ScopeBase_Global {
		return false
	}
	if authority.BaseRank(aBase) < authority.BaseRank(bBase) || (aExcl && !bExcl) {
		return true
	}
	kept := make(map[string]bool, len(aIDs))
	for _, id := range aIDs {
		kept[id] = true
	}
	for _, id := range bIDs {
		if !kept[id] {
			return true
		}
	}
	return false
}

// visibleIDs is every id a scope makes visible on top of its rank — the id
// half of visibilitySet's formula.
func visibleIDs(s authority.Scope) []string {
	out := append(append([]string(nil), s.VisIDs...), s.IDs...)
	for _, o := range s.Overrides {
		out = append(out, o.IDs...)
	}
	return out
}

// clampScopeUnder re-imposes the monotonic invariant on ONE descendant after
// its parent's authority changed. Every axis is clamped here, in one function,
// because the inline struct literal this replaced carried Base and IDs only —
// a cascade silently erased a child's visibility rank and its overrides, which
// is a rewrite of authority nothing reported.
//
// It measures against the parent through grantReach, the same helper spawn-time
// attenuation uses, so a cascade and a spawn agree on what the parent covers:
// each held bit's rank and ids are bounded by the parent's scope FOR THAT BIT
// (boundRanks, which narrows only the bits the parent narrows), and an id
// survives when it is still inside the parent's reach — not only when the
// parent names it, which dropped ids inside the parent's subtree. Ids outside
// it are dropped; a cascade has no caller to refuse.
//
// childCaps is the descendant's ALREADY-clamped capability mask, so an
// override for a bit it no longer holds or exercises (exercisedBits) is
// dropped rather than left to reapply if the bit is granted again later.
func clampScopeUnder(child authority.Scope, childCaps protocol.Capability, parent TaskEntry, children map[string][]string) authority.Scope {
	r := newGrantReach(parent.ID, parent.Scope, children)
	exercised := exercisedBits(childCaps)
	in := authority.Scope{Base: child.Base, ExcludeSelf: child.ExcludeSelf, IDs: child.IDs}
	for _, o := range child.Overrides {
		o.Caps &= exercised
		if o.Caps != protocol.Capability_None {
			in.Overrides = append(in.Overrides, o)
		}
	}
	bounded := r.boundRanks(in, exercised)
	keep := func(ids []string, bits protocol.Capability) []string {
		out := ids[:0:0]
		for _, id := range ids {
			if r.reaches(id, bits) {
				out = append(out, id)
			}
		}
		return out
	}

	var overridden protocol.Capability
	for _, o := range bounded.Overrides {
		overridden |= o.Caps
	}
	out := authority.Scope{
		Base:        bounded.Base,
		IDs:         keep(bounded.IDs, exercised&^overridden),
		ExcludeSelf: child.ExcludeSelf, // only ever narrows; nothing to clamp
		VisIDs:      keep(child.VisIDs, protocol.Capability_None),
	}

	// The visibility rank clamps against the parent's rank. Written explicitly
	// rather than copied, so a child that inherited its rank from its base does
	// not silently acquire a present bit it never had.
	if clamped := authority.MinBase(child.VisRank(), parent.Scope.VisRank()); clamped != out.Base {
		out.VisBasePresent = true
		out.VisBase = clamped
	}

	// Ids are filtered per piece: an id the parent may still reach for some of
	// an entry's bits survives in those pieces and leaves the others.
	for _, o := range bounded.Overrides {
		o.IDs = keep(o.IDs, o.Caps)
		out.Overrides = append(out.Overrides, o)
	}

	// No final clamp against the visibility rank. One used to live here to keep
	// every action set inside what ls shows; that rule is gone, and the two
	// ranks clamp independently against the parent above.
	return out
}
