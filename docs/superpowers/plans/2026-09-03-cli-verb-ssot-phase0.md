# CLI verb SSOT — Phase 0 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stand up `cli/verb` — one declaration per verb — and prove it end to end by migrating `prune` on all three surfaces, with the guard tests that make the next phases safe.

**Architecture:** A parse-only Go package holds a `VerbSpec` table. The CLI builds a `flag.FlagSet` from a spec and executes the resulting `Action`; the TUI calls the same parse; the WebUI reaches it through a new wasm bridge function and dispatches on the neutral `Bound` result. Execute and rendering stay per-surface on purpose.

**Tech Stack:** Go 1.25.7, stdlib `flag`, `syscall/js` for the wasm bridge, vanilla JS for the WebUI. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-09-03-cli-verb-ssot-design.md`
(companion data: `docs/superpowers/specs/2026-09-03-cli-verb-flag-inventory.md`)

## Global Constraints

- **Scope is Phase 0 only.** `prune` is the only verb migrated. The spec's Phase 1-6 families are untouched; their legacy parsers stay exactly as they are.
- **A newly found exception halts the phase.** Spec, "A newly found exception halts the phase": if something contradicts a decision (D1-D24), stop, write down what was found and which decision it contradicts, and bring it back. Do not absorb it and do not decide it alone.
- **`cli/verb` must never import `cli`.** D23. It imports `runner/protocol` and stdlib only. `cli` imports `cli/verb`. Task 1 adds the test that enforces this.
- **D23 is applied in full.** All four grammar primitives move into `cli/verb` in Task 1: `ParsePermuted` (`cli/permute.go`, 37 lines), `ParseGridArgs` (`cli/gridargs.go`, 68), `ParseCaps` (`cli/caps.go`, 289) and `ParseScope` (`cli/scope.go`, 421). Measured before deciding: every one of them imports only stdlib plus `runner/protocol`, which is exactly what `cli/verb` may import, so there is no dependency barrier. Doing only the one `prune` happens to need would be scope contraction dressed as sequencing — the failure `implementation-pitfalls` Pitfall 1 records — and would make Phases 4 and 6 re-decide the same thing.
  What imports cannot show is whether those files reference symbols declared in *other* `cli` files, since same-package references need none. The compiler answers that immediately. **If a primitive drags in other `cli` symbols, that is an exception: stop, record what it pulls and which decision it contradicts, and bring it back** — do not fall back to moving a subset.
- **Cross-surface dispatch completeness is Phase 1 work, not Phase 0.** The spec's completeness tests assert every declared verb has a dispatch entry on each of its surfaces. With one verb in the table the CLI and TUI checks would assert a single hard-coded pair and teach nothing; the WebUI one lands now (Task 6) because it is a runtime assertion the page needs anyway. The CLI and TUI AST checks are the first task of Phase 1, when there are two families to be inconsistent about.
- **Aliases are declared, never inferred.** D4. `-e` is not short for `--enter`.
- **`Trailing == nil` implies permuted parsing.** D6. `prune` has `Trailing == nil`.
- **No new dependencies.** `go.mod` gains nothing.
- **Verify with make targets, not ad-hoc `go build`:** `make check`, `make vet`, `make test`, `make wasm-check`. `go build ./...` alone hides wasm-only breakage.
- **Behaviour must not change in Phase 0.** Every task's differential test asserts the new path produces exactly what the legacy path produced.

## File Structure

| File | Responsibility |
| --- | --- |
| `cli/verb/verb.go` (new) | `Surface`, `VerbSpec`, `Arg`, `Flag`, `Trailing`, `Bound`, `Action`, `ActionMarker` — the declaration vocabulary |
| `cli/verb/permute.go`, `gridargs.go`, `caps.go`, `scope.go` (new, moved) | The four grammar primitives, verbatim from `cli/` (D23) |
| `cli/permute.go`, `cli/gridargs.go`, `cli/caps.go`, `cli/scope.go` (modify) | Thin re-exports so existing call sites keep compiling |
| `cli/verb/build.go` (new) | `VerbSpec.NewFlagSet`, `VerbSpec.Parse` → `Bound` |
| `cli/verb/table.go` (new) | The `Verbs` table. Phase 0 holds one entry: `prune` |
| `cli/verb/actions.go` (new) | Shared action types. Phase 0 holds `PruneAction` |
| `cli/verb/usage.go` (new) | `VerbSpec.Usage()` — the generated usage line |
| `cli/permute.go` (modify) | Becomes a one-line re-export so the ~30 existing call sites keep compiling |
| `cli/verb/import_test.go` (new) | Guard: `cli/verb` imports nothing from `cli` |
| `cli/verb/alias_test.go` (new) | Guard: declared `Aliases` match the legacy FlagSet's `Value`-pointer grouping |
| `cli/verb/invariant_test.go` (new) | Guard: `Trailing == nil` ⇒ permuted; `WidensIfUnset` ⇒ its verb permutes |
| `cli/verb/differential_test.go` (new) | Legacy `prune` parse vs declaration parse, same corpus |
| `cmd/harness-cli/main.go` (modify, `case "prune":` at :340-352) | Parse via the spec; keep only the execute half |
| `tui/cmdline.go` (modify: `PruneAction` :45-50, `parsePrune` :766-777, dispatch :451) | Delete `parsePrune`; call `verb.Parse` |
| `tui/app.go` (modify, `case PruneAction:` :3145) | Becomes `case verb.PruneAction:` |
| `cmd/harness-webui-wasm/main.go` (modify, bridge table :87) | Add `parseCommand` and `pathsForSurface` |
| `webui/static/main.js` (modify, `case "prune"` :2484-2512) | Delete the hand-written loop; dispatch on `Bound` |

---

### Task 1: `cli/verb` skeleton, `ParsePermuted` moved, import direction enforced

**Files:**
- Create: `cli/verb/verb.go`, `cli/verb/permute.go`, `cli/verb/import_test.go`
- Modify: `cli/permute.go`
- Test: `cli/verb/import_test.go`

**Interfaces:**
- Consumes: nothing.
- Produces: `verb.Surface` (`verb.CLI`, `verb.TUI`, `verb.WebUI`), `verb.VerbSpec`, `verb.Arg`, `verb.Flag`, `verb.Trailing`, `verb.Bound`, `verb.Action`, `verb.ActionMarker`, `verb.ParsePermuted(fs *flag.FlagSet, args []string) ([]string, error)`. `cli.ParsePermuted` keeps its exact current signature as a re-export.

- [ ] **Step 1: Write the failing guard test**

`cli/verb/import_test.go`:

```go
package verb_test

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestVerbImportsNothingFromCli is the load-bearing constraint of the whole
// design (spec D23). cli/cmd_board.go parses the `board` family INSIDE package
// cli, so cli must be free to import cli/verb. The moment cli/verb imports cli
// back, that becomes a cycle and the board migration is blocked -- discovered
// at Phase 3, after five phases were built on the wrong direction.
//
// Checked here rather than left to the compiler because the compiler only
// complains once the cycle actually closes, which is phases away.
func TestVerbImportsNothingFromCli(t *testing.T) {
	const forbidden = "github.com/on-keyday/agent-harness/cli"

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read .: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, e.Name(), nil, parser.ImportsOnly)
		if perr != nil {
			t.Fatalf("parse %s: %v", e.Name(), perr)
		}
		for _, imp := range f.Imports {
			path, uerr := strconv.Unquote(imp.Path.Value)
			if uerr != nil {
				continue
			}
			if path == forbidden {
				t.Errorf("%s:%d: cli/verb imports %q.\n"+
					"cli/verb is BELOW cli: package cli parses the board family itself "+
					"(cli/cmd_board.go), so cli must import cli/verb and not the reverse. "+
					"If a helper is needed here, move the helper down rather than importing up.",
					e.Name(), fset.Position(imp.Pos()).Line, path)
			}
		}
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./cli/verb/ -run TestVerbImportsNothingFromCli -v`
Expected: FAIL — the package does not exist yet (`no Go files in .../cli/verb`).

- [ ] **Step 3: Create the declaration vocabulary**

`cli/verb/verb.go`:

```go
// Package verb holds the single declaration of this project's command
// grammar: one VerbSpec per verb path, from which the CLI builds its
// FlagSet, the TUI parses, and the WebUI parses through the wasm bridge.
//
// It is deliberately BELOW package cli (see import_test.go): cli parses the
// board family itself, so cli imports verb and never the reverse.
//
// Parse only. What a surface DOES with a parsed Action -- stdout and an exit
// code, a tea.Cmd, a DOM update -- stays in that surface (spec D3, D10).
package verb

import "flag"

// Surface is the set of front-ends a verb or option is reachable from.
// Which surfaces a verb appears on is part of the declaration, not an
// invariant over it: `trsf` is TUI-only and `board` is CLI-only, both on
// purpose.
type Surface uint8

const (
	CLI Surface = 1 << iota
	TUI
	WebUI
)

// Has reports whether s includes every bit of want.
func (s Surface) Has(want Surface) bool { return s&want == want }

// Action is what a parsed command becomes. The marker is EXPORTED because
// surface-local actions live in their own packages -- tui's ClearAction and
// QuitAction cannot implement an unexported method declared here.
type Action interface{ IsAction() }

// ActionMarker is embedded by every action type to satisfy Action.
type ActionMarker struct{}

// IsAction marks the embedding type as an Action.
func (ActionMarker) IsAction() {}

// ArgType names how a positional is validated.
type ArgType int

const (
	ArgString ArgType = iota
	ArgTaskID         // 32 hex
	ArgTopic
	ArgUint
)

// Arg is one positional argument.
type Arg struct {
	Name     string
	Type     ArgType
	Variadic bool // absorbs the rest; only valid as the last Arg

	// Surfaces narrower than the verb's requires a reason (spec D20):
	// `file push` takes one fewer positional in a browser, which has no
	// local path to name.
	Surfaces      Surface
	SurfaceReason string
}

// FlagType names a flag's value type.
type FlagType int

const (
	FlagBool FlagType = iota
	FlagString
	FlagUint
	FlagUint64
	FlagDuration
)

// Flag is one option.
type Flag struct {
	Name string

	// Aliases are additional spellings for THIS flag. Declared explicitly and
	// never inferred from spelling (spec D4): `session send` registers
	// --enter (append a carriage return) and -e (interpret backslash escapes)
	// as two independent flags, while `session new` binds --detach and -d to
	// one variable and `git diff` does the same for --staged and --cached.
	// Pairing short with long by shape would merge the first pair and inject
	// a spurious Enter into a live PTY.
	Aliases []string

	Type    FlagType
	Default any
	Help    string

	// WidensIfUnset marks a flag whose ABSENCE makes the operation cover more.
	// `board purge <topic> --seq N` -- the line the help text printed -- left
	// --seq at its zero value, which is the whole-topic form, and destroyed
	// two messages on a live board. Stated here so the property lives with
	// the flag instead of in a comment in permute.go.
	WidensIfUnset bool

	Surfaces      Surface
	SurfaceReason string
}

// Trailing describes free-form words after the positionals. Non-nil means the
// verb CANNOT permute: a '-'-leading word is indistinguishable from a flag,
// so flags must precede the text.
type Trailing struct {
	Name   string
	Reason string
}

// VerbSpec is one verb path's whole grammar.
type VerbSpec struct {
	Path     []string
	Surfaces Surface
	Args     []Arg
	Flags    []Flag
	Trailing *Trailing
	Examples []string
	Build    func(Bound) (Action, error)
}

// Bound is a parsed command before Build: the neutral form the wasm bridge
// hands to JS, so no per-action marshaller is needed (spec D8).
type Bound struct {
	Path  []string
	Args  []string
	Flags map[string]any
	Set   map[string]bool // flags the caller actually supplied
	Trail string
}

// Str returns a string flag's value.
func (b Bound) Str(name string) string {
	v, _ := b.Flags[name].(string)
	return v
}

// Bool returns a bool flag's value.
func (b Bound) Bool(name string) bool {
	v, _ := b.Flags[name].(bool)
	return v
}

// FlagSetName is the name a verb's FlagSet carries, and the key the alias
// guard matches a legacy FlagSet by.
func (v VerbSpec) FlagSetName() string {
	out := ""
	for i, p := range v.Path {
		if i > 0 {
			out += " "
		}
		out += p
	}
	return out
}

// ErrorHandling is re-exported so callers need not import flag for the one
// argument NewFlagSet takes: the CLI wants ExitOnError, the TUI
// ContinueOnError with a discarded writer.
type ErrorHandling = flag.ErrorHandling
```

- [ ] **Step 4: Move all four grammar primitives down (D23)**

```bash
for f in permute gridargs caps scope; do
  git mv cli/$f.go cli/verb/$f.go
  sed -i '1s/^package cli$/package verb/' cli/verb/$f.go
done
go build ./cli/verb/
```

Expected: builds clean. **If it does not** — because one of these files references a symbol declared in another `cli` file — that is the exception this task's constraint names: stop, record what it pulls in, and bring it back. Do not move a subset instead.

Also move the primitives' own tests so they keep running against their code:

```bash
for f in caps_test scope_test scope_spec_test caps_completeness_test gridargs_test; do
  [ -f cli/$f.go ] && git mv cli/$f.go cli/verb/$f.go && sed -i '1s/^package cli$/package verb/' cli/verb/$f.go
done
go test ./cli/verb/
```

- [ ] **Step 5: Re-export from `cli` so existing call sites keep compiling**

Create `cli/grammar.go`:

```go
package cli

import (
	"flag"

	"github.com/on-keyday/agent-harness/cli/verb"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// The grammar primitives moved down to cli/verb (spec D23): package cli parses
// the board family itself, so cli must be free to import verb and not the
// reverse. These re-exports exist because roughly thirty call sites across
// cmd/harness-cli, cli, cli/agent and tui name them, and relocating a function
// is not a reason to touch all of them in one commit.

// ParsePermuted parses fs tolerating flags written after positionals.
func ParsePermuted(fs *flag.FlagSet, args []string) ([]string, error) {
	return verb.ParsePermuted(fs, args)
}

// ParseCaps parses a --caps mask.
func ParseCaps(s string) (protocol.Capability, error) { return verb.ParseCaps(s) }

// ParseScope parses a --scope spec.
func ParseScope(s string) (protocol.TaskScope, error) { return verb.ParseScope(s) }

// ParseGridArgs parses the `grid` command's arguments.
func ParseGridArgs(args []string) (verb.GridScopeMode, string, []string, error) {
	return verb.ParseGridArgs(args)
}
```

Every other exported name those four files carried — `CapsFlagUsage`, `ScopeGrammar`, `CapsCatalog`, `ScopesCatalog`, `GridArgsUsage`, `GridArgsString`, `GridScopeMode`, `ScopeForFlagUsage`, `ParseScopeFor`, `OverridesLabel`, `FormatPruneCutoff` if it lives there — needs the same treatment. Enumerate them from the moved files before writing this file:

```bash
grep -hE '^(func|type|const|var) [A-Z]' cli/verb/{permute,gridargs,caps,scope}.go
```

and add a re-export (`func`) or alias (`type X = verb.X`, `const`/`var`) for each. `go build ./...` names anything missed.

- [ ] **Step 6: Run the guard test and the build**

Run: `go test ./cli/verb/ -run TestVerbImportsNothingFromCli -v`
Expected: PASS.

Run: `make vet && make check`
Expected: both succeed, no output from vet.

Run: `go test ./cli/... ./cmd/... ./tui/...`
Expected: PASS — the re-export means no caller changed.

- [ ] **Step 7: Commit**

```bash
git add cli/verb/verb.go cli/verb/permute.go cli/verb/import_test.go cli/permute.go
git commit -m "feat(verb): declaration vocabulary, with ParsePermuted moved below cli

cli/verb is the grammar package the three surfaces will parse from. It sits
BELOW cli on purpose (spec D23): cli/cmd_board.go parses the board family
inside package cli, so cli must be free to import verb. import_test.go
enforces that direction now rather than letting the compiler discover it at
Phase 3, after five phases were built the wrong way round.

ParsePermuted moves down with it; cli keeps a re-export so the ~30 existing
call sites are untouched.

The Action marker is exported (ActionMarker), unlike tui's current
unexported isAction(): surface-local actions such as tui's ClearAction live
in their own package and cannot implement an unexported method declared here."
```

---

### Task 2: Build a FlagSet from a spec and parse to `Bound`

**Files:**
- Create: `cli/verb/build.go`, `cli/verb/build_test.go`
- Test: `cli/verb/build_test.go`

**Interfaces:**
- Consumes: `verb.VerbSpec`, `verb.Bound`, `verb.ParsePermuted` from Task 1.
- Produces: `func (v VerbSpec) NewFlagSet(eh ErrorHandling) *flag.FlagSet`, `func (v VerbSpec) Parse(fs *flag.FlagSet, args []string) (Bound, error)`.

- [ ] **Step 1: Write the failing test**

`cli/verb/build_test.go`:

```go
package verb

import (
	"flag"
	"io"
	"testing"
	"time"
)

// synthetic spec: one bool with an alias, one duration, variadic positionals.
// Shaped like prune without being prune, so this test keeps passing when the
// real declaration changes.
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

// The whole point of D6. Without permuting, `probe aaa --force` leaves force
// false -- the board purge failure, which destroyed two live messages.
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
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./cli/verb/ -run 'TestParse|TestAlias|TestFlagAfter|TestSet' -v`
Expected: FAIL — `sp.NewFlagSet undefined`.

- [ ] **Step 3: Implement**

`cli/verb/build.go`:

```go
package verb

import (
	"flag"
	"fmt"
	"strings"
	"time"
)

// NewFlagSet registers this verb's flags, aliases included, on a fresh
// FlagSet named after the verb path.
//
// eh differs per surface and always has: the CLI takes ExitOnError because a
// bad command line should end the process with usage, while the TUI takes
// ContinueOnError with a discarded writer because a typo there is a line in a
// results pane, not an exit.
func (v VerbSpec) NewFlagSet(eh ErrorHandling) *flag.FlagSet {
	fs := flag.NewFlagSet(v.FlagSetName(), eh)
	for _, f := range v.Flags {
		names := append([]string{f.Name}, f.Aliases...)
		switch f.Type {
		case FlagBool:
			p := new(bool)
			if d, ok := f.Default.(bool); ok {
				*p = d
			}
			for _, n := range names {
				fs.BoolVar(p, n, *p, f.Help)
			}
		case FlagString:
			p := new(string)
			if d, ok := f.Default.(string); ok {
				*p = d
			}
			for _, n := range names {
				fs.StringVar(p, n, *p, f.Help)
			}
		case FlagUint:
			p := new(uint)
			if d, ok := f.Default.(uint); ok {
				*p = d
			}
			for _, n := range names {
				fs.UintVar(p, n, *p, f.Help)
			}
		case FlagUint64:
			p := new(uint64)
			if d, ok := f.Default.(uint64); ok {
				*p = d
			}
			for _, n := range names {
				fs.Uint64Var(p, n, *p, f.Help)
			}
		case FlagDuration:
			p := new(time.Duration)
			if d, ok := f.Default.(time.Duration); ok {
				*p = d
			}
			for _, n := range names {
				fs.DurationVar(p, n, *p, f.Help)
			}
		}
	}
	return fs
}

// Parse runs fs over args and returns the neutral Bound.
//
// Permuted unless Trailing is set: with stdlib flag alone, parsing stops at
// the first non-flag token and a flag written after a positional is silently
// dropped. A verb WITH Trailing cannot permute, because a '-'-leading word in
// free-form text is indistinguishable from a flag.
func (v VerbSpec) Parse(fs *flag.FlagSet, args []string) (Bound, error) {
	var (
		positionals []string
		err         error
	)
	if v.Trailing == nil {
		positionals, err = ParsePermuted(fs, args)
	} else {
		err = fs.Parse(args)
		positionals = fs.Args()
	}
	if err != nil {
		return Bound{}, err
	}

	b := Bound{
		Path:  v.Path,
		Flags: map[string]any{},
		Set:   map[string]bool{},
	}
	// Canonical names only: an alias is a spelling, not a key. Reading the
	// value off the canonical name's registration is enough because
	// NewFlagSet binds every spelling to one variable.
	for _, f := range v.Flags {
		fl := fs.Lookup(f.Name)
		if fl == nil {
			continue
		}
		b.Flags[f.Name] = flagValue(fl)
	}
	canonical := map[string]string{}
	for _, f := range v.Flags {
		canonical[f.Name] = f.Name
		for _, a := range f.Aliases {
			canonical[a] = f.Name
		}
	}
	fs.Visit(func(fl *flag.Flag) {
		if name, ok := canonical[fl.Name]; ok {
			b.Set[name] = true
		}
	})

	fixed := 0
	for _, a := range v.Args {
		if !a.Variadic {
			fixed++
		}
	}
	if v.Trailing != nil {
		if len(positionals) < fixed {
			return Bound{}, fmt.Errorf("%s: %s", v.FlagSetName(), v.Usage())
		}
		b.Args = positionals[:fixed]
		b.Trail = strings.Join(positionals[fixed:], " ")
		return b, nil
	}
	if err := v.checkArity(len(positionals)); err != nil {
		return Bound{}, err
	}
	b.Args = positionals
	return b, nil
}

// checkArity replaces the per-verb `len(pargs) != 3` checks the three
// surfaces each wrote by hand.
func (v VerbSpec) checkArity(n int) error {
	fixed, variadic := 0, false
	for _, a := range v.Args {
		if a.Variadic {
			variadic = true
			continue
		}
		fixed++
	}
	if n < fixed || (!variadic && n > fixed) {
		return fmt.Errorf("%s: %s", v.FlagSetName(), v.Usage())
	}
	return nil
}

// flagValue reads a parsed flag's typed value. Every stdlib flag Value
// implements Getter; the fallback is for a custom flag.Value that does not.
func flagValue(fl *flag.Flag) any {
	if g, ok := fl.Value.(flag.Getter); ok {
		return g.Get()
	}
	return fl.Value.String()
}
```

- [ ] **Step 4: Add the generated usage line**

`cli/verb/usage.go`:

```go
package verb

import "strings"

// Usage renders the verb's synopsis from the declaration.
//
// Generated rather than written by hand because a hand-written usage line
// drifts from the parser: `board purge <topic> --seq N` is what the help text
// told operators to type, and stdlib parsing left --seq unread, so the call
// fell through to the whole-topic form and destroyed two live messages.
// Printing and parsing now come from one source.
func (v VerbSpec) Usage() string {
	var b strings.Builder
	b.WriteString("usage: ")
	b.WriteString(v.FlagSetName())
	for _, f := range v.Flags {
		b.WriteString(" [--")
		b.WriteString(f.Name)
		if f.Type != FlagBool {
			b.WriteString(" ")
			b.WriteString(strings.ToUpper(f.Name))
		}
		b.WriteString("]")
	}
	for _, a := range v.Args {
		b.WriteString(" <")
		b.WriteString(a.Name)
		b.WriteString(">")
		if a.Variadic {
			b.WriteString("...")
		}
	}
	if v.Trailing != nil {
		b.WriteString(" <")
		b.WriteString(v.Trailing.Name)
		b.WriteString(">...")
	}
	return b.String()
}
```

- [ ] **Step 5: Run the tests**

Run: `go test ./cli/verb/ -v`
Expected: PASS, all five tests.

- [ ] **Step 6: Commit**

```bash
git add cli/verb/build.go cli/verb/usage.go cli/verb/build_test.go
git commit -m "feat(verb): build a FlagSet from a spec and parse to Bound

NewFlagSet binds every spelling of a flag -- name and aliases -- to one
variable, so an alias can never drift from the flag it spells. Parse permutes
unless Trailing is set, which is D6's invariant expressed once instead of per
verb, and collects into Bound under canonical names only.

Usage is generated from the same spec that parses. That is the actual repair
for the board purge class: the printed invocation and the accepted invocation
now cannot disagree."
```

---

### Task 3: Declare `prune`, wire the CLI, prove no behaviour change

**Files:**
- Create: `cli/verb/table.go`, `cli/verb/actions.go`, `cli/verb/differential_test.go`
- Modify: `cmd/harness-cli/main.go:340-352`
- Test: `cli/verb/differential_test.go`

**Interfaces:**
- Consumes: everything from Tasks 1-2.
- Produces: `verb.Verbs []VerbSpec`, `verb.Lookup(path ...string) (VerbSpec, bool)`, `verb.PruneAction{Before time.Duration; TaskIDs []string; Force bool}`.

- [ ] **Step 1: Write the failing differential test**

`cli/verb/differential_test.go`:

```go
package verb

import (
	"flag"
	"io"
	"reflect"
	"testing"
	"time"
)

// legacyPrune is cmd/harness-cli/main.go:341-345 verbatim, kept here for the
// length of the migration. When the CLI stops parsing prune by hand this
// function is the only remaining copy, and deleting it is the last step of
// the family's migration.
func legacyPrune(args []string) (time.Duration, []string, bool, error) {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	before := fs.Duration("before", 7*24*time.Hour, "")
	force := fs.Bool("force", false, "")
	fs.BoolVar(force, "f", false, "")
	taskIDs, err := ParsePermuted(fs, args)
	if err != nil {
		return 0, nil, false, err
	}
	return *before, taskIDs, *force, nil
}

// The corpus is every prune form the three surfaces document or test:
// tui/cmdline_test.go:173-215 supplies the first four, the CLI's usage line
// supplies the rest.
var pruneCorpus = [][]string{
	{},
	{"--before=1h"},
	{"--before", "1h"},
	{"--force", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
	{"-f", "deadbeefdeadbeefdeadbeefdeadbeef"},
	{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "--force"},
	{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
	{"--before", "30m", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
}

// TestPruneDeclarationMatchesLegacy is the proof that Phase 0 changed no
// behaviour. It is the gate on deleting the hand-written parser, and the
// pattern every later phase repeats.
func TestPruneDeclarationMatchesLegacy(t *testing.T) {
	sp, ok := Lookup("prune")
	if !ok {
		t.Fatal("prune is not in the table")
	}
	for _, args := range pruneCorpus {
		wantBefore, wantIDs, wantForce, wantErr := legacyPrune(args)

		fs := sp.NewFlagSet(flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		b, err := sp.Parse(fs, args)
		if (err != nil) != (wantErr != nil) {
			t.Errorf("%q: err = %v, legacy err = %v", args, err, wantErr)
			continue
		}
		if err != nil {
			continue
		}
		act, berr := sp.Build(b)
		if berr != nil {
			t.Errorf("%q: Build: %v", args, berr)
			continue
		}
		got, isPrune := act.(PruneAction)
		if !isPrune {
			t.Errorf("%q: Build returned %T, want PruneAction", args, act)
			continue
		}
		if got.Before != wantBefore {
			t.Errorf("%q: Before = %v, legacy %v", args, got.Before, wantBefore)
		}
		if got.Force != wantForce {
			t.Errorf("%q: Force = %v, legacy %v", args, got.Force, wantForce)
		}
		if !reflect.DeepEqual(nilAsEmpty(got.TaskIDs), nilAsEmpty(wantIDs)) {
			t.Errorf("%q: TaskIDs = %q, legacy %q", args, got.TaskIDs, wantIDs)
		}
	}
}

func nilAsEmpty(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./cli/verb/ -run TestPruneDeclarationMatchesLegacy -v`
Expected: FAIL — `undefined: Lookup`, `undefined: PruneAction`.

- [ ] **Step 3: Add the action type**

`cli/verb/actions.go`:

```go
package verb

import "time"

// PruneAction asks the server to forget tasks. With TaskIDs empty the server
// runs in time mode (terminal tasks older than Before); with TaskIDs set it
// considers only those, ignores Before, and skips still-active tasks unless
// Force.
//
// Moved here from tui/cmdline.go so all three surfaces name one type. The
// TUI's own screen-state actions (ClearAction, QuitAction, ...) stay in tui
// and satisfy Action by embedding ActionMarker.
type PruneAction struct {
	ActionMarker
	Before  time.Duration
	TaskIDs []string
	Force   bool
}
```

- [ ] **Step 4: Add the table with `prune` in it**

`cli/verb/table.go`:

```go
package verb

import "time"

// Verbs is the declaration. One entry per verb path.
//
// Phase 0 holds prune alone: it is on all three surfaces, is the smallest
// family, has a real alias (--force/-f), takes no free-form trailing text,
// and is the one family whose TUI parser has no arity check. Migrating it
// exercises every mechanism this package has exactly once.
var Verbs = []VerbSpec{
	{
		Path:     []string{"prune"},
		Surfaces: CLI | TUI | WebUI,
		Args: []Arg{
			{Name: "task-id", Type: ArgTaskID, Variadic: true},
		},
		Flags: []Flag{
			{
				Name: "before", Type: FlagDuration, Default: 7 * 24 * time.Hour,
				Help: "forget terminal tasks older than this (ignored when TASK_IDs are passed)",
			},
			{
				Name: "force", Aliases: []string{"f"}, Type: FlagBool, Default: false,
				Help: "with TASK_IDs: also forget tasks the server still considers active (Queued/Running/Detached)",
			},
		},
		Examples: []string{
			"prune",
			"prune --before 24h",
			"prune --force aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"prune aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa --force",
		},
		Build: func(b Bound) (Action, error) {
			d, _ := b.Flags["before"].(time.Duration)
			return PruneAction{
				Before:  d,
				TaskIDs: b.Args,
				Force:   b.Bool("force"),
			}, nil
		},
	},
}

// Lookup finds the spec for a verb path.
func Lookup(path ...string) (VerbSpec, bool) {
	for _, v := range Verbs {
		if len(v.Path) != len(path) {
			continue
		}
		match := true
		for i := range path {
			if v.Path[i] != path[i] {
				match = false
				break
			}
		}
		if match {
			return v, true
		}
	}
	return VerbSpec{}, false
}
```

- [ ] **Step 5: Run the differential test**

Run: `go test ./cli/verb/ -run TestPruneDeclarationMatchesLegacy -v`
Expected: PASS.

- [ ] **Step 6: Add the examples test**

Append to `cli/verb/differential_test.go`:

```go
// TestExamplesParse is not cosmetic. The board purge incident was a usage line
// the parser rejected, and Usage() is now generated from this same spec -- so
// an example that fails to parse is a documented invocation that does not work.
func TestExamplesParse(t *testing.T) {
	for _, v := range Verbs {
		for _, ex := range v.Examples {
			fields := splitExample(ex, v)
			fs := v.NewFlagSet(flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			b, err := v.Parse(fs, fields)
			if err != nil {
				t.Errorf("%s: example %q does not parse: %v\n%s",
					v.FlagSetName(), ex, err, v.Usage())
				continue
			}
			if _, err := v.Build(b); err != nil {
				t.Errorf("%s: example %q parses but does not build: %v", v.FlagSetName(), ex, err)
			}
		}
	}
}

// splitExample drops the verb path words from the front of an example and
// returns the argument tail.
func splitExample(ex string, v VerbSpec) []string {
	fields := strings.Fields(ex)
	if len(fields) < len(v.Path) {
		return nil
	}
	return fields[len(v.Path):]
}
```

Add `"strings"` to that file's imports.

Run: `go test ./cli/verb/ -run TestExamplesParse -v`
Expected: PASS.

- [ ] **Step 7: Wire the CLI**

In `cmd/harness-cli/main.go`, replace the body of `case "prune":` (lines 340-352) with:

```go
	case "prune":
		sp, ok := verb.Lookup("prune")
		if !ok {
			die(fmt.Errorf("prune: not in the verb table"))
		}
		fs := sp.NewFlagSet(flag.ExitOnError)
		b, perr := sp.Parse(fs, args)
		if perr != nil {
			die(perr)
		}
		act, berr := sp.Build(b)
		if berr != nil {
			die(berr)
		}
		p := act.(verb.PruneAction)
		if err := cli.Prune(ctx, parseCID(), p.Before, p.TaskIDs, p.Force, os.Stdout); err != nil {
			die(err)
		}
```

Add the import `"github.com/on-keyday/agent-harness/cli/verb"` to that file.

- [ ] **Step 8: Verify the CLI still behaves**

Run: `make vet && make check && make test`
Expected: all pass.

Run: `go run ./cmd/harness-cli prune --help 2>&1 | head -5`
Expected: the flag package's usage listing `-before` and `-f`/`-force`, no panic.

- [ ] **Step 9: Commit**

```bash
git add cli/verb/table.go cli/verb/actions.go cli/verb/differential_test.go cmd/harness-cli/main.go
git commit -m "feat(verb): declare prune and parse the CLI's prune from it

prune is the Phase 0 verb because it exercises every mechanism once: three
surfaces, a real alias, no trailing text, and a TUI parser with no arity
check. main.go's case keeps only the execute half.

The differential test holds the legacy parser beside the declaration and
asserts they agree over the corpus the three surfaces document, which is what
makes deleting a hand-written parser safe rather than hopeful. TestExamplesParse
closes the other half of the board purge failure: every documented invocation
must parse, and Usage() is generated from the spec that parses it."
```

---

### Task 4: Alias-grouping guard

**Files:**
- Create: `cli/verb/alias_test.go`
- Test: `cli/verb/alias_test.go`

**Interfaces:**
- Consumes: `verb.Verbs`, `verb.VerbSpec.NewFlagSet` from Tasks 1-3.
- Produces: nothing consumed by later tasks; a guard that lives for the migration.

- [ ] **Step 1: Write the test**

`cli/verb/alias_test.go`:

```go
package verb

import (
	"flag"
	"reflect"
	"sort"
	"testing"
)

// legacyFlagSets pairs a verb path with the FlagSet the pre-migration code
// built for it. Entries are ADDED as families migrate and REMOVED with the
// last legacy parser, so this file empties itself out.
var legacyFlagSets = map[string]func() *flag.FlagSet{
	"prune": func() *flag.FlagSet {
		fs := flag.NewFlagSet("prune", flag.ContinueOnError)
		fs.Duration("before", 0, "")
		force := fs.Bool("force", false, "")
		fs.BoolVar(force, "f", false, "")
		return fs
	},
}

// TestDeclaredAliasesMatchLegacyGrouping derives the ground truth for which
// spellings are one flag from the legacy FlagSet, by grouping names whose
// flag.Flag.Value is the same pointer. stdlib's newBoolValue is
// (*boolValue)(p) -- a pointer conversion, not an allocation -- so two
// bindings of one variable share a pointer while two independent fs.Bool
// calls do not.
//
// This exists because `-e` is NOT short for `--enter`: session send registers
// --enter (append a carriage return) and -e (interpret backslash escapes) as
// two independent flags, while session new binds --detach and -d to one
// variable and git diff does the same for --staged and --cached. Declaring
// the first pair as an alias would turn `session send -e '...'` into a
// spurious Enter typed into a live PTY -- and it would compile.
func TestDeclaredAliasesMatchLegacyGrouping(t *testing.T) {
	for _, v := range Verbs {
		build, have := legacyFlagSets[v.FlagSetName()]
		if !have {
			continue // not yet migrated, or its legacy parser is already gone
		}
		want := groupByValuePointer(build())
		got := groupByValuePointer(v.NewFlagSet(flag.ContinueOnError))
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: alias grouping differs.\n declared: %v\n   legacy: %v\n"+
				"Two spellings belong together only when the legacy code bound them to ONE "+
				"variable. Sharing a prefix is not evidence: session send's --enter and -e "+
				"are different flags.", v.FlagSetName(), got, want)
		}
	}
}

// groupByValuePointer returns the name groups, each sorted, ordered by their
// first name, so two FlagSets compare regardless of registration order.
func groupByValuePointer(fs *flag.FlagSet) [][]string {
	byPtr := map[uintptr][]string{}
	fs.VisitAll(func(f *flag.Flag) {
		p := reflect.ValueOf(f.Value).Pointer()
		byPtr[p] = append(byPtr[p], f.Name)
	})
	out := make([][]string, 0, len(byPtr))
	for _, names := range byPtr {
		sort.Strings(names)
		out = append(out, names)
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}
```

- [ ] **Step 2: Run it**

Run: `go test ./cli/verb/ -run TestDeclaredAliasesMatchLegacyGrouping -v`
Expected: PASS — `prune` declares `force` with alias `f`, and the legacy FlagSet binds both to one `*bool`.

- [ ] **Step 3: Prove the guard actually catches the mistake**

Temporarily edit `cli/verb/table.go` to split the alias — change `{Name: "force", Aliases: []string{"f"}, ...}` into two entries, `{Name: "force", ...}` and `{Name: "f", ...}`.

Run: `go test ./cli/verb/ -run TestDeclaredAliasesMatchLegacyGrouping -v`
Expected: FAIL, reporting `declared: [[f] [force]]` against `legacy: [[f force]]`.

Revert the edit. Run again.
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add cli/verb/alias_test.go
git commit -m "test(verb): derive alias grouping from the legacy FlagSet

Ground truth for 'are these two spellings one flag' is pointer identity of
flag.Flag.Value: stdlib's newBoolValue is a pointer conversion, so binding one
variable twice shares a pointer while two independent fs.Bool calls do not.

The mistake this refuses is assuming -e is short for --enter. It is not:
session send registers --enter (append a carriage return) and -e (interpret
backslash escapes) as separate flags, and merging them would type a spurious
Enter into a live PTY while compiling cleanly. Verified by splitting prune's
alias and watching the test fail.

legacyFlagSets grows as families migrate and empties with the last legacy
parser, so the guard is scaffolding with a defined end."
```

---

### Task 5: Wire the TUI onto the declaration

**Files:**
- Modify: `tui/cmdline.go` (delete `parsePrune` :766-777 and `PruneAction` :45-50; dispatch at :451), `tui/app.go:3145`
- Test: `tui/cmdline_test.go` (existing tests at :173-215 must keep passing unchanged)

**Interfaces:**
- Consumes: `verb.Lookup`, `verb.PruneAction` from Task 3.
- Produces: `tui` no longer defines `PruneAction`; `tui/app.go` switches on `verb.PruneAction`.

- [ ] **Step 1: Run the existing TUI prune tests to record the baseline**

Run: `go test ./tui/ -run 'Prune' -v`
Expected: PASS — four tests around `tui/cmdline_test.go:173-215`. Note their names; they must still pass verbatim at the end of this task.

- [ ] **Step 2: Export the TUI's action marker**

`tui/cmdline.go:19` reads `type Action interface{ isAction() }`, with **39** implementations at :395-433. Aliasing `Action` to the shared interface changes what every one of them must satisfy, so all 38 that remain in `tui` after `PruneAction` moves are edited, not just the TUI-local ones.

Replace the interface:

```go
// Action is the shared type (cli/verb). The TUI's own screen-state actions --
// Clear, Quit, Help, Refresh, Repo, TrsfDebug, GridDiag, Grid -- stay here and
// satisfy it by embedding verb.ActionMarker, which is why that marker is
// exported: an unexported method declared in cli/verb could not be implemented
// from this package at all.
type Action = verb.Action
```

Delete the 39 `isAction()` methods:

```bash
sed -i '/^func (.*) isAction()  *{}$/d' tui/cmdline.go
grep -c 'isAction()' tui/cmdline.go
```

Expected: `0`.

Then embed the marker in every remaining action struct. They are the 38 types listed by:

```bash
grep -n '^type [A-Za-z]*Action struct' tui/cmdline.go
```

For a struct with fields, add the embed as the first line; for an empty one, inline it:

```go
type ClearAction struct{ verb.ActionMarker }

type SubmitAction struct {
	verb.ActionMarker
	Repo string
	// ... existing fields unchanged
}
```

Run `go build ./tui/` after this step — the compiler names every type that still fails to satisfy `Action`, which is the checklist for this edit.

- [ ] **Step 3: Delete the TUI's prune parser and PruneAction**

Delete `PruneAction` (`tui/cmdline.go:45-50`) and `parsePrune` (:766-777). Change the dispatch at :451 from `return parsePrune(tokens[1:])` to:

```go
	case "prune":
		return parseViaSpec("prune", tokens[1:])
```

Add the shared helper next to `ParseCommand`:

```go
// parseViaSpec routes one verb through the declaration (cli/verb). The TUI
// uses ContinueOnError with a discarded writer because a typo here is a line
// in the results pane, not an exit.
func parseViaSpec(path string, args []string) (Action, error) {
	sp, ok := verb.Lookup(path)
	if !ok {
		return nil, fmt.Errorf("%s: not in the verb table", path)
	}
	fs := sp.NewFlagSet(flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	b, err := sp.Parse(fs, args)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return sp.Build(b)
}
```

- [ ] **Step 4: Point the executor at the shared type**

`tui/app.go:3145`: change `case PruneAction:` to `case verb.PruneAction:`. The body is unchanged — it already reads `v.TaskIDs`, `v.Force`, `v.Before`.

- [ ] **Step 5: Run the TUI tests**

Run: `go test ./tui/ -run 'Prune' -v`
Expected: the same four tests PASS, unedited. They call `ParseCommand("prune ...")` and assert on the returned action's fields; the action is now `verb.PruneAction` with the same field names, so the assertions hold.

Run: `go test ./tui/...`
Expected: PASS.

- [ ] **Step 6: Fix the TUI's arity gap deliberately, and record it**

The legacy `parsePrune` had no arity check, so `prune <id> --force` yielded `TaskIDs = ["<id>", "--force"]` with `Force = false` — caught downstream by `cli/prune.go:85-89`'s hex decode, so a confusing error rather than destruction. The declaration permutes, so this now works.

Add to `tui/cmdline_test.go`:

```go
// TestPruneFlagAfterIDsIsRead pins the behaviour change the declaration makes
// on this surface. The old parsePrune had no arity check and used stdlib
// Parse, so `prune <id> --force` put "--force" into TaskIDs and left Force
// false; cli/prune.go rejected it as a bad task id. Permuting is what the CLI
// always did, and the TUI now matches.
func TestPruneFlagAfterIDsIsRead(t *testing.T) {
	const id = "deadbeefdeadbeefdeadbeefdeadbeef"
	got, err := ParseCommand("prune "+id+" --force", "/cwd")
	if err != nil {
		t.Fatalf("ParseCommand: %v", err)
	}
	act, ok := got.(verb.PruneAction)
	if !ok {
		t.Fatalf("got %T, want verb.PruneAction", got)
	}
	if !act.Force {
		t.Error("--force after the id was dropped")
	}
	if len(act.TaskIDs) != 1 || act.TaskIDs[0] != id {
		t.Errorf("TaskIDs = %q, want [%s]", act.TaskIDs, id)
	}
}
```

Run: `go test ./tui/ -run TestPruneFlagAfterIDsIsRead -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add tui/cmdline.go tui/app.go tui/cmdline_test.go
git commit -m "refactor(tui): parse prune from the declaration

tui's Action becomes an alias of verb.Action, and the TUI-local screen-state
actions satisfy it by embedding verb.ActionMarker rather than an unexported
method -- an unexported marker declared in cli/verb could not be implemented
from package tui at all.

parsePrune and tui's own PruneAction are gone. The four existing prune parse
tests pass unedited, which is the point: the field names and semantics are
identical.

One behaviour DOES change, deliberately and now pinned by a test:
`prune <id> --force` used to put \"--force\" into TaskIDs and leave Force
false, because parsePrune had no arity check and used stdlib Parse. It was
caught downstream by cli/prune.go's hex decode, so a confusing error rather
than a destructive one. Permuting is what the CLI always did."
```

---

### Task 6: wasm bridge and the WebUI

**Files:**
- Modify: `cmd/harness-webui-wasm/main.go` (bridge table :87), `webui/static/main.js` (`case "prune"` :2484-2512)
- Test: `cmd/harness-webui-wasm` compiles under `GOOS=js GOARCH=wasm`; WebUI verified in a browser

**Interfaces:**
- Consumes: `verb.Lookup`, `verb.VerbSpec.Parse`, `verb.Bound` from Tasks 1-3.
- Produces: `window.harness.parseCommand(line, ctx) -> {path, args, flags, set, trail}` and `window.harness.pathsForSurface("webui") -> [string]`.

- [ ] **Step 1: Add the bridge functions**

In `cmd/harness-webui-wasm/main.go`, add to the table at :87:

```go
		"parseCommand":    js.FuncOf(harnessParseCommand),
		"pathsForSurface": js.FuncOf(harnessPathsForSurface),
```

and the implementations:

```go
// harnessParseCommand parses one command line through the shared declaration.
//
// Unlike fileLs and its siblings this needs no client, so it is synchronous:
// no Promise, no goroutine. It returns the neutral Bound rather than a built
// Action -- an Action boundary would need a marshaller per action type, which
// is the duplication this design removes.
//
//	harness.parseCommand(line, {repo, host, agent}) -> {path, args, flags, set, trail}
func harnessParseCommand(this js.Value, args []js.Value) any {
	if len(args) < 1 {
		return js.ValueOf(map[string]any{"error": "parseCommand: missing line"})
	}
	fields := strings.Fields(args[0].String())
	if len(fields) == 0 {
		return js.ValueOf(map[string]any{"error": "parseCommand: empty line"})
	}
	sp, ok := verb.Lookup(fields[0])
	if !ok {
		return js.ValueOf(map[string]any{"error": "unknown command: " + fields[0]})
	}
	if !sp.Surfaces.Has(verb.WebUI) {
		return js.ValueOf(map[string]any{"error": fields[0] + ": not available here"})
	}
	fs := sp.NewFlagSet(flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	b, err := sp.Parse(fs, fields[1:])
	if err != nil {
		return js.ValueOf(map[string]any{"error": err.Error()})
	}
	return js.ValueOf(boundToJS(b))
}

// boundToJS renders a Bound as a plain JS object. Durations cross as strings
// so the JS side never has to know Go's nanosecond representation.
func boundToJS(b verb.Bound) map[string]any {
	path := make([]any, 0, len(b.Path))
	for _, p := range b.Path {
		path = append(path, p)
	}
	as := make([]any, 0, len(b.Args))
	for _, a := range b.Args {
		as = append(as, a)
	}
	flags := map[string]any{}
	for k, v := range b.Flags {
		switch t := v.(type) {
		case time.Duration:
			flags[k] = t.String()
		case uint:
			flags[k] = float64(t)
		case uint64:
			flags[k] = float64(t)
		default:
			flags[k] = t
		}
	}
	set := map[string]any{}
	for k := range b.Set {
		set[k] = true
	}
	return map[string]any{
		"path": path, "args": as, "flags": flags, "set": set, "trail": b.Trail,
	}
}

// harnessPathsForSurface lists the verb paths declared for a surface, so the
// page can assert at startup that its dispatch covers all of them.
//
//	harness.pathsForSurface("webui") -> ["prune", ...]
func harnessPathsForSurface(this js.Value, args []js.Value) any {
	want := verb.WebUI
	if len(args) > 0 {
		switch args[0].String() {
		case "cli":
			want = verb.CLI
		case "tui":
			want = verb.TUI
		}
	}
	out := []any{}
	for _, v := range verb.Verbs {
		if v.Surfaces.Has(want) {
			out = append(out, v.FlagSetName())
		}
	}
	return js.ValueOf(out)
}
```

Add `"flag"`, `"io"`, `"strings"`, `"time"` and the `cli/verb` import if not already present.

- [ ] **Step 2: Verify it compiles for wasm**

Run: `make wasm-check`
Expected: succeeds. This is the target that catches wasm-only breakage; `go build ./...` does not compile this package for `GOOS=js`.

- [ ] **Step 3: Replace the WebUI's hand-written prune loop**

In `webui/static/main.js`, replace `case "prune": { ... }` (:2484-2512) with:

```js
        case "prune": {
          // Parsed by the shared declaration (cli/verb) through the wasm
          // bridge, so this surface cannot drift from the CLI's grammar --
          // the hand-written --before/-f loop that used to live here was a
          // third independent copy of it.
          const b = window.harness.parseCommand(line, {});
          if (b.error) throw new Error(b.error);
          if (b.args.length > 0) {
            out = await window.harness.prune({ taskIds: b.args, force: !!b.flags.force });
          } else {
            out = await window.harness.prune({ before: b.flags.before });
          }
          break;
        }
```

- [ ] **Step 4: Add the startup dispatch assertion**

Near the other startup wiring in `webui/static/main.js`, after `window.harness` is available:

```js
  // Every verb declared for this surface must have a dispatch entry above.
  // Checked from inside the runtime that owns the dispatch: scanning this
  // file from a Go test would be a regex over JavaScript.
  const WEBUI_DISPATCH = new Set(["prune"]);
  for (const p of window.harness.pathsForSurface("webui")) {
    if (!WEBUI_DISPATCH.has(p)) {
      throw new Error(`webui: verb "${p}" is declared for this surface but runCmd has no case for it`);
    }
  }
```

- [ ] **Step 5: Build and verify in a browser**

Run: `make build`
Expected: succeeds, `bin/` refreshed.

Drive the live WebUI with the Playwright MCP tools: navigate to the WebUI URL with the PSK fragment, type each of these into the command input and confirm the output line:

| Typed | Expected |
| --- | --- |
| `prune` | `prune: cutoff = 168h0m0s; ...` — the time-mode line |
| `prune --before 1h` | the same line with a 1h cutoff |
| `prune --before=1h` | identical to the previous row |
| `prune <32-hex> --force` | id-mode line, `force=true` — the form the old loop also accepted |
| `prune --nope` | an error naming the unknown flag, not a silent no-op |

Confirm the browser console shows no startup assertion error. Take a screenshot of the command output and report its path.

- [ ] **Step 6: Commit**

```bash
git add cmd/harness-webui-wasm/main.go webui/static/main.js
git commit -m "feat(webui): parse prune through the shared declaration

parseCommand joins the bridge table beside fileLs and friends, but needs no
client, so it is synchronous. It returns the neutral Bound rather than a built
Action: an Action boundary would need one marshaller per action type, which is
the duplication being removed.

main.js's hand-written --before / -f loop is gone -- it was the third
independent copy of this grammar, after cmd/harness-cli and tui/cmdline.go.

pathsForSurface plus a startup assertion means a verb declared for the WebUI
with no dispatch entry fails loudly at load, rather than reporting 'unknown
command' to whoever types it first."
```

---

### Task 7: The permanent invariants

**Files:**
- Create: `cli/verb/invariant_test.go`
- Test: `cli/verb/invariant_test.go`

**Interfaces:**
- Consumes: `verb.Verbs` from Task 3.
- Produces: the guards Phases 1-6 rely on.

- [ ] **Step 1: Write the tests**

`cli/verb/invariant_test.go`:

```go
package verb

import (
	"flag"
	"io"
	"strings"
	"testing"
)

// TestTrailingImpliesNoPermute is D6 stated as a property of the table rather
// than inferred from a code shape.
//
// cli/flagorder_test.go infers it: it looks for verbs that define flags, read
// positionals, and parse with stdlib flag. That inference has two blind spots
// this test does not -- a positional read behind a helper call (agent send
// reads its payload inside resolvePayload, so the scan sees a verb with no
// positionals), and a FlagSet whose name is built rather than literal
// (`flag.NewFlagSet("session stream "+verb)` is skipped entirely).
func TestTrailingImpliesNoPermute(t *testing.T) {
	for _, v := range Verbs {
		if v.Trailing == nil {
			continue
		}
		if v.Trailing.Reason == "" {
			t.Errorf("%s: Trailing needs a Reason -- it is the record of WHY this verb "+
				"cannot permute, and without it the next reader deletes it", v.FlagSetName())
		}
	}
}

// TestWidensIfUnsetVerbsPermute keeps `board purge --seq N` impossible to
// reintroduce: a flag whose absence WIDENS the operation must never sit on a
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

// TestSurfaceNarrowingHasAReason enforces D19 and D20. A flag or positional
// declared for fewer surfaces than its verb is a real thing -- ls --filtered
// means nothing without a filter pane -- but it is also exactly the shape that
// becomes silent drift when it is only a habit.
func TestSurfaceNarrowingHasAReason(t *testing.T) {
	for _, v := range Verbs {
		for _, f := range v.Flags {
			if f.Surfaces != 0 && f.Surfaces != v.Surfaces && f.SurfaceReason == "" {
				t.Errorf("%s: --%s is declared for a narrower surface set than its verb "+
					"and gives no SurfaceReason", v.FlagSetName(), f.Name)
			}
		}
		for _, a := range v.Args {
			if a.Surfaces != 0 && a.Surfaces != v.Surfaces && a.SurfaceReason == "" {
				t.Errorf("%s: positional <%s> is declared for a narrower surface set than "+
					"its verb and gives no SurfaceReason", v.FlagSetName(), a.Name)
			}
		}
	}
}

// TestNoDuplicateSpellings catches a spelling registered twice within one
// verb, which flag.FlagSet would panic on at first use -- at runtime, in
// whichever surface reached it first.
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

// TestUsageParsesItself is the last piece of the board purge repair: whatever
// Usage() prints, the parser must accept. Checked on the flagless form, which
// is the one an operator copies first.
func TestUsageParsesItself(t *testing.T) {
	for _, v := range Verbs {
		if v.Trailing != nil {
			continue // free-form text has no synthesisable sample
		}
		u := v.Usage()
		if !strings.HasPrefix(u, "usage: "+v.FlagSetName()) {
			t.Errorf("%s: Usage() = %q, does not name the verb", v.FlagSetName(), u)
		}
		fs := v.NewFlagSet(flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		if _, err := v.Parse(fs, nil); err != nil && len(v.Args) == 0 {
			t.Errorf("%s: takes no positionals but the empty form fails: %v", v.FlagSetName(), err)
		}
	}
}
```

- [ ] **Step 2: Run them**

Run: `go test ./cli/verb/ -v`
Expected: every test PASSES, including Tasks 2-4's.

- [ ] **Step 3: Full verification**

Run: `make vet`
Expected: no output.

Run: `make check`
Expected: succeeds.

Run: `make test`
Expected: PASS. Note: `server`'s `TestOpenInteractiveSessionMux` is flaky at roughly one run in four and is pre-existing — re-run before attributing a failure there to this work.

Run: `make wasm-check`
Expected: succeeds.

- [ ] **Step 4: Commit**

```bash
git add cli/verb/invariant_test.go
git commit -m "test(verb): the permanent invariants

Six properties asserted on the table rather than inferred from code shape:
Trailing carries its reason, a WidensIfUnset flag never sits on a verb that
cannot permute, surface narrowing is justified, no spelling is registered
twice, a variadic positional is last, and Usage() names the verb it parses.

The reason these are table properties: cli/flagorder_test.go infers the same
class of thing from source shape and has two blind spots because of it -- a
positional read behind a helper call (agent send reads its payload inside
resolvePayload) and a FlagSet whose name is concatenated rather than literal.
A declaration cannot have either."
```

---

## Phase 0 exit criteria

Phase 0 is deployable when all of these hold:

1. `make vet`, `make check`, `make test`, `make wasm-check` all pass.
2. `prune` parses from `cli/verb` on all three surfaces; no hand-written prune parser remains in `cmd/harness-cli/main.go`, `tui/cmdline.go`, or `webui/static/main.js`.
3. `TestPruneDeclarationMatchesLegacy` passes over the whole corpus — the proof that behaviour is unchanged, with the single deliberate exception pinned by `TestPruneFlagAfterIDsIsRead`.
4. The alias guard has been seen to FAIL when `prune`'s alias is split, and to pass when it is restored.
5. The WebUI has been driven in a real browser through the five command lines in Task 6 Step 5, with no startup assertion error.

At that point the design has been exercised end to end and Phase 1 (`file`) can be planned. **If anything in Phases 1-6 contradicts a decision, stop there** — the spec's halt rule, and the reason this plan covers Phase 0 alone.
