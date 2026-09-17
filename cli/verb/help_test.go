package verb

import (
	"errors"
	"flag"
	"io"
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
