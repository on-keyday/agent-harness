package verb

import (
	"errors"
	"flag"
	"io"
	"reflect"
	"strings"
	"testing"
)

// Every declared flag must say something, because now the saying is PRINTED.
//
// It was true of all 233 flags on the day HelpBlock started rendering them —
// the descriptions had been written and maintained all along, they just reached
// no operator. This holds that: an empty Help is a blank line in a column an
// operator reads, which is worse than the flag not being listed at all.
//
// The companion for Notes is TestEveryReachableVerbHasANote. This is the same
// question one level down.
func TestEveryFlagHasHelp(t *testing.T) {
	for _, v := range Verbs {
		for _, f := range v.Flags {
			if strings.TrimSpace(f.Help) == "" {
				t.Errorf("%s: --%s has no Help. It is printed by `%s -h` now, so "+
					"a flag with none renders a blank description.",
					v.FlagSetName(), f.Name, v.FlagSetName())
			}
		}
	}
}

// Every flag a surface accepts must appear in the block that surface prints.
//
// Checked per surface rather than on the raw table because For() drops the
// flags another surface declares, and a block listing a flag this surface
// refuses documents a call that cannot be made — the `board purge --seq`
// failure shape, one layer over.
func TestHelpBlockNamesEveryFlagOfItsSurface(t *testing.T) {
	for _, s := range []struct {
		name string
		surf Surface
	}{{"CLI", CLI}, {"TUI", TUI}, {"WebUI", WebUI}} {
		for _, path := range PathsForSurface(s.surf) {
			sp, ok := Lookup(strings.Fields(path)...)
			if !ok {
				continue
			}
			sp = sp.For(s.surf)
			block := strings.Join(sp.HelpBlock(0), "\n")
			for _, f := range sp.Flags {
				if !strings.Contains(block, dashList([]string{f.Name})) {
					t.Errorf("%s %s: --%s is accepted but not listed in HelpBlock",
						s.name, path, f.Name)
				}
				if h := strings.Join(strings.Fields(f.Help), " "); h != "" && !strings.Contains(block, h) {
					t.Errorf("%s %s: --%s's Help does not reach the block",
						s.name, path, f.Name)
				}
			}
		}
	}
}

// A wrapped description stays in its column. The point of taking a width at all
// is that a 381-character --route folding back to the left margin is harder to
// read than no wrap: the continuation must hang under the description, not
// under the flag name.
func TestHelpBlockWrapsIntoTheDescriptionColumn(t *testing.T) {
	sp, ok := Lookup("file", "push")
	if !ok {
		t.Fatal("file push must be declared")
	}
	const width = 100
	lines := sp.For(CLI).HelpBlock(width)
	var (
		inFlags bool
		wrapped int
	)
	// The synopsis is deliberately exempt: it is a grammar, and folding it
	// mid-bracket describes a shape the parser does not have. Everything
	// UNDER it is prose and wraps.
	for _, l := range lines[1:] {
		if l == "flags:" {
			inFlags = true
			continue
		}
		if len(l) > width {
			t.Errorf("line exceeds width %d:\n%q", width, l)
		}
		if !inFlags || !strings.HasPrefix(l, "    ") || strings.TrimSpace(l) == "" {
			continue
		}
		// A continuation: indented past the two columns a flag line starts at.
		wrapped++
	}
	if wrapped == 0 {
		t.Errorf("--route's description is longer than %d columns; expected it to "+
			"wrap into the column. Got:\n%s", width, strings.Join(lines, "\n"))
	}
}

// Width 0 is "do not wrap", which is what the browser and a pipe pass. The
// terminal folds it; a guess made here would fight whatever does.
func TestHelpBlockWidthZeroDoesNotWrap(t *testing.T) {
	sp, _ := Lookup("file", "push")
	for _, l := range sp.For(CLI).HelpBlock(0) {
		if strings.HasPrefix(strings.TrimSpace(l), "and reads them") {
			t.Fatalf("width 0 wrapped anyway: %q", l)
		}
	}
}

// -h is answered with the block, not with the synopsis alone. This is the
// symptom the change exists to remove: `session send --help` printed four
// lines and none of them described a flag.
func TestHelpRequestedCarriesTheFlagDescriptions(t *testing.T) {
	sp, ok := Lookup("session", "send")
	if !ok {
		t.Fatal("session send must be declared")
	}
	sp = sp.For(CLI)
	fs := sp.NewFlagSet(flag.ContinueOnError)
	fs.SetOutput(io.Discard) // as every surface does; the answer is ours, not flag's
	_, err := sp.Parse(fs, []string{"--help"})
	var help *HelpRequested
	if !errors.As(err, &help) {
		t.Fatalf("--help must return *HelpRequested, got %T (%v)", err, err)
	}
	got := strings.Join(help.Lines(0), "\n")
	for _, want := range []string{"flags:", "--enter", "--detect-agent"} {
		if !strings.Contains(got, want) {
			t.Errorf("help does not mention %q:\n%s", want, got)
		}
	}
}

// Every declared default that is NOT the zero value must reach the block.
//
// The guard exists because the first version of defaultNote was a type switch
// over the five declared FlagTypes, and a switch answers "" for a type it does
// not know — the same answer as "this is the zero value". A sixth FlagType
// would have silently stopped printing defaults for every flag using it.
func TestEveryNonZeroDefaultIsPrinted(t *testing.T) {
	for _, v := range Verbs {
		block := strings.Join(v.HelpBlock(0), "\n")
		for _, f := range v.Flags {
			if f.Default == nil || reflect.ValueOf(f.Default).IsZero() {
				continue
			}
			note := defaultNote(f)
			if note == "" {
				t.Errorf("%s: --%s declares %#v and renders no default note",
					v.FlagSetName(), f.Name, f.Default)
				continue
			}
			if !strings.Contains(block, strings.TrimSpace(note)) {
				t.Errorf("%s: --%s's %q does not reach the block", v.FlagSetName(), f.Name, note)
			}
		}
	}
}

// A printed default on a LADDERED flag is the ladder's BOTTOM tier, not the
// answer, so every tier above it must be named beside the flag.
//
// Resolve (build.go) reaches the declared Default only after each tier answers
// empty: flag, then env, then the workspace config, then Default. `prune-local
// --repo` is the only flag in the table with both a non-zero Default and tiers
// above it, and its block read `(default ".")` while HARNESS_REPO_PATH — set in
// every runner-spawned task, so "." is the case that never happens there —
// decided which repo the verb removes worktrees from. Measured 2026-09-18 with
// `prune-local --before 100000h` against `env -u HARNESS_REPO_PATH`: two
// different directories, one help line.
//
// The obligation is on the PROSE, deliberately. Nothing here asks whether the
// note was printed, so the check cannot be met by changing the renderer.
// Suppressing the note for any laddered flag was the first fix and was wrong:
// the thirteen others declare a ZERO default, so it would have been a rule with
// one live subject that silently stops printing the day one of them is given a
// real default — the failure the reflect-over-type-switch choice above exists to
// avoid. HelpBlock carrying Help verbatim is held by
// TestHelpBlockNamesEveryFlagOfItsSurface, so the declaration is what to read.
func TestLadderTiersAreNamedWhenADefaultIsPrinted(t *testing.T) {
	for _, v := range Verbs {
		for _, f := range v.Flags {
			if len(f.Resolve) == 0 || defaultNote(f) == "" {
				continue
			}
			for _, tr := range f.Resolve {
				var tier, want string
				switch {
				case tr.Env != "":
					tier, want = "env "+tr.Env, tr.Env
				case tr.Workspace != "":
					// The SOURCE, not the key: the key here is "repo", a word
					// every second line of this table already contains.
					tier, want = "workspace key "+tr.Workspace, "workspace"
				default:
					// SurfaceContext is a dropdown or a session field, not a
					// name an operator can look up and set.
					continue
				}
				if !strings.Contains(f.Help, want) {
					t.Errorf("%s: --%s prints %q, but its %s tier preempts that default and "+
						"is not named in the flag's Help: %q",
						v.FlagSetName(), f.Name, strings.TrimSpace(defaultNote(f)), tier, f.Help)
				}
			}
		}
	}
}

// A ZERO default is a sentinel in this table and must not be printed.
// `session send --settle-ms` declares 0 and waits 1500ms once --snapshot
// follows; `git log --max` declares 0 and fetches 100. "(default 0)" would
// state a fact neither flag has, and what zero means is in the Help already.
func TestZeroDefaultIsNotPrinted(t *testing.T) {
	for _, tc := range []struct{ path []string }{
		{[]string{"session", "send"}},
		{[]string{"git", "log"}},
	} {
		sp, ok := Lookup(tc.path...)
		if !ok {
			t.Fatalf("%v must be declared", tc.path)
		}
		for _, f := range sp.Flags {
			if f.Default == nil || !reflect.ValueOf(f.Default).IsZero() {
				continue
			}
			if note := defaultNote(f); note != "" {
				t.Errorf("%s: --%s declares the zero value and printed %q",
					sp.FlagSetName(), f.Name, note)
			}
		}
	}
}
