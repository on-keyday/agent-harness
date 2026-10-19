# Caps audit → tree Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the caps audit lead into the creator tree: choosing a holder
shows that task in the tree, and the tree marks capabilities itself. The
spec's Amendment 2 is D16–D22.

**Architecture:** Three pieces of `cli` are shared by every surface:
- the label (`AuditLabel`, gaining repo and status);
- a per-task tier mark (`TierMark`);
- the scope-cell text for unboundable bits (`global（固定）`).

The TUI gets three things: a jump from the audit modal into the task list's
tree order, a bit mark (`◆`), and pinned rows past the 100-row cut. The WebUI
moves the audit into the タスク tab under the tree graph, and the graph gains
tier marks, a bit highlight, and a focus pulse.

**Tech Stack:** Go (cli, Bubble Tea TUI, wasm bridge), vanilla JS/CSS,
`node --test`.

**Spec:** `docs/superpowers/specs/2026-10-19-caps-audit-design.md`, section
"Amendment 2 — the audit leads into the tree" (D16–D22). Earlier decisions
D1–D15 and Amendment 1 stand except where D16/D17/D21 supersede them.

## Global Constraints

- **Unboundable-bit text.** A holder whose bit is `ScopeNone` renders exactly
  `global（固定）`, coloured as global, on every surface. `scope_kind: "none"`
  stays in the data (D21).
- **Label.** The order is `<id8> <agent> <repo basename> <Status> <prompt
  head ≤16 runes>`, with empty parts dropped (D19).
- **Tier mark.** It is `<tier label><count>`: `強3` / `中2` / `弱1`. The tier
  is the strongest one present, and the count is how many bits the task holds
  at that tier. A task holding no granular bit has no mark (D18-2).
- **Ended tasks are marked.** Tree marks, both D18 kinds, cover ended tasks
  too (D22).
- Tests run with HARNESS_* unset, e.g. `env -u HARNESS_SERVER_PUBKEY -u
  HARNESS_AUTH_TICKET -u HARNESS_TASK_ID -u HARNESS_SERVER_CID …`.

## Review Focus

1. **A task whose mask has no granular bit** (`caps=none`). It gets no tier
   mark: no `弱0`, no stray space in the ID cell or on the node. *Test: Task 1
   (`TierMark`), Task 2 (ID cell).*
2. **A pinned old holder whose creator chain hits a creator absent from the
   listing** (pruned). The chain stops there, the row shows as an orphan `†`,
   and the walk neither loops nor panics. *Test: Task 2.*
3. **`t` on a bit nobody holds** (the `e` toggle is on). The header still
   reads `ID (◆ notify)`, no row is marked, and the cursor stays put. *Test:
   Task 2.*
4. **A Windows repo path** (`C:\workspace\x` and `C:/workspace/x/`). The label
   shows `x`. *Test: Task 1.*
5. **The WebUI chip click while the tree is off and the status filter hides
   ended tasks.** The graph ignores the list filter, so the node exists and is
   brought into view. *Checked live in Task 4.*

---

### Task 1: cli — label, tier mark, unboundable-bit text

**Files:**
- Modify: `cli/caps_audit.go` (`AuditLabel`), `cli/caps_audit_render.go`
  (`auditScopeText`)
- Create: `cli/caps_tier.go` (`TierMark`)
- Test: `cli/caps_audit_test.go`, `cli/caps_audit_render_test.go`,
  `cli/caps_tier_test.go`

**Interfaces:**
- Produces:
  - `func TierMark(caps protocol.Capability) (mark string, tier verb.CapTier, ok bool)`.
    `mark` is e.g. `"強3"`. `ok` is false when no `verb.AuditOrder()` bit is
    set.
  - `const UnboundedScopeLabel = "global（固定）"` in `cli`. Every renderer
    uses it.
  - `AuditLabel(t)` with the D19 order.

- [ ] **Step 1: Failing tests**
  - `TestAuditLabelCarriesRepoAndStatus`: a task with agent `claude`, repo
    `/home/u/remote-agent-harness`, status Detached and an empty prompt gives
    `"ab000000 claude remote-agent-harness Detached"`.
  - `TestAuditLabelRepoBasename`: the paths `C:\workspace\x`, `C:/workspace/x/`
    and `/a/x` each give the part `x`.
  - `TestAuditLabelPromptHead16`: a 40-rune prompt is cut to 16 runes, and the
    whitespace is collapsed first, as now.
  - `TestAuditLabelRedactedParent`: a task with only an id, an empty agent,
    repo and prompt, and status Running gives `"ab000000 Running"`. Update the
    existing test that expected the bare id.
  - `TestTierMark`:
    - `Spawn|Purge|ExecView` → `"強2", TierStrong, true`
    - `ExecView` → `"弱1", TierWeak, true`
    - `None` → `"", _, false`
  - `TestWriteCapsAuditUnboundedLabel`: the CLI line for a `spawn` holder
    contains `[global（固定）]` and not `scope 対象外`. Change the existing
    assertions in `TestWriteCapsAuditLine` to the new text.
- [ ] **Step 2: Run** `go test ./cli/ -run 'AuditLabel|TierMark|CapsAudit' -count=1`
  → FAIL.
- [ ] **Step 3: Implement.**
  - **Repo basename.** Split on both `/` and `\`, after trimming trailing
    separators. The status is `t.Status.String()`.
  - **`TierMark`.** Walk `verb.AuditOrder()`. It is strongest-first, so the
    first set bit gives the tier, and the count is the set bits sharing that
    tier.
  - **Rename the label everywhere.** Grep `scope 対象外` across `cli/`, `tui/`
    and `webui/` and change every hit to `UnboundedScopeLabel`; in JS, the
    literal `global（固定）`. Tasks 2 and 3 own the TUI and WebUI hits, but the
    grep runs here so none is missed. Leave a hit in a test that asserts the
    old text is gone.
- [ ] **Step 4: Run** the same command → PASS. Then `go test ./cli/... ./tui/
  -count=1` → PASS. The TUI's `TestCapsAuditModalScopeNone` asserts the old
  text: update it to `global（固定）`.
- [ ] **Step 5: Commit** `cli: audit label carries repo and status; TierMark; global（固定）`.

### Task 2: TUI — jump, bit mark, tier mark, pinned rows

**Files:**
- Modify: `tui/tasks.go` (tree columns, ID cell, marks, `SelectID`),
  `tui/app.go` (`refreshTasksTable` pins), `tui/overlays.go`
  (`inCapsAuditModal`: `enter` on a holder, `t`), `tui/capsauditmodal.go`
  (`SelectedCap`, footer, `UnboundedScopeLabel` in `auditScopeCell`),
  `tui/actions.go` (`onTree` clears marks and pins)
- Test: `tui/capsauditmodal_test.go`, `tui/tasks_test.go`

**Interfaces:**
- Consumes: `cli.TierMark`, `cli.UnboundedScopeLabel` (Task 1).
- Produces:
  - `func (m *TasksModel) SelectID(id string) bool`: moves the cursor to that
    row, or returns false.
  - `func (m *TasksModel) SetMark(c protocol.Capability)`: 0 clears it.
  - `func (m *CapsAuditModal) SelectedCap() (protocol.Capability, bool)`: the
    cap of the line under the cursor, for a cap line or a holder line alike.
  - `App.auditPins map[string]bool`: the pinned task ids, each including its
    creator chain.
  - `func (a *App) jumpToTask(id string)`.
  - `func (a *App) markBit(c protocol.Capability)`.

- [ ] **Step 1: Failing tests** in `tui/capsauditmodal_test.go`. Use the
  `setAuditTasks` fixture there.
  - `TestCapsAuditEnterJumpsToTree`: `i`, `enter` (spawn ▾), `down`, `enter`.
    Then:
    - the modal is closed;
    - the authority picker is NOT open;
    - `a.tasks.TreeMode()`;
    - `a.tasks.SelectedID()` is the holder's id;
    - `a.focus == focusTasks`.
  - `TestCapsAuditJumpReachesAnOldTask`:
    - an old task (`CreatedAt: 1`) holding Notify, created by another old task
      (`CreatedAt: 0`), plus 120 newer ended tasks;
    - after jumping to the old one, `SelectedID()` is it;
    - its row's ID cell begins with the tree gutter, not `†`: it sits under its
      creator.
  - `TestCapsAuditJumpChainStopsAtAMissingCreator` (Review Focus 2): the old
    holder's creator id is absent from `tasksByID`. The jump lands, and the
    row shows `†`.
  - `TestCapsAuditTMarksHolders`: `i`, cursor on the spawn line, then `t`.
    Then:
    - tree mode is on;
    - the column header contains `◆ spawn`;
    - exactly the spawn holders' ID cells contain `◆`.
    - Then `T`: no cell contains `◆`, and no pinned row remains.
  - `TestCapsAuditTOnAnUnheldBit` (Review Focus 3): toggle `e`, put the cursor
    on `notify`, press `t`. The header says `◆ notify`, no cell has `◆`, and
    nothing panics.
  - `TestCapsAuditTMarksAnOldHolder`: an old Spawn holder past the 100 rows is
    in the table with `◆` after `t`.
- [ ] **Step 2: Failing tests** in `tui/tasks_test.go`.
  - `TestTreeIDCellCarriesTierMark`: in tree mode, a Spawn|Purge task's ID
    cell ends with `強2`; in flat mode it does not.
  - `TestTreeIDCellNoMarkForNoCaps` (Review Focus 1): a `caps=none` task's ID
    cell is the gutter plus the id8 with no trailing space.
  - The existing `TestViewFitsTerminalWidth` must still pass at 80 columns.
- [ ] **Step 3: Run** `go test ./tui/ -run 'CapsAudit|TreeIDCell|ViewFits' -count=1`
  → FAIL.
- [ ] **Step 4: Implement.**
  - **Tree columns.** `treeCols()` sets the ID to `ID (by creator)` with width
    28. Repo becomes 18, and `repoCellWidth(true)` returns 18.
  - **The ID cell in tree mode.** It is `gutter + id8`, then `" ◆"` when the
    task holds the marked bit, then `" " + mark` when `TierMark` is ok. While a
    mark is set, the ID column's title reads `ID (◆ <cap>)`.
  - **Jump.** `enter` on a holder (`Enter()` returns an id) calls
    `jumpToTask(id)`:
    1. set the pins to the id plus its creator chain;
    2. turn tree order on if it is off;
    3. call `refreshTasksTable`;
    4. call `SelectID`;
    5. call `setFocus(focusTasks)`.
  - **Mark.** `t` takes `SelectedCap()` and calls `markBit(c)`:
    1. set the pins to every task in `tasksByID` holding `c`, plus their
       chains;
    2. call `SetMark(c)`;
    3. turn tree order on;
    4. close the modal.
  - **Creator chain.** Follow `CreatorTaskId` through `tasksByID` until the
    root, an absent id, or an id already seen.
  - **`refreshTasksTable`.** After the 100-row cut, append every pinned task
    not already present.
  - **`onTree` (`T`).** Clear the mark and the pins.
  - **Modal footer.** `enter: expand / show in tree · t: mark bit in tree · a:
    ended tasks · e: unheld bits · Esc: close`.
- [ ] **Step 5: Run** the Step 3 command → PASS. Then `make test` → PASS.
- [ ] **Step 6: Commit** `tui: caps audit jumps into the tree; ◆ bit mark, tier mark, pinned old rows`.

### Task 3: WebUI — audit under the tree, tier marks, bit highlight, focus

**Files:**
- Modify:
  - `cmd/harness-webui-wasm/main.go`:
    - the snapshot task row gains `tierMark` and `tierClass`;
    - new pure bridge `capsAuditOrder`;
  - `webui/index.html`:
    - drop the 権限 tab button and its section;
    - add the `<details id="caps-audit">` section under `#task-tree-graph`;
    - add `<select id="task-tree-bit">` beside `#task-chip-tree`;
  - `webui/static/main.js`:
    - the audit block keyed off the details' `toggle`;
    - the node mark, ring and dim, and pulse in `renderTaskTreeGraph`;
    - the chip handler;
  - `webui/static/style.css`: drop the `audit` row from the tab-hide list,
    and add the node mark, ring, dim and pulse classes;
  - `webui/static/harness_env.mjs`: add `capsAuditOrder` to the pure set.
- Test: `webui/static/cmd_test.mjs`.

**Interfaces:**
- Consumes: `cli.TierMark`, `cli.UnboundedScopeLabel` (Task 1).
- Produces:
  - `harness.capsAuditOrder()` → `[{name, bit, tier}]` in `verb.AuditOrder()`
    order, where `tier` is `"強"|"中"|"弱"`.
  - Snapshot task fields `tierMark` (`""` when none) and `tierClass`
    (`"strong"|"mid"|"weak"|""`).
  - `renderTaskTreeGraph` gains a final argument `opts =
    {markBit, focusId}`. `markBit` is a number, or 0 for none.

- [ ] **Step 1: Failing test** in `cmd_test.mjs`:
  `capsAuditOrder lists every bit strongest first`.
  - The result's length equals `page.bridge.capList().length`.
  - The first entry is `{name: "spawn", tier: "強"}`.
  - The tiers never go weaker-then-stronger along the list.
- [ ] **Step 2: Run** `make js-test` → FAIL.
- [ ] **Step 3: Implement.**
  - **The bridge and the snapshot fields** are exactly as in Interfaces.
  - **`#task-tree-bit`.** The first option is 「権限で強調: なし」 with value
    0, then one option per `capsAuditOrder()` entry as `<name> (<tier>)`.
    `change` re-renders the graph with `markBit`.
  - **The audit section.**
    - It is `<details id="caps-audit"><summary>権限の棚卸し</summary>…`, with
      the old section's toggles, refresh button, error line and rows.
    - `toggle` to open calls `refreshCapsAudit`.
    - The snapshot hook refreshes only when `capsAuditDetails.open`; it no
      longer checks `activeTab === "audit"`.
  - **Chip click** (replaces `openRegrantDialog`):
    1. If the tree is off, click the tree chip.
    2. Set the select to the chip's row bit.
    3. Re-render with `{markBit, focusId: id}`.
    4. `scrollIntoView({block: "center", inline: "center"})` on the node.

    The node gets class `tt-focus` for about 2 s.
  - **Node rendering.**
    - Add `<text class="tt-tier tt-tier-<tierClass>">` beside the id label
      when `t.tierMark` is non-empty.
    - When `markBit` is set: add class `tt-holds` to nodes whose `capsBits &
      markBit`, and `tt-dim` to the others.
  - **Chip text.** A `scope_kind === "none"` chip reads `global（固定）` with
    class `sc global`.
  - **CSS.**
    - Tier colours are the badge colours already used: strong `#f2a3a3`, mid
      `#e5c07b`, weak `#9fcf8f`.
    - `tt-holds`: a ring stroke `#ffd88a`.
    - `tt-dim`: `opacity:.35`.
    - `tt-focus`: a 2 s pulse animation on the ring.
- [ ] **Step 4: Run** `make check && make wasm-check && make js-test && make
  test` → PASS.
- [ ] **Step 5: Commit** `webui: caps audit under the task tree; tier marks, bit highlight, holder → tree`.

### Task 4: Docs, walk, live check

**Files:**
- Modify:
  - `README.md`: the `caps audit` paragraph, the TUI key sentence, and the
    WebUI tab list (remove 権限, mention the section under the tree);
  - `runner/agentskills/supervising-workers/SKILL.md` and both mirrors, only
    if a sentence there names 「scope 対象外」 or the tab (grep first);
  - `.claude/skills/surface-parity-checklist/firing-log.md`;
  - the spec, with an "Amendment 2 — what shipped" paragraph.

- [ ] **Step 1: Edit the docs.** Run `go test ./runner/agentskills/ -count=1`
  → PASS.
- [ ] **Step 2: Walk surface-parity items 1–39.** Item 39 is Amendment 2's
  "How each surface does it" list, row by row.
- [ ] **Step 3: Live check on a dummy harness.**
  - **Setup.** An old-`all`-like task, a supervisor with `spawn` and a child,
    an ended task holding `spawn`, and a `caps=none` task.
  - **CLI.** Labels show the repo and status; `[global（固定）]`.
  - **TUI.**
    - `i` → enter on a holder → tree order, cursor on it.
    - `t` on spawn → `◆` on every holder, the ended one included.
    - `T` clears.
    - `強N` marks are present.
  - **WebUI** (Playwright, 1280 and 390):
    - the section opens under the tree;
    - a chip click turns the tree on, selects the bit and brings the node
      into view (Review Focus 5: set the list filter to Active first);
    - a node click opens the sheet, and its 🔑 opens re-grant;
    - tier marks are drawn;
    - at 390px, `scrollWidth == clientWidth`.

    Keep the screenshots.
  - Bring the dummy down.
- [ ] **Step 4: `make test`, `make check`, `make wasm-check`, `make vet`** →
  PASS.
- [ ] **Step 5: Commit** `docs: caps audit leads into the tree`.
