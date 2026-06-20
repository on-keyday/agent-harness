package server

import (
	"encoding/hex"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// requiredCap maps a direction-independent TaskControlKind to the cap it needs.
// Kinds absent from the map are gated elsewhere: OpenFileTransfer / ListFiles /
// OpenPortForward are direction-dependent (Task 5); List / GetTaskLog are
// INFO-scoped (Task 6).
var requiredCap = map[protocol.TaskControlKind]protocol.Capability{
	protocol.TaskControlKind_Submit:          protocol.Capability_Spawn,
	protocol.TaskControlKind_OpenInteractive: protocol.Capability_Spawn,
	protocol.TaskControlKind_Cancel:          protocol.Capability_Cancel,
	protocol.TaskControlKind_PruneTasks:      protocol.Capability_Prune,
	protocol.TaskControlKind_Notify:          protocol.Capability_Notify,
	protocol.TaskControlKind_AttachSession:   protocol.Capability_ExecAttach,
	protocol.TaskControlKind_DialRunner:      protocol.Capability_RunnerAdmin,
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
