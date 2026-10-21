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
	got, _, ok := h.attenuateScope(cid, Scope{Base: protocol.ScopeBase_Global}, protocol.Capability_All)
	if !ok || got.Base != protocol.ScopeBase_Subtree {
		t.Errorf("global under a subtree parent = %v ok=%v, want subtree", got.Base, ok)
	}
	// Narrowing is always allowed.
	got, _, ok = h.attenuateScope(cid, Scope{Base: protocol.ScopeBase_None}, protocol.Capability_All)
	if !ok || got.Base != protocol.ScopeBase_None {
		t.Errorf("none under a subtree parent = %v ok=%v, want none", got.Base, ok)
	}
	// A none-based parent cannot hand out subtree.
	setScope(t, h, c, Scope{Base: protocol.ScopeBase_None})
	got, _, ok = h.attenuateScope(cid, Scope{Base: protocol.ScopeBase_Subtree}, protocol.Capability_All)
	if !ok || got.Base != protocol.ScopeBase_None {
		t.Errorf("subtree under a none parent = %v ok=%v, want none", got.Base, ok)
	}
	// An operator may ask for anything.
	if got, _, ok = h.attenuateScope("ws:127.0.0.1:1-0", Scope{Base: protocol.ScopeBase_Global}, protocol.Capability_All); !ok || got.Base != protocol.ScopeBase_Global {
		t.Errorf("operator global = %v ok=%v", got.Base, ok)
	}
}

func TestAttenuateScopeRejectsForeignID(t *testing.T) {
	h, _, c, g, u := scopeFixture(t)
	cid := bindPrincipal(t, h, c)

	// A descendant is inside the subtree base, so naming it is fine.
	if _, _, ok := h.attenuateScope(cid, Scope{IDs: []string{g}}, protocol.Capability_All); !ok {
		t.Error("rejected an id that is inside the spawner's own subtree")
	}
	// A stranger is not, and the offender is named rather than dropped.
	_, offender, ok := h.attenuateScope(cid, Scope{IDs: []string{u}}, protocol.Capability_All)
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
	if _, off, ok := h.attenuateScope(cid, Scope{Base: protocol.ScopeBase_None, IDs: []string{c}}, protocol.Capability_All); !ok {
		t.Errorf("a parent could not grant a child access to the parent itself (offender %q)", off)
	}
}

// A global VISIBILITY rank is not a global action rank. Attenuation used to
// return before any clamp when the creator could see everything, so a creator
// acting on its subtree handed out a global action scope.
func TestAttenuateScopeClampsUnderGlobalVisibility(t *testing.T) {
	h, _, c, _, u := scopeFixture(t)
	cid := bindPrincipal(t, h, c)
	setScope(t, h, c, Scope{Base: protocol.ScopeBase_Subtree, VisBasePresent: true, VisBase: protocol.ScopeBase_Global})

	got, _, ok := h.attenuateScope(cid, Scope{Base: protocol.ScopeBase_Global}, protocol.Capability_Cancel)
	if !ok || got.Base != protocol.ScopeBase_Subtree {
		t.Errorf("action base = %v ok=%v, want subtree: the creator acts on its subtree only", got.Base, ok)
	}
	// Seeing is the creator's to hand on: the child keeps the global rank.
	if got.VisRank() != protocol.ScopeBase_Global {
		t.Errorf("visibility rank = %v, want global", got.VisRank())
	}
	// u is visible to the creator but outside what it may cancel.
	if _, off, ok := h.attenuateScope(cid, Scope{Base: protocol.ScopeBase_None, IDs: []string{u}}, protocol.Capability_Cancel); ok || off != u {
		t.Errorf("ids:u for a cancel holder: ok=%v offender=%q, want refused naming %s", ok, off, u)
	}
}

// A view-only id is something the creator may SEE. Handing it on as an action
// id would let the child act where the creator may not.
func TestAttenuateScopeViewOnlyIDIsNotActionable(t *testing.T) {
	h, _, c, _, u := scopeFixture(t)
	cid := bindPrincipal(t, h, c)
	setScope(t, h, c, Scope{Base: protocol.ScopeBase_Subtree, VisIDs: []string{u}})

	if _, off, ok := h.attenuateScope(cid, Scope{Base: protocol.ScopeBase_Subtree, IDs: []string{u}}, protocol.Capability_Cancel); ok || off != u {
		t.Errorf("action id from a view-only grant: ok=%v offender=%q, want refused naming %s", ok, off, u)
	}
	// As a view-only id it passes on unchanged.
	if _, off, ok := h.attenuateScope(cid, Scope{Base: protocol.ScopeBase_Subtree, VisIDs: []string{u}}, protocol.Capability_Cancel); !ok {
		t.Errorf("view-only id refused (offender %q)", off)
	}
	// A child that holds no bit can only SEE through an action id, so the
	// visibility check is the one that binds it.
	if _, off, ok := h.attenuateScope(cid, Scope{Base: protocol.ScopeBase_Subtree, IDs: []string{u}}, protocol.Capability_None); !ok {
		t.Errorf("action id for a child holding nothing refused (offender %q)", off)
	}
}

// The creator's override for a bit bounds the child's scope for that bit, even
// when the child writes no override for it — and ONLY that bit: the child's
// other bits keep the rank they asked for, so the narrowing arrives as an
// override on the child rather than as a lowered base.
func TestAttenuateScopeHonoursTheCreatorsOverride(t *testing.T) {
	h, _, c, _, _ := scopeFixture(t)
	cid := bindPrincipal(t, h, c)
	setScope(t, h, c, Scope{Base: protocol.ScopeBase_Global,
		Overrides: []ScopeOverride{{Caps: protocol.Capability_Cancel, Base: protocol.ScopeBase_None}}})

	got, _, ok := h.attenuateScope(cid, Scope{Base: protocol.ScopeBase_Global}, protocol.Capability_Cancel|protocol.Capability_FileRead)
	if !ok {
		t.Fatal("refused")
	}
	if base, _, _ := got.ForCap(protocol.Capability_Cancel); base != protocol.ScopeBase_None {
		t.Errorf("cancel = %v, want none: the creator may cancel nothing but itself", base)
	}
	if base, _, _ := got.ForCap(protocol.Capability_FileRead); base != protocol.ScopeBase_Global {
		t.Errorf("file_read = %v, want global: the creator's cancel override does not narrow it", base)
	}
	if err := validateScope(got); err != nil {
		t.Errorf("result is not a legal scope: %v", err)
	}
	// A child that does not hold cancel is not bound by the cancel override.
	got, _, ok = h.attenuateScope(cid, Scope{Base: protocol.ScopeBase_Global}, protocol.Capability_FileRead)
	if !ok || got.Base != protocol.ScopeBase_Global || len(got.Overrides) != 0 {
		t.Errorf("scope = %+v ok=%v, want plain global for a file_read-only child", got, ok)
	}
	// The same bound on the child's own override, split where the creator's
	// ranks for its bits differ.
	got, _, ok = h.attenuateScope(cid, Scope{Base: protocol.ScopeBase_None,
		Overrides: []ScopeOverride{{Caps: protocol.Capability_Cancel | protocol.Capability_FileRead, Base: protocol.ScopeBase_Global}}},
		protocol.Capability_Cancel|protocol.Capability_FileRead)
	if !ok {
		t.Fatal("refused")
	}
	if base, _, _ := got.ForCap(protocol.Capability_Cancel); base != protocol.ScopeBase_None {
		t.Errorf("override cancel = %v, want none", base)
	}
	if base, _, _ := got.ForCap(protocol.Capability_FileRead); base != protocol.ScopeBase_Global {
		t.Errorf("override file_read = %v, want global", base)
	}
	if err := validateScope(got); err != nil {
		t.Errorf("result is not a legal scope: %v", err)
	}
	// spawn names no task and may not appear in an override, so a child
	// holding it gets no override for it.
	got, _, ok = h.attenuateScope(cid, Scope{Base: protocol.ScopeBase_Global}, protocol.Capability_All)
	if !ok {
		t.Fatal("refused")
	}
	if err := validateScope(got); err != nil {
		t.Errorf("an all-holding child got an illegal scope: %v", err)
	}
}

// Where the creator's override is WIDER than its base, the child may have
// exactly that much for the bit: the bound is the creator's scope for the bit,
// not its base.
func TestAttenuateScopeAllowsWhatTheCreatorsOverrideAllows(t *testing.T) {
	h, _, c, _, _ := scopeFixture(t)
	cid := bindPrincipal(t, h, c)
	setScope(t, h, c, Scope{Base: protocol.ScopeBase_None,
		Overrides: []ScopeOverride{{Caps: protocol.Capability_ExecView, Base: protocol.ScopeBase_Global}}})

	got, _, ok := h.attenuateScope(cid, Scope{Base: protocol.ScopeBase_Global}, protocol.Capability_ExecView|protocol.Capability_Cancel)
	if !ok {
		t.Fatal("refused")
	}
	if base, _, _ := got.ForCap(protocol.Capability_ExecView); base != protocol.ScopeBase_Global {
		t.Errorf("exec_view = %v, want global like the creator's", base)
	}
	if base, _, _ := got.ForCap(protocol.Capability_Cancel); base != protocol.ScopeBase_None {
		t.Errorf("cancel = %v, want none like the creator's base", base)
	}
}

// The visibility rank clamps against the creator's VISIBILITY rank, not its
// action base: a creator that acts on its subtree but sees only itself must not
// hand out sight of that subtree.
func TestAttenuateScopeVisibilityClampsToTheCreatorsVisibility(t *testing.T) {
	h, _, c, _, _ := scopeFixture(t)
	cid := bindPrincipal(t, h, c)
	setScope(t, h, c, Scope{Base: protocol.ScopeBase_Subtree, VisBasePresent: true, VisBase: protocol.ScopeBase_None})

	got, _, ok := h.attenuateScope(cid, Scope{Base: protocol.ScopeBase_Subtree}, protocol.Capability_None)
	if !ok || got.VisRank() != protocol.ScopeBase_None {
		t.Errorf("visibility rank = %v ok=%v, want none", got.VisRank(), ok)
	}
	if got.Base != protocol.ScopeBase_Subtree {
		t.Errorf("action base = %v, want subtree (the creator's own)", got.Base)
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
	h.Tasks.Assign(u, testRunnerID("runner-x"), "/wt/u", false)
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
