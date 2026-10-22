package server

import (
	"encoding/hex"
	"reflect"
	"testing"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func storeAsAuthorityTasks(s *TaskStore) []authority.Task {
	var out []authority.Task
	for _, e := range s.List(0) {
		t := authority.Task{ID: e.ID, Caps: e.Capabilities, Scope: e.Scope,
			DelegateCaps: e.DelegateCaps, DelegateScope: e.DelegateScope}
		if e.CreatorTaskID.Id != ([16]byte{}) {
			t.Creator = hex.EncodeToString(e.CreatorTaskID.Id[:])
		}
		out = append(out, t)
	}
	return out
}

// normGrant makes an empty id list and a nil one the same value, which they
// are as authority, so DeepEqual compares what the grant means.
func normGrant(g authority.Grant) authority.Grant {
	norm := func(s authority.Scope) authority.Scope {
		if len(s.IDs) == 0 {
			s.IDs = nil
		}
		if len(s.VisIDs) == 0 {
			s.VisIDs = nil
		}
		if len(s.Overrides) == 0 {
			s.Overrides = nil
		}
		for i := range s.Overrides {
			if len(s.Overrides[i].IDs) == 0 {
				s.Overrides[i].IDs = nil
			}
		}
		return s
	}
	g.Scope, g.DelegateScope = norm(g.Scope), norm(g.DelegateScope)
	return g
}

// The what-if of a caps set is what the caps set then stores, on the target
// and on every descendant the cascade reaches, and nothing outside the diff
// moves.
func TestWhatIfAgreesWithSetCaps(t *testing.T) {
	cases := []struct {
		name    string
		caps    *protocol.Capability
		scope   *authority.Scope
		dcaps   *protocol.Capability
		dscope  *authority.Scope
		cascade bool
	}{
		{name: "caps --cascade", caps: capsPtr(protocol.Capability_FileRead | protocol.Capability_Cancel), cascade: true},
		{name: "scope --cascade", scope: scopePtr(authority.Scope{Base: protocol.ScopeBase_None}), cascade: true},
		{name: "bound --cascade", dcaps: capsPtr(protocol.Capability_FileRead), dscope: scopePtr(authority.Scope{}), cascade: true},
		{name: "no cascade", caps: capsPtr(protocol.Capability_FileRead)},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, p, c, g, u := scopeFixture(t)
			wide := authority.Grant{
				Caps:          protocol.Capability_FileRead | protocol.Capability_Cancel | protocol.Capability_Spawn,
				Scope:         authority.Scope{Base: protocol.ScopeBase_Subtree, VisIDs: []string{u}},
				DelegateCaps:  protocol.Capability_FileRead | protocol.Capability_Cancel,
				DelegateScope: authority.Scope{Base: protocol.ScopeBase_Subtree, VisIDs: []string{u}},
			}
			all := GrantUpdate{Caps: true, Scope: true, DelegateCaps: true, DelegateScope: true, To: wide}
			for _, id := range []string{p, c, g} {
				h.Tasks.SetCaps(id, all, AuthorityWrite{})
			}

			before := map[string]authority.Grant{}
			for _, e := range h.Tasks.List(0) {
				before[e.ID] = normGrant(e.Grant())
			}
			diffs, err := authority.WhatIf(storeAsAuthorityTasks(h.Tasks),
				authority.Regrant{Target: p, Caps: tc.caps, Scope: tc.scope, DelegateCaps: tc.dcaps, DelegateScope: tc.dscope, Cascade: tc.cascade})
			if err != nil {
				t.Fatal(err)
			}

			req := boundReq(t, p, tc.dcaps, tc.dscope)
			if tc.caps != nil {
				req.Caps = *tc.caps
				req.SetCapsPresent(true)
			}
			if tc.scope != nil {
				req.Scope = tc.scope.ToWire()
				req.SetScopePresent(true)
			}
			req.SetCascade(tc.cascade)
			if got := sendSetCaps(t, h, opConnAt(string(rune('0'+i))+"990"), req); got.Status != protocol.SetCapsStatus_Ok {
				t.Fatalf("status = %v", got.Status)
			}

			inDiff := map[string]bool{}
			for _, d := range diffs {
				inDiff[d.ID] = true
				e, _ := h.Tasks.Get(d.ID)
				if want, got := normGrant(d.After), normGrant(e.Grant()); !reflect.DeepEqual(want, got) {
					t.Errorf("%s: what-if drew %+v, the server stored %+v", d.ID[:4], want, got)
				}
			}
			for _, e := range h.Tasks.List(0) {
				if !inDiff[e.ID] && !reflect.DeepEqual(before[e.ID], normGrant(e.Grant())) {
					t.Errorf("%s changed outside the what-if: %+v → %+v", e.ID[:4], before[e.ID], e.Grant())
				}
			}
			if !inDiff[p] {
				t.Errorf("the target is missing from the diff: %+v", diffs)
			}
			if tc.cascade && !inDiff[c] {
				t.Errorf("the cascade drew nothing on the child: %+v", diffs)
			}
		})
	}
}
