# Authority visualizer — what a task can reach, and what a change would do — design

## Problem

- **P1.** The scope rules have outgrown reading. On 2026-10-22 three fixes
  (`3e5ae4ab`, `a839cacf`, `b78264bb`) changed how a child's scope is bounded
  per bit, and the operator could follow them only as prose: "I roughly
  imagine where it propagates, but I am not confident I am right." There is
  no way to SEE, for a given tree, what a task reaches per bit, or what a
  spawn or a `caps set --cascade` would change.
- **P2.** The operator cannot tell which combinations are tested. The rules
  are pinned by tests scattered over `server/*_test.go`; nothing lists the
  cases, and nothing says a given combination is not among them.
- **P3.** The scope semantics are written twice. The server resolves them in
  `server/scope.go` + `server/capabilities.go` + `server/set_caps_handler.go`;
  the clients restate them in `cli/verb/scope.go` (`scopeForCap`, "mirrors the
  server's Scope.ForCap"), `cli/tasktree.go` (`reachOfDescendant`,
  `BuildTaskGrants`) and `cli/caps_audit.go`. The server's own comment calls a
  second scope implementation "the drift shape this file exists to prevent"
  (`scopeSetWith`). No mismatch has been seen: the 2026-10-22 fixes changed
  how authority is WRITTEN (attenuation, cascade), while the clients only READ
  stored scopes, which they resolve as the server does. The risk is the next
  change to how a scope is read, which would have to be made twice.
- **P4.** A task wider than its parent is legitimate when the operator granted
  it (`caps set` writes without attenuation, `server/set_caps_handler.go`) or
  when the parent was narrowed later without `--cascade`, and a defect only if
  a spawn produced it. Nothing records which: `TaskInfo` does not say who last
  wrote a task's authority.

## Decisions taken

**operator** = the human chose it in conversation (2026-10-22). **this spec** =
the author chose it while writing; those rows are the ones to check.

| # | Decision | Decided by |
| --- | --- | --- |
| V1 | It lives in the repository and is kept, to look at again whenever the scope rules change | operator |
| V2 | Viewable in the WebUI | operator |
| V3 | The operator can try conditions of their own, not only look at prepared cases | operator |
| V4 | Built on the WebUI's existing creator tree (`renderTaskTreeGraph`), keeping its arrows, and adding "pick a task → everything it reaches lights up" | operator |
| V5 | Both: what-if on the LIVE task tree, and a separate practice tree of made-up tasks | operator |
| V6 | Where a task is wider than its parent, the reason is shown — which needs the author of its authority recorded | operator |
| V7 | Order: the shared package (Phase 1) before any picture, because the picture must be drawn by the code the server enforces with | operator (accepted the plan) |
| V8 | Package `authority`, importable by `server`, `cli` and the wasm build (it may import only `runner/protocol` and the standard library) | this spec |
| V9 | The types MOVE: `server.Scope` becomes `authority.Scope` at every use, with no alias left behind | this spec |
| V10 | The case catalog is Go data in a non-test file of `authority`, so the wasm build carries it; one test runs every case and fails on a mismatch | this spec |
| V11 | A what-if never sends anything to the server. It runs the same `authority` functions on the snapshot the page already holds | this spec |
| V12 | CLI and TUI get the reach and what-if as TEXT (`caps reach`, `caps what-if`); the graph and the practice tree are WebUI-only | this spec |
| V13 | The provenance is three values on the task: who wrote its authority last (a kind), the acting task if any, and when | this spec |
| V14 | Reach is per OPERATION, not per literal bit: a task reaches through every bit it exercises (`exercisedBits`, `server/capabilities.go` since `0313d08c`), so an `exec_control` holder is shown its view and cowrite reach under `exec_view`'s and `exec_cowrite`'s own scopes, as the attach gate resolves them | this spec (builder check) |
| V15 | "Wider than parent" is never drawn as a defect. Reparent, swap, a resume by someone other than the creator, and a parent narrowed without cascade all produce it legitimately with no write to the child, so no predicate over the live tree tells a failed attenuation apart; attenuation is pinned by the case catalog instead. The mark is one colour, with the provenance as its explanation | this spec (builder check) |
| V16 | The comparison leaves out the child's own id (a task always acts on itself, whatever its parent may do) and includes visibility: a child that SEES more than its parent is marked too | this spec (builder check) |
| V17 | ~~`caps set-parent` and `--swap` write `AuthoritySource.reparent`~~ **Withdrawn 2026-10-22.** The record has one slot, so a reparent overwrote who had actually granted the task's caps, and a reparent grants nothing. `caps set-parent` / `--swap` now leave the record alone and the value is gone from the enum; the moved parent link is read off the tree itself | operator (2026-10-22: the record is the granter's) |
| V18 | `authority_actor` is zeroed in a `TaskInfo` sent to a caller that cannot see the actor, as every other fact about an unseen task is withheld; `authority_actor_hidden` says so, and the label reads `@hidden`. Operators see every actor | this spec (builder check); wording unified with V21 |
| V21 | Provenance has ONE rendering, `cli.AuthorityLabel` (`<source>@<actor> (<age>)`), on every surface — ls, TUI popup, WebUI row, caps audit, and Phase 3's marks. This replaces the per-surface wording the first draft invented (「見えない task」, Japanese tooltip phrases), which no decision asked for | operator (2026-10-22: 「統一でいいのでは」) |
| V20 | "This scenario is a catalog case" is decided up to RENAMING: same tree shape, same caps and scope on each task, same change, task ids ignored (§ Matching a scenario to a case) | operator |
| V19 | The delegation bound lands after Phase 5, inside `authority` (V7's order). Each feature makes its own `TaskInfo` change | operator (the agreed order), restated after the builder check |

## Phases

Each phase lands on its own; a later one needs the earlier ones.

| Phase | What | Visible change |
| --- | --- | --- |
| 1 | `authority` package; server and clients on it | none expected (P3); a changed arrow is a regression to explain |
| 2 | provenance (V6, V13) | `ls`, task detail, caps audit show who wrote the authority |
| 3 | reach: `authority.Reach`, the tree overlay, `caps reach` | pick a task → its reach lights up; "wider than parent" marks |
| 4 | what-if on the live tree, `caps what-if` | before/after of an edit, a cascade, or a spawn |
| 5 | practice tree + case catalog | a WebUI view to build trees; every catalog case loadable, with its test |

## Phase 1 — the `authority` package

### What moves

From `server/scope.go`: `Scope`, `ScopeOverride`, `ForCap`, `VisRank`,
`String`, `validateScope`, `scopeBaseRank`, `minScopeBase`,
`normalizeScopeIDs`, `idsFromWire` / `idsToWire`, `scopeFromWire`, `toWire`,
`overridesToWire`, `defaultScope`, `taskIDHexLen`.

From `server/capabilities.go`: `descendantsOf`, `actionSet`, `visibleSetOf`,
`grantReach` with `reaches` and `boundRanks`, `exercisedBits`,
`attachModeScopeCap`, `scopeCoversNewChild`, and the pure half of
`attenuateScope` (everything after the creator is resolved).

From `server/set_caps_handler.go`: `clampScopeUnder`, `isNarrowing`,
`reachNarrowed`, `visibleIDs`.

Exported names are the moved names capitalised (`authority.ActionSet`,
`authority.Attenuate`, `authority.ClampUnder`, `authority.IsNarrowing`, …).

### What stays in `server`

- Everything that resolves a CONNECTION to a task (`lookupPrincipal`,
  `callerCaps`, `scopeSet`, `inScope`, `authorize`, `attenuateScope` as a thin
  wrapper that resolves the creator and calls `authority.Attenuate`).
- The WAL projection (`applyToWAL`, `scopeFromWAL`), because `WALEvent` is a
  server type; they become functions over `authority.Scope`.
- `childIndexOf(*TaskStore)` builds the creator index the package takes as a
  plain `map[string][]string`.

### Clients onto it

- `cli/verb/scope.go`'s `scopeForCap` is deleted; `ScopeByCap` converts the
  wire scope with `authority.ScopeFromWire` and calls `ForCap`.
- `cli/tasktree.go`'s `reachOfDescendant` and the per-clause reasoning inside
  `BuildTaskGrants` are rebuilt on `authority.ActionSet` / `VisibleSetOf`
  over a creator index built from the listed `TaskInfo`s. The edge kinds
  (`action` / `override` / `view`) and the global ring keep their meaning;
  the computation underneath changes.
- `cli/caps_audit.go` resolves each holder's scope through `authority`.

The listing a client holds is what its caller may SEE, so a creator index
built from it lacks the tasks the caller cannot see. That is the same limit
the tree has today, and the picture says so where it matters (Phase 3).

### Pinning the single implementation

`authority`'s tests carry the server's current scope and attenuation tests
(moved, not copied). One more test greps `cli/`, `tui/`, `server/` and
`cmd/` for the moved function names declared outside `authority`, so a
second copy fails the build, in the manner of
`TestNoHandWrittenVerbFlagSetsRemain`.

## Phase 2 — provenance

### Values

`AuthoritySource` (wire enum, `u8`):

| value | written when |
| --- | --- |
| `unknown = 0` | a record from before this field (legacy replay) |
| `spawn` | a task created by another task (the creator is the actor) |
| `operator_spawn` | a task created by an operator connection |
| `operator_caps_set` | `caps set` wrote caps, scope (or, with the delegation spec, a bound half) |
| `cascade` | a `caps set --cascade` clamped this task as a descendant (the actor is the cascade's target) |
| `resume` | a resume re-granted caps or scope (the resumer is the actor; zero for an operator) |

A resume that keeps the recorded authority writes nothing: the authority did
not change.

### Where it lives

- `TaskEntry` gains `AuthoritySource`, `AuthorityActor protocol.TaskID`,
  `AuthorityAt time.Time`.
- `WALEvent` / `walEventJSON` gain `authority_source`, `authority_actor`,
  `authority_at`, `omitempty`, written on `task_created` and
  `task_caps_changed`. A legacy record replays as `unknown`.
- `TaskInfo` appends `authority_source :AuthoritySource`, `authority_actor
  :TaskID`, `authority_at :u64` (unix nanos, server clock). The actor is zeroed
  for a caller whose visible set does not contain it (V18).

### Display

`ls` rows carry it as `granter=<AuthorityLabel>` and `ls --json` as its fields
(the key was `auth=` until 2026-10-22, which read as authentication).
The TUI detail popup, the WebUI task row (after `delegate=`, as on an `ls` row;
it was on the task sheet until 2026-10-22, alone among the row's authority
fields) and each caps audit holder line show the same label (V21).

## Phase 3 — reach

### `authority.Reach`

```
Reach(tasks []Task, id string, bit Capability) ReachResult
```

`Task` is the package's own minimal view (id, creator, caps, scope); clients
build it from `TaskInfo`, the server from `TaskEntry`. `ReachResult` lists,
per listed task, `Act` (inside `ActionSet` for the bit, AND the bit is in
`exercisedBits` of the task's caps — V14), `See` (inside `VisibleSetOf`), and
`Global` when the set is every task — including tasks the caller cannot list,
which the picture states as "+ tasks you cannot see".

`WiderThanParent(tasks, id) Wider` returns `Bits` — the exercised bits whose
target set, less the task's own id (V16), is not inside the creator's set for
the bit — and `Sees`, set when the visible set is not inside the creator's.
Empty for an operator-created task.

### WebUI

- The task sheet gains 「到達範囲を見る」. Choosing it puts the tree into reach
  mode for that task: a bit selector (every bit the task exercises, plus 「閲覧」)
  replaces 「権限で強調」 while the mode is on; nodes the task can act on for the
  chosen bit are filled, nodes it can only see are outlined, the rest dim. A
  global reach shows a banner instead of filling every node. Escape or the
  sheet's close leaves the mode.
- Arrows and creator-edge labels stay as they are (V4).
- A node whose `WiderThanParent` is non-empty carries one mark (V15); its
  tooltip names the bits (and `vis` when it sees more) and the task's
  `AuthorityLabel` (V21) — `operator_caps_set@…`, `spawn@…` for a parent
  narrowed after the fact or a task moved under a narrower one, `unknown`.

### CLI / TUI

`caps reach <id> [--cap NAME] [--json]` (verb table, `CmdlineSurfaces: CLI |
TUI | WebUI`): per bit, the tasks acted on, then the tasks only seen; global
as `global`. Plus a `wider than parent: <bits> (<provenance>)` line when non-empty.

## Phase 4 — what-if on the live tree

### `authority.WhatIf`

```
WhatIf(tasks []Task, change Change) []TaskDiff
```

`Change` is one of:

- `Regrant{Target, Caps*, Scope*, Cascade}` — exactly what `caps set` would do,
  through the same `ClampUnder` the server's cascade uses;
- `Spawn{Creator, RequestedCaps, Scope}` — a phantom child, through
  `Attenuate` and the caps intersection (and, once the delegation spec lands,
  its bound);
- `Reparent{Target, NewParent}` — `caps set-parent`, which moves no authority
  but changes every subtree it touches.

`TaskDiff` is, per task whose authority or reach changed: caps before/after,
scope before/after (labels), and per bit the tasks added to and removed from
its action set.

### WebUI

The task sheet gains 「もし…」, opening the existing re-grant dialog in a
what-if mode (same caps chips, scope rows, override rows and id checklists,
plus `--cascade`, plus a 「仮の子を置く」 tab for `Spawn`). Applying it does NOT
send `caps set`: it overlays the tree — changed nodes ringed, a phantom child
drawn dashed — and lists the `TaskDiff`s beside it. A button in the overlay
turns the what-if into the real `caps set` command line (shown, not sent).

### CLI / TUI

`caps what-if <id> [--caps …] [--scope …] [--scope-for …] [--cascade] |
--spawn-under <id> --caps … --scope …` prints the `TaskDiff`s. Nothing is
sent but the listing request.

## Phase 5 — practice tree and case catalog

### Catalog

`authority/cases.go` holds `Cases []Case`:

```
Case{Name, Why, Tree []Task, Change Change, Want []TaskDiff}
```

`authority/cases_test.go` runs `WhatIf` on each and compares with `Want`;
every case is a subtest, `TestCases/<Name>`. The cases that pin the
2026-10-22 fixes are moved here from the server tests, so each fix appears in
the catalog under the name of what it guards.

### WebUI view

A new tab 「権限の練習」 holds a tree of made-up tasks:

- add a task under a task, remove it, set its caps and scope with the same
  controls the re-grant dialog uses;
- the reach mode and the what-if of Phases 3–4, on this tree;
- a list of the catalog's cases; choosing one loads its tree and change and
  shows `Want` beside the computed result, with the test name;
- after any what-if, a line saying whether this scenario is a catalog
  case — "テストあり: TestCases/…" or "テストなし" — and a button that copies
  it as a Go `Case` literal, so a combination the operator found worth
  checking can become a test by pasting.

The practice tree is kept in `localStorage` (per-browser convenience; losing
it loses nothing the repository holds).

### Matching a scenario to a case (V20)

`authority.Canonical(tree, change) string` renders a scenario with every task
id replaced by its position in a canonical order, and `CaseFor(scenario)`
returns the catalog case with the same rendering:

1. Order the tree: roots, then each task's children, depth first. Siblings are
   sorted by a key built WITHOUT ids — the task's caps, its scope with each id
   reduced to a placeholder, and recursively its children's keys — so the same
   tree built in a different order sorts the same way.
2. Number the tasks in that order and rewrite every id (scope ids, override
   ids, vis ids, the change's target / creator / new parent) as its number.
3. Render the numbered tree and change as text; equal text is a match.

Caps are compared exactly: a bit that plays no part in the change still makes
two scenarios different. Two identical siblings referenced differently can
order the same way and number the other way round, so an equivalent scenario
can occasionally read 「テストなし」. A match is never claimed falsely, because
step 3 compares the whole rewritten scenario; `TestCanonicalIgnoresOrderAndIds`
pins both directions over permuted and renamed copies of every catalog case.

## Wire

| Format | Change | Phase |
| --- | --- | --- |
| `AuthoritySource` | new enum (values above) | 2 |
| `TaskInfo` | append `authority_source`, `authority_actor`, `authority_at` | 2 |

No other phase touches the wire. Rollout server first; run
`scripts/wire-skew-check.sh` before landing Phase 2.

## Surfaces

The `surface-parity-checklist` is walked item by item per phase while writing
the plan, and this table is checked back against the code at the end (item 39).

| Surface | Phase | What |
| --- | --- | --- |
| WebUI tree | 1 | arrows and labels computed by `authority` |
| `ls` rows / `--json`, `session ls` | 2 | provenance |
| TUI detail popup, WebUI task row | 2 | provenance, the same label (V21) |
| `caps audit` (CLI, TUI modal, WebUI) | 2 | provenance per holder |
| WebUI tree, task sheet | 3 | reach mode, wider-than-parent marks |
| CLI / TUI / WebUI command line | 3 | `caps reach` |
| WebUI re-grant dialog, task sheet | 4 | what-if mode |
| CLI / TUI / WebUI command line | 4 | `caps what-if` |
| WebUI new tab | 5 | practice tree + catalog |
| TUI graph, TUI practice tree | 3–5 | **omitted**: the TUI has no canvas; its text forms are `caps reach` / `caps what-if` (V12) |
| README, `caps` catalog text | 3–5 | the two verbs; the practice tab |
| Agent skill (`supervising-workers`) | 3 | `caps reach <own id>` as the way an agent checks what a worker it is about to spawn could reach |

## Testing

- Phase 1: the moved tests pass unchanged except for the package; the
  duplicate-implementation grep test; `BuildTaskGrants` output over a fixed
  tree compared with `authority.ActionSet` for every (task, bit, target).
- Phase 2: WAL round trip of the three fields, legacy replay = `unknown`,
  each source written by its path (spawn, operator spawn, `caps set`,
  cascade, resume with re-grant; NOT a resume that keeps).
- Phase 3: `Reach` and `WiderThanParent` on fixed trees, including an operator
  grant (`operator_caps_set`), a parent narrowed without cascade (`spawn`), a
  swap (the granter unchanged), an `exec_control` holder's view reach under `exec_view`'s
  scope, and a child of a `none` parent acting on itself (NOT marked).
- Phase 4: `WhatIf(Regrant…Cascade)` equals what `handleSetCaps` then stores,
  on the same tree (the server test calls both).
- Phase 5: `TestCases`; the copied Go literal of a loaded case round-trips to
  the same `Case`.
- Live, against `scripts/dummy-harness.sh` and a browser (Playwright, per the
  WebUI memory): reach mode on a three-level tree; a what-if cascade whose
  overlay matches the tree after the real `caps set`; a practice case loaded
  from the catalog.

## Non-goals

- **No server-side what-if.** A what-if is computed on the caller's listing
  (V11), so for a confined agent it covers only what it can see; operators see
  everything.
- **No editing from the practice tree into the live server.** The only bridge
  is the shown `caps set` command line.
- **No history view.** Provenance is the LAST write, not a log of all of them.
