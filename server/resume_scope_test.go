package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TestResumeScopePresence: the two halves of authority re-grant independently
// on resume — caps behind capsOverride, scope behind scopeOverride
// (scope_present on the wire). Before the presence bit, both rode the caps
// override: --caps on a resume silently reset a task's scope to the request
// default, and a lone --scope was silently ignored.
func TestResumeScopePresence(t *testing.T) {
	sib := protocol.TaskID{}
	sib.Id[0] = 0xbb
	kept := Scope{Base: protocol.ScopeBase_None, IDs: []string{"bb"}}

	t.Run("caps_override_alone_keeps_scope", func(t *testing.T) {
		h := newTestHandler(t)
		id := h.Tasks.Create("/r", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli,
			protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_Spawn, kept, "")
		markTerminalForTest(t, h, id)

		if _, err := h.Tasks.Resume(id, "", nil, protocol.RunnerSelector{}, "", protocol.ClientKind_Cli,
			true, protocol.Capability_FileRead, false, Scope{}, protocol.TaskKind_Oneshot, ""); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		e, _ := h.Tasks.Get(id)
		if e.Capabilities != protocol.Capability_FileRead {
			t.Fatalf("caps = %#x, want FileRead", e.Capabilities)
		}
		if e.Scope.Base != protocol.ScopeBase_None || len(e.Scope.IDs) != 1 {
			t.Fatalf("scope changed to %+v — caps override alone must keep it", e.Scope)
		}
	})

	t.Run("scope_presence_alone_applies_scope_keeps_caps", func(t *testing.T) {
		h := newTestHandler(t)
		id := h.Tasks.Create("/r", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli,
			protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_Spawn, kept, "")
		markTerminalForTest(t, h, id)

		if _, err := h.Tasks.Resume(id, "", nil, protocol.RunnerSelector{}, "", protocol.ClientKind_Cli,
			false, protocol.Capability_None, true, Scope{Base: protocol.ScopeBase_Global}, protocol.TaskKind_Oneshot, ""); err != nil {
			t.Fatalf("Resume: %v", err)
		}
		e, _ := h.Tasks.Get(id)
		if e.Capabilities != protocol.Capability_Spawn {
			t.Fatalf("caps changed to %#x — scope presence alone must keep them", e.Capabilities)
		}
		if e.Scope.Base != protocol.ScopeBase_Global {
			t.Fatalf("scope = %+v, want global applied", e.Scope)
		}
	})
}
