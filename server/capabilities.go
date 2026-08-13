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
// GetTaskLog / ListPortForwards are INFO-scoped (visibleToCaller, not a single
// cap); KillPortForward is direction-dependent AND the direction is only known
// after the registry lookup, so its gate lives inline in the dispatch case,
// after h.pforwards().get.
//
// GitQuery sits in this map rather than beside the direction-dependent file
// ops because it is read-only by construction — there is no write direction to
// discriminate. It is also the one task-scoped kind that does NOT require
// Running/Detached; see handleGitQuery for why.
var requiredCap = map[protocol.TaskControlKind]protocol.Capability{
	protocol.TaskControlKind_Submit:           protocol.Capability_Spawn,
	protocol.TaskControlKind_OpenInteractive:  protocol.Capability_Spawn,
	protocol.TaskControlKind_Cancel:           protocol.Capability_Cancel,
	protocol.TaskControlKind_PruneTasks:       protocol.Capability_Prune,
	protocol.TaskControlKind_Notify:           protocol.Capability_Notify,
	protocol.TaskControlKind_AttachSession:    protocol.Capability_ExecAttach,
	protocol.TaskControlKind_AwaitIdle:        protocol.Capability_ExecAttach,
	protocol.TaskControlKind_DialRunner:       protocol.Capability_RunnerAdmin,
	protocol.TaskControlKind_BoardTopics:      protocol.Capability_InfoGlobal,
	protocol.TaskControlKind_BoardRead:        protocol.Capability_InfoGlobal,
	protocol.TaskControlKind_BoardPurge:       protocol.Capability_Purge,
	protocol.TaskControlKind_BoardSubscribers: protocol.Capability_InfoGlobal,
	protocol.TaskControlKind_GitQuery:         protocol.Capability_FileRead,
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
// It deliberately does NOT consult Capability_InfoGlobal. That bit widens what
// may be SEEN; folding it in here would make it widen what may be DONE, and a
// caller holding info_global and cancel would be able to kill anything on the
// server. visibleToCaller is the wrapper that adds it.
func (h *TaskHandler) scopeSet(connID string) (all bool, allowed map[string]bool) {
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
	if scope.Base == protocol.ScopeBase_Global {
		return true, nil
	}

	// Self is unconditional: a task must reach its own log, worktree and
	// session however narrowly it was scoped.
	allowed = map[string]bool{callerHex: true}
	for _, id := range scope.IDs {
		allowed[id] = true
	}
	if scope.Base == protocol.ScopeBase_Subtree {
		descendantsOf(h.childIndex(), callerHex, allowed)
	}
	return false, allowed
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
	all, allowed := h.scopeSet(connID)
	return all || allowed[targetHex]
}

// visibleToCaller is the INFO scope: the action set widened by
// Capability_InfoGlobal. ls, task logs, the port-forward list and the conns
// filter call it exactly as before and inherit the scope with no change of
// their own.
func (h *TaskHandler) visibleToCaller(connID string) (all bool, allowed map[string]bool) {
	if h.lookupPrincipal(connID).Id == ([16]byte{}) {
		return true, nil
	}
	if hasCap(h.callerCaps(connID), protocol.Capability_InfoGlobal) {
		return true, nil
	}
	return h.scopeSet(connID)
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
// for operators, InfoGlobal holders (all=true covers everything anyway),
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
