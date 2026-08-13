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

func TestZeroScopeIsTheSubtreeDefault(t *testing.T) {
	var zero protocol.TaskScope
	if !IsDefaultScope(zero) {
		t.Fatal("the zero TaskScope must be the subtree default")
	}
	if got := ScopeLabel(zero); got != "subtree" {
		t.Fatalf("ScopeLabel(zero) = %q, want subtree", got)
	}
	if IsDefaultScope(protocol.TaskScope{Base: protocol.ScopeBase_None}) {
		t.Fatal("none must not read as the default")
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
	opts := SessionOpts{Scope: sc}
	if got := buildSubmitRequest("/r", "p", opts).Scope; got.Base != sc.Base || got.IdsLen != 1 {
		t.Errorf("submit request scope = %+v, want %+v", got, sc)
	}
	if got := buildOpenInteractiveRequest("/r", opts).Scope; got.Base != sc.Base || got.IdsLen != 1 {
		t.Errorf("open-interactive request scope = %+v, want %+v", got, sc)
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
