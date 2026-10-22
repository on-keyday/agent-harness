package authority

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The what-if tree: the Phase 3 tree (O global root, P under O, C under P,
// X a second root) plus G under C. P and C hold cancel over their subtrees.
var viewG = hexID(0x9)

func whatIfTree() []Task {
	p := Task{ID: viewP, Creator: viewO, Caps: cancel | fileRead | protocol.Capability_Spawn,
		DelegateCaps: cancel, DelegateScope: Scope{}}
	c := Task{ID: viewC, Creator: viewP, Caps: cancel | protocol.Capability_Spawn, DelegateCaps: cancel}
	g := Task{ID: viewG, Creator: viewC, Caps: cancel, DelegateCaps: cancel}
	return []Task{
		{ID: viewO, Caps: protocol.Capability_All, Scope: Scope{Base: protocol.ScopeBase_Global}},
		p, c, g,
		{ID: viewX, Caps: fileRead},
	}
}

func capsPtr(c protocol.Capability) *protocol.Capability { return &c }

func diffFor(t *testing.T, diffs []TaskDiff, id string) TaskDiff {
	t.Helper()
	for _, d := range diffs {
		if d.ID == id {
			return d
		}
	}
	t.Fatalf("no diff for %s in %+v", id[:2], diffs)
	return TaskDiff{}
}

func hasDiff(diffs []TaskDiff, id string) bool {
	for _, d := range diffs {
		if d.ID == id {
			return true
		}
	}
	return false
}

func TestCascadeClampMatchesTheOldLoop(t *testing.T) {
	tasks := whatIfTree()
	v := newView(tasks)
	target := Task{ID: viewP, Creator: viewO, Caps: fileRead, Scope: Scope{Base: protocol.ScopeBase_None}, DelegateCaps: 0}
	d := Grant{Caps: cancel | fileRead, Scope: Scope{Base: protocol.ScopeBase_Subtree}, DelegateCaps: cancel}

	got := CascadeClamp(target, d, viewC, false, v.children)
	wantCaps := d.Caps & target.Caps
	wantScope := ClampUnder(d.Scope, wantCaps, NewReach(target.ID, target.Scope, v.children))
	if got.Caps != wantCaps || !sameScope(got.Scope, wantScope) || got.DelegateCaps != cancel {
		t.Errorf("caps-only branch = %+v", got)
	}

	got = CascadeClamp(target, d, viewC, true, v.children)
	want := NewGranter(target.ID, target.grant(), v.children).ClampDescendant(d, viewC)
	if got.Caps != want.Caps || !sameScope(got.Scope, want.Scope) || got.DelegateCaps != want.DelegateCaps || !sameScope(got.DelegateScope, want.DelegateScope) {
		t.Errorf("bound branch = %+v, want %+v", got, want)
	}
}

// Review Focus 1: a caps-only cascade clamps caps and scope and leaves bounds;
// a cascade that writes a bound clamps the bounds too; without --cascade only
// the target changes.
func TestWhatIfRegrantCascade(t *testing.T) {
	diffs, err := WhatIf(whatIfTree(), Regrant{Target: viewP, Caps: capsPtr(fileRead), Cascade: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{viewC, viewG} {
		d := diffFor(t, diffs, id)
		if d.After.Caps&cancel != 0 || d.After.DelegateCaps != d.Before.DelegateCaps || len(d.Removed[cancel]) == 0 {
			t.Errorf("%s after a caps-only cascade = %+v", id[:2], d)
		}
	}

	diffs, err = WhatIf(whatIfTree(), Regrant{Target: viewP, Caps: capsPtr(fileRead), DelegateCaps: capsPtr(0), Cascade: true})
	if err != nil {
		t.Fatal(err)
	}
	if d := diffFor(t, diffs, viewC); d.After.DelegateCaps != 0 {
		t.Errorf("a bound cascade left C's bound %s", CapsLabel(d.After.DelegateCaps))
	}

	diffs, err = WhatIf(whatIfTree(), Regrant{Target: viewP, Caps: capsPtr(fileRead)})
	if err != nil {
		t.Fatal(err)
	}
	if hasDiff(diffs, viewC) || hasDiff(diffs, viewG) || !hasDiff(diffs, viewP) {
		t.Errorf("without --cascade only P changes, got %+v", diffs)
	}
}

// Review Focus 2: a spawn the server would refuse says why, in its words.
func TestWhatIfSpawnRefused(t *testing.T) {
	_, err := WhatIf(whatIfTree(), Spawn{Creator: viewP, Scope: Scope{Base: protocol.ScopeBase_None, IDs: []string{viewX}}})
	if err == nil || !strings.Contains(err.Error(), "outside your own scope") {
		t.Errorf("err = %v, want the own-scope refusal", err)
	}
	tasks := whatIfTree()
	tasks[1].Scope = Scope{Base: protocol.ScopeBase_Subtree, IDs: []string{viewX}}
	_, err = WhatIf(tasks, Spawn{Creator: viewP, Scope: Scope{Base: protocol.ScopeBase_None, IDs: []string{viewX}}})
	if err == nil || !strings.Contains(err.Error(), "outside what you may delegate") {
		t.Errorf("err = %v, want the bound refusal", err)
	}
}

// Review Focus 3: a spawn adds the phantom to every ancestor whose subtree now
// holds it.
func TestWhatIfSpawnAncestorsGainThePhantom(t *testing.T) {
	diffs, err := WhatIf(whatIfTree(), Spawn{Creator: viewC, Caps: cancel})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{viewP, viewC} {
		if d := diffFor(t, diffs, id); !contains(d.Added[cancel], PhantomID) {
			t.Errorf("%s does not gain the phantom: %+v", id[:2], d)
		}
	}
	if hasDiff(diffs, viewO) {
		t.Error("O is global already; the phantom changes nothing for it")
	}
	if d := diffFor(t, diffs, PhantomID); !d.New || d.After.Caps != cancel {
		t.Errorf("phantom = %+v", d)
	}
}

// Review Focus 4: a reparent into the target's own subtree is a cycle.
func TestWhatIfReparentCycle(t *testing.T) {
	if _, err := WhatIf(whatIfTree(), Reparent{Target: viewP, NewParent: viewG}); err == nil || !strings.Contains(err.Error(), "its own ancestor") {
		t.Errorf("err = %v, want a cycle refusal", err)
	}
	diffs, err := WhatIf(whatIfTree(), Reparent{Target: viewG})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{viewP, viewC} {
		if d := diffFor(t, diffs, id); !contains(d.Removed[cancel], viewG) {
			t.Errorf("%s keeps G after G moved to the root: %+v", id[:2], d)
		}
	}
}

func TestWhatIfNoChange(t *testing.T) {
	diffs, err := WhatIf(whatIfTree(), Regrant{Target: viewP})
	if err != nil || len(diffs) != 0 {
		t.Errorf("an empty regrant = %+v, %v; want nothing", diffs, err)
	}
}

// The wire carries no overrides on a bound, so a what-if must not draw one.
func TestWhatIfRegrantRefusesBoundOverrides(t *testing.T) {
	bound := Scope{Overrides: []ScopeOverride{{Caps: cancel, Base: protocol.ScopeBase_Subtree}}}
	if _, err := WhatIf(whatIfTree(), Regrant{Target: viewP, DelegateScope: &bound}); err == nil {
		t.Error("a bound with overrides was accepted")
	}
}
