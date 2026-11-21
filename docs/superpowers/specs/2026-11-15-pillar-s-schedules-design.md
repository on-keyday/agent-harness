# Pillar S — schedules (recurring starts)

Date: 2026-11-15. Line references are to `main` at `3b0c44e0`.

Implements pillar S of `2026-11-12-run-modes-and-records-grand-design.md`
(cited as **GD**, its rows as G1…G46). Builds on pillar A
(`2026-11-13-pillar-a-run-modes-design.md`, **PA**): background mode, run
conditions and `session new --oneshot` are landed. Where this spec reverses a
GD row, its Decisions table names the row and what moved it.

## Amendment — 2026-11-21: independent management and manual firing

This amendment supersedes the single-capability rules in S-D17 and S-D15,
and the corresponding GD G34/G37/G46 references. Management (new/edit/rm)
and manual firing (run) use separate capability bits, with the management bit
named `schedule_edit` (operator, 2026-11-21). The cron trigger and firing
algorithm remain as specified; manual-only triggers and per-run input are
still proposals.

- `schedule_edit` gates `new`, `edit`, `rm`, binding and foreground unbinding.
  It retains the former `schedule` bit, `0x20000000`.
- `schedule_run` independently gates `run`, with a new bit, `0x40000000`.
  Neither capability implies the other. Each action uses its own capability's
  scope over the bound task, or the creator while no task is bound.
- The S-D17a `spawn` / `resume` checks, stored grants, prompt redaction and
  S-D20 self-refusals remain in force. Run reach must not authorize edits or
  deletion.
- Reading through management reach still needs no capability. Builder's
  clarification after review: a holder of `schedule_run` may also list/show
  schedules within its run scope, so a run-only caller can discover its
  targets. An ungranted run bit's default scope does not widen read reach.
  A visible schedule can still be outside one action's scope; visibility
  does not promise permission for every action.
- The old CLI capability name `schedule` is rejected. Persisted numeric
  grants keep management access but lose manual firing unless explicitly
  re-granted `schedule_run`. A saved `all=0x3fffffff` does not gain the new
  bit; a newly granted `all=0x7fffffff` includes both. No automatic grant
  migration is performed: preserving management access without adding
  execution authority is intentional.

Validation: server tests cover both scope directions for list/show/run,
management denial through read-only reach, and no read widening when the run
bit is absent or both scopes exclude the target. The embedded worker skill
and both mirrors name the new capabilities. Dummy harness verification uses
actual runner-issued task tickets for edit-only and run-only callers.

### Surface audit for the capability split and review follow-up

The initial commit omitted the embedded skill and specification amendment;
this walk records those corrections and the read-reach follow-up.

| Item | Verdict | Evidence / reason |
|---|---|---|
| 1 | done | `cli/verb/table.go` new/run help names the separate bits. |
| 2 | n/a | No cross-flag validity change. |
| 3 | done | Existing buttons/keys retain the same shared Schedule requests. |
| 4 | done | `tui/schedules.go` run/delete requests meet server-side action gates. |
| 5 | done | `tui/authoritypicker.go` consumes the shared capability catalog. |
| 6 | done | WebUI capability controls consume that same catalog. |
| 7 | done | Schedule verb dispatch declarations remain shared in `cli/verb/table.go`. |
| 8 | n/a | No new bridge action or argument interpretation. |
| 9 | n/a | No session-default change. |
| 10 | done | Existing result renderers retain action-specific permission denials. |
| 11 | done | Schedule text rows use the server-filtered list in `cli/schedule.go`. |
| 12 | done | Schedule JSON uses the same list, without extra client filtering. |
| 13 | done | `whoami` capability names derive from the shared catalog. |
| 14 | n/a | No session-list field added. |
| 15 | done | `cli/verb/caps.go` lists/describes both bits independently. |
| 16 | n/a | No task-table field added. |
| 17 | n/a | No task-detail field added. |
| 18 | n/a | No runner-detail change. |
| 18a | n/a | No runner-table change. |
| 19 | done | Authority picker rows use the shared catalog, not a second bit list. |
| 20 | n/a | No task-row field added. |
| 21 | n/a | No task-sheet action added. |
| 22 | n/a | No dialog-prefill field added. |
| 23 | done | WASM capability catalog and schedule lists use shared Go/server data. |
| 24 | done | Create/edit/delete/unbind use edit; run uses run; read admits either applicable reach. |
| 25 | n/a | No optional field or presence-bit change. |
| 26 | n/a | No default application change. |
| 27 | done | Existing shared Schedule request builders remain the single path. |
| 28 | done | Numeric management bit retained; old all does not gain run; protocol fixture tested. |
| 28a | done | CLI/TUI/WASM use the same Schedule methods; no request field added. |
| 29 | done | Existing errors identify the denied capability; scope refusals remain no_such_schedule. |
| 30 | n/a | No result-routing change. |
| 31 | done | Run-only rows become discoverable; existing prompt-redaction rules remain. |
| 32 | done | Capability names still come from the protocol enum and shared ParseCaps. |
| 33 | done | Removed schedule name errors; action gates cannot silently accept missing bits. |
| 34 | n/a | No varying column set. |
| 34a | n/a | No new form control. |
| 35 | done | README capability and schedule sections updated, including compatibility. |
| 36 | done | Embedded supervising-workers skill and both mirrors updated together. |
| 37 | done | This amendment records the operator's reversal and GD references. |
| 38 | n/a | No screen-rendering change. |
| 39 | done | Pillar S Surfaces: CLI text/JSON, TUI table/detail/command line, WebUI dialog/Compose/command line retain their shared requests and server list/detail predicates. |

Agent-launch audit S1–S6 does not fire: no agent preset, argv, credentials,
launch environment or addressing contract changed.

## Amendment — 2026-11-22: manual firing accepts resume_exact

Decided by: operator, 2026-11-22. Source: decision D16 of
`2026-11-21-credential-tasks-design.md`. This supersedes S-D17a for
`schedule run` only.

- `schedule run` of a schedule bound to T needs `resume` OR `resume_exact`
  over T, with T inside the scope of a bit the caller holds (previously
  `resume` only). A caller holding neither answers the permission denial
  naming both bits (`resume|resume_exact`); T outside the scope answers
  `task_not_reachable`.
- Reason: the firer chooses only the timing. The prompt is the schedule's,
  and whoever bound T (`schedule new --task T` or an edit that binds T) or
  last edited a schedule bound to T already needed full `resume` over T.
- Unchanged: `schedule new`, `schedule edit` and binding still need `resume`
  (`resume_exact` is not enough, since they set the prompt a firing passes);
  a run while the task is still to be created needs `spawn`; an unbound
  schedule needs neither; S-D20 and reach are checked first.
- The rule applies to every principal, not only credential tasks.

Implementation: `scheduleRunPower` in `server/schedule_handler.go`, used only
by `scheduleRunCheck`; `schedulePower` now serves new and edit. Tests:
`TestSchedulePowerResumeExactRunsButDoesNotEdit`,
`TestScheduleRunExactOutOfScopeIsNotReachable`.

## Why

GD's second motive: work a machine starts on a schedule and that ends itself
(GD P7, G8). The first intended job is a repository watcher that triages new
issues and pull requests (GD G40). The operator wants firings at fixed clock
times, not only at intervals (operator, 2026-11-15).

## Goals

- A schedule fires a background run of its own task at the times a cron
  expression names, in a stated time zone.
- Definitions survive a server restart. A firing missed while the server was
  down runs once when it comes back (O3, S-D9).
- Every firing, skip and unbinding is visible with its outcome and its wait
  (GD G31), on the CLI, the TUI and the WebUI.
- Agents holding the `schedule` bit can create schedules, bounded by their own
  authority (GD G36, G37).

## Non-goals

- Event triggers (GD G40). The schedule's trigger is one field of a schedule,
  so an event trigger is a sibling of it later; nothing else here anticipates
  events.
- ~~A schedule running the stream kind. Firings open interactive sessions,
  `--oneshot` included (PA A-D29); `--stream` is refused at create.~~
  Withdrawn 2026-11-17: it reversed GD G1 without a Decisions row; see S-D23.
- Unlimited history. A schedule keeps its last 50 history entries (S-D7).
- Priorities or fairness among firings (GD anti-goal).

## Anti-goals

- **A schedule that silently stops.** Every reason a due firing does not start
  a run is a history entry naming that reason, shown on every surface.
- **A server crash from a schedule's input.** The cron expression comes from
  agents; parsing it must not be able to panic the server (Facts: F1).
- **A queue filled by one schedule.** At most one run per schedule is queued
  or running (GD G28).

## Facts

- **F1 — the cron parser panics on one input.** `github.com/robfig/cron/v3`
  v3.0.1 `ParseStandard("TZ=")` panics with a slice bound of -1
  (`parser.go:99`); measured 2026-11-15 with a Go fuzz test in a scratch
  module (`go test -fuzz FuzzParse`, seed corpus of ordinary expressions;
  the crash was found within a second). With inputs that start with `TZ=` or
  `CRON_TZ=` excluded, 6.3M fuzz executions over 120 s found no other panic.
  The module is MIT, imports only the standard library, has no `require`
  lines, and has no OSV advisory (queried 2026-11-15).
- **F2 — `Next` returns the zero time when nothing matches within five
  years** (`spec.go:87-92`), e.g. `0 0 30 2 *`.
- **F3 — the day-of-month / day-of-week OR rule is implemented.** Measured:
  `0 9 13 * 5` from Sunday 2026-11-15 08:59 JST gives Friday 2026-11-20
  09:00, not the next Friday the 13th.
- **F4 — a firing has an internal path.** `openBackground`
  (`server/background_open.go:51`) ends in `TaskStore.Resume(...)` with a
  `RunSpec{Mode: Background, …}` (`server/runmode.go:22`) followed by
  `h.OnChange()`, and `Scheduler.Tick` places it. No client connection is
  needed after the request is decoded.
- **F5 — only the latest run's facts are kept on a task.** `TaskEntry`
  carries one `EndReason` (`server/taskstore.go:165-167`) and one
  created/started time. A schedule's per-firing history must therefore keep
  its own copy of each run's wait and end (GD G31).
- **F6 — config-like server state is a JSON file replaced atomically.**
  `runnerkeys.go:177-180` and `runnerdials.go:206-209` write a temp file and
  `os.Rename` it; `LoadRunnerKeyStore` refuses to start on a file it cannot
  parse (`runnerkeys.go:58-59`).
- **F7 — the server already arms one timer on the earliest persisted
  deadline** (`watchHeldDeadlines`, `server/hold.go:527`). GD (§Pillar A)
  asks firings to follow that shape.
- **F8 — capability bits.** `all = 0x1fffffff` (29 bits,
  `runner/protocol/message.bgn:4895`). The `schedule` bit is the 30th, leaving
  2; the u64 widening is triggered by the NEXT bit proposed (pillar C's), not
  this one.
- **F9 — `ClientKind` is client↔server only** (`message.bgn:795`); it tags a
  caller in `ClientHello` and is echoed in `TaskInfo.resumed_by_kind`
  (`:1116`). A new value is not decoded by any runner.
- **F11 — the same library skips the spring-forward day.** For
  `30 2 * * *` in America/New_York its `Next` goes from 2027-03-13 02:30 to
  2027-03-15 02:30, so 2027-03-14 never fires (measured 2026-11-15 while
  implementing). With F1 this is why the library was dropped (S-D2).
- **F10 — the name `Scheduler` is taken** by the queue placer
  (`server/scheduler.go`). This spec's types are `Schedule…` and the firing
  loop is not called a scheduler.

## Decisions

**operator** = chosen by the human in conversation. **this spec** = the
author's call; those rows are the ones to check.

| # | Decision | Decided by |
| --- | --- | --- |
| S-D1 | **Times are a standard 5-field cron expression** (minute hour day-of-month month day-of-week, with lists, ranges, steps, month and weekday names, and the `@hourly`-style descriptors). An interval is written as a step (`*/30 * * * *`); there is no separate interval option, and `@every` is refused so that every schedule fires on clock boundaries | operator (fixed clock times; no interval option and `@every` refused, on this spec's proposal) |
| S-D2 | **A self-written parser and matcher; no cron dependency.** Reverses this spec's first choice of `robfig/cron/v3` (F1, F11): once S-D3a moved zones and DST to our side, the library's remaining work was parsing and matching on UTC, both small. Grammar, exactly: ≤ 256 bytes; a descriptor (`@yearly`/`@annually`, `@monthly`, `@weekly`, `@daily`/`@midnight`, `@hourly`; any other `@word` refused) or 5 fields — minute 0-59, hour 0-23, day-of-month 1-31, month 1-12 or `jan`…`dec`, day-of-week 0-7 (0 and 7 are Sunday) or `sun`…`sat`, names case-insensitive. A field is a comma list of `*`, `N` or `N-M`, each optionally `/S` (S ≥ 1; `N/S` runs N to the field's maximum); `N > M`, out-of-range values, empty items and any other character (`?`, `L`, `W`, `#`, a sixth field) are refused. Day rule (Vixie): if the day-of-month or the day-of-week field begins with `*`, a day must match both; otherwise either. `parseCron` also refuses an expression with no match within five years. Every path that reads an expression — create, edit, server start — goes through `parseCron` | operator (self-written, 2026-11-15, on this spec's proposal), this spec (the grammar) |
| S-D3 | **Each schedule has a time zone**, an IANA name (`--tz Asia/Tokyo`), defaulting to the server's local zone, resolved and stored as a name at create. A local zone with no IANA name the server can determine (a Windows host, or a copied `/etc/localtime`) is not stored as `Local`, which would follow the host's zone later: the create is refused and asks for `--tz`. The server embeds the zone database (`time/tzdata`, standard library, about 450 KB) so a name loads on a host without one, which a Windows server is. Every surface shows the next firing with its zone | this spec |
| S-D3a | **The expression is matched against local wall-clock time, and each matching day fires exactly once, never early.** `nextFiring` matches on the local wall-clock fields carried in UTC (which has no transitions), then maps the match back to an instant in the zone: a match inside a spring-forward gap fires at the end of the gap (the transition instant; New York 02:30 → 03:00 EDT, Berlin 02:30 → 03:00 CEST); a match inside a repeated hour fires once, at the earlier of its two instants. Neither can be left to `time.Date`, whose choice depends on the sign of the zone's offset (measured 2026-11-15: a gap time goes an hour early in New York and an hour late in Berlin; a repeated time takes the earlier instant in New York and the later in Berlin). A mapped instant not after `after` is skipped, and the search continues past a whole repeated hour rather than giving up | this spec |
| S-D4 | **Schedules are kept in `<data-dir>/schedules.json`**, rewritten whole and atomically (temp file + rename, mode 0600) after every change, as F6's stores are. The server refuses to start on a file it cannot parse, as `LoadRunnerKeyStore` does. The file has a `version` field (1) | operator (a file of its own, not WAL events), this spec (atomic rewrite, refuse on corrupt) |
| S-D5 | **One timer on the earliest `next_at` across schedules** (F7), re-armed after each firing and after every create, edit and delete. The timer is armed for at most one hour ahead, so a wall-clock jump or a host suspend delays a firing by at most that long | this spec |
| S-D6 | **A firing is a background resume of the schedule's task** through the same path `openBackground` uses (F4), with `ResumedByKind = schedule` (GD G45; new `ClientKind` value, F9), the schedule's prompt, profile, selector, extra args, `resume_conversation`, conditions, record, `--oneshot` and PTY choice, and a `GrantUpdate` that writes the schedule's stored authority onto the task (GD G36). `next_at` is then recomputed from the firing time, not from the due time, so one late firing does not cascade | GD G38/G45 (resume), this spec (the rest) |
| S-D7 | **History: the last 50 entries per schedule**, each with `due_at`, `at`, an outcome, and — for a run it started — `queued_at`, `started_at`, `ended_at` and the end reason (PA's `EndReason`), filled in as the task's events arrive (F5). Outcomes: `pending` and `lost` (S-D22), `fired`, `run_now` (S-D13), `skipped_busy` (GD G28), `skipped_no_task` (G46), `skipped_task_gone` (G38), `skipped_refused` (the resume was refused; the message is kept), `resumed_by` (a run of the task that the schedule did not start; the resumer's `ClientKind` is kept — not its task id, which `TaskEntry` does not keep (`ResumedByKind` only) — O9, S-D10), `unbound` (G46; who and when) | this spec |
| S-D8 | **The first firing creates the task.** GD G38 says the task is created with the schedule; this reverses that one clause. A task created at schedule-create time has no run, and the `TaskStatus` machine has no terminal state for "never ran" (GD non-goal: no change to it), so the first resume would be refused `ResumeErrNotTerminal`. Instead: at create, spawn limits are checked as a dry run against the creator (as `CreateWithinLimits` would decide, without creating); the first firing calls `CreateWithinLimits` in background mode with the schedule's run spec and binds the new id to the schedule; every later firing resumes it. A first firing refused by spawn limits is `skipped_refused`. Until then every surface shows the schedule's task as "created at first firing" | operator (both creating and binding, on this spec's proposal; reverses G38's "created with the schedule") |
| S-D9 | **Missed firings run once (settles GD O3).** On server start, a schedule whose stored `next_at` is in the past fires once immediately (subject to GD G28), its history entry carrying the old `due_at`; then `next_at` is recomputed from now. However many occurrences were missed, at most one runs | operator (on this spec's proposal) |
| S-D10 | **Runs a schedule did not start (settles GD O9):** listed in the history as `resumed_by` with the resumer, because a firing skipped as busy ran into them; not counted in the wait statistics, because their wait is not the schedule's | operator (GD's candidate, on this spec's proposal) |
| S-D11 | **Wait statistics:** over the `fired` and `run_now` entries in the history that have both `queued_at` and `started_at`, every surface that shows a schedule's detail shows the count, median and maximum of `started_at − queued_at`. Not stored; computed when shown | this spec |
| S-D12 | **Creating a schedule needs at least one candidate runner for its repo now** (GD G26), and refuses a missing deadline (GD G41), a missing prompt, a missing agent profile, a profile no candidate offers~~, and `--stream` (Non-goals)~~ — the same checks `openBackground` makes, through the same function where one exists, for the schedule's kind (S-D23) | GD G26/G41, this spec (reuse) |
| S-D13 | **`schedule run <id>` fires a schedule now**, outside its times, through the same firing function, recorded as `run_now`; `next_at` is not changed. It exists so a schedule set for 09:00 can be tried at once | operator (on this spec's proposal) |
| S-D14 | **Binding an existing task (GD G46).** `schedule new --task <id>` binds a task instead of creating one at first firing. The task must be within the caller's reach and must not be running in foreground (refused with that reason: end it or switch it with `session mode` first). Binding writes the task's mode to background. The repo is the task's; `--repo` with `--task` is refused | GD G46 (binding), this spec (refusing a running foreground task) |
| S-D15 | **Unbinding (GD G46).** A resume that asks for foreground, or `session mode … foreground`, on a task bound to a schedule requires the `schedule` bit as well as what it requires today, unbinds the task, and appends `unbound` (who, when) to the schedule's history. A schedule with no task shows as such on every surface and its firings are `skipped_no_task` until `schedule edit <id> --task <id>` binds one | GD G46 |
| S-D16 | **Authority (GD G35, G36).** A schedule stores an `authority.Grant` computed at create by the spawn rule (`TaskHandler.grantFor`, `server/task_handler.go:1283`) with the creator as granter, from the same `--caps / --scope / --scope-for / --delegate-*` flags `session new` takes: as for a new child of the creator (`grantFor(g, r, "")`) when the first firing will create the task, and as a resume over the bound task (`grantFor(g, r, T)`) for `--task T`, since scope bases are relative to the task that holds them — with every half of the request treated as present, so the stored grant is bounded by the creator even when no authority flag is given (a resume's "keep the recorded caps" default does not apply: otherwise binding a more privileged task would run the creator's prompt with that task's authority). **Every `schedule edit` recomputes it with the editor as granter**, whether or not it passes authority flags (GD G36 as written): otherwise a principal that may edit someone else's schedule could change its prompt and have it run with the original creator's grant. Each firing writes the stored grant onto the task | GD G35/G36, this spec (flags shared with `session new`, the two `grantFor` forms) |
| S-D17 | **The `schedule` capability bit** (GD G34) gates `schedule new`, `edit`, `rm`, `run`, and the unbinding in S-D15. Acting on a particular schedule (`edit`, `run`, `rm`) also needs the caller to reach its target: the schedule's task, or — while it has none — its creator task; `--task T` (on `new` or `edit`) needs T within reach. Reading (`schedule ls`, `schedule show`) needs no bit and the same reach; the operator reaches all. A schedule's prompt is shown only where the caller could read its task's prompt in `ls` | GD G34/G37, this spec (target and read rule) |
| S-D17a | **A schedule verb also needs the power it exercises.** `schedule` alone let a principal without `spawn` create a task (the first firing) and one without `resume` re-run a task (every firing of a bound task), which no decision intended. This stays inside GD G37: G37's "the schedule bit is the only gate" settled that agents and humans are gated alike, by capability rather than by an agent-specific rule; the rule below applies to every principal the same way. So, besides `schedule`: `schedule new` without `--task` needs `spawn`; `schedule new --task T` and an edit that binds T need `resume` with T in that bit's scope; an edit or a `schedule run` of a schedule needs what creating it would need now — `spawn` while its task is still to be created, `resume` over its bound task. A missing bit answers a permission denial naming the bit; T outside `resume`'s scope answers `task_not_reachable`. Firings check nothing: the creator may be gone, and the check was made when the schedule was written (GD G36's reasoning). Consequence: a scheduled task that holds `schedule` but not `spawn` can no longer leave a new schedule behind (S-D20's remaining gap) | operator (2026-11-15, on this spec's proposal) |
| S-D18 | **No `ClientHello` may claim `ClientKind.schedule`.** The server refuses such a hello; the value only appears as `resumed_by_kind` and `origin_kind` on tasks a schedule created or resumed | this spec |
| S-D19 | **Deleting a schedule leaves its task.** The task is an ordinary background task afterwards; a run in progress is not cancelled (cancel it to stop it) | this spec (GD G38: deleting the schedule is how it is stopped) |
| S-D20 | **A schedule's own task cannot act on its schedule**: `edit`, `run`, `rm`, a `--task` rebind and the S-D15 unbinding are refused when the caller is the bound task, whatever its grant holds. The first job reads untrusted issue and pull-request text; an injected instruction must not outlive the run by rewriting the prompt, the times or the binding. Same reasoning as PA's refusal of a task switching its own run mode (A-D19) | this spec |
| S-D21 | **A schedule's task state is stored, not derived from history**: `pending_create` (the first firing will create the task; a refused creation stays here and the next firing tries again), `bound` (with the task id), or `unbound` (S-D15; firings are `skipped_no_task`, and `schedule edit --task` binds again). A schedule created with `--task` starts `bound`; one unbound before its first firing is `unbound`, never `pending_create` | this spec |
| S-D22 | **A firing runs at most once.** Before it resumes or creates the task, the firing writes an intent to `schedules.json` — a history entry with outcome `pending`, the run number it expects (`TaskEntry.Run + 1`; `Run` is persisted on `task_resumed`, `server/taskstore.go:747-753`), or, for a creation, a task id chosen in advance — and advances `next_at`. Then it acts, then it rewrites the entry with the outcome. On server start — and, for one schedule, at the start of each of its firings, since a failed outcome save leaves the entry `pending` in memory — each `pending` entry is settled from the task store: the task's `Run` reached the expected number (or the pre-chosen id exists) → `fired`, and its run ends however replay ended it (a run left `Running` replays as `Failed`, `server_restart`, `server/taskstore.go:1612-1620`); otherwise → `lost`. A crash therefore never repeats a firing and never loses one silently, and a created task is never orphaned. Creating a task under a pre-chosen id needs a `CreateWithinLimits` variant that takes the id | operator (at most once), this spec (the intent record) |
| S-D23 | **A schedule stores a task kind, `interactive` or `stream`, and every firing creates or resumes its task as that kind** (`--stream` on `schedule new` / `edit`). This withdraws the Non-goal that excluded the stream kind: it reversed GD G1 (the run mode is independent of the data plane) without a row here, against this spec's own rule for reversing a GD row. Design and the as-built decisions: `2026-11-17-schedule-stream-kind-design.md` | operator (2026-11-17, citing GD G1) |

## Design

### The schedule

```
Schedule
  id            ScheduleID (16 random bytes, shown as 32 hex like a task id)
  trigger       { cron: "<expr>", tz: "<IANA name>" }      # S-D1, S-D3
  task          pending_create | bound(TaskID) | unbound   # S-D8, S-D14, S-D15, S-D21
  repo          string                                     # fixed at create
  agent         profile name
  selector      RunnerSelector                             # as session new
  prompt        string                                     # ≤ protocol.PromptMax
  extra_args    []string
  resume_conversation bool
  kind          interactive | stream                       # S-D23
  run           RunSpec (Mode = Background)                # PA: conditions, record, oneshot, NoPTY
  grant         authority.Grant                            # S-D16
  creator       TaskID (zero = operator)
  created_at, edited_at, edited_by
  next_at       time (UTC instant)                         # S-D5, S-D9
  history       last 50 entries                            # S-D7
```

`schedules.json` holds `{"version": 1, "schedules": [...]}` with these
fields. Times are stored as UTC instants; the zone is applied only when
computing `Next` and when displaying.

### Firing

One function, used by the timer, by S-D9's start-up pass and by
`schedule run`:

1. Task state `unbound` → `skipped_no_task`.
2. `bound` and the task id is not in the store → `skipped_task_gone` (GD
   G38: the schedule never recreates a task the operator removed; `restore`
   brings it back and the next firing runs).
3. `bound` and the task is not terminal → `skipped_busy` (GD G28).
4. Write the intent (S-D22): a `pending` entry with the expected run number,
   or for `pending_create` a task id chosen now; advance `next_at` (unless
   `run_now`); rewrite the file.
5. `pending_create` → create the task under the chosen id in background mode
   with the schedule's run spec and grant (S-D8), and on success set the
   state to `bound`. `bound` → `TaskStore.Resume` with S-D6's arguments.
   Either way then `OnChange`.
6. Rewrite the entry with the outcome — `fired` / `run_now`, or
   `skipped_refused` with the message (a refused creation leaves the state
   `pending_create`) — rewrite the file, re-arm the timer.

Skips (steps 1–3) write their entry and advance `next_at` in one rewrite;
nothing ran, so there is nothing to settle after a crash.

The firing function holds a schedule-store lock across steps 2–6, so two
firings of one schedule (the timer and `schedule run`) cannot both pass step 3.

History entries for a started run are completed from the task's status
events (assigned → `started_at`; ended → `ended_at`, end reason). A resume
of the task that the firing function did not make appends `resumed_by`
(S-D10).

### Verbs

All under one family, declared in `cli/verb/table.go` so the CLI, the TUI
command line and the WebUI command line parse them from one row each:

| Verb | Does |
| --- | --- |
| `schedule new --cron EXPR [--tz Z] (--repo R \| --task T) --agent A --prompt P --deadline D [run flags] [authority flags]` | create (S-D12, S-D14, S-D16) |
| `schedule ls [--json]` | one row per visible schedule: id, cron + zone, next firing, last outcome and age, task (id + status, or "created at first firing" / "no task"), agent, repo, prompt head |
| `schedule show <id> [--json]` | the definition, authority, the history (S-D7) and the wait statistics (S-D11) |
| `schedule edit <id> [any create flag] [--task T]` | change future firings only (GD G30); recomputes `next_at` when `--cron` or `--tz` changes |
| `schedule run <id>` | fire now (S-D13) |
| `schedule rm <id>` | delete (S-D19) |

"Run flags" are the flags `session new --background` takes for its run:
`--turn-end`, `--quiet`, `--blocked`, `--queue-timeout`, `--record`,
`--oneshot`, `--stream` (S-D23), `--pty` / `--no-pty`, `--resume-conversation`, `--agent-arg`,
`--runner` / selector, `--rows` / `--cols`. They are the same `Flag` rows,
not copies, so a later change to one reaches both verbs.

### Surfaces

- **CLI:** the verbs above. `ls` and the task detail name the schedule a task
  is bound to; a task the schedule CREATED (its first firing) reads
  `from=schedule`, while a task bound with `--task` keeps the origin it had and
  its firings show as `resumed_by=schedule`.
- **TUI:** a third table, Schedules, in the one-table-at-a-time layout
  (landed separately, 2026-11-15): the focus cycle becomes runners → tasks →
  schedules → notify → cmdresult → cmdline. Columns as `schedule ls`. `d`
  opens `schedule show`'s content in the detail popup; keys for run-now and
  delete (with a confirm); editing is by the command line. The task detail
  popup names the bound schedule.
- **WebUI:** a Schedules dialog listing the rows, with a detail view (history,
  wait statistics) and run-now / delete buttons; `schedule …` on the command
  line. Compose gains a cron + zone pair: when filled, Submit creates a
  schedule from the form's background fields instead of opening a session.
  The task row and sheet name the bound schedule.

The TUI table and the WebUI Compose fields were agreed with the operator on
this spec's proposal (2026-11-15). The `surface-parity-checklist` is walked for the new fields (the schedule on
a task, `from=schedule`) and the new verb family.

### Wire

New request and response formats in `runner/protocol/message.bgn` for the
verb family, a `ScheduleID` type, `ClientKind.schedule`, the `schedule`
capability bit, and a `schedule_id` on `TaskInfo` / `TaskStatusEvent` for the
bound schedule. Constraints the formats must meet:

- **Every control message is one datagram** over UDP; a variable-length field
  must be bounded and tested or streamed. The prompt travels on a payload
  stream as an open's does (PA A-D17, `readOpenPrompt`); `schedule ls` and
  `schedule show` answer on a send stream as `ls` does.
- No runner-decoded format changes, so the runner is not on the upgrade path.

## Approaches considered

- **How a time is written** — an interval only (`--every 30m`), a cron
  expression, or both. Interval-only needs no dependency and no zone but
  cannot say "weekdays at 09:00"; both doubles every surface. The operator
  wants fixed clock times; cron also writes intervals as steps, so cron alone
  (S-D1).
- **Who parses cron** — `robfig/cron/v3`, or a hand-written 5-field parser.
  The library handles the OR rule and month ends (F3), but it was last
  released in 2020, panics on one input (F1) and skips the spring-forward
  day (F11). The library was chosen first (operator, after the F1 check);
  F11 moved zone handling to our side (S-D3a), which left the library only
  parsing and UTC matching, so it was dropped for a self-written parser with
  a grammar stated in full (S-D2) and a matcher checked against a
  minute-by-minute brute force (operator, 2026-11-15).
- **Where schedules are persisted** — their own JSON file, or new event types
  in the task WAL. The WAL would keep unbounded history and reuse replay, but
  `WALEvent` is a task record and every WAL reader would meet the new types;
  a file matches the other config-like state (F6). Chosen: the file
  (operator).
- **A crash in the middle of a firing** — at most once (the intent record,
  S-D22) or at least once (resume first, record after). At least once
  repeats the firing after a crash: a run left `Running` replays as `Failed`,
  so the task is terminal again with `next_at` not advanced. It also orphans a
  task created by a first firing. Chosen: at most once, with the lost firing
  recorded (operator).

## Left to the builder

- Field names, flag spellings beyond those above, the `.bgn` layout of the
  new formats, the TUI column widths, and the WebUI dialog's markup.
- Where the firing function lives (a `ScheduleStore` plus a loop on
  `Server`), provided the name does not reuse `Scheduler` (F10).
- How history entries are matched to task events (by task id and the run's
  resume time is sufficient: one run per schedule at a time, GD G28).

## Testing

- `parseCron`: `TZ=`, `CRON_TZ=UTC 0 0 * * *`, `@every 1h`, `0 0 30 2 *`
  refused; a fuzz test asserting no panic over arbitrary input; the matcher
  equal to a minute-by-minute brute force over generated expressions.
- Firing: each outcome of S-D7 from a store in the matching state, including
  two concurrent firings of one schedule producing one run and one
  `skipped_busy`.
- S-D9: a store loaded with `next_at` two days in the past fires once, with
  the old `due_at`, and the next `next_at` is in the future.
- S-D8: the first firing creates the task with the schedule's authority and
  run spec; a second firing resumes it; spawn limits checked at create.
- S-D15: a foreground resume of a bound task without the `schedule` bit is
  refused; with it, the task is unbound and `unbound` recorded.
- Persistence: create, restart (new store from the file), every field equal;
  a corrupt file refuses start.
- S-D22: a `pending` entry whose task's `Run` reached the expected number
  settles as `fired`; one whose task did not, as `lost`; a `pending` creation
  whose pre-chosen id exists binds that task; neither case fires again.
- S-D20: each verb refused when the caller is the bound task, even with
  `schedule` and the task in its own scope.
- S-D16: an edit by a principal narrower than the creator, with no authority
  flags, leaves the stored grant bounded by the editor.
- S-D21: a schedule unbound before its first firing records
  `skipped_no_task` and creates nothing; a refused first creation retries at
  the next firing.
- A dummy-harness run: a schedule `* * * * *` on the fake agent fires within a
  minute, its run ends by its deadline, `schedule show` shows `fired` with a
  wait; the TUI table and the WebUI dialog show it.
