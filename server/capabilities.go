package server

import (
	"encoding/hex"
	"log/slog"

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
// AttachSession is absent because its cap depends on the requested AttachMode,
// which this map cannot see — the gate is inline in the dispatch case, via
// attachModeCap. AwaitIdle is absent because it needs NO capability: it reports
// last_output_at, which `ls` hands to any caller that can see the task, so a
// gate here would only make the edge-triggered path cost more authority than
// polling for the same fact. Its one side effect, sink=notify, is gated on
// `notify` inside the handler.
var requiredCap = map[protocol.TaskControlKind]protocol.Capability{
	protocol.TaskControlKind_Submit:           protocol.Capability_Spawn,
	protocol.TaskControlKind_OpenInteractive:  protocol.Capability_Spawn,
	protocol.TaskControlKind_Cancel:           protocol.Capability_Cancel,
	protocol.TaskControlKind_PruneTasks:       protocol.Capability_Prune,
	protocol.TaskControlKind_Notify:           protocol.Capability_Notify,
	protocol.TaskControlKind_DialRunner:       protocol.Capability_RunnerAdmin,
	protocol.TaskControlKind_BoardTopics:      protocol.Capability_BoardObserve,
	protocol.TaskControlKind_BoardRead:        protocol.Capability_BoardObserve,
	protocol.TaskControlKind_BoardPurge:       protocol.Capability_Purge,
	protocol.TaskControlKind_BoardRetract:     protocol.Capability_Purge,
	protocol.TaskControlKind_BoardSubscribers: protocol.Capability_BoardObserve,
	protocol.TaskControlKind_GitQuery:         protocol.Capability_FileRead,
	protocol.TaskControlKind_OpenExecRun:      protocol.Capability_ExecRun,
	// OpenForwardTap sits in this map, and NOT inline like KillPortForward's,
	// because it is direction-INDEPENDENT: reading the payload of a -R forward
	// is the same power as reading a -L one, so the bit is known without the
	// registry lookup. The visibility and scope halves still happen in the
	// handler, which is where the target becomes known.
	protocol.TaskControlKind_OpenForwardTap: protocol.Capability_ForwardTap,
}

// attachModeCap returns the capability set that satisfies an attach in the
// given mode. The three attach powers are RANKED, not orthogonal, so each mode
// accepts its own bit or any stronger one:
//
//	view    <- exec_view | exec_cowrite | exec_control
//	cowrite <- exec_cowrite | exec_control
//	control <- exec_control
//
// A holder of the stronger power can always choose to exercise the weaker one;
// requiring it to also hold the weaker bit would be bookkeeping, not
// confinement. The returned value is therefore checked with hasAnyCap, not
// hasCap — it is a set of alternatives, not a set of requirements.
//
// An unrecognised mode from a newer peer falls through to exec_control, the
// strictest answer, so an unknown mode fails closed.
func attachModeCap(mode protocol.AttachMode) protocol.Capability {
	switch mode {
	case protocol.AttachMode_View:
		return protocol.Capability_ExecView | protocol.Capability_ExecCowrite | protocol.Capability_ExecControl
	case protocol.AttachMode_Cowrite:
		return protocol.Capability_ExecCowrite | protocol.Capability_ExecControl
	default:
		return protocol.Capability_ExecControl
	}
}

// hasAnyCap reports whether have includes AT LEAST ONE bit of want. Distinct
// from hasCap (which requires every bit) because attachModeCap returns
// alternatives that satisfy the same request.
func hasAnyCap(have, want protocol.Capability) bool {
	return have&want != 0
}

// hasCap reports whether have includes every bit in want.
func hasCap(have, want protocol.Capability) bool {
	return have&want == want
}

// intersectCaps is spawn-time attenuation: a child receives the bits its
// creator holds AND requested. Monotonically non-increasing.
func intersectCaps(creator, requested protocol.Capability) protocol.Capability {
	return creator & requested
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
	allTasks := h.Tasks.List(0)
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

// descendantsOf marks self and every task reachable through the creator index
// into allowed.
//
// visited is deliberately separate from allowed. allowed arrives pre-seeded
// with self and the scope's explicit ids, so reusing it as the visited set
// would make the walk skip its own root on the first pop and return without
// ever expanding a single child.
func descendantsOf(children map[string][]string, selfHex string, allowed map[string]bool) {
	visited := make(map[string]bool, len(children)+1)
	queue := []string{selfHex}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if visited[cur] {
			continue
		}
		visited[cur] = true
		allowed[cur] = true
		queue = append(queue, children[cur]...)
	}
}

// scopeSet resolves the caller's effective TARGET set from its task's stored
// TaskScope:
//   - all=true when the caller is an operator (zero principal) or its base is
//     ScopeBase_Global; allowed is nil in that case.
//   - otherwise allowed = {self} ∪ scope.IDs, plus every descendant when the
//     base is subtree.
//
// It deliberately does NOT consult Capability_BoardObserve. That bit widens what
// may be SEEN; folding it in here would make it widen what may be DONE, and a
// caller holding info_global and cancel would be able to kill anything on the
// server. visibleToCaller is the wrapper that adds it.
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

	scope := defaultScope()
	if t, ok := h.Tasks.Get(callerHex); ok {
		scope = t.Scope
	}

	if want == protocol.Capability_None {
		return h.visibilitySet(callerHex, scope)
	}

	base, excludeSelf, ids := scope.ForCap(want)
	if base == protocol.ScopeBase_Global {
		return true, nil
	}
	allowed = map[string]bool{}
	if !excludeSelf {
		allowed[callerHex] = true
	}
	for _, id := range ids {
		allowed[id] = true
	}
	if base == protocol.ScopeBase_Subtree {
		// descendantsOf seeds from callerHex and marks it, so exclude_self has
		// to be applied AFTER the walk. Deleting first would make the walk skip
		// its own root and return without expanding a single child — the same
		// trap the visited/allowed split in descendantsOf documents.
		descendantsOf(children, callerHex, allowed)
		if excludeSelf {
			delete(allowed, callerHex)
		}
	}
	return false, allowed
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
func (h *TaskHandler) visibilitySet(callerHex string, scope Scope) (all bool, allowed map[string]bool) {
	visRank := scope.VisRank()
	if visRank == protocol.ScopeBase_Global {
		return true, nil
	}
	allowed = map[string]bool{callerHex: true}
	for _, id := range scope.VisIDs {
		allowed[id] = true
	}
	for _, id := range scope.IDs {
		allowed[id] = true
	}
	for _, o := range scope.Overrides {
		for _, id := range o.IDs {
			allowed[id] = true
		}
	}
	if visRank == protocol.ScopeBase_Subtree {
		descendantsOf(h.childIndex(), callerHex, allowed)
	}
	return false, allowed
}

// attachModeScopeCap is the SINGLE bit whose scope governs an attach in the
// given mode. Distinct from attachModeCap, which returns the set of bits that
// SATISFY the request: a holder of exec_control attaching in view mode is
// exercising the view power, so the view scope is the one that binds it.
//
// An unrecognised mode falls through to exec_control, matching attachModeCap's
// fail-closed default.
func attachModeScopeCap(mode protocol.AttachMode) protocol.Capability {
	switch mode {
	case protocol.AttachMode_View:
		return protocol.Capability_ExecView
	case protocol.AttachMode_Cowrite:
		return protocol.Capability_ExecCowrite
	default:
		return protocol.Capability_ExecControl
	}
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

// attenuateScope clamps a requested scope to the creator's effective one, so a
// spawned task can never reach further than its spawner.
//
// Both axes are clamped independently:
//   - base by permissiveness (see minScopeBase), which is why a subtree parent
//     asking for a global child yields subtree. The child's own subtree is a
//     subset of the parent's, so nothing is gained.
//   - ids by membership in the creator's set AT SPAWN TIME. This is a static,
//     one-shot check: the ids are literal, so a later narrowing of the parent
//     does not retroactively invalidate the child (that is what set_caps
//     --cascade is for).
//
// A rejected id is returned as `offender` with ok=false rather than being
// dropped. Silent clamping would produce a task whose scope differs from the
// one the caller wrote with nothing saying so — the same invisible-divergence
// failure the presence bits on set_caps exist to avoid.
//
// An operator creator (all=true) may request anything, including ids for tasks
// it has never touched.
func (h *TaskHandler) attenuateScope(creatorCID string, req Scope) (out Scope, offender string, ok bool) {
	out = Scope{
		Base:           req.Base,
		VisBase:        req.VisBase,
		VisBasePresent: req.VisBasePresent,
		ExcludeSelf:    req.ExcludeSelf,
		IDs:            normalizeScopeIDs(req.IDs),
		VisIDs:         normalizeScopeIDs(req.VisIDs),
	}
	for _, o := range req.Overrides {
		out.Overrides = append(out.Overrides, ScopeOverride{
			Caps: o.Caps, Base: o.Base, ExcludeSelf: o.ExcludeSelf, IDs: normalizeScopeIDs(o.IDs),
		})
	}

	// Consistency first, before anything is measured against the parent: an
	// override mask that is empty or overlaps another describes nothing
	// coherent, so it is rejected as written rather than clamped into
	// something the caller never asked for.
	//
	// There is no second pass after clamping any more. One existed to catch an
	// override left stranded above a lowered visibility rank; ranks are
	// independent now, so nothing clamping does can make a consistent value
	// inconsistent.
	if err := validateScope(out); err != nil {
		// Prefixed so the caller can tell a consistency failure from an
		// out-of-reach id without parsing: both arrive through `offender`, and
		// the submit path used to wrap every one of them in "scope id … is
		// outside your own scope", which turned a sentence into nonsense.
		return Scope{}, "invalid scope: " + err.Error(), false
	}

	visAll, visAllowed := h.scopeSet(creatorCID, protocol.Capability_None)
	if visAll {
		// Operator, or a creator with global visibility: anything it wrote is
		// inside its own reach by definition.
		return out, "", true
	}

	// Rank clamping, per axis and per capability. A child asking for more than
	// its parent holds is lowered rather than refused -- the historical
	// behaviour for the base, extended to the axes that did not exist then.
	parentBase := h.callerScopeBase(creatorCID)
	out.Base = minScopeBase(out.Base, parentBase)
	if out.VisBasePresent {
		out.VisBase = minScopeBase(out.VisBase, parentBase)
	}
	for i := range out.Overrides {
		out.Overrides[i].Base = minScopeBase(out.Overrides[i].Base, parentBase)
	}

	// Ids are CHECKED, never clamped: a silently dropped target produces a task
	// whose scope differs from the one the caller wrote with nothing saying so.
	// Each override's ids are measured against the parent's set FOR THAT
	// CAPABILITY, so an id the parent may read but not cancel is refused on the
	// cancel override alone.
	for _, o := range out.Overrides {
		_, parentAllowed := h.scopeSet(creatorCID, o.Caps)
		for _, id := range o.IDs {
			if !parentAllowed[id] {
				return Scope{}, id, false
			}
		}
	}
	for _, id := range out.IDs {
		if !visAllowed[id] {
			return Scope{}, id, false
		}
	}
	for _, id := range out.VisIDs {
		if !visAllowed[id] {
			return Scope{}, id, false
		}
	}

	return out, "", true
}

// callerScopeBase is the stored base of the connection's principal task, or
// global for an operator.
func (h *TaskHandler) callerScopeBase(connID string) protocol.ScopeBase {
	pid := h.lookupPrincipal(connID)
	if pid.Id == ([16]byte{}) {
		return protocol.ScopeBase_Global
	}
	t, ok := h.Tasks.Get(hex.EncodeToString(pid.Id[:]))
	if !ok {
		return protocol.ScopeBase_None
	}
	return t.Scope.Base
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
