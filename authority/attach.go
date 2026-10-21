package authority

import (
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// AttachModeCap returns the capability set that satisfies an attach in the
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
func AttachModeCap(mode protocol.AttachMode) protocol.Capability {
	switch mode {
	case protocol.AttachMode_View:
		return protocol.Capability_ExecView | protocol.Capability_ExecCowrite | protocol.Capability_ExecControl
	case protocol.AttachMode_Cowrite:
		return protocol.Capability_ExecCowrite | protocol.Capability_ExecControl
	default:
		return protocol.Capability_ExecControl
	}
}

// AttachModeScopeCap is the SINGLE bit whose scope governs an attach in the
// given mode. Distinct from AttachModeCap, which returns the set of bits that
// SATISFY the request: a holder of exec_control attaching in view mode is
// exercising the view power, so the view scope is the one that binds it.
//
// An unrecognised mode falls through to exec_control, matching AttachModeCap's
// fail-closed default.
func AttachModeScopeCap(mode protocol.AttachMode) protocol.Capability {
	switch mode {
	case protocol.AttachMode_View:
		return protocol.Capability_ExecView
	case protocol.AttachMode_Cowrite:
		return protocol.Capability_ExecCowrite
	default:
		return protocol.Capability_ExecControl
	}
}

// ExercisedBits is the set of bits whose SCOPE a holder of caps acts through.
// The attach powers are ranked (AttachModeCap): a holder of exec_control may
// view and cowrite, and a holder of exec_cowrite may view, while each attach
// is bound by its MODE's own bit's scope (AttachModeScopeCap). Bounding a
// child by its literal bits would leave the scopes it attaches through
// unbounded. The resume rank does not widen this way: resumeTargetAllowed
// reads only the scope of a resume bit the caller holds.
func ExercisedBits(caps protocol.Capability) protocol.Capability {
	if caps&protocol.Capability_ExecControl != 0 {
		caps |= protocol.Capability_ExecCowrite
	}
	if caps&protocol.Capability_ExecCowrite != 0 {
		caps |= protocol.Capability_ExecView
	}
	return caps
}
