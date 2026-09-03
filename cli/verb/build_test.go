package verb

import (
	"flag"
	"io"
	"testing"
	"time"
)

// testSpec is a synthetic verb: one bool with an alias, one duration, variadic
// positionals. Shaped like prune without being prune, so these tests keep
// meaning the same thing when the real declaration changes.
func testSpec() VerbSpec {
	return VerbSpec{
		Path:     []string{"probe"},
		Surfaces: CLI | TUI | WebUI,
		Args:     []Arg{{Name: "id", Type: ArgString, Variadic: true}},
		Flags: []Flag{
			{Name: "force", Aliases: []string{"f"}, Type: FlagBool, Default: false, Help: "force it"},
			{Name: "before", Type: FlagDuration, Default: 7 * 24 * time.Hour, Help: "cutoff"},
		},
	}
}

func parseProbe(t *testing.T, args ...string) Bound {
	t.Helper()
	sp := testSpec()
	fs := sp.NewFlagSet(flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	b, err := sp.Parse(fs, args)
	if err != nil {
		t.Fatalf("Parse(%q): %v", args, err)
	}
	return b
}

func TestParseCollectsFlagsAndPositionals(t *testing.T) {
	b := parseProbe(t, "--before", "1h", "aaa", "bbb")
	if got := b.Flags["before"]; got != time.Hour {
		t.Errorf("before = %v, want 1h", got)
	}
	if b.Bool("force") {
		t.Error("force should default false")
	}
	if len(b.Args) != 2 || b.Args[0] != "aaa" || b.Args[1] != "bbb" {
		t.Errorf("Args = %q, want [aaa bbb]", b.Args)
	}
}

// An alias must land on the canonical name. A consumer reading b.Flags["f"]
// would be reading a name the declaration does not own.
func TestAliasResolvesToCanonicalName(t *testing.T) {
	b := parseProbe(t, "-f", "aaa")
	if !b.Bool("force") {
		t.Error("-f did not set force")
	}
	if _, present := b.Flags["f"]; present {
		t.Error("alias f leaked into Flags; only canonical names belong there")
	}
}

// The whole point of the permute rule. Without it, `probe aaa --force` leaves
// force false -- the board purge failure, which destroyed two live messages.
func TestFlagAfterPositionalIsRead(t *testing.T) {
	b := parseProbe(t, "aaa", "--force")
	if !b.Bool("force") {
		t.Error("--force written after the positional was dropped")
	}
	if len(b.Args) != 1 || b.Args[0] != "aaa" {
		t.Errorf("Args = %q, want [aaa]", b.Args)
	}
}

// Set distinguishes "gave the default explicitly" from "said nothing", which
// is what resume-time re-granting turns on elsewhere in this tree.
func TestSetRecordsOnlySuppliedFlags(t *testing.T) {
	b := parseProbe(t, "--force=false", "aaa")
	if !b.Set["force"] {
		t.Error("explicitly-passed force should be in Set")
	}
	if b.Set["before"] {
		t.Error("untouched before should not be in Set")
	}
}

// An alias is a spelling of one flag, so supplying it must mark the canonical
// name as set -- otherwise `-f` and `--force` disagree about whether the
// operator said anything.
func TestSetRecordsAliasUnderCanonicalName(t *testing.T) {
	b := parseProbe(t, "-f")
	if !b.Set["force"] {
		t.Error("-f did not mark force as set")
	}
	if b.Set["f"] {
		t.Error("Set should key on canonical names only")
	}
}

// Arity replaces the per-verb len(pargs) != N checks each surface wrote by
// hand. A fixed positional list rejects both too few and too many.
func TestFixedArityIsEnforced(t *testing.T) {
	sp := VerbSpec{
		Path: []string{"pair"},
		Args: []Arg{{Name: "a", Type: ArgString}, {Name: "b", Type: ArgString}},
	}
	for _, args := range [][]string{{"one"}, {"one", "two", "three"}} {
		fs := sp.NewFlagSet(flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		if _, err := sp.Parse(fs, args); err == nil {
			t.Errorf("Parse(%q) succeeded; want an arity error", args)
		}
	}
	fs := sp.NewFlagSet(flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if _, err := sp.Parse(fs, []string{"one", "two"}); err != nil {
		t.Errorf("Parse of the exact arity failed: %v", err)
	}
}

// A Trailing verb keeps its free-form words joined and out of Args, and does
// NOT permute: a '-'-leading word there is text, not a flag.
func TestTrailingCollectsTheRest(t *testing.T) {
	sp := VerbSpec{
		Path:     []string{"say"},
		Args:     []Arg{{Name: "id", Type: ArgString}},
		Trailing: &Trailing{Name: "text", Reason: "the literal words to send"},
		Flags:    []Flag{{Name: "quiet", Type: FlagBool, Default: false}},
	}
	fs := sp.NewFlagSet(flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	b, err := sp.Parse(fs, []string{"--quiet", "abc", "hello", "world"})
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !b.Bool("quiet") {
		t.Error("--quiet before the positional was dropped")
	}
	if len(b.Args) != 1 || b.Args[0] != "abc" {
		t.Errorf("Args = %q, want [abc]", b.Args)
	}
	if b.Trail != "hello world" {
		t.Errorf("Trail = %q, want %q", b.Trail, "hello world")
	}
}
