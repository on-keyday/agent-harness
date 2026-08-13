---
name: operator-surface-checklist
description: Use BEFORE adding, changing, or displaying any operator-visible task/runner field (caps, scope, status, agent, repo, …), adding an option to any verb family, or wiring a new operator action's result reporting. A NUMBERED checklist (1–33) that must be walked item by item with a verdict per number — never summarized. Every input surface, every display surface, the per-path semantics axes, and the result/display conventions across CLI/TUI/WebUI/wasm.
---

# Operator-surface checklist

Every operator-visible field lives on ~15 surfaces across three UIs, and
every option's MEANING exists once per path it is reachable from. History
shows these get found one at a time, by the user, after landing. Sibling:
`implementation-pitfalls` Pitfall 9 is the incident record; this file is the
checklist.

## How to use — non-negotiable

This is a CHECKLIST, not reference prose. When this skill applies, walk
**every number from 1 to 33 in order** and record a verdict for each:

- `done` — implemented/verified, with the file touched
- `n/a` — genuinely not applicable, WITH the reason stated
- `omitted` — deliberately skipped, WITH the reason stated (this is a design
  decision that belongs in the diff or spec, not a silent default)

A walk that skips numbers or collapses into "surfaces covered" is invalid —
the summary sentence is exactly where discretionary omission hides. If an
item is expensive, say so and mark it; do not silently drop it.

## Input surfaces (new flag / option / request field)

1. CLI flags — `cmd/harness-cli/main.go` (submit / interactive / caps set),
   `cmd/harness-cli/session.go` (session new), and their help text.
2. CLI parse helpers — `cli/caps.go` (`ParseCaps`, `CapsCatalog`),
   `cli/scope.go` (`ParseScope`, `ScopesCatalog`) — one grammar, one parser;
   never a per-surface copy.
3. TUI cmdline verbs — `tui/cmdline.go` (`parseSetCaps`, per-command flag
   loops).
4. TUI keybindings — `tui/keys.go` (mainKeyMap + mainKeyBindings row —
   keys_test enforces the pair), dispatcher in `tui/app.go`.
5. TUI pickers/popups — `tui/authoritypicker.go`, `tui/popup.go` — remember
   batched KeyRunes (a fast `jjj` is ONE msg).
6. WebUI controls — `webui/index.html` + `webui/static/main.js` (chips via
   `buildCapChips`, checklists via `buildTaskChecklist`, dialogs via
   `<dialog class="picker-modal …">`).
7. WebUI command input — `runCmd` in `main.js` — a separate parser from the
   buttons.
8. wasm bridge — `cmd/harness-webui-wasm/main.go` `opts.Get("…")` names must
   match the JS request keys.
9. Session-default state — TUI `App.sessionCaps`/`sessionScope`; WebUI
   `spawnCaps`/`spawnScope` — every spawn call site must read them.
10. Every OTHER verb family — `file` / `git` / `forward` / `prune` /
    `notify` / `board` / `session` sub-verbs / `caps set`: each exists as a
    CLI flag set, a TUI cmdline parser, and (subset) a WebUI control or
    `runCmd` case. A new option on one family member (`file pull -n`) has
    the same per-UI parity obligations as a spawn flag.

## Display surfaces (field must be VISIBLE, not just accepted)

11. `ls` human rows — `cli/list.go` (row renderer).
12. `ls --json` — `cli/list.go` (JSON struct — always carries everything,
    no elision).
13. `whoami` — `cli/whoami.go` (both the operator and task branches).
14. `session ls` — JSON rows in `cli/` session listing.
15. `caps` catalog — `cli/caps.go` `WriteCaps` + SCOPE section.
16. TUI task table — `tui/tasks.go` `SetRows` columns.
17. TUI task detail popup (`d`) — `tui/detail.go` `formatTaskDetail` —
    **this one had no scope line for a full release**.
18. TUI runner detail — `tui/detail.go` `formatRunnerDetail`.
19. TUI picker rows — `tui/authoritypicker.go` `buildRows` (id, status,
    agent, repo, prompt head).
20. WebUI task row meta — `renderTaskList` metaText in `main.js`.
21. WebUI task detail sheet — the `addItem` action list per task.
22. WebUI dialogs' prefill — needs RAW values in the wasm snapshot
    (`capsBits`/`scopeBase`/`scopeIds` pattern) — labels like `all,-spawn`
    cannot be re-parsed in JS.
23. wasm snapshot JSON — the `ls` conversion map in
    `cmd/harness-webui-wasm/main.go` — label string AND raw value.

## Semantics axes — ask these PER OPTION, not per surface

Items 1–23 find missing knobs and missing pixels; these find wrong
MEANINGS. They apply to EVERY verb family (item 10), not just spawns: any
option reachable from more than one verb, mode, or path needs its meaning
written down per path (`prune` with ids vs the bare age sweep, `file push
-f` vs `pull -f`, a forward flag on register vs kill — same discipline).

24. Same option, other path — for every option on a multi-path verb: what
    happens on EACH path? "Applied", "kept", "ignored-with-error" — written
    down, not implied. The incident: spawn options on the resume path — a
    zero value that means "default" on create means "overwrite with
    default" on resume unless a presence bit says otherwise.
25. Presence — can the wire tell "not given" from "given the zero value"?
    If not and the difference matters (resume, set-style RPCs), add a
    presence bit — reserved bits in an existing byte first (`scope_present`
    cost zero layout change).
26. Session defaults — defaults (TUI `sessionCaps`/`sessionScope`, WebUI
    Compose state) feed FRESH spawns only. A resume must never inherit
    whatever the default picker happens to hold — gate it behind an
    explicit control.
27. Shared funnel — new request fields land in the shared builders
    (`cli.buildSubmitRequest` / `buildOpenInteractiveRequest` via
    `cli.SessionOpts`), never in one caller — native, wasm, and x11 all
    funnel through them, so a per-path field silently misses the other two.
28. Persistence — a new task field needs its `WALEvent` fields AND a
    decided replay meaning for records written before it existed (scope
    chose zero = subtree = old behaviour).

## Conventions (each learned from a user complaint)

29. Result messages name the target and the change — `caps set <id8>:
    caps=… scope=…`, never a bare count; counts only as a fan-out suffix
    (`+N descendant(s) clamped`) when a cascade actually reached something.
30. Results go to the result surface, not the status indicator — WebUI:
    `appendCmdOutput` (like the ✕ Cancel handler); `setStatus` is the
    CONNECTION badge. TUI: `a.cmdresult`.
31. Do not hide default values in detail-ish views — a hidden
    `scope=subtree` reads as "this task has no scope". Documented
    exception: CLI human `ls` rows and the TUI table elide the subtree
    default for row width — the JSON forms always carry it. If that
    exception draws complaints, drop it everywhere at once.
32. One serializer per grammar per runtime, pinned by tests —
    `scopeSpecFor` (Go) / `scopeSpecJS` (JS), both feeding
    `cli.ParseScope`; round-trip tests keep them honest. Fixed frames:
    popup/dialog width derives from the terminal/viewport, never content
    (TUI picker `fit()`, `.picker-modal.regrant-modal`).
33. A typed option either takes effect or errors — never silently ignored,
    never silently overwriting. Both failure shapes shipped at once: a lone
    `--scope` on resume was dropped, and `--caps` without `--scope` reset
    the task's scope. When the wire cannot express "keep", the fix is a
    presence bit, not documentation.

## When to invoke

- Adding a field to `TaskInfo` / `RunnerInfo` / any snapshot row
- Adding or renaming a capability, scope form, or status value
- Adding an option to ANY verb family (spawn or otherwise)
- Adding an operator action (new keybinding / button / dialog / verb)
- Reviewing a diff that claims "CLI, TUI and WebUI covered"

When a defect ships through this checklist anyway: fix the defect AND add
the number that would have caught it. The list grows by incident, and the
1–N walk is renumbered — a stale N in reports is harmless; a skipped
surface is not.
