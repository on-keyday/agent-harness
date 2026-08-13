package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

func sendSetParent(t *testing.T, h *TaskHandler, conn *fakeConn, req protocol.SetParentRequest) protocol.SetParentResponse {
	t.Helper()
	tc := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_SetParent, RequestId: 1}
	tc.SetSetParent(req)
	h.Handle(conn, encodeTaskControlRequest(t, tc))
	resp := lastTaskControlResponse(t, conn)
	got := resp.SetParent()
	if got == nil {
		t.Fatalf("no set_parent response (kind %v)", resp.Kind)
	}
	return *got
}

// Same gate as set_caps: operator identity, never a capability bit — a bit
// authorising "re-point parents" would adopt any victim under its holder.
func TestSetParentRejectsNonOperator(t *testing.T) {
	h, p, c, _, u := scopeFixture(t)
	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9910-1")}
	if h.principals == nil {
		h.principals = make(map[string]protocol.TaskID)
	}
	h.principals[conn.ConnectionID().String()] = hexToTaskID(t, c)

	got := sendSetParent(t, h, conn, protocol.SetParentRequest{
		TaskId: hexToTaskID(t, u), ParentId: hexToTaskID(t, c),
	})
	if got.Status != protocol.SetParentStatus_NotOperator {
		t.Fatalf("status = %v, want not_operator", got.Status)
	}
	if e, _ := h.Tasks.Get(c); e.CreatorTaskID != hexToTaskID(t, p) {
		t.Fatal("a confined caller moved a parent link")
	}
}

// descendantsOf seeds the walk with its own root, so parent==target is inside
// the same predicate as parent∈descendants — both must answer would_cycle.
func TestSetParentRejectsCycles(t *testing.T) {
	h, p, c, g, _ := scopeFixture(t)
	opConn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9911-1")}

	// g is a descendant of c.
	got := sendSetParent(t, h, opConn, protocol.SetParentRequest{
		TaskId: hexToTaskID(t, c), ParentId: hexToTaskID(t, g),
	})
	if got.Status != protocol.SetParentStatus_WouldCycle {
		t.Fatalf("descendant parent: status = %v, want would_cycle", got.Status)
	}
	// self-parent.
	got = sendSetParent(t, h, opConn, protocol.SetParentRequest{
		TaskId: hexToTaskID(t, c), ParentId: hexToTaskID(t, c),
	})
	if got.Status != protocol.SetParentStatus_WouldCycle {
		t.Fatalf("self parent: status = %v, want would_cycle", got.Status)
	}
	if e, _ := h.Tasks.Get(c); e.CreatorTaskID != hexToTaskID(t, p) {
		t.Fatal("a rejected request moved the link anyway")
	}
}

func TestSetParentDetachAndRepoint(t *testing.T) {
	h, p, c, _, u := scopeFixture(t)
	opConn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9912-1")}

	// Detach c to the root.
	got := sendSetParent(t, h, opConn, protocol.SetParentRequest{TaskId: hexToTaskID(t, c)})
	if got.Status != protocol.SetParentStatus_Ok {
		t.Fatalf("detach: status = %v, want ok", got.Status)
	}
	if got.OldParent != hexToTaskID(t, p) || got.NewParent.Id != ([16]byte{}) {
		t.Fatalf("detach echo: old=%x new=%x", got.OldParent.Id, got.NewParent.Id)
	}
	// Re-point u under c.
	got = sendSetParent(t, h, opConn, protocol.SetParentRequest{
		TaskId: hexToTaskID(t, u), ParentId: hexToTaskID(t, c),
	})
	if got.Status != protocol.SetParentStatus_Ok || got.NewParent != hexToTaskID(t, c) {
		t.Fatalf("repoint: status=%v new=%x", got.Status, got.NewParent.Id)
	}
	// Unknown parent.
	var ghost protocol.TaskID
	ghost.Id[0] = 0xEE
	got = sendSetParent(t, h, opConn, protocol.SetParentRequest{
		TaskId: hexToTaskID(t, u), ParentId: ghost,
	})
	if got.Status != protocol.SetParentStatus_ParentNotFound {
		t.Fatalf("ghost parent: status = %v, want parent_not_found", got.Status)
	}
	// Unknown target.
	got = sendSetParent(t, h, opConn, protocol.SetParentRequest{TaskId: ghost})
	if got.Status != protocol.SetParentStatus_NotFound {
		t.Fatalf("ghost target: status = %v, want not_found", got.Status)
	}
}

func TestSetParentSwap(t *testing.T) {
	h, p, c, g, _ := scopeFixture(t)
	opConn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9913-1")}

	// A sibling of g under c must not move.
	sib := h.Tasks.Create("/r", "sib", protocol.TaskKind_Oneshot,
		protocol.ClientKind_Agent, hexToTaskID(t, c), "", protocol.RunnerSelector{},
		nil, protocol.Capability_Cancel, Scope{}, "")
	capsBefore := map[string]protocol.Capability{}
	for _, id := range []string{p, c, g, sib} {
		e, _ := h.Tasks.Get(id)
		capsBefore[id] = e.Capabilities
	}

	// swap with a non-zero parent_id is rejected, not ignored.
	req := protocol.SetParentRequest{TaskId: hexToTaskID(t, g), ParentId: hexToTaskID(t, p)}
	req.SetSwap(true)
	if got := sendSetParent(t, h, opConn, req); got.Status != protocol.SetParentStatus_SwapTakesNoParent {
		t.Fatalf("swap+parent: status = %v, want swap_takes_no_parent", got.Status)
	}

	// swap on an operator-rooted task: nothing to invert.
	req = protocol.SetParentRequest{TaskId: hexToTaskID(t, p)}
	req.SetSwap(true)
	if got := sendSetParent(t, h, opConn, req); got.Status != protocol.SetParentStatus_NoParent {
		t.Fatalf("root swap: status = %v, want no_parent", got.Status)
	}

	// p → c → g: swap(g) puts g under p and c under g.
	req = protocol.SetParentRequest{TaskId: hexToTaskID(t, g)}
	req.SetSwap(true)
	got := sendSetParent(t, h, opConn, req)
	if got.Status != protocol.SetParentStatus_Ok {
		t.Fatalf("swap: status = %v, want ok", got.Status)
	}
	if got.OldParent != hexToTaskID(t, c) || got.NewParent != hexToTaskID(t, p) || got.SwappedId != hexToTaskID(t, c) {
		t.Fatalf("swap echo: old=%x new=%x swapped=%x", got.OldParent.Id, got.NewParent.Id, got.SwappedId.Id)
	}
	if e, _ := h.Tasks.Get(g); e.CreatorTaskID != hexToTaskID(t, p) {
		t.Fatalf("g's parent = %x, want p", e.CreatorTaskID.Id)
	}
	if e, _ := h.Tasks.Get(c); e.CreatorTaskID != hexToTaskID(t, g) {
		t.Fatalf("c's parent = %x, want g", e.CreatorTaskID.Id)
	}
	if e, _ := h.Tasks.Get(sib); e.CreatorTaskID != hexToTaskID(t, c) {
		t.Fatal("sibling moved; must stay under c")
	}
	// No caps changed anywhere.
	for id, want := range capsBefore {
		if e, _ := h.Tasks.Get(id); e.Capabilities != want {
			t.Fatalf("caps of %s changed: %v -> %v", id[:8], want, e.Capabilities)
		}
	}

	// swap(g) again: g is now under p (the root task); c follows g. This is
	// the P=zero shape one level up: swap(p's child) where the parent is
	// operator-rooted.
	req = protocol.SetParentRequest{TaskId: hexToTaskID(t, g)}
	req.SetSwap(true)
	got = sendSetParent(t, h, opConn, req)
	if got.Status != protocol.SetParentStatus_Ok || got.NewParent.Id != ([16]byte{}) {
		t.Fatalf("swap to root: status=%v new=%x, want ok/(root)", got.Status, got.NewParent.Id)
	}
	if e, _ := h.Tasks.Get(g); e.CreatorTaskID.Id != ([16]byte{}) {
		t.Fatal("g should now be operator-rooted")
	}
	if e, _ := h.Tasks.Get(p); e.CreatorTaskID != hexToTaskID(t, g) {
		t.Fatal("p should now be under g")
	}
}

// After a swap the subtree walk follows the new edges: the promoted task sees
// its former parent; the demoted one no longer sees its former child.
func TestSetParentSwapMovesScope(t *testing.T) {
	h, _, c, g, _ := scopeFixture(t)
	opConn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9914-1")}

	req := protocol.SetParentRequest{TaskId: hexToTaskID(t, g)}
	req.SetSwap(true)
	if got := sendSetParent(t, h, opConn, req); got.Status != protocol.SetParentStatus_Ok {
		t.Fatalf("swap: %v", got.Status)
	}

	gCID := bindPrincipal(t, h, g)
	all, allowed := h.scopeSet(gCID)
	if all || !allowed[c] {
		t.Fatalf("g's subtree after swap: all=%v allowed[c]=%v, want c inside", all, allowed[c])
	}
	cCID := bindPrincipal(t, h, c)
	all, allowed = h.scopeSet(cCID)
	if all || allowed[g] {
		t.Fatalf("c's subtree after swap: all=%v allowed[g]=%v, want g outside", all, allowed[g])
	}
}

// A dangling current-parent link (pruned creator) answers parent_not_found on
// the swap path rather than writing garbage.
func TestSetParentSwapDanglingParent(t *testing.T) {
	h, _, c, _, _ := scopeFixture(t)
	opConn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9915-1")}
	var ghost protocol.TaskID
	ghost.Id[15] = 0xEE
	if _, ok := h.Tasks.SetParent(c, ghost); !ok {
		t.Fatal("fixture: SetParent to a dangling id failed")
	}
	req := protocol.SetParentRequest{TaskId: hexToTaskID(t, c)}
	req.SetSwap(true)
	if got := sendSetParent(t, h, opConn, req); got.Status != protocol.SetParentStatus_ParentNotFound {
		t.Fatalf("status = %v, want parent_not_found", got.Status)
	}
}
