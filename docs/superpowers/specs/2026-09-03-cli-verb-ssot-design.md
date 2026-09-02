# One declaration per verb, three surfaces derived from it

Date: 2026-09-03

## Decisions taken

**operator** means the human chose it in conversation, **this spec** means the
author chose it while writing — those are the rows worth a second look.

| # | Decision | Decided by |
| --- | --- | --- |
| D1 | Full migration to a declaration-driven verb layer, staged by verb family — not a conformance test over three hand-written parsers | operator |
| D2 | The declaration lives in a new Go package `cli/verb`, as a table of `VerbSpec` values. No external DSL, no codegen | operator (chose approach A over reflection / `.bgn`) |
| D3 | The shared boundary is **parse only**. Each surface keeps its own execute/dispatch and its own rendering | this spec |
| D4 | Aliases are declared explicitly (`Flag.Aliases []string`) and never inferred from spelling. `-e` is not the short form of `--enter` | operator (caught the `session send` case) |
| D5 | A migration-lifetime test derives the ground-truth alias grouping from the legacy `FlagSet` by `flag.Flag.Value` pointer identity, and fails on any mismatch with the declaration | this spec |
| D6 | `Trailing == nil` implies permuted parsing. The `flagsMustPrecedePositionals` allowlist becomes a per-verb property of the declaration | this spec |
| D7 | Value resolution order (`flag → env → workspace config → surface context`) moves into the declaration as `Flag.Resolve`; each surface injects only the surface-context tier | this spec |
| D8 | The wasm bridge returns the neutral `Bound` value, not a built `Action` — an `Action` boundary would need ~40 hand-written marshallers | this spec |
| D9 | Required capabilities are **not** in the declaration. `server/capabilities.go` owns that mapping and a second copy would be a second source of truth | this spec |
| D10 | Result rendering is **not** in the declaration. Three surfaces render differently on purpose | this spec |
| D11 | `Flag.WidensIfUnset` marks flags whose absence widens the operation, so `board purge --seq`'s failure mode is expressible in the type | this spec |
| D12 | Usage text is generated from the declaration; the per-verb inline usage strings are deleted | this spec |
| D13 | WebUI completeness is enforced by a startup assertion in JS against `verb.PathsForSurface(WebUI)` exposed on the bridge, not by scanning `main.js` from Go | this spec |
| D14 | Where the three surfaces' existing resolution orders disagree, the CLI's order is authoritative and the other surface is fixed | this spec |
| D15 | `board` migrates in Phase 3, not Phase 6, so `WidensIfUnset` is exercised on the verb that motivated it | this spec |
| D16 | Where a verb's path differs across surfaces, the CLI's path becomes canonical and the divergent spelling is removed. No path-alias mechanism is added | this spec |

## Problem

The same verb grammar is hand-written three times, and nothing mechanical
holds the three copies together.

- **Three independent parsers.** `cmd/harness-cli/main.go:141` dispatches with
  `flag.FlagSet` per verb (plus `session.go`, `git.go`, `exec.go`,
  `workspace.go`). `tui/cmdline.go:438 ParseCommand` is a second parser over
  the same verb names — shlex tokenize, its own `FlagSet` per verb, and manual
  scanning where order matters (`parseServer`, cmdline.go:531-534, comments
  that stdlib `flag` stops at the first non-flag token).
  `webui/static/main.js:2310 runCmd` is a third, in JavaScript, with its own
  `--agent` / `--agent=` branches. Its own comments name the duplication:
  *"mirrors the TUI cmdline's --agent"*, *"Mirrors harness-cli caps
  set-parent"*.

- **The one mechanical guard reaches one of the three.**
  `cli/flagorder_test.go` walks the AST for verbs that define flags, read
  positionals, and parse with stdlib `flag` — the shape that silently drops a
  flag written after a positional. Its search path
  (`cli/flagorder_test.go:48`) is `{".", "agent", "../cmd/harness-cli"}`. It
  does not walk `tui/`, and it structurally cannot reach `main.js`.
  `cli.ParsePermuted` has **zero** callers under `tui/`.

- **That defect class has already cost data.** `cli/permute.go:17-22` records
  it: `board purge <topic> --seq N` — the exact line the help text printed —
  left `--seq` at its zero value, which is the whole-topic form, and destroyed
  two messages on a live board. The same shape existed in `board read
  --in-reply-to`, `board retract --seq`, `server dial-runner --via`, and
  `agent read` / `agent retract`'s `--server-cid`. Every one had been
  reviewed; the feature that introduced the third had unit and in-process E2E
  coverage. Neither reached it, because the tests called the client method
  directly.

- **The TUI has the same structural gap, currently without a known widening
  case.** `tui/cmdline.go:766 parsePrune` has no arity check: `prune <id>
  --force` yields `TaskIDs = ["<id>", "--force"]` with `Force = false`. That
  one is caught downstream — `cli/prune.go:85-89` hex-decodes every id and
  rejects `"--force"` before sending — so it is a confusing error, not
  destruction. The verbs that *are* arity-checked (`file pull`'s `len(pargs)
  != 3`, `session await-idle`'s `fs.NArg() > 1`) are protected by that check
  rather than by permuted parsing. The protection is incidental.

- **Help text and parser can disagree, because they are different code.** The
  CLI prints per-verb usage from inline strings at `cmd/harness-cli/main.go`
  lines 468, 492, 518, 540, 559, 567, 575, 592, 616, 633, 664, 711 and 757,
  and a top-level `usage()` at :1090. The `board purge` incident was exactly a
  usage line describing an invocation the parser could not accept.

- **Aliases have two different shapes and no way to tell them apart.**
  `cmd/harness-cli/session.go:584-585` binds one variable twice (`--detach` /
  `-d`) — a real alias. `cmd/harness-cli/git.go:98` does the same across two
  long names (`--cached` / `--staged`). But `session.go:756-757` declares
  `--enter` (append a carriage return) and `-e` (interpret backslash escapes)
  as **two independent flags**. Anything that pairs short with long by
  spelling would merge those two and turn `session send -e '...'` into a
  spurious Enter injected into a live PTY.

- **Drift has already happened where the grammar is genuinely shared.** The
  WebUI spells the task listing `list` (`main.js:2359`); the CLI and TUI spell
  it `ls`. The WebUI also reaches two operations at shorter paths than the
  other surfaces: `set-parent` and `await-idle` at top level, against `caps
  set-parent` (`cmd/harness-cli/main.go:257`, same nesting in
  `tui/cmdline.go`) and `session await-idle` (`tui/cmdline.go:916`).

- **The mirroring is done by hand, and the code says so.**
  `tui/cmdline.go:793-795`: *"The CLI registers -d as a shorthand
  (cmd/harness-cli/session.go); an operator who learned the command there
  types it here and gets 'flag provided but not defined'."* That comment is
  the maintenance cost written down — someone had to notice a flag on one
  surface and replicate it on another, after an operator hit the gap.

- **One surface avoids flags because it has no flag parser.**
  `webui/static/main.js:2664-2667` explains that `session stream approve`
  takes a bare word rather than a flag "as in the TUI's command line and for
  the same reason: this input is whitespace-split with no flag parser, so a
  word cannot be silently dropped the way a [flag] can". The WebUI's grammar
  is shaped by the absence of a parser, not by what reads best.

- **The compensation is a 380-line manual checklist.**
  `.claude/skills/surface-parity-checklist/SKILL.md` requires walking 39
  numbered items with a verdict each, before any operator-visible option
  changes. Its item 2 already names the fix in the abstract — `cli/caps.go`
  (`GrantableCaps` :16, `CapsCatalog` :138) and `cli/scope.go` (`ParseScope`
  :54) are "one grammar, one parser; never a per-surface copy" — and all three
  surfaces do call those. Verb grammar simply never got the same treatment.

### What is NOT the problem

The verb *sets* differ between surfaces, and that is correct, not drift:

- `trsf`, `diag`, `grid`, `repo` are TUI-only because they manipulate TUI
  screen state.
- `conns`, `logs`, `watch`, `board` are CLI verbs whose TUI/WebUI equivalents
  are dedicated UI, not a command line.
- `skill`, `whoami`, and the `agent` family are CLI-only by construction: they
  are called from an agent's Bash tool and read `HARNESS_*` env.

A declaration that forces every verb onto every surface would be wrong. Which
surfaces a verb appears on is therefore part of the declaration (`Surfaces`),
not an invariant over it.

## Scope

IN:

- `cli/verb`, a new parse-only package: `VerbSpec` table, `FlagSet`
  construction, permuted parsing, `Bound` result, `Action` construction.
- Migration of every verb family reachable from more than one surface, plus
  the CLI-only families, staged (see **Migration**).
- Deletion of the parse half of `tui/cmdline.go` and of the hand-written
  argument branches in `runCmd` (`webui/static/main.js`).
- One new wasm bridge function, `parseCommand`, and one bridge accessor,
  `pathsForSurface`.
- Generated usage text replacing the inline per-verb usage strings.
- Tests: differential (migration-lifetime), alias grouping
  (migration-lifetime), `Trailing`/permute invariant (permanent), per-surface
  completeness (permanent), spec examples parse (permanent).
- An edit to `.claude/skills/surface-parity-checklist/SKILL.md` collapsing
  items 1, 2, 3, 7, 8 and 10 into "add a row to `cli/verb`".

OUT, each with its reason:

- **Execute and dispatch.** `cmd/harness-cli/main.go`'s post-parse calls,
  `tui/app.go:2931 runAction`, and the JS dispatch in `runCmd` stay three
  separate tables. The CLI writes stdout and an exit code, the TUI returns
  `tea.Cmd` and opens modals, the WebUI appends to `cmd-output` and touches
  the DOM. Merging them would force one surface's output model onto the other
  two (D3, D10).
- **Required capabilities.** The authority mapping is request-kind →
  capability in `server/capabilities.go` (`callerCaps` :106, `inScope` :270,
  `authorize` :281), not verb → capability. A verb-side copy would drift from
  the enforcement point, which is the one place that actually decides (D9).
- **MCP or any fourth consumer.** This design makes a fourth consumer cheap;
  it does not add one. `docs/superpowers/specs/2026-04-28-agent-comms-design.md:21`
  already records MCP as a v2 item.
- **TUI-only and CLI-only verb *behaviour*.** Those verbs get declarations so
  usage generation and completeness testing are uniform, but nothing about
  them changes.
- **`flag` package replacement.** `flag.FlagSet` remains the parsing engine;
  the declaration builds it. A custom parser would be a second behaviour
  change riding along with the restructure.

## The declaration

```go
package verb

type VerbSpec struct {
    Path     []string   // {"board","purge"}, {"submit"}, {"session","stream","turn"}
    Surfaces Surface    // CLI | TUI | WebUI
    Args     []Arg
    Flags    []Flag
    Trailing *Trailing  // non-nil only for verbs taking free-form trailing text
    Examples []string   // every one must parse and Build; see Testing
    Build    func(Bound) (Action, error)
}

type Arg struct {
    Name     string
    Type     ArgType
    Variadic bool
}

type Flag struct {
    Name          string
    Aliases       []string   // explicit; never inferred from spelling (D4)
    Type          FlagType
    Default       any
    Help          string
    Resolve       Chain      // nil = flag only
    WidensIfUnset bool       // D11
}
```

**Arity** lives in `Args`. The hand-written checks it replaces are
`tui/cmdline.go`'s `len(pargs) != 3` (file push/pull) and `fs.NArg() > 1`
(session await-idle), and their CLI counterparts.

**`Trailing`** is non-nil for exactly the four verbs in
`cli/flagorder_test.go:20-26` — `session send`, `session exec`, `session
stream turn`, `notify` — whose trailing words are literal text, so a
`-`-leading word cannot be distinguished from a flag. Everywhere else
`Trailing` is nil and the parse is permuted (D6). This turns an allowlist the
test carries into a property the verb carries.

**`Aliases`** is explicit. The three shapes in the tree are declared as:

```go
// session send — two independent flags (session.go:756-757)
{Name: "enter", Type: FlagBool, Help: "append a carriage return (Enter) after the text"},
{Name: "e",     Type: FlagBool, Help: `interpret backslash escapes (\n \r \t \e \xHH \\)`},

// session new — short alias (session.go:584-585)
{Name: "detach", Aliases: []string{"d"}, Type: FlagBool},

// git diff — long-to-long alias (git.go:98)
{Name: "staged", Aliases: []string{"cached"}, Type: FlagBool},
```

**`Resolve`** carries the tier order that currently exists three times in
three shapes: `cliopts.ResolveString(*repo, "HARNESS_REPO_PATH")` in the CLI
(`cli/cliopts/cliopts.go:95`), a `FlagSet` default in the TUI
(`tui/cmdline.go:618 parseSubmit` takes `defaultRepo` and passes it to
`fs.String("repo", defaultRepo, "")`), and a dropdown read in the WebUI
(`runnerSelect.value` inside `runCmd`).

```go
Resolve: Chain{Flag, Env("HARNESS_REPO_PATH"), WorkspaceConfig("repo"), SurfaceContext}
```

Each surface injects only `SurfaceContext`: the TUI its `defaultRepo`, the
WebUI its dropdown values, the CLI nothing. The `--config` / `--workspace`
tier described in README becomes available on all three surfaces for the
first time.

**Custom value types** use `FlagCustom{New: func() flag.Value}`, which is how
`scopeForFlag` (`cmd/harness-cli/main.go:35`) and `repeatableStrings` (:1358)
survive unchanged.

**`WidensIfUnset`** marks a flag whose absence makes the operation cover more,
not less — `board purge --seq` being the case that cost two messages. In this
spec it has one consumer: the invariant test asserting such a flag's verb is
permuted. It exists so the property is stated where the flag is declared
rather than in a comment in `cli/permute.go`.

## Derivation per surface

### CLI

```go
spec := verb.Lookup(sub, args)
fs   := spec.NewFlagSet(flag.ExitOnError)
b, err := spec.Parse(fs, args, verb.SurfaceContext{})
act, err := spec.Build(b)
```

`NewFlagSet` takes the `flag.ErrorHandling` mode because the CLI uses
`ExitOnError` and the TUI uses `ContinueOnError` with `io.Discard`
(`tui/cmdline.go:918` and siblings).

`cmd/harness-cli/main.go`'s switch keeps only the `Action → func(ctx,
*cli.Client) error` half. The inline usage strings are deleted and usage is
rendered from the spec (D12) — which is the actual repair for the `board
purge` class, since the printed invocation and the accepted invocation now
come from one source.

### TUI

`tui/cmdline.go:438 ParseCommand` becomes a call into `verb.Parse` with
`SurfaceContext{Repo: defaultRepo}`. The parse half of the file (~1300 lines)
is deleted. `tui/app.go:2941`'s type switch gains a package qualifier and is
otherwise unchanged.

TUI-only actions — `QuitAction`, `ClearAction`, `HelpAction`,
`RefreshAction`, `RepoAction`, `TrsfDebugAction`, `GridDiagAction`,
`GridAction` — stay in `tui` and are declared with `Surfaces: TUI`,
concatenated onto the shared table.

`CapsAction` and `ScopeAction` are **shared**, despite sitting in the
"handled without a client" group at `tui/app.go:2942`. The WebUI has the same
session-default state (`spawnCaps` / `spawnScope`, surface-parity checklist
item 9). Treating them as TUI-only during migration would drop the WebUI's
session defaults.

### WebUI

One entry added to the bridge table at `cmd/harness-webui-wasm/main.go:87`:

```go
"parseCommand":    js.FuncOf(harnessParseCommand),
"pathsForSurface": js.FuncOf(harnessPathsForSurface),
```

Neither needs a client, so neither wraps a Promise; both return plain values,
unlike `harnessFileLs` and its siblings.

`parseCommand` returns `Bound`, not `Action` (D8): `{path: ["file","pull"],
args: {...}, flags: {...}}` serializes with no per-verb code, whereas an
`Action` boundary would need a marshaller for each of the ~40 Action types —
reintroducing the duplication this design removes.

```js
const b = window.harness.parseCommand(line, {
  repo: runnerSelect.value, host: hostSelect.value, agent: agentSelect.value,
});
```

Because the dropdowns are passed as `SurfaceContext`, the `submit` case's
`runnerSelect.value` read and the `--agent` / `--agent=` branches inside
`runCmd` are deleted; JS keeps only the dispatch from `b.path` to
`window.harness.*`. `list` becomes `ls` here.

## Migration

One verb family per phase. A phase is complete when the differential test
passes for that family and the legacy parser for it is deleted.

| Phase | Families | Why this position |
| --- | --- | --- |
| 0 | `cli/verb` skeleton + `prune` | On all three surfaces, smallest, has a real alias (`-f`), no `Trailing`, and is the one family with a live arity gap (`tui/cmdline.go:766`). Exercises the whole mechanism once |
| 1 | `file` (7 sub-verbs) | Dense aliases (`-r`/`-f`/`-p`/`-o`/`-n`) and fixed arity 3 — the workout for `Args` and `Aliases` |
| 2 | `git` | Carries the long-to-long alias `--cached`/`--staged` (git.go:98, tui/cmdline.go:1443) |
| 3 | `forward`, `exec`, `server`, `workspace`, `ssh-gateway`, `board` | Structurally simple. `board` is here rather than last (D15) so `WidensIfUnset` is exercised on `board purge`, the verb that motivated it |
| 4 | `submit`, `interactive`, `session` (every sub-verb except the four `Trailing` ones) | First real use of `Resolve` chains and `FlagCustom` (`scopeForFlag`, `repeatableStrings`), and of `SurfaceContext` injection from the WebUI dropdowns. Settles the `session await-idle` / `await-idle` path (D16) |
| 5 | `session send`, `session exec`, `session stream turn`, `notify` | The four `Trailing` verbs, including the `-e` / `--enter` pair. `session send` and `session exec` are CLI-only, so the blast radius is one surface. Last, so every mechanism is proven before the most dangerous parse is touched |
| 6 | Everything left. CLI-only: `agent`, `logs`, `watch`, `conns`, `ls`, `whoami`, `skill`, `version`, `cancel`, `prune-local`, `notify-watch`. Shared but trivial: `caps`, `caps set`, `caps set-parent`, `scope`, `grid`, `refresh`/`sync`, `help`. Single-surface: `preview` (WebUI), `trsf`, `diag`, `repo`, `clear`, `quit` (TUI) | Little or no cross-surface derivation left; consolidation so usage generation and the completeness tests cover every verb. Settles the `caps set-parent` / `set-parent` and `ls` / `list` paths (D16) |

Phase 0 decides whether the design holds. If it does not, the correct response
is to revise the declaration before Phase 1, not to special-case `prune`.

### Surface matrix

| Family | CLI | TUI | WebUI | Phase |
| --- | --- | --- | --- | --- |
| `prune` | ✓ | ✓ | ✓ | 0 |
| `file` | ✓ | ✓ | ✓ | 1 |
| `git` | ✓ | ✓ | ✓ | 2 |
| `forward` | ✓ | ✓ | ✓ | 3 |
| `exec` (exec_run) | ✓ | ✓ | ✓ | 3 |
| `server dial-runner` | ✓ | ✓ | ✓ | 3 |
| `workspace` | ✓ | ✓ | — | 3 |
| `ssh-gateway` | ✓ | ✓ | — | 3 |
| `board` | ✓ | — | — | 3 |
| `submit` | ✓ | ✓ | ✓ | 4 |
| `interactive` | ✓ | ✓ | — (buttons) | 4 |
| `session new` / `attach` / `ls` / `kill` | ✓ | ✓ | — (buttons) | 4 |
| `session snapshot` / `resize` | ✓ | — | — | 4 |
| `session await-idle` | ✓ | ✓ | ✓ as top-level `await-idle` | 4 |
| `session stream` (attach/approve/interrupt/finish/requests/snapshot) | ✓ | ✓ | ✓ | 4 |
| `session send` / `session exec` | ✓ | — | — | 5 |
| `session stream turn` | ✓ | ✓ | ✓ | 5 |
| `notify` | ✓ | ✓ | — | 5 |
| `caps` / `caps set` / `scope` | ✓ | ✓ | — (chips + `capList`/`setCaps` bridge) | 6 |
| `caps set-parent` | ✓ | ✓ | ✓ as top-level `set-parent` | 6 |
| `grid` | — | ✓ | ✓ | 6 |
| `refresh` / `sync` / `help` | — | ✓ | ✓ | 6 |
| `preview` | — | — | ✓ | 6 |
| `agent`, `logs`, `watch`, `conns`, `ls`, `whoami`, `skill`, `version`, `cancel`, `prune-local`, `notify-watch` | ✓ | — | — | 6 |
| `trsf`, `diag`, `repo`, `clear`, `quit` | — | ✓ | — | 6 (declared in `tui`) |

`—` in this table is a decision, not an omission: the reasons are in
**What is NOT the problem**, except the two "(buttons)" cells and the `caps`
row, where the WebUI reaches the operation through a control rather than the
command input.

Three rows record paths that differ across surfaces today — `ls`/`list`,
`caps set-parent`/`set-parent`, `session await-idle`/`await-idle`. A
`VerbSpec` has one `Path`, so migrating those families settles the spelling:
the CLI's path becomes canonical and the WebUI's shorter forms stop working
(D16). This is a deliberate behaviour change on the WebUI command input,
accepted because a path alias mechanism would preserve exactly the two-
spellings-for-one-verb condition this design exists to remove.

## Testing

**Differential (migration-lifetime).** For each family, feed the same command
lines to the legacy parser and to the spec-driven one and assert the resulting
`Action` values are equal. The legacy parser is deleted only after this
passes. Corpus: `tui/cmdline_test.go` is 1502 lines with 130 `ParseCommand`
call sites, which covers the TUI side directly. `cmd/harness-cli/main_test.go`
is 168 lines, so the CLI side is thin — `Examples` closes that gap.

**Examples (permanent).** Every string in `VerbSpec.Examples` must parse and
`Build` without error. This is not cosmetic: the `board purge` incident was a
usage line the parser rejected, and usage is now generated from the same spec
(D12), so an example that fails is a documented invocation that does not work.

**Alias grouping (migration-lifetime, D5).** Build the legacy `FlagSet`,
`VisitAll` it, and group names by `flag.Flag.Value` pointer identity. That
grouping is ground truth: stdlib's `newBoolValue` is `(*boolValue)(p)`, a
pointer conversion, so two bindings of one variable share a pointer while two
independent `fs.Bool` calls do not. Verified by probe:

```
0x…0c3  [cached staged]   ← long-to-long alias, grouped
0x…0c0  [d detach]        ← short alias, grouped
0x…0c2  [e]               ← separate
0x…0c1  [enter]           ← separate
```

Compare against the declaration's `Aliases` and fail on mismatch. This makes
"assumed `-e` was short for `--enter`" a failing test rather than a review
habit. It dies with the last legacy `FlagSet` in Phase 6.

**Trailing/permute invariant (permanent).** Assert directly on the table:
every spec with `Trailing == nil` parses permuted, and every flag with
`WidensIfUnset` belongs to such a spec. This replaces
`cli/flagorder_test.go`'s AST offender scan, which shrinks to the unmigrated
verbs and is deleted in Phase 6. Unlike the AST scan it covers the TUI and the
WebUI, because it inspects the declaration rather than Go source.

**Completeness (permanent).** Go has no exhaustive switch check, so this
follows the pattern already used at
`server/scope_percap_completeness_test.go:133` and `cli/caps_completeness_test.go`:

- every spec whose `Surfaces` includes CLI has an entry in the CLI dispatch table;
- every spec whose `Surfaces` includes TUI has a case in `tui/app.go`'s type
  switch (AST scan);
- every spec whose `Surfaces` includes WebUI has a JS dispatch entry — checked
  by a startup assertion in `main.js` against `window.harness.pathsForSurface("webui")`,
  which throws on a missing entry (D13). Scanning `main.js` from a Go test
  would be a regex over JavaScript; asserting from inside the runtime that
  owns the dispatch is exact.

## Risks

**The `Resolve` chain is the one place this design can silently freeze an
existing bug.** Nobody has verified that the CLI, TUI and WebUI resolve
`repo` / `host` / `agent` in the same order today; the three implementations
are `cliopts.ResolveString`, a `FlagSet` default, and a dropdown read. A
differential test only proves old and new agree *per surface*, so a
pre-existing cross-surface disagreement would pass through it and become
declared behaviour.

Mitigation, as an explicit task before Phase 4: compare the three orders for
each affected flag and record the result. Where they disagree, the CLI's order
is authoritative and the diverging surface is fixed as part of that phase
(D14).

**Phase 5 touches a live PTY's input path.** `session send` is the only verb
where a parse mistake types characters into a running agent's terminal. It is
last in the order for that reason, and its `Examples` must cover `-e`,
`--enter`, and both together.

## Completion

1. The parse half of `tui/cmdline.go` is gone; Action types and TUI-only verbs remain.
2. `runCmd` in `webui/static/main.js` holds no argument branches — `parseCommand` plus dispatch.
3. `cli/flagorder_test.go`'s offender scan reports zero and is deleted, along with the alias-grouping test.
4. `.claude/skills/surface-parity-checklist/SKILL.md` items 1, 2, 3, 7, 8 and 10 collapse into one instruction: add a row to `cli/verb`. **The checklist getting shorter is the deliverable this work is measured by.**
