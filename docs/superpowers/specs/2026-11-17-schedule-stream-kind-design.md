# A schedule can run the stream kind — short design

Status: approved by the operator in conversation, 2026-11-17.

## Intent

GD G1 (`2026-11-12-run-modes-and-records-grand-design.md:77`, decided by the
operator) makes the run mode independent of the data plane (PTY / stream
kind). Pillar S excluded the stream kind from schedules from its first commit
(`2017f725`, Non-goals) with no Decisions row, although its own header requires
one for every GD row it reverses; the code followed (`stream_refused` removed
in `8fbd1d96`, Ruling R16; `scheduleOff["stream"]`). The operator never made
that exclusion and wants it withdrawn (operator, 2026-11-17). Backlog entry:
"A schedule cannot run the stream kind, against GD G1".

## Facts the design rests on

- The server's background open is already kind-agnostic: `openBackground`
  (`server/background_open.go`) takes the kind from the request and passes it
  to `checkBackgroundRun`, `checkConditions`, `RunConditions.WithDefaults`
  (stream: turn-end `any` → `lifecycle`) and `TaskStore.Resume`.
- The schedule code pins `TaskKind_Interactive` in four places:
  `server/schedule_handler.go` (`WithDefaults`, `checkBackgroundRun` in the
  spec overlay) and `server/schedule_fire.go` (`CreateWithinLimitsID`,
  `Resume`).
- `TaskStore.Resume` sets the task's kind (`e.Kind = newKind`): a task may
  change kind between runs, as `session new --resume X --stream` does.
- The open refuses stream with `--oneshot` (`OneshotStream`) and with `--pty`
  (`PtyStream`) (`server/task_handler.go:1671-1680`); a stream run stores
  `NoPTY=false` (`resolveNoPTY`).
- Schedules persist as `<data-dir>/schedules.json`, not in the WAL.

## Decisions

| # | Decision | Decided by |
|---|---|---|
| K1 | A schedule stores a task kind, `interactive` or `stream`; every firing creates or resumes the task as that kind. | operator (G1; withdraw the exclusion, 2026-11-17) |
| K2 | **Wire: the enum, not a bit.** `ScheduleSpec` gains `kind_present :u1` (from `reserved`, which goes u4 → u3) and `if kind_present == 1: kind :TaskKind`. `ScheduleDetail` gains `kind :TaskKind` (one byte; a layout change on a server↔client format, which no runner decodes). | operator (enum over `stream_set`/`stream` bits, 2026-11-17) |
| K3 | The server refuses a kind other than `interactive` / `stream` (the retired `oneshot`, an unknown value) with a new status `kind_unsupported`, appended to `ScheduleStatus`. | this spec (the cost of K2) |
| K4 | Combination checks mirror the open, applied to the spec AFTER the edit overlay: kind `stream` with `oneshot` → new status `oneshot_stream`; with `pty` set on → new status `pty_stream`. A stream schedule stores `NoPTY=false`. | this spec (same rules as `session new`) |
| K5 | An edit may change the kind (`--stream` / `--stream=false`), like any run setting; the conditions are re-defaulted for the new kind (`WithDefaults(kind)`), and `checkBackgroundRun(kind, …)` judges the result, so a turn-end the stream kind lacks (e.g. `program`) is refused rather than kept. | this spec |
| K6 | `--task` binding: the kind is what the spec says (`--stream` or not), not the bound task's current kind — the rule `session new --resume X` already follows. A create with no kind given is `interactive`. | this spec |
| K7 | Persistence: `schedules.json` gains `"kind"` (the enum's wire name). A record without it reads as `interactive`, which is what every existing schedule is. | this spec |
| K8 | Surfaces: `--stream` on `schedule new` / `edit` (drop `scheduleOff["stream"]`; edit carries presence so "not named" ≠ false; `scheduleExclusive` gains stream×oneshot and stream×pty); the TUI schedule form gains a stream toggle; the WebUI Compose schedule tab reads its existing `stream` checkbox; `schedule show` prints the kind on all three. | this spec (G-wide: features span all three UIs) |
| K9 | Pillar S spec: strike the Non-goals line and add a Decisions row recording the withdrawal, citing G1. | operator |

## Approaches

- **Enum `kind :TaskKind` (picked, K2).** The field says what it is and
  matches `TaskInfo.kind`; a future kind needs no new shape. Costs K3's refusal.
- **Two bits `stream_set` / `stream`.** Only valid states are expressible and
  it matches `OpenInteractiveRequest.event_stream`, but a bool would stand in
  for an enum. Rejected by the operator.

## Non-goals

- Changing `OpenInteractiveRequest.event_stream` to the enum — recorded as its
  own backlog entry.
- Any change to how a background stream run is placed or ended.

## As built (2026-11-17)

- **The zero value is guarded in memory too, not only on disk.** `Schedule`
  is a Go struct and `TaskKind`'s zero is `oneshot`, so every read goes
  through `Schedule.runKind()` (stream only when the kind is stream, else
  interactive): a `Schedule` built without a kind fires interactive.
  `schedules.json` writes `"kind"` for every schedule, by name.
- **The edit's field copy** (`apply` in `server/schedule_handler.go`) copies
  fields by name; it now copies the kind (a test caught it missing).
- **`schedule show` prints `pty: none` for a stream schedule**: the stored PTY
  bit of a stream run is false (`resolveNoPTY`), which the detail otherwise
  reads as "on".
- **The edit's result line names `stream`** (`scheduleChangeList`).
- **A kind switch round-trips** (whole-branch review). K5 first stored the
  stream defaults over the interactive settings, so stream → interactive left
  turn-end `lifecycle` and a PTY the operator had turned off. Now:
  - the stored PTY bit is not touched by a switch; the firing builds a stream
    run with no PTY (`schedule_fire.go`), and `schedule show` prints
    `pty: none` for stream;
  - a switch with no `--turn-end` takes a stored turn-end equal to the old
    kind's default (`RunConditions{}.WithDefaults(old)`) back to unset, so the
    new kind's default applies. An explicitly chosen non-default turn-end is
    kept and judged for the new kind. Edge: an interactive schedule set
    explicitly to `lifecycle` reads as the stream default after a switch to
    stream and back, and returns as `any`.
  - K4's `--pty` check applies to the request only: the store keeps no "pty
    was named" bit, so a stored PTY cannot be told from the interactive
    default; it stays stored and is ignored while the kind is stream.
- Five existing guards encoded the old exclusion and changed with it:
  `TestScheduleRefusesWhatAFiringCannotCarry`, `TestScheduleFlagsAreSpawnFlags`,
  the WebUI parse-error case in `schedule_test.mjs`, `composeFormFor` in
  `cmd_test.mjs`, and a throw in `composeScheduleTokens`.
- Live check (dummy harness, fake agent): a schedule created with `--stream`
  from the CLI, the TUI form and the WebUI form each reads back
  `kind: stream`; `schedule run` creates a `stream` task. The fake agent has
  no stream adapter, so that task fails the same way a one-off
  `session new --background --stream` does on it; a run to completion needs
  `--agent claude`, which was not done.

## Left to the builder

- Status message wording; the TUI toggle's key and label; where the kind sits
  in each `schedule show` rendering.
