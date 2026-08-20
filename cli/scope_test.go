package cli

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestParseScope(t *testing.T) {
	a := strings.Repeat("ab", 16)
	b := strings.Repeat("cd", 16)
	for _, c := range []struct {
		in      string
		base    protocol.ScopeBase
		ids     int
		wantErr string
	}{
		{in: "", base: protocol.ScopeBase_Subtree},
		{in: "subtree", base: protocol.ScopeBase_Subtree},
		{in: " subtree ", base: protocol.ScopeBase_Subtree},
		{in: "none", base: protocol.ScopeBase_None},
		{in: "global", base: protocol.ScopeBase_Global},
		{in: "ids:" + a, base: protocol.ScopeBase_None, ids: 1},
		{in: "none+ids:" + a, base: protocol.ScopeBase_None, ids: 1},
		{in: "subtree+ids:" + a + "," + b, base: protocol.ScopeBase_Subtree, ids: 2},
		// Duplicates collapse rather than being sent twice.
		{in: "ids:" + a + "," + a, base: protocol.ScopeBase_None, ids: 1},
		// Rejected, not silently ignored.
		{in: "global+ids:" + a, wantErr: "meaningless under global"},
		{in: "ids:", wantErr: "with no ids"},
		{in: "ids:zz", wantErr: "hex characters"},
		{in: "ids:" + strings.Repeat("a", 31), wantErr: "hex characters"},
		{in: "bogus", wantErr: "unknown scope base"},
		{in: "bogus+ids:" + a, wantErr: "unknown scope base"},
	} {
		got, err := ParseScope(c.in)
		if c.wantErr != "" {
			if err == nil {
				t.Errorf("ParseScope(%q) = %+v, want error containing %q", c.in, got, c.wantErr)
			} else if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("ParseScope(%q) error = %v, want it to mention %q", c.in, err, c.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseScope(%q): %v", c.in, err)
			continue
		}
		if got.Base != c.base || len(got.Ids) != c.ids || int(got.IdsLen) != c.ids {
			t.Errorf("ParseScope(%q) = base %v ids %d (len %d), want base %v ids %d",
				c.in, got.Base, len(got.Ids), got.IdsLen, c.base, c.ids)
		}
	}
}

// What ls prints must be pasteable back into --scope.
func TestScopeLabelRoundTrips(t *testing.T) {
	a := strings.Repeat("ab", 16)
	b := strings.Repeat("cd", 16)
	for _, canonical := range []string{
		"subtree", "none", "global",
		"ids:" + a,
		"ids:" + a + "," + b,
		"subtree+ids:" + a,
	} {
		parsed, err := ParseScope(canonical)
		if err != nil {
			t.Fatalf("ParseScope(%q): %v", canonical, err)
		}
		if got := ScopeLabel(parsed); got != canonical {
			t.Errorf("ScopeLabel(ParseScope(%q)) = %q", canonical, got)
		}
		if _, err := ParseScope(ScopeLabel(parsed)); err != nil {
			t.Errorf("ScopeLabel output %q does not parse back: %v", ScopeLabel(parsed), err)
		}
	}
}

// Three separate things read a zero TaskScope and all three must mean subtree:
// a client that sent no scope, a WAL record written before scopes existed (its
// JSON fields are omitempty), and a zero-valued Go struct. That is why subtree
// is ordinal 0 rather than none — see the ScopeBase comment in message.bgn.
func TestZeroScopeIsTheSubtreeDefault(t *testing.T) {
	var zero protocol.TaskScope
	if zero.Base != protocol.ScopeBase_Subtree || len(zero.Ids) != 0 {
		t.Fatalf("the zero TaskScope must be plain subtree, got base=%v ids=%d", zero.Base, len(zero.Ids))
	}
	if got := ScopeLabel(zero); got != "subtree" {
		t.Fatalf("ScopeLabel(zero) = %q, want subtree", got)
	}
	if got := ScopeLabel(protocol.TaskScope{Base: protocol.ScopeBase_None}); got == "subtree" {
		t.Fatal("none must not render as the default")
	}
}

// Both spawn builders must carry the scope, or the flag is accepted and
// dropped on one of the two paths. The wasm interactive path shares
// buildOpenInteractiveRequest with the native one precisely so they cannot
// drift; this pins that.
func TestBothSpawnBuildersCarryScope(t *testing.T) {
	sc, err := ParseScope("none+ids:" + strings.Repeat("ab", 16))
	if err != nil {
		t.Fatal(err)
	}
	// The override list rides the same funnel and is checked here too: it is a
	// SIBLING wire field, which is exactly the shape that gets set in one
	// builder and forgotten in the other.
	_, ov, err := ParseScopeFor("cancel=none")
	if err != nil {
		t.Fatal(err)
	}
	opts := SessionOpts{Scope: sc, Overrides: []protocol.ScopeOverride{ov}}

	sub := buildSubmitRequest("/r", "p", opts)
	if sub.Scope.Base != sc.Base || sub.Scope.IdsLen != 1 {
		t.Errorf("submit request scope = %+v, want %+v", sub.Scope, sc)
	}
	if sub.OverridesLen != 1 || len(sub.Overrides) != 1 {
		t.Errorf("submit request overrides = %d/%d, want one", sub.OverridesLen, len(sub.Overrides))
	}

	oi := buildOpenInteractiveRequest("/r", opts)
	if oi.Scope.Base != sc.Base || oi.Scope.IdsLen != 1 {
		t.Errorf("open-interactive request scope = %+v, want %+v", oi.Scope, sc)
	}
	if oi.OverridesLen != 1 || len(oi.Overrides) != 1 {
		t.Errorf("open-interactive request overrides = %d/%d, want one", oi.OverridesLen, len(oi.Overrides))
	}
}

// The Cancel status used to be a bare u8 that the server always wrote as 0, so
// the client returned nil unconditionally. It now carries no_such_task — which
// is also the answer for a target outside the caller's scope — and reporting
// that as success would tell an agent it had cancelled something it had not.
func TestCancelReportsNoSuchTask(t *testing.T) {
	id := strings.Repeat("ab", 16)
	for _, c := range []struct {
		status  protocol.CancelResult
		wantErr string
	}{
		{protocol.CancelResult_Ok, ""},
		{protocol.CancelResult_NoSuchTask, "no such task"},
	} {
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_Cancel}
		resp.SetCancel(protocol.CancelStatus{Status: c.status})
		err := cancelStatusErr(&resp, id)
		if c.wantErr == "" {
			if err != nil {
				t.Errorf("status %v: err = %v, want nil", c.status, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("status %v: err = %v, want it to mention %q", c.status, err, c.wantErr)
		}
	}
}

// The visibility half is a prefix. Omitting it must leave the presence bit
// CLEAR, which is what pins the rank pair to the diagonal — a scope that
// silently set the bit would encode one authority two ways.
func TestParseScopeVisibilityPair(t *testing.T) {
	got, err := ParseScope("global/subtree")
	if err != nil {
		t.Fatalf("ParseScope: %v", err)
	}
	if got.Base != protocol.ScopeBase_Subtree || got.VisBase != protocol.ScopeBase_Global || !got.VisBasePresent() {
		t.Errorf("= base %v vis %v present %v, want subtree / global / true",
			got.Base, got.VisBase, got.VisBasePresent())
	}
	plain, err := ParseScope("subtree")
	if err != nil {
		t.Fatal(err)
	}
	if plain.VisBasePresent() || plain.VisBase != protocol.ScopeBase(0) {
		t.Error("an omitted visibility half must leave the field canonical-zero")
	}
}

// descendants is a UI word for base=subtree with exclude_self; the wire has no
// such base value.
func TestParseScopeDescendantsIsAFlagNotABase(t *testing.T) {
	got, err := ParseScope("descendants")
	if err != nil {
		t.Fatalf("ParseScope: %v", err)
	}
	if got.Base != protocol.ScopeBase_Subtree || !got.ExcludeSelf() {
		t.Errorf("= base %v exclude_self %v, want subtree / true", got.Base, got.ExcludeSelf())
	}
	empty, err := ParseScope("none-self")
	if err != nil {
		t.Fatalf("ParseScope(none-self): %v", err)
	}
	if empty.Base != protocol.ScopeBase_None || !empty.ExcludeSelf() {
		t.Errorf("none-self = base %v exclude %v", empty.Base, empty.ExcludeSelf())
	}
}

// Self is always visible, so the visibility half must refuse to exclude it
// rather than accepting a word whose meaning it would then drop.
func TestParseScopeRejectsExcludeSelfOnVisibility(t *testing.T) {
	if _, err := ParseScope("descendants/subtree"); err == nil {
		t.Error("accepted an exclude_self spelling on the visibility half")
	}
}

// vis-ids are view-only extras, written after the action half.
func TestParseScopeVisIDs(t *testing.T) {
	a := strings.Repeat("ab", 16)
	got, err := ParseScope("subtree+vis-ids:" + a)
	if err != nil {
		t.Fatalf("ParseScope: %v", err)
	}
	if got.Base != protocol.ScopeBase_Subtree || got.VisIdsLen != 1 || len(got.Ids) != 0 {
		t.Errorf("= base %v vis_ids %d ids %d, want subtree / 1 / 0",
			got.Base, got.VisIdsLen, len(got.Ids))
	}
}

// Every new spelling must survive ls -> paste -> --scope, or the grammar has
// two ways to say one thing and the round trip is a lie.
func TestScopeLabelRoundTripsTheNewForms(t *testing.T) {
	a := strings.Repeat("ab", 16)
	b := strings.Repeat("cd", 16)
	for _, canonical := range []string{
		"descendants",
		"none-self",
		"global/subtree",
		"none/none",
		"subtree+vis-ids:" + a,
		"global/subtree+ids:" + a + "+vis-ids:" + b,
		"global/descendants",
	} {
		parsed, err := ParseScope(canonical)
		if err != nil {
			t.Fatalf("ParseScope(%q): %v", canonical, err)
		}
		if got := ScopeLabel(parsed); got != canonical {
			t.Errorf("ScopeLabel(ParseScope(%q)) = %q", canonical, got)
		}
		if _, err := ParseScope(ScopeLabel(parsed)); err != nil {
			t.Errorf("ScopeLabel output %q does not parse back: %v", ScopeLabel(parsed), err)
		}
	}
}

// --scope-for takes a capability LIST, mirroring --caps: a grouped narrowing
// must cost one flag, not one per bit.
func TestParseScopeForTakesACapabilityList(t *testing.T) {
	caps, ov, err := ParseScopeFor("exec_cowrite,file_write=descendants")
	if err != nil {
		t.Fatalf("ParseScopeFor: %v", err)
	}
	want := protocol.Capability_ExecCowrite | protocol.Capability_FileWrite
	if caps != want || ov.Caps != want {
		t.Errorf("caps = %v, want both bits", caps)
	}
	if ov.Base != protocol.ScopeBase_Subtree || !ov.ExcludeSelf() {
		t.Errorf("override = base %v exclude %v, want subtree / true", ov.Base, ov.ExcludeSelf())
	}
}

// Visibility is a property of the task, not of one verb, so an override that
// tries to carry it is refused rather than silently dropped.
func TestParseScopeForRejectsVisibilityAndEmptyMasks(t *testing.T) {
	for _, in := range []string{
		"cancel=global/none",
		"cancel=subtree+vis-ids:" + strings.Repeat("ab", 16),
		"none=subtree",
		"cancel",
	} {
		if _, _, err := ParseScopeFor(in); err == nil {
			t.Errorf("ParseScopeFor(%q) = nil error, want a rejection", in)
		}
	}
}

// Overlapping masks are what disjointness buys: caught client-side so a typo
// costs a parse error rather than a round trip.
func TestMergeScopeOverrideRejectsOverlap(t *testing.T) {
	_, a, err := ParseScopeFor("cancel,purge=none")
	if err != nil {
		t.Fatal(err)
	}
	_, b, err := ParseScopeFor("cancel=subtree")
	if err != nil {
		t.Fatal(err)
	}
	list, err := MergeScopeOverride(nil, a)
	if err != nil {
		t.Fatalf("first merge: %v", err)
	}
	if _, err := MergeScopeOverride(list, b); err == nil {
		t.Error("accepted an override overlapping one already present")
	}
}

// What ls prints for the overrides must paste back as --scope-for values.
func TestOverridesLabelRoundTrips(t *testing.T) {
	_, ov, err := ParseScopeFor("exec_cowrite,file_write=descendants")
	if err != nil {
		t.Fatal(err)
	}
	got := OverridesLabel([]protocol.ScopeOverride{ov})
	want := "exec_cowrite,file_write:descendants"
	if got != want {
		t.Fatalf("OverridesLabel = %q, want %q", got, want)
	}
	if _, _, err := ParseScopeFor(strings.Replace(got, ":", "=", 1)); err != nil {
		t.Errorf("rendered override does not parse back: %v", err)
	}
}
