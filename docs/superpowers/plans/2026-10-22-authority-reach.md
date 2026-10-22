# Authority reach (visualizer Phase 3) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Pick a task and see what it reaches per bit, what it may hand down, and whether it is wider than its parent — as a mode of the WebUI creator tree and as `caps reach` on every command line.

**Architecture:** Three pure functions in package `authority` over a minimal task view (`ReachOf`, `DelegableOf`, `WiderThanParent`), so the picture is computed by the code the server enforces with. `cli` converts a listing into that view and renders one text/JSON report used by the CLI, the TUI cmdline and the WebUI command input. The WebUI tree draws reach from a bridge call over the listing the wasm already holds, and draws "wider than parent" from a per-node field of the snapshot.

**Tech Stack:** Go 1.26 (`authority`, `cli`, `cli/verb` codegen via `go generate ./cli/verb/`), the wasm bridge, `webui/static/main.js`, `make` targets.

**Spec:** `docs/superpowers/specs/2026-10-22-authority-visualizer-design.md` — § Phase 3 — reach, V4, V12, V14, V15, V16, V21; Surfaces rows for Phase 3.

## Global Constraints

- Reach is per OPERATION: a task acts through every bit in `ExercisedBits(caps)` (V14); a bit it neither holds nor exercises reaches nothing.
- "Wider than parent" is never drawn as a defect: one mark, one colour, its explanation is the task's `AuthorityLabel` (V15). The comparison leaves out the task's own id and includes visibility (V16). Empty for an operator-created task.
- One rendering for provenance: `cli.AuthorityLabel`, shown under the key `granter=` (V21, as amended 2026-10-22).
- The listing a client holds is what its caller may see; a global set is shown as `global`, never as "every listed task", and text says `(+ tasks you cannot list)` (spec § Phase 3).
- Arrows and creator-edge labels of the tree stay as they are (V4).
- Nothing in this phase touches the wire.
- Work in `/home/kforfk/workspace/remote-agent-harness/.harness-worktrees/70fbad4a6eb6f1e992be8a669f1bcefd`. Run `make test` / `go test ./cli/...` with `env -u HARNESS_SERVER_PUBKEY -u HARNESS_SERVER_CID -u HARNESS_AUTH_TICKET -u HARNESS_TASK_ID`.

## Decisions this plan adds (spec silent or overtaken)

- **`ReachOf`, not `Reach`.** The spec's `Reach(tasks, id, bit)` collides with `authority.Reach`, the type Phase 1 shipped (`NewReach`, `Reaches`).
- **The delegation bound is shown too, as one more entry of the bit selector: `delegate`.** The bound landed before this phase (V19's order was reversed), and "what may this task hand down" is the reach question the bound raises. `DelegableOf` answers it through the same `Granter` ceiling the server enforces with: filled = ids it may hand down as action ids, outlined = view-only ids. Its caps half (`delegate_caps`) is printed beside it.
- **`view` is the selector entry for visibility** (the spec's 「閲覧」), spelled as the `caps reach` text spells it, so the page and the command line use one word.
- **Text ids are 8-hex short ids** (as `ls` prints `by=`); `--json` carries full ids.
- **Reach mode reads the wasm's own copy of the last listing** (`lastListing`, set where the snapshot builds `taskTree`), so a bridge call needs no request and no JS copy of the rule.

## Review Focus

1. **A confined caller (agent) runs `caps reach` on itself with a global scope** — it gets `global (+ tasks you cannot list)`, not the handful of ids it happens to see. Pinned in Task 2.
2. **An `exec_control` holder** is shown its `exec_view` / `exec_cowrite` reach under those bits' own scopes, and a bit it does not exercise is not offered. Pinned in Task 1.
3. **A task whose creator is not in the listing** (pruned, or invisible to the caller) is not marked wider — there is nothing to compare against. Pinned in Task 1.
4. **The snapshot poll re-renders the tree every few seconds while reach mode is on**: the mode, the selected task and the selected bit survive the redraw. Pinned in Task 3's Playwright check.
5. **A global root (operator-spawned, `--scope global`)** in reach mode shows the banner instead of filling every node. Pinned in Task 3.

---

## File Structure

| File | Change |
| --- | --- |
| `authority/view.go` (new) + `authority/view_test.go` | `Task`, `ReachResult`, `ReachOf`, `DelegableOf`, `Wider`, `WiderThanParent` |
| `cli/reach.go` (new) + `cli/reach_test.go` | `AuthorityTasks`, `ReachReport`, `BuildReachReport`, `WriteReachReport`, `WiderLine` |
| `cli/verb/table.go`, regenerated `cli/verb/actions_gen.go` | `caps reach` row |
| `cmd/harness-cli/dispatch.go`, `tui/dispatch.go` | `CapsReach` |
| `cmd/harness-webui-wasm/main.go` | `lastListing`; bridge `capsReachText`, `reachOf`; snapshot node `wider` |
| `webui/index.html`, `webui/static/main.js`, `webui/static/style.css` | reach mode, `tt-wider` mark |
| `README.md`, `runner/agentskills/supervising-workers/SKILL.md` + mirrors, `.claude/skills/surface-parity-checklist/firing-log.md`, the spec | docs |

---

### Task 1: the reach functions in `authority`

**Files:** Create `authority/view.go`, `authority/view_test.go`

**Interfaces:**
- Consumes: `ActionSet`, `VisibleSet`, `ExercisedBits`, `NewGranter`, `Granter` (existing).
- Produces:
  - `type Task struct { ID, Creator string; Caps, DelegateCaps protocol.Capability; Scope, DelegateScope Scope }` — `Creator` is `""` for an operator-created task.
  - `type ReachResult struct { ActAll, SeeAll bool; Act, See []string }` — `Act` / `See` are listed ids, sorted; `See` excludes ids in `Act`; both empty when the matching `…All` is set.
  - `func ReachOf(tasks []Task, id string, bit protocol.Capability) ReachResult` — `bit == Capability_None` is visibility only (`Act` empty); a bit not in `ExercisedBits(task.Caps)` gives an empty `Act`; an unknown `id` gives the zero value.
  - `func DelegableOf(tasks []Task, id string) ReachResult` — `Act` = ids the task may hand down as action ids, `See` = as view-only ids, through `NewGranter(id, …).ceil` (`Reaches(id, ceilingBit)` / `Reaches(id, Capability_None)`); `ActAll` / `SeeAll` when the ceiling's ranks are global.
  - `type Wider struct { Bits protocol.Capability; Sees bool }` with `func (w Wider) Empty() bool`.
  - `func WiderThanParent(tasks []Task, id string) Wider` — per exercised bit of the task: its action set minus its own id is not inside the creator's action set for that bit (a creator not exercising the bit has an empty set); `Sees` when its visible set minus its own id is not inside the creator's visible set. A global set is inside only a global one. Zero when `Creator == ""` or the creator is not in `tasks`.

All three build the creator index from `tasks` once (`children[Creator] = append(…, ID)`).

- [ ] **Step 1: Write the tests** in `authority/view_test.go` over one fixed tree (O operator root `global` caps all; P under O `subtree` caps `cancel,file_read`; C under P; X a second root):
  - `TestReachOfPerBit`: `ReachOf(P, cancel)` → `Act` = `[C, P]` sorted, `See` = `[]`; `ReachOf(P, exec_view)` (not held) → empty `Act`; `ReachOf(O, cancel)` → `ActAll`.
  - `TestReachOfView`: `ReachOf(P, Capability_None)` → `Act` empty, `See` = `[C, P]`; a P with `vis-ids:X` → `See` includes X.
  - `TestReachOfExercisedBits` (Review Focus 2): P holding `exec_control` with `--scope-for exec_view=ids:X` (base `none`) → `ReachOf(P, exec_view)` `Act` = `[P, X]`; `ReachOf(P, exec_control)` `Act` = `[P]`.
  - `TestDelegableOfDefaultAndDescendants`: P's bound zero → `Act` = `See` = `[C, P]`; bound `descendants` → `Act` = `[C]`, `See` = `[C, P]`; bound `global` → `ActAll`, `SeeAll`.
  - `TestWiderThanParentCases` (the spec's § Testing list): C granted `global` by an operator under a `subtree` P → `Bits` has C's bits; P narrowed to `none` after C was spawned `subtree` → C wider; a swap (C becomes P's parent) → P measured against C; C under a `none` P acting only on itself → `Empty()` (V16); C seeing `global` under a `subtree/subtree` P → `Sees`.
  - `TestWiderThanParentNeedsAListedCreator` (Review Focus 3): C whose `Creator` is not in `tasks` → `Empty()`; O (`Creator == ""`) → `Empty()`.
- [ ] **Step 2: Run** `go test ./authority/ -run 'TestReachOf|TestDelegableOf|TestWiderThanParent'` — Expected: FAIL to compile.
- [ ] **Step 3: Implement** `authority/view.go` per the Interfaces.
- [ ] **Step 4: Run** `go test ./authority/ -count=1` — Expected: PASS, `TestNoSecondScopeImplementation` included.
- [ ] **Step 5: Commit** `feat(authority): what a task reaches, may hand down, and whether it is wider than its parent`.

---

### Task 2: `caps reach` on the CLI and the TUI

**Files:** Create `cli/reach.go`, `cli/reach_test.go`; Modify `cli/verb/table.go` (regenerate `actions_gen.go`), `cmd/harness-cli/dispatch.go`, `tui/dispatch.go`

**Interfaces:**
- Consumes: Task 1.
- Produces:
  - `func AuthorityTasks(tasks []protocol.TaskInfo) []authority.Task` — `Creator` `""` for a zero creator; scopes via `authority.ScopeFromWire` (the bound with no overrides).
  - `type ReachReport struct { TaskID string; Granter string; Bits []ReachLine; View, Delegate ReachLine; DelegateCaps protocol.Capability; Wider authority.Wider }` and `type ReachLine struct { Cap string; authority.ReachResult }`.
  - `func BuildReachReport(tasks []protocol.TaskInfo, id string, focus protocol.Capability) (ReachReport, error)` — `Bits` = one line per bit of `ExercisedBits(caps)` (strongest-first as `verb.AuditOrder`), or just `focus` when non-zero; `Granter` = `AuthorityLabel`; error `no such task <id8>` when the id is not listed.
  - `func WiderLine(w authority.Wider, granter string) string` — `wider than parent: <caps>[ +vis] (granter=<label>)`, `""` when empty. The WebUI tooltip uses the same string.
  - `func WriteReachReport(w io.Writer, r ReachReport, asJSON bool) error`.
  - Verb row `caps reach <task-id> [--cap NAME] [--json]`: `CmdlineSurfaces: CLI | TUI | WebUI`, `WebUIDispatch{Fn: "capsReachText"}`, `--json` on CLI | WebUI (`SurfaceReason` as `caps audit`'s), `ModalSurfaces: [{WebUI, "webui/index.html#task-tree-reach"}]`, `Validate`: `--cap` names exactly one capability (as `caps audit`).

Text form (one line per entry, ids 8-hex, a count before the ids):

```
caps reach 3f2a9c1d  granter=spawn@0870ea17 (2h ago)
  cancel     act 2: 3f2a9c1d 8f2e1a7b   see +0
  file_read  act global (+ tasks you cannot list)
  view       see 3: 3f2a9c1d 8f2e1a7b a214413f
  delegate   caps=file_read  act 2: 3f2a9c1d 8f2e1a7b   see +0
  wider than parent: cancel +vis (granter=operator_caps_set@operator (5m ago))
```

JSON: one object `{"task","granter","bits":[{"cap","act_all","act","see_all","see"}],"view":{…},"delegate":{…,"caps"},"wider":{"caps","sees"}}`, full ids, empty arrays not null.

- [ ] **Step 1: Write the tests** in `cli/reach_test.go` on a listing built as `TaskInfo`s: `TestReachReportLines` (the text above for a fixed tree, line by line); `TestReachReportGlobalSaysUnlisted` (Review Focus 1: a task with `global` reads `act global (+ tasks you cannot list)` even when only two tasks are listed); `TestReachReportFocus` (`--cap cancel` → one `Bits` line); `TestReachReportUnknownTask` (error names the id); `TestReachReportJSON` (keys, full ids, `[]` not `null`); `TestWiderLine`.
- [ ] **Step 2: Run** `go test ./cli/ -run 'TestReachReport|TestWiderLine'` (env scrub) — Expected: FAIL to compile.
- [ ] **Step 3: Implement** `cli/reach.go`; add the verb row and `go generate ./cli/verb/`; `cliVerbs.CapsReach` = snapshot + `BuildReachReport` + `WriteReachReport(os.Stdout, …)`; `tuiVerbs.CapsReach` = the same over the TUI's listing, each text line appended to `cmdresult` (an error in `ErrorStyle`).
- [ ] **Step 4: Run** `go test ./cli/... ./tui/ -count=1` — Expected: PASS, including the verb invariant and consumers tests.
- [ ] **Step 5: Commit** `feat(cli): caps reach — what a task reaches per bit, may hand down, and whether it is wider than its parent`.

---

### Task 3: the WebUI tree's reach mode and the wider mark

**Files:** Modify `cmd/harness-webui-wasm/main.go`, `webui/index.html`, `webui/static/main.js`, `webui/static/style.css`; Test `webui/static/cmd_test.mjs` (`caps reach` dispatch), Playwright

**Interfaces:**
- Consumes: Tasks 1–2.
- Produces:
  - wasm: `lastListing []protocol.TaskInfo`, assigned where the snapshot builds `taskTree`.
  - Bridge `harness.capsReachText({taskId, cap, json}) -> Promise<string>` (the command input's dispatch, as `capsAuditText`).
  - Bridge `harness.reachOf({taskId, sel}) -> {actAll, seeAll, act: [hex], see: [hex], bits: [{name, label}]}` — `sel` is a capability name, `"view"` or `"delegate"`; `bits` lists the selector's entries for that task (its exercised bits in audit order, then `view`, `delegate`). Over `lastListing`.
  - Snapshot tree node: `wider` = `cli.WiderLine(...)` string, present only when non-empty.
  - `main.js`: `reachMode = {taskId, sel}` or `null`; task sheet item 「🎯 到達範囲を見る」 sets it, switches on tree mode and opens the 🌳 graph; while on, a `<select id="task-tree-reach-sel">` with `reachOf(...).bits` replaces `#task-tree-bit` and `drawTaskTree` passes `opts.reach = harness.reachOf(reachMode)`; `renderTaskTreeGraph` classes nodes `tt-reach-act` (filled), `tt-reach-see` (outlined), `tt-dim` (the rest); when `actAll` (or `seeAll` for `view`) a banner `#task-tree-reach` reads `<sel>: global (+ tasks you cannot list)` instead of filling nodes; Escape, the banner's ✕ or selecting another task's sheet item leaves or retargets the mode. A node with `wider` carries one `tt-wider` mark (a small ring segment in one colour) whose `<title>` is the `wider` string.

- [ ] **Step 1: Write the test** in `cmd_test.mjs`: `caps reach <ID> --cap cancel --json` dispatches `capsReachText` with `{taskId: ID, cap: "cancel", json: true}`.
- [ ] **Step 2: Run** `make js-test` — Expected: FAIL (no dispatch for `caps reach`).
- [ ] **Step 3: Implement** the bridge, the snapshot field and the page.
- [ ] **Step 4: Run** `make js-test` — Expected: PASS. Then against `scripts/dummy-harness.sh` (a three-level tree; an operator-widened grandchild; a global root), in Playwright at 1280px and 390px: reach mode on the middle task fills its subtree for `cancel` and outlines a `vis-ids` target; `view` and `delegate` entries change the fill; the global root shows the banner (Review Focus 5); a poll redraw keeps the mode and selection (Review Focus 4); the widened grandchild shows the mark with the `wider than parent: …` title; Escape leaves. Screenshots under the worktree's `.superpowers/`, reported by path.
- [ ] **Step 5: Commit** `feat(webui): reach mode on the creator tree; wider-than-parent marks`.

---

### Task 4: docs, parity walk, land

**Files:** Modify `README.md` (Capabilities and scope: `caps reach`; the WebUI tree's reach mode), `runner/agentskills/supervising-workers/SKILL.md` + both mirrors (`caps reach <own id>` — and its `delegate` line — as how an agent checks what a worker it is about to spawn could reach), the spec (amendment: `ReachOf`, the `delegate` and `view` selector entries, V19 overtaken), `.claude/skills/surface-parity-checklist/firing-log.md`

- [ ] **Step 1: README and skill** (read each in full first; Edit, not Bash; `cmp` the three skill copies after).
- [ ] **Step 2: Spec amendment** for the three decisions above.
- [ ] **Step 3: Walk `surface-parity-checklist` items 1–39** (S1–S6 n/a: no agent launch change), log the walk.
- [ ] **Step 4: `make test`** (env scrub) — Expected: PASS.
- [ ] **Step 5: Commit**, then ask the operator before landing (no wire change: land + `make build`, no server restart).
