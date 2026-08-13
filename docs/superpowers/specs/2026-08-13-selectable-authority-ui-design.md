# Selectable caps/scope pickers: no more typed grammar in TUI/WebUI

Date: 2026-08-13

## Problem

The re-grant and spawn surfaces added by
`2026-08-13-task-scoped-caps-design.md` take their input as typed strings:

- WebUI re-grant is three chained `window.prompt`s (`promptSetCaps`,
  `webui/static/main.js:2631`): scope grammar by hand, caps names by hand,
  then a bare confirm for cascade. `keep_conns` is not reachable at all.
- WebUI spawn scope is a free-text `#scope-input` validated only when the
  request is built.
- TUI re-grant (`a` key) prefills `caps set <id> ` on the command line and
  leaves the operator to type `--caps NAMES --scope SPEC` from memory.
- TUI session defaults are `caps <names>` / `scope <spec>` — same typing.

Typing capability names and scope grammar requires remembering both. A typo
costs a round trip through an error line and a full re-type. And the id form
`ids:<32-hex>` — which is a primary use ("let this worker reach that specific
task"), not an edge case — is the worst to type: nobody transcribes 32-hex ids
by hand (`feedback_no_hand_typed_ids_in_agent_messages` exists for exactly
this reason). Every value being chosen here is enumerable: 12 capability
bits, 3 scope bases, and the tasks currently on the server. Enumerable
choices should be selected, not typed.

CLI flags are out of scope: text is the CLI's native interface and its
`--caps`/`--scope` stay as they are.

## Design

### 1. One selection model, serialized to the existing grammar

Every picker edits the same three-part state: a caps bitmask, a `ScopeBase`,
and a set of selected task ids. On apply, the scope parts are serialized to
the existing `--scope` grammar string (`subtree`, `none`, `global`,
`ids:X[,Y]`, `subtree+ids:X`) and handed to the same funnel as today — `cli.ParseScope` behind the wasm bridge and `cli.SetCapsOpts` /
`SessionOpts` in the TUI. No wire change, no bridge change, no second
scope representation to drift from the parser.

Rules shared by every picker:

- `base = global` disables (and visually clears) the task id selection —
  the grammar has no `global+ids` form and the effective set is already
  everything.
- The task id list is the current snapshot's tasks, terminal ones included
  (scope ids on terminal tasks are meaningful: resume, logs, git), in task
  table order. Each row shows short id, status, agent, and the prompt's
  head. On re-grant, the target task itself is omitted from the list —
  `{self}` is unconditionally in scope, so listing it is noise.
- Prefill: re-grant pickers open with the target task's current
  `Capabilities` and `Scope` (its ids pre-checked); spawn pickers open with
  the session defaults (`spawnCaps`/`spawnScope` in WebUI,
  `sessionCaps`/`sessionScope` in TUI).
- **A re-grant apply always sends both fields explicitly** — the bitmask as
  edited and the scope string including a literal `subtree` (a valid
  `ParseScope` form). The prompt flow's "leave empty to keep" disappears:
  the picker is prefilled with the current values, so an untouched field
  sends its current value, which is the same keep, without the dialog
  needing a fourth "unset" state per field. Spawn pickers serialize
  base-subtree-no-ids to the empty string, which is the existing spawn
  default.
- The serialized grammar string is echoed in the picker (small, monospace,
  ellipsized in CSS / truncated to the pane width in the TUI), so the
  selection and its text form stay visibly the same thing and the picker
  doubles as grammar documentation.

### 2. WebUI

**Re-grant dialog.** `promptSetCaps` is replaced by a native `<dialog>`
(the `showModal()` pattern `filePreviewModal` / `gridModal` already use)
containing:

- a caps chip row — the same `cap-chip` buttons `renderCaps()`
  (`main.js:2555`) builds for Compose, factored into a helper
  `buildCapChips(container, getBits, setBits)` so Compose and the dialog
  share one implementation instead of a copy;
- three radio buttons for the base (`subtree` / `none` / `global`);
- the task checklist (checkboxes, scrollable, `max-height` with
  `overflow-y: auto`);
- `--cascade` and `--keep-conns` checkboxes, both default off —
  `keep_conns` becomes reachable from the WebUI for the first time;
- the grammar echo line;
- 適用 / キャンセル buttons. 適用 sends via `window.harness.setCaps` exactly
  as today and reports through `setStatus` (affected count, connections
  closed). キャンセル and Esc close without a request.

**Compose spawn scope.** The `#scope-input` free-text field is replaced by
the same base radios plus the task checklist folded into a
`<details>`（`対象タスクを選択 (n)`, n = checked count) to keep Compose short
on a 390px screen, with the grammar echo underneath. The assembled string
goes into `spawnScope` exactly where the text field's value went — the
`sessionReq` funnel is untouched. Dark theme (`#1e1e1e`/`#d4d4d4`), both
widths verified.

### 3. TUI

**One widget, two openers.** A new `AuthorityPickerModel` (own file,
`tui/authoritypicker.go`, sibling of `RunnerPickerModel` / `PopupModel`) is
a single scrollable list — deliberately not a multi-section form, so there
is no intra-popup focus management:

```
[x] spawn          … 12 capability rows, Space toggles
[ ] cancel
…
base: subtree      … Space cycles subtree → none → global
[ ] 51646738 Done fake-claude.sh "deviation target"   … task rows, Space toggles
…
[ ] --cascade      … re-grant mode only
[ ] --keep-conns   … re-grant mode only
```

j/k and ↑/↓ move, Space toggles (on the base row: cycles), Enter applies,
Esc closes without applying. The footer line shows the grammar echo. Task
rows are disabled (skipped by the cursor) while `base = global`.

- **Re-grant mode** — the tasks-pane `a` key opens the picker on the
  selected task instead of prefilling the command line. Enter dispatches
  the existing `DoSetCaps` and the result line stays
  `caps set: N task(s) changed`. The cmdline `caps set <id> --caps/--scope`
  text form remains for scripting.
- **Session-default mode** — `caps` and `scope` with no argument both open
  the picker prefilled with `sessionCaps`/`sessionScope` (cascade/keep-conns
  rows hidden); Enter writes both defaults and echoes them. The current
  no-arg behaviour (print the value) moves into the picker itself — it
  shows the value more legibly than the one-line echo did. `caps <names>` /
  `scope <spec>` with arguments keep their parse-and-set behaviour.

The `a` key's no-selection warning and the nil-client guard behaviour are
unchanged: opening the picker needs no client, applying goes through
`runAction`'s existing default-deny guard.

### 4. What does not change

- CLI: no changes.
- Wire/protocol: no changes.
- The scope grammar and `cli.ParseScope`: no changes — the pickers are a
  front end to the same strings.
- `supervising-workers` skill text: no changes (agent-side is CLI).

## Testing

**TUI unit (`tui/`)**

- Picker state: toggle caps row, cycle base, toggle task row, and assert
  the serialized grammar string for each base × ids combination (including
  `ids:` with base none and `subtree+ids:`).
- `base = global` skips/disables task rows and serializes to `global`.
- Re-grant mode prefill from a `TaskInfo` (caps + scope ids pre-checked,
  target task absent from the row list).
- `a` on the tasks pane opens the picker on the selected task; Esc closes
  without dispatching; Enter dispatches `DoSetCaps` with the assembled
  opts. No-selection still warns. Nil client still answers "not connected"
  on apply.
- `caps` / `scope` with no argument open the picker; with arguments they
  behave as before (existing tests keep passing).

**WebUI (Playwright, dummy harness)**

- Re-grant dialog: chips prefilled from the task's current caps, its scope
  ids pre-checked; select `none` + one task, apply, `ls` shows the new
  scope. Cancel and Esc leave the server state untouched.
- keep-conns checkbox round-trips (visible in the request; server behaviour
  is already covered by `set_caps` tests).
- Compose: scope selection feeds a spawned task's scope (verify via `ls`).
- Both widths (desktop, 390px), dark theme; screenshots kept as
  deliverables.

**TUI E2E (dummy harness, real keystrokes)**

- `a` → toggle a cap off → cycle base to none → Enter →
  `caps set: 1 task(s) changed`, `ls` reflects it.
- Esc mid-picker leaves the TUI usable and the server unchanged.
