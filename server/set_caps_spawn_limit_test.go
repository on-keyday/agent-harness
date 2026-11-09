package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func limitReq(t *testing.T, target string, depth, max *protocol.SpawnLimit) protocol.SetCapsRequest {
	t.Helper()
	req := protocol.SetCapsRequest{TaskId: hexToTaskID(t, target)}
	if depth != nil {
		req.SpawnDepth = *depth
		req.SetSpawnDepthPresent(true)
	}
	if max != nil {
		req.MaxDescendants = *max
		req.SetMaxDescendantsPresent(true)
	}
	return req
}

func limitPtr(l protocol.SpawnLimit) *protocol.SpawnLimit { return &l }

func TestSetCapsLimitsAlone(t *testing.T) {
	h, _, c, _, _ := scopeFixture(t)
	before, _ := h.Tasks.Get(c)
	if got := sendSetCaps(t, h, opConnAt("9980"), limitReq(t, c, limitPtr(authority.LimitOf(2)), nil)); got.Status != protocol.SetCapsStatus_Ok {
		t.Fatalf("status = %v", got.Status)
	}
	e, _ := h.Tasks.Get(c)
	if !sameSpawnLimit(e.SpawnDepth, authority.LimitOf(2)) || e.MaxDescendants.Limited() {
		t.Errorf("depth-only write: depth %s max %s", authority.LimitLabel(e.SpawnDepth), authority.LimitLabel(e.MaxDescendants))
	}
	if e.Capabilities != before.Capabilities || e.DelegateCaps != before.DelegateCaps {
		t.Error("a limit-only write changed caps or the bound")
	}
	sendSetCaps(t, h, opConnAt("9981"), limitReq(t, c, nil, limitPtr(authority.LimitOf(4))))
	e, _ = h.Tasks.Get(c)
	if !sameSpawnLimit(e.SpawnDepth, authority.LimitOf(2)) || !sameSpawnLimit(e.MaxDescendants, authority.LimitOf(4)) {
		t.Errorf("max-only write: depth %s max %s, want 2 4", authority.LimitLabel(e.SpawnDepth), authority.LimitLabel(e.MaxDescendants))
	}
}

// Review Focus 3 / D13: no cascade changes a descendant's limits, whether the
// call writes caps or a limit.
func TestCascadeLeavesLimits(t *testing.T) {
	h, _, c, g, _ := scopeFixture(t)
	h.Tasks.SetCaps(g, GrantUpdate{SpawnDepth: true, MaxDescendants: true,
		To: authority.Grant{SpawnDepth: authority.LimitOf(2), MaxDescendants: authority.LimitOf(4)}}, AuthorityWrite{})

	req := protocol.SetCapsRequest{TaskId: hexToTaskID(t, c), Caps: protocol.Capability_FileRead}
	req.SetCapsPresent(true)
	req.SetCascade(true)
	sendSetCaps(t, h, opConnAt("9982"), req)

	req = limitReq(t, c, limitPtr(authority.LimitOf(0)), nil)
	req.SetCascade(true)
	sendSetCaps(t, h, opConnAt("9983"), req)

	e, _ := h.Tasks.Get(g)
	if !sameSpawnLimit(e.SpawnDepth, authority.LimitOf(2)) || !sameSpawnLimit(e.MaxDescendants, authority.LimitOf(4)) {
		t.Errorf("descendant after two cascades: depth %s max %s, want 2 4",
			authority.LimitLabel(e.SpawnDepth), authority.LimitLabel(e.MaxDescendants))
	}
	if e.Capabilities&^protocol.Capability_FileRead != 0 {
		t.Errorf("the caps cascade no longer clamps: descendant holds %s", e.Capabilities)
	}
}

func TestLimitOnlyChangeClosesNoConns(t *testing.T) {
	h, _, c, _, _ := scopeFixture(t)
	var dropped []string
	h.DropConnsForPrincipal = func(id string) int { dropped = append(dropped, id); return 1 }
	got := sendSetCaps(t, h, opConnAt("9984"), limitReq(t, c, limitPtr(authority.LimitOf(0)), limitPtr(authority.LimitOf(0))))
	if got.ConnsClosed != 0 || len(dropped) != 0 {
		t.Fatalf("a limit-only change closed %d (%v), want 0", got.ConnsClosed, dropped)
	}
}
