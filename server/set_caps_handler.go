package server

import (
	"encoding/hex"
	"log/slog"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// handleSetCaps rewrites a live task's authority.
//
// The operator (a caller with NO principal task) may write anything. A bit
// authorising "rewrite capabilities" freely would be self-amplifying: whoever
// held it would grant itself all on first use. Operator identity is the only
// ungrantable predicate the server has, established at the PSK gate
// (server/psk.go) where a non-agent client must prove operatorPSK.
//
// A TASK caller gets the bounded re-grant a re-granting resume already gives it
// (docs/superpowers/specs/2026-11-22-delegated-regrant-and-rename-design.md):
// only on a strict descendant inside its resume scope (regrantReaches, R1), and
// the update is grantFor's with the caller as granter (R2) — its own caps and
// delegation bound bound every half, so nothing here lets a task raise itself
// or anyone outside its subtree. Every refusal of R1 is not_found, so the
// answer is no existence oracle (R4).
func (h *TaskHandler) handleSetCaps(conn ConnHandle, requestID uint32, cid string, req *protocol.SetCapsRequest) {
	respondMsg := func(status protocol.SetCapsStatus, affected []string, connsClosed uint32, msg string) {
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_SetCaps, RequestId: requestID}
		body := protocol.SetCapsResponse{Status: status, ConnsClosed: connsClosed}
		body.SetMessage([]byte(msg))
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
	respond := func(status protocol.SetCapsStatus, affected []string, connsClosed uint32) {
		respondMsg(status, affected, connsClosed, "")
	}

	caller := h.lookupPrincipal(cid)
	operator := caller.Id == ([16]byte{})
	targetHex := hex.EncodeToString(req.TaskId.Id[:])
	before, ok := h.Tasks.Get(targetHex)
	if !ok || (!operator && !h.regrantReaches(cid, targetHex)) {
		respond(protocol.SetCapsStatus_NotFound, nil, 0)
		return
	}
	// A credential can never hold credential_issue or schedule_edit (D14).
	// Naming one is refused rather than left to the store's mask, which would
	// answer ok to a grant that did not take effect.
	if before.Kind == protocol.TaskKind_Credential {
		var named protocol.Capability
		if req.CapsPresent() {
			named |= req.Caps
		}
		if req.DelegateCapsPresent() {
			named |= req.DelegateCaps
		}
		if named&CredentialMaskedCaps != 0 {
			slog.Warn("set_caps denied: a masked bit on a credential", "cid", cid, "task", targetHex,
				"masked", authority.CapsLabel(named&CredentialMaskedCaps))
			respond(protocol.SetCapsStatus_MaskedCap, nil, 0)
			return
		}
	}

	by := AuthorityWrite{Source: protocol.AuthoritySource_OperatorCapsSet}
	var u GrantUpdate
	if operator {
		var refused bool
		if u, refused = h.operatorCapsUpdate(cid, req, respond); refused {
			return
		}
	} else {
		var err error
		u, err = h.grantFor(h.granter(cid, h.callerCaps(cid)), setCapsGrantRequest(req), targetHex)
		if err != nil {
			slog.Warn("set_caps denied: re-grant outside the caller's bound", "cid", cid, "task", targetHex, "err", err)
			respondMsg(protocol.SetCapsStatus_InvalidScope, nil, 0, err.Error())
			return
		}
		by = AuthorityWrite{Source: protocol.AuthoritySource_TaskCapsSet, Actor: caller}
	}

	after, ok := h.Tasks.SetCaps(targetHex, u, by)

	if !ok {
		respond(protocol.SetCapsStatus_NotFound, nil, 0)
		return
	}
	h.finishSetCaps(conn, req, before, after, u, respond)
}

// operatorCapsUpdate builds the operator's update: every present half written
// as given, after validation only (no attenuation — the operator may widen
// past its own reach). refused reports that a response was already sent.
func (h *TaskHandler) operatorCapsUpdate(cid string, req *protocol.SetCapsRequest,
	respond func(protocol.SetCapsStatus, []string, uint32)) (GrantUpdate, bool) {
	// The spawn limits are written on the target only: the cascade below
	// never sets their bits, so no descendant's limits change (spec D13).
	u := GrantUpdate{
		Caps: req.CapsPresent(), Scope: req.ScopePresent(),
		DelegateCaps: req.DelegateCapsPresent(), DelegateScope: req.DelegateScopePresent(),
		SpawnDepth: req.SpawnDepthPresent(), MaxDescendants: req.MaxDescendantsPresent(),
		To: authority.Grant{Caps: req.Caps, DelegateCaps: req.DelegateCaps,
			SpawnDepth: req.SpawnDepth, MaxDescendants: req.MaxDescendants},
	}
	if req.ScopePresent() {
		newScope := authority.ScopeFromWire(req.Scope, req.Overrides)
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
			return GrantUpdate{}, true
		}
		u.To.Scope = newScope
	}
	// The bound is validated like any scope and otherwise written as given:
	// the operator may grant any bound. It carries no overrides on the wire.
	if req.DelegateScopePresent() {
		bound := authority.ScopeFromWire(req.DelegateScope, nil)
		if err := authority.Validate(bound); err != nil {
			slog.Warn("set_caps denied: inconsistent delegate scope", "cid", cid, "err", err)
			respond(protocol.SetCapsStatus_InvalidScope, nil, 0)
			return GrantUpdate{}, true
		}
		u.To.DelegateScope = bound
	}
	return u, false
}

// setCapsGrantRequest reads a set_caps request as the re-grant request a
// resume carries: each half behind its own presence bit.
func setCapsGrantRequest(req *protocol.SetCapsRequest) grantRequest {
	return grantRequest{
		Caps: req.Caps, Scope: authority.ScopeFromWire(req.Scope, req.Overrides),
		DelegateCaps: req.DelegateCaps, DelegateScope: authority.ScopeFromWire(req.DelegateScope, nil),
		CapsPresent: req.CapsPresent(), ScopePresent: req.ScopePresent(),
		DelegateCapsPresent: req.DelegateCapsPresent(), DelegateScopePresent: req.DelegateScopePresent(),
		SpawnDepth: req.SpawnDepth, MaxDescendants: req.MaxDescendants,
		SpawnDepthPresent: req.SpawnDepthPresent(), MaxDescendantsPresent: req.MaxDescendantsPresent(),
	}
}

// regrantReaches is R1: a task may re-grant (or rename) only a STRICT
// descendant — never itself, an ancestor or a task outside its subtree, which
// would let it act through that task's tree position (credential D18a) — and
// only one inside the scope of its resume bit, the power a re-granting resume
// already exercises. Descendants are read from the live creator index.
func (h *TaskHandler) regrantReaches(cid, targetHex string) bool {
	callerHex := h.principalHex(cid)
	if callerHex == "" || callerHex == targetHex || !h.resumeTargetAllowed(cid, false, targetHex) {
		return false
	}
	descendants := map[string]bool{}
	authority.DescendantsOf(h.childIndex(), callerHex, descendants)
	return descendants[targetHex]
}

// finishSetCaps runs everything after the target's write, for either caller:
// the cascade, the narrowing teardown and the answer.
func (h *TaskHandler) finishSetCaps(conn ConnHandle, req *protocol.SetCapsRequest, before, after TaskEntry, u GrantUpdate,
	respond func(protocol.SetCapsStatus, []string, uint32)) {
	targetHex := after.ID
	affected := []string{targetHex}
	// Over caps and scope only: the bound is exercised over no connection, so
	// a change to it alone tears nothing down.
	narrowed := authority.IsNarrowing(before.Capabilities, before.Scope, after.Capabilities, after.Scope)

	if req.Cascade() {
		// Attenuation happens at spawn, so a task already spawned keeps what it
		// was given: revoking a parent without cascading leaves it able to act
		// through a child it created while still wide. The cascade re-imposes
		// the monotonic invariant over the whole subtree.
		children := h.childIndex()
		// Only a call that writes the bound clamps descendants to it. Every
		// task recorded before the bound replays with the empty one, so
		// clamping on every cascade would turn the first ordinary
		// `caps set <supervisor> --caps … --cascade` into caps=none for all its
		// workers.
		boundWritten := u.DelegateCaps || u.DelegateScope
		target := authority.Task{ID: after.ID, Caps: after.Capabilities, Scope: after.Scope,
			DelegateCaps: after.DelegateCaps, DelegateScope: after.DelegateScope}
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
			cu := GrantUpdate{Caps: true, Scope: true, DelegateCaps: boundWritten, DelegateScope: boundWritten,
				To: authority.CascadeClamp(target, cBefore.Grant(), childHex, boundWritten, children)}
			cAfter, ok := h.Tasks.SetCaps(childHex, cu, AuthorityWrite{Source: protocol.AuthoritySource_Cascade, Actor: req.TaskId})

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
