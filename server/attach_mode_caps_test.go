package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The three attach modes are ranked, not orthogonal: a holder of a stronger
// power may exercise a weaker one without also being granted the weaker bit.
func TestAttachModeCapImplication(t *testing.T) {
	cases := []struct {
		name  string
		held  protocol.Capability
		mode  protocol.AttachMode
		allow bool
	}{
		// exec_view: the weakest grant, and it stops there.
		{"view holds view", protocol.Capability_ExecView, protocol.AttachMode_View, true},
		{"view may not cowrite", protocol.Capability_ExecView, protocol.AttachMode_Cowrite, false},
		{"view may not take control", protocol.Capability_ExecView, protocol.AttachMode_Control, false},

		// exec_cowrite implies view — typing into a stream you cannot read
		// would be a strange thing to grant.
		{"cowrite may view", protocol.Capability_ExecCowrite, protocol.AttachMode_View, true},
		{"cowrite holds cowrite", protocol.Capability_ExecCowrite, protocol.AttachMode_Cowrite, true},
		{"cowrite may not take control", protocol.Capability_ExecCowrite, protocol.AttachMode_Control, false},

		// exec_control implies both. This is what every task carrying the old
		// exec_attach bit holds, so none of them lost anything in the split.
		{"control may view", protocol.Capability_ExecControl, protocol.AttachMode_View, true},
		{"control may cowrite", protocol.Capability_ExecControl, protocol.AttachMode_Cowrite, true},
		{"control holds control", protocol.Capability_ExecControl, protocol.AttachMode_Control, true},

		// Nothing grants nothing.
		{"none may not view", protocol.Capability_None, protocol.AttachMode_View, false},
		{"none may not cowrite", protocol.Capability_None, protocol.AttachMode_Cowrite, false},
		{"none may not take control", protocol.Capability_None, protocol.AttachMode_Control, false},

		// An unrelated grant must not leak in.
		{"spawn is not an attach power", protocol.Capability_Spawn, protocol.AttachMode_View, false},
	}
	for _, c := range cases {
		got := hasAnyCap(c.held, attachModeCap(c.mode))
		if got != c.allow {
			t.Errorf("%s: allowed=%v, want %v", c.name, got, c.allow)
		}
	}
}

// An unrecognised mode from a newer peer must fail closed — answered by the
// strictest bit, not by the most permissive one.
func TestUnknownAttachModeRequiresControl(t *testing.T) {
	unknown := protocol.AttachMode(200)
	if got := attachModeCap(unknown); got != protocol.Capability_ExecControl {
		t.Errorf("unknown mode requires %#x, want exec_control (%#x)", uint32(got), uint32(protocol.Capability_ExecControl))
	}
	if hasAnyCap(protocol.Capability_ExecView|protocol.Capability_ExecCowrite, attachModeCap(unknown)) {
		t.Error("an unknown attach mode was satisfied by the weaker bits — it must fail closed")
	}
}

// hasAnyCap is a set of ALTERNATIVES, unlike hasCap which requires every bit.
// Confusing the two would make attachModeCap's return value demand all three.
func TestHasAnyCapIsAlternativesNotRequirements(t *testing.T) {
	both := protocol.Capability_ExecView | protocol.Capability_ExecControl
	if !hasAnyCap(protocol.Capability_ExecControl, both) {
		t.Error("hasAnyCap should accept a caller holding one of the alternatives")
	}
	if hasCap(protocol.Capability_ExecControl, both) {
		t.Error("hasCap must still require EVERY bit — the two helpers cannot be interchangeable")
	}
}

// The whole point of the split, stated as a test: granting a supervisor the
// ability to watch its workers must not also let it evict whoever is driving
// one. Before the split there was a single bit and this was impossible.
func TestWatchingDoesNotGrantEviction(t *testing.T) {
	supervisor := protocol.Capability_Spawn | protocol.Capability_ExecView
	if !hasAnyCap(supervisor, attachModeCap(protocol.AttachMode_View)) {
		t.Fatal("a supervisor granted exec_view cannot watch — the grant is useless")
	}
	if hasAnyCap(supervisor, attachModeCap(protocol.AttachMode_Control)) {
		t.Error("exec_view let the supervisor take a session over; that is the over-grant this split removes")
	}
	if hasAnyCap(supervisor, attachModeCap(protocol.AttachMode_Cowrite)) {
		t.Error("exec_view let the supervisor type into a session it should only watch")
	}
}
