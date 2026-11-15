# Compose explicit actions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every WebUI launch control does one named thing, a resume with a prompt
happens in a dialog, a task can be acted on by pasted id, and the TUI gets a
schedule form.

**Architecture:**
- The logic the decisions need goes into pure functions in
  `webui/static/task_models.js`, tested in `cmd_test.mjs`.
- `main.js` and `index.html` wire those functions to markup.
- The TUI reuses `BgFormModel` and the existing `schedule new` verb path, so
  the request builders stay single (`sessionReq`, `ScheduleSpecOptsFromAction`).

**Tech Stack:** vanilla JS + node:test, Go (bubbletea TUI).

**Spec:** docs/superpowers/specs/2026-11-16-compose-explicit-actions-design.md

## Global Constraints

- No change to what is sent, except the WebUI command line's typed
  `--caps` / `--scope` / `--scope-for` on a resume, which now re-grants without
  a checkbox.
  - Every combination the server accepts stays reachable: background +
    one-shot, background + event-stream, and schedule + one-shot / PTY /
    conversation.
- No compatibility path for a stale page or wasm.
- No restyling outside Compose, the task sheet's resume controls, the new
  dialogs, and the id field.
- Button labels: interactive "session を開く", one-shot "one-shot を実行",
  background "background で投入", schedule "schedule を作る". Resume dialog
  button "再開する".
- The section heading `Compose` becomes `新規作成` (operator, 2026-11-16).
- localStorage keys: `composeTab` (default `interactive`). Every read and
  write is in try/catch, as `taskOrder` is.
- TUI keys stay unique (`TestMainKeysAreUnique`). The schedule form opens from
  the existing `N` binding when the Schedules table has focus.
- Repo is public: no local identifiers (land.py checks).

## Review Focus

1. Fields filled on one tab, then the tab is switched: the hidden tab's fields
   are not sent. For example, a cron typed and then interactive chosen opens a
   session and creates no schedule.
2. The id field given an id outside the current snapshot (pruned, or beyond
   the list): says unknown and acts on nothing.
3. A Resume… dialog open across a snapshot poll: what was typed survives.
4. A background task resumed from a path that is not its sheet (the
   notification feed): the dialog opens; nothing resumes directly.
5. A typed `session new --resume X --scope none` without `--caps`: scope is
   re-granted, caps is not.

---

### Task 1: The resume re-grant rule

**Files:** `webui/static/task_models.js`, `webui/static/main.js` (`sessionReq`,
`applyCapsOnResume`), `webui/static/cmd_test.mjs`.

**Interfaces — Produces:**
- `resumeRegrantFlags({ resumeTaskId, resumeExact, caps, scope, scopeFor })`
  → `{ resumeCapsOverride, scopePresent }`.
  - `resumeCapsOverride` is true iff `resumeTaskId && !resumeExact && caps !== null`.
  - `scopePresent` is true iff `resumeTaskId && !resumeExact && (scope !== null || scopeFor !== null)`.

- [ ] **Tests:** `resumeRegrantFlags`:
  - Typed caps on a resume → `{true, false}`.
  - Typed `scope:"none"` only → `{false, true}` (Review Focus 5).
  - Nothing typed → `{false, false}`.
  - No resume id, caps typed → `{false, false}`.
  - `resumeExact` → `{false, false}`.
- [ ] Run `node --test --test-name-pattern=resumeRegrantFlags webui/static/cmd_test.mjs`.
  Expected: FAIL (TypeError, not defined).
- [ ] **Implement.** `sessionReq` sets the two flags from `resumeRegrantFlags`
  over the caller's raw `caps` / `scope` / `scopeFor`, before the null → panel
  fallback.
  - Delete `applyCapsOnResume` and its checkbox wiring (`#caps-on-resume`,
    `capsOnResumeCb`).
  - Update the `cmdline.js` comment and the error text that mention Resume
    task id / the checkbox.
- [ ] `make js-test` → pass. Commit.

### Task 2: Compose action tabs

**Files:** `webui/index.html` (`#compose`), `webui/static/main.js` (Compose
wiring, `composeForm`, `composeRequest`, `syncComposeSchedule`,
`currentResumeTaskID` and every reader of it), `webui/static/task_models.js`
(`composeSubmitAction` replaced; `composeCronState`, `composeCronSync` and
`composeCapsOnResumeState` deleted), `webui/static/style.css`,
`webui/static/cmd_test.mjs`.

**Interfaces — Produces:**
- `COMPOSE_TABS = ["interactive", "oneshot", "background", "schedule"]`.
- `composeTabModel(tab)` → `{ label, action, fields }`.
  - `action` ∈ `"open-session" | "create-schedule"`.
  - `fields` is the set of field keys the tab shows and reads:
    - Shared: `repo host agent args prompt`.
    - interactive: `stream pty`.
    - oneshot: `pty`.
    - background: `cond oneshot stream pty`.
    - schedule: `cond cron tz target oneshot pty resumeConversation`.
- `composeFormFor(tab, raw)` → `raw` with every key outside
  `composeTabModel(tab).fields` reset to its empty value. This is the only path
  from the form to a request.
- `scheduleTargetTokens(target, repo, agent)` → tokens. The target is
  `{kind:"new"}` | `{kind:"task", id}`. `--task` is sent for a task, and
  `--repo` / `--agent` for new.

- [ ] **Tests:**
  - `composeTabModel` labels match Global Constraints.
  - `composeFormFor("interactive", {cron:"@daily", prompt:"p", ...})` has
    `cron === ""` (Review Focus 1).
  - `composeFormFor("schedule", …)` keeps `cron`, `tz`, `oneshot` and
    `resumeConversation`.
  - `composeFormFor("background", …)` keeps `oneshot` and `stream`.
  - `scheduleTargetTokens` for each target kind.
- [ ] Run → FAIL. **Implement.**
  - Tabs as buttons above Repo. The active tab is stored in `composeTab`.
  - The Open button's text is `composeTabModel(tab).label`, and its click
    builds from `composeFormFor`.
  - The 権限 `<details>` wraps the existing authority markup. Its `<summary>`
    is caps label + scope echo, updated on every authority change.
  - 録画など `<details>` holds PTY 記録.
  - The schedule target is a `<select>`:
    - "新規に作る".
    - Bindable tasks: ended, or `runMode === "background"`, labelled id8 +
      `repoTail` + About.
    - "id で指定", which shows an input resolved by Task 3's
      `resolveTaskIdInput`.
  - Remove Task id, Reattach, caps-on-resume and resume-conversation from
    Compose.
  - The command line's `ctx.resumeTaskID()` is deleted. `session new` resumes
    only on a typed `--resume`.
- [ ] `make js-test`, `make wasm-check`.
- [ ] **Live** (dummy `--known-psk`, Playwright): each tab's button does its
  action. A schedule with target new and with target task. A cron typed, then
  interactive chosen, opens a session and creates no schedule.
- [ ] Commit.

### Task 3: Acting on a task by id

**Files:** `webui/index.html` (beside `#task-filter-input`),
`webui/static/main.js`, `webui/static/task_models.js`, `cmd_test.mjs`.

**Interfaces — Produces:**
- `resolveTaskIdInput(input, tasks)` → one of `{kind:"one", task}`,
  `{kind:"many", tasks}`, `{kind:"none"}`, `{kind:"empty"}`.
  - The input is trimmed and lowercased, then matched as a prefix of `t.id`.
- `openTaskActionsDialog(t)` (main.js): a `<dialog>` holding the id-driven
  actions for `t`:
  - Reattach, when `taskSessionAlive`.
  - For an ended task: Task 4's direct-resume buttons and Resume….
- `taskResumeActions(t)` (main.js) → `[{label, run}]`. These are the sheet's
  existing four direct resumes (only Resume… for a background task), built once
  and used by both the sheet and this dialog.

- [ ] **Tests:** `resolveTaskIdInput`:
  - A full id → one.
  - A unique prefix → one.
  - A shared prefix → many.
  - An id not in tasks → none (Review Focus 2).
  - Whitespace → empty.
- [ ] Run → FAIL. **Implement.**
  - The field and the 開く button.
  - many → a candidate list in the dialog, each opening that task's actions.
  - none → "unknown task id" text beside the field.
  - The sheet's resume buttons are rebuilt from `taskResumeActions`.
- [ ] `make js-test`.
- [ ] **Live:** in activity order with two live sessions, paste an id: the
  dialog opens and stays put across polls.
- [ ] Commit.

### Task 4: Resume… dialog

**Files:** `webui/static/main.js` (`buildTaskSheet` resume block,
`resumeTaskById`, `prefillComposeFromTask` deleted), `webui/static/task_models.js`,
`webui/static/style.css`, `cmd_test.mjs`.

**Interfaces — Consumes:** Task 1 `resumeRegrantFlags` (through `sessionReq`),
Task 3 `taskResumeActions`.

**Interfaces — Produces:**
- `resumeDialogDefaults(t)` → `{ mode, stream, oneshot, runner, conversation, agent, cond }`.
  - mode ∈ `interactive | oneshot | background`, from `t.runMode` and
    `composeBoxesFromTask(t)`.
  - runner is `"assigned"` iff `t.assignedTo`, else `"any"`.
  - conversation is true.
  - cond is from `t.conditions`, as `prefillComposeFromTask` read it.
- `resumeDialogRequest(form, t)` → the object handed to `sessionReq`.
  - `caps`, `scope` and `scopeFor` are null unless `form.override` is open.
  - When it is open, they are the dialog's own state.
- `openResumeDialog(t)` (main.js): a `<dialog>`.
  - Its authority override is a `<details>` with its own `buildCapChips`
    state and scope controls, built the way `openRegrantDialog` builds them.
  - It offers caps and scope only.

- [ ] **Tests:**
  - `resumeDialogDefaults` for a background one-shot task, a stream task, and
    a task with no assigned runner.
  - `resumeDialogRequest` with the override closed → `caps` / `scope` /
    `scopeFor` null.
  - With the override open → the dialog's values.
  - A background mode with no prompt → error "prompt".
- [ ] Run → FAIL. **Implement.**
  - A background task's sheet offers only Resume….
  - `resumeTaskById` on a background task calls `openResumeDialog`
    (Review Focus 4). Otherwise it resumes directly as today.
  - A refusal shows its message in the dialog and keeps it open.
  - An ambiguous runner goes through `routeAmbiguous`.
  - The dialog is not a child of `#task-list` (Review Focus 3).
- [ ] `make js-test`.
- [ ] **Live:**
  - A direct Resume is one click.
  - Resume… in each mode.
  - Type a prompt, wait more than 5 s: the text is still there.
  - A background task from the notification feed opens the dialog.
- [ ] Commit.

### Task 5: TUI schedule form

**Files:** `tui/bgform.go`, `tui/actions.go` (`onBackgroundOpen`),
`tui/keys.go` (the `N` row's Scope and Long), `tui/app.go` (submit path),
`tui/bgform_test.go`.

**Interfaces — Produces:**
- `bgSchedule` purpose.
- `(*BgFormModel).OpenSchedule(repo string, targets []scheduleTarget)`.
- `(*BgFormModel).ScheduleArgs() ([]string, error)` → the argv after
  `schedule new`.
  - Built from cron, zone, repo or target, agent, prompt, the conditions,
    record, pty, one-shot and conversation.
- Submit parses those args with `verb.ParseCmdScheduleNew(verb.TUI, args, nil)`
  and dispatches through `tuiVerbs.ScheduleNew`. No second builder of
  `cli.ScheduleSpecOpts` (a test enforces that).
- `scheduleTarget{Label, ID string}`. `ID ""` = new task.

- [ ] **Tests:**
  - `N` with Schedules focused opens the form; its title contains
    "new schedule".
  - `N` with Tasks focused still opens the background form.
  - `ScheduleArgs` with target new → contains `--repo R --agent A --cron @daily`.
  - With a task target → contains `--task <id>` and no `--repo`.
  - An empty cron → error "cron".
  - The args parse with `ParseCmdScheduleNew`.
- [ ] Run → FAIL. **Implement.**
  - Cron / zone / one-shot / conversation / target fields exist only for
    `bgSchedule`.
  - The target is cycled over the listed targets plus a typed id field.
  - The `N` row becomes `Scope: scopeTasks | scopeSchedules` and
    `onBackgroundOpen` branches on focus. Its Long names both.
- [ ] Run `go test ./tui/` (env-scrubbed).
- [ ] **Live:** TUI in tmux: Schedules, `N`, fill, submit; the row appears.
- [ ] Commit.

### Task 6: Docs and landing

- [ ] **README:**
  - The WebUI Compose bullet: tabs, 権限 folded, the id field, Resume….
  - The WebUI command-line note: no Task id default, and typed
    `--caps` / `--scope` on a resume re-grant.
  - The TUI Schedules paragraph: `N`.
- [ ] Walk `surface-parity-checklist` for the moved controls and record it in
  the ledger.
- [ ] Commit, then land with `scripts/land.py` after the final review.
