package verb

import (
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
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
		// BuildFunc, not Build: a verb that declares Action gets its build
		// generated, and reading the field directly is how a caller finds nil
		// for a verb that is perfectly well wired.
		if v.BuildFunc() == nil {
			t.Errorf("%s: has neither a Build nor a generated one (declare Action, or write Build)", v.FlagSetName())
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

// TestUsagePositionalsParse holds the synopsis to the parser, positional by
// positional. TestUsageNamesItsVerb checked only the prefix, and
// TestExamplesParse checks lines an author chose -- neither reads the
// <angle-bracket> half, which is how `skill [--list] <name>...` came to print
// an ellipsis on an argument capped at one and `git diff <base> <target>`
// printed as required two revisions that are both optional.
//
// The check: build a line from the synopsis' own positional shapes -- fill
// each required one, omit every bracketed one -- and parse it. A synopsis
// that describes a call the parser refuses is the `board purge` shape, which
// is the whole reason Usage() is generated.
func TestUsagePositionalsParse(t *testing.T) {
	for _, v := range Verbs {
		if v.Trailing != nil || v.Pathspec {
			continue // free-form tails and `--` pathspecs are their own grammar
		}
		u := v.Usage()
		var args []string
		for _, a := range v.Args {
			optional := a.Optional || (a.Variadic && a.MaxCount == 1)
			var want string
			switch {
			case optional:
				want = "[<" + a.Name + ">]"
			case a.Variadic:
				want = "<" + a.Name + ">..."
			default:
				want = "<" + a.Name + ">"
			}
			// Checked for the optional ones TOO. The first version of this
			// `continue`d before the check, which left it inert for exactly
			// the shape it exists to pin -- confirmed by reverting the fix and
			// watching it stay green.
			if !strings.Contains(u, want) {
				t.Errorf("%s: Usage() does not spell %s as %q:\n  %s", v.FlagSetName(), a.Name, want, u)
			}
			if !optional {
				args = append(args, valueFor(a))
			}
		}
		// Brackets mean "may be omitted", so a Required flag must not wear
		// them: `board retract [--seq SEQ]` described the one call this verb
		// refuses.
		for _, f := range v.Flags {
			named := dashList([]string{f.Name})
			if f.Required {
				if strings.Contains(u, "["+named) {
					t.Errorf("%s: %s is Required and the synopsis brackets it:\n  %s", v.FlagSetName(), named, u)
				}
				continue
			}
			if !strings.Contains(u, "["+named) {
				t.Errorf("%s: %s may be omitted and the synopsis does not bracket it:\n  %s", v.FlagSetName(), named, u)
			}
		}
		// Plus whatever the cross-flag rules demand -- `forward` needs one of
		// -L/-R/-W and `caps set-parent` one of three. Those are rules the
		// synopsis does not render, not positional shapes it gets wrong.
		fs := v.NewFlagSet(flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		if _, err := v.Parse(fs, append(satisfyingFlags(v, nil, false), args...)); err != nil {
			t.Errorf("%s: the synopsis' own minimal positional form does not parse: %v\n  %s",
				v.FlagSetName(), err, u)
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

// TestEveryTuiParseFuncGoesThroughTheDeclaration closes the hole the test
// above cannot see.
//
// That one greps for `flag.NewFlagSet(`. Every parser that survived the
// migration in tui/cmdline.go walked argv BY HAND -- `for i := 0; i <
// len(args); i++` with a switch on the token -- so it built no FlagSet and the
// guard stayed green with twelve declared paths parsed somewhere else. Three
// of them had diverged: `caps set --caps X --scope-for Y` was refused by the
// CLI and silently accepted here with the override dropped, `notify --level
// warn --title T body` produced a notification TITLED "--level", and `session
// attach <id> --view` was "too many arguments" on the one surface where
// --view is declared.
//
// So this checks the positive property instead of a negative pattern: a
// function in that file whose name begins with `parse` must reach the
// declaration, directly or through one of the helpers that does.
func TestEveryTuiParseFuncGoesThroughTheDeclaration(t *testing.T) {
	const path = "../../tui/cmdline.go"
	src, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("read %s: %v", path, err)
	}
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	// Reaching the declaration means calling one of these, or being one.
	gateways := map[string]bool{
		"parseViaPath": true, "parseViaSpec": true, "parseViaSpec2": true,
		"parseSpawnTUI": true, "Lookup": true,
	}
	seen := map[string]bool{}
	// EVERY top-level func in the file, not just the ones spelled `parse*`.
	// Scanning by name prefix left two ways through, both confirmed by
	// negative control: the same hand walk moved inline into ParseCommand (a
	// capital P), and parseNotify renamed to handleNotify.
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil || fn.Body == nil {
			continue
		}
		if gateways[fn.Name.Name] {
			continue
		}
		// A function that never looks at argv is not a parser. The tell is
		// indexing or ranging a []string parameter.
		if !takesArgs(fn) {
			continue
		}
		seen[fn.Name.Name] = true
		if reason, exempt := tuiParseNotDeclared[fn.Name.Name]; exempt {
			if strings.TrimSpace(reason) == "" {
				t.Errorf("%s: an exemption needs a reason", fn.Name.Name)
			}
			continue
		}
		reaches := false
		ast.Inspect(fn, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch c := call.Fun.(type) {
			case *ast.Ident:
				if gateways[c.Name] || strings.HasPrefix(c.Name, "parse") {
					reaches = true
				}
			case *ast.SelectorExpr:
				if gateways[c.Sel.Name] {
					reaches = true
				}
			}
			return !reaches
		})
		if !reaches {
			t.Errorf("tui/cmdline.go: %s parses argv without reaching the declaration.\n"+
				"A hand-written token walk builds no FlagSet, so "+
				"TestNoHandWrittenVerbFlagSetsRemain cannot see it -- which is how "+
				"`caps set`, `caps set-parent` and `notify` kept grammars this "+
				"surface alone had. Route it through parseViaPath, or add %q to "+
				"tuiParseNotDeclared with the reason.", fn.Name.Name, fn.Name.Name)
		}
	}
}

// takesArgs reports whether a function has a []string parameter -- the shape
// every verb parser in this file has, and the thing a hand-written token walk
// needs.
func takesArgs(fn *ast.FuncDecl) bool {
	if fn.Type.Params == nil {
		return false
	}
	for _, p := range fn.Type.Params.List {
		if at, ok := p.Type.(*ast.ArrayType); ok {
			if id, ok := at.Elt.(*ast.Ident); ok && id.Name == "string" {
				return true
			}
		}
	}
	return false
}

// An exemption that outlives its function is a rule nobody is following.
func TestTuiParseExemptionsAreLive(t *testing.T) {
	src, err := os.ReadFile("../../tui/cmdline.go")
	if err != nil {
		t.Skip(err)
	}
	for name := range tuiParseNotDeclared {
		if !strings.Contains(string(src), "func "+name+"(") {
			t.Errorf("stale exemption: tui/cmdline.go has no %s", name)
		}
	}
}

// tuiParseNotDeclared names parse functions that deliberately do not reach the
// declaration, with the reason.
var tuiParseNotDeclared = map[string]string{
	// `repo <path>` sets what a later submit inherits when its line names no
	// --repo. It is this process's own session state, not a request, and it
	// is the SurfaceContext tier of --repo's ladder rather than a verb the
	// other surfaces have.
	"parseRepo": "sets this TUI session's default repo; the surface-context tier, not a request",
}
