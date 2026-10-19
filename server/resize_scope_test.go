package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// exec_resize is scoped like every other bit: an observer may resize only the
// sessions inside the bit's own target set, so "drive A and B, resize only A"
// is a grantable shape and --scope-for exec_resize means something.
func TestResizeAllowedFollowsItsOwnScope(t *testing.T) {
	f := newResumeFixture(t, protocol.Capability_ExecCowrite|protocol.Capability_ExecResize,
		Scope{Base: protocol.ScopeBase_Global})
	a := f.terminalTask(t, false)
	b := f.terminalTask(t, false)
	f.h.Tasks.mu.Lock()
	f.h.Tasks.tasks[f.selfID].Scope.Overrides = []ScopeOverride{
		{Caps: protocol.Capability_ExecResize, Base: protocol.ScopeBase_None, IDs: []string{a}},
	}
	f.h.Tasks.mu.Unlock()
	cid := f.conn.ConnectionID().String()
	if !f.h.resizeAllowed(cid, a) {
		t.Fatal("A is inside exec_resize's scope but resize was refused")
	}
	if f.h.resizeAllowed(cid, b) {
		t.Fatal("B is outside exec_resize's scope but resize was allowed")
	}
}

func TestResizeNeedsTheBit(t *testing.T) {
	f := newResumeFixture(t, protocol.Capability_ExecCowrite, Scope{Base: protocol.ScopeBase_Global})
	a := f.terminalTask(t, false)
	if f.h.resizeAllowed(f.conn.ConnectionID().String(), a) {
		t.Fatal("resize allowed without exec_resize")
	}
}

func TestOperatorMayResizeAnywhere(t *testing.T) {
	h := newTestHandler(t)
	if !h.resizeAllowed("operator-conn", "ee000000000000000000000000000000") {
		t.Fatal("an operator connection was refused a resize")
	}
}
