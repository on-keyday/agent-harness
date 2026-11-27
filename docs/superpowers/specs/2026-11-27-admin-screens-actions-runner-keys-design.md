# Screens for actions and runner administration (TUI, WebUI) — design

Date: 2026-11-27. Line references are to `main` at `4c4e1d48`.

**Status: DESIGN, awaiting the operator's approval.** Decisions taken with the
operator on 2026-11-27: the TUI gains override editing (Q1 = b); the
command-line-implies-screen rule and its shrinking list are in scope (Q2).

## Problem

Twelve operator verbs, and the action variant of `exec`, reach the TUI and the
WebUI only through their command input:

- actions — `action new`, `action edit`, `action rm`, `action ls`
  (`cli/verb/table.go:916-979`), `exec --action`, and the `actions:<id>` grant
  condition. The WebUI's authority dialogs take it as 32 hex typed into a text
  box (`buildOverrideRows`, `webui/static/main.js:5044-5066`, shared by Compose
  and regrant). The TUI has no control for it at all: its authority picker
  carries per-capability overrides and their conditions without editing them
  (`tui/authoritypicker.go:38-49`, "overrides is CARRIED, not edited"), so
  `--scope-for` and `--grant-condition` on the command input are the only way
  to narrow one capability on the TUI;
- runner administration — `runner enroll`, `runner keys`, `runner unenroll`,
  `runner kick`, `runner dial add`, `runner dial ls`, `runner dial rm`,
  `server dial-runner` (`cli/verb/table.go:884-1133`).

Each declares `NoModalSurface` with "an administrative verb … the command line
is the way in". The command input of the TUI or the WebUI is the CLI inside
another window; a feature is on a UI when that UI has its own controls for it,
as schedules (`#schedules-modal`, the TUI Schedules table) and credentials
(`#credentials-modal`, the TUI Credentials table) do. The operator does not
count the command input as "usable from the WebUI" (2026-11-27).

## Goals

- Every verb above is reachable on the TUI and the WebUI through that UI's own
  controls: a list with per-row actions, and a form for each create or edit.
  The command input keeps working as it does.
- Each of the twelve verbs drops its `NoModalSurface` and declares
  `ModalSurfaces` rows naming the new entry points, which
  `TestModalSurfaceEntryPointsExist` (`cli/verb/modal_surface_test.go`)
  resolves to definitions. `exec` declares the TUI and WebUI entry points that
  run an action.
- A grant condition on `exec_run` is built by picking actions by name, never by
  typing an id — on the WebUI, and on the TUI, whose authority picker gains
  per-capability override editing to have somewhere to put it (§ TUI override
  editing).
- **A verb typable on a UI's command input has a screen on that UI**, enforced
  by a test (§ The command-line-implies-screen rule). Typing it there proves the
  UI can run it, so "functionally impossible" cannot be claimed for it; a
  free-text reason is no longer accepted for it.

## Non-goals

- Building screens for the other verbs the rule finds without one. They stay on
  the rule's shrinking list (below) and leave it one by one; `docs/backlog.md`
  gets one entry for that inventory. Before building a screen for one, check
  whether an equivalent screen exists and is only undeclared: `session send`
  declares "no WebUI element reaches this" while the WebUI session preview sends
  input to the session (`webui/static/main.js:6526`, `previewInput`).
- Verbs typable on no UI's command input (CLI only: `agent *`, `credential
  keygen/bind`, `mcp`, …). The rule says nothing about them, and whether one
  should be typable on a UI is a separate question.
- A generic form generated from `VerbSpec` flags (see Approaches).
- Any wire, server or CLI behaviour change beyond the one validation rule in
  § Actions screen. Every RPC these screens call exists.

## Anti-goals

- A screen that accepts what the server will refuse when the UI could have made
  it unbuildable (surface-parity item 34a): a template whose first element is
  a param, a choice param with no choices, a condition on a bit it does not
  apply to.
- A second wording of a result: list rows and results are rendered by the
  `cli.Format*` / row functions the CLI uses, as every existing modal does.
- A destructive row action without confirmation (`runner unenroll`,
  `runner kick`, `action rm`, `runner dial rm`), and a `--force` the operator
  did not ask for: force is offered only after the plain request was refused
  for running tasks, and says how many.

## Approaches

| Approach | What it is | Trade-off |
| --- | --- | --- |
| **A. One screen per feature, following Credentials / Schedules** (picked) | WebUI: a `<dialog>` per feature with a list, row actions and a form. TUI: a modal per feature (the `execsmodal` shape, since both lists come from their own RPC, not from the snapshot the TUI tables render) | Two screens, two forms each, per UI. Every control is the kind its neighbours use (chips, selects, task checklist), so nothing is free text that the UI could have built |
| B. Forms generated from `VerbSpec` | One generic admin dialog renders each verb's flags as inputs | One mechanism for all 71 verbs, but a flag is a string to it: `--param env=choice:a,b` and `--bind ID,…` stay typed text, which is the complaint; and it has no list to act on |
| C. Command-line prefill | A button fills the command input (the TUI's `credential new` does this today) | Cheapest, and it is the command input again |

## Design

### Data across the wasm bridge

The WebUI bridge functions for these verbs return `{text, ok}` today
(`cmd/harness-webui-wasm/main.go:3317-3504`, `harnessActionList`). A list
screen needs rows. Add row-returning siblings that call the same `cli`
functions and convert the protocol structs to plain objects:

- `harness.actionRows()` → `[{id, name, revision, description, params:
  [{name, type, choices}], template: [{literal} | {param}], templateText,
  editedBy, editedAt}]`. `templateText` is `cli.ActionTemplateString`.
- `harness.runnerKeyRows()` → `[{prefix, key, label, enrolledBy, enrolledAt,
  bound: [taskId]}]`; `harness.runnerDialRows()` → `[{id, candidates, keyPrefix,
  label, …the fields RunnerDialRecord carries}]`.
- `harness.actionPutObject(obj)` takes the form's object, builds an
  `ActionPutInput`, validates with `actiondef.Validate` and sends through
  `cli.RunActionPut`; the refusal text is the CLI's.

The TUI calls the `cli` functions directly and keeps the protocol structs.

### Actions screen

WebUI: `#actions-modal`, opened by an "Actions" button beside "Credentials".
TUI: `ActionsModal`, opened from a key in the main key map (bound beside the
credential keys; `tui/keys.go` and its test hold the pair).

```
Actions                                    [＋ New] [↻] [Close]
 deploy   rev 3   ./deploy.sh --env {env} --ref {ref}   [Edit] [🗑]
          env: choice staging, prod   ref: token
          edited by operator, 2026-11-27 10:02
```

- The list is `action_list`, sorted by name as the server sends it.
- New and Edit open one form:
  - name; description;
  - params, one row each: name, type (`choice` / `token` select), and for
    `choice` a list of choices (add / remove rows, not a comma-separated box).
    A choice containing `,` is refused: `--param NAME=choice:A,B` splits on it
    (`actiondef.ParseParam`), so such an action could not be restated by
    `action edit` from any command line without losing the choice. The rule
    goes into `actiondef.Validate`, so the server, the CLI and both screens
    refuse it alike — the one server-side change here (this spec's call);
  - template, one row per argv element: a select of "literal" or one of the
    declared params, and a text box when literal. The first row is fixed to
    literal (C17). A param that no row references is refused before sending,
    as `actiondef.Validate` refuses it.
  - Edit sends `expect_revision` = the revision the form was opened from; a
    `stale_revision` refusal says to reload.
- Delete asks for confirmation, naming the action.
- `holders_unreachable` shows the CLI's text with the count.
- `action ls` lists a choice param's choices joined by `|` (`cli/actions.go`
  `FormatActions`), which is ambiguous for a choice containing `|`. Choices are
  printed quoted instead (`%q`, space-separated), on every surface, since the
  screens render the CLI's row text (this spec's call).

### Running an action against a task

WebUI: a "▶ action を実行" item on the task sheet, beside "⌨ コマンド実行".
TUI: a key on the tasks table. Both open one form:

- action: a select of the actions the list returns;
- one control per param: a select of its choices for `choice`, a text box for
  `token` with `actiondef.ValidToken` checked before sending.

It sends `cli.ActionInvocationFor` through the existing exec path, with no
stdin and no shell (the server forces them off anyway). The result goes where
"⌨ コマンド実行" puts its result on the WebUI and to `cmdresult` on the TUI,
labelled with the action name.

### Grant condition picker

In the WebUI's authority dialogs (`buildOverrideRows`, Compose and regrant) and
in the TUI's override rows (next section), the condition control of a row whose
bits are exactly `exec_run` is a checklist of actions by name (and id8), from
`action_list`. The checked ids are serialized to `actions:<id>,…` for the
existing parser. The ports condition stays a text box: no existing control
expresses a port range. An action id in the condition that the list no longer
has (deleted) is shown as "deleted <id8>" and kept checked until unchecked, so
opening the dialog does not silently drop it.

### TUI override editing

`tui/authoritypicker.go` stops only carrying overrides and edits them, in both
of its modes (a task's regrant and the session defaults), with the controls the
WebUI's `buildOverrideRows` has, in the TUI's own widgets:

- a list of override rows; add and remove a row;
- per row: capability toggles, with a bit another row already claims not
  selectable (override masks are pairwise disjoint; the server refuses an
  overlap, so the picker cannot build one); a base (the picker's existing base
  control); exclude-self where the base takes it; the task id set (the picker's
  existing id checklist); and the condition control of the previous section
  where a condition kind applies to every bit of the row.

What it must keep: a regrant still sends the overrides and their condition
bodies it did not touch. An override the operator removed is the only one that
disappears, and the result line names it (surface-parity item 29). The picker's
comment at `tui/authoritypicker.go:38-49` states the failure this guards —
returning an empty list erases the target's per-capability rules — and its
test becomes: open on a task with two overrides, change one, send; the other
arrives unchanged with its condition body.

### Runner keys screen

WebUI: `#runner-keys-modal`, opened from the runners section header. TUI:
`RunnerKeysModal`, opened from a key on the runners table. Two lists:

- **Keys** (`runner keys`): prefix, label, enrolled by, bound tasks (id8 list,
  "unbound" otherwise). Row action: Unenroll (confirm; on a refusal, a second
  confirm offering force that shows what the refusal names — the connected
  runners on that key, not tasks). An unenroll prints the restore line the CLI
  prints; the screen shows it.
- **Enroll form**: public key (64 hex, a text box — a key is opaque data, not a
  choice), label, bind: the task checklist the scope dialogs use
  (`buildTaskChecklist` on the WebUI), empty = unbound.
- **Dial records** (`runner dial ls`): id, candidates, key, label. Row action:
  Remove (confirm). **Add form**: candidates (text, comma-separated, as the
  CLI), key (a select of enrolled keys; required — the server refuses a record
  whose key does not resolve, `server/runner_dial_handler.go:54-60`, and the key
  is what makes a runner answering on that address under another key refused),
  label.
- **Dial once** (`server dial-runner`): runner cid (text — the target is not
  registered, so there is nothing to pick), via (a select of registered
  runners, or direct).

### Kick

A row action on each runner: WebUI runners section, TUI runners table key.
Confirm naming the runner; on a refusal because it runs tasks, offer force with
the task count.

### Verb table

The twelve rows drop `NoModalSurface` and gain `ModalSurfaces` for TUI and
WebUI naming the entry points above; `exec` gains the run-action entry points.
`tui/surface_coverage_test.go` must see the new TUI modals as declared.

### The command-line-implies-screen rule

A `NoModalSurface` reason is free text, and a test cannot tell a reason from a
sentence that only says no screen exists yet ("surveyed: no TUI action and no
WebUI element reach this" is the reason on most of the 71 rows that have one);
any rule on the wording is passed by rewording. The rule is therefore on a fact
the table already declares, `CmdlineSurfaces`:

- For each surface S in {TUI, WebUI}: a verb with S in `CmdlineSurfaces` has a
  `ModalSurfaces` row for S, unless it is a command of that command line itself.
- That exception is a typed field, `LineCommand bool` on `VerbSpec`: the verb
  acts on the command line or the program hosting it (`quit`, `help`,
  `refresh`, `clear`, `diag`, …) and has no capability to put on a screen. No
  other exception exists.
- Verbs that break the rule today are listed by path in one Go slice beside the
  test (the shrinking list). The test fails when a verb breaks the rule and is
  not listed, and when a listed verb no longer breaks it, so the list cannot
  keep a verb that has gained its screen. The twelve verbs of this design are
  not on it.
- `NoModalSurface` stays for verbs typable on no UI's command input, where it is
  a note and the rule does not apply.

On 2026-11-27 (`4c4e1d48`) the 71 rows with a reason split as: 31 typable on
no UI; 22 on both; 17 on the TUI only; 1 on the WebUI only (`preview`, a WebUI
panel already). The rows typable somewhere are the input to the `LineCommand`
marks and the initial list.

## Constraints

- Results follow surface-parity items 29–31: named target and change, on the
  result surface (WebUI `appendCmdOutput`, TUI `cmdresult`), never on the
  connection badge; a zero is printed, not elided.
- WebUI bit operations only through `capHas` / `capOr` / `capXor`.
- Dialog width from the viewport, not content (item 32); phone width usable.
- Each screen refreshes its list after a successful change.

## Testing

- `cli/verb`: the existing modal-surface tests pass with the new rows; the
  rule's test fails for a verb typable on the TUI with no TUI screen and not
  listed, and for a listed verb that has one (both shown with a fixture table).
- `actiondef`: a choice containing `,` is refused by `Validate`.
- TUI override editing: open on a task with two overrides (one conditioned),
  change one, send — the other arrives unchanged with its body; remove one —
  only it disappears, and the result line names it; a bit claimed by one row
  cannot be toggled on in another.
- wasm bridge: row functions round-trip a record (JS test under
  `webui/static/*_test.mjs` with the real wasm, as `action_test.mjs` does).
- WebUI models: the action form builds a definition `actiondef.Validate`
  accepts; a form with an unreferenced param or a param-first template cannot be
  submitted; the condition checklist serializes to the CLI spelling and keeps a
  deleted id.
- TUI: each modal opens from its key, lists, and its form submits (model tests
  with a fake client, as the credentials tests do).
- Live (`dummy-harness --known-psk`): on the WebUI through headless Playwright
  and on the TUI in a dummy PTY — create, edit, run and delete an action;
  enroll, list and unenroll a key; add and remove a dial record; kick a runner.
  Screenshots kept as deliverables.

## Left to the builder

- Exact key bindings, within `tui/keys.go`'s free keys and its pair test.
- Layout details of each form, within the constraints above.
- Whether the TUI's two runner-key lists are one modal with a toggle or two
  sections.
