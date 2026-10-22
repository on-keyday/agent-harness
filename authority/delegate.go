package authority

import (
	"fmt"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Grant is the authority written onto one task: what it may do (Caps, Scope)
// and what it may hand to the tasks it grants to (DelegateCaps,
// DelegateScope — its delegation bound).
//
// The zero bound delegates no bits and reads its ids as a subtree around the
// holder: its own id and its descendants'. That is the default every task
// gets, a legacy record included, because both zero values already mean it.
//
// DelegateScope never carries Overrides: the bound is one ceiling for every
// bit, and a per-capability bound is deliberately not expressible.
type Grant struct {
	Caps          protocol.Capability
	Scope         Scope
	DelegateCaps  protocol.Capability
	DelegateScope Scope
}

// Granter is one task about to write a Grant onto another, measured twice:
// by its own scope, as Attenuate and ClampUnder measure a creator, and by its
// delegation bound read as a scope AROUND IT. The second reading is the
// ceiling — the ids it names are the ones this task may write into a child's
// scope, so a subtree ceiling admits the granter's own id and its
// descendants' with no list, which is what the supervisor recipes need.
//
// A nil *Granter is an operator connection: unbounded, as its own caps are.
type Granter struct {
	held Grant
	own  *Reach
	ceil *Reach
}

// ceilingBit is the bit the ceiling's action set is asked about. Any single
// bit gives the same answer: the ceiling carries no overrides, so ForCap
// returns its base for every bit.
const ceilingBit = protocol.Capability_FileRead

// NewGranter measures the task hex, holding held, over the creator index
// children.
func NewGranter(hex string, held Grant, children map[string][]string) *Granter {
	return &Granter{
		held: held,
		own:  NewReach(hex, held.Scope, children),
		ceil: NewReach(hex, held.DelegateScope, children),
	}
}

// InvalidScopeError is a scope inconsistent on its own terms (Validate).
type InvalidScopeError struct{ Err error }

func (e *InvalidScopeError) Error() string { return "invalid scope: " + e.Err.Error() }
func (e *InvalidScopeError) Unwrap() error { return e.Err }

// OutOfReachError is an id outside the granter's own scope.
type OutOfReachError struct{ ID string }

func (e *OutOfReachError) Error() string { return "scope id " + e.ID + " is outside your own scope" }

// OutOfDelegationError is an id the granter could name for itself but may not
// hand down: outside its delegation bound. Kept apart from OutOfReachError
// because an agent told "outside your own scope" about an id it can act on
// would go and look at the wrong field. Field is the flag the id came in:
// "scope" or "delegate-scope".
type OutOfDelegationError struct {
	ID    string
	Field string
}

func (e *OutOfDelegationError) Error() string {
	return e.Field + " id " + e.ID + " is outside what you may delegate (see whoami: delegate-scope)"
}

// ChildCaps is the mask a task granted by g holds: g's caps, inside what g may
// delegate, inside the request. Capability_All therefore means "everything
// the granter may delegate".
func (g *Granter) ChildCaps(requested protocol.Capability) protocol.Capability {
	if g == nil {
		return requested
	}
	return g.held.Caps & g.held.DelegateCaps & requested
}

// ChildScope is the scope a task granted by g gets: Attenuate against g's own
// scope, then the ceiling. held is the mask the task will hold. An id must
// pass both — inside g's own reach and inside what g may delegate.
func (g *Granter) ChildScope(req Scope, held protocol.Capability) (Scope, error) {
	if g == nil {
		out, offender, ok := Attenuate(req, held, nil)
		if !ok {
			return Scope{}, attenuateError(offender)
		}
		return out, nil
	}
	out, offender, ok := Attenuate(req, held, g.own)
	if !ok {
		return Scope{}, attenuateError(offender)
	}
	return g.ceiling(out, "scope", false)
}

// attenuateError types Attenuate's offender: a consistency failure arrives
// prefixed, anything else is an id.
func attenuateError(offender string) error {
	const prefix = "invalid scope: "
	if len(offender) >= len(prefix) && offender[:len(prefix)] == prefix {
		return &InvalidScopeError{Err: fmt.Errorf("%s", offender[len(prefix):])}
	}
	return &OutOfReachError{ID: offender}
}

// ChildBound is the delegation bound a task granted by g gets: the request
// inside g's own bound, so the limit carries down the chain. childHex is the
// task's id, "" for one not yet created.
//
// The caps are intersected with g's bound only, not with g's caps: the bound
// is kept as written and every use intersects with the holder's caps, so a
// bit granted to the child later is delegable if its bound still names it.
//
// exclude_self is forced on when the child is not among the action ids g may
// hand down: otherwise, under a none ceiling, the child could hand its own id
// to a grandchild although g may not hand that id to anyone.
func (g *Granter) ChildBound(reqCaps protocol.Capability, req Scope, childHex string) (protocol.Capability, Scope, error) {
	if len(req.Overrides) != 0 {
		return 0, Scope{}, &InvalidScopeError{Err: fmt.Errorf("a delegation bound carries no per-capability overrides")}
	}
	if err := Validate(req); err != nil {
		return 0, Scope{}, &InvalidScopeError{Err: err}
	}
	req.IDs, req.VisIDs = NormalizeIDs(req.IDs), NormalizeIDs(req.VisIDs)
	if g == nil {
		return reqCaps, req, nil
	}
	out, err := g.ceiling(req, "delegate-scope", false)
	if err != nil {
		return 0, Scope{}, err
	}
	out.ExcludeSelf = out.ExcludeSelf || !g.coversChild(childHex)
	return reqCaps & g.held.DelegateCaps, out, nil
}

// ClampDescendant re-imposes g's whole grant on a descendant d after g's
// bound was rewritten: caps and scope as a cascade clamps them today, then
// the ceiling, then d's own bound inside g's. A cascade has no caller to
// refuse, so an id outside the ceiling is dropped. g must not be nil.
func (g *Granter) ClampDescendant(d Grant, dHex string) Grant {
	caps := d.Caps & g.held.Caps & g.held.DelegateCaps
	scope, _ := g.ceiling(ClampUnder(d.Scope, caps, g.own), "scope", true)
	bound, _ := g.ceiling(d.DelegateScope, "delegate-scope", true)
	bound.ExcludeSelf = bound.ExcludeSelf || !g.coversChild(dHex)
	return Grant{Caps: caps, Scope: scope, DelegateCaps: d.DelegateCaps & g.held.DelegateCaps, DelegateScope: bound}
}

// coversChild reports whether a task is among the action ids g may hand
// down. A task not yet created has no id an id list could name, so only the
// ceiling's base decides.
func (g *Granter) coversChild(childHex string) bool {
	if childHex == "" {
		return CoversNewChild(g.held.DelegateScope, ceilingBit)
	}
	return g.ceil.Reaches(childHex, ceilingBit)
}

// ceiling bounds s by g's delegation bound C: every rank at most C's, every
// action id inside the ids C lets g hand down as action ids, every view-only
// id inside those it may hand down at all. A refused id is an
// OutOfDelegationError naming field, or with drop it is left out.
// exclude_self only ever narrows and is left alone.
func (g *Granter) ceiling(s Scope, field string, drop bool) (Scope, error) {
	c := g.held.DelegateScope
	var refused string
	keep := func(ids []string, bits protocol.Capability) []string {
		if refused != "" {
			return nil
		}
		out := ids[:0:0]
		for _, id := range ids {
			if g.ceil.Reaches(id, bits) {
				out = append(out, id)
			} else if !drop {
				refused = id
				return nil
			}
		}
		return out
	}

	out := Scope{
		Base:        MinBase(s.Base, c.Base),
		ExcludeSelf: s.ExcludeSelf,
		IDs:         keep(s.IDs, ceilingBit),
	}
	// Written explicitly only once the rank no longer follows the clamped
	// base, so the result has the one encoding Validate accepts.
	if vis := MinBase(s.VisRank(), c.VisRank()); vis != out.Base {
		out.VisBasePresent, out.VisBase = true, vis
	}
	for _, o := range s.Overrides {
		o.Base = MinBase(o.Base, c.Base)
		o.IDs = keep(o.IDs, ceilingBit)
		out.Overrides = append(out.Overrides, o)
	}
	out.VisIDs = keep(s.VisIDs, protocol.Capability_None)
	if refused != "" {
		return Scope{}, &OutOfDelegationError{ID: refused, Field: field}
	}
	return out, nil
}
