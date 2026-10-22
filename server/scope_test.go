package server

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// SetCaps is the store half of the operator's live re-grant. Unlike Resume it
// has no terminal-state precondition — the whole point is to change a task
// that is still Running — and it must persist BOTH halves in one record.
func TestSetCapsRewritesRunningTaskAndPersistsBoth(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "setcaps.log")
	wal, err := OpenWAL(walPath)
	if err != nil {
		t.Fatalf("OpenWAL: %v", err)
	}
	s := NewTaskStore()
	s.SetWAL(wal)

	id := s.Create("/repo", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_All, Scope: authority.Scope{}},
		"")

	s.Assign(id, testRunnerID("runner-x"), "/wt/x", false)
	if got, _ := s.Get(id); got.Status != protocol.TaskStatus_Running {
		t.Fatalf("status = %v, want Running (the precondition this method must NOT have)", got.Status)
	}

	peer := strings.Repeat("ab", 16)
	e, ok := s.SetCaps(id, GrantUpdate{Caps: true, Scope: true, To: authority.Grant{Caps: protocol.Capability_Cancel, Scope: authority.Scope{Base: protocol.ScopeBase_None, IDs: []string{peer}}}},
		AuthorityWrite{})

	if !ok {
		t.Fatal("SetCaps reported the task missing")
	}
	if e.Capabilities != protocol.Capability_Cancel {
		t.Errorf("caps = %v, want cancel", e.Capabilities)
	}
	if e.Scope.Base != protocol.ScopeBase_None || len(e.Scope.IDs) != 1 || e.Scope.IDs[0] != peer {
		t.Errorf("scope = %+v, want none+ids:%s", e.Scope, peer)
	}

	// Half-writes must leave the other half alone.
	if e2, _ := s.SetCaps(id, GrantUpdate{Caps: false, Scope: true, To: authority.Grant{Caps: protocol.Capability_All, Scope: authority.Scope{Base: protocol.ScopeBase_Global}}}, AuthorityWrite{}); true {
		if e2.Capabilities != protocol.Capability_Cancel {
			t.Errorf("caps changed on a scope-only write: %v", e2.Capabilities)
		}
		if e2.Scope.Base != protocol.ScopeBase_Global {
			t.Errorf("scope = %v, want global", e2.Scope.Base)
		}
	}
	if _, ok := s.SetCaps("no-such-task", GrantUpdate{Caps: true, Scope: false, To: authority.Grant{Caps: protocol.Capability_None, Scope: authority.Scope{}}}, AuthorityWrite{}); ok {
		t.Error("SetCaps on a missing task reported ok")
	}

	wal.Close() //nolint:errcheck
	events, _, readErr := ReadWAL(walPath)
	if readErr != nil {
		t.Fatalf("ReadWAL: %v", readErr)
	}
	var last *WALEvent
	for i := range events {
		if events[i].Type == "task_caps_changed" && events[i].TaskID == id {
			last = &events[i]
		}
	}
	if last == nil {
		t.Fatal("no task_caps_changed event written")
	}
	// The record is a snapshot, not a delta: the scope-only write must still
	// carry the caps, or replay would reset them to zero.
	if protocol.Capability(last.Capabilities) != protocol.Capability_Cancel {
		t.Errorf("WAL caps = %v, want cancel carried through a scope-only write",
			protocol.Capability(last.Capabilities))
	}
	if protocol.ScopeBase(last.ScopeBase) != protocol.ScopeBase_Global {
		t.Errorf("WAL scope base = %v, want global", protocol.ScopeBase(last.ScopeBase))
	}
}

// A record from before scopes, and one from before the visibility axis whose
// caps carry board_observe, replay to the authority they had: the subtree
// default, and global visibility respectively.
func TestScopeFromWALLegacyRecords(t *testing.T) {
	if got := scopeFromWAL(WALEvent{}); got.Base != protocol.ScopeBase_Subtree || got.VisBasePresent {
		t.Errorf("empty record = %+v, want the subtree default", got)
	}
	got := scopeFromWAL(WALEvent{Capabilities: uint32(protocol.Capability_BoardObserve)})
	if got.VisRank() != protocol.ScopeBase_Global {
		t.Errorf("legacy board_observe record vis = %v, want global", got.VisRank())
	}
}
