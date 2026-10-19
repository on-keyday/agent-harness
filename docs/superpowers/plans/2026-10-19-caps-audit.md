# Capability audit — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A per-capability "who holds this bit" view — CLI `caps audit`, a TUI modal on `i`, and a WebUI 「権限」 tab — so a leftover strong grant is seen before someone points it out.

**Architecture:** One tier/scope-kind/implication table in `cli/verb/caps.go`, one row builder `cli.CapsAudit` over the existing task listing, and three thin renderers. No wire change. The WebUI reaches the builder through a wasm bridge, so no surface re-derives tiers, ordering or scope colour.

**Tech Stack:** Go, Bubble Tea (TUI), Go→wasm bridge + `webui/static/main.js`.

**Spec:** `docs/superpowers/specs/2026-10-19-caps-audit-design.md` — read § Problem and § Decisions taken (D1–D15) first.

## Global Constraints

- Tier table, verbatim (D3). 強: `spawn`, `resume`, `exec_control`, `purge`, `prune`, `board_send`, `runner_admin`, `exec_tap`, `forward_tap`, `forward_local`, `forward_remote`. 中: `exec_cowrite`, `exec_run`, `file_write`, `cancel`, `board_peer`, `board_observe`, `resume_exact`. 弱: `exec_view`, `exec_resize`, `file_read`, `notify`.
- `ScopeNone` bits (D12): `spawn`, `notify`, `runner_admin`, `board_observe`, `board_send`. Every other bit is `ScopeOwn`.
- Implied-by notes (D13): `exec_view` ← `exec_cowrite`, `exec_control`; `exec_cowrite` ← `exec_control`; `resume_exact` ← `resume`.
- A row lists only tasks whose mask holds the literal bit (D13).
- Breadth colours (D4): red = global, yellow = subtree / descendants, green = none / ids. `ScopeNone` is a neutral grey 「scope 対象外」. An override-derived scope carries `*`.
- Holder label (D14): `<id8> <agent> <prompt head>`. The prompt head is the first 24 runes, and the empty parts are dropped.
- Default: live tasks only, empty bits folded (D6, D7).
- No wire change, and no new server request (D9).
- Work in this worktree on `harness/70fbad4a6eb6f1e992be8a669f1bcefd`. Run tests with every `HARNESS_*` env var unset. Never run a bare `go build ./cmd/<x>/`.
- Every commit ends with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

1. **A task holding the old `all` (`0xfffff`)** appears in every row except `resume` and `resume_exact`, and the counts include it. Test in Task 2.
2. **The redacted parent row an agent's listing contains** (empty agent and prompt, real caps) produces a label of just the id, with no stray spaces and no panic. Test in Task 2.
3. **An empty listing, or one where no task holds any bit**, prints/renders an explicit "no task holds a capability" line, never a blank screen. Test in Task 2 (builder returns no rows) and Task 3 (text).
4. **A `ScopeNone` bit with an override naming it** (an operator wrote `--scope-for board_observe=ids:x`) still renders 「scope 対象外」, never a coloured breadth. Test in Task 2.
5. **The WebUI tab at 390px** has no horizontal page overflow: holder chips wrap. Checked live in Task 6 (`scrollWidth <= clientWidth`).

---

### Task 1: The table — tiers, scope kinds, implications

**Files:**
- Modify: `cli/verb/caps.go`
- Test: `cli/verb/caps_audit_table_test.go` (create); `server/caps_audit_agreement_test.go` (create)

**Interfaces — Produces (in `cli/verb`):**
- `type CapTier int` with `TierStrong`, `TierMid`, `TierWeak`; `func (CapTier) Label() string` → `"強"` / `"中"` / `"弱"`
- `type CapScopeKind int` with `ScopeOwn`, `ScopeNone`
- `func CapTierOf(c protocol.Capability) CapTier`
- `func CapScopeKindOf(c protocol.Capability) CapScopeKind`
- `func CapImpliedBy(c protocol.Capability) []protocol.Capability`
- `func AuditOrder() []protocol.Capability` — every granular bit (not `none`/`all`), grouped strong → mid → weak, and within a tier in `GrantableCaps` order

- [ ] **Step 1: Failing tests.**
  - `TestEveryGrantableCapHasATier`: for each granular bit in `GrantableCaps`, the table has an explicit entry. The test reads a `capAuditTable` map directly, so it cannot be satisfied by a default value.
  - `TestAuditOrderIsStrongFirstAndComplete`: `len(AuditOrder()) == 22`; no tier index ever decreases along the list; `AuditOrder()[0] == Capability_Spawn`.
  - `TestTierTableMatchesTheSpec`: the 強/中/弱 sets above, verbatim.
  - In `server`, `TestAuditScopeKindAgreesWithTargetClasses`: for each bit in `capTargetClasses`, `capNoTargetResolution` ⇔ `verb.CapScopeKindOf(bit) == verb.ScopeNone`.
  - In `server`, `TestAuditImpliedByAgreesWithGates`: for each `(bit, implier)` in `CapImpliedBy`, `hasAnyCap(implier, mask)` holds. `mask` is `attachModeCap(View)` for `exec_view`, `attachModeCap(Cowrite)` for `exec_cowrite`, and `resumeRequestCap(nonZeroID, true)` for `resume_exact`.
- [ ] **Step 2: Run** `go test ./cli/verb/ ./server/ -run 'Tier|AuditOrder|AuditScopeKind|AuditImpliedBy' -count=1` → FAIL (undefined).
- [ ] **Step 3: Implement.** One unexported `capAuditTable map[protocol.Capability]struct{ tier CapTier; scope CapScopeKind; impliedBy []protocol.Capability }` in `cli/verb/caps.go`, with the exported functions reading it.
- [ ] **Step 4: Run** the same command → PASS; then `go test ./cli/... ./server/ -count=1` → PASS.
- [ ] **Step 5: Commit** — `cli/verb: capability tiers, scope kinds and implications for the audit`

### Task 2: The row builder

**Files:**
- Create: `cli/caps_audit.go`
- Test: `cli/caps_audit_test.go`

**Interfaces:**
- Consumes: Task 1.
- Produces (in `cli`), exactly as spec § The audit rows: `type ScopeBreadth int` (`BreadthGlobal`, `BreadthSubtree`, `BreadthNarrow`); `type AuditHolder struct{ TaskID, Label, Parent string; Live bool; ScopeKind verb.CapScopeKind; Scope string; Breadth ScopeBreadth; Override bool; TaskScope string }`; `type AuditRow struct{ Cap protocol.Capability; Tier verb.CapTier; ImpliedBy []protocol.Capability; Holders []AuditHolder }`; `func CapsAudit(tasks []protocol.TaskInfo, includeEnded, includeEmpty bool) []AuditRow`; `func AuditLabel(t protocol.TaskInfo) string`; `func ScopeBreadthOf(scope string) ScopeBreadth`.

- [ ] **Step 1: Failing tests**, using `protocol.TaskInfo` fixtures:
  - `TestCapsAuditListsHoldersPerBit`: three tasks. The `spawn` row has exactly the two that hold it, and rows follow `AuditOrder`.
  - `TestCapsAuditLiveOnlyByDefault`: a `Succeeded` holder is absent unless `includeEnded`.
  - `TestCapsAuditFoldsEmptyBits`: a bit nobody holds is absent unless `includeEmpty`.
  - `TestCapsAuditOldAll` (Review Focus 1): a task holding `0xfffff` appears in 20 rows and not in `resume` / `resume_exact`.
  - `TestCapsAuditOverrideScope`: a task with base `subtree` and an override `exec_control=ids:X` shows `Scope` `ids:X…` with `Override == true` and `Breadth == BreadthNarrow` on `exec_control`, and `subtree` / `BreadthSubtree` on `exec_cowrite`.
  - `TestCapsAuditScopeNoneIgnoresOverride` (Review Focus 4): `board_observe` with an override naming it → `ScopeKind == ScopeNone`, `Scope == ""`.
  - `TestScopeBreadthOfActionHalf`: `global` → Global, `global/subtree` → Subtree, `subtree-self` → Subtree, `none/global` → Global (the action half is `global`), `ids:a,b` → Narrow, `none` → Narrow.
  - `TestCapsAuditHolderOrder`: global holders before subtree before narrow, then by task id.
  - `TestCapsAuditImpliedNote`: an `exec_control`-only task is absent from the `exec_view` row, and that row's `ImpliedBy` contains `exec_control`.
  - `TestAuditLabelRedactedParent` (Review Focus 2): empty agent and prompt → `Label == id8`.
  - `TestCapsAuditEmpty` (Review Focus 3): no tasks → `len(rows) == 0`.
- [ ] **Step 2: Run** `go test ./cli/ -run 'CapsAudit|ScopeBreadth|AuditLabel' -count=1` → FAIL.
- [ ] **Step 3: Implement.**
  - Live = status not `Succeeded` / `Failed` / `Cancelled`.
  - Scope comes from `ResolvedScopeByCap(t.Capabilities, t.Scope, t.Overrides)[c.String()]`.
  - `Override` = some `t.Overrides[i].Caps & c != 0`.
  - `TaskScope` = `ScopeLabel(t.Scope)` (or the label `ls` prints for the base).
  - `Parent` = 8-hex of `CreatorTaskId`, or `operator` when it is zero.
- [ ] **Step 4: Run** → PASS.
- [ ] **Step 5: Commit** — `cli: CapsAudit builds the per-capability holder rows`

### Task 3: CLI `caps audit`

**Files:**
- Modify: `cli/verb/table.go` (new row after `caps set`), `cmd/harness-cli/dispatch.go`
- Create: `cli/caps_audit_render.go`
- Regenerate: `cli/verb/actions_gen.go` (`go generate ./cli/verb/`)
- Test: `cli/caps_audit_render_test.go`, `cli/verb/rules_test.go` (accept rows)

**Interfaces:**
- Consumes: Task 2.
- Produces:
  - `verb.CapsAuditAction{ All, Empty, JSON bool; Cap string }` (generated from the row: `Path: {"caps","audit"}`, `CmdlineSurfaces: CLI | TUI | WebUI`, `Action: "CapsAuditAction"`, flags `--all`, `--empty`, `--cap NAME`, `--json`).
  - `func WriteCapsAudit(w io.Writer, rows []AuditRow, focus protocol.Capability, tasks []protocol.TaskInfo) error` — text form.
  - `func WriteCapsAuditJSON(w io.Writer, rows []AuditRow) error`.

- [ ] **Step 1: Failing tests**:
  - `TestWriteCapsAuditLine`: the row text `spawn  強  2  3f2a9c… <label>[subtree] 70fbad… <label>[global/global*]`. Assert the tier label, the count, `[scope]` per holder, `*` on an override, and `[scope 対象外]` for a `ScopeNone` holder.
  - `TestWriteCapsAuditFocus`: with focus `spawn`, one line per holder followed by `  revoke: harness-cli caps set <full id> --caps <CapsLabel(caps &^ spawn)>`.
  - `TestWriteCapsAuditEmpty` (Review Focus 3): no rows → `no task holds a capability (use --all to include ended tasks)`.
  - `TestWriteCapsAuditImpliedNote`: the `exec_view` line ends with `(also: exec_cowrite, exec_control)`.
  - Rules: `caps audit --cap spawn` parses; `--cap bogus` is refused naming the valid names.
- [ ] **Step 2: Run** `go test ./cli/ ./cli/verb/ -run 'CapsAudit|DeclaredRules' -count=1` → FAIL.
- [ ] **Step 3: Implement** the row, `go generate ./cli/verb/`, and `func (h cliVerbs) CapsAudit(a verb.CapsAuditAction) error` in `cmd/harness-cli/dispatch.go`, written like `Caps` / `CapsSet` there. It does `c.Snapshot`, then `CapsAudit(body.Tasks, a.All, a.Empty || a.Cap != "")`, then the writer.
- [ ] **Step 4: Run** `make check && go test ./cli/... ./cmd/... -count=1` → PASS. `TestEverySurfaceReadsEveryActionField` will name the TUI until Task 4: note it, leave it.
- [ ] **Step 5: Commit** — `cli: caps audit`

### Task 4: TUI modal on `i`

**Files:**
- Create: `tui/capsauditmodal.go`, `tui/capsauditmodal_test.go`
- Modify: `tui/keys.go` (`CapsAudit: "i"` + binding row), `tui/actions.go` (`onCapsAudit`), `tui/app.go` (field, construction, SetSize, Update routing, View placement, beside `idleWatchersModal`), `tui/dispatch.go` (`func (h tuiVerbs) CapsAudit(v verb.CapsAuditAction) tea.Cmd` opens the modal with the action's toggles), `tui/keys_test.go` (drop `TestIIsFreeInTheMainView`; `i` is now this)

**Interfaces:**
- Consumes: Task 2 (`CapsAudit`, `AuditRow`).
- Produces: `type CapsAuditModal`, following `IdleWatchersModal`'s shape: `Open(tasks []protocol.TaskInfo, includeEnded, includeEmpty bool)`, `IsOpen`, `Close`, `SetSize`, `Update`, `View`, and `SelectedHolder() (taskID string, ok bool)`.

- [ ] **Step 1: Failing tests**:
  - `TestIOpensCapsAudit`: pressing `i` on the main view → the modal is open.
  - `TestCapsAuditModalExpandsBit`: `enter` on the `spawn` line → its holders appear below it.
  - `TestCapsAuditModalHolderOpensRegrant`: `enter` on a holder → the modal closes, and `authorityPicker` is open in re-grant mode for that task id.
  - `TestCapsAuditModalToggles`: `a` adds ended tasks, `e` adds empty bits.
  - `TestCapsAuditModalScopeNone`: a `board_observe` holder renders `scope 対象外`.
- [ ] **Step 2: Run** `go test ./tui/ -run 'CapsAudit|IOpens' -count=1` → FAIL.
- [ ] **Step 3: Implement.**
  - Rows come from `cli.CapsAudit(a.tasks.Rows(), …)`.
  - Breadth uses the existing styles: global → `ErrStyle`, subtree → `WarnStyle`, narrow → `OkStyle`, `ScopeNone` → the dim style. Use whatever names `tui/styles.go` actually has.
  - The re-grant hand-off calls `a.authorityPicker.OpenRegrant(task, a.tasks.Rows())`, as `onReGrant` does.
  - `keys.go` gets a binding with Short `"i caps audit"` and Long `"who holds which capability (enter expands / opens re-grant, a ended, e empty bits)"`.
- [ ] **Step 4: Run** `make test` (all of it, including the consumer and key tables) → PASS.
- [ ] **Step 5: Commit** — `tui: caps audit modal on i`

### Task 5: WebUI 「権限」 tab and command input

**Files:**
- Modify: `cmd/harness-webui-wasm/main.go` (bridges `capsAudit`, `capsAuditText`), `webui/index.html` (tab button `data-tab="audit"` 「権限」 + `<section id="caps-audit" data-tabgroup="audit">`), `webui/static/main.js` (render + toggles + holder click), `cli/verb/table.go` (`WebUIDispatch{Fn: "capsAuditText"}` on the row)
- Test: `webui/static/cmd_test.mjs`

**Interfaces:**
- Consumes: Task 2, Task 3's text writer.
- Produces:
  - `harness.capsAudit({includeEnded, includeEmpty})` → Promise of rows. Each row is `{cap, tier, impliedBy[], holders[{taskId, label, parent, live, scopeKind: "own"|"none", scope, breadth: "global"|"subtree"|"narrow"|"", override}]}`. The bridge fetches `Snapshot` itself (D15).
  - `harness.capsAuditText(bound)` → Promise of the CLI text.

- [ ] **Step 1: Failing test** in `cmd_test.mjs`: `caps audit` and `caps audit --cap spawn` dispatch to `capsAuditText` with those flags.
- [ ] **Step 2: Run** `make js-test` → FAIL.
- [ ] **Step 3: Implement.**
  - The layout follows the prototype (`.superpowers/brainstorm/…/audit-proto-b-v3.html`): rows, tier badge, count, holder chips.
  - Chip classes are `sc global|subtree|narrow|none`. The colours come from the prototype, with grey for none. The `*` mark goes on overrides.
  - A row click expands it; a chip click finds the task in `lastTasks` by id and calls `openRegrantDialog(t)`.
  - The toggles are 「終了した task も」 and 「持ち主 0 の bit も」.
  - The data reloads when the tab is shown, and on each snapshot while it is visible.
  - Holder chips use `flex-wrap`.
- [ ] **Step 4: Run** `make check && make wasm-check && make test` → PASS.
- [ ] **Step 5: Commit** — `webui: 権限 tab and caps audit in the command input`

### Task 6: Docs, live verification, wrap-up

**Files:**
- Modify: `README.md` (Capabilities section: a `caps audit` paragraph; TUI keys sentence: `i` caps audit), `runner/agentskills/supervising-workers/SKILL.md` + both mirrors (one line: `harness-cli caps audit` shows what you handed out), `.claude/skills/surface-parity-checklist/firing-log.md`, the spec (Amendment)

- [ ] **Step 1: Edit the docs**, then run `go test ./runner/agentskills/ -count=1` → PASS.
- [ ] **Step 2: Walk `surface-parity-checklist` items 1–39.** Item 39 checks the spec's § Surfaces table row by row.
- [ ] **Step 3: Live, on the dummy harness.**
  - Setup: four tasks — one with `spawn`, one with the old-`all`-like mask, one with `--scope-for exec_control=ids:X`, one with `board_observe` and scope `none`.
  - CLI: `caps audit` and `caps audit --cap spawn`; read the output.
  - TUI: `i` → the modal matches the CLI rows → `enter` on a holder → the re-grant picker opens → revoke `spawn` → `i` again shows it gone.
  - WebUI (Playwright): the 「権限」 tab matches; a chip opens the re-grant dialog. At 390px, `document.documentElement.scrollWidth <= clientWidth` (Review Focus 5). Keep the screenshot.
  - Bring the dummy down.
- [ ] **Step 4: `make test`, `make check`, `make wasm-check`, `make vet`** → PASS.
- [ ] **Step 5: Amendment + firing-log entry; commit** — `docs: caps audit — README, skill, what shipped`
