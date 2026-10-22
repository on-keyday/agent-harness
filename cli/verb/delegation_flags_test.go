package verb

import (
	"flag"
	"io"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// buildOn parses args for the verb at path as surface sf would, and builds it.
func buildOn(t *testing.T, sf Surface, path []string, args ...string) (Action, error) {
	t.Helper()
	sp, ok := Lookup(path...)
	if !ok {
		t.Fatalf("%v is not in the table", path)
	}
	v := sp.For(sf)
	fs := v.NewFlagSet(flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	b, err := v.Parse(fs, args)
	if err != nil {
		return nil, err
	}
	return v.BuildFunc()(b)
}

func TestDelegateFlagsOnEverySpawnVerb(t *testing.T) {
	want := protocol.TaskScope{Base: protocol.ScopeBase_Subtree}
	want.SetExcludeSelf(true)
	for _, path := range [][]string{{"submit"}, {"session", "new"}} {
		for _, sf := range []Surface{CLI, TUI, WebUI} {
			args := []string{"--repo", "/r", "--delegate-caps", "file_read", "--delegate-scope", "descendants"}
			if path[0] == "submit" {
				args = append(args, "x") // the prompt; session new takes none
			}
			act, err := buildOn(t, sf, path, args...)
			if err != nil {
				t.Fatalf("%v on %v: %v", path, sf, err)
			}
			a := act.(SpawnAction)
			if a.DelegateCaps == nil || *a.DelegateCaps != protocol.Capability_FileRead ||
				a.DelegateScope == nil || a.DelegateScope.Base != want.Base || !a.DelegateScope.ExcludeSelf() ||
				!a.DelegateCapsPresent || !a.DelegateScopePresent {
				t.Errorf("%v on %v: %+v", path, sf, a)
			}
		}
	}
}

func TestDelegateFlagsConflictWithExact(t *testing.T) {
	for _, flagName := range []string{"--delegate-caps", "--delegate-scope"} {
		_, err := buildOn(t, CLI, []string{"submit"}, "--resume", idA, "--exact", flagName, "none")
		if err == nil || !strings.Contains(err.Error(), "exact") || !strings.Contains(err.Error(), strings.TrimPrefix(flagName, "--")) {
			t.Errorf("--exact %s: err = %v, want a conflict naming both", flagName, err)
		}
	}
}

func TestCapsSetAcceptsBoundAlone(t *testing.T) {
	act, err := buildOn(t, CLI, []string{"caps", "set"}, idA, "--delegate-scope", "none")
	if err != nil {
		t.Fatalf("caps set --delegate-scope alone: %v", err)
	}
	if a := act.(SetCapsAction); a.DelegateScope == nil || a.DelegateScope.Base != protocol.ScopeBase_None || a.DelegateCaps != nil {
		t.Errorf("caps set --delegate-scope none: %+v", a)
	}
	if _, err := buildOn(t, CLI, []string{"caps", "set"}, idA); err == nil {
		t.Error("caps set naming nothing was accepted")
	}
}

func TestSetDefaultsDelegateFlags(t *testing.T) {
	act, err := buildOn(t, TUI, []string{"caps", "set-defaults"}, "--delegate-caps", "file_read")
	if err != nil {
		t.Fatal(err)
	}
	if a := act.(SetDefaultsAction); a.DelegateCaps == nil || *a.DelegateCaps != protocol.Capability_FileRead {
		t.Errorf("caps set-defaults --delegate-caps: %+v", a)
	}
	act, err = buildOn(t, WebUI, []string{"scope"}, "--delegate-scope", "subtree")
	if err != nil {
		t.Fatal(err)
	}
	if a := act.(SetDefaultsAction); a.DelegateScope == nil || a.DelegateScope.Base != protocol.ScopeBase_Subtree {
		t.Errorf("scope --delegate-scope: %+v", a)
	}
}

// A bound produced in the browser goes through ScopeSpec and back through
// ParseScope; every form the bound uses must survive that.
func TestScopeSpecRoundTripsBound(t *testing.T) {
	for _, tc := range []struct {
		base        string
		excludeSelf bool
		ids         []string
		visBase     string
	}{
		{"subtree", true, nil, ""},
		{"none", false, []string{idA}, ""},
		{"subtree", false, nil, "global"},
	} {
		spec, err := ScopeSpec(tc.base, tc.excludeSelf, tc.ids, tc.visBase, nil)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ParseScope(spec)
		if err != nil {
			t.Fatalf("ParseScope(%q): %v", spec, err)
		}
		if ScopeLabel(got) != spec {
			t.Errorf("%+v → %q → %q", tc, spec, ScopeLabel(got))
		}
	}
}
