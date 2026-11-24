package server

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

var taskCallerSeq int

// taskCaller binds a fake connection to taskHex and gives that task the
// delegation bound dcaps, so a re-grant it makes has something to hand down.
func taskCaller(t *testing.T, h *TaskHandler, taskHex string, dcaps protocol.Capability) *fakeConn {
	t.Helper()
	taskCallerSeq++
	cid := objproto.MustParseConnectionID(fmt.Sprintf("ws:127.0.0.1:9800-%d", taskCallerSeq))
	if h.principals == nil {
		h.principals = make(map[objproto.ConnectionID]protocol.TaskID)
	}
	h.principals[cid] = hexToTaskID(t, taskHex)
	if _, ok := h.Tasks.SetCaps(taskHex, GrantUpdate{DelegateCaps: true, To: authority.Grant{DelegateCaps: dcaps}}, AuthorityWrite{}); !ok {
		t.Fatalf("set bound on %s", taskHex)
	}
	return &fakeConn{id: cid}
}

func capsReq(t *testing.T, target string, caps protocol.Capability) protocol.SetCapsRequest {
	t.Helper()
	req := protocol.SetCapsRequest{TaskId: hexToTaskID(t, target), Caps: caps}
	req.SetCapsPresent(true)
	return req
}

// R2: a task's live re-grant of a descendant computes exactly what a
// re-granting resume computes (grantFor with the caller as granter), including
// the clamp of bits past the caller's delegate_caps (Review Focus 2).
func TestTaskSetCapsMatchesResumeRegrant(t *testing.T) {
	h, _, c, g, _ := scopeFixture(t)
	conn := taskCaller(t, h, c, protocol.Capability_FileRead|protocol.Capability_Cancel)
	requested := protocol.Capability_FileRead | protocol.Capability_Spawn

	cid := conn.ConnectionID()
	want, err := h.grantFor(h.granter(cid, h.callerCaps(cid)),
		grantRequest{Caps: requested, CapsPresent: true}, g)
	if err != nil {
		t.Fatal(err)
	}
	got := sendSetCaps(t, h, conn, capsReq(t, g, requested))
	if got.Status != protocol.SetCapsStatus_Ok {
		t.Fatalf("status = %v (%s), want ok", got.Status, got.Message)
	}
	after, _ := h.Tasks.Get(g)
	if after.Capabilities != want.To.Caps {
		t.Fatalf("caps = %v, want the resume re-grant's %v", after.Capabilities, want.To.Caps)
	}
	if after.Capabilities != protocol.Capability_FileRead {
		t.Fatalf("caps = %v: spawn is past the caller's delegate and must be clamped", after.Capabilities)
	}
}

// R1/R4: self, an ancestor, a sibling and an unrelated task are all not_found,
// even when the caller's resume scope reaches them (Review Focus 1).
func TestTaskSetCapsRefusesSelfAncestorSiblingNonDescendant(t *testing.T) {
	h, p, c, _, u := scopeFixture(t)
	s := h.Tasks.Create("/r", "s", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		hexToTaskID(t, p), "", protocol.RunnerSelector{}, nil,
		authority.Grant{Caps: protocol.Capability_Cancel}, "")
	setScope(t, h, c, authority.Scope{Base: protocol.ScopeBase_Global})
	conn := taskCaller(t, h, c, protocol.Capability_All)
	for name, target := range map[string]string{"self": c, "ancestor": p, "sibling": s, "unrelated": u} {
		before, _ := h.Tasks.Get(target)
		got := sendSetCaps(t, h, conn, capsReq(t, target, protocol.Capability_None))
		if got.Status != protocol.SetCapsStatus_NotFound {
			t.Fatalf("%s: status = %v, want not_found", name, got.Status)
		}
		if after, _ := h.Tasks.Get(target); after.Capabilities != before.Capabilities {
			t.Fatalf("%s: caps changed %v → %v", name, before.Capabilities, after.Capabilities)
		}
	}
}

// R1: the resume bit over the target is required; a descendant out of the
// resume scope is not_found like any other target out of reach.
func TestTaskSetCapsNeedsResumeOverTarget(t *testing.T) {
	h, _, c, g, _ := scopeFixture(t)
	conn := taskCaller(t, h, c, protocol.Capability_All)
	if _, ok := h.Tasks.SetCaps(c, GrantUpdate{Caps: true, To: authority.Grant{Caps: protocol.Capability_All &^ protocol.Capability_Resume}}, AuthorityWrite{}); !ok {
		t.Fatal("drop resume")
	}
	if got := sendSetCaps(t, h, conn, capsReq(t, g, protocol.Capability_None)); got.Status != protocol.SetCapsStatus_NotFound {
		t.Fatalf("status = %v, want not_found without resume", got.Status)
	}
}

// R4 / Review Focus 3: a scope id outside the caller's own reach is refused
// with invalid_scope and the authority package's text in message.
func TestTaskSetCapsRefusesScopePastOwnReach(t *testing.T) {
	h, _, c, g, u := scopeFixture(t)
	conn := taskCaller(t, h, c, protocol.Capability_All)
	req := protocol.SetCapsRequest{TaskId: hexToTaskID(t, g)}
	req.SetScopePresent(true)
	req.Scope = authority.Scope{Base: protocol.ScopeBase_Subtree, IDs: []string{u}}.ToWire()
	got := sendSetCaps(t, h, conn, req)
	if got.Status != protocol.SetCapsStatus_InvalidScope {
		t.Fatalf("status = %v, want invalid_scope", got.Status)
	}
	if !strings.Contains(string(got.Message), u[:8]) {
		t.Fatalf("message %q does not name the refused id", got.Message)
	}
}

// R8: a descendant credential is re-grantable, but D14's masked bits stay
// refused.
func TestTaskSetCapsCredentialMaskedCap(t *testing.T) {
	h, _, c, _, _ := scopeFixture(t)
	conn := taskCaller(t, h, c, protocol.Capability_All)
	var pub [32]byte
	pub[0] = 9
	cred, err := h.Tasks.CreateCredential(hexToTaskID(t, c), protocol.ClientKind_Agent,
		authority.Grant{Caps: protocol.Capability_Cancel}, pub, time.Now().Add(time.Hour), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := sendSetCaps(t, h, conn, capsReq(t, cred, protocol.Capability_ScheduleEdit)); got.Status != protocol.SetCapsStatus_MaskedCap {
		t.Fatalf("status = %v, want masked_cap", got.Status)
	}
}

// R7: the write is stamped task_caps_set with the caller as actor.
func TestTaskSetCapsProvenance(t *testing.T) {
	h, _, c, g, _ := scopeFixture(t)
	conn := taskCaller(t, h, c, protocol.Capability_FileRead)
	if got := sendSetCaps(t, h, conn, capsReq(t, g, protocol.Capability_FileRead)); got.Status != protocol.SetCapsStatus_Ok {
		t.Fatalf("status = %v", got.Status)
	}
	after, _ := h.Tasks.Get(g)
	if after.AuthoritySource != protocol.AuthoritySource_TaskCapsSet || after.AuthorityActor != hexToTaskID(t, c) {
		t.Fatalf("provenance = %v/%x, want task_caps_set by c", after.AuthoritySource, after.AuthorityActor.Id)
	}
}

// R5/R6: --cascade from a task clamps the target's descendants, and a
// narrowing tears down the narrowed tasks' connections, as for the operator.
func TestTaskSetCapsCascadeAndTeardown(t *testing.T) {
	h, p, c, g, _ := scopeFixture(t)
	conn := taskCaller(t, h, p, protocol.Capability_FileRead)
	dropped := map[string]bool{}
	h.DropConnsForPrincipal = func(id string) int { dropped[id] = true; return 1 }
	req := capsReq(t, c, protocol.Capability_FileRead)
	req.SetCascade(true)
	got := sendSetCaps(t, h, conn, req)
	if got.Status != protocol.SetCapsStatus_Ok || len(got.Affected) != 2 {
		t.Fatalf("status = %v affected = %d, want ok and 2", got.Status, len(got.Affected))
	}
	if ge, _ := h.Tasks.Get(g); hasCap(ge.Capabilities, protocol.Capability_Cancel) {
		t.Fatal("the grandchild kept cancel after a cascading narrowing")
	}
	if !dropped[c] || !dropped[g] {
		t.Fatalf("dropped = %v, want c and g", dropped)
	}
}

// Final review #2: a re-grant (live or resume) only ever TIGHTENS a target's
// spawn depth, so an ancestor cannot lift its own depth limit by widening a
// grandchild's.
func TestTaskSetCapsCannotWidenSpawnDepth(t *testing.T) {
	h, p, c, g, _ := scopeFixture(t)
	if _, ok := h.Tasks.SetCaps(p, GrantUpdate{SpawnDepth: true, To: authority.Grant{SpawnDepth: authority.LimitOf(2)}}, AuthorityWrite{}); !ok {
		t.Fatal("limit p")
	}
	if _, ok := h.Tasks.SetCaps(g, GrantUpdate{SpawnDepth: true, To: authority.Grant{SpawnDepth: authority.LimitOf(0)}}, AuthorityWrite{}); !ok {
		t.Fatal("limit g")
	}
	_ = c
	conn := taskCaller(t, h, p, protocol.Capability_All)
	req := protocol.SetCapsRequest{TaskId: hexToTaskID(t, g), SpawnDepth: authority.LimitOf(1)}
	req.SetSpawnDepthPresent(true)
	if got := sendSetCaps(t, h, conn, req); got.Status != protocol.SetCapsStatus_Ok {
		t.Fatalf("status = %v", got.Status)
	}
	if e, _ := h.Tasks.Get(g); !e.SpawnDepth.Limited() || e.SpawnDepth.Value() != 0 {
		t.Fatalf("g spawn depth = %+v, want still 0 (a re-grant only tightens it)", e.SpawnDepth)
	}
}
