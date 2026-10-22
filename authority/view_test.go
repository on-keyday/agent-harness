package authority

import (
	"reflect"
	"sort"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The tree every view test reads: O is an operator-created root with a global
// scope, P sits under O with a subtree scope, C under P, and X is a second
// operator root.
var (
	viewO = hexID(0xa)
	viewP = hexID(0xb)
	viewC = hexID(0xc)
	viewX = hexID(0xd)
)

func viewTree(p Task, c Task) []Task {
	return []Task{
		{ID: viewO, Caps: protocol.Capability_All, Scope: Scope{Base: protocol.ScopeBase_Global}},
		p, c,
		{ID: viewX, Caps: fileRead},
	}
}

func viewP0() Task {
	return Task{ID: viewP, Creator: viewO, Caps: cancel | fileRead}
}
func viewC0() Task { return Task{ID: viewC, Creator: viewP, Caps: cancel} }

func sorted(ids ...string) []string {
	out := append([]string{}, ids...)
	sort.Strings(out)
	return out
}

func wantIDs(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %v, want %v", what, got, want)
	}
}

func TestReachOfPerBit(t *testing.T) {
	tasks := viewTree(viewP0(), viewC0())
	r := ReachOf(tasks, viewP, cancel)
	wantIDs(t, "P cancel act", r.Act, sorted(viewC, viewP))
	wantIDs(t, "P cancel see", r.See, nil)
	if r.ActAll {
		t.Error("a subtree scope reported global")
	}
	if r := ReachOf(tasks, viewP, protocol.Capability_ExecView); len(r.Act) != 0 || r.ActAll {
		t.Errorf("P reaches through a bit it does not hold: %+v", r)
	}
	if r := ReachOf(tasks, viewO, cancel); !r.ActAll || len(r.Act) != 0 {
		t.Errorf("a global root = %+v, want ActAll and no listed ids", r)
	}
	if r := ReachOf(tasks, hexID(0xe), cancel); !reflect.DeepEqual(r, ReachResult{}) {
		t.Errorf("an unlisted task = %+v, want the zero value", r)
	}
}

func TestReachOfView(t *testing.T) {
	r := ReachOf(viewTree(viewP0(), viewC0()), viewP, protocol.Capability_None)
	wantIDs(t, "view act", r.Act, nil)
	wantIDs(t, "view see", r.See, sorted(viewC, viewP))

	p := viewP0()
	p.Scope.VisIDs = []string{viewX}
	r = ReachOf(viewTree(p, viewC0()), viewP, protocol.Capability_None)
	wantIDs(t, "view see with vis-ids", r.See, sorted(viewC, viewP, viewX))
}

// V14: a task acts through every bit it exercises, each under that bit's own
// scope.
func TestReachOfExercisedBits(t *testing.T) {
	p := Task{ID: viewP, Creator: viewO, Caps: protocol.Capability_ExecControl, Scope: Scope{
		Base:      protocol.ScopeBase_None,
		Overrides: []ScopeOverride{{Caps: protocol.Capability_ExecView, Base: protocol.ScopeBase_None, IDs: []string{viewX}}},
	}}
	tasks := viewTree(p, viewC0())
	wantIDs(t, "exec_view act", ReachOf(tasks, viewP, protocol.Capability_ExecView).Act, sorted(viewP, viewX))
	wantIDs(t, "exec_control act", ReachOf(tasks, viewP, protocol.Capability_ExecControl).Act, []string{viewP})
}

func TestDelegableOfDefaultAndDescendants(t *testing.T) {
	tasks := viewTree(viewP0(), viewC0())
	r := DelegableOf(tasks, viewP)
	wantIDs(t, "default act", r.Act, sorted(viewC, viewP))
	wantIDs(t, "default see", r.See, nil)

	p := viewP0()
	p.DelegateScope = Scope{Base: protocol.ScopeBase_Subtree, ExcludeSelf: true}
	r = DelegableOf(viewTree(p, viewC0()), viewP)
	wantIDs(t, "descendants act", r.Act, []string{viewC})
	wantIDs(t, "descendants see", r.See, []string{viewP})

	// A global ceiling does not let a subtree task hand down what it cannot
	// reach itself: the server checks its own scope first.
	p.DelegateScope = Scope{Base: protocol.ScopeBase_Global}
	r = DelegableOf(viewTree(p, viewC0()), viewP)
	if r.ActAll || r.SeeAll {
		t.Errorf("a global ceiling over a subtree scope = %+v, want its own reach", r)
	}
	wantIDs(t, "global ceiling act", r.Act, sorted(viewC, viewP))
}

// DelegableOf answers exactly what a spawn would be allowed: an id is an
// action id it may hand down iff Granter.ChildScope accepts ids:<id> for a
// child holding everything it may delegate, and a view-only one iff
// vis-ids:<id> is accepted.
func TestDelegableOfAgreesWithChildScope(t *testing.T) {
	scopes := []Scope{
		{}, {Base: protocol.ScopeBase_None}, {Base: protocol.ScopeBase_Global},
		{Base: protocol.ScopeBase_Subtree, IDs: []string{viewX}},
		{Base: protocol.ScopeBase_None, VisIDs: []string{viewX}},
	}
	bounds := []Scope{
		{}, {Base: protocol.ScopeBase_None}, {Base: protocol.ScopeBase_Global},
		{Base: protocol.ScopeBase_Subtree, ExcludeSelf: true},
		{Base: protocol.ScopeBase_None, VisIDs: []string{viewX}},
	}
	for _, own := range scopes {
		for _, bound := range bounds {
			for _, dcaps := range []protocol.Capability{0, cancel, cancel | fileRead} {
				p := Task{ID: viewP, Creator: viewO, Caps: cancel | fileRead, Scope: own, DelegateCaps: dcaps, DelegateScope: bound}
				tasks := viewTree(p, viewC0())
				r := DelegableOf(tasks, viewP)
				g := NewGranter(viewP, Grant{Caps: p.Caps, Scope: own, DelegateCaps: dcaps, DelegateScope: bound}, newView(tasks).children)
				held := g.ChildCaps(protocol.Capability_All)
				for _, x := range []string{viewO, viewP, viewC, viewX} {
					_, errAct := g.ChildScope(Scope{Base: protocol.ScopeBase_None, IDs: []string{x}}, held)
					_, errSee := g.ChildScope(Scope{Base: protocol.ScopeBase_None, VisIDs: []string{x}}, held)
					inAct := r.ActAll || contains(r.Act, x)
					inSee := inAct || r.SeeAll || contains(r.See, x)
					if inAct != (errAct == nil) || inSee != (errSee == nil) {
						t.Errorf("own=%s bound=%s dcaps=%s id=%s: delegable act=%v see=%v, ChildScope act=%v see=%v",
							own, bound, CapsLabel(dcaps), x[:2], inAct, inSee, errAct == nil, errSee == nil)
					}
				}
			}
		}
	}
}

func contains(ids []string, x string) bool {
	for _, id := range ids {
		if id == x {
			return true
		}
	}
	return false
}

func TestWiderThanParentCases(t *testing.T) {
	// An operator granted the child a global scope under a subtree parent.
	c := viewC0()
	c.Scope = Scope{Base: protocol.ScopeBase_Global}
	if w := WiderThanParent(viewTree(viewP0(), c), viewC); w.Bits != cancel || !w.Sees {
		t.Errorf("operator-widened child = %+v, want cancel and sees", w)
	}

	// The parent was narrowed to none after the child was spawned subtree.
	p := viewP0()
	p.Scope = Scope{Base: protocol.ScopeBase_None}
	c = viewC0()
	gc := Task{ID: hexID(0xf), Creator: viewC}
	if w := WiderThanParent(append(viewTree(p, c), gc), viewC); w.Bits != cancel {
		t.Errorf("child of a narrowed parent = %+v, want cancel", w)
	}

	// A swap: the former parent now sits under the former child, and is
	// measured against it. It reaches X by id, which its new parent does not.
	swappedC := Task{ID: viewC, Creator: viewO, Caps: cancel, Scope: Scope{Base: protocol.ScopeBase_None}}
	swappedP := Task{ID: viewP, Creator: viewC, Caps: cancel | fileRead, Scope: Scope{Base: protocol.ScopeBase_Subtree, IDs: []string{viewX}}}
	if w := WiderThanParent(viewTree(swappedP, swappedC), viewP); w.Empty() {
		t.Error("after a swap the former parent is wider than its new parent and is not marked")
	}

	// V16: a child of a none parent acting only on itself is not wider.
	p = viewP0()
	p.Scope = Scope{Base: protocol.ScopeBase_None}
	c = viewC0()
	c.Scope = Scope{Base: protocol.ScopeBase_None}
	if w := WiderThanParent(viewTree(p, c), viewC); !w.Empty() {
		t.Errorf("a child acting on itself = %+v, want empty", w)
	}

	// A child that SEES more than its parent is marked too.
	p = viewP0()
	c = viewC0()
	c.Scope = Scope{Base: protocol.ScopeBase_Subtree, VisBase: protocol.ScopeBase_Global, VisBasePresent: true}
	if w := WiderThanParent(viewTree(p, c), viewC); !w.Sees || w.Bits != 0 {
		t.Errorf("a child seeing globally = %+v, want sees only", w)
	}
}

func TestWiderThanParentNeedsAListedCreator(t *testing.T) {
	c := viewC0()
	c.Creator = hexID(0xe) // pruned, or invisible to the caller
	c.Scope = Scope{Base: protocol.ScopeBase_Global}
	if w := WiderThanParent(viewTree(viewP0(), c), viewC); !w.Empty() {
		t.Errorf("a child whose creator is not listed = %+v, want empty", w)
	}
	if w := WiderThanParent(viewTree(viewP0(), viewC0()), viewO); !w.Empty() {
		t.Errorf("an operator-created root = %+v, want empty", w)
	}
}

// exec_resize is honoured only on an observer attach, which must itself pass
// exec_view's scope: its reach is its own scope inside exec_view's, and nothing
// without exec_view.
func TestReachOfResizeRidesAnAttach(t *testing.T) {
	p := Task{ID: viewP, Creator: viewO, Caps: protocol.Capability_ExecView | protocol.Capability_ExecResize, Scope: Scope{
		Base:      protocol.ScopeBase_Subtree,
		Overrides: []ScopeOverride{{Caps: protocol.Capability_ExecResize, Base: protocol.ScopeBase_Global}},
	}}
	r := ReachOf(viewTree(p, viewC0()), viewP, protocol.Capability_ExecResize)
	if r.ActAll {
		t.Fatal("resize reach is global although exec_view reaches only the subtree")
	}
	wantIDs(t, "resize act", r.Act, sorted(viewC, viewP))

	p.Caps = protocol.Capability_ExecResize
	if r := ReachOf(viewTree(p, viewC0()), viewP, protocol.Capability_ExecResize); r.ActAll || len(r.Act) != 0 {
		t.Errorf("resize without exec_view = %+v, want no reach", r)
	}
}
