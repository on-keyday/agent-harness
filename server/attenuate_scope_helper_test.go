package server

import (
	"encoding/hex"
	"errors"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// attenuateScope is the OWN-scope half of a grant from the connection cid:
// the principal resolved as granter does, but with an unbounded delegation
// bound, so only the creator's own scope can clamp or refuse. The tests that
// call it predate the bound and pin that half alone; the bound's own tests
// (delegation_spawn_test.go) go through the handlers.
//
// It returns the offending id (or the "invalid scope: …" text) as the
// pre-bound helper did.
func (h *TaskHandler) attenuateScope(cid string, req authority.Scope, childCaps protocol.Capability) (authority.Scope, string, bool) {
	var g *authority.Granter
	if pid := h.lookupPrincipal(cid); pid.Id != ([16]byte{}) {
		id := hex.EncodeToString(pid.Id[:])
		held := authority.Grant{Scope: authority.Scope{Base: protocol.ScopeBase_None}}
		if t, ok := h.Tasks.Get(id); ok {
			held.Scope = t.Scope
		}
		held.Caps, held.DelegateCaps = protocol.Capability_All, protocol.Capability_All
		held.DelegateScope = authority.Scope{Base: protocol.ScopeBase_Global}
		g = authority.NewGranter(id, held, h.childIndex())
	}
	out, err := g.ChildScope(req, childCaps)
	var reach *authority.OutOfReachError
	switch {
	case err == nil:
		return out, "", true
	case errors.As(err, &reach):
		return authority.Scope{}, reach.ID, false
	default:
		return authority.Scope{}, err.Error(), false
	}
}
