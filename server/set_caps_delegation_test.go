package server

import (
	"reflect"
	"testing"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

func boundReq(t *testing.T, target string, dcaps *protocol.Capability, dscope *authority.Scope) protocol.SetCapsRequest {
	t.Helper()
	req := protocol.SetCapsRequest{TaskId: hexToTaskID(t, target)}
	if dcaps != nil {
		req.DelegateCaps = *dcaps
		req.SetDelegateCapsPresent(true)
	}
	if dscope != nil {
		req.DelegateScope = dscope.ToWire()
		req.SetDelegateScopePresent(true)
	}
	return req
}

func capsPtr(c protocol.Capability) *protocol.Capability { return &c }
func scopePtr(s authority.Scope) *authority.Scope        { return &s }

func opConnAt(port string) *fakeConn {
	return &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:" + port + "-1")}
}

func TestSetCapsBoundHalvesAlone(t *testing.T) {
	h, _, c, _, _ := scopeFixture(t)
	before, _ := h.Tasks.Get(c)

	if got := sendSetCaps(t, h, opConnAt("9970"), boundReq(t, c, capsPtr(protocol.Capability_FileRead), nil)); got.Status != protocol.SetCapsStatus_Ok {
		t.Fatalf("status = %v", got.Status)
	}
	e, _ := h.Tasks.Get(c)
	if e.DelegateCaps != protocol.Capability_FileRead || !reflect.DeepEqual(e.DelegateScope, before.DelegateScope) ||
		e.Capabilities != before.Capabilities || !reflect.DeepEqual(e.Scope, before.Scope) {
		t.Errorf("delegate caps only: %+v", e.Grant())
	}

	none := authority.Scope{Base: protocol.ScopeBase_None}
	sendSetCaps(t, h, opConnAt("9971"), boundReq(t, c, nil, &none))
	e, _ = h.Tasks.Get(c)
	if e.DelegateCaps != protocol.Capability_FileRead || !reflect.DeepEqual(e.DelegateScope, none) {
		t.Errorf("delegate scope only: %+v", e.Grant())
	}
}

func TestSetCapsBoundInvalidScope(t *testing.T) {
	h, _, c, _, _ := scopeFixture(t)
	req := boundReq(t, c, nil, scopePtr(authority.Scope{}))
	req.DelegateScope.VisBase = protocol.ScopeBase_Global // without vis_base_present: non-canonical
	if got := sendSetCaps(t, h, opConnAt("9972"), req); got.Status != protocol.SetCapsStatus_InvalidScope {
		t.Fatalf("status = %v, want invalid_scope", got.Status)
	}
	if e, _ := h.Tasks.Get(c); !reflect.DeepEqual(e.DelegateScope, authority.Scope{}) {
		t.Errorf("the record changed on a refused write: %+v", e.DelegateScope)
	}
}

// A two-level subtree under T: every descendant's caps, scope and bound
// come down inside T's new bound, and an id outside it is dropped.
func TestCascadeWithBoundClampsTwoLevels(t *testing.T) {
	h, p, c, g, u := scopeFixture(t)
	wide := authority.Grant{
		Caps:          protocol.Capability_FileRead | protocol.Capability_Cancel,
		Scope:         authority.Scope{Base: protocol.ScopeBase_Subtree, VisIDs: []string{u}},
		DelegateCaps:  protocol.Capability_FileRead | protocol.Capability_Cancel,
		DelegateScope: authority.Scope{Base: protocol.ScopeBase_Subtree, VisIDs: []string{u}},
	}
	all := GrantUpdate{Caps: true, Scope: true, DelegateCaps: true, DelegateScope: true, To: wide}
	h.Tasks.SetCaps(c, all, AuthorityWrite{})
	h.Tasks.SetCaps(g, all, AuthorityWrite{})

	req := boundReq(t, p, capsPtr(protocol.Capability_FileRead), scopePtr(authority.Scope{}))
	req.SetCascade(true)
	if got := sendSetCaps(t, h, opConnAt("9973"), req); got.Status != protocol.SetCapsStatus_Ok || len(got.Affected) != 3 {
		t.Fatalf("status = %v affected = %d, want ok and 3", got.Status, len(got.Affected))
	}
	for _, id := range []string{c, g} {
		e, _ := h.Tasks.Get(id)
		if e.Capabilities != protocol.Capability_FileRead || len(e.Scope.VisIDs) != 0 ||
			e.DelegateCaps != protocol.Capability_FileRead || len(e.DelegateScope.VisIDs) != 0 {
			t.Errorf("%s after a bound cascade: %+v", id[:8], e.Grant())
		}
	}
}

// D19: every supervisor replays with the empty bound, so a cascade that
// writes no bound half must not clamp anyone to it.
func TestCascadeCapsOnlyLeavesBoundUntouched(t *testing.T) {
	h, _, c, g, _ := scopeFixture(t)
	h.Tasks.SetCaps(g, GrantUpdate{Caps: true, DelegateCaps: true,
		To: authority.Grant{Caps: protocol.Capability_FileRead, DelegateCaps: protocol.Capability_FileRead}}, AuthorityWrite{})

	req := protocol.SetCapsRequest{TaskId: hexToTaskID(t, c), Caps: protocol.Capability_Spawn | protocol.Capability_FileRead}
	req.SetCapsPresent(true)
	req.SetCascade(true)
	if got := sendSetCaps(t, h, opConnAt("9974"), req); got.Status != protocol.SetCapsStatus_Ok {
		t.Fatalf("status = %v", got.Status)
	}
	e, _ := h.Tasks.Get(g)
	if e.Capabilities != protocol.Capability_FileRead || e.DelegateCaps != protocol.Capability_FileRead {
		t.Errorf("caps-only cascade under a default-bound target: %+v, want file_read kept and its bound kept", e.Grant())
	}
}

// D14: the bound is exercised over no connection, so changing it alone is no
// narrowing; a bound cascade that clamps a descendant's caps still is.
func TestBoundOnlyChangeClosesNoConns(t *testing.T) {
	h, _, c, g, _ := scopeFixture(t)
	var dropped []string
	h.DropConnsForPrincipal = func(id string) int { dropped = append(dropped, id); return 1 }
	h.Tasks.SetCaps(c, GrantUpdate{DelegateCaps: true, To: authority.Grant{DelegateCaps: protocol.Capability_All}}, AuthorityWrite{})

	got := sendSetCaps(t, h, opConnAt("9975"), boundReq(t, c, capsPtr(protocol.Capability_None), scopePtr(authority.Scope{Base: protocol.ScopeBase_None})))
	if got.ConnsClosed != 0 || len(dropped) != 0 {
		t.Fatalf("bound-only narrowing closed %d (%v), want 0", got.ConnsClosed, dropped)
	}

	req := boundReq(t, c, capsPtr(protocol.Capability_FileRead), nil)
	req.SetCascade(true)
	got = sendSetCaps(t, h, opConnAt("9976"), req)
	if got.ConnsClosed == 0 {
		t.Fatal("a bound cascade that clamped a descendant's caps closed nothing")
	}
	var droppedG bool
	for _, id := range dropped {
		droppedG = droppedG || id == g
	}
	if !droppedG {
		t.Errorf("dropped %v, want the clamped descendant %s among them", dropped, g[:8])
	}
}
