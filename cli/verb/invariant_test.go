package verb

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The invariants below are asserted on the TABLE, not inferred from the shape
// of code. That distinction is the point.
//
// cli/flagorder_test.go used to infer the same class of property from source:
// it looked for verbs that define flags, read positionals, and parse with
// stdlib flag. That inference had two blind spots a declaration cannot have --
//
//   - a positional read behind a helper call. `agent send` read its payload
//     inside resolvePayload(fs, ...), so the scan saw a verb with no
//     positionals, and it was therefore neither reported nor required to be on
//     the flags-must-precede allowlist. It takes free-form trailing text all
//     the same.
//   - a FlagSet whose name is built rather than literal.
//     `flag.NewFlagSet("session stream "+verb, ...)` was skipped entirely.
//
// Both were found by counting the surface; neither is reachable from here,
// because Trailing is stated by the verb rather than deduced from how it
// happens to parse.

// TestTrailingCarriesItsReason: a verb that cannot permute must record WHY,
// or the next reader deletes the restriction and silently re-enables dropping
// a flag written after free-form text.
func TestTrailingCarriesItsReason(t *testing.T) {
	for _, v := range Verbs {
		if v.Trailing == nil {
			continue
		}
		if strings.TrimSpace(v.Trailing.Reason) == "" {
			t.Errorf("%s: Trailing needs a Reason", v.FlagSetName())
		}
		if strings.TrimSpace(v.Trailing.Name) == "" {
			t.Errorf("%s: Trailing needs a Name for the usage line", v.FlagSetName())
		}
	}
}

// TestWidensIfUnsetVerbsPermute keeps `board purge --seq N` impossible to
// reintroduce: a flag whose ABSENCE widens the operation must never sit on a
// verb where a flag can be silently dropped.
func TestWidensIfUnsetVerbsPermute(t *testing.T) {
	for _, v := range Verbs {
		for _, f := range v.Flags {
			if !f.WidensIfUnset {
				continue
			}
			if v.Trailing != nil {
				t.Errorf("%s: --%s widens when unset, but the verb takes trailing text "+
					"so it cannot permute -- a flag written after the positional would be "+
					"swallowed, which is how `board purge <topic> --seq N` destroyed two "+
					"live messages", v.FlagSetName(), f.Name)
			}
		}
	}
}

// TestSurfaceNarrowingHasAReason: a flag or positional declared for fewer
// surfaces than its verb is a real thing -- `ls --filtered` means nothing
// without a filter pane, and `file push` takes one fewer positional in a
// browser, which has no local path -- but it is also exactly the shape that
// becomes silent drift when it is only a habit.
func TestSurfaceNarrowingHasAReason(t *testing.T) {
	for _, v := range Verbs {
		for _, f := range v.Flags {
			if f.Surfaces != 0 && f.Surfaces != v.Surfaces && strings.TrimSpace(f.SurfaceReason) == "" {
				t.Errorf("%s: --%s is declared for a narrower surface set than its verb "+
					"and gives no SurfaceReason", v.FlagSetName(), f.Name)
			}
		}
		for _, a := range v.Args {
			if a.Surfaces != 0 && a.Surfaces != v.Surfaces && strings.TrimSpace(a.SurfaceReason) == "" {
				t.Errorf("%s: positional <%s> is declared for a narrower surface set than "+
					"its verb and gives no SurfaceReason", v.FlagSetName(), a.Name)
			}
		}
	}
}

// TestNoDuplicateSpellings catches a spelling registered twice within one
// verb, which flag.FlagSet panics on at first use -- at runtime, in whichever
// surface reached it first.
func TestNoDuplicateSpellings(t *testing.T) {
	for _, v := range Verbs {
		seen := map[string]string{}
		for _, f := range v.Flags {
			for _, n := range append([]string{f.Name}, f.Aliases...) {
				if prev, dup := seen[n]; dup {
					t.Errorf("%s: %q is declared by both --%s and --%s", v.FlagSetName(), n, prev, f.Name)
				}
				seen[n] = f.Name
			}
		}
	}
}

// TestVariadicArgIsLast keeps arity decidable: a variadic positional followed
// by a fixed one cannot be split.
func TestVariadicArgIsLast(t *testing.T) {
	for _, v := range Verbs {
		for i, a := range v.Args {
			if a.Variadic && i != len(v.Args)-1 {
				t.Errorf("%s: variadic <%s> is not the last positional", v.FlagSetName(), a.Name)
			}
		}
	}
}

// TestEveryVerbReachesSomeSurface: a spec with no Surfaces is unreachable
// from anywhere, which is a declaration nobody can invoke.
func TestEveryVerbReachesSomeSurface(t *testing.T) {
	for _, v := range Verbs {
		if v.Surfaces == 0 {
			t.Errorf("%s: declared for no surface", v.FlagSetName())
		}
		if v.Build == nil {
			t.Errorf("%s: has no Build", v.FlagSetName())
		}
	}
}

// TestUsageNamesItsVerb is the last piece of the board purge repair: whatever
// Usage() prints must belong to the verb that parses it. TestExamplesParse
// covers the other direction -- that what it documents actually parses.
func TestUsageNamesItsVerb(t *testing.T) {
	for _, v := range Verbs {
		u := v.Usage()
		if !strings.HasPrefix(u, "usage: "+v.FlagSetName()) {
			t.Errorf("%s: Usage() = %q, does not name the verb", v.FlagSetName(), u)
		}
		if len(v.Args) == 0 && v.Trailing == nil {
			fs := v.NewFlagSet(flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			if _, err := v.Parse(fs, nil); err != nil {
				t.Errorf("%s: takes no positionals but the bare form fails: %v", v.FlagSetName(), err)
			}
		}
	}
}

// TestNoHandWrittenVerbFlagSetsRemain replaces cli/flagorder_test.go, which
// guarded the same defect class by walking Go source for verbs that define
// flags, read positionals, and parse with stdlib flag -- the shape that
// silently drops a flag written after a positional.
//
// That guard is gone because what it searched for is gone: every verb's
// FlagSet is now built by VerbSpec.NewFlagSet and parsed by VerbSpec.Parse,
// which permutes unless Trailing says it cannot. This test keeps the property
// from coming back by hand.
//
// It also closes the two blind spots the AST scan had, both found by counting
// the surface rather than by review: a positional read behind a helper call
// (agent send reads its payload inside resolvePayload, so the scan saw a verb
// with no positionals) and a FlagSet whose name is built rather than literal
// (`flag.NewFlagSet("session stream "+verb)` was skipped entirely). A
// declaration cannot have either, because Trailing is stated by the verb
// instead of deduced from how it happens to parse.
func TestNoHandWrittenVerbFlagSetsRemain(t *testing.T) {
	// Directories that hold verb parsing. cli/verb itself is excluded: it is
	// where the one remaining NewFlagSet call belongs.
	dirs := []string{
		filepath.Join("..", "..", "cmd", "harness-cli"),
		filepath.Join("..", "..", "cli"),
		filepath.Join("..", "..", "cli", "agent"),
		filepath.Join("..", "..", "tui"),
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // a layout change should not fail this as a false alarm
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(dir, e.Name())
			src, rerr := os.ReadFile(path)
			if rerr != nil {
				t.Fatalf("read %s: %v", path, rerr)
			}
			if strings.Contains(string(src), "flag.NewFlagSet(") {
				t.Errorf("%s builds a FlagSet by hand.\n"+
					"Verb flags are declared in cli/verb and the FlagSet comes from "+
					"VerbSpec.NewFlagSet, which is what makes the permuted parse, the "+
					"alias grouping and the arity check apply everywhere instead of "+
					"per surface. Add a VerbSpec entry rather than a FlagSet here.", path)
			}
		}
	}
}
