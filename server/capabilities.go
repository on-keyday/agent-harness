package server

import (
	"encoding/hex"
	"log/slog"
	"time"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// requiredCentral is the bit the central gate checked for kind, for a handler
// that holds only the decoded body and scopes its target by that same bit. A
// kind the central gate does not check has no such bit: asking is a bug.
func requiredCentral(kind protocol.TaskControlKind) protocol.Capability {
	c, ok := authority.CentralCap(kind)
	if !ok {
		panic("requiredCentral: " + kind.String() + " is not gated centrally")
	}
	return c
}

// anyResume is a non-zero resume id: which bits a resume needs does not depend
// on which task it names.
var anyResume = protocol.TaskID{Id: [16]byte{1}}

// capBits is c's set bits, lowest first.
func capBits(c protocol.Capability) []protocol.Capability {
	var out []protocol.Capability
	eachBit(c, func(b protocol.Capability) { out = append(out, b) })
	return out
}

// resumeRequestBits is authority.ResumeCap's resume set as separate bits, for
// resolving the target against each bit's own scope.
func resumeRequestBits(exact bool) []protocol.Capability {
	if exact {
		return capBits(authority.ResumeCap(anyResume, true))
	}
	return capBits(authority.ResumeCap(anyResume, false))
}

// resumeTargetAllowed is the target gate of a resume: allowed when SOME bit the
// caller holds that permits this resume has the target in that bit's own scope.
//
// Unlike attach, which binds by the mode's own bit (authority.AttachModeScopeCap), each
// resume bit is a grant the operator writes separately, and each grant's scope
// is what the operator wrote for it. The cost: narrowing a task that holds both
// bits takes narrowing both scopes, since either admits a target.
func (h *TaskHandler) resumeTargetAllowed(cid objproto.ConnectionID, exact bool, targetHex string) bool {
	held := h.callerCaps(cid)
	for _, resumeBit := range resumeRequestBits(exact) {
		if hasCap(held, resumeBit) && h.inScope(cid, resumeBit, targetHex) {
			return true
		}
	}
	return false
}

// controlSeatAllowed is the scope half of an attached open: the opener takes
// the session's control seat, so the task must be inside its exec_control
// scope, as for an attach --control. resumeHex is "" for a fresh open, whose
// task does not exist yet. The bit half is checked in the dispatch case.
func (h *TaskHandler) controlSeatAllowed(cid objproto.ConnectionID, resumeHex string) bool {
	pid := h.lookupPrincipal(cid)
	if pid.Id == ([16]byte{}) {
		return true // operator: global
	}
	if resumeHex != "" {
		return h.inScope(cid, authority.AttachModeScopeCap(protocol.AttachMode_Control), resumeHex)
	}
	// principalRecord, not the bare record: a credential that is not live, or
	// whose key is no longer the one this connection proved, holds no seat.
	self, ok := h.principalRecord(cid, hex.EncodeToString(pid.Id[:]))
	return ok && authority.CoversNewChild(self.Scope, authority.AttachModeScopeCap(protocol.AttachMode_Control))
}

// resizeAllowed reports whether an observer attach by cid to targetHex may
// resize the session's PTY: the bit, and the target inside the bit's OWN scope.
// It used to read the bit alone, so the attach's scope bounded it and
// `--scope-for exec_resize=…` was accepted and changed nothing; "may drive A
// and B, may resize only A" was not grantable.
func (h *TaskHandler) resizeAllowed(cid objproto.ConnectionID, targetHex string) bool {
	return hasCap(h.callerCaps(cid), authority.ResizeCap) &&
		h.inScope(cid, authority.ResizeCap, targetHex)
}

// hasAnyCap reports whether have includes AT LEAST ONE bit of want. Distinct
// from hasCap (which requires every bit) because authority.AttachModeCap returns
// alternatives that satisfy the same request.
func hasAnyCap(have, want protocol.Capability) bool {
	return have&want != 0
}

// hasCap reports whether have includes every bit in want.
func hasCap(have, want protocol.Capability) bool {
	return have&want == want
}

// callerCaps resolves the connection's principal task and returns its
// capability set. Operator connections (no principal task → zero TaskID) are
// the trusted root and receive the full set. A principal that holds nothing
// (principalRecord) receives none.
func (h *TaskHandler) callerCaps(connID objproto.ConnectionID) protocol.Capability {
	pid := h.lookupPrincipal(connID)
	if pid.Id == ([16]byte{}) {
		return protocol.Capability_All
	}
	t, ok := h.principalRecord(connID, hex.EncodeToString(pid.Id[:]))
	if !ok {
		return protocol.Capability_None
	}
	return t.Capabilities
}

// principalRecord is the task connID's non-operator principal idHex acts
// through, and ok=false when it holds nothing: its record is gone (pruned
// while a connection stayed open), or it is a credential that is not live
// now, or whose stored key is no longer the one this connection proved.
//
// A credential's liveness is read here, per request, and not left to its
// status: an ended TaskEntry keeps its Capabilities and Scope, and a
// credential whose expiry passed is still Active until the deadline watcher
// ends it (credential-tasks D8, anti-goal 3). The key comparison is what makes
// a re-key final for the old key (D18): a rotation leaves the task live, so
// liveness alone would let a connection authenticated with the replaced key
// keep its authority until something closed it. A connection that proved no
// credential key (zero) never matches. Other kinds are not asked — their
// authority is unchanged by this.
func (h *TaskHandler) principalRecord(connID objproto.ConnectionID, idHex string) (TaskEntry, bool) {
	t, ok := h.Tasks.Get(idHex)
	if !ok {
		return TaskEntry{}, false
	}
	if t.Kind == protocol.TaskKind_Credential {
		cur, state, ok := h.Tasks.CredentialLive(idHex, time.Now())
		if !ok || state != CredentialLive || cur != h.lookupCredentialKey(connID) {
			return TaskEntry{}, false
		}
	}
	return t, true
}

// childIndex builds the creatorHex → []childHex map used by the subtree walk
// and by the cascade in set_caps. Both need it regardless of the caller's own
// base, so it is factored out rather than reached through scopeSet.
func (h *TaskHandler) childIndex() map[string][]string {
	return childIndexOf(h.Tasks)
}

// childIndexOf is childIndex over a store, for callers that hold one without
// a handler — the board's reach source.
func childIndexOf(s *TaskStore) map[string][]string {
	allTasks := s.List(0)
	children := make(map[string][]string, len(allTasks))
	for _, t := range allTasks {
		if t.CreatorTaskID.Id != ([16]byte{}) {
			pHex := hex.EncodeToString(t.CreatorTaskID.Id[:])
			children[pHex] = append(children[pHex], t.ID)
		}
	}
	return children
}

// walChildIndex adds the creator links of tasks the live store no longer
// holds. A pruned task's task_created still names its creator, which is the
// only place that edge survives -- and without it a restore could never be
// scope-gated, because the subtree walk would stop at the hole the prune left.
func walChildIndex(base map[string][]string, events []WALEvent) map[string][]string {
	out := make(map[string][]string, len(base))
	for k, v := range base {
		out[k] = append([]string(nil), v...)
	}
	seen := map[string]bool{}
	for k, v := range base {
		_ = k
		for _, c := range v {
			seen[c] = true
		}
	}
	for _, ev := range events {
		if ev.Type != "task_created" || ev.TaskID == "" || ev.CreatorTaskID == "" {
			continue
		}
		if seen[ev.TaskID] {
			continue // the live index already carries this edge
		}
		seen[ev.TaskID] = true
		out[ev.CreatorTaskID] = append(out[ev.CreatorTaskID], ev.TaskID)
	}
	return out
}

// scopeSet resolves the caller's effective TARGET set from its task's stored
// TaskScope:
//   - all=true when the caller is an operator (zero principal) or its base is
//     ScopeBase_Global; allowed is nil in that case.
//   - otherwise allowed = {self} ∪ scope.IDs, plus every descendant when the
//     base is subtree.
//
// It deliberately does NOT consult the VISIBILITY rank (TaskScope.vis_base).
// That rank widens what may be SEEN; folding it in here would make it widen
// what may be DONE, and a caller that could see the whole server while holding
// cancel would be able to kill anything on it. visibleToCaller is the wrapper
// that adds it.
//
// The argument used to be written about info_global, one bit that gated task
// visibility, connection visibility AND the board. That bit is gone: the first
// two became the visibility rank and the third became board_observe. The
// reasoning survives the split unchanged, which is why it is restated rather
// than deleted.
func (h *TaskHandler) scopeSet(connID objproto.ConnectionID, want protocol.Capability) (all bool, allowed map[string]bool) {
	return h.scopeSetWith(connID, want, h.childIndex())
}

// scopeSetWith is scopeSet over a caller-supplied creator index.
//
// It exists for restore, whose targets are ids a prune already removed:
// childIndex is built from the LIVE store, so a forgotten task has no parent
// link there and would fall out of every subtree. Restore merges the WAL's
// creator links in and passes the result here, so the SAME policy decides --
// base, exclude_self, explicit ids, the subtree walk. A second scope
// implementation reading the WAL is the drift shape this file exists to
// prevent.
func (h *TaskHandler) scopeSetWith(connID objproto.ConnectionID, want protocol.Capability, children map[string][]string) (all bool, allowed map[string]bool) {
	pid := h.lookupPrincipal(connID)
	if pid.Id == ([16]byte{}) {
		// Operator: unrestricted.
		return true, nil
	}
	callerHex := hex.EncodeToString(pid.Id[:])

	// A principal that holds nothing reaches nothing, itself included — not
	// the default scope a missing record once fell back to, which handed a
	// connection whose task was pruned its whole former subtree to read.
	// Reads gated by scope alone (ls, conns, task events) go dark with it.
	t, ok := h.principalRecord(connID, callerHex)
	if !ok {
		return false, map[string]bool{}
	}
	scope := t.Scope

	if want == protocol.Capability_None {
		return h.visibilitySet(callerHex, scope)
	}
	return authority.ActionSet(callerHex, scope, want, children)
}

// visibilitySet is the want == Capability_None branch of scopeSet: what the
// caller may SEE.
//
//	{self} u baseSet(visRank) u VisIDs u IDs u every override's IDs
//
// Two things it does that the action branch does not, both deliberate:
//
//   - self is unconditional. ExcludeSelf removes self from an ACTION set only;
//     seeing your own row is orientation, not authority, and a task denied
//     write access to itself still knows it exists.
//   - every action id joins the set without being repeated in VisIDs. An id
//     written into a grant was disclosed by the granter, so hiding it protects
//     nothing — and it is what makes "no capability acts outside what ls shows"
//     hold while an override may still name targets outside the base.
func (h *TaskHandler) visibilitySet(callerHex string, scope authority.Scope) (all bool, allowed map[string]bool) {
	return authority.VisibleSet(callerHex, scope, h.childIndex())
}

// inScope reports whether targetHex is inside the caller's effective target
// set, ignoring capabilities.
//
// It exists so a dispatch case can answer the two failures differently: a
// MISSING CAP is permission_denied (informative — it says nothing about any
// particular task), while an OUT-OF-SCOPE TARGET is the kind's own "no such
// task" (a missing-capability answer about something the caller cannot see is
// an existence oracle).
func (h *TaskHandler) inScope(connID objproto.ConnectionID, want protocol.Capability, targetHex string) bool {
	all, allowed := h.scopeSet(connID, want)
	return all || allowed[targetHex]
}

// authorize is the single gate for every request naming a target task: the
// caller must hold the verb AND the target must be inside its scope.
//
// Callers that fail it answer with the kind's own "no such task" status rather
// than permission_denied — a missing-capability answer about something the
// caller cannot see is an existence oracle.
func (h *TaskHandler) authorize(connID objproto.ConnectionID, want protocol.Capability, targetHex string) bool {
	if !hasCap(h.callerCaps(connID), want) {
		return false
	}
	return h.inScope(connID, want, targetHex)
}

// granter is the connection's principal as the task that will write a grant:
// nil for an operator, which is unbounded in what it grants as in what it
// holds. A principal that holds nothing (principalRecord: its record is gone,
// or a credential that is not live) keeps callerCaps but reaches and
// delegates nothing.
func (h *TaskHandler) granter(cid objproto.ConnectionID, callerCaps protocol.Capability) *authority.Granter {
	pid := h.lookupPrincipal(cid)
	if pid.Id == ([16]byte{}) {
		return nil
	}
	hexID := hex.EncodeToString(pid.Id[:])
	held := authority.Grant{Caps: callerCaps, Scope: authority.Scope{Base: protocol.ScopeBase_None},
		DelegateScope: authority.Scope{Base: protocol.ScopeBase_None}}
	if t, ok := h.principalRecord(cid, hexID); ok {
		held = t.Grant()
		held.Caps = callerCaps
	}
	return authority.NewGranter(hexID, held, h.childIndex())
}

// resumedCaps is the capability mask a resumed task will hold: re-granted by
// the resumer when the request overrides it, the record's otherwise — which may
// exceed the resumer's own (the resume bit's documented behaviour).
func (h *TaskHandler) resumedCaps(idHex string, override bool, g *authority.Granter, requested protocol.Capability) protocol.Capability {
	if override {
		return g.ChildCaps(requested)
	}
	rec, _ := h.Tasks.Get(idHex)
	return rec.Capabilities
}

// visibleToCaller is the INFO scope: the action set widened by
// Capability_BoardObserve. ls, the port-forward list and the conns
// filter call it exactly as before and inherit the scope with no change of
// their own.
func (h *TaskHandler) visibleToCaller(connID objproto.ConnectionID) (all bool, allowed map[string]bool) {
	if h.lookupPrincipal(connID).Id == ([16]byte{}) {
		return true, nil
	}
	return h.scopeSet(connID, protocol.Capability_None)
}

// listVisibleToCaller is the LIST scope: visibleToCaller (the ACCESS scope)
// plus the caller's direct creator, returned separately as parentHex.
//
// The two scopes differ on purpose. visibleToCaller answers "is this task in
// my scope" and never reaches upward on its own — every op gated by it (conn
// snapshots, port-forward list/kill) would otherwise open onto a
// task that, by attenuation, holds a superset of the caller's caps.
// listVisibleToCaller answers the narrower "does this task exist and is it
// alive", which a child legitimately needs about the parent that is driving it
// — and whose id it can already read from the ungated whoami CreatorTaskId.
//
// The hop is justified by the creator relationship, not by the target set, so
// it is unconditional under every ScopeBase: a scope=none task still sees one
// redacted parent row. It must never feed authorize. The one way the parent
// becomes actionable is an explicit scope id naming it, which is inside the
// granter's own reach — and then the parent is already in allowed, the early
// return below fires, and the row is shown unredacted.
//
// Exactly ONE hop: grandparents and siblings stay invisible. parentHex is ""
// for operators, callers whose visibility rank is global (all=true covers
// everything anyway),
// operator-created tasks (zero creator), and a creator no longer in the store.
// The row itself is redacted by the caller — see redactParentTaskInfo.
func (h *TaskHandler) listVisibleToCaller(connID objproto.ConnectionID) (all bool, allowed map[string]bool, parentHex string) {
	all, allowed = h.visibleToCaller(connID)
	if all {
		return true, nil, ""
	}
	pid := h.lookupPrincipal(connID)
	self, ok := h.principalRecord(connID, hex.EncodeToString(pid.Id[:]))
	if !ok || self.CreatorTaskID.Id == ([16]byte{}) {
		return false, allowed, ""
	}
	creatorHex := hex.EncodeToString(self.CreatorTaskID.Id[:])
	if allowed[creatorHex] {
		// Already inside the subtree (only reachable through a creator cycle,
		// which task creation cannot produce); nothing to add.
		return false, allowed, ""
	}
	return false, allowed, creatorHex
}

// agentCallerCaps resolves the capability set of the agentboard caller
// identified by ac.state.Identity(). Returns Capability_None if the task
// is not found or the connection state is nil (not yet helloed). The task is
// read through principalRecord on ac's connection, so a credential that is not
// live, or whose key is no longer the one that connection proved, holds none
// here either.
func (s *Server) agentCallerCaps(ac *agentConn) protocol.Capability {
	if ac == nil || ac.state == nil {
		return protocol.Capability_None
	}
	_, tid, _, _ := ac.state.Identity()
	if tid.Id == ([16]byte{}) {
		// Zero TaskID means no authenticated identity → no caps.
		return protocol.Capability_None
	}
	tidHex := hex.EncodeToString(tid.Id[:])
	t, ok := s.taskHandler.principalRecord(ac.cid, tidHex)
	if !ok {
		slog.Warn("agentCallerCaps: no live principal record", "task_id", tidHex)
		return protocol.Capability_None
	}
	return t.Capabilities
}
