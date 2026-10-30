package server

import (
	"encoding/hex"
	"log/slog"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// requiredCap maps a direction-independent TaskControlKind to the cap it needs.
// Kinds absent from the map are gated elsewhere: OpenFileTransfer / ListFiles are
// direction-dependent, RegisterPortForward is direction-dependent (its gate
// moved off OpenPortForward, which is now unconditionally ForwardLocal); List /
// ListPortForwards is INFO-scoped (visibleToCaller, not a single cap);
// GetTaskLog was too until 2026-08-21 and is now an ordinary targeted op under
// Capability_ExecView — a task log is the agent's output recorded, which is
// what that bit gates live; KillPortForward is direction-dependent AND the direction is only known
// after the registry lookup, so its gate lives inline in the dispatch case,
// after h.pforwards().get.
//
// ExecRunList is absent because it is INFO-scoped like ListPortForwards, and
// ExecRunKill because its target task is only known after the registry
// lookup, so its gate lives inline in the handler as KillPortForward's does.
//
// GitQuery sits in this map rather than beside the direction-dependent file
// ops because it is read-only by construction — there is no write direction to
// discriminate. It is also the one task-scoped kind that does NOT require
// Running/Detached; see handleGitQuery for why.
//
// Submit and OpenInteractive are absent because their bit depends on the
// request: a zero resume_task_id creates a task (spawn), a non-zero one resumes
// (resume, or resume_exact when the exact flag is set). The gate is inline in
// both dispatch cases, via resumeRequestCap.
//
// AttachSession is absent because its cap depends on the requested AttachMode,
// which this map cannot see — the gate is inline in the dispatch case, via
// authority.AttachModeCap. AwaitIdle is absent because it needs NO capability: it reports
// last_output_at, which `ls` hands to any caller that can see the task, so a
// gate here would only make the edge-triggered path cost more authority than
// polling for the same fact. Its one side effect, sink=notify, is gated on
// `notify` inside the handler.
var requiredCap = map[protocol.TaskControlKind]protocol.Capability{
	protocol.TaskControlKind_Cancel:     protocol.Capability_Cancel,
	protocol.TaskControlKind_PruneTasks: protocol.Capability_Prune,
	protocol.TaskControlKind_Notify:     protocol.Capability_Notify,
	// Emptying another task's progress line: whoever may end the task may
	// empty its status line, which is the weaker act. Target-gated on
	// cancel's scope in handleProgressClear.
	protocol.TaskControlKind_ProgressClear: protocol.Capability_Cancel,
	protocol.TaskControlKind_DialRunner:    protocol.Capability_RunnerAdmin,
	// Which keys may register as runners: enrolling, listing and removing
	// them. Not runner_admin, which is the server's network reach.
	protocol.TaskControlKind_RunnerEnroll:   protocol.Capability_RunnerEnroll,
	protocol.TaskControlKind_RunnerUnenroll: protocol.Capability_RunnerEnroll,
	protocol.TaskControlKind_RunnerKeys:     protocol.Capability_RunnerEnroll,
	protocol.TaskControlKind_BoardTopics:    protocol.Capability_BoardObserve,
	protocol.TaskControlKind_BoardRead:      protocol.Capability_BoardObserve,
	protocol.TaskControlKind_BoardPurge:     protocol.Capability_BoardPurge,
	// A task caller is also checked against the board reach in the handler:
	// it may retain only a topic it may publish to.
	protocol.TaskControlKind_BoardRetain:  protocol.Capability_BoardPersist,
	protocol.TaskControlKind_BoardRetract: protocol.Capability_BoardPurge,
	// Forgetting a task's contacts removes another task's board state, the
	// same class as withdrawing its message; the handler adds the scope gate.
	protocol.TaskControlKind_BoardContactsClear: protocol.Capability_BoardPurge,
	// Subscribing another task, or making it a member of a topic; the handler
	// adds the scope gate and the reach bounds.
	protocol.TaskControlKind_BoardMembership:  protocol.Capability_BoardMember,
	protocol.TaskControlKind_BoardSubscribers: protocol.Capability_BoardObserve,
	protocol.TaskControlKind_BoardSend:        protocol.Capability_BoardSend,
	protocol.TaskControlKind_BoardWake:        protocol.Capability_BoardSend,
	protocol.TaskControlKind_GitQuery:         protocol.Capability_FileRead,
	protocol.TaskControlKind_OpenExecRun:      protocol.Capability_ExecRun,
	// OpenForwardTap sits in this map, and NOT inline like KillPortForward's,
	// because it is direction-INDEPENDENT: reading the payload of a -R forward
	// is the same power as reading a -L one, so the bit is known without the
	// registry lookup. The visibility and scope halves still happen in the
	// handler, which is where the target becomes known.
	protocol.TaskControlKind_OpenForwardTap: protocol.Capability_ForwardTap,
	// OpenExecTap: direction-independent like OpenForwardTap — reading an
	// exec's stdin is the same power as reading its stdout — so the bit is
	// known before the registry lookup. Visibility and scope are in the handler.
	protocol.TaskControlKind_OpenExecTap: protocol.Capability_ExecTap,
}

// resumeRequestCap returns the capability set that satisfies a submit or an
// interactive open. Creating a task (zero resumeID) is spawn alone. Resuming is
// the resume rank, which spawn does NOT imply: resume ⊇ resume_exact, checked
// with hasAnyCap like authority.AttachModeCap, so a holder of resume may also resume
// exactly.
func resumeRequestCap(resumeID protocol.TaskID, exact bool) protocol.Capability {
	switch {
	case resumeID.Id == ([16]byte{}):
		return protocol.Capability_Spawn
	case exact:
		return protocol.Capability_Resume | protocol.Capability_ResumeExact
	default:
		return protocol.Capability_Resume
	}
}

// resumeRequestBits is resumeRequestCap's resume set as separate bits, for
// resolving the target against each bit's own scope.
func resumeRequestBits(exact bool) []protocol.Capability {
	if exact {
		return []protocol.Capability{protocol.Capability_Resume, protocol.Capability_ResumeExact}
	}
	return []protocol.Capability{protocol.Capability_Resume}
}

// resumeTargetAllowed is the target gate of a resume: allowed when SOME bit the
// caller holds that permits this resume has the target in that bit's own scope.
//
// Unlike attach, which binds by the mode's own bit (authority.AttachModeScopeCap), each
// resume bit is a grant the operator writes separately, and each grant's scope
// is what the operator wrote for it. The cost: narrowing a task that holds both
// bits takes narrowing both scopes, since either admits a target.
func (h *TaskHandler) resumeTargetAllowed(cid string, exact bool, targetHex string) bool {
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
func (h *TaskHandler) controlSeatAllowed(cid, resumeHex string) bool {
	pid := h.lookupPrincipal(cid)
	if pid.Id == ([16]byte{}) {
		return true // operator: global
	}
	if resumeHex != "" {
		return h.inScope(cid, authority.AttachModeScopeCap(protocol.AttachMode_Control), resumeHex)
	}
	self, ok := h.Tasks.Get(hex.EncodeToString(pid.Id[:]))
	return ok && authority.CoversNewChild(self.Scope, protocol.Capability_ExecControl)
}

// resizeAllowed reports whether an observer attach by cid to targetHex may
// resize the session's PTY: the bit, and the target inside the bit's OWN scope.
// It used to read the bit alone, so the attach's scope bounded it and
// `--scope-for exec_resize=…` was accepted and changed nothing; "may drive A
// and B, may resize only A" was not grantable.
func (h *TaskHandler) resizeAllowed(cid, targetHex string) bool {
	return hasCap(h.callerCaps(cid), protocol.Capability_ExecResize) &&
		h.inScope(cid, protocol.Capability_ExecResize, targetHex)
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
// the trusted root and receive the full set.
func (h *TaskHandler) callerCaps(connID string) protocol.Capability {
	pid := h.lookupPrincipal(connID)
	if pid.Id == ([16]byte{}) {
		return protocol.Capability_All
	}
	t, ok := h.Tasks.Get(hex.EncodeToString(pid.Id[:]))
	if !ok {
		return protocol.Capability_None
	}
	return t.Capabilities
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
func (h *TaskHandler) scopeSet(connID string, want protocol.Capability) (all bool, allowed map[string]bool) {
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
func (h *TaskHandler) scopeSetWith(connID string, want protocol.Capability, children map[string][]string) (all bool, allowed map[string]bool) {
	pid := h.lookupPrincipal(connID)
	if pid.Id == ([16]byte{}) {
		// Operator: unrestricted.
		return true, nil
	}
	callerHex := hex.EncodeToString(pid.Id[:])

	scope := authority.DefaultScope()
	if t, ok := h.Tasks.Get(callerHex); ok {
		scope = t.Scope
	}

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
func (h *TaskHandler) inScope(connID string, want protocol.Capability, targetHex string) bool {
	all, allowed := h.scopeSet(connID, want)
	return all || allowed[targetHex]
}

// authorize is the single gate for every request naming a target task: the
// caller must hold the verb AND the target must be inside its scope.
//
// Callers that fail it answer with the kind's own "no such task" status rather
// than permission_denied — a missing-capability answer about something the
// caller cannot see is an existence oracle.
func (h *TaskHandler) authorize(connID string, want protocol.Capability, targetHex string) bool {
	if !hasCap(h.callerCaps(connID), want) {
		return false
	}
	return h.inScope(connID, want, targetHex)
}

// granter is the connection's principal as the task that will write a grant:
// nil for an operator, which is unbounded in what it grants as in what it
// holds. A principal whose record is gone keeps callerCaps but reaches and
// delegates nothing.
func (h *TaskHandler) granter(cid string, callerCaps protocol.Capability) *authority.Granter {
	pid := h.lookupPrincipal(cid)
	if pid.Id == ([16]byte{}) {
		return nil
	}
	hexID := hex.EncodeToString(pid.Id[:])
	held := authority.Grant{Caps: callerCaps, Scope: authority.Scope{Base: protocol.ScopeBase_None},
		DelegateScope: authority.Scope{Base: protocol.ScopeBase_None}}
	if t, ok := h.Tasks.Get(hexID); ok {
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
// Capability_BoardObserve. ls, task logs, the port-forward list and the conns
// filter call it exactly as before and inherit the scope with no change of
// their own.
func (h *TaskHandler) visibleToCaller(connID string) (all bool, allowed map[string]bool) {
	if h.lookupPrincipal(connID).Id == ([16]byte{}) {
		return true, nil
	}
	return h.scopeSet(connID, protocol.Capability_None)
}

// listVisibleToCaller is the LIST scope: visibleToCaller (the ACCESS scope)
// plus the caller's direct creator, returned separately as parentHex.
//
// The two scopes differ on purpose. visibleToCaller answers "is this task in
// my scope" and never reaches upward on its own — every op gated by it (task
// logs, conn snapshots, port-forward list/kill) would otherwise open onto a
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
func (h *TaskHandler) listVisibleToCaller(connID string) (all bool, allowed map[string]bool, parentHex string) {
	all, allowed = h.visibleToCaller(connID)
	if all {
		return true, nil, ""
	}
	pid := h.lookupPrincipal(connID)
	self, ok := h.Tasks.Get(hex.EncodeToString(pid.Id[:]))
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
// is not found or the connection state is nil (not yet helloed).
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
	t, ok := s.tasks.Get(tidHex)
	if !ok {
		slog.Warn("agentCallerCaps: task not found", "task_id", tidHex)
		return protocol.Capability_None
	}
	return t.Capabilities
}
