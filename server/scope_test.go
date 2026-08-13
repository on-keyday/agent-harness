package server

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func hexID(b byte) string { return strings.Repeat(string("0123456789abcdef"[b&0xf]), taskIDHexLen) }

func TestMinScopeBaseRanksByPermissiveness(t *testing.T) {
	unknown := protocol.ScopeBase(9)
	cases := []struct{ a, b, want protocol.ScopeBase }{
		{protocol.ScopeBase_Subtree, protocol.ScopeBase_Global, protocol.ScopeBase_Subtree},
		{protocol.ScopeBase_Global, protocol.ScopeBase_Subtree, protocol.ScopeBase_Subtree},
		{protocol.ScopeBase_None, protocol.ScopeBase_Subtree, protocol.ScopeBase_None},
		{protocol.ScopeBase_Subtree, protocol.ScopeBase_None, protocol.ScopeBase_None},
		{protocol.ScopeBase_Global, protocol.ScopeBase_Global, protocol.ScopeBase_Global},
		// A base from a newer peer must rank as none, so it cannot widen.
		{unknown, protocol.ScopeBase_Global, unknown},
		{protocol.ScopeBase_Global, unknown, unknown},
	}
	for _, c := range cases {
		if got := minScopeBase(c.a, c.b); got != c.want {
			t.Errorf("minScopeBase(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
	if scopeBaseRank(protocol.ScopeBase(9)) != scopeBaseRank(protocol.ScopeBase_None) {
		t.Error("an unrecognised base must rank as none")
	}
}

func TestScopeWireRoundTripNormalises(t *testing.T) {
	a, b := hexID(0xa), hexID(0xb)
	in := Scope{Base: protocol.ScopeBase_None, IDs: []string{b, a, a}}
	got := scopeFromWire(in.toWire())
	if got.Base != protocol.ScopeBase_None {
		t.Fatalf("base = %v, want none", got.Base)
	}
	if len(got.IDs) != 2 || got.IDs[0] != a || got.IDs[1] != b {
		t.Fatalf("ids = %v, want sorted+deduped [%s %s]", got.IDs, a, b)
	}
}

func TestScopeFromWireDropsMalformedIDs(t *testing.T) {
	// A wire scope can only carry well-formed 16-byte ids, so the drop path is
	// exercised through the server-side struct instead: a short hex string must
	// not survive a round trip as a padded id that would match nothing.
	s := Scope{Base: protocol.ScopeBase_Subtree, IDs: []string{"aa", "", hexID(1)}}
	w := s.toWire()
	if w.IdsLen != 1 || len(w.Ids) != 1 {
		t.Fatalf("IdsLen = %d, want 1 (short and empty hex dropped)", w.IdsLen)
	}
	if got := scopeFromWire(w); len(got.IDs) != 1 || got.IDs[0] != hexID(1) {
		t.Fatalf("round trip = %v, want [%s]", got.IDs, hexID(1))
	}
}

func TestScopeString(t *testing.T) {
	a := hexID(0xa)
	for _, c := range []struct {
		in   Scope
		want string
	}{
		{Scope{}, "subtree"},
		{defaultScope(), "subtree"},
		{Scope{Base: protocol.ScopeBase_None}, "none"},
		{Scope{Base: protocol.ScopeBase_Global}, "global"},
		{Scope{Base: protocol.ScopeBase_None, IDs: []string{a}}, "ids:" + a},
		{Scope{Base: protocol.ScopeBase_Subtree, IDs: []string{a}}, "subtree+ids:" + a},
	} {
		if got := c.in.String(); got != c.want {
			t.Errorf("Scope%+v.String() = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDefaultScopeIsTheZeroValue(t *testing.T) {
	d := defaultScope()
	if d.Base != (Scope{}).Base || len(d.IDs) != 0 {
		t.Fatal("defaultScope must be the zero value so an absent scope reads as subtree")
	}
}

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
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_All, Scope{}, "")
	s.Assign(id, "runner-x", "/wt/x")
	if got, _ := s.Get(id); got.Status != protocol.TaskStatus_Running {
		t.Fatalf("status = %v, want Running (the precondition this method must NOT have)", got.Status)
	}

	peer := strings.Repeat("ab", 16)
	e, ok := s.SetCaps(id, true, protocol.Capability_Cancel, true,
		Scope{Base: protocol.ScopeBase_None, IDs: []string{peer}})
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
	if e2, _ := s.SetCaps(id, false, protocol.Capability_All, true, Scope{Base: protocol.ScopeBase_Global}); true {
		if e2.Capabilities != protocol.Capability_Cancel {
			t.Errorf("caps changed on a scope-only write: %v", e2.Capabilities)
		}
		if e2.Scope.Base != protocol.ScopeBase_Global {
			t.Errorf("scope = %v, want global", e2.Scope.Base)
		}
	}
	if _, ok := s.SetCaps("no-such-task", true, protocol.Capability_None, false, Scope{}); ok {
		t.Error("SetCaps on a missing task reported ok")
	}

	wal.Close() //nolint:errcheck
	events, readErr := ReadWAL(walPath)
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
