# Compose: one action per button — design

Status: approved by the operator in conversation, 2026-11-16.

## Intent

The WebUI's Compose form is hard to read, and one of its behaviours is the
opposite of what a reader expects:

- The single submit button is labelled "Open interactive session".
- What it actually does depends on other fields: interactive, one-shot,
  background, event-stream, a resume (when Task id is filled) or
  `schedule new` (when cron is filled). The operator called the cron case
  "maximally surprising".
- The capability chips (33 of them) take the top three rows. The rest of the
  authority panel sits below the prompt. Resume options sit above the prompt,
  while the Task id they apply to is at the bottom. Unrelated settings
  (background, one-shot, PTY record, PTY, cron, zone) share one row.

How the operator actually uses Compose today:

- **Its resume.** Only because the prompt can be written nowhere else. The
  sheet's Resume buttons take no prompt, and a background task's Resume sends
  the operator to Compose (`prefillComposeFromTask`).
- **Its Task id.** As a stationary place to act from. An activity-ordered list
  reorders on every poll, so the operator copies a task's id (the sheet's
  `Copy id`), pastes it into Task id, and acts from there instead of chasing
  the moving row.
- **Its args field.** To change settings when launching into a sandbox, so it
  stays.
- **The sheet's direct Resume buttons.** One click and the operator is in;
  that stays.

The TUI has no form for creating a schedule; it is the command line's
`schedule new` only. The pillar S spec scoped it that way without a stated
reason.

## Goals

1. **The action is chosen explicitly.** The button names the action.
   - No field's content changes what a button does.
2. **Compose only creates.** It has one tab per action: interactive, one-shot,
   background and schedule.
   - Each tab shows the fields its action reads, and its button names it
     ("session を開く", "one-shot を実行", "background で投入",
     "schedule を作る").
3. **A resume with a prompt happens in a dialog** opened from the task's sheet,
   never through Compose. The direct one-click Resume buttons stay.
4. **A task can be acted on by pasted id** from a stationary place, replacing
   Compose's Task id.
5. **Authority is folded.** It shows one summary line by default (`caps: none ·
   scope: subtree`), and the existing chips, scope rows and disclosures appear
   when the line is opened.
6. **The TUI can create a schedule from a form,** opened by `N` in the
   Schedules table and labelled "new schedule".

## Non-goals

- **No change to what the forms send.** Every combination the server accepts
  today stays reachable, including background + one-shot, background +
  event-stream, and schedule + one-shot / PTY / conversation resume. The one
  intended change on the wire side is in the WebUI command line (see
  Constraints).
- **No change elsewhere:** the task list, the Schedules dialog's list and
  detail, the re-grant dialog and the terminal view stay as they are.
- **No TUI change beyond the schedule form.** Its `s` / `S` / `N` are already
  one action per key.
- **No new resume semantics.** Assigned vs any runner, conversation or not, and
  the A-D7 / A-D29 rules (a resume reuses nothing of the last run) are kept.

## Anti-goals (failure even if it works)

- A button whose effect depends on a field elsewhere. That includes a tab whose
  button changes with a checkbox another tab set.
- An extra click on a resume that needs no input (operator, 2026-11-16).
- A form typed into inside the task list: the list moves and is rebuilt on
  every poll.
- A compatibility path for an older page or wasm. A stale artifact is an
  accident to surface, not a case to handle (operator, 2026-11-16).
- Restyling surfaces that were not asked about.

## Constraints

- **Features span CLI / TUI / WebUI** (repo norm). The CLI is unchanged: it
  already has one verb per action.
- **The WebUI command line takes Compose's values for flags it was not typed
  with.** This is in the README WebUI section: repo, host, agent, args, Resume
  task id, and the authority panel.
  - Removing Compose's Task id removes that default: a typed `session new`
    without `--resume` never resumes. (My call: a resume is chosen on the sheet
    or typed, never inherited silently.)
  - Removing "apply caps/scope on resume" changes a typed
    `session new --resume X --caps … / --scope …`.
    - Today those flags take effect only while that checkbox is ticked
      (`main.js` `resumeCapsOverride` / `scopePresent`). That contradicts
      `cmdline.js`'s own rule ("a resume re-grants only what was typed") and
      the CLI.
    - After this change a typed `--caps` / `--scope` / `--scope-for` on a
      resume re-grants exactly what was typed, and an untyped one re-grants
      nothing, as the delegation bound and spawn limits already behave.
    - (My call, an authority behaviour change on this surface, to be stated to
      the operator.) The README note is updated.
- **`surface-parity-checklist`** must be walked for the moved controls (resume
  options, caps-on-resume, event-stream, cron/zone, Task id).
- **No local identifiers** in the doc or tests; the repo is public.

## Approaches (decided in conversation)

- **Compose shape:**
  - (a) Action tabs at the top, each showing only its fields, with a button
    named for the action.
  - (b) One form with one button per action, every field always visible.
  - **Operator chose (a).** (b) leaves the clutter in place.
- **Where a resume's prompt is written:**
  - (i) On the task's sheet.
  - (ii) A "resume <id>" tab in Compose.
  - **Operator chose (i).** The complaint is being sent elsewhere to write a
    prompt.
  - A form *inside* the sheet would move with the activity order and be wiped by
    the 5 s rebuild, so (i) is realised as a dialog opened from the sheet.
    (Operator agreed the stationary-target concern.)
- **How Compose is rebuilt** (my call):
  - The same page, vanilla JS, and the same request builders (`sessionReq`,
    `composeRunMode`, `composeScheduleTokens`).
  - The markup is regrouped and visibility is decided by the active tab.
  - No alternative is worth weighing: the page has no framework, and the
    builders already encode the request rules.

## Design

### WebUI Compose

```
Compose
[ interactive ] [ one-shot ] [ background ] [ schedule ]
Repo [..........v]  Host [....v]  Agent [....v]
args [ --settings ... ]
Prompt [                                   ]
 background / schedule:  deadline [1h] turn end [any v] quiet [] blocked [] queue []
 schedule:               cron [@daily] zone [Asia/Tokyo v]  target [新規に作る v]
 (per tab, see below):   [ ] one-shot で  [ ] event-stream (チャット)  PTY [既定 v]
▸ 権限  caps: none · scope: subtree
▸ 録画など  PTY 記録 [既定 v]
                              [ session を開く ]
```

**Rows shared by every tab:** Repo / Host / Agent, args, Prompt.
- The prompt is optional on interactive and required on the other three (as
  today).

**Tab-specific fields.** The tab is the primary choice. Modifiers the server
accepts in combination stay as checkboxes on the tabs that allow them.
- **interactive:** event-stream, and PTY.
- **one-shot:** PTY.
- **background:** the conditions block, plus one-shot, event-stream and PTY.
  - The existing rules about which combinations are refused (e.g. a stream's
    allowed turn-end values) stay where they are enforced today.
- **schedule:** the conditions block, cron, zone, **target**, plus one-shot,
  PTY and "会話も再開" (each firing's `--resume-conversation`).
  - ~~Event-stream is not offered: `composeScheduleTokens` refuses it today.~~
    Superseded 2026-11-17: the schedule tab reads event-stream and sends
    `--stream` (pillar S S-D23, `2026-11-17-schedule-stream-kind-design.md`).

**The schedule target:**
- A select:
  - "新規に作る" (the default): the first firing creates the task, which needs
    Repo and Agent.
  - One of the listed tasks a schedule can bind (ended, or live in
    background), labelled id8 + repo tail + About.
  - "id で指定": a text field taking a full id or unique prefix, resolved as in
    "Acting on a task by id".
- Choosing a task sends `--task` and hides Repo (the task's repo is used).

**Folded sections, closed by default:**
- **権限:** the existing chips, scope rows and disclosures, unchanged inside.
  - The summary line is the existing scope echo plus the caps label.
- **録画など:** PTY 記録.
  - The tab-level PTY select stays on its tab, because it changes what runs.

**The button** names the tab's action, and the tab is the only input deciding
which action runs. Tab-specific fields of the tabs not shown are not read.

**Tab memory:** the selected tab is remembered per browser (localStorage), like
the task order.

**Removed from Compose:** Task id + Reattach, "apply caps/scope on resume",
"resume the agent's conversation too" (it moves to the schedule tab and the
resume dialog), and the cron-ticks-Background coupling (`composeCronState`,
`composeCronSync`, `composeCapsOnResumeState`).

### WebUI: acting on a task by id

**Why:** the operator acts on a task by pasted id when the list is moving.

**The control:** an `id [____] [開く]` field sits beside the task-list filter.
- It takes a full id or a unique prefix.
- It opens a stationary **task actions dialog** for that task, holding the
  id-driven actions:
  - Reattach, for a live session.
  - The four direct Resume buttons and Resume…, for an ended task.
  - The same set of actions the sheet offers for it.

**Other outcomes:**
- A prefix matching several tasks opens none and lists the candidates (id8 +
  repo tail + About) to pick from.
- An unknown id says so.

### WebUI task sheet: Resume…

**The direct buttons stay.** The four Resume buttons (assigned / any runner ×
with / without conversation) keep resuming in one click, as today. So does the
notification feed's quick resume (`resumeTaskById`) for a non-background task
(operator, 2026-11-16).

**Added:** **▶ Resume…**, for a resume with a prompt, args, a mode or an
authority override. It opens a dialog for that task.

**Background tasks:** a background task shows only Resume…, because its resume
needs a prompt and a deadline. This replaces today's jump to a prefilled
Compose. The same applies to the feed's quick resume of a background task, and
to every other caller of `prefillComposeFromTask`, which is removed.

**Why a dialog:**
- The sheet moves with its row in an activity-ordered list.
- `renderTaskList` rebuilds every sheet on each poll
  (`taskList.innerHTML = ""`).
- A dialog is outside the list. It follows the sheet's existing
  `🔑 caps/scope 再付与` dialog.

```
▶ Resume abcd1234…
  mode   ( ) interactive  ( ) one-shot  ( ) background
         [ ] event-stream            (interactive / background)
         [ ] one-shot で              (background)
  runner ( ) assigned (where it last ran)  ( ) any runner
  [x] 会話も再開
  Agent [task's own v]   args [ ... ]
  Prompt [                       ]
  background: deadline [1h] turn end [..] quiet [] blocked [] queue []
  ▸ 権限を上書き   (closed: nothing is re-granted)
                         [ 再開する ]   [ 閉じる ]
```

**Defaults:**
- The mode and its modifiers are the task's last run, taken from
  `composeBoxesFromTask` and the run mode.
- The background conditions are prefilled from the last run, as
  `prefillComposeFromTask` did.
- Runner is "assigned" when the task has an assigned runner; otherwise "any"
  (the "assigned" option is then absent).
- Conversation is on.
- Agent is the task's own profile.
- Args is empty.

**The authority override:**
- It has its own state, separate from Compose's. It is built from the
  components the re-grant dialog already instantiates per dialog: caps chips
  through `buildCapChips`, and the scope controls.
- It offers **caps and scope only**, what today's "apply caps/scope on resume"
  re-grants. The delegation bound and the spawn limits are not offered on a
  resume here. They stay command-line-only and re-grant only when typed, as
  today.
- Closed sends no re-grant (`resumeCapsOverride` and `scopePresent` false).
  Open sends both from the dialog's own state.

**The button** sends the same `sessionReq` the direct buttons send, plus the
prompt, the mode, the modifiers and the conditions.
- A refusal keeps the dialog open with what was typed.
- An ambiguous runner goes to the existing runner picker (`routeAmbiguous`).

### TUI: new schedule form

- **Opening:** `N` in the Schedules table (free there; in the Tasks table `N`
  opens a background session) opens a form titled "new schedule".
- **Fields:**
  - The background form's fields (prompt, agent, deadline, turn end, quiet,
    blocked, queue timeout, record, pty).
  - Plus cron, zone (empty = the server's zone), repo (prefilled with the
    `--repo` the TUI was started with), one-shot (yes/no), conversation resume
    (yes/no), and target.
  - Target is "new task", a bindable task picked from a list, or a typed
    id / prefix.
- **Submit:** sends `schedule new` with those flags through the existing
  schedule client call.
  - On success the form closes, and the Schedules table shows the row on its
    next refresh.
  - A refusal keeps the form open, as the background form does.
- **Reuse:** the form is `BgFormModel` with a fourth purpose (my call), so the
  background-shaped forms cannot drift.

## Verification

- **JS tests** (`cmd_test.mjs` style, pure models):
  - The tab → visible fields → action/label model.
  - Fields of hidden tabs are not read.
  - The schedule target → tokens (`--task` vs `--repo` / `--agent`).
  - The resume dialog → `sessionReq` fields, with the authority override closed
    vs open.
  - The command line: `session new --resume X --caps c` re-grants caps, and
    without `--caps` it re-grants nothing.
  - The id field: a full id, a unique prefix, an ambiguous prefix, an unknown
    id.
- **TUI tests:**
  - `N` in Schedules opens the schedule form.
  - Submit builds `schedule new` with the typed fields, for each target kind.
- **Live:**
  - Dummy harness (`--known-psk`) in Playwright:
    - Each tab does what its button says.
    - A schedule with each target.
    - A direct Resume is still one click.
    - Resume… in each mode.
    - The id field opens the actions dialog while the list re-sorts.
  - TUI in tmux: `N` → create → the row appears.
- **Parity:** the `surface-parity-checklist` walked and recorded.

## Left to the builder

- Markup details, CSS, and the exact Japanese labels beyond those above.
- Which tasks the target picker lists when many qualify (order and filter).
- How the TUI target picker is drawn (an inline list or a sub-picker).

## Amendment (as built, 2026-11-16)

- **The heading.** The section is titled 新規作成, not Compose (operator,
  2026-11-16). The element id stays `#compose`.
- **The TUI target.** It is a typed field only: empty means a new task; an id
  or a unique prefix binds a listed task. The list to pick from was not built.
  Every value on that form is a typed field, and a cycling picker inside a
  textinput form would fight its key handling.
- **The id dialog's actions.** "The same set of actions the sheet offers" is
  narrowed to the id-driven ones:
  - Reattach, for a live session.
  - The one-click resumes and Resume…, for an ended task.
  - A note, for a Queued or Held task (it can do neither).

  Files, re-grant and the other sheet actions stay on the sheet.
