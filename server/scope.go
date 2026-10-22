package server

import (
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
			Caps: uint32(o.Caps), Base: uint8(o.Base), ExcludeSelf: o.ExcludeSelf, IDs: o.IDs,
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
		})
	}
	if !s.VisBasePresent && protocol.Capability(ev.Capabilities)&protocol.Capability_BoardObserve != 0 {
		s.VisBasePresent = true
		s.VisBase = protocol.ScopeBase_Global
	}
	return s
}

// applyDelegationToWAL / delegationFromWAL are the same projection for the
// delegation bound, under the delegate_ key prefix. A bound carries no
// overrides, so there is no override list to write.
//
// scopeFromWAL's board_observe migration deliberately has no counterpart: it
// exists for records older than the visibility axis, and every bound is newer.
func applyDelegationToWAL(caps protocol.Capability, s authority.Scope, ev *WALEvent) {
	ev.DelegateCaps = uint32(caps)
	ev.DelegateScopeBase = uint8(s.Base)
	ev.DelegateScopeVisBase = uint8(s.VisBase)
	ev.DelegateScopeVisBasePresent = s.VisBasePresent
	ev.DelegateScopeExcludeSelf = s.ExcludeSelf
	ev.DelegateScopeIDs = s.IDs
	ev.DelegateScopeVisIDs = s.VisIDs
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
