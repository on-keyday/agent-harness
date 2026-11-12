# Pillar A — run modes, PTY records, retiring oneshot

Date: 2026-11-13. Line references are to `main` at `b3959555`.

Implements pillar A of
`2026-11-12-run-modes-and-records-grand-design.md` (cited below as **GD**,
its rows as G1…G44). This spec stays inside GD's decisions; where it settles
something GD left to it, the row says so. It lands in three stages (G44):
**A2 background mode** first, then **A1 PTY record**, then **A3 retiring
oneshot**. A2 is specified in full here. A1 and A3 are specified to the
depth their order requires: each gets its decisions settled before its plan
is written, and the questions still open for them are listed in §A1 and §A3.

## Why

GD's intent, restated for the reader who has not read it: work a machine
starts and nobody watches must be able to wait for a free runner, run an
agent with a prompt it was given up front, and end itself on conditions —
then the oneshot kind, whose queue is the only part of it anyone needed, can
be removed. The first intended job is a repository watcher that triages new
issues and pull requests on a schedule (operator, 2026-11-13); pillar S
builds the schedule, A2 builds what each firing runs.

## Facts this spec builds on

Read on `b3959555`; each was traced to the handler, not inferred.

- **F1 — an interactive open never queues.** `handleOpenInteractive`
  (`server/task_handler.go:1723-2098`) picks the runner itself
  (`Registry.Candidates`, `:1816`), refuses a full one with `RunnerBusy`
  (`:1893-1896`), creates the task (`CreateWithinLimits` `:1931` or
  `Tasks.Resume` `:1914`), then marks it Running (`Tasks.Assign` `:1947`)
  and reserves the slot (`BindTask` `:1948`, return value ignored). It then
  sends `OpenExec` and builds the `SessionMux` (`:1986-2090`).
- **F2 — the queue only knows oneshot.** `Scheduler.Tick`
  (`server/scheduler.go:76-116`) walks every Queued task, and its `assign`
  is `sendAssign` → `AssignTask` (`server/server.go:1741-1770`), which the
  runner runs through `handleAssign`, the oneshot path. `AssignTask` carries
  no kind (`runner/protocol/message.bgn:280-309`).
- **F3 — a latent race, pre-existing (the walk before pillar 0 did not filter
  by kind either).** Between F1's create (status Queued, `OnCreate` fires)
  and its `Assign`, no scheduler lock is held, and `Tick` does not filter by
  kind. A `Tick` landing in that window would send `AssignTask` for an
  interactive task, which the runner would run as a oneshot with an empty
  prompt while `OpenExec` also starts it. Not observed; read from the code.
- **F4 — no end reason exists.** Only free-form `ErrorMsg` and the exit
  code. A cancelled PTY task is reported by the runner as exit 0 with no
  message (`runner/session.go:1220-1226`), and `TaskStore.Finish` turns
  Cancelled into Succeeded for exit 0 (`server/taskstore.go:846-858`). So
  today every cancelled interactive task ends `Succeeded`.
- **F5 — a server cancel is an immediate SIGKILL of the child process.**
  The per-task context is cancelled (`runner/hold.go:302-320`); the PTY
  child was started through go-pty's `CommandContext`, whose default
  `Cancel` is `Process.Kill()` on the child pid alone, not its group
  (go-pty `cmd_unix.go:24-36`). The SIGHUP→SIGTERM→SIGKILL ladder in
  objtrsf `exec/exec.go:381-390` runs only when the child's stdin stream
  ends. Two comments say a cancel meets the ladder or that "procTree
  signalled the group" (`server/dispatch.go:183`,
  `runner/session.go:1187-1189`); both are wrong for the PTY path.
- **F6 — the idle evaluator.** `evaluateIdle` (`server/idle_signal.go:100-121`)
  checks program → lifecycle → quiet; `legSatisfied` (`:58-68`) implements
  await-idle D6/D18 with `muxStart` as the floor. It treats lifecycle
  `waiting`/`error` and program `blocked`/`error` as satisfying. Watchers
  are memory-only goroutines on a `SessionMux` (`ArmIdleWatcher`,
  `server/session_mux.go:1293-1323`, 500 ms tick).
- **F7 — after re-adoption a mux starts over.** `rebindHeldSessions`
  (`server/readopt.go:177-258`) builds a new `SessionMux` (new `muxStart`,
  zero `lastInput`/`lastOutput`) and loads the held screen capture, whose
  repaint includes the 7501 program state, stamping `programAt` after
  `muxStart` (`session_mux.go:435-437`). Lifecycle survives a deliberate
  restart only through the progress snapshot, without `LifecycleWorkingAt`
  (`server/taskstore.go:465-472`).
- **F8 — one deadline timer exists, shaped for a fixed set.**
  `watchHeldDeadlines` (`server/hold.go:444-460`) runs a startup pass, sleeps
  to the earliest deadline, and exits when none remain; nothing re-arms it
  for a deadline added later.
- **F9 — a detached open can already be sized.** `initial_rows`/`initial_cols`
  ride `OpenInteractiveRequest` when `detached` (`message.bgn:2388-2390`) and
  the server applies them at open (`task_handler.go:2066-2076`). Without
  them the PTY has no size until a client sends one.
- **F10 — the open request has 5 spare bits** (`reserved2 :u5`,
  `message.bgn:2380`) and no prompt field. `OpenExecRunnerRequest`
  (`message.bgn:324-346`, decoded by the runner) has no prompt either.
- **F11 — argv templates.** `{args}` and `{prompt}` are the only
  placeholders (`runner/agent_command.go:6-7`); interactive fresh and resume
  templates reject `{prompt}` (`:70-95`). Preset interactive templates are in
  `scripts/agent_presets.py` (claude `:132-133`, codex `:160-161`, agy
  `:189-190`, opencode `:227-228`, pi `:285-286`, bash `:306-307`, cmd
  `:322-323`).
- **F12 — measured output volume** (GD G43): claude idle about 4.5 B/s,
  waiting on a tool about 770 B/s, streaming about 3.3 KB/s. The waiting
  figure matters here: claude keeps drawing during a long tool call, so a
  quiet threshold of minutes is not reached while it works.
- **F13 — quiet works for the agents without hooks.** Measured 2026-11-13 on
  a dummy harness (`session snapshot --json --settle-ms N` `.live.bytes`, PTY
  150×40): claude, opencode 1.18.18 and pi each produced **0 bytes** in three
  20 s windows while idle at their prompt; while running a 45 s
  `sleep 45` tool call, opencode drew 15–32 KB and pi 41–56 KB per 8 s
  window, falling to 0 once the turn ended. So byte quiescence over minutes
  separates working from done for all three. One run each; the earlier
  claude idle figure of 4.5 B/s (F12) was a single 45-byte frame not seen
  again.
- **F14 — the server cannot see argv templates.** `RunnerHello` advertises
  profile names only (`message.bgn:130-131`); templates are validated on the
  runner (`runner/agent_profile.go:83`, `runner/session.go:631`), whose only
  refusal of an `OpenExec` is `TaskFinished(-1, …)` (as for a missing stream
  adapter, `runner/session.go:955`).
- **F15 — control messages are one datagram.** Both the open request and
  `OpenExecRunnerRequest` are size-checked as single control messages
  (`cli/client.go:333`, `server/runner_send.go:27`; about 1170 B over UDP).
  `AssignTask` sends its prompt on a stream for this reason
  (`message.bgn:280-292`).
- **F16 — "mode" already means kind in this code.** The resume path calls
  the per-open, latest-recorded `Kind` a mode (`task_handler.go:1774-1778`,
  `taskstore.go:654-658`), and `submit --resume` changes any task's kind to
  oneshot (`task_handler.go:1425-1445`, `taskstore.go:695`). This spec
  therefore says **run mode** for foreground/background.
- **F17 — no cancel actor is recorded.** The cancel handler records no
  principal (`task_handler.go:438-446`) and `task_cancelled` carries none
  (`taskstore.go:910`). `TaskEntry.AuthorityActor` is who last wrote the
  task's authority, not who ended it.

## Decisions taken

**operator** = chosen by the human in conversation; **GD** = inherited from
the grand design row named; **this spec** = the author's call, to check.

| # | Decision | Decided by |
|---|---|---|
| A-D1 | **A task carries a run mode, `foreground` or `background`, fixed for its life** (G1, G24; "run mode" because "mode" already names kind, F16). Foreground is the zero value, so every existing task and every WAL record without the field is foreground. **A resume takes the run mode from the task**: a plain resume of a background task (the TUI's resume keys included) is a background resume. A resume that asks for background on a foreground task is refused `mode_mismatch`, and so is `submit --resume` of a background task (a new `SubmitStatus` value): it would turn the task into a oneshot with no session to watch | GD (G24), this spec (zero value, inherit on resume, refusals) |
| A-D2 | **Background applies to the interactive kind only in A2.** The stream kind could carry it (G1 makes mode independent of data plane), but nothing needs it yet; the open refuses `background` with `event_stream` | this spec (anti-goal: nothing without a consumer) |
| A-D3 | **Both run modes take an initial prompt (G6).** The open request and `OpenExecRunnerRequest` carry a prompt (on a stream, A-D17). Interactive fresh and resume templates accept `{prompt}`; an empty prompt drops the token entirely rather than passing `""`. A profile whose interactive template has no `{prompt}` cannot take one, and the harness does not type it into the PTY instead, which would race the program's startup. The server cannot see templates (F14), so **the runner refuses** such a start with `TaskFinished(-1, "prompt_unsupported: …")`: the task ends `Failed` with end reason `failed` and that message, in either run mode. Each preset's template is set from that agent's own help output in the plan; an agent that parses a leading `-` in a positional as a flag gets `--` before `{prompt}` | GD (G6), this spec (drop when empty, runner refuses, no typing) |
| A-D4 | **A background open is enqueued, never refused for capacity or for no runner (G26).** It is created Queued and placed by `Tick`. Refusals that remain at open time: grant (`ScopeNotPermitted`), spawn limits, a resume target that is missing or not terminal, `mode_mismatch`, a missing required field (A-D6, A-D7), and — when candidates exist — `ProfileUnavailable`. No runner is chosen at open time, so `AmbiguousRunner` is never answered: when several runners could take the task, `Tick` gives it to the first free one in `Candidates` order (connected earliest, then id), and an opener who cares pins one with the selector (`--runner`/`--host`) | GD (G26), this spec (which refusals stay, first-free placement) |
| A-D5 | **A background open must name its agent profile.** With no candidate at open time the profile cannot be resolved, and `Tick` would otherwise place an unnamed profile on whichever runner is free. Every fleet runner carries all presets (operator memory: fleet runs `--agents all`), so a bare open is ambiguous there anyway | this spec |
| A-D6 | **The deadline is required when a background task is created; no server default** (G41). Given as a duration; resolved to an absolute time when each run starts (A-D10) and persisted (G4). A resume reuses the stored duration (A-D7) | GD (G4, G41), this spec (reuse on resume) |
| A-D7 | **Background conditions, stored on the task:** deadline duration (required); turn-end signal `off`/`quiet`/`lifecycle`/`program`/`any`, default `any` (G33); quiet threshold, default **5 minutes** (F13 shows minutes separate working from done); blocked limit, default **5 minutes**; queueing period, unset by default (G29); PTY size, default **40×120** when the opener gives none. A resume of a background task reuses the stored values **and the stored prompt** when the resume carries none (a background run with no prompt would never report `working` and could only end at its deadline); each may be overridden on the resume request, and an override is stored. A foreground resume keeps today's behaviour of taking the request's prompt | GD (G29, G33), this spec (the three defaults, reuse on resume) |
| A-D8 | **What ends a background run**, evaluated by the server, each with its end reason (A-D11): the process exits (`process_exit`); the deadline passes (`deadline`); a turn ends on the chosen leg (`turn_done_lifecycle`, `turn_done_program`, `turn_done_quiet`); the agent reports `error` on lifecycle or program (`agent_error`); the agent stays blocked longer than the limit (`blocked_timeout`); the queueing period passes while still Queued (`queue_timeout`). The action is cancel (G3) | GD (G3, G4, G29), this spec (`agent_error` split from turn end) |
| A-D9 | **Turn end and blocked differ from await-idle's evaluator** (F6), which counts `waiting`/`blocked`/`error` as satisfying. For a background run: a turn end is lifecycle `idle` or program `idle`/`done`, or quiet; lifecycle `waiting` or program `blocked` starts the blocked clock and never counts as a turn end; `error` is A-D8's `agent_error`. **G16 applies to every leg, and per mux:** a lifecycle or program turn end counts only after that leg reported `working` since the current `SessionMux` started; the quiet leg only after output arrived since it started. The blocked clock runs while a blocked state stays the latest report, and stops on any `working` or turn end | GD (G4, G16, G33), this spec (the state sets, quiet's G16 reading, the clock) |
| A-D10 | **A run starts when `Tick` places it** (`TaskStore.Assign`, server clock). The deadline resolves then, is written on `task_assigned`, and keeps running while the task is Held. The queueing period resolves when the task is queued (create or resume) and is written on that record | GD (G4, G27, G29), this spec (server clock at Assign) |
| A-D11 | **The end reason is a task field, recorded for every task, both modes** (G5): `process_exit` when the runner reports the end of a task nobody cancelled; `cancelled` when a principal cancels, with who did it in a new field (operator, or the agent task's id; persisted on `task_cancelled` and carried in `TaskInfo` — the authority actor is not reused, F17); A-D8's values for automatic ends; and the existing failure paths keep `ErrorMsg` and get `failed` (`runner_disconnected`, `hold_expired`, `server_restart` stay as the message). It is written with the record that ends the run (`task_cancelled`, `task_finished`, `task_failed`); `Finish` after a cancel keeps the reason the cancel wrote. The status machine is untouched (G5): an automatic cancel will usually read `Succeeded` (F4), and the end reason is what the operator reads. Until A1 introduces runs, the field holds the latest run's reason | GD (G5), this spec (vocabulary, every task, latest run until A1) |
| A-D12 | **`Tick` places only queued work it can start: a task whose kind is oneshot, or whose run mode is background.** (Every oneshot task is foreground by A-D1's zero value, so the rule is on kind for those.) A foreground interactive or stream task is never placed by `Tick`, which closes F3. For a background task, placement starts a detached session on the chosen runner through the same code an interactive open uses after it has chosen a runner (F1's steps from the ticket onward), extracted into one function both call. The task gets the PTY size stored for it | this spec |
| A-D13 | **Conditions survive a restart by being re-derived, not by persisting watchers.** Deadlines and queueing periods are absolute times in the WAL; one deadline watcher (F8's shape, but woken whenever a deadline is added rather than exiting) covers both, with a startup pass after replay. Turn-end and blocked watchers are armed on the mux whenever a background session gets one: at placement and at re-adoption (`rebindHeldSessions`). Per A-D9 a re-adopted mux needs a fresh `working`, so a restored idle screen (F7) cannot end the run on the first tick. Accepted cost: a run whose turn ended across a deliberate server restart, before its watcher fired, runs on until its deadline | this spec |
| A-D14 | **Every surface shows, for a background task:** the mode; the end reason; the deadline (absolute, and remaining while running); and, while Queued, **why it waits** — no connected runner has the repo as a root and matches the selector and profile, or such runners exist and none has a free slot (G27). The wait reason is computed when a listing is built, from the same `Candidates` call `Tick` uses, not stored | GD (G27), this spec (computed at listing) |
| A-D15 | **Surfaces for opening a background session.** CLI: `session new --background` (implies detached) with `--prompt`, `--deadline` (required), `--turn-end`, `--quiet`, `--blocked`, `--queue-timeout`; `--prompt` is accepted in foreground too. TUI: the `session new` command line takes the same flags, and a form for background opens prefills the deadline with 1 hour. WebUI: Compose gains a prompt field and a background section (deadline prefilled 1 hour, turn-end select, the other limits), and sends the PTY size. The plan walks `surface-parity-checklist` | GD (G41: front-ends may prefill), this spec (flag names, 1 hour prefill) |
| A-D16 | **The cancel path is not changed in A2.** F5's SIGKILL reaches a background run the same way it reaches a human's cancel. The two wrong comments are corrected. Whether an automatic cancel should first let the agent stop gracefully (the agent's transcript is its resume state, and the child's own children are not in the signal's reach) is measured in the plan, not designed here: the plan checks whether a claude killed mid-turn resumes with `--continue` intact and whether its tool subprocesses outlive it. **If either fails, a graceful stop for automatic cancels is part of A2**, as its own stage after the core: schedules resume the same conversation (G38), so a broken resume state would break every later firing | this spec |
| A-D17 | **The prompt travels on a stream on both legs**, client→server and server→runner, never inside the open request or `OpenExecRunnerRequest` (F15; GD G13's rule, `AssignTask`'s precedent). A triage prompt can run to several KB, and a background open accepted over WebSocket would otherwise fail only at placement, on a UDP runner, after the opener was told it was queued. The size cap is the plan's, stated on every surface | this spec |

## A2 — background mode

### Opening

A background open is an `OpenInteractiveRequest` with a new `background`
bit (one of F10's spare bits) and, when it is set, a block of fields: the
conditions of A-D7 as durations in milliseconds and the turn-end enum. The
prompt (A-D3), in either run mode, follows on a stream (A-D17). The server:

1. validates the grant, spawn limits and resume target exactly as a
   foreground open does (F1 steps 1–4), plus A-D1's mode check and the
   required fields (A-D5, A-D6);
2. if candidates exist, checks the named profile is advertised by one of
   them (`ProfileUnavailable` otherwise); if none exist, skips that check;
3. creates or resumes the task as Queued with mode `background` and its
   conditions, resolving the queueing period to an absolute time;
4. answers `ok` with the task id, and calls `OnChange` so `Tick` runs.

It never takes a slot or contacts a runner itself. The response tells the
client the task was queued; the CLI prints the id, as `-d` does.

### Placement

`Tick` (A-D12) walks Queued tasks in creation order as it does after pillar
0, skipping foreground ones. For a background task it asks `Candidates`,
takes the first Idle, non-displaced runner advertising the profile, reserves
the slot (`BindTask`), re-reads the task, and starts the session. `Tick`'s
contract is that its assign step is cheap (`AssignFunc` doc,
`scheduler.go:11-19`): the plan checks whether opening a runner stream can
block, and if it can, `Tick` reserves and marks Running under its lock and
hands the session start to a goroutine that releases both on failure.

Starting the session is F1's sequence from the ticket onward: ticket and
board registration, runner stream, `OpenExec` with the prompt, a detached
`SessionMux` sized from the task (F9's path), the `afterMuxStopped`
goroutine, and the condition watchers (A-D13). `Assign` stamps `StartedAt`
and the absolute deadline.

### Ending

The deadline watcher and the per-mux condition watcher call one function,
`endBackgroundRun(taskID, reason)`, which cancels through `TaskStore.Cancel`
with the reason (A-D11) and lets the existing `OnCancel` → `CancelTask` path
reach the runner. A human's cancel takes the same path with reason
`cancelled`. A queueing period expiring cancels a task that never reached a
runner; `Dispatcher.OnCancel` already skips the runner for an unassigned
task (`server/dispatch.go:216-218`).

A human may attach, view or type into a background session (G24). Typing
stamps `lastInput`, so under A-D9 a turn end then needs a `working` reported
after that input; the run is cut at the next turn end or the deadline.

### Persistence (disk axis)

All new WAL fields are JSON keys whose zero value is today's behaviour
(the pattern `server/wal.go` already follows for every added field):

- `task_created` / `task_resumed`: mode, the A-D7 conditions, the prompt
  (already a field), the queueing deadline as absolute nanoseconds.
- `task_assigned`: the run's absolute deadline.
- `task_cancelled`, `task_finished`, `task_failed`: the end reason.

Replay restores each into `TaskEntry`. Queued tasks already survive replay as
Queued (the post-replay sweep at `taskstore.go:1447-1454` fails only Running
ones); Held tasks keep their deadline. `task_cancelled` also carries who
cancelled (A-D11). The
deadline watcher's startup pass ends whatever expired while the server was
down — including a Held background task whose deadline passed during the
hold — before re-adoption could revive it.

### Wire

- `OpenInteractiveRequest`: `background` bit; conditional block; the prompt
  follows on a stream (A-D17). Client→server only.
- `OpenInteractiveResponse`: a `queued` indication for a background open;
  `OpenInteractiveStatus` gains `mode_mismatch`, `deadline_required`,
  `profile_required`. `SubmitStatus` gains `mode_mismatch` (A-D1).
- Server→runner: the prompt on a stream with the `OpenExec` (A-D17), and the
  runner's `prompt_unsupported` refusal (A-D3). **Runner-decoded**: the plan
  runs `scripts/wire-skew-check.sh` and states the restart order.
- `TaskInfo` and `TaskStatusEvent`: run mode, end reason, who cancelled,
  deadline, wait reason (A-D14), and the stored conditions for display.

### Not in A2

The per-run record and run identity (A1); retiring oneshot, `submit`, the
`task.<id>.log` path and the TUI log pane (A3); schedules (pillar S).

## A1 — PTY record

Settled (GD G11, G20, G21, G42, G43):

- Every session's PTY output can be recorded server-side, per run; **off by
  default**, a server flag sets the default, a session opened with
  recording on is recorded (G42).
- Retention follows agentboard's topic retention: forever or a period from
  the run's end, changeable; expiry deletes the record only; prune deletes it
  with the task; a new capability bit deletes records alone (G20, G34, G43);
  every surface lists size and expiry.
- Records travel on streams (G13); reading needs `exec_view` under the
  task's scope (G34).
- Raw bytes with timestamps are kept; replay and text rendering are both
  derived from them (operator, 2026-11-13, on G43's measurement).
- Turning recording on for a live session starts the record from the
  detach ring's contents, as `agentboard.Board.Retain` writes what a topic
  already holds (this spec).

Open, to settle before A1's plan: run identity (G21 leaves it to this spec —
a per-task run number assigned at create and each resume is the candidate);
the size cap (O7); compression on disk; how a record is viewed on each
surface.

## A3 — retiring oneshot

Settled (GD G7, G22, G23): the oneshot kind stops being created; old WAL
records still replay and display (`TaskKind_Oneshot == 0`); `submit` and its
in-repo callers move to `session new` (`scripts/e2e-oneshot-progress.sh` and
`scripts/sandbox/probe.sh` rely on print-mode argv and the task log; the
skills under `runner/agentskills/` document `submit`, `logs` and `watch`);
`task.<id>.log` and its producer, consumers and authorization go; the TUI's
permanent log pane goes.

Open, to settle before A3's plan, with the operator: what the TUI's freed
space shows (G23); whether `session new --oneshot` (G7) selects a print-mode
template in background mode only or in both.

## Testing (A2)

- Unit, server: a background open with no candidate is Queued, not refused;
  with a full runner it is Queued and placed when a slot frees; a foreground
  open is never placed by `Tick` (F3); missing deadline / profile are
  refused; a resume in the other mode is refused.
- Unit, evaluator: A-D9's table — `waiting`/`blocked` never end a turn and
  start the clock; a turn end before any `working` since mux start does not
  fire; after input, it needs a `working` after the input; a restored idle
  screen on a new mux does not fire.
- Unit, persistence: replay restores mode, conditions, deadlines and end
  reasons; a deadline that passed while the server was down ends the task in
  the startup pass with reason `deadline`.
- Dummy harness, real claude (`dummy-harness` skill): a background open with
  a short prompt ends `turn_done_lifecycle`; a prompt that triggers a
  permission prompt under manual mode ends `blocked_timeout`; a short
  deadline ends `deadline`; each surface shows mode, end reason and, for a
  queued task on a full runner, the wait reason.
- The F5 measurement of A-D16.
