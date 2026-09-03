package verb

import (
	"flag"
	"io"
	"testing"
)

// The legacy* functions below are cmd/harness-cli/main.go's file parsers as
// they stood before the migration. They live here for the length of it: when
// the last surface stops parsing a sub-verb by hand, its legacy copy is the
// only one left and deleting it closes the family.

func legacyFilePush(args []string) (pos []string, r, f, p bool, err error) {
	fs := flag.NewFlagSet("file push", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	rec := fs.Bool("recursive", false, "")
	fs.BoolVar(rec, "r", false, "")
	force := fs.Bool("force", false, "")
	fs.BoolVar(force, "f", false, "")
	par := fs.Bool("parents", false, "")
	fs.BoolVar(par, "p", false, "")
	pos, err = ParsePermuted(fs, args)
	return pos, *rec, *force, *par, err
}

func legacyFilePull(args []string) (pos []string, r, f bool, off, ln uint64, err error) {
	fs := flag.NewFlagSet("file pull", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	rec := fs.Bool("recursive", false, "")
	fs.BoolVar(rec, "r", false, "")
	force := fs.Bool("force", false, "")
	fs.BoolVar(force, "f", false, "")
	o := fs.Uint64("offset", 0, "")
	l := fs.Uint64("length", 0, "")
	pos, err = ParsePermuted(fs, args)
	return pos, *rec, *force, *o, *l, err
}

const idA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// TestFilePushMatchesLegacy holds the declaration to the CLI's pre-migration
// parse over every documented form, the flag-after-positional one included.
func TestFilePushMatchesLegacy(t *testing.T) {
	sp, ok := Lookup("file", "push")
	if !ok {
		t.Fatal("file push is not in the table")
	}
	cli := sp.For(CLI)
	for _, args := range [][]string{
		{idA, "src", "dst"},
		{"-r", idA, "src", "dst"},
		{"--recursive", "--force", idA, "src", "dst"},
		{idA, "src", "dst", "-f"},
		{"-p", idA, "src", "dst"},
		{idA, "src", "dst", "--parents", "--force"},
	} {
		wantPos, wr, wf, wp, werr := legacyFilePush(args)
		fs := cli.NewFlagSet(flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		b, err := cli.Parse(fs, args)
		if (err != nil) != (werr != nil) {
			t.Errorf("%q: err=%v legacy=%v", args, err, werr)
			continue
		}
		if err != nil {
			continue
		}
		act, _ := cli.BuildFunc()(b)
		got := act.(FilePushAction)
		if got.Recursive != wr || got.Force != wf || got.Parents != wp {
			t.Errorf("%q: flags r/f/p = %v/%v/%v, legacy %v/%v/%v",
				args, got.Recursive, got.Force, got.Parents, wr, wf, wp)
		}
		if len(wantPos) != 3 || got.TaskID != wantPos[0] || got.LocalSrc != wantPos[1] || got.RemoteDst != wantPos[2] {
			t.Errorf("%q: positionals = %q/%q/%q, legacy %q",
				args, got.TaskID, got.LocalSrc, got.RemoteDst, wantPos)
		}
	}
}

// TestFilePullMatchesLegacy also pins the one deliberate widening in this
// family: -o and -n existed only in the TUI, and now parse everywhere. The
// legacy CLI parser rejects them, so those forms are checked against the TUI's
// behaviour instead of the CLI's.
func TestFilePullMatchesLegacy(t *testing.T) {
	sp, ok := Lookup("file", "pull")
	if !ok {
		t.Fatal("file pull is not in the table")
	}
	cli := sp.For(CLI)
	for _, args := range [][]string{
		{idA, "src", "dst"},
		{"-r", idA, "src", "dst"},
		{idA, "src", "dst", "--offset", "10"},
		{"--length", "20", idA, "src", "dst"},
		{idA, "src", "dst", "-f"},
	} {
		wantPos, wr, wf, wo, wl, werr := legacyFilePull(args)
		fs := cli.NewFlagSet(flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		b, err := cli.Parse(fs, args)
		if (err != nil) != (werr != nil) {
			t.Errorf("%q: err=%v legacy=%v", args, err, werr)
			continue
		}
		if err != nil {
			continue
		}
		got := mustBuild(t, cli, b).(FilePullAction)
		if got.Recursive != wr || got.Force != wf || got.Offset != wo || got.Length != wl {
			t.Errorf("%q: r/f/off/len = %v/%v/%d/%d, legacy %v/%v/%d/%d",
				args, got.Recursive, got.Force, got.Offset, got.Length, wr, wf, wo, wl)
		}
		if len(wantPos) != 3 || got.TaskID != wantPos[0] || got.RemoteSrc != wantPos[1] || got.LocalDst != wantPos[2] {
			t.Errorf("%q: positionals = %q/%q/%q, legacy %q",
				args, got.TaskID, got.RemoteSrc, got.LocalDst, wantPos)
		}
	}

	// The widening: -o / -n were TUI-only spellings before the migration.
	for _, args := range [][]string{
		{idA, "src", "dst", "-o", "5"},
		{idA, "src", "dst", "-n", "7"},
	} {
		fs := cli.NewFlagSet(flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		b, err := cli.Parse(fs, args)
		if err != nil {
			t.Errorf("%q: short offset/length spelling should now parse on every surface: %v", args, err)
		} else if _, berr := cli.BuildFunc()(b); berr != nil {
			t.Errorf("%q: Build: %v", args, berr)
		}
	}
}

// TestFileBrowserArityDropsTheLocalPath is D20 in practice: the same verb takes
// one fewer positional where there is no local filesystem to name.
func TestFileBrowserArityDropsTheLocalPath(t *testing.T) {
	push, _ := Lookup("file", "push")
	web := push.For(WebUI)
	fs := web.NewFlagSet(flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	b, err := web.Parse(fs, []string{idA, "docs/x.txt"})
	if err != nil {
		t.Fatalf("WebUI file push with two positionals should parse: %v", err)
	}
	got := mustBuild(t, web, b).(FilePushAction)
	if got.TaskID != idA || got.RemoteDst != "docs/x.txt" || got.LocalSrc != "" {
		t.Errorf("got %+v, want the destination in RemoteDst and no LocalSrc", got)
	}
	// And the CLI's three-positional form must NOT parse there: accepting it
	// would silently treat a local path as the destination.
	fs2 := web.NewFlagSet(flag.ContinueOnError)
	fs2.SetOutput(io.Discard)
	if _, err := web.Parse(fs2, []string{idA, "src", "dst"}); err == nil {
		t.Error("WebUI file push accepted three positionals; it has no local path to name")
	}
}

func mustBuild(t *testing.T, v VerbSpec, b Bound) Action {
	t.Helper()
	a, err := v.BuildFunc()(b)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return a
}
