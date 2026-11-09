package verb

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestSpawnLimitFlagsOnEverySpawnVerb(t *testing.T) {
	for _, path := range [][]string{{"submit"}, {"session", "new"}} {
		for _, sf := range []Surface{CLI, TUI, WebUI} {
			args := []string{"--repo", "/r", "--spawn-depth", "2", "--max-descendants", "unbounded"}
			if path[0] == "submit" {
				args = append(args, "x")
			}
			act, err := buildOn(t, sf, path, args...)
			if err != nil {
				t.Fatalf("%v on %v: %v", path, sf, err)
			}
			a := act.(SpawnAction)
			if a.SpawnDepth == nil || !a.SpawnDepth.Limited() || a.SpawnDepth.Value() != 2 ||
				a.MaxDescendants == nil || a.MaxDescendants.Limited() ||
				!a.SpawnDepthPresent || !a.MaxDescendantsPresent {
				t.Errorf("%v on %v: %+v", path, sf, a)
			}
		}
	}
}

func TestSpawnLimitFlagGrammar(t *testing.T) {
	for _, bad := range []string{"", "-1", "16777216", "abc", "2x", "Unbounded"} {
		if _, err := parseSpawnLimitFlag(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for in, want := range map[string]protocol.SpawnLimit{
		"0": limitOf(0), "16777215": limitOf(16777215), "unbounded": {},
	} {
		got, err := parseSpawnLimitFlag(in)
		if err != nil || got.Limited() != want.Limited() || got.Value() != want.Value() {
			t.Errorf("%q: got %+v, %v", in, got, err)
		}
	}
}

func limitOf(n uint32) protocol.SpawnLimit {
	var l protocol.SpawnLimit
	l.SetLimited(true)
	l.SetValue(n)
	return l
}

func TestSpawnLimitFlagsConflictWithExact(t *testing.T) {
	for _, flagName := range []string{"--spawn-depth", "--max-descendants"} {
		_, err := buildOn(t, CLI, []string{"submit"}, "--resume", idA, "--exact", flagName, "1")
		if err == nil || !strings.Contains(err.Error(), "exact") || !strings.Contains(err.Error(), strings.TrimPrefix(flagName, "--")) {
			t.Errorf("--exact %s: err = %v, want a conflict naming both", flagName, err)
		}
	}
}

func TestCapsSetAcceptsLimitAlone(t *testing.T) {
	act, err := buildOn(t, CLI, []string{"caps", "set"}, idA, "--max-descendants", "3")
	if err != nil {
		t.Fatalf("caps set --max-descendants alone: %v", err)
	}
	a := act.(SetCapsAction)
	if a.MaxDescendants == nil || a.MaxDescendants.Value() != 3 || a.SpawnDepth != nil {
		t.Errorf("caps set --max-descendants 3: %+v", a)
	}
}

func TestSetDefaultsSpawnLimitFlags(t *testing.T) {
	act, err := buildOn(t, TUI, []string{"caps", "set-defaults"}, "--max-descendants", "3")
	if err != nil {
		t.Fatalf("caps set-defaults: %v", err)
	}
	if a := act.(SetDefaultsAction); a.MaxDescendants == nil || a.MaxDescendants.Value() != 3 {
		t.Errorf("caps set-defaults --max-descendants 3: %+v", a)
	}
	act, err = buildOn(t, WebUI, []string{"scope"}, "--spawn-depth", "1")
	if err != nil {
		t.Fatalf("scope: %v", err)
	}
	if a := act.(SetDefaultsAction); a.SpawnDepth == nil || a.SpawnDepth.Value() != 1 {
		t.Errorf("scope --spawn-depth 1: %+v", a)
	}
}
