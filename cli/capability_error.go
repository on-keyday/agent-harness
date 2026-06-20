package cli

import (
	"fmt"

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
	RequiredCap protocol.Capability
}

func (e *CapabilityDeniedError) Error() string {
	return fmt.Sprintf("permission denied: %s requires capability %s",
		e.RequestedKind.String(), e.RequiredCap.String())
}
