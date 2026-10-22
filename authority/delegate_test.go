package authority

import (
	"errors"
	"reflect"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The tree every delegation test grants over: P created S, S created G, and X
// is unrelated to all three — reachable by P only through its explicit ids.
var (
	delegP = hexID(1)
	delegS = hexID(2)
	delegX = hexID(3)
	delegG = hexID(4)

	delegTree = map[string][]string{delegP: {delegS}, delegS: {delegG}}
)

const (
	fileRead = protocol.Capability_FileRead
	cancel   = protocol.Capability_Cancel
)

// granterP is P holding spawn,file_read,cancel over subtree+ids:X, with the
// given bound.
func granterP(delegateCaps protocol.Capability, ceiling Scope) *Granter {
	return NewGranter(delegP, Grant{
		Caps:          protocol.Capability_Spawn | fileRead | cancel,
		Scope:         Scope{Base: protocol.ScopeBase_Subtree, IDs: []string{delegX}},
		DelegateCaps:  delegateCaps,
		DelegateScope: ceiling,
	}, delegTree)
}

func TestChildCapsTable(t *testing.T) {
	requests := []protocol.Capability{protocol.Capability_None, fileRead, fileRead | cancel, protocol.Capability_All}
	for _, tc := range []struct {
		bound protocol.Capability
		want  []protocol.Capability
	}{
		{protocol.Capability_None, []protocol.Capability{0, 0, 0, 0}},
		{fileRead, []protocol.Capability{0, fileRead, fileRead, fileRead}},
	} {
		g := granterP(tc.bound, Scope{})
		for i, req := range requests {
			if got := g.ChildCaps(req); got != tc.want[i] {
				t.Errorf("bound %s, requested %s: got %s, want %s", CapsLabel(tc.bound), CapsLabel(req), CapsLabel(got), CapsLabel(tc.want[i]))
			}
		}
	}
	var operator *Granter
	if got := operator.ChildCaps(protocol.Capability_All); got != protocol.Capability_All {
		t.Errorf("operator requested all: got %s, want all", CapsLabel(got))
	}
}

// D13: the bound is kept as written and intersected at use, so a bit the
// holder lacks is not delegated, and granting the bit back re-enables it.
func TestChildCapsBoundNamesBitHolderLacks(t *testing.T) {
	held := Grant{Caps: fileRead, DelegateCaps: cancel}
	if got := NewGranter(delegP, held, delegTree).ChildCaps(cancel); got != 0 {
		t.Errorf("holder without cancel delegated %s", CapsLabel(got))
	}
	held.Caps |= cancel
	if got := NewGranter(delegP, held, delegTree).ChildCaps(cancel); got != cancel {
		t.Errorf("holder granted cancel back: got %s, want cancel", CapsLabel(got))
	}
}

func TestChildScopeVisClampedToCeiling(t *testing.T) {
	g := NewGranter(delegP, Grant{
		Caps:  fileRead,
		Scope: Scope{Base: protocol.ScopeBase_Subtree, VisBase: protocol.ScopeBase_Global, VisBasePresent: true},
	}, delegTree)
	got, err := g.ChildScope(Scope{Base: protocol.ScopeBase_Subtree, VisBase: protocol.ScopeBase_Global, VisBasePresent: true}, fileRead)
	if err != nil {
		t.Fatal(err)
	}
	if got.Base != protocol.ScopeBase_Subtree || got.VisBasePresent || got.VisBase != 0 {
		t.Errorf("got %+v, want plain subtree (visibility clamped to the ceiling, canonical encoding)", got)
	}
}

// wantDelegRefused asserts err is the ceiling's refusal naming id in field.
func wantDelegRefused(t *testing.T, what string, err error, id, field string) {
	t.Helper()
	var od *OutOfDelegationError
	if !errors.As(err, &od) || od.ID != id || od.Field != field {
		t.Errorf("%s: err = %v, want OutOfDelegationError{%s, %s}", what, err, id, field)
	}
}

func childScope(t *testing.T, g *Granter, s Scope) error {
	t.Helper()
	_, err := g.ChildScope(s, fileRead)
	return err
}

func TestChildScopeDefaultCeilingIDs(t *testing.T) {
	g := granterP(fileRead, Scope{})
	if err := childScope(t, g, Scope{Base: protocol.ScopeBase_None, IDs: []string{delegP}}); err != nil {
		t.Errorf("ids:<P>: %v", err)
	}
	if err := childScope(t, g, Scope{Base: protocol.ScopeBase_None, IDs: []string{delegS}}); err != nil {
		t.Errorf("ids:<S>: %v", err)
	}
	wantDelegRefused(t, "ids:X", childScope(t, g, Scope{Base: protocol.ScopeBase_None, IDs: []string{delegX}}), delegX, "scope")
}

// D22: a descendants ceiling keeps P's own id out of the action ids it hands
// down, never out of the view-only ones.
func TestChildScopeDescendantsCeiling(t *testing.T) {
	g := granterP(fileRead, Scope{Base: protocol.ScopeBase_Subtree, ExcludeSelf: true})
	wantDelegRefused(t, "ids:<P>", childScope(t, g, Scope{Base: protocol.ScopeBase_None, IDs: []string{delegP}}), delegP, "scope")
	if err := childScope(t, g, Scope{Base: protocol.ScopeBase_None, VisIDs: []string{delegP}}); err != nil {
		t.Errorf("vis-ids:<P>: %v", err)
	}
	if err := childScope(t, g, Scope{Base: protocol.ScopeBase_None, IDs: []string{delegS}}); err != nil {
		t.Errorf("ids:<S>: %v", err)
	}
}

func TestChildBoundNoneCeilingForcesExcludeSelf(t *testing.T) {
	g := granterP(fileRead, Scope{Base: protocol.ScopeBase_None, IDs: []string{delegX}})
	for _, id := range []string{delegP, delegX} {
		if err := childScope(t, g, Scope{Base: protocol.ScopeBase_None, IDs: []string{id}}); err != nil {
			t.Errorf("ids:%s: %v", id[:2], err)
		}
	}
	wantDelegRefused(t, "ids:<S>", childScope(t, g, Scope{Base: protocol.ScopeBase_None, IDs: []string{delegS}}), delegS, "scope")
	_, bound, err := g.ChildBound(0, Scope{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !bound.ExcludeSelf {
		t.Error("a new child outside a none ceiling's action ids must get exclude_self forced on")
	}
}

func TestChildScopeCeilingIDLists(t *testing.T) {
	ids := func(id string) Scope { return Scope{Base: protocol.ScopeBase_None, IDs: []string{id}} }
	vis := func(id string) Scope { return Scope{Base: protocol.ScopeBase_None, VisIDs: []string{id}} }

	if err := childScope(t, granterP(fileRead, Scope{IDs: []string{delegX}}), ids(delegX)); err != nil {
		t.Errorf("ceiling subtree+ids:X, ids:X: %v", err)
	}
	g := granterP(fileRead, Scope{VisIDs: []string{delegX}})
	if err := childScope(t, g, vis(delegX)); err != nil {
		t.Errorf("ceiling subtree+vis-ids:X, vis-ids:X: %v", err)
	}
	wantDelegRefused(t, "ceiling subtree+vis-ids:X, ids:X", childScope(t, g, ids(delegX)), delegX, "scope")
	if err := childScope(t, granterP(fileRead, Scope{Base: protocol.ScopeBase_Global}), ids(delegX)); err != nil {
		t.Errorf("ceiling global, ids:X: %v", err)
	}
	over := Scope{Base: protocol.ScopeBase_Subtree, Overrides: []ScopeOverride{{Caps: fileRead, Base: protocol.ScopeBase_None, IDs: []string{delegX}}}}
	wantDelegRefused(t, "override ids:X under a subtree ceiling", childScope(t, granterP(fileRead, Scope{}), over), delegX, "scope")
}

// The ceiling's two id sets are Reach over C around P, not a second reading
// of it: with P's own scope global (so only the ceiling can refuse), what the
// ceiling admits is exactly ActionSet ∩ VisibleSet and VisibleSet over C.
func TestCeilingSetsAreReachOverCeiling(t *testing.T) {
	tree := map[string][]string{delegP: {delegS}, delegS: {delegG}, delegX: {hexID(5)}}
	all := []string{delegP, delegS, delegG, delegX, hexID(5)}
	for _, c := range []Scope{
		{},
		{Base: protocol.ScopeBase_Subtree, ExcludeSelf: true},
		{Base: protocol.ScopeBase_None, IDs: []string{delegX}, VisIDs: []string{hexID(5)}},
		{Base: protocol.ScopeBase_Global},
	} {
		g := NewGranter(delegP, Grant{Caps: fileRead, Scope: Scope{Base: protocol.ScopeBase_Global}, DelegateScope: c}, tree)
		actAll, act := ActionSet(delegP, c, fileRead, tree)
		visAll, visible := VisibleSet(delegP, c, tree)
		for _, id := range all {
			wantVis := visAll || visible[id]
			wantAct := wantVis && (actAll || act[id])
			_, errAct := g.ChildScope(Scope{Base: protocol.ScopeBase_None, IDs: []string{id}}, fileRead)
			_, errVis := g.ChildScope(Scope{Base: protocol.ScopeBase_None, VisIDs: []string{id}}, fileRead)
			if (errAct == nil) != wantAct {
				t.Errorf("C=%s id %s: action admitted=%v, want %v", c, id[:2], errAct == nil, wantAct)
			}
			if (errVis == nil) != wantVis {
				t.Errorf("C=%s id %s: view-only admitted=%v, want %v", c, id[:2], errVis == nil, wantVis)
			}
		}
	}
}

func TestChildBoundClampsToGranters(t *testing.T) {
	g := granterP(fileRead, Scope{})
	caps, bound, err := g.ChildBound(fileRead|cancel, Scope{Base: protocol.ScopeBase_Global, VisIDs: []string{delegS}}, "")
	if err != nil {
		t.Fatal(err)
	}
	want := Scope{Base: protocol.ScopeBase_Subtree, VisIDs: []string{delegS}}
	if caps != fileRead || !reflect.DeepEqual(bound, want) {
		t.Errorf("child bound = %s / %+v, want file_read / %+v", CapsLabel(caps), bound, want)
	}

	// A child spawned with the default bound delegates nothing further, even
	// when its own child asks for everything.
	child := NewGranter(delegS, Grant{Caps: g.ChildCaps(protocol.Capability_All)}, delegTree)
	if got := child.ChildCaps(protocol.Capability_All); got != 0 {
		t.Errorf("grandchild of a default-bound child holds %s, want none", CapsLabel(got))
	}
}

func TestChildBoundRefusesOverrides(t *testing.T) {
	_, _, err := granterP(fileRead, Scope{}).ChildBound(0, Scope{Overrides: []ScopeOverride{{Caps: fileRead, Base: protocol.ScopeBase_None}}}, "")
	var inv *InvalidScopeError
	if !errors.As(err, &inv) {
		t.Errorf("err = %v, want InvalidScopeError", err)
	}
}

func TestClampDescendantDropsInsteadOfRefusing(t *testing.T) {
	g := NewGranter(delegP, Grant{
		Caps:         fileRead | cancel,
		Scope:        Scope{Base: protocol.ScopeBase_Subtree, IDs: []string{delegX}},
		DelegateCaps: fileRead,
	}, delegTree)
	d := Grant{
		Caps:          fileRead | cancel,
		Scope:         Scope{Base: protocol.ScopeBase_Subtree, IDs: []string{delegX}},
		DelegateCaps:  fileRead | cancel,
		DelegateScope: Scope{Base: protocol.ScopeBase_Subtree, VisIDs: []string{delegX}},
	}
	got := g.ClampDescendant(d, delegS)
	want := Grant{
		Caps:          fileRead,
		Scope:         Scope{Base: protocol.ScopeBase_Subtree, IDs: []string{}},
		DelegateCaps:  fileRead,
		DelegateScope: Scope{Base: protocol.ScopeBase_Subtree, VisIDs: []string{}},
	}
	if got.Caps != want.Caps || got.DelegateCaps != want.DelegateCaps ||
		len(got.Scope.IDs) != 0 || len(got.DelegateScope.VisIDs) != 0 || got.DelegateScope.ExcludeSelf {
		t.Errorf("ClampDescendant = %+v, want %+v", got, want)
	}
	if out := g.ClampDescendant(d, delegX); !out.DelegateScope.ExcludeSelf {
		t.Error("a descendant outside T's delegable action ids must get exclude_self forced on its bound")
	}
}

func TestDelegationErrorTexts(t *testing.T) {
	id := hexID(10)
	if got, want := (&OutOfDelegationError{ID: id, Field: "scope"}).Error(),
		"scope id "+id+" is outside what you may delegate (see whoami: delegate-scope)"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := (&OutOfDelegationError{ID: id, Field: "delegate-scope"}).Error(),
		"delegate-scope id "+id+" is outside what you may delegate (see whoami: delegate-scope)"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got, want := (&OutOfReachError{ID: id}).Error(), "scope id "+id+" is outside your own scope"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
