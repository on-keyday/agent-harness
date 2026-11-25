package server

import (
	"encoding/hex"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// applyScopeToWAL / scopeFromWAL are the WAL projection of authority.Scope,
// kept together so the two directions cannot drift apart. They stay in server
// because WALEvent is a server type.
//
// applyScopeToWAL writes into the event rather than returning a tuple: with six
// fields to carry, a caller that populated some and forgot the rest would
// produce a record that replays as a DIFFERENT authority, and nothing would
// say so.
func applyScopeToWAL(s authority.Scope, ev *WALEvent) {
	ev.ScopeBase = uint8(s.Base)
	ev.ScopeIDs = s.IDs
	ev.ScopeVisBase = uint8(s.VisBase)
	ev.ScopeVisBasePresent = s.VisBasePresent
	ev.ScopeExcludeSelf = s.ExcludeSelf
	ev.ScopeVisIDs = s.VisIDs
	ev.ScopeOverrides = nil
	for _, o := range s.Overrides {
		ev.ScopeOverrides = append(ev.ScopeOverrides, WALScopeOverride{
			Caps: uint64(o.Caps), Base: uint8(o.Base), ExcludeSelf: o.ExcludeSelf, IDs: o.IDs,
			Cond: conditionToWAL(o.Cond),
		})
	}
}

// scopeFromWAL reconstructs a Scope from a record.
//
// The migration here is ADDITIVE. A legacy record whose caps carry
// board_observe (formerly info_global, which gated task visibility as well)
// gains global visibility; its capability mask is never touched. Clearing the
// bit would silently revoke board observation at the moment of a restart, with
// no operator action requesting it and no way afterwards to tell the result
// apart from a deliberate narrowing.
//
// The condition is !VisBasePresent, so it fires only for records written
// before the axis existed — a task explicitly granted a visibility rank keeps
// the one it was given.
func scopeFromWAL(ev WALEvent) authority.Scope {
	s := authority.Scope{
		Base:           protocol.ScopeBase(ev.ScopeBase),
		IDs:            authority.NormalizeIDs(ev.ScopeIDs),
		VisBase:        protocol.ScopeBase(ev.ScopeVisBase),
		VisBasePresent: ev.ScopeVisBasePresent,
		ExcludeSelf:    ev.ScopeExcludeSelf,
		VisIDs:         authority.NormalizeIDs(ev.ScopeVisIDs),
	}
	for _, o := range ev.ScopeOverrides {
		s.Overrides = append(s.Overrides, authority.ScopeOverride{
			Caps: protocol.Capability(o.Caps), Base: protocol.ScopeBase(o.Base),
			ExcludeSelf: o.ExcludeSelf, IDs: authority.NormalizeIDs(o.IDs),
			Cond: conditionFromWAL(o.Cond),
		})
	}
	if !s.VisBasePresent && protocol.Capability(ev.Capabilities)&protocol.Capability_BoardObserve != 0 {
		s.VisBasePresent = true
		s.VisBase = protocol.ScopeBase_Global
	}
	return s
}

// conditionToWAL / conditionFromWAL are the stored form of a grant condition,
// shared by the WAL and the schedule store.
func conditionToWAL(c *authority.Condition) *WALCondition {
	if c == nil {
		return nil
	}
	w := &WALCondition{Kind: uint8(c.Kind)}
	for _, a := range c.Actions {
		w.Actions = append(w.Actions, hex.EncodeToString(a[:]))
	}
	for _, s := range c.Spans {
		w.Spans = append(w.Spans, [2]uint16{s.Lo, s.Hi})
	}
	return w
}

// conditionFromWAL reads a stored condition back. Anything it cannot read —
// an unknown kind, no kind, a malformed id, a backwards span, a list over the
// limit — is kind 0, which reaches nothing (C7): a stored condition never
// replays as unconditioned.
func conditionFromWAL(w *WALCondition) *authority.Condition {
	if w == nil {
		return nil
	}
	var acts [][16]byte
	for _, h := range w.Actions {
		raw, err := hex.DecodeString(h)
		if err != nil || len(raw) != 16 {
			return &authority.Condition{}
		}
		var id [16]byte
		copy(id[:], raw)
		acts = append(acts, id)
	}
	var spans []authority.PortSpan
	for _, s := range w.Spans {
		spans = append(spans, authority.PortSpan{Lo: s[0], Hi: s[1]})
	}
	c, err := authority.NewCondition(protocol.ConditionKind(w.Kind), acts, spans)
	if err != nil {
		return &authority.Condition{}
	}
	return c
}

// applyDelegationToWAL / delegationFromWAL are the same projection for the
// delegation bound, under the delegate_ key prefix. A bound carries no
// overrides, so there is no override list to write.
//
// scopeFromWAL's board_observe migration deliberately has no counterpart: it
// exists for records older than the visibility axis, and every bound is newer.
func applyDelegationToWAL(caps protocol.Capability, s authority.Scope, ev *WALEvent) {
	ev.DelegateCaps = uint64(caps)
	ev.DelegateScopeBase = uint8(s.Base)
	ev.DelegateScopeVisBase = uint8(s.VisBase)
	ev.DelegateScopeVisBasePresent = s.VisBasePresent
	ev.DelegateScopeExcludeSelf = s.ExcludeSelf
	ev.DelegateScopeIDs = s.IDs
	ev.DelegateScopeVisIDs = s.VisIDs
}

// applyLimitsToWAL / limitsFromWAL are the projection of the two spawn
// limits. The zero values (an absent key) read back as unbounded.
func applyLimitsToWAL(depth, max protocol.SpawnLimit, ev *WALEvent) {
	ev.SpawnDepth, ev.SpawnDepthLimited = depth.Value(), depth.Limited()
	ev.MaxDescendants, ev.MaxDescendantsLimited = max.Value(), max.Limited()
}

func limitsFromWAL(ev WALEvent) (depth, max protocol.SpawnLimit) {
	if ev.SpawnDepthLimited {
		depth = authority.LimitOf(ev.SpawnDepth)
	}
	if ev.MaxDescendantsLimited {
		max = authority.LimitOf(ev.MaxDescendants)
	}
	return depth, max
}

func delegationFromWAL(ev WALEvent) (protocol.Capability, authority.Scope) {
	return protocol.Capability(ev.DelegateCaps), authority.Scope{
		Base:           protocol.ScopeBase(ev.DelegateScopeBase),
		VisBase:        protocol.ScopeBase(ev.DelegateScopeVisBase),
		VisBasePresent: ev.DelegateScopeVisBasePresent,
		ExcludeSelf:    ev.DelegateScopeExcludeSelf,
		IDs:            authority.NormalizeIDs(ev.DelegateScopeIDs),
		VisIDs:         authority.NormalizeIDs(ev.DelegateScopeVisIDs),
	}
}
