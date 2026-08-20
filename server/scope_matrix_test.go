package server

import (
	"math/rand"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The legal-value matrix of the 2026-08-20 design §11, as a table. It exists so
// the model can be reviewed by inspection rather than by argument, and so the
// spec's table and the code's behaviour have exactly one place to disagree.

// §11, rank pair: legal iff rank(base) <= rank(visRank).
func TestScopeMatrixRankPairs(t *testing.T) {
	vis := func(base, visBase protocol.ScopeBase) Scope {
		return Scope{Base: base, VisBasePresent: true, VisBase: visBase}
	}
	for _, tc := range []struct {
		name    string
		scope   Scope
		wantErr bool
	}{
		{"none / none", vis(protocol.ScopeBase_None, protocol.ScopeBase_None), false},
		{"none / subtree", vis(protocol.ScopeBase_None, protocol.ScopeBase_Subtree), false},
		{"none / global", vis(protocol.ScopeBase_None, protocol.ScopeBase_Global), false},
		{"subtree / subtree", vis(protocol.ScopeBase_Subtree, protocol.ScopeBase_Subtree), false},
		{"subtree / global", vis(protocol.ScopeBase_Subtree, protocol.ScopeBase_Global), false},
		{"global / global", vis(protocol.ScopeBase_Global, protocol.ScopeBase_Global), false},
		{"subtree act, none vis", vis(protocol.ScopeBase_Subtree, protocol.ScopeBase_None), true},
		{"global act, none vis", vis(protocol.ScopeBase_Global, protocol.ScopeBase_None), true},
		{"global act, subtree vis", vis(protocol.ScopeBase_Global, protocol.ScopeBase_Subtree), true},
	} {
		if err := validateScope(tc.scope); (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, wantErr = %v", tc.name, err, tc.wantErr)
		}
	}
}

// §11: vis_base_present = 0 pins the pair to the diagonal, so the default and
// every pre-change record are legal by construction. An illegal pair takes an
// explicit ask.
func TestScopeMatrixAbsentVisibilityIsAlwaysLegal(t *testing.T) {
	for _, base := range []protocol.ScopeBase{
		protocol.ScopeBase_None, protocol.ScopeBase_Subtree, protocol.ScopeBase_Global,
	} {
		if err := validateScope(Scope{Base: base}); err != nil {
			t.Errorf("base %v with no visibility rank: %v", base, err)
		}
	}
}

// §11: an override may be WIDER than the action base as long as it is within
// visibility. That is the case a single scope had to refuse.
func TestScopeMatrixOverrideRankAgainstVisibilityNotBase(t *testing.T) {
	for _, tc := range []struct {
		name    string
		scope   Scope
		wantErr bool
	}{
		{"override narrower than base", Scope{
			Base:      protocol.ScopeBase_Subtree,
			Overrides: []ScopeOverride{{Caps: protocol.Capability_Cancel, Base: protocol.ScopeBase_None}},
		}, false},
		{"override wider than base, within visibility", Scope{
			Base: protocol.ScopeBase_None, VisBasePresent: true, VisBase: protocol.ScopeBase_Global,
			Overrides: []ScopeOverride{{Caps: protocol.Capability_ExecView, Base: protocol.ScopeBase_Global}},
		}, false},
		{"override outranks visibility", Scope{
			Base: protocol.ScopeBase_None, VisBasePresent: true, VisBase: protocol.ScopeBase_None,
			Overrides: []ScopeOverride{{Caps: protocol.Capability_ExecControl, Base: protocol.ScopeBase_Subtree}},
		}, true},
	} {
		if err := validateScope(tc.scope); (err != nil) != tc.wantErr {
			t.Errorf("%s: err = %v, wantErr = %v", tc.name, err, tc.wantErr)
		}
	}
}

// §11, "accepted and deliberately not errors": these read like mistakes.
// Rejecting them would turn a redundant grant into a failed spawn.
func TestScopeMatrixRedundantFormsAreAccepted(t *testing.T) {
	id := "00112233445566778899aabbccddeeff"
	for _, tc := range []struct {
		name  string
		scope Scope
	}{
		{"ids under a global base", Scope{
			Base: protocol.ScopeBase_Global, VisBasePresent: true, VisBase: protocol.ScopeBase_Global,
			IDs: []string{id}}},
		{"vis_ids under a global visibility rank", Scope{
			Base: protocol.ScopeBase_Subtree, VisBasePresent: true, VisBase: protocol.ScopeBase_Global,
			VisIDs: []string{id}}},
		{"a vis_id that is also an action id", Scope{
			Base: protocol.ScopeBase_Subtree, IDs: []string{id}, VisIDs: []string{id}}},
		{"an override for a bit nothing holds", Scope{
			Base:      protocol.ScopeBase_Subtree,
			Overrides: []ScopeOverride{{Caps: protocol.Capability_Purge, Base: protocol.ScopeBase_None}}}},
		{"the empty action set", Scope{Base: protocol.ScopeBase_None, ExcludeSelf: true}},
	} {
		if err := validateScope(tc.scope); err != nil {
			t.Errorf("%s: rejected (%v); a redundant grant must not become a failed spawn", tc.name, err)
		}
	}
}

// §11's numbered rejection list, including the one that is about encoding
// rather than authority.
func TestScopeMatrixEveryRejection(t *testing.T) {
	for _, tc := range []struct {
		name  string
		scope Scope
	}{
		{"1: base outranks visibility", Scope{
			Base: protocol.ScopeBase_Subtree, VisBasePresent: true, VisBase: protocol.ScopeBase_None}},
		{"2: override outranks visibility", Scope{
			Base: protocol.ScopeBase_None, VisBasePresent: true, VisBase: protocol.ScopeBase_None,
			Overrides: []ScopeOverride{{Caps: protocol.Capability_Cancel, Base: protocol.ScopeBase_Global}}}},
		{"3: empty override mask", Scope{
			Overrides: []ScopeOverride{{Caps: protocol.Capability_None}}}},
		{"4: override masks intersect", Scope{Overrides: []ScopeOverride{
			{Caps: protocol.Capability_Cancel | protocol.Capability_Purge},
			{Caps: protocol.Capability_Purge},
		}}},
		{"5: vis_base set without its presence bit", Scope{VisBase: protocol.ScopeBase_Global}},
	} {
		if err := validateScope(tc.scope); err == nil {
			t.Errorf("%s: accepted, want a rejection", tc.name)
		}
	}
}

// The invariant, over randomised legal grants: no capability may act outside
// what ls shows. This is the assertion that catches the axes being collapsed
// in EITHER direction, which prose review kept nearly missing.
func TestEffectiveIsAlwaysWithinVisible(t *testing.T) {
	rng := rand.New(rand.NewSource(20260820)) // fixed: a failure must reproduce
	bases := []protocol.ScopeBase{
		protocol.ScopeBase_None, protocol.ScopeBase_Subtree, protocol.ScopeBase_Global,
	}
	bits := []protocol.Capability{
		protocol.Capability_Cancel, protocol.Capability_ExecView,
		protocol.Capability_ExecCowrite, protocol.Capability_FileRead,
		protocol.Capability_FileWrite,
	}
	// Ids are part of the generator, not decoration. The first version of this
	// test randomised only ranks and flags, so the "a granted id joins the
	// visible set" path was never exercised -- deleting that union from
	// visibilitySet left the test GREEN. A guard that cannot fail is worse
	// than none, so the negative control is what these ids exist to satisfy.
	idPool := []string{
		"00112233445566778899aabbccddee01",
		"00112233445566778899aabbccddee02",
		"00112233445566778899aabbccddee03",
	}
	pick := func() []string {
		switch rng.Intn(3) {
		case 0:
			return nil
		case 1:
			return []string{idPool[rng.Intn(len(idPool))]}
		default:
			return []string{idPool[0], idPool[rng.Intn(len(idPool))]}
		}
	}

	checked := 0
	sawOverrideIDs := false
	for i := 0; i < 400; i++ {
		s := Scope{
			Base:           bases[rng.Intn(len(bases))],
			VisBasePresent: rng.Intn(2) == 1,
			ExcludeSelf:    rng.Intn(2) == 1,
		}
		if s.VisBasePresent {
			s.VisBase = bases[rng.Intn(len(bases))]
		}
		s.IDs = pick()
		s.VisIDs = pick()
		// Disjoint by construction: each chosen bit goes to at most one entry.
		perm := rng.Perm(len(bits))
		for n := 0; n < rng.Intn(3); n++ {
			ovIDs := pick()
			if len(ovIDs) > 0 {
				sawOverrideIDs = true
			}
			s.Overrides = append(s.Overrides, ScopeOverride{
				Caps:        bits[perm[n]],
				Base:        bases[rng.Intn(len(bases))],
				ExcludeSelf: rng.Intn(2) == 1,
				IDs:         ovIDs,
			})
		}
		if err := validateScope(s); err != nil {
			continue // illegal by construction; the matrix tests cover those
		}

		h, _, c, _, _ := scopeFixture(t)
		cid := bindPrincipal(t, h, c)
		setScope(t, h, c, s)

		_, visible := h.scopeSet(cid, protocol.Capability_None)
		if visible == nil {
			continue // global visibility: inclusion is trivial
		}
		checked++
		for _, bit := range bits {
			all, allowed := h.scopeSet(cid, bit)
			if all {
				t.Fatalf("case %d: %v resolved to ALL while visibility is bounded — "+
					"an action set escaped the visible set\nscope: %+v", i, bit, s)
			}
			for id := range allowed {
				if !visible[id] {
					t.Fatalf("case %d: %v reaches %s, which visibility does not include\nscope: %+v",
						i, bit, id, s)
				}
			}
		}
	}
	if checked < 20 {
		t.Fatalf("only %d cases actually exercised the invariant — the generator is "+
			"producing values the filter throws away, so a green run proves little", checked)
	}
	if !sawOverrideIDs {
		t.Fatal("no generated scope carried an override id, so the path where a granted " +
			"id joins the visible set went untested — this is the exact hole the first " +
			"version of this test had")
	}
}
