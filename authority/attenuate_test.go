package authority

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// entry is a task's authority as IsNarrowing compares it.
type entry struct {
	caps  protocol.Capability
	scope Scope
}

func narrow(before, after entry) bool {
	return IsNarrowing(before.caps, before.scope, after.caps, after.scope)
}

// An operator creator is not bounded: the request comes back as written,
// only validated for consistency.
func TestAttenuateOperatorIsUnbounded(t *testing.T) {
	req := Scope{Base: protocol.ScopeBase_Global, IDs: []string{strings.Repeat("ab", 16)}}
	got, _, ok := Attenuate(req, protocol.Capability_All, nil)
	if !ok || got.Base != protocol.ScopeBase_Global || len(got.IDs) != 1 {
		t.Errorf("operator = %+v ok=%v, want the request unchanged", got, ok)
	}
	if _, off, ok := Attenuate(Scope{Overrides: []ScopeOverride{{Caps: protocol.Capability_Spawn}}}, protocol.Capability_All, nil); ok || !strings.HasPrefix(off, "invalid scope: ") {
		t.Errorf("operator incoherent scope: ok=%v offender=%q, want refused", ok, off)
	}
}

// A cascade must clamp EVERY axis. The inline literal this replaced carried
// Base and IDs only, so a cascade silently erased a descendant's visibility
// rank and its overrides — a rewrite of authority that nothing reported.
func TestClampScopeUnderCarriesEveryAxis(t *testing.T) {
	parent := NewReach("00112233445566778899aabbccddeeff", Scope{Base: protocol.ScopeBase_None}, nil)
	child := Scope{
		Base: protocol.ScopeBase_Subtree, VisBasePresent: true, VisBase: protocol.ScopeBase_Global,
		ExcludeSelf: true,
		Overrides: []ScopeOverride{
			{Caps: protocol.Capability_Cancel, Base: protocol.ScopeBase_Subtree},
			{Caps: protocol.Capability_Purge, Base: protocol.ScopeBase_Subtree},
		},
	}

	// The descendant keeps cancel but loses purge.
	out := ClampUnder(child, protocol.Capability_Cancel, parent)

	if out.Base != protocol.ScopeBase_None {
		t.Errorf("Base = %v, want clamped to none", out.Base)
	}
	if out.VisRank() != protocol.ScopeBase_None {
		t.Errorf("VisRank = %v, want clamped to none — the rank must not survive a narrowing", out.VisRank())
	}
	if !out.ExcludeSelf {
		t.Error("ExcludeSelf was dropped; it only ever narrows and must be kept")
	}
	if len(out.Overrides) != 1 || out.Overrides[0].Caps != protocol.Capability_Cancel {
		t.Fatalf("overrides = %+v, want the cancel entry only", out.Overrides)
	}
	if out.Overrides[0].Base != protocol.ScopeBase_None {
		t.Errorf("override base = %v, want clamped to none", out.Overrides[0].Base)
	}
	if err := Validate(out); err != nil {
		t.Errorf("a clamped scope must stay legal: %v", err)
	}
}

// An override for a bit the descendant no longer holds is dropped, not left to
// reapply if the bit is granted again later.
func TestClampScopeUnderDropsOverridesForLostBits(t *testing.T) {
	parent := NewReach("00112233445566778899aabbccddeeff", Scope{Base: protocol.ScopeBase_Subtree}, nil)
	child := Scope{
		Base:      protocol.ScopeBase_Subtree,
		Overrides: []ScopeOverride{{Caps: protocol.Capability_Cancel | protocol.Capability_Purge}},
	}
	out := ClampUnder(child, protocol.Capability_Cancel, parent)
	if len(out.Overrides) != 1 {
		t.Fatalf("overrides = %+v, want one surviving entry", out.Overrides)
	}
	if out.Overrides[0].Caps != protocol.Capability_Cancel {
		t.Errorf("mask = %v, want purge masked out of it", out.Overrides[0].Caps)
	}
}

// Narrowing the scope alone — no cap bit touched — still counts.
func TestScopeOnlyNarrowingCountsAsNarrowing(t *testing.T) {
	before := entry{protocol.Capability_All, Scope{Base: protocol.ScopeBase_Subtree, IDs: []string{"aa", "bb"}}}
	if !narrow(before, entry{protocol.Capability_All, Scope{Base: protocol.ScopeBase_None, IDs: []string{"aa", "bb"}}}) {
		t.Error("a base ranked down is not reported as a narrowing")
	}
	if !narrow(before, entry{protocol.Capability_All, Scope{Base: protocol.ScopeBase_Subtree, IDs: []string{"aa"}}}) {
		t.Error("a dropped scope id is not reported as a narrowing")
	}
	if narrow(before, entry{protocol.Capability_All, Scope{Base: protocol.ScopeBase_Global, IDs: []string{"aa", "bb", "cc"}}}) {
		t.Error("a pure widening was reported as a narrowing")
	}
}

// A narrowing of ONE bit's scope, or of what a task can see, is still a
// narrowing: an attach opened under the wider grant must not outlive it.
func TestPerBitAndVisibilityNarrowingCountsAsNarrowing(t *testing.T) {
	ov := func(b protocol.ScopeBase) Scope {
		return Scope{Base: protocol.ScopeBase_Subtree,
			Overrides: []ScopeOverride{{Caps: protocol.Capability_Cancel, Base: b}}}
	}
	caps := protocol.Capability_Cancel
	if !narrow(entry{caps, ov(protocol.ScopeBase_Subtree)},
		entry{caps, ov(protocol.ScopeBase_None)}) {
		t.Error("an override ranked down is not reported as a narrowing")
	}
	if narrow(entry{caps, ov(protocol.ScopeBase_None)},
		entry{caps, ov(protocol.ScopeBase_Global)}) {
		t.Error("an override ranked up was reported as a narrowing")
	}
	wide := Scope{Base: protocol.ScopeBase_Subtree, VisBasePresent: true, VisBase: protocol.ScopeBase_Global}
	if !narrow(entry{0, wide}, entry{0, Scope{Base: protocol.ScopeBase_Subtree}}) {
		t.Error("a visibility rank ranked down is not reported as a narrowing")
	}
	if !narrow(entry{0, Scope{VisIDs: []string{"aa"}}}, entry{0, Scope{}}) {
		t.Error("a dropped view-only id is not reported as a narrowing")
	}
}
