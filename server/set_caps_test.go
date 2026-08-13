package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

func sendSetCaps(t *testing.T, h *TaskHandler, conn *fakeConn, req protocol.SetCapsRequest) protocol.SetCapsResponse {
	t.Helper()
	tc := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_SetCaps, RequestId: 1}
	tc.SetSetCaps(req)
	h.Handle(conn, encodeTaskControlRequest(t, tc))
	resp := lastTaskControlResponse(t, conn)
	got := resp.SetCaps()
	if got == nil {
		t.Fatalf("no set_caps response (kind %v)", resp.Kind)
	}
	return *got
}

// The gate is operator identity, never a capability bit — a bit authorising
// "rewrite capabilities" would grant itself all on first use.
func TestSetCapsRejectsNonOperator(t *testing.T) {
	h, _, c, _, u := scopeFixture(t)
	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9900-1")}
	if h.principals == nil {
		h.principals = make(map[string]protocol.TaskID)
	}
	h.principals[conn.ConnectionID().String()] = hexToTaskID(t, c)

	req := protocol.SetCapsRequest{TaskId: hexToTaskID(t, u), Caps: protocol.Capability_All}
	req.SetCapsPresent(true)
	got := sendSetCaps(t, h, conn, req)
	if got.Status != protocol.SetCapsStatus_NotOperator {
		t.Fatalf("status = %v, want not_operator", got.Status)
	}
	if e, _ := h.Tasks.Get(u); e.Capabilities == protocol.Capability_All {
		t.Fatal("a confined caller raised another task's caps")
	}
}

// Enforcement re-reads the store per request, so the new authority is in force
// on the target's next RPC with nothing restarted.
func TestSetCapsIsLive(t *testing.T) {
	h, _, c, _, _ := scopeFixture(t)
	opConn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9901-1")}
	victimCID := bindPrincipal(t, h, c)

	if !hasCap(h.callerCaps(victimCID), protocol.Capability_Cancel) {
		t.Fatal("fixture precondition: the task should start with cancel")
	}
	req := protocol.SetCapsRequest{TaskId: hexToTaskID(t, c), Caps: protocol.Capability_None}
	req.SetCapsPresent(true)
	if got := sendSetCaps(t, h, opConn, req); got.Status != protocol.SetCapsStatus_Ok {
		t.Fatalf("status = %v, want ok", got.Status)
	}
	if hasCap(h.callerCaps(victimCID), protocol.Capability_Cancel) {
		t.Fatal("the revoked cap is still in force on the next lookup")
	}
}

// Attenuation happens at spawn, so a revoked parent keeps reaching through a
// child it created while still wide. --cascade re-imposes the invariant.
func TestSetCapsCascadeClampsDescendants(t *testing.T) {
	h, _, c, g, _ := scopeFixture(t)
	opConn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9902-1")}

	// Without cascade the grandchild keeps what it was given.
	req := protocol.SetCapsRequest{TaskId: hexToTaskID(t, c), Caps: protocol.Capability_FileRead}
	req.SetCapsPresent(true)
	got := sendSetCaps(t, h, opConn, req)
	if len(got.Affected) != 1 {
		t.Fatalf("affected = %d, want 1 without cascade", len(got.Affected))
	}
	if ge, _ := h.Tasks.Get(g); !hasCap(ge.Capabilities, protocol.Capability_Cancel) {
		t.Fatal("the descendant was clamped without --cascade")
	}

	// With cascade it is AND'd down and the base is clamped.
	req = protocol.SetCapsRequest{
		TaskId: hexToTaskID(t, c),
		Caps:   protocol.Capability_FileRead,
		Scope:  Scope{Base: protocol.ScopeBase_None}.toWire(),
	}
	req.SetCapsPresent(true)
	req.SetScopePresent(true)
	req.SetCascade(true)
	got = sendSetCaps(t, h, opConn, req)
	if got.Status != protocol.SetCapsStatus_Ok {
		t.Fatalf("status = %v, want ok", got.Status)
	}
	if len(got.Affected) != 2 {
		t.Fatalf("affected = %d, want the target plus its descendant", len(got.Affected))
	}
	ge, _ := h.Tasks.Get(g)
	if hasCap(ge.Capabilities, protocol.Capability_Cancel) {
		t.Error("the descendant kept a cap the parent no longer holds")
	}
	if ge.Scope.Base != protocol.ScopeBase_None {
		t.Errorf("descendant base = %v, want it clamped to none", ge.Scope.Base)
	}
}

// A narrowing must reach in-flight work; a widening must not.
func TestSetCapsDropsConnsOnlyWhenNarrowing(t *testing.T) {
	h, _, c, _, _ := scopeFixture(t)
	opConn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9903-1")}
	var dropped []string
	h.DropConnsForPrincipal = func(id string) int { dropped = append(dropped, id); return 2 }

	// Start narrow so the next call is unambiguously a widening.
	h.Tasks.SetCaps(c, true, protocol.Capability_FileRead, false, Scope{}) //nolint:errcheck

	widen := protocol.SetCapsRequest{TaskId: hexToTaskID(t, c), Caps: protocol.Capability_All}
	widen.SetCapsPresent(true)
	if got := sendSetCaps(t, h, opConn, widen); got.ConnsClosed != 0 {
		t.Errorf("conns_closed = %d on a pure widening, want 0", got.ConnsClosed)
	}
	if len(dropped) != 0 {
		t.Fatalf("dropped %v on a widening", dropped)
	}

	narrow := protocol.SetCapsRequest{TaskId: hexToTaskID(t, c), Caps: protocol.Capability_None}
	narrow.SetCapsPresent(true)
	got := sendSetCaps(t, h, opConn, narrow)
	if got.ConnsClosed != 2 || len(dropped) != 1 || dropped[0] != c {
		t.Fatalf("narrowing: conns_closed=%d dropped=%v, want 2 and [%s]", got.ConnsClosed, dropped, c)
	}

	// --keep-conns opts out.
	dropped = nil
	keep := protocol.SetCapsRequest{TaskId: hexToTaskID(t, c), Caps: protocol.Capability_None,
		Scope: Scope{Base: protocol.ScopeBase_None}.toWire()}
	keep.SetScopePresent(true)
	keep.SetKeepConns(true)
	if got := sendSetCaps(t, h, opConn, keep); got.ConnsClosed != 0 || len(dropped) != 0 {
		t.Fatalf("--keep-conns still dropped: closed=%d dropped=%v", got.ConnsClosed, dropped)
	}
}

// Narrowing the scope alone — no cap bit touched — still counts.
func TestScopeOnlyNarrowingCountsAsNarrowing(t *testing.T) {
	before := TaskEntry{Capabilities: protocol.Capability_All,
		Scope: Scope{Base: protocol.ScopeBase_Subtree, IDs: []string{"aa", "bb"}}}
	if !isNarrowing(before, TaskEntry{Capabilities: protocol.Capability_All,
		Scope: Scope{Base: protocol.ScopeBase_None, IDs: []string{"aa", "bb"}}}) {
		t.Error("a base ranked down is not reported as a narrowing")
	}
	if !isNarrowing(before, TaskEntry{Capabilities: protocol.Capability_All,
		Scope: Scope{Base: protocol.ScopeBase_Subtree, IDs: []string{"aa"}}}) {
		t.Error("a dropped scope id is not reported as a narrowing")
	}
	if isNarrowing(before, TaskEntry{Capabilities: protocol.Capability_All,
		Scope: Scope{Base: protocol.ScopeBase_Global, IDs: []string{"aa", "bb", "cc"}}}) {
		t.Error("a pure widening was reported as a narrowing")
	}
}

func TestSetCapsMissingTask(t *testing.T) {
	h, _, _, _, _ := scopeFixture(t)
	opConn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9904-1")}
	req := protocol.SetCapsRequest{Caps: protocol.Capability_All}
	req.SetCapsPresent(true)
	if got := sendSetCaps(t, h, opConn, req); got.Status != protocol.SetCapsStatus_NotFound {
		t.Fatalf("status = %v, want not_found", got.Status)
	}
}
