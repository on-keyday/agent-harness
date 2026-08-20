package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
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

// The unit tests above prove attachModeCap's table. This one proves it is
// actually WIRED: the gate lives inline in the AttachSession dispatch case
// (the requiredCap map cannot see AttachMode), so the table being right says
// nothing about the dispatch reading it. A caller holding only exec_view must
// pass the gate for mode=view and be denied for the two stronger modes.
func TestAttachSessionGateIsWiredPerMode(t *testing.T) {
	h := newTestHandler(t)

	// A confined principal with exec_view and an explicit scope id for the
	// target, so a denial can only come from the capability gate — without the
	// scope id the target gate answers first and the gate under test is never
	// reached.
	const targetHex = "ee000000000000000000000000000000"
	pidHex := h.Tasks.Create("repo", "p", protocol.TaskKind_Oneshot,
		protocol.ClientKind_Agent, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, protocol.Capability_ExecView,
		Scope{Base: protocol.ScopeBase_Subtree, IDs: []string{targetHex}}, "")
	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9702-1")}
	if h.principals == nil {
		h.principals = make(map[string]protocol.TaskID)
	}
	h.principals[conn.ConnectionID().String()] = hexToTaskID(t, pidHex)

	var target protocol.TaskID
	target.Id[0] = 0xEE

	send := func(t *testing.T, mode protocol.AttachMode, reqID uint32) protocol.TaskControlResponse {
		t.Helper()
		req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AttachSession, RequestId: reqID}
		req.SetAttach(protocol.AttachSessionRequest{TaskId: target, Mode: mode})
		h.Handle(conn, encodeTaskControlRequest(t, req))
		return lastTaskControlResponse(t, conn)
	}

	// Denied: exec_view does not authorise typing into a session or taking it.
	for _, c := range []struct {
		mode protocol.AttachMode
		name string
	}{
		{protocol.AttachMode_Cowrite, "cowrite"},
		{protocol.AttachMode_Control, "control"},
	} {
		resp := send(t, c.mode, 1)
		if resp.Kind != protocol.TaskControlKind_PermissionDenied {
			t.Fatalf("mode=%s with only exec_view: kind = %v, want PermissionDenied — "+
				"the mode-aware gate is not wired into the dispatch", c.name, resp.Kind)
		}
		pd := resp.PermissionDenied()
		if pd == nil {
			t.Fatalf("mode=%s: no PermissionDenied body", c.name)
		}
		// The reported requirement is the SET of bits that would have
		// satisfied it, so an operator reading the denial learns which grant
		// to ask for rather than only that one was missing.
		if !hasAnyCap(pd.RequiredCap, protocol.Capability_ExecControl) {
			t.Errorf("mode=%s: RequiredCap = %#x, want it to name exec_control among the alternatives",
				c.name, uint32(pd.RequiredCap))
		}
	}

	// Allowed past the gate: mode=view. The session does not exist in this
	// fixture, so the handler's own not_found is the PASS signal — what matters
	// is that it is not permission_denied.
	resp := send(t, protocol.AttachMode_View, 2)
	if resp.Kind == protocol.TaskControlKind_PermissionDenied {
		t.Fatalf("mode=view with exec_view was denied (RequiredCap %#x) — the grant buys nothing",
			uint32(resp.PermissionDenied().RequiredCap))
	}
	if resp.Attach() == nil {
		t.Fatalf("mode=view: expected an AttachSession response, got kind %v", resp.Kind)
	}
}
