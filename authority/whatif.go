package authority

import (
	"fmt"
	"sort"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func (t Task) grant() Grant {
	return Grant{Caps: t.Caps, Scope: t.Scope, DelegateCaps: t.DelegateCaps, DelegateScope: t.DelegateScope}
}

func (t *Task) setGrant(g Grant) {
	t.Caps, t.Scope, t.DelegateCaps, t.DelegateScope = g.Caps, g.Scope, g.DelegateCaps, g.DelegateScope
}

// CascadeClamp is what `caps set --cascade` writes onto one descendant d of
// target, target holding its authority AFTER the change. Without a bound half
// in the call, d's caps come down to target's and its scope under target's
// reach, and its bound is kept: every supervisor recorded before the bound
// replays with the empty one, so clamping to it on every cascade would zero
// their workers. With a bound half, all of d comes down inside target's grant,
// bound included (Granter.ClampDescendant).
//
// The server's cascade and WhatIf both call this, so a what-if cannot draw a
// cascade the server would not perform.
func CascadeClamp(target Task, d Grant, dHex string, boundWritten bool, children map[string][]string) Grant {
	if boundWritten {
		return NewGranter(target.ID, target.grant(), children).ClampDescendant(d, dHex)
	}
	out := d
	out.Caps = d.Caps & target.Caps
	out.Scope = ClampUnder(d.Scope, out.Caps, NewReach(target.ID, target.Scope, children))
	return out
}

// Change is one operation WhatIf can apply: Regrant, Spawn or Reparent.
type Change interface{ isChange() }

// Regrant is `caps set`: each non-nil half is written as given (no
// attenuation — the operator may grant anything), and Cascade clamps every
// descendant through CascadeClamp.
type Regrant struct {
	Target        string
	Caps          *protocol.Capability
	Scope         *Scope
	DelegateCaps  *protocol.Capability
	DelegateScope *Scope
	Cascade       bool
}

// Spawn is a new task created by Creator ("" for an operator) with the
// requested authority, granted as the server grants a spawn.
type Spawn struct {
	Creator       string
	Caps          protocol.Capability
	Scope         Scope
	DelegateCaps  protocol.Capability
	DelegateScope Scope
}

// Reparent is `caps set-parent`: Target moves under NewParent ("" = the
// root). It moves no authority, but every subtree it leaves or joins changes.
type Reparent struct {
	Target, NewParent string
}

func (Regrant) isChange()  {}
func (Spawn) isChange()    {}
func (Reparent) isChange() {}

// PhantomID is the id a Spawn's child carries in the diff.
const PhantomID = "new"

// TaskDiff is one task whose authority or reach the change alters. Added and
// Removed are, per bit the task exercises before or after (Capability_None for
// what it sees), the listed tasks it gains or loses, sorted.
type TaskDiff struct {
	ID            string
	New           bool
	Before, After Grant
	Added         map[protocol.Capability][]string
	Removed       map[protocol.Capability][]string
}

func short(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// WhatIf applies c to the listing and returns every task it changes. Nothing
// is sent anywhere; the rules are the ones the server enforces with.
func WhatIf(tasks []Task, c Change) ([]TaskDiff, error) {
	before := append([]Task(nil), tasks...)
	after, err := apply(before, c)
	if err != nil {
		return nil, err
	}
	return diff(before, after), nil
}

func apply(before []Task, c Change) ([]Task, error) {
	v := newView(before)
	after := append([]Task(nil), before...)
	index := map[string]int{}
	for i, t := range after {
		index[t.ID] = i
	}
	switch c := c.(type) {
	case Regrant:
		i, ok := index[c.Target]
		if !ok {
			return nil, fmt.Errorf("no such task %s", short(c.Target))
		}
		t := after[i]
		if c.Caps != nil {
			t.Caps = *c.Caps
		}
		if c.Scope != nil {
			if err := Validate(*c.Scope); err != nil {
				return nil, &InvalidScopeError{Err: err}
			}
			t.Scope = *c.Scope
		}
		if c.DelegateCaps != nil {
			t.DelegateCaps = *c.DelegateCaps
		}
		if c.DelegateScope != nil {
			if len(c.DelegateScope.Overrides) != 0 {
				return nil, &InvalidScopeError{Err: fmt.Errorf("a delegation bound carries no per-capability overrides")}
			}
			if err := Validate(*c.DelegateScope); err != nil {
				return nil, &InvalidScopeError{Err: err}
			}
			t.DelegateScope = *c.DelegateScope
		}
		after[i] = t
		if c.Cascade {
			bound := c.DelegateCaps != nil || c.DelegateScope != nil
			desc := map[string]bool{}
			DescendantsOf(v.children, c.Target, desc)
			for id := range desc {
				if id == c.Target {
					continue
				}
				j := index[id]
				after[j].setGrant(CascadeClamp(t, after[j].grant(), id, bound, v.children))
			}
		}
	case Spawn:
		var g *Granter
		if c.Creator != "" {
			creator, ok := v.byID[c.Creator]
			if !ok {
				return nil, fmt.Errorf("no such task %s", short(c.Creator))
			}
			// The server's submit gate, ahead of any grant: a creator without
			// spawn creates nothing. The text is the one the client prints for
			// that denial (cli.CapabilityDeniedError).
			if creator.Caps&protocol.Capability_Spawn == 0 {
				return nil, fmt.Errorf("permission denied: %s requires capability %s",
					protocol.TaskControlKind_Submit, CapsLabel(protocol.Capability_Spawn))
			}
			g = NewGranter(creator.ID, creator.grant(), v.children)
		}
		caps := g.ChildCaps(c.Caps)
		scope, err := g.ChildScope(c.Scope, caps)
		if err != nil {
			return nil, err
		}
		dcaps, dscope, err := g.ChildBound(c.DelegateCaps, c.DelegateScope, "")
		if err != nil {
			return nil, err
		}
		after = append(after, Task{ID: PhantomID, Creator: c.Creator, Caps: caps, Scope: scope, DelegateCaps: dcaps, DelegateScope: dscope})
	case Reparent:
		i, ok := index[c.Target]
		if !ok {
			return nil, fmt.Errorf("no such task %s", short(c.Target))
		}
		if c.NewParent != "" {
			if _, ok := index[c.NewParent]; !ok {
				return nil, fmt.Errorf("no such task %s", short(c.NewParent))
			}
			desc := map[string]bool{}
			DescendantsOf(v.children, c.Target, desc)
			if desc[c.NewParent] {
				return nil, fmt.Errorf("would make %s its own ancestor", short(c.Target))
			}
		}
		after[i].Creator = c.NewParent
	default:
		return nil, fmt.Errorf("unknown change %T", c)
	}
	return after, nil
}

// setOf lists, over the view's ids, a set given as (all, members).
func setOf(v view, all bool, set map[string]bool) map[string]bool {
	out := map[string]bool{}
	for _, id := range v.ids {
		if all || set[id] {
			out[id] = true
		}
	}
	return out
}

func diff(before, after []Task) []TaskDiff {
	bv, av := newView(before), newView(after)
	var out []TaskDiff
	for _, id := range av.ids {
		a := av.byID[id]
		b, existed := bv.byID[id]
		d := TaskDiff{ID: id, New: !existed, After: a.grant(),
			Added: map[protocol.Capability][]string{}, Removed: map[protocol.Capability][]string{}}
		if existed {
			d.Before = b.grant()
		}
		bits := ExercisedBits(a.Caps)
		if existed {
			bits |= ExercisedBits(b.Caps)
		}
		measure := func(bit protocol.Capability) {
			var bAll, aAll bool
			var bSet, aSet map[string]bool
			if bit == protocol.Capability_None {
				aAll, aSet = VisibleSet(id, a.Scope, av.children)
				if existed {
					bAll, bSet = VisibleSet(id, b.Scope, bv.children)
				}
			} else {
				aAll, aSet, _ = actionReach(a, bit, av.children)
				if existed {
					bAll, bSet, _ = actionReach(b, bit, bv.children)
				}
			}
			if bAll && aAll {
				return // global before and after: nothing a listing can add
			}
			was, now := setOf(bv, bAll, bSet), setOf(av, aAll, aSet)
			for x := range now {
				if !was[x] {
					d.Added[bit] = append(d.Added[bit], x)
				}
			}
			for x := range was {
				if !now[x] {
					d.Removed[bit] = append(d.Removed[bit], x)
				}
			}
			sort.Strings(d.Added[bit])
			sort.Strings(d.Removed[bit])
		}
		measure(protocol.Capability_None)
		for bit := protocol.Capability(1); bit != 0 && bit <= protocol.Capability_All; bit <<= 1 {
			if bits&bit != 0 {
				measure(bit)
			}
		}
		for bit, ids := range d.Added {
			if len(ids) == 0 {
				delete(d.Added, bit)
			}
		}
		for bit, ids := range d.Removed {
			if len(ids) == 0 {
				delete(d.Removed, bit)
			}
		}
		if d.New || !sameGrant(d.Before, d.After) || len(d.Added) > 0 || len(d.Removed) > 0 {
			out = append(out, d)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].New != out[j].New {
			return !out[i].New
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func sameGrant(a, b Grant) bool {
	return a.Caps == b.Caps && a.DelegateCaps == b.DelegateCaps && sameScope(a.Scope, b.Scope) && sameScope(a.DelegateScope, b.DelegateScope)
}

// sameScope compares two scopes as authority, so a nil and an empty id list
// are the same list.
func sameScope(a, b Scope) bool {
	sameIDs := func(x, y []string) bool {
		if len(x) != len(y) {
			return false
		}
		for i := range x {
			if x[i] != y[i] {
				return false
			}
		}
		return true
	}
	if a.Base != b.Base || a.VisRank() != b.VisRank() || a.VisBasePresent != b.VisBasePresent ||
		a.ExcludeSelf != b.ExcludeSelf || !sameIDs(a.IDs, b.IDs) || !sameIDs(a.VisIDs, b.VisIDs) ||
		len(a.Overrides) != len(b.Overrides) {
		return false
	}
	for i := range a.Overrides {
		x, y := a.Overrides[i], b.Overrides[i]
		if x.Caps != y.Caps || x.Base != y.Base || x.ExcludeSelf != y.ExcludeSelf || !sameIDs(x.IDs, y.IDs) {
			return false
		}
	}
	return true
}
