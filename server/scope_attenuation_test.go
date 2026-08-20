package server

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// A child can never reach further than its spawner. Base clamps by
// permissiveness; ids must already be inside the spawner's set.
func TestAttenuateScopeClampsBase(t *testing.T) {
	h, _, c, _, _ := scopeFixture(t)
	cid := bindPrincipal(t, h, c)

	// subtree parent asking for a global child → subtree. The child's own
	// subtree is a subset of the parent's, so nothing is gained by the ask.
	got, _, ok := h.attenuateScope(cid, Scope{Base: protocol.ScopeBase_Global})
	if !ok || got.Base != protocol.ScopeBase_Subtree {
		t.Errorf("global under a subtree parent = %v ok=%v, want subtree", got.Base, ok)
	}
	// Narrowing is always allowed.
	got, _, ok = h.attenuateScope(cid, Scope{Base: protocol.ScopeBase_None})
	if !ok || got.Base != protocol.ScopeBase_None {
		t.Errorf("none under a subtree parent = %v ok=%v, want none", got.Base, ok)
	}
	// A none-based parent cannot hand out subtree.
	setScope(t, h, c, Scope{Base: protocol.ScopeBase_None})
	got, _, ok = h.attenuateScope(cid, Scope{Base: protocol.ScopeBase_Subtree})
	if !ok || got.Base != protocol.ScopeBase_None {
		t.Errorf("subtree under a none parent = %v ok=%v, want none", got.Base, ok)
	}
	// An operator may ask for anything.
	if got, _, ok = h.attenuateScope("ws:127.0.0.1:1-0", Scope{Base: protocol.ScopeBase_Global}); !ok || got.Base != protocol.ScopeBase_Global {
		t.Errorf("operator global = %v ok=%v", got.Base, ok)
	}
}

func TestAttenuateScopeRejectsForeignID(t *testing.T) {
	h, _, c, g, u := scopeFixture(t)
	cid := bindPrincipal(t, h, c)

	// A descendant is inside the subtree base, so naming it is fine.
	if _, _, ok := h.attenuateScope(cid, Scope{IDs: []string{g}}); !ok {
		t.Error("rejected an id that is inside the spawner's own subtree")
	}
	// A stranger is not, and the offender is named rather than dropped.
	_, offender, ok := h.attenuateScope(cid, Scope{IDs: []string{u}})
	if ok {
		t.Fatal("accepted an id outside the spawner's scope")
	}
	if offender != u {
		t.Errorf("offender = %q, want %q — a silently dropped id would leave the "+
			"child with a scope the caller never wrote", offender, u)
	}
}

// Self is unconditionally in the effective set, so a parent may hand a child an
// explicit grant over the parent itself. Reach stays monotone: the target is
// inside the granter's own reach.
func TestAttenuateScopeAllowsSelfID(t *testing.T) {
	h, _, c, _, _ := scopeFixture(t)
	cid := bindPrincipal(t, h, c)
	if _, off, ok := h.attenuateScope(cid, Scope{Base: protocol.ScopeBase_None, IDs: []string{c}}); !ok {
		t.Errorf("a parent could not grant a child access to the parent itself (offender %q)", off)
	}
}

// End to end through the dispatch loop: the status is reported, not clamped.
func TestSubmitRejectsUnpermittedScope(t *testing.T) {
	h, _, c, _, u := scopeFixture(t)
	h.Registry = NewRegistry()
	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9800-1")}
	if h.principals == nil {
		h.principals = make(map[string]protocol.TaskID)
	}
	h.principals[conn.ConnectionID().String()] = hexToTaskID(t, c)

	utid := hexToTaskID(t, u)
	sub := protocol.SubmitRequest{RequestedCaps: protocol.Capability_All}
	sub.SetRepoPath([]byte("/r"))
	sub.SetPrompt([]byte("p"))
	sub.Scope = Scope{Base: protocol.ScopeBase_None, IDs: []string{u}}.toWire()
	_ = utid
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_Submit, RequestId: 1}
	req.SetSubmit(sub)
	h.Handle(conn, encodeTaskControlRequest(t, req))

	resp := lastTaskControlResponse(t, conn)
	got := resp.Submit()
	if got == nil || got.Status != protocol.SubmitStatus_ScopeNotPermitted {
		t.Fatalf("status = %v, want scope_not_permitted", got)
	}
	if !strings.Contains(string(got.ErrorMsg), u) {
		t.Errorf("error_msg = %q, want it to name the offending id %s", got.ErrorMsg, u)
	}
}

// Resume is an operation on someone else's task: it re-queues that id under a
// prompt of the caller's choosing. An out-of-scope target must be reported as
// absent, not resumed.
func TestResumeOfOutOfScopeTaskIsNotFound(t *testing.T) {
	h, _, c, _, u := scopeFixture(t)
	h.Registry = NewRegistry()
	conn := &fakeConn{id: objproto.MustParseConnectionID("ws:127.0.0.1:9801-1")}
	if h.principals == nil {
		h.principals = make(map[string]protocol.TaskID)
	}
	h.principals[conn.ConnectionID().String()] = hexToTaskID(t, c)

	// Make u terminal so a missing gate really would resume it.
	h.Tasks.Assign(u, "runner-x", "/wt/u", false)
	h.Tasks.Finish(u, 0, nil)

	sub := protocol.SubmitRequest{ResumeTaskId: hexToTaskID(t, u), RequestedCaps: protocol.Capability_All}
	sub.SetRepoPath([]byte("/r"))
	sub.SetPrompt([]byte("takeover"))
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_Submit, RequestId: 2}
	req.SetSubmit(sub)
	h.Handle(conn, encodeTaskControlRequest(t, req))

	resp := lastTaskControlResponse(t, conn)
	if got := resp.Submit(); got == nil || got.Status != protocol.SubmitStatus_ResumeNotFound {
		t.Fatalf("status = %v, want resume_not_found", got)
	}
	if ut, _ := h.Tasks.Get(u); ut.Status == protocol.TaskStatus_Queued {
		t.Fatal("the out-of-scope task was re-queued anyway")
	}
	if string(ut(h, u).Prompt) == "takeover" {
		t.Fatal("the caller's prompt was written onto someone else's task")
	}
}

func ut(h *TaskHandler, id string) TaskEntry {
	e, _ := h.Tasks.Get(id)
	return e
}
