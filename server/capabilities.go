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
	protocol.TaskControlKind_Submit:          protocol.Capability_Spawn,
	protocol.TaskControlKind_OpenInteractive: protocol.Capability_Spawn,
	protocol.TaskControlKind_Cancel:          protocol.Capability_Cancel,
	protocol.TaskControlKind_PruneTasks:      protocol.Capability_Prune,
	protocol.TaskControlKind_Notify:          protocol.Capability_Notify,
	protocol.TaskControlKind_AttachSession:   protocol.Capability_ExecAttach,
	protocol.TaskControlKind_AwaitIdle:       protocol.Capability_ExecAttach,
	protocol.TaskControlKind_DialRunner:      protocol.Capability_RunnerAdmin,
	protocol.TaskControlKind_BoardTopics:     protocol.Capability_InfoGlobal,
	protocol.TaskControlKind_BoardRead:       protocol.Capability_InfoGlobal,
	protocol.TaskControlKind_BoardPurge:      protocol.Capability_Purge,
	protocol.TaskControlKind_GitQuery:        protocol.Capability_FileRead,
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

// visibleToCaller returns visibility scope for connID:
//   - all=true when the caller is an operator (zero principal) or holds
//     Capability_InfoGlobal; allowed is nil in that case.
//   - all=false with allowed = {callerTaskHex: true} ∪ descendants when the
//     caller is a confined agent without InfoGlobal.
//
// The descendant set is computed by BFS over a creatorHex→[]childHex index
// built from the full task list. The caller's own task id hex is included.
func (h *TaskHandler) visibleToCaller(connID string) (all bool, allowed map[string]bool) {
	pid := h.lookupPrincipal(connID)
	if pid.Id == ([16]byte{}) {
		// Operator: unrestricted.
		return true, nil
	}
	caps := h.callerCaps(connID)
	if hasCap(caps, protocol.Capability_InfoGlobal) {
		return true, nil
	}

	callerHex := hex.EncodeToString(pid.Id[:])

	// Build creator→children index from all tasks.
	allTasks := h.Tasks.List(0)
	children := make(map[string][]string, len(allTasks))
	for _, t := range allTasks {
		if t.CreatorTaskID.Id != ([16]byte{}) {
			pHex := hex.EncodeToString(t.CreatorTaskID.Id[:])
			children[pHex] = append(children[pHex], t.ID)
		}
	}

	// BFS from the caller's own task id.
	allowed = make(map[string]bool)
	queue := []string{callerHex}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if allowed[cur] {
			continue
		}
		allowed[cur] = true
		for _, child := range children[cur] {
			if !allowed[child] {
				queue = append(queue, child)
			}
		}
	}
	return false, allowed
}

// listVisibleToCaller is the LIST scope: visibleToCaller (the ACCESS scope)
// plus the caller's direct creator, returned separately as parentHex.
//
// The two scopes differ on purpose. visibleToCaller answers "may I operate on
// this task" and stays strictly self+descendants — every op gated by it (task
// logs, conn snapshots, port-forward list/kill) would otherwise open upward
// onto a task that, by attenuation, holds a superset of the caller's caps.
// listVisibleToCaller answers the narrower "does this task exist and is it
// alive", which a child legitimately needs about the parent that is driving it
// — and whose id it can already read from the ungated whoami CreatorTaskId.
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
