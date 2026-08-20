package cli

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Item 32 of the surface-parity checklist says the scope serializers are
// "pinned by tests; round-trip tests keep them honest". They were not: neither
// the Go picker copy nor the JS copy had one, and both were lossy — three of
// the six bases and neither half of the visibility pair. These are the tests
// that sentence was describing.

const (
	idA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	idB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// TestScopeLabelRoundTripsThroughParseScope is the property everything else
// here leans on: whatever ScopeLabel prints, ParseScope must read back as the
// same scope. ls prints with the first and the UIs send through the second.
func TestScopeLabelRoundTripsThroughParseScope(t *testing.T) {
	mk := func(base protocol.ScopeBase, excludeSelf bool, ids []string,
		visBase protocol.ScopeBase, visPresent bool, visIDs []string) protocol.TaskScope {
		sc := protocol.TaskScope{Base: base, VisBase: visBase}
		sc.SetExcludeSelf(excludeSelf)
		sc.SetVisBasePresent(visPresent)
		for _, h := range ids {
			sc.Ids = append(sc.Ids, mustTaskID(t, h))
		}
		sc.IdsLen = uint16(len(sc.Ids))
		for _, h := range visIDs {
			sc.VisIds = append(sc.VisIds, mustTaskID(t, h))
		}
		sc.VisIdsLen = uint16(len(sc.VisIds))
		return sc
	}

	cases := []struct {
		name  string
		scope protocol.TaskScope
		want  string
	}{
		{"subtree", mk(protocol.ScopeBase_Subtree, false, nil, 0, false, nil), "subtree"},
		{"descendants", mk(protocol.ScopeBase_Subtree, true, nil, 0, false, nil), "descendants"},
		{"none", mk(protocol.ScopeBase_None, false, nil, 0, false, nil), "none"},
		{"none-self", mk(protocol.ScopeBase_None, true, nil, 0, false, nil), "none-self"},
		{"global", mk(protocol.ScopeBase_Global, false, nil, 0, false, nil), "global"},
		{"global-self", mk(protocol.ScopeBase_Global, true, nil, 0, false, nil), "global-self"},
		{"ids", mk(protocol.ScopeBase_None, false, []string{idA}, 0, false, nil), "ids:" + idA},
		{"subtree+ids", mk(protocol.ScopeBase_Subtree, false, []string{idA, idB}, 0, false, nil),
			"subtree+ids:" + idA + "," + idB},
		{"descendants+ids", mk(protocol.ScopeBase_Subtree, true, []string{idA}, 0, false, nil),
			"descendants+ids:" + idA},
		{"none-self+ids", mk(protocol.ScopeBase_None, true, []string{idA}, 0, false, nil),
			"none-self+ids:" + idA},
		{"vis rank", mk(protocol.ScopeBase_Subtree, false, nil, protocol.ScopeBase_Global, true, nil),
			"global/subtree"},
		{"vis rank + descendants", mk(protocol.ScopeBase_Subtree, true, nil, protocol.ScopeBase_Global, true, nil),
			"global/descendants"},
		{"vis ids", mk(protocol.ScopeBase_None, false, nil, 0, false, []string{idB}),
			"none+vis-ids:" + idB},
		{"both halves", mk(protocol.ScopeBase_Subtree, true, []string{idA}, protocol.ScopeBase_Global, true, []string{idB}),
			"global/descendants+ids:" + idA + "+vis-ids:" + idB},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ScopeLabel(tc.scope)
			if got != tc.want {
				t.Fatalf("ScopeLabel = %q, want %q", got, tc.want)
			}
			back, err := ParseScope(got)
			if err != nil {
				t.Fatalf("ParseScope(%q): %v", got, err)
			}
			if again := ScopeLabel(back); again != got {
				t.Errorf("round trip: %q -> %q", got, again)
			}
			if back.Base != tc.scope.Base || back.ExcludeSelf() != tc.scope.ExcludeSelf() {
				t.Errorf("base/self = %v/%v, want %v/%v",
					back.Base, back.ExcludeSelf(), tc.scope.Base, tc.scope.ExcludeSelf())
			}
			if back.VisBasePresent() != tc.scope.VisBasePresent() || back.VisBase != tc.scope.VisBase {
				t.Errorf("visibility = %v/%v, want %v/%v",
					back.VisBasePresent(), back.VisBase, tc.scope.VisBasePresent(), tc.scope.VisBase)
			}
			if len(back.Ids) != len(tc.scope.Ids) || len(back.VisIds) != len(tc.scope.VisIds) {
				t.Errorf("ids = %d/%d, want %d/%d",
					len(back.Ids), len(back.VisIds), len(tc.scope.Ids), len(tc.scope.VisIds))
			}
		})
	}
}

// TestScopeSpecReachesAllSixBases is the defect the WebUI shipped: three of the
// six were unreachable from a graphical control, because the JS copy had no
// exclude-self path at all.
func TestScopeSpecReachesAllSixBases(t *testing.T) {
	want := map[string]string{
		"subtree|false": "subtree",
		"subtree|true":  "descendants",
		"none|false":    "none",
		"none|true":     "none-self",
		"global|false":  "global",
		"global|true":   "global-self",
	}
	seen := make(map[string]bool)
	for _, base := range []string{"subtree", "none", "global"} {
		for _, excl := range []bool{false, true} {
			key := base + "|" + map[bool]string{true: "true", false: "false"}[excl]
			got, err := ScopeSpec(base, excl, nil, "")
			if err != nil {
				t.Fatalf("%s: %v", key, err)
			}
			if got != want[key] {
				t.Errorf("%s = %q, want %q", key, got, want[key])
			}
			if seen[got] {
				t.Errorf("%s produced %q, already produced by another combination — "+
					"the six scopes must be six distinct strings", key, got)
			}
			seen[got] = true
			if _, err := ParseScope(got); err != nil {
				t.Errorf("%s produced %q, which does not parse: %v", key, got, err)
			}
		}
	}
	if len(seen) != 6 {
		t.Errorf("reached %d distinct scopes, want 6", len(seen))
	}
}

// TestScopeSpecCarriesTheVisibilityHalf is the erasure half: the re-grant
// dialog shows no control for the visibility pair, so an apply must leave it
// exactly as it found it rather than collapsing it.
func TestScopeSpecCarriesTheVisibilityHalf(t *testing.T) {
	const carry = "global/subtree+vis-ids:" + idB
	got, err := ScopeSpec("none", true, []string{idA}, carry)
	if err != nil {
		t.Fatalf("ScopeSpec: %v", err)
	}
	sc, err := ParseScope(got)
	if err != nil {
		t.Fatalf("ParseScope(%q): %v", got, err)
	}
	// The edited half is what the controls said.
	if sc.Base != protocol.ScopeBase_None || !sc.ExcludeSelf() {
		t.Errorf("edited half = %v/%v, want none/true", sc.Base, sc.ExcludeSelf())
	}
	if len(sc.Ids) != 1 {
		t.Errorf("ids = %d, want 1", len(sc.Ids))
	}
	// The carried half is untouched.
	if !sc.VisBasePresent() || sc.VisBase != protocol.ScopeBase_Global {
		t.Errorf("visibility rank = %v/%v, want present/global — a dialog with no "+
			"control for it must not clear it", sc.VisBasePresent(), sc.VisBase)
	}
	if len(sc.VisIds) != 1 {
		t.Errorf("vis-ids = %d, want the carried one", len(sc.VisIds))
	}

	// And an empty carry must NOT invent one — a fresh spawn has no visibility
	// half, and defaulting to the action rank is the wire's own zero meaning.
	fresh, err := ScopeSpec("subtree", false, nil, "")
	if err != nil {
		t.Fatalf("ScopeSpec: %v", err)
	}
	if fs, _ := ParseScope(fresh); fs.VisBasePresent() {
		t.Errorf("a spawn with no carry got a visibility rank: %q", fresh)
	}
}

// TestScopeSpecRejectsBadInputRatherThanGuessing — the browser panics the
// bridge call on error, which is the intended shape: a silently-wrong scope is
// the failure this replaced.
func TestScopeSpecRejectsBadInput(t *testing.T) {
	if _, err := ScopeSpec("subtre", false, nil, ""); err == nil {
		t.Error("a misspelled base was accepted")
	}
	if _, err := ScopeSpec("subtree", false, []string{"nothex"}, ""); err == nil {
		t.Error("a non-hex id was accepted")
	}
	if _, err := ScopeSpec("subtree", false, nil, "not a scope"); err == nil {
		t.Error("an unparseable carry was accepted")
	}
	// A base word that already carries the flag agrees with the checkbox
	// rather than fighting it: either source setting it is enough.
	for _, excl := range []bool{false, true} {
		got, err := ScopeSpec("descendants", excl, nil, "")
		if err != nil {
			t.Fatalf("descendants/%v: %v", excl, err)
		}
		if got != "descendants" {
			t.Errorf("descendants with excludeSelf=%v = %q, want descendants", excl, got)
		}
	}
}

func mustTaskID(t *testing.T, h string) protocol.TaskID {
	t.Helper()
	sc, err := ParseScope("ids:" + h)
	if err != nil {
		t.Fatalf("bad test id %q: %v", h, err)
	}
	if len(sc.Ids) != 1 {
		t.Fatalf("test id %q produced %d ids", h, len(sc.Ids))
	}
	return sc.Ids[0]
}

func init() {
	// Guard against a copy-paste that makes the two fixture ids equal, which
	// would make every "the carried id survived" assertion vacuous.
	if idA == idB || len(idA) != 32 || strings.TrimLeft(idA, "a") != "" {
		panic("scope_spec_test fixtures are malformed")
	}
}
