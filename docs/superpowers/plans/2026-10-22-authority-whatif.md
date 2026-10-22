# Authority what-if (visualizer Phase 4) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Before a `caps set` (with or without `--cascade`), a spawn or a reparent is sent, show what it would change: each task's authority before/after and the tasks each one gains or loses per bit — on the WebUI tree and as `caps what-if` on every command line — computed by the code the server enforces with.

**Architecture:** The cascade's per-descendant clamp moves out of `server/set_caps_handler.go` into `authority.CascadeClamp`, which the server and the what-if both call, so the two cannot disagree. `authority.WhatIf(tasks, change)` applies a `Regrant`, `Spawn` or `Reparent` to the listing and diffs before/after grants and per-bit reach (`actionReach`, Phase 3). `cli` renders the diffs and the real command line; the WebUI asks the bridge over the cached listing and overlays the tree.

**Tech Stack:** Go 1.26 (`authority`, `server`, `cli`, `cli/verb` codegen), wasm bridge, `webui/static/main.js`, `make` targets.

**Spec:** `docs/superpowers/specs/2026-10-22-authority-visualizer-design.md` — § Phase 4 — what-if on the live tree, V7, V11, V12, and the Phase 3 "As implemented" rules it reuses.

## Global Constraints

- A what-if never sends anything to the server; it runs on the listing the caller holds (V11). The WebUI's 「もし…」 never sends `caps set`; it SHOWS the real command line (spec § Phase 4 WebUI).
- `Regrant` is exactly what `caps set` would do: target written without attenuation, `--cascade` clamps every descendant through `authority.CascadeClamp` — to the target's caps and scope, and to its bound only when the call writes a bound half (delegation spec D19).
- `Spawn` is exactly what a spawn by the creator would be given: `Granter.ChildCaps` / `ChildScope` / `ChildBound`; a refusal is reported with the server's text (`OutOfReachError` / `OutOfDelegationError` / `InvalidScopeError`).
- Reach diffs use Phase 3's rules: per exercised bit through `actionReach` (exec_resize inside exec_view), bits naming no task left out of the display (`cli.ReachBits`), visibility as the `view` entry.
- Over a partial listing (an agent caller) the result is stated as computed over that listing, as Phase 3 does.
- Nothing touches the wire.
- Work in `/home/kforfk/workspace/remote-agent-harness/.harness-worktrees/70fbad4a6eb6f1e992be8a669f1bcefd`; test env scrub `env -u HARNESS_SERVER_PUBKEY -u HARNESS_SERVER_CID -u HARNESS_AUTH_TICKET -u HARNESS_TASK_ID`.

## Decisions this plan adds (spec silent)

- **The bound is part of `Regrant` and `Spawn`** (`--delegate-caps` / `--delegate-scope`), since `caps set` and spawns carry it now.
- **`caps what-if <task-id> --spawn …` instead of `--spawn-under <id>`**: the positional is the target for a re-grant and the creator for a spawn, so the verb keeps one required positional.
- **`Reparent` is offered on the command line (`--parent <id>` / `--parent-none`) but not in a WebUI dialog**: the spec lists it as a `Change` and names no WebUI control for it.
- **Unchanged tasks are not listed**; a what-if that changes nothing prints `nothing changes`.

## Review Focus

1. **A cascade what-if on a supervisor recorded with the default bound** (D19): a caps-only `--cascade` leaves the workers' bounds and does not zero their caps; a bound `--cascade` clamps them. Pinned in Task 1 and Task 2's agreement test.
2. **A spawn what-if whose scope names an id outside the creator's reach or bound** shows the server's refusal text, not an empty diff. Pinned in Task 1.
3. **A spawn adds the phantom to every ancestor whose subtree now holds it** (their `cancel` gains it), not only to the creator. Pinned in Task 1.
4. **A reparent into the target's own subtree** is refused as a cycle, as `caps set-parent` refuses it. Pinned in Task 1.
5. **The WebUI overlay survives the snapshot poll and is cleared by Escape / ✕**, as reach mode is. Pinned in Task 4's Playwright check.

---

## File Structure

| File | Change |
| --- | --- |
| `authority/whatif.go` (new) + `authority/whatif_test.go` | `CascadeClamp`, `Regrant`, `Spawn`, `Reparent`, `TaskDiff`, `WhatIf` |
| `server/set_caps_handler.go`, `server/whatif_agreement_test.go` (new) | the cascade calls `authority.CascadeClamp`; WhatIf vs handleSetCaps |
| `cli/whatif.go` (new) + `cli/whatif_test.go` | `WhatIfReport`, `BuildWhatIf`, `WriteWhatIf`, `WhatIfCommand` |
| `cli/verb/table.go` (+ regenerated `actions_gen.go`), `cmd/harness-cli/dispatch.go`, `tui/dispatch.go` | `caps what-if` |
| `cmd/harness-webui-wasm/main.go`, `webui/index.html`, `webui/static/main.js`, `webui/static/style.css` | bridges `capsWhatIfText`, `whatIf`; dialog what-if mode, 「仮の子を置く」, overlay, diff panel |
| README, spec, firing-log | docs |

---

### Task 1: `CascadeClamp` and `WhatIf` in `authority`

**Files:** Create `authority/whatif.go`, `authority/whatif_test.go`; Modify `server/set_caps_handler.go`

**Interfaces:**
- Consumes: `Task`, `actionReach`, `newView` (Phase 3), `Granter`, `ClampUnder`, `NewReach`, `Validate`, `DescendantsOf`.
- Produces:
  - `func CascadeClamp(target Task, d Grant, dHex string, boundWritten bool, children map[string][]string) Grant` — the body of today's cascade loop: with `boundWritten`, `NewGranter(target.ID, target's Grant, children).ClampDescendant(d, dHex)`; else caps `d.Caps & target.Caps`, scope `ClampUnder(d.Scope, caps, NewReach(target.ID, target.Scope, children))`, bound kept. The server's loop calls it.
  - `type Regrant struct { Target string; Caps *protocol.Capability; Scope *Scope; DelegateCaps *protocol.Capability; DelegateScope *Scope; Cascade bool }`
  - `type Spawn struct { Creator string; Caps protocol.Capability; Scope Scope; DelegateCaps protocol.Capability; DelegateScope Scope }` — `Creator ""` is an operator.
  - `type Reparent struct { Target, NewParent string }` — `NewParent ""` is the root.
  - `type Change interface{ isChange() }` implemented by the three.
  - `const PhantomID = "new"` — the id a `Spawn` gives its child in the diff.
  - `type TaskDiff struct { ID string; New bool; Before, After Grant; Added, Removed map[protocol.Capability][]string }` — key `Capability_None` is visibility; ids sorted; only tasks whose grant or reach changed.
  - `func WhatIf(tasks []Task, c Change) ([]TaskDiff, error)` — diffs sorted by id (`PhantomID` last); errors: unknown target/creator `no such task <id8>`, invalid scope (`Validate`), a spawn refusal (the `Granter` error, unwrapped), a reparent cycle `would make <id8> its own ancestor`.

- [ ] **Step 1: Write the tests** in `authority/whatif_test.go` on the Phase 3 tree (O global root, P under O, C under P, X root) plus G under C:
  - `TestCascadeClampMatchesTheOldLoop`: both branches against hand-computed grants (caps-only: `d.Caps & target.Caps`, ClampUnder scope, bound kept; bound-written: `ClampDescendant`).
  - `TestWhatIfRegrantCascade` (Review Focus 1): `Regrant{P, Caps: file_read, Cascade}` → C and G lose `cancel` (diff `Before.Caps`/`After.Caps`, `Removed[cancel]`), their bounds unchanged; with `DelegateCaps: none` too → bounds clamped; without `Cascade` → only P.
  - `TestWhatIfSpawnRefused` (Review Focus 2): `Spawn{P, Scope: ids:X}` → error text `scope id <X> is outside your own scope`; a bound refusal → `… outside what you may delegate …`.
  - `TestWhatIfSpawnAncestorsGainThePhantom` (Review Focus 3): `Spawn{C, Caps: cancel}` with C's bound `cancel` → P's and C's `Added[cancel]` contain `PhantomID` (their subtrees now hold it); O, already global, has no diff; the phantom's diff has `New` and `After.Caps == cancel`.
  - `TestWhatIfReparentCycle` (Review Focus 4): `Reparent{P, G}` → error; `Reparent{G, ""}` → P and C lose G in `Removed[cancel]`.
  - `TestWhatIfNoChange`: `Regrant{P}` with nothing set → no diffs.
- [ ] **Step 2: Run** `go test ./authority/ -run 'TestCascadeClamp|TestWhatIf'` — Expected: FAIL to compile.
- [ ] **Step 3: Implement** `authority/whatif.go`; replace the server loop's two branches with `authority.CascadeClamp(…)` (the `after` entry as a `Task`).
- [ ] **Step 4: Run** `go test ./authority/ ./server/ -count=1` — Expected: PASS (the server's existing cascade tests are the refactor's proof).
- [ ] **Step 5: Commit** `feat(authority): what-if — a re-grant, spawn or reparent diffed before it is sent; the cascade's clamp shared with the server`.

---

### Task 2: the server agreement test

**Files:** Create `server/whatif_agreement_test.go`

- [ ] **Step 1: Write `TestWhatIfAgreesWithSetCaps`**: over `scopeFixture` plus a bound on the target, for each of {caps-only `--cascade`, scope `--cascade`, bound `--cascade`, no cascade}: compute `authority.WhatIf` over the store's tasks (as `authority.Task`s) BEFORE, then send the real `SetCapsRequest` through `handleSetCaps`, then assert every diff's `After` equals the store's `Grant()` for that id and that no task outside the diffs changed.
- [ ] **Step 2: Run** `go test ./server/ -run TestWhatIfAgreesWithSetCaps -count=1` — Expected: PASS (both sides call `CascadeClamp`; the test pins the target write and the cascade's reach set). If it fails, the disagreement is a Task 1 defect — fix there, not here.
- [ ] **Step 3: Commit** `test(server): a cascade what-if equals what caps set then stores`.

---

### Task 3: `caps what-if` on the CLI and TUI

**Files:** Create `cli/whatif.go`, `cli/whatif_test.go`; Modify `cli/verb/table.go` (regenerate), `cmd/harness-cli/dispatch.go`, `tui/dispatch.go`

**Interfaces:**
- Produces:
  - `func BuildWhatIf(tasks []protocol.TaskInfo, c authority.Change) (WhatIfReport, error)` and `type WhatIfReport struct { Diffs []authority.TaskDiff; Command string }`.
  - `func WhatIfCommand(c authority.Change) string` — the real `harness-cli caps set …` / `submit …` / `caps set-parent …` line, flags through `cli.CapsLabel` / `cli.ScopeLabel` / `OverridesLabel` (the grammar the CLI parses back).
  - `func WriteWhatIf(w io.Writer, r WhatIfReport, asJSON bool) error` — text: per diff a head line `  <id8|new>  caps A → B  scope A → B  delegate A → B` (each part only when it changed), then one line per changed bit `      <bit>  +N: ids  -M: ids` (bits naming no task left out, `view` for visibility), then `  command: <line>`; `nothing changes` when there are no diffs. JSON: one object `{"diffs":[…],"command":…}`.
  - Verb `caps what-if <task-id> [--caps] [--scope] [--scope-for…] [--delegate-caps] [--delegate-scope] [--cascade] [--spawn] [--parent ID | --parent-none] [--json]`: `CmdlineSurfaces CLI|TUI|WebUI`, `WebUIDispatch{Fn: "capsWhatIfText"}`, `ModalSurfaces [{WebUI, "webui/index.html#task-tree-whatif"}]`; rules: `--spawn` excludes `--cascade` and the parent flags; parent flags exclude every authority flag; `AtLeastOne` of the authority flags, `--spawn` or a parent flag.
- [ ] **Step 1: Write the tests**: `TestWhatIfReportRegrantCascade` (text lines for Task 1's cascade case, the command `harness-cli caps set <P> --caps file_read --cascade`), `TestWhatIfReportSpawnRefused` (error text reaches the report), `TestWhatIfReportNothingChanges`, `TestWhatIfCommandRoundTrips` (the command for a regrant with scope, `--scope-for` and a bound parses back through the `caps set` verb to the same values), `TestWhatIfVerbRules` (the exclusions).
- [ ] **Step 2: Run** `go test ./cli/... -run 'TestWhatIf'` — Expected: FAIL to compile.
- [ ] **Step 3: Implement**; `go generate ./cli/verb/`; CLI handler over a fresh snapshot; TUI handler over `a.auditTasks()` into `cmdresult`. Over a partial listing (agent caller, `cli.EffectiveClientKind`) the report's first line says `(over your listing only)`.
- [ ] **Step 4: Run** `go test ./cli/... ./tui/ -count=1` — Expected: PASS (verb invariants, consumers guard).
- [ ] **Step 5: Commit** `feat(cli): caps what-if — the diff of a re-grant, spawn or reparent before it is sent`.

---

### Task 4: the WebUI what-if

**Files:** Modify `cmd/harness-webui-wasm/main.go`, `webui/index.html`, `webui/static/main.js`, `webui/static/style.css`; Test `webui/static/cmd_test.mjs`, Playwright

**Interfaces:**
- Produces:
  - Bridge `harness.capsWhatIfText({taskId, …flags, json}) -> Promise<string>` (fresh listing, as `capsReachText`).
  - Bridge `harness.whatIf({kind: "regrant"|"spawn", taskId, caps, scope, scopeFor, delegateCaps, delegateScope, cascade}) -> {error, diffs: [{id, isNew, head, bits: [{name, added, removed}]}], command, tree}` over `lastListing`; `head` is the text head line from `cli`; `tree` is the layout of the listing WITH the phantom (`cli.BuildTaskTree` / `TaskTreeLayout` over the listing plus a phantom `TaskInfo` under the creator) when the change is a spawn.
  - Re-grant dialog: a 「もし（送らない）」 button beside 適用 that calls `whatIf` with the dialog's request and closes the dialog into the overlay; the task sheet's 「🔮 もし…」 opens the same dialog with a 「仮の子を置く」 toggle that turns its controls into the phantom child's requested caps / scope / bound.
  - Overlay `whatIfState = {result}` or `null`: changed nodes ringed (`tt-whatif`), the phantom drawn dashed (`tt-phantom`) from `result.tree`; a panel `#task-tree-whatif` lists each diff's `head` and bit lines and the `command` with a 📋 copy button (never sent); an `error` shows in the panel instead. Escape (same guard as reach mode), the panel's ✕, closing the tree, or entering reach mode clears it; it survives the poll.
- [ ] **Step 1: Write the test** in `cmd_test.mjs`: `caps what-if <ID> --caps cancel --cascade` dispatches `capsWhatIfText` with those flags.
- [ ] **Step 2: Run** `make js-test` — Expected: FAIL (no dispatch).
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make js-test` — Expected: PASS. Then, on a dummy with O → P → G and root X, Playwright at 1280 / 390: P's 「もし…」 with `cancel` removed and `--cascade` rings P and G, the panel lists both and the command `harness-cli caps set <P> --caps spawn --cascade`, and nothing was sent (`ls` unchanged); a spawn what-if under G draws the phantom and rings P and G (Review Focus 3); a refused spawn shows the server's text; the overlay survives a poll and Escape clears it (Review Focus 5). Screenshots under `.superpowers/`.
- [ ] **Step 5: Commit** `feat(webui): what-if on the creator tree — re-grant, cascade and a phantom child, with the real command shown`.

---

### Task 5: docs, parity walk, land

- [ ] README (Capabilities and scope: `caps what-if`; the WebUI 「もし…」), supervising-workers skill + mirrors (`caps what-if <own-id> --spawn --caps … --scope …` before a real spawn), spec "As implemented" for the four decisions above; read each in full, Edit only.
- [ ] Walk `surface-parity-checklist` 1–39 and log it.
- [ ] `make test` (env scrub) — Expected: PASS.
- [ ] Commit; ask the operator before landing (no wire change, no server restart: the server only moved code into `authority`).
