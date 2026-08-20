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
	got := scopeFromWire(in.toWire(), in.overridesToWire())
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
	if got := scopeFromWire(w, nil); len(got.IDs) != 1 || got.IDs[0] != hexID(1) {
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
	s.Assign(id, "runner-x", "/wt/x", false)
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

// VisRank follows the action base unless the presence bit says otherwise. The
// absent case is what pins the rank pair to the diagonal, so a legacy record
// and a zero struct can never encode an illegal pair.
func TestVisRankFollowsBaseWhenAbsent(t *testing.T) {
	s := Scope{Base: protocol.ScopeBase_None}
	if got := s.VisRank(); got != protocol.ScopeBase_None {
		t.Errorf("VisRank = %v, want none (follows base)", got)
	}
	s.VisBasePresent = true
	s.VisBase = protocol.ScopeBase_Global
	if got := s.VisRank(); got != protocol.ScopeBase_Global {
		t.Errorf("VisRank = %v, want global", got)
	}
}

// An override binds the bits in its mask and no others. The leak this guards
// against is one bit's narrowing silently applying to a neighbour.
func TestForCapPrefersTheOverride(t *testing.T) {
	s := Scope{
		Base: protocol.ScopeBase_Subtree,
		Overrides: []ScopeOverride{{
			Caps: protocol.Capability_ExecCowrite | protocol.Capability_FileWrite,
			Base: protocol.ScopeBase_Subtree, ExcludeSelf: true,
		}},
	}
	for _, c := range []protocol.Capability{protocol.Capability_ExecCowrite, protocol.Capability_FileWrite} {
		if _, ex, _ := s.ForCap(c); !ex {
			t.Errorf("%v: ExcludeSelf = false, want true from the mask", c)
		}
	}
	if _, ex, _ := s.ForCap(protocol.Capability_ExecView); ex {
		t.Error("exec_view picked up an override that does not name it")
	}
}

// validateScope is the CONSISTENCY list. Ranks are deliberately absent from
// it — an action rank above the visibility rank is legal, because "cannot
// enumerate" and "cannot act" are different powers and an operator may want
// the first without the second. Ids are absent too: their bound is the
// parent's set at grant time, not the task's base.
func TestValidateScopeRejections(t *testing.T) {
	cases := []struct {
		name string
		s    Scope
	}{
		{"empty mask", Scope{
			Overrides: []ScopeOverride{{Caps: protocol.Capability_None}}}},
		{"masks intersect", Scope{Overrides: []ScopeOverride{
			{Caps: protocol.Capability_ExecView | protocol.Capability_Cancel},
			{Caps: protocol.Capability_Cancel},
		}}},
		{"non-canonical vis_base", Scope{VisBase: protocol.ScopeBase_Global}},
	}
	for _, tc := range cases {
		if err := validateScope(tc.s); err == nil {
			t.Errorf("%s: validateScope = nil, want an error", tc.name)
		}
	}
}

// The shapes that look wrong and are legal. Rejecting these would turn a
// redundant grant into a failed spawn.
func TestValidateScopeAccepts(t *testing.T) {
	id := hexID(7)
	cases := []struct {
		name string
		s    Scope
	}{
		{"the zero value", Scope{}},
		{"named reach out of a blind base", Scope{
			Base: protocol.ScopeBase_None, VisBasePresent: true, VisBase: protocol.ScopeBase_None,
			Overrides: []ScopeOverride{{Caps: protocol.Capability_Cancel, Base: protocol.ScopeBase_None, IDs: []string{id}}}}},
		// The board-driven observer: enumerates nothing, looks at whatever it
		// is handed an id for. The rank rule used to forbid exactly this.
		{"action rank above the visibility rank", Scope{
			Base: protocol.ScopeBase_None, VisBasePresent: true, VisBase: protocol.ScopeBase_None,
			Overrides: []ScopeOverride{{Caps: protocol.Capability_ExecView, Base: protocol.ScopeBase_Global}}}},
		{"a global action base under a none visibility rank", Scope{
			Base: protocol.ScopeBase_Global, VisBasePresent: true, VisBase: protocol.ScopeBase_None}},
		{"override wider than base, within visibility", Scope{
			Base: protocol.ScopeBase_None, VisBasePresent: true, VisBase: protocol.ScopeBase_Global,
			Overrides: []ScopeOverride{{Caps: protocol.Capability_ExecView, Base: protocol.ScopeBase_Global}}}},
		{"ids redundant under a global base", Scope{
			Base: protocol.ScopeBase_Global, VisBasePresent: true, VisBase: protocol.ScopeBase_Global,
			IDs: []string{id}}},
		{"the empty action set", Scope{
			Base: protocol.ScopeBase_None, ExcludeSelf: true}},
	}
	for _, tc := range cases {
		if err := validateScope(tc.s); err != nil {
			t.Errorf("%s: validateScope = %v, want nil", tc.name, err)
		}
	}
}
