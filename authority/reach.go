package authority

import (
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Reach is what attenuation measures against: ONE granting task's scope,
// asked the two questions a grant raises. Attenuate (spawn) and
// ClampUnder (cascade) both use it, so the two cannot disagree about what
// a granter covers.
type Reach struct {
	hex      string
	scope    Scope
	children map[string][]string
	visAll   bool
	vis      map[string]bool
	sets     map[protocol.Capability]map[string]bool // per bit; a nil value is "all"
}

func NewReach(hex string, scope Scope, children map[string][]string) *Reach {
	r := &Reach{hex: hex, scope: scope, children: children, sets: map[protocol.Capability]map[string]bool{}}
	r.visAll, r.vis = VisibleSet(hex, scope, children)
	return r
}

// BoundRanks re-expresses s so that every bit in held resolves to at most the
// granter's rank FOR THAT BIT, narrowing only the bits the granter narrows.
//
// Each entry — the base scope, each override — keeps one rank. A held bit
// whose bound differs from it moves into an override of its own rank,
// carrying the entry's exclude_self and ids; bits of one entry bound to the
// same rank share one override. Lowering the whole entry instead would narrow
// every other bit it serves: a parent narrowed for cancel alone would take its
// child's file_read down with it. Masks stay pairwise disjoint because a piece
// only ever holds bits of the entry it came from.
//
//   - The base keeps min(its rank, the granter's base): the historical bound,
//     which also decides visibility where the rank follows the base.
//   - An override keeps min(its rank, the granter's base) for the bits the
//     task does not hold, so a grant template survives reuse as it always did.
//   - spawn never moves. It names no task, and Validate refuses it in a
//     mask.
//
// Where the granter's override for a bit is WIDER than its base, the bit may
// rise to it: the bound is the granter's scope for the bit, not its base.
func (r *Reach) BoundRanks(s Scope, held protocol.Capability) Scope {
	bound := func(rank protocol.ScopeBase, bit protocol.Capability) protocol.ScopeBase {
		b, _, _ := r.scope.ForCap(bit)
		return MinBase(rank, b)
	}
	// split groups the held bits of one entry by the rank they are bound to,
	// leaving out those bound to keep; ranks come back in ascending order.
	split := func(rank, keep protocol.ScopeBase, bits protocol.Capability) (stay protocol.Capability, moved []ScopeOverride) {
		byRank := map[protocol.ScopeBase]protocol.Capability{}
		for bit := protocol.Capability(1); bit != 0 && bit <= protocol.Capability_All; bit <<= 1 {
			if bits&bit == 0 {
				continue
			}
			if b := bound(rank, bit); b != keep {
				byRank[b] |= bit
			} else {
				stay |= bit
			}
		}
		for _, b := range []protocol.ScopeBase{protocol.ScopeBase_None, protocol.ScopeBase_Subtree, protocol.ScopeBase_Global} {
			if m := byRank[b]; m != 0 {
				moved = append(moved, ScopeOverride{Caps: m, Base: b})
			}
		}
		return stay, moved
	}
	withEntry := func(pieces []ScopeOverride, excl bool, ids []string) []ScopeOverride {
		for i := range pieces {
			pieces[i].ExcludeSelf = excl
			pieces[i].IDs = append([]string(nil), ids...)
		}
		return pieces
	}

	out := s
	out.Base = MinBase(s.Base, r.scope.Base)
	out.Overrides = nil
	var overridden protocol.Capability
	for _, o := range s.Overrides {
		overridden |= o.Caps
		keep := MinBase(o.Base, r.scope.Base)
		stay, moved := split(o.Base, keep, o.Caps&held)
		if main := stay | o.Caps&^held; main != 0 {
			out.Overrides = append(out.Overrides, ScopeOverride{Caps: main, Base: keep, ExcludeSelf: o.ExcludeSelf, IDs: o.IDs})
		}
		out.Overrides = append(out.Overrides, withEntry(moved, o.ExcludeSelf, o.IDs)...)
	}
	_, moved := split(s.Base, out.Base, held&^overridden&^protocol.Capability_Spawn)
	out.Overrides = append(out.Overrides, withEntry(moved, s.ExcludeSelf, s.IDs)...)
	return out
}

// reaches reports whether id may be named in a grant serving bits. Every id
// joins the grantee's visible set, so it must be visible to the granter; an
// action id must also be in the granter's set for every bit it serves, so an
// id the granter may read but not cancel is refused wherever it would carry
// cancel — a view-only grant included.
func (r *Reach) Reaches(id string, bits protocol.Capability) bool {
	if !r.visAll && !r.vis[id] {
		return false
	}
	for bit := protocol.Capability(1); bit != 0 && bit <= protocol.Capability_All; bit <<= 1 {
		if bits&bit == 0 {
			continue
		}
		set, cached := r.sets[bit]
		if !cached {
			if all, allowed := ActionSet(r.hex, r.scope, bit, r.children); !all {
				set = allowed
			}
			r.sets[bit] = set
		}
		if set != nil && !set[id] {
			return false
		}
	}
	return true
}
