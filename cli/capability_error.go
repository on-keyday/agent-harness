package cli

import (
	"fmt"
	"math/bits"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// CapabilityDeniedError is returned by RoundTripTaskControl when the server
// responds with TaskControlKind_PermissionDenied. It carries the operation
// kind that was attempted and the capability the server required.
type CapabilityDeniedError struct {
	// RequestedKind is the TaskControlKind the client sent (e.g. Submit,
	// OpenInteractive) that the server rejected.
	RequestedKind protocol.TaskControlKind
	// RequiredCap is the capability the server required for the operation.
	//
	// It may be a MASK of alternatives rather than one bit: an attach is gated
	// by `attachModeCap`, whose result the server checks with `hasAnyCap`, so a
	// view attach arrives here as exec_view|exec_cowrite|exec_control — "any of
	// these", not "all of these".
	RequiredCap protocol.Capability
}

// Error names the capability the way every other surface does — through
// CapsLabel, the one renderer for a capability mask.
//
// It used to call the generated enum String(), which only knows SINGLE values
// and falls back to the numeric form for anything else. An attach denial
// therefore read `requires capability Capability(12292)`: unreadable, and
// wrong in the other direction too, since the singular "capability" claimed one
// bit was required when the mask is a set of alternatives.
func (e *CapabilityDeniedError) Error() string {
	label := CapsLabel(e.RequiredCap)
	if bits.OnesCount32(uint32(e.RequiredCap)) > 1 {
		return fmt.Sprintf("permission denied: %s requires any of %s",
			e.RequestedKind.String(), label)
	}
	return fmt.Sprintf("permission denied: %s requires capability %s",
		e.RequestedKind.String(), label)
}
