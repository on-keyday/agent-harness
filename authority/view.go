package authority

import (
	"sort"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Task is the view of one task the reach functions read: who created it and
// the authority it holds. Clients build it from a listing, the server from its
// store, so both answer with the same rules. Creator is "" for a task an
// operator created.
type Task struct {
	ID, Creator          string
	Caps, DelegateCaps   protocol.Capability
	Scope, DelegateScope Scope
}

// ReachResult is one task's reach over a listing. Act are the listed tasks it
// acts on, See those it only sees; both are sorted, See never repeats an Act
// id, and each is empty when the matching All is set — an "every task" answer
// is never spelled as the tasks this caller happens to list.
type ReachResult struct {
	ActAll, SeeAll bool
	Act, See       []string
}

// view indexes a listing once for the functions below.
type view struct {
	byID     map[string]Task
	ids      []string
	children map[string][]string
}

func newView(tasks []Task) view {
	v := view{byID: make(map[string]Task, len(tasks)), children: map[string][]string{}}
	for _, t := range tasks {
		v.byID[t.ID] = t
		v.ids = append(v.ids, t.ID)
		if t.Creator != "" {
			v.children[t.Creator] = append(v.children[t.Creator], t.ID)
		}
	}
	sort.Strings(v.ids)
	return v
}

// result lists the listed ids in act and, of the rest, those in see.
func (v view) result(actAll bool, act func(string) bool, seeAll bool, see func(string) bool) ReachResult {
	r := ReachResult{ActAll: actAll, SeeAll: seeAll}
	for _, id := range v.ids {
		switch {
		case actAll:
			// every listed task is acted on; nothing is only seen
		case act(id):
			r.Act = append(r.Act, id)
		case !seeAll && see(id):
			r.See = append(r.See, id)
		}
	}
	return r
}

func inSet(all bool, set map[string]bool) func(string) bool {
	return func(id string) bool { return all || set[id] }
}

// ReachOf is what the task id reaches over the listing through bit, as the
// server's gates resolve it: through every bit the task exercises
// (ExercisedBits), each under that bit's own scope. bit Capability_None asks
// only what it sees. An id not in the listing reaches nothing.
func ReachOf(tasks []Task, id string, bit protocol.Capability) ReachResult {
	v := newView(tasks)
	t, ok := v.byID[id]
	if !ok {
		return ReachResult{}
	}
	visAll, vis := VisibleSet(id, t.Scope, v.children)
	actAll, act := false, map[string]bool(nil)
	if bit != protocol.Capability_None && ExercisedBits(t.Caps)&bit != 0 {
		actAll, act = ActionSet(id, t.Scope, bit, v.children)
	}
	return v.result(actAll, inSet(actAll, act), visAll, inSet(visAll, vis))
}

// DelegableOf is what the task id may hand down: Act the ids it may write into
// a child's scope as action ids, See those it may hand down only as view-only
// ids — the delegation bound read as a scope around the task, through the same
// Granter ceiling a spawn is checked against.
func DelegableOf(tasks []Task, id string) ReachResult {
	v := newView(tasks)
	t, ok := v.byID[id]
	if !ok {
		return ReachResult{}
	}
	g := NewGranter(id, Grant{Caps: t.Caps, Scope: t.Scope, DelegateCaps: t.DelegateCaps, DelegateScope: t.DelegateScope}, v.children)
	actAll, _ := ActionSet(id, t.DelegateScope, ceilingBit, v.children)
	visAll, _ := VisibleSet(id, t.DelegateScope, v.children)
	return v.result(actAll && visAll,
		func(x string) bool { return g.ceil.Reaches(x, ceilingBit) },
		visAll,
		func(x string) bool { return g.ceil.Reaches(x, protocol.Capability_None) })
}

// Wider is how a task reaches further than its creator: the exercised bits
// whose target set is not inside the creator's for that bit, and whether it
// sees more than the creator sees.
type Wider struct {
	Bits protocol.Capability
	Sees bool
}

// Empty reports that the task is not wider than its creator.
func (w Wider) Empty() bool { return w.Bits == 0 && !w.Sees }

// WiderThanParent compares a task with its creator over the listing. The
// task's own id is left out — a task always acts on itself, whatever its
// creator may do — and visibility is compared too. A global set is inside only
// a global one.
//
// It describes, never accuses: an operator's grant, a creator narrowed after
// the fact and a swap all produce it legitimately. Zero for an
// operator-created task and for one whose creator the listing does not hold,
// which leaves nothing to compare against.
func WiderThanParent(tasks []Task, id string) Wider {
	v := newView(tasks)
	t, ok := v.byID[id]
	if !ok || t.Creator == "" {
		return Wider{}
	}
	p, ok := v.byID[t.Creator]
	if !ok {
		return Wider{}
	}
	exceeds := func(cAll bool, cSet map[string]bool, pAll bool, pSet map[string]bool) bool {
		if pAll {
			return false
		}
		if cAll {
			return true
		}
		for x := range cSet {
			if x != id && !pSet[x] {
				return true
			}
		}
		return false
	}
	var w Wider
	parentBits := ExercisedBits(p.Caps)
	for bit := protocol.Capability(1); bit != 0 && bit <= protocol.Capability_All; bit <<= 1 {
		if ExercisedBits(t.Caps)&bit == 0 {
			continue
		}
		cAll, cSet := ActionSet(id, t.Scope, bit, v.children)
		pAll, pSet := false, map[string]bool{}
		if parentBits&bit != 0 {
			pAll, pSet = ActionSet(p.ID, p.Scope, bit, v.children)
		}
		if exceeds(cAll, cSet, pAll, pSet) {
			w.Bits |= bit
		}
	}
	cAll, cVis := VisibleSet(id, t.Scope, v.children)
	pAll, pVis := VisibleSet(p.ID, p.Scope, v.children)
	w.Sees = exceeds(cAll, cVis, pAll, pVis)
	return w
}
