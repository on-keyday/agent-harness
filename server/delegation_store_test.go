package server

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

var boundX = "0123456789abcdef0123456789abcdef"

func createWithBound(s *TaskStore, dcaps protocol.Capability, dscope authority.Scope) string {
	return s.Create("/r", "p", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, authority.Grant{Caps: protocol.Capability_All, DelegateCaps: dcaps, DelegateScope: dscope}, "")
}

func wantBound(t *testing.T, what string, e TaskEntry, caps protocol.Capability, scope authority.Scope) {
	t.Helper()
	if e.DelegateCaps != caps || !reflect.DeepEqual(e.DelegateScope, scope) {
		t.Errorf("%s: bound = %s / %+v, want %s / %+v", what, e.DelegateCaps, e.DelegateScope, caps, scope)
	}
}

func TestCreateStoresBound(t *testing.T) {
	s, _ := provenanceStore(t)
	scope := authority.Scope{Base: protocol.ScopeBase_None, IDs: []string{boundX}}
	id := createWithBound(s, protocol.Capability_FileRead, scope)
	e, _ := s.Get(id)
	wantBound(t, "created", e, protocol.Capability_FileRead, scope)
}

func TestSetCapsEachHalfAlone(t *testing.T) {
	s, _ := provenanceStore(t)
	id := createWithBound(s, protocol.Capability_FileRead, authority.Scope{})
	to := authority.Grant{Caps: protocol.Capability_None, DelegateCaps: protocol.Capability_Cancel,
		DelegateScope: authority.Scope{Base: protocol.ScopeBase_None}}

	e, _ := s.SetCaps(id, GrantUpdate{DelegateCaps: true, To: to}, AuthorityWrite{})
	wantBound(t, "delegate caps only", e, protocol.Capability_Cancel, authority.Scope{})
	if e.Capabilities != protocol.Capability_All {
		t.Errorf("caps changed to %s on a bound-only write", e.Capabilities)
	}
	e, _ = s.SetCaps(id, GrantUpdate{DelegateScope: true, To: to}, AuthorityWrite{})
	wantBound(t, "delegate scope only", e, protocol.Capability_Cancel, authority.Scope{Base: protocol.ScopeBase_None})
	e, _ = s.SetCaps(id, GrantUpdate{Caps: true, To: authority.Grant{Caps: protocol.Capability_FileRead}}, AuthorityWrite{})
	wantBound(t, "caps only", e, protocol.Capability_Cancel, authority.Scope{Base: protocol.ScopeBase_None})
}

// Every task_caps_changed is a snapshot that replay assigns wholesale, so a
// write of caps alone, a resume and a forced exclude_self must all survive a
// restart with the bound they left.
func TestBoundSurvivesReplay(t *testing.T) {
	s, path := provenanceStore(t)
	a := createWithBound(s, protocol.Capability_FileRead, authority.Scope{Base: protocol.ScopeBase_Global})
	s.SetCaps(a, GrantUpdate{Caps: true, To: authority.Grant{Caps: protocol.Capability_Cancel}}, AuthorityWrite{})

	b := createWithBound(s, protocol.Capability_FileRead, authority.Scope{})
	s.Finish(b, 0, nil)
	if _, err := s.Resume(b, "p2", nil, protocol.RunnerSelector{}, "", protocol.ClientKind_Cli,
		GrantUpdate{DelegateScope: true, To: authority.Grant{DelegateScope: authority.Scope{Base: protocol.ScopeBase_None, ExcludeSelf: true}}},
		protocol.TaskKind_Oneshot, "", protocol.TaskID{}); err != nil {
		t.Fatalf("resume: %v", err)
	}

	events, _, err := ReadWAL(path)
	if err != nil {
		t.Fatalf("ReadWAL: %v", err)
	}
	fresh := NewTaskStore()
	fresh.ReplayEvents(events)
	for _, id := range []string{a, b} {
		want, _ := s.Get(id)
		got, ok := fresh.Get(id)
		if !ok {
			t.Fatalf("%s lost on replay", id[:8])
		}
		wantBound(t, "replayed "+id[:8], got, want.DelegateCaps, want.DelegateScope)
	}
	if got, _ := fresh.Get(b); !got.DelegateScope.ExcludeSelf {
		t.Error("a bound's exclude_self was lost on replay")
	}
}

// A record from before the bound replays as the default bound, and the
// board_observe → global-visibility migration of the task's own scope does not
// leak into it.
func TestLegacyRecordReplaysDefaultBound(t *testing.T) {
	var legacy WALEvent
	if err := json.Unmarshal([]byte(`{"type":"task_created","task_id":"`+boundX+
		`","repo_path":"/r","ts":1,"capabilities":1024}`), &legacy); err != nil {
		t.Fatal(err)
	}
	fresh := NewTaskStore()
	fresh.ReplayEvents([]WALEvent{legacy})
	got, ok := fresh.Get(boundX)
	if !ok {
		t.Fatal("legacy record lost")
	}
	wantBound(t, "legacy", got, protocol.Capability_None, authority.Scope{})
	if !got.Scope.VisBasePresent {
		t.Error("the task's own scope lost its legacy migration; the fixture no longer exercises it")
	}
}
