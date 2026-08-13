---
name: operator-surface-checklist
description: Use BEFORE adding, changing, or displaying any operator-visible task/runner field (caps, scope, status, agent, repo, …) or wiring a new operator action's result reporting. The enumerated checklist of every input surface AND every display surface across CLI/TUI/WebUI/wasm, plus the result-message and default-value display conventions. Exists because these surfaces were repeatedly rediscovered one user complaint at a time.
---

# Operator-surface checklist

Every operator-visible field lives on ~15 surfaces across three UIs. History
shows they get found one at a time, by the user, after landing ("the TUI
detail has no scope line", "the WebUI hides the default", "the result says
1 task changed which is zero information"). Walk BOTH tables below before
declaring an operator-visible change done. Sibling: the INPUT-parity version
of this lesson is `implementation-pitfalls` Pitfall 9 — this file is the
superset checklist to actually walk.

## A. Input surfaces (new flag / option / request field)

| # | Surface | Where |
|---|---------|-------|
| 1 | CLI flags | `cmd/harness-cli/main.go` (submit / interactive / caps set), `cmd/harness-cli/session.go` (session new), help text |
| 2 | CLI parse helpers | `cli/caps.go` (`ParseCaps`, `CapsCatalog`), `cli/scope.go` (`ParseScope`, `ScopesCatalog`) — one grammar, one parser; never a per-surface copy |
| 3 | TUI cmdline verbs | `tui/cmdline.go` (`parseSetCaps`, per-command flag loops) |
| 4 | TUI keybindings | `tui/keys.go` (mainKeyMap + mainKeyBindings row — keys_test enforces the pair), dispatcher in `tui/app.go` |
| 5 | TUI pickers/popups | `tui/authoritypicker.go`, `tui/popup.go` — remember batched KeyRunes (a fast `jjj` is ONE msg) |
| 6 | WebUI controls | `webui/index.html` + `webui/static/main.js` (chips via `buildCapChips`, checklists via `buildTaskChecklist`, dialogs via `<dialog class="picker-modal …">`) |
| 7 | WebUI command input | `runCmd` in `main.js` — a separate parser from the buttons |
| 8 | wasm bridge | `cmd/harness-webui-wasm/main.go` `opts.Get("…")` names must match the JS request keys |
| 9 | Session-default state | TUI `App.sessionCaps`/`sessionScope`; WebUI `spawnCaps`/`spawnScope` — every spawn call site must read them |
| 10 | Every OTHER verb family | `file` / `git` / `forward` / `prune` / `notify` / `board` / `session` sub-verbs / `caps set` — each exists as a CLI flag set, a TUI cmdline parser, and (subset) a WebUI control or `runCmd` case. A new option on one family member (`file pull -n`) has the same per-UI parity obligations as a spawn flag; this table is not spawn-only. |

## B. Display surfaces (field must be VISIBLE, not just accepted)

| # | Surface | Where |
|---|---------|-------|
| 1 | `ls` human rows | `cli/list.go` (row renderer) |
| 2 | `ls --json` | `cli/list.go` (JSON struct — always carries everything, no elision) |
| 3 | `whoami` | `cli/whoami.go` (both the operator and task branches) |
| 4 | `session ls` | JSON rows in `cli/` session listing |
| 5 | `caps` catalog | `cli/caps.go` `WriteCaps` + SCOPE section |
| 6 | TUI task table | `tui/tasks.go` `SetRows` columns |
| 7 | TUI task detail popup (`d`) | `tui/detail.go` `formatTaskDetail` — **this one had no scope line for a full release** |
| 8 | TUI runner detail | `tui/detail.go` `formatRunnerDetail` |
| 9 | TUI picker rows | `tui/authoritypicker.go` `buildRows` (id, status, agent, repo, prompt head) |
| 10 | WebUI task row meta | `renderTaskList` metaText in `main.js` |
| 11 | WebUI task detail sheet | the `addItem` action list per task |
| 12 | WebUI dialogs' prefill | needs RAW values in the wasm snapshot (`capsBits`/`scopeBase`/`scopeIds` pattern) — labels like `all,-spawn` cannot be re-parsed in JS |
| 13 | wasm snapshot JSON | the `ls` conversion map in `cmd/harness-webui-wasm/main.go` — label string AND raw value |

## B2. Semantics axes — ask these PER OPTION, not per surface

Walking A and B finds missing knobs and missing pixels; this table finds
wrong MEANINGS. The resume-scope reset shipped through both tables above —
every surface had the flag, every view displayed the value — because nobody
asked what the flag meant on the other path. These axes apply to EVERY verb
family (A#10), not just spawns: any option reachable from more than one
verb, mode, or path needs its meaning written down per path (`prune` with
ids vs the bare age sweep, `file push -f` vs `pull -f`, a forward flag on
register vs kill — same discipline).

| Axis | Question to answer explicitly |
|------|------------------------------|
| same option, other path | For every option on a multi-path verb: what happens on EACH path? "Applied", "kept", "ignored-with-error" — written down, not implied. The incident: spawn options on the resume path — a zero value that means "default" on create means "overwrite with default" on resume unless a presence bit says otherwise. |
| presence | Can the wire tell "not given" from "given the zero value"? If not and the difference matters (resume, set-style RPCs), add a presence bit — reserved bits in an existing byte first (`scope_present` cost zero layout change). |
| session defaults | Defaults (TUI `sessionCaps`/`sessionScope`, WebUI Compose state) feed FRESH spawns only. A resume must never inherit whatever the default picker happens to hold — gate it behind an explicit control. |
| shared funnel | New request fields land in the shared builders (`cli.buildSubmitRequest` / `buildOpenInteractiveRequest`), never in one caller — native, wasm, and x11 all funnel through them, so a per-path field silently misses the other two. |
| persistence | A new task field needs its `WALEvent` fields AND a decided replay meaning for records written before it existed (scope chose zero = subtree = old behaviour). |

## C. Conventions (each learned from a user complaint)

- **Result messages name the target and the change.** `caps set <id8>:
  caps=… scope=…`, never a bare count — "1 task(s) changed" is zero
  information on a single-target call. Counts appear only as a fan-out
  suffix (`+N descendant(s) clamped`) when a cascade actually reached
  something.
- **Results go to the result surface, not the status indicator.** WebUI:
  `appendCmdOutput` (like the ✕ Cancel handler); `setStatus` is the
  CONNECTION badge and writing a result there parks it over "connected".
  TUI: `a.cmdresult`.
- **Do not hide default values in detail-ish views.** A hidden
  `scope=subtree` reads as "this task has no scope". WebUI task rows and
  the TUI detail popup always show scope. Documented exception: CLI human
  `ls` rows and the TUI table still elide the subtree default for row
  width — the JSON forms always carry it. If that exception starts
  producing complaints too, drop it everywhere at once.
- **One serializer per grammar per runtime, pinned by tests.** Scope
  strings come from `scopeSpecFor` (Go) / `scopeSpecJS` (JS), both feeding
  `cli.ParseScope`; round-trip tests keep them honest.
- **Fixed frames.** Popup/dialog width derives from the terminal/viewport,
  never from content — content-sized frames resize on every selection
  (TUI picker `fit()`, `.picker-modal.regrant-modal` width).
- **A typed option either takes effect or errors — never silently ignored,
  never silently overwriting.** Both failure shapes shipped at once: a lone
  `--scope` on resume was dropped without a word, and `--caps` without
  `--scope` reset the task's scope to the request default. When the wire
  cannot express "keep", the fix is a presence bit, not documentation.

## When to invoke

- Adding a field to `TaskInfo` / `RunnerInfo` / any snapshot row
- Adding or renaming a capability, scope form, or status value
- Adding an operator action (new keybinding / button / dialog / verb)
- Reviewing a diff that claims "CLI, TUI and WebUI covered"
