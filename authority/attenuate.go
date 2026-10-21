package authority

import (
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Attenuate clamps a requested scope to the creator's effective one, so a
// spawned task can never reach further than its spawner. creator is the
// spawner measured as a Reach; nil is an operator, which only gets its
// request normalised and validated.
//
// childCaps is the mask the task will hold, because "further" is per bit: for
// every bit the child holds, its target set for that bit stays inside the
// creator's set for that bit, and its visible set stays inside the creator's
// visible set.
//
//   - Ranks are clamped by permissiveness (see MinBase), which is why a
//     subtree parent asking for a global child yields subtree. Each held bit is
//     bounded by the creator's rank for that bit (Reach.BoundRanks), so a
//     creator narrowed for one bit narrows that bit alone. The visibility rank
//     clamps to the creator's VISIBILITY rank, never to its action base.
//   - Ids are checked by membership AT SPAWN TIME: action ids against the
//     creator's set for each held bit they serve, view-only ids against what it
//     sees. This is a static, one-shot check: the ids are literal, so a later
//     narrowing of the parent does not retroactively invalidate the child (that
//     is what set_caps --cascade is for).
//
// A rejected id is returned as `offender` with ok=false rather than being
// dropped. Silent clamping would produce a task whose scope differs from the
// one the caller wrote with nothing saying so — the same invisible-divergence
// failure the presence bits on set_caps exist to avoid.
//
// An operator creator may request anything, including ids for tasks it has
// never touched. A creator that can SEE everything may not: it used to share
// the operator's early return, from when the visibility rank was the action
// base, and handed out a global action scope from a subtree one — the exact
// widening scopeSet's comment rules out.
func Attenuate(req Scope, childCaps protocol.Capability, creator *Reach) (out Scope, offender string, ok bool) {
	out = Scope{
		Base:           req.Base,
		VisBase:        req.VisBase,
		VisBasePresent: req.VisBasePresent,
		ExcludeSelf:    req.ExcludeSelf,
		IDs:            NormalizeIDs(req.IDs),
		VisIDs:         NormalizeIDs(req.VisIDs),
	}
	for _, o := range req.Overrides {
		out.Overrides = append(out.Overrides, ScopeOverride{
			Caps: o.Caps, Base: o.Base, ExcludeSelf: o.ExcludeSelf, IDs: NormalizeIDs(o.IDs),
		})
	}

	// Consistency first, before anything is measured against the parent: an
	// override mask that is empty or overlaps another describes nothing
	// coherent, so it is rejected as written rather than clamped into
	// something the caller never asked for.
	//
	// There is no second pass after clamping any more. One existed to catch an
	// override left stranded above a lowered visibility rank; ranks are
	// independent now, so nothing clamping does can make a consistent value
	// inconsistent.
	if err := Validate(out); err != nil {
		// Prefixed so the caller can tell a consistency failure from an
		// out-of-reach id without parsing: both arrive through `offender`, and
		// the submit path used to wrap every one of them in "scope id … is
		// outside your own scope", which turned a sentence into nonsense.
		return Scope{}, "invalid scope: " + err.Error(), false
	}

	if creator == nil {
		return out, "", true
	}
	r := creator

	// Rank clamping, per axis and per capability. A child asking for more than
	// its parent holds is lowered rather than refused -- the historical
	// behaviour for the base, extended to the axes that did not exist then.
	wantVis := MinBase(out.VisRank(), r.scope.VisRank())
	exercised := ExercisedBits(childCaps)
	out = r.BoundRanks(out, exercised)
	// Written explicitly once the rank no longer follows the clamped base, as
	// ClampUnder does: a child whose visibility followed a base the
	// creator may act through but not see would otherwise see it.
	if out.VisBasePresent || wantVis != out.Base {
		out.VisBasePresent = true
		out.VisBase = wantVis
	}
	var overridden protocol.Capability
	for _, o := range out.Overrides {
		overridden |= o.Caps
	}
	baseBits := exercised &^ overridden

	// Ids are CHECKED, never clamped: a silently dropped target produces a task
	// whose scope differs from the one the caller wrote with nothing saying so.
	// Checking the pieces BoundRanks produced is checking the request: every
	// bit of an entry lands in exactly one piece, carrying the entry's ids.
	for _, o := range out.Overrides {
		for _, id := range o.IDs {
			if !r.Reaches(id, o.Caps&exercised) {
				return Scope{}, id, false
			}
		}
	}
	for _, id := range out.IDs {
		if !r.Reaches(id, baseBits) {
			return Scope{}, id, false
		}
	}
	for _, id := range out.VisIDs {
		if !r.Reaches(id, protocol.Capability_None) {
			return Scope{}, id, false
		}
	}

	return out, "", true
}

// IsNarrowing reports whether the change removed authority in any direction:
// a cap bit cleared, the scope of a still-held bit narrowed, or the visible set
// narrowed. Only a narrowing justifies tearing down live connections.
//
// Judged per bit, because a cascade narrows per bit: comparing the base scope
// alone missed an override ranked down, so an attach opened under the wider
// grant outlived it.
func IsNarrowing(beforeCaps protocol.Capability, before Scope, afterCaps protocol.Capability, after Scope) bool {
	if beforeCaps&^afterCaps != 0 {
		return true
	}
	if reachNarrowed(before.VisRank(), false, visibleIDs(before),
		after.VisRank(), false, visibleIDs(after)) {
		return true
	}
	exercised := ExercisedBits(beforeCaps)
	for bit := protocol.Capability(1); bit != 0 && bit <= protocol.Capability_All; bit <<= 1 {
		if exercised&bit == 0 {
			continue
		}
		bBase, bExcl, bIDs := before.ForCap(bit)
		aBase, aExcl, aIDs := after.ForCap(bit)
		if reachNarrowed(bBase, bExcl, bIDs, aBase, aExcl, aIDs) {
			return true
		}
	}
	return false
}

// reachNarrowed compares two descriptions of one target set: a rank ranked
// down, self newly excluded, or an explicit id no longer named. A global rank
// afterwards covers everything, so nothing it replaced was narrowed.
func reachNarrowed(bBase protocol.ScopeBase, bExcl bool, bIDs []string, aBase protocol.ScopeBase, aExcl bool, aIDs []string) bool {
	if aBase == protocol.ScopeBase_Global {
		return false
	}
	if BaseRank(aBase) < BaseRank(bBase) || (aExcl && !bExcl) {
		return true
	}
	kept := make(map[string]bool, len(aIDs))
	for _, id := range aIDs {
		kept[id] = true
	}
	for _, id := range bIDs {
		if !kept[id] {
			return true
		}
	}
	return false
}

// visibleIDs is every id a scope makes visible on top of its rank — the id
// half of VisibleSet's formula.
func visibleIDs(s Scope) []string {
	out := append(append([]string(nil), s.VisIDs...), s.IDs...)
	for _, o := range s.Overrides {
		out = append(out, o.IDs...)
	}
	return out
}

// ClampUnder re-imposes the monotonic invariant on ONE descendant after
// its parent's authority changed. Every axis is clamped here, in one function,
// because the inline struct literal this replaced carried Base and IDs only —
// a cascade silently erased a child's visibility rank and its overrides, which
// is a rewrite of authority nothing reported.
//
// It measures against the parent through Reach, the same helper spawn-time
// attenuation uses, so a cascade and a spawn agree on what the parent covers:
// each held bit's rank and ids are bounded by the parent's scope FOR THAT BIT
// (BoundRanks, which narrows only the bits the parent narrows), and an id
// survives when it is still inside the parent's reach — not only when the
// parent names it, which dropped ids inside the parent's subtree. Ids outside
// it are dropped; a cascade has no caller to refuse.
//
// childCaps is the descendant's ALREADY-clamped capability mask, so an
// override for a bit it no longer holds or exercises (ExercisedBits) is
// dropped rather than left to reapply if the bit is granted again later.
//
// parent must not be nil. Unlike Attenuate, where nil is an operator creator,
// a cascade always clamps under a task.
func ClampUnder(child Scope, childCaps protocol.Capability, parent *Reach) Scope {
	r := parent
	exercised := ExercisedBits(childCaps)
	in := Scope{Base: child.Base, ExcludeSelf: child.ExcludeSelf, IDs: child.IDs}
	for _, o := range child.Overrides {
		o.Caps &= exercised
		if o.Caps != protocol.Capability_None {
			in.Overrides = append(in.Overrides, o)
		}
	}
	bounded := r.BoundRanks(in, exercised)
	keep := func(ids []string, bits protocol.Capability) []string {
		out := ids[:0:0]
		for _, id := range ids {
			if r.Reaches(id, bits) {
				out = append(out, id)
			}
		}
		return out
	}

	var overridden protocol.Capability
	for _, o := range bounded.Overrides {
		overridden |= o.Caps
	}
	out := Scope{
		Base:        bounded.Base,
		IDs:         keep(bounded.IDs, exercised&^overridden),
		ExcludeSelf: child.ExcludeSelf, // only ever narrows; nothing to clamp
		VisIDs:      keep(child.VisIDs, protocol.Capability_None),
	}

	// The visibility rank clamps against the parent's rank. Written explicitly
	// rather than copied, so a child that inherited its rank from its base does
	// not silently acquire a present bit it never had.
	if clamped := MinBase(child.VisRank(), parent.scope.VisRank()); clamped != out.Base {
		out.VisBasePresent = true
		out.VisBase = clamped
	}

	// Ids are filtered per piece: an id the parent may still reach for some of
	// an entry's bits survives in those pieces and leaves the others.
	for _, o := range bounded.Overrides {
		o.IDs = keep(o.IDs, o.Caps)
		out.Overrides = append(out.Overrides, o)
	}

	// No final clamp against the visibility rank. One used to live here to keep
	// every action set inside what ls shows; that rule is gone, and the two
	// ranks clamp independently against the parent above.
	return out
}
