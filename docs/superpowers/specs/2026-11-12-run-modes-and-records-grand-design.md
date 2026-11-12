# Run modes, machine-run work, and remote records — grand design

Date: 2026-11-12. Line references are to `main` at `a718691e`.

**Status: GRAND DESIGN.** This document fixes the decisions shared by several
pieces of work and the order they are built in. Each pillar below gets its own
spec, which must stay inside the decisions here or record a reversal of one.
Nothing here is implemented yet.

## Problem

- **P1 — the oneshot kind is half-built and unused.** Of the 37 tasks visible
  from one operator session on 2026-11-12, none was oneshot
  (`harness-cli ls --json`, counted by `kind`; the visible set is bounded by
  that session's scope and by prune, so this is a lower bound on use, not a
  census). Interactive took over almost as soon as it existed.
- **P2 — `kind` bundles three independent choices.** Choosing oneshot today
  chooses all of these at once:
  - *invocation*: print mode (`claude -p`, `--agent-oneshot-argv`) rather than
    the agent's TUI;
  - *admission*: a server-side queue (`Scheduler.Tick`, `server/scheduler.go`)
    rather than fail-fast — an interactive open answers `RunnerBusy` /
    `AmbiguousRunner` at once (`handleOpenInteractive`,
    `server/task_handler.go:1702-1714`);
  - *lifetime*: the process exits by itself rather than a human ending it.
  None of the three needs the other two.
- **P3 — the queue is defective, which is itself evidence nobody uses it.**
  Submit accepts a repo *under* a runner root (`Registry.Candidates`,
  longest-prefix match, `server/registry.go:393`); `Tick` hands a runner only
  tasks whose `RepoPath` *equals* one of its roots (`server/scheduler.go:99` →
  `TaskStore.NextQueuedForRepoFunc`, `server/taskstore.go:1271`). A submit to
  `/root/sub` on a runner serving `/root` is answered `ok` and stays `Queued`
  forever (reproduced with a unit probe against `Scheduler.Tick`: one
  candidate, zero assignments). The README says roots match "verbatim"
  (`README.md:229-230`, `:250`); the runner-side gate and `Candidates` match by
  prefix (`runner/session.go:702`, `protocol.IsUnderRoot`). Which of the two is
  the intended contract is decided in G9.
- **P4 — interactive cannot be given its first prompt.** Fresh and resume
  interactive argv templates reject `{prompt}` (`runner/agent_command.go:70-73`,
  "there is no prompt on a PTY open"). claude itself starts its interactive
  session with a positional prompt (`claude [prompt]`; `-p/--print` is the
  non-interactive mode — `claude --help`, 2.1.322).
- **P5 — oneshot's log is a lossy copy of a record that already exists.** The
  oneshot log is the runner's rendering of the agent's stdout
  (`--agent-log-format`, `runner/streamagent/`). The agent's own transcript is
  richer, and the runner can already find and read claude's
  (`findClaudeTranscript` / `readClaudeTranscript`,
  `runner/streamagent/claude_transcript.go:80`, `:150`, built for
  `2026-09-30-stream-resume-transcript-replay-design.md`). Nothing lets an
  operator fetch a transcript from a task on another host.
- **P6 — process logs are only readable on their host.** Remote management
  needs the server's and each runner's own log. The runner does not know where
  its log goes: `scripts/runner.sh` redirects it under `bin/.run/`
  (`.claude/skills/implementation-pitfalls/SKILL.md`, Pitfall 4).
- **P7 — there is no mode for work a machine starts and nobody watches.** No
  time-based start, no automatic end. The first intended use is a
  cron-like recurring job, probably run as an interactive agent (operator,
  2026-11-12).

## Intent

The motives, in order (operator, 2026-11-12):

1. **Stop carrying a half-built mechanism.** The queue either gets a reason to
   exist or is removed. It is kept only because background mode (G1) gives it
   one: work a machine starts can wait for a slot.
2. Work a machine starts, on a schedule, that ends itself.
3. Records and logs readable remotely, without logging in to the host.

## Decisions taken

**operator** = the human chose it in conversation (2026-11-12). **this spec** =
the author chose it while writing; those rows are the ones to check.

| # | Decision | Decided by |
| --- | --- | --- |
| G1 | **Two run modes.** *Foreground* is for a human: admitted immediately or refused (today's interactive behaviour), and ended by a human. *Background* is for a machine: queued until a slot is free, and ended by conditions (G4). The mode is independent of invocation (TUI / print) and of the data plane (PTY / stream kind) | operator |
| G2 | The queue serves background only. A foreground open keeps today's fail-fast answers | this spec (follows from G1) |
| G3 | When a background condition is met, the action is **cancel**. A cancelled task can be resumed; only prune deletes it. (At any task end, cancel included, the runner removes a *clean* worktree and keeps the `harness/<id>` branch — `RemoveIfClean`, `runner/session.go:857`, `:1137`, `:1244` — and a resume re-attaches a worktree to that branch. G3 does not change this) | operator |
| G4 | Background end conditions: **process exit** (always); **deadline** (**required** on every background task, so one always ends even when its agent reports no signals — G25 and pillar D rely on this; given as a duration that starts when a run starts, not when the task is queued (G27), and persisted as the absolute time it resolves to at that start; each run, a resume included, gets its own); **turn end** (the program reports a finished turn); **blocked too long** (a permission prompt or similar outlives a limit). Turn end and blocked are read from the signals `session await-idle` already evaluates, with its stale-state rules D6 and D18 (`2026-11-08-await-idle-signal-design.md`): turn end from the leg chosen per task (G33), blocked from lifecycle and program status only | this spec |
| G5 | **The end reason is recorded on the task** (`turn_done`, `deadline`, `blocked_timeout`, `process_exit`, a human's cancel, …) and shown on every surface. The `TaskStatus` state machine is not changed. Reason: `TaskStore.Finish` replaces `Cancelled` with `Succeeded`/`Failed` from the exit code (`server/taskstore.go:846-858`), so after an automatic cancel the status reports how the agent answered the kill ladder, not whether the work was done. Whatever claude's exit code on SIGHUP is, one case reads wrong: if 0, a deadline cut mid-turn reads `Succeeded`; if not 0, a finished turn reads `Failed` (claude's actual exit code is not measured) | this spec |
| G6 | **Interactive takes an initial prompt.** The open request carries a prompt, and interactive argv templates accept `{prompt}` | operator |
| G7 | **The oneshot task kind is retired.** One session kind for PTY work. Whether a run "happens once" is a property of the profile's argv (`claude -p`, `bash -c` exit by themselves), not of the task. `session new --oneshot` selects the profile's print-mode template. The `stream` kind stays: it is a data-plane difference, not one of the three choices in P2. The `submit` verb's in-repo callers (`scripts/kvm-runner/kvm-runner.py`, `scripts/sandbox/probe.sh`, `scripts/e2e-oneshot-progress.sh`, and the skills that document `submit`) move in the same change; no compatibility alias is kept | operator (fold oneshot into `session new`), this spec (retire the kind, no alias) |
| G8 | **Recurring starts (a cron substitute) live in the server.** The harness server keeps the schedules itself and fires them on its own timer; the host's crontab or systemd timers are not used. Each firing creates a background task | operator |
| G9 | **Roots match exactly: a task's repo must equal one of the runner's roots.** This reverses a deliberate design (directory-boundary prefix, `7b98e2f3`; longest-prefix tiers, `85190971`), not a code fix toward the README's "verbatim". Reasons: (1) prefix lets a broad runner take a task silently while the repo's own runner is reconnecting or restarting, because the longest-root comparison only sees connected runners — the operator has been surprised by this, and an unattended background task would not be seen at all; (2) roots exist to prevent accidents and to keep paths out of remote reach (operator, 2026-11-12), which an explicit list serves better than a subtree; (3) no current use needs a subtree: every fleet runner's root is the repo itself except one `bash`-only and one `cmd`-only runner rooted at a workspace directory, used only to open a shell there (operator), and a child task inherits its parent's `HARNESS_REPO_PATH`, which equals the root. A foreground open of a repo that no connected runner has as a root is refused `NoRunner`; a background task waits in the queue showing why (G26, G27). The same predicate replaces `protocol.IsUnderRoot` on the runner gates (`runner/session.go:702`, `:921`, `runner/exec_run.go:197`, `runner/git_query.go:569`) | operator |
| G10 | **Transcripts are fetched by task id, never by path.** The runner resolves the location from the task's agent profile and worktree. A runner flag turns the feature on or off for that host | operator (on/off), this spec (by id) |
| G11 | **Every session's PTY output is recorded, whatever the agent.** Not a fallback for agents without a transcript: the two record different things (the transcript is the agent's conversation; the PTY record is what the operator saw, including permission prompts and output of the agent's subprocesses), and recording one way for all agents removes the "does this agent write a transcript" branch. **Recording can be turned off**, per session when it is opened, with a server-wide default (O8). The record is kept on the server, which relays every PTY byte, so it stays readable while the runner host is down; a per-runner flag (G10) therefore cannot gate it, which is why the switch is per session | operator (record every agent; on/off), this spec (server side, per-session switch) |
| G12 | **Server and runner process logs are readable through the harness.** The runner writes its own log file (a flag) rather than the harness guessing the path a launcher chose. Reading is gated by a new **grantable** capability bit, not reserved to the operator | operator (the want), this spec (mechanism, gate) |
| G13 | Every transcript, PTY record or process log travels on a stream, never inside one control message: a control message must fit one datagram and an oversized one is dropped silently over UDP (the pattern of the `*-streamed-design.md` specs, e.g. `2026-10-10-exec-request-streamed-design.md`) | this spec |
| G14 | **Order:** (0) the P3 fix; (A) run modes, **including the PTY record (G11, G22)**; (S) recurring starts; (B) transcripts and (C) host logs, in parallel with each other and independent of A; (D) ephemeral runners, later. Amended: the PTY record was first placed in B. The builder check found that the only server-side task log today comes from the oneshot path (G22), so retiring oneshot before B would have shipped unattended runs with no record | operator (order; the amendment, 2026-11-12) |
| G15 | **Ephemeral runners (pillar D) are not built now.** A, S, B and C must not preclude them; § Pillar D lists what that means | operator |
| G16 | **Turn end needs a `working` observed in this session.** The `SessionStart` hook reports `idle` (`runner/settings.go:55`), and an argv prompt (G6) is not client input, so it stamps no `lastInput` (await-idle D14). Under await-idle's D6 as written, a background session launched with its prompt would read "turn done" at startup and be cancelled before doing anything. For G4, a turn end counts only after the same signal has reported `working` since the session started | this spec (builder check) |
| G17 | **Submit and queue assignment answer from one function.** `Tick` asks `Candidates` for each queued task and assigns to a returned runner with a free slot, instead of matching roots and selector/profile with its own closure (`server/scheduler.go:91-99`). With exact matching (G9) the longest-root tiers have nothing left to rank, so the scoring (`protocol.MatchLen`, `bestRootScore`, `matchedRoot`, `server/registry.go:380-440`, `server/task_handler.go:2873`) is removed. `Dispatcher.TryDispatch` (`server/dispatch.go:189`), a third copy of placement with no production caller, is deleted | this spec (builder check; simplified after G9) |
| G18 | **A transcript is bound to its run by the agent's session id, recorded when the run starts** (for claude, the `session_id` its `SessionStart` hook receives), and persisted with the task. Fetch returns the bound transcripts only. It never falls back to "newest `*.jsonl` in the directory", which is `findClaudeTranscript`'s `--continue` rule (`runner/streamagent/claude_transcript.go:75-80`): a `--no-worktree` task runs in the repo itself (`runner/session.go:750`), where other tasks' and the operator's own sessions live, so that rule could return another conversation under this task's scope. A task resumed into several sessions has several bound transcripts | this spec (builder check) |
| G19 | The host-log capability (G12) is a capability bit alone, not bit plus scope: a process log mixes every task on the host and cannot be filtered to a scope reliably. The cost is that a holder reads about every task on that host; who holds it by default is O6 | this spec (builder check) |
| G20 | **Every record the harness keeps for a task can be deleted without pruning the task.** Today the only way to delete a task's log is prune (`TaskStore.PruneByIDs` removes `<data-dir>/logs/<id>.log`, `server/taskstore.go:1549-1554`), which also forgets the task. This covers the task log, the PTY record (G11) and any server-side transcript cache (O5). It does not cover the agent's own transcript on the runner host: that file is the agent's resume state (`--continue`), not a harness record | operator (delete a log, keep the task), this spec (scope) |
| G21 | **Records belong to a run, not to a task.** A run is one open of a task: its creation, and each resume after it (`TaskStore.Resume`, `server/taskstore.go:659`). Today a resumed task's output is appended to the same `<id>.log` (`LogStore.Append` opens it `O_APPEND`, `server/logstore.go:26-38`) with no boundary between runs, so runs mix (operator, 2026-11-12). The task log, the PTY record (G11) and transcript bindings (G18) are each kept per run, and listed, fetched and deleted (G20) per run. The run's identity (e.g. a per-task sequence persisted in the WAL) is left to the pillar spec | operator (runs mix today), this spec (per-run keying) |
| G22 | **The `task.<id>.log` path is removed with G7.** Its only producer is the oneshot `handleAssign` (`runner/session.go:660`); its consumers are the CLI `logs` verb (`cli/logs.go`, CLI-only, `cli/verb/table.go:2276`), the TUI log pane (`tui/events.go:192`), the server's file store (`server/logtap.go` → `LogStore`) and its authorization (`server/pubsub_authz.go`). The per-run PTY record (G11, G21) and transcripts (G18) hold what it held, more faithfully. Raw PTY bytes include redraws, so reading a record later needs rendering (replaying it through the server's screen model, or showing the transcript); how is pillar A's spec. Whether the runner also writes diagnostic lines into the task log, which would need a new home, is not checked here | operator |
| G23 | **The TUI's permanent log pane is not kept.** It takes too much of the screen for what it shows (operator, 2026-11-12). Records are viewed on demand; what the freed space is used for is decided in pillar A's spec | operator (the pane is too large), this spec (on demand) |
| G24 | **A task's mode is fixed for its life, and a human attaching does not pause its conditions.** The two modes have different lifecycles; a background task stays one until a condition or a human ends it. **A human can cancel a background task at any time**, queued or running (`TaskStore.Cancel` accepts `Queued`, `Running`, `Detached` and `Held`, `server/taskstore.go:877-880`); the end reason records it (G5). Background tasks are run so that no permission prompt appears, so there is no approval to protect while a human is attached. How a run avoids prompts is the agent's own argv, not a harness setting: the profile's `agentArgs` or the task's `extra_args` (`OpenInteractiveRequest.extra_args`, merged after the runner's baseline, `runner/extraargs.go`), where each agent has its own flag (`scripts/agent_presets.py` `_SANDBOX_AGENT_ARGS`). Consequence, accepted: a human who attaches and types into a background task is cut at the next turn end or the deadline. Blocked-too-long (G4) stays, as the detector for the premise failing: its end reason (G5) tells the operator a prompt appeared | operator |
| G25 | **Foreground and background share a runner's slots** (`--max-tasks`); no slot is reserved for either. A human's open while background tasks fill the runner is refused `RunnerBusy` as today; a background task waits in the queue while humans fill it. Reserving slots would be a priority, which the anti-goals exclude, and every background task's deadline bounds how long a human waits. Accepted cost: on a one-slot runner, a long-deadline recurring job locks humans out of that runner for its length | operator |
| G26 | **A background task is queued even when no runner can take it now.** Whoever starts it (a schedule firing, or a human or agent opening in background mode) only puts it in the queue; placement happens when a candidate (G17) appears with a free slot. Today a submit with no candidate is refused `NoRunner` (`server/task_handler.go:1255-1259`); with exact roots (G9) that would silently drop every firing that lands while the repo's runner restarts. Foreground keeps `NoRunner`. A typo'd repo is caught when a schedule is created: creation is refused unless at least one candidate exists then | operator (enqueue only), this spec (check at schedule creation) |
| G27 | **The queue is managed by the operator; nothing in it expires by itself.** A queued task waits until it is placed or someone cancels it (`TaskStore.Cancel` already accepts `Queued`, `server/taskstore.go:880`). The deadline (G4) does not run while queued. Every surface shows why a queued task is waiting — no candidate runner connected, or candidates all busy — since that is what tells the operator whether it has gone stale | operator (manual cancel, no expiry, deadline from run start), this spec (wait reason on every surface) |
| G28 | **A schedule fires at most one run at a time** (settles O2). A firing while the schedule's previous task is still queued or running is skipped, and the skip is recorded on the schedule. Without this, a schedule whose runner is down fills the queue one task per firing until someone notices (G27); with it, the queue holds at most one task per schedule | operator |
| G29 | **An optional queueing period.** A background task (or a schedule, for each task it fires) may carry a limit on how long it waits in the queue; past it, the task is cancelled with its own end reason (G5, e.g. `queue_timeout`). The limit is a field of the task and the only one the timeout reads: a human or agent opening in background mode sets it directly, and a schedule copies its value into each task when it fires, so both paths reach one mechanism. Unset by default, which keeps G27's no-expiry rule. Separate from the deadline (G4), which starts only when the run starts. For a schedule it is what lets a stale firing give way: under G28 a queued firing blocks the next ones, and a queueing period shorter than the interval cancels it before the next firing | operator (the option, at both task and schedule level), this spec (unset by default, separate end reason) |
| G30 | **A schedule can be edited after it is created** — its period and conditions included — so a queueing period (G29) can be set once the usual wait is known: past it, something is probably wrong. An edit applies to firings after it; a task already fired keeps the values it was created with (cancel it to drop it) | operator (editable), this spec (future firings only) |
| G31 | **The usual wait is observable.** Each run records when it was queued and when it started (today `TaskEntry` has one `CreatedAt`/`StartedAt` per task, `server/taskstore.go`, and a resume re-queues the same task, so these become per run with G21). A schedule lists its recent firings with each one's wait, end reason (G5) and skips (G28). Without this G30's "once the usual wait is known" has nothing to read | this spec (follows from G30) |
| G32 | **Transcripts (pillar B) cover every agent the fleet runs that keeps a transcript** — today's presets are claude, codex, agy, opencode and pi, and their sandboxed variants — not claude first with the rest later. A program with no conversation (bash, cmd) has the PTY record (G11) only. Pillar B does not land claude-only | operator |
| G33 | **Which signal ends a turn is chosen per task: `off`, `quiet`, `lifecycle`, `program` or `any`; the default is `any`.** The vocabulary is `session await-idle --signal`'s (`2026-11-08-await-idle-signal-design.md` D3), plus `off`. `any` is the default because quiet needs nothing from the agent, so some leg always fires whatever the agent is — reading turn end from lifecycle and program only (G4 as first written) left agents without hooks with no turn end. `off` is for work that spans turns, e.g. a task that ends its turn to wait for another agent's reply. Quiet's threshold is set per task and is minutes, not await-idle's 2500 ms default, or quiet would beat the hooks on agents that have them; its default is pillar A's. A schedule carries the choice and copies it into each task it fires (as G29). The end reason (G5) names the leg that fired (await-idle D8). G16 applies to every leg | operator |
| G34 | **Capability bits.** Three new bits: reading host logs (G12, G19), creating and editing schedules (pillar S; a schedule keeps creating tasks after its creator is gone, which `spawn` does not cover), and deleting records (G20; reusing `prune` would make granting record deletion also grant forgetting tasks, and would let any agent holding `prune` for its probe tasks erase its own PTY records, which are the evidence of what it did). Reading a PTY record or a transcript reuses `exec_view`: both show what that bit already shows, a session's content, under the task's scope. Bit scarcity is not a reason to merge gates: `Capability` is `u32` with 26 bits used (`runner/protocol/message.bgn:4305`), and widening it to `u64` is cheap — its fields are only in client↔server formats (no runner-decoded format), the WAL stores caps as JSON numbers, and the WebUI's `row.caps & c.bit` (`webui/static/main.js:4106`) breaks only from bit 31. The widening is deferred until few bits remain | operator (record deletion its own bit; widen later), this spec (the other two bits, `exec_view` reuse) |
| G35 | **A background task's authority is computed as a spawn's.** Caps are `caps_creator ∩ delegate_caps_creator ∩ requested_caps`, and scope and the delegation bound are attenuated the same way (`SubmitRequest.requested_caps`, `runner/protocol/message.bgn:1214-1220`); the mode adds no default of its own (settles O1). For a human's or an agent's background open the creator is the opener. For a schedule's firings the creator is the schedule (G36) | operator |
| G36 | **A schedule carries its own authority, declared when it is created**: caps, scope and delegation bound, computed by G35 with the schedule's creator as creator, so it never exceeds what that principal held then. Each firing's task gets exactly that authority. It is shown with the schedule on every surface, and an edit (G30) re-computes it with the editor as creator. Consequence, accepted: narrowing the creator's own authority later does not reach the schedule; editing or deleting the schedule does | operator (the schedule holds its authority), this spec (bounded at create and edit; no propagation) |
| G37 | **Agents may create schedules; the schedule bit (G34) is the only gate** (settles O4). Like every bit it reaches a task by the spawn rule (G35), so it is attenuated down a spawn chain and is held only where it was granted. Keeping an agent from leaving schedules behind is done by not granting the bit, not by an agent-specific rule | operator |

## Anti-goals

Outcomes that count as failure even if the code works.

- **A half-built mechanism left in place.** Nothing in pillar A is built
  without a consumer. The oneshot queue path is removed, not kept beside
  background mode (operator).
- **A scheduler with priorities or fairness.** There are not enough runners
  for one to matter (operator).
- **(pillar D) a provisioned resource that nobody reclaims.** See § Pillar D.

## Non-goals of this round

- Pillar D itself (G15).
- Per-task on/off for transcripts (G10 is per runner).
- Any change to the `TaskStatus` state machine (G5).

## Pillar 0 — the placement fix

Submit and queue assignment answer "which runners may take this task" from the
same function, `Registry.Candidates` (G17): exact root match (G9), the task's
selector, and its agent profile. Today `Tick` walks runners with its own
selector/profile closure (`server/scheduler.go:91-99`); it becomes a walk over
queued tasks that asks `Candidates`. `Dispatcher.TryDispatch` and the
longest-prefix scoring are deleted (G17). Regression tests: a submit to a
subdirectory of a root is refused `NoRunner` at submit rather than left
`Queued` (today's submit; background admission changes in pillar A, G26); a runner rooted at a parent directory does not take a task for a repo
below it, including while that repo's own runner is disconnected.

The disk axis: a task already in the WAL whose repo is a subdirectory of a root
could no longer be resumed, exec'd or git-queried once the runner gates are
exact. Before landing, list such tasks in the live store; the spec decides
whether any matter.

## Pillar A — run modes

What the pillar spec must settle, and the facts it builds on:

- **Retiring the kind (G7) has a disk axis.** `TaskKind_Oneshot == 0`, and old
  WAL records replay as oneshot (`server/taskstore.go:1400-1410` comment). Old
  records must still replay and display; only new oneshot tasks stop.
- **`Queued` is not oneshot-only.** `TaskStore.Resume` sets it on every resume,
  interactive included (`server/taskstore.go:682`; called from both submit and
  open, `server/task_handler.go:1511`, `:1914`). The spec enumerates every
  producer and consumer of `Queued` before changing it.
- **Conditions must survive a server restart.** Idle watchers are memory-only
  (`server/idle_watchers.go:15-17`). A background task's conditions are
  persisted with the task and re-armed when the server restarts and when a held
  task is re-adopted (`server/readopt.go`). A deadline is stored as an absolute
  time.
- **`Tick` is event-driven only.** Deadlines (and pillar S's firings) follow
  the shape the server already uses for held tasks: one timer armed on the
  earliest persisted absolute deadline, re-armed after each firing, with a
  startup pass (`watchHeldDeadlines`, `server/hold.go:444`).
- **Turn end is not "any idle fire".** `await-idle` treats `blocked` and
  `waiting` as satisfying its legs and reports which state it saw
  (`2026-11-08-await-idle-signal-design.md` D5, D8). For G4, `done`/`idle` is a
  turn end; `blocked`/`waiting` starts the blocked-too-long clock. And a turn
  end needs a `working` seen in this session first (G16).
- **Some agents report neither lifecycle nor program status:** an agent without
  hooks (today opencode and pi: the lifecycle hooks in `runner/settings.go`
  cover claude, codex and agy), any program on a Windows runner, a
  `--no-worktree` task (same spec, D10). The quiet signal (G33) is what ends
  their turns; the spec measures whether opencode and pi keep drawing while
  they work, which quiet relies on (claude repaints its spinner about every
  100 ms, `server/await_idle_handler.go:15-18`). Blocked-too-long cannot be
  read from quiet, so for them it does not apply.
- **A background interactive session starts with no client attached.** A
  detached session's PTY is 0×0 until a client sends a size
  (`.claude/skills/dummy-harness/SKILL.md`, "Driving harness-tui"). Background
  opens need a default size (`session new` already has `--rows/--cols`).
- **Nobody is watching to approve anything.** Avoiding prompts is the agent's
  argv (G24), so the harness adds no permission-mode setting, and caps follow
  spawn's rule (G35).
- **The PTY record (G11, G21, G22)** is built here, server-side, per run, with
  the per-session switch. Its volume is the cost: an agent TUI repaints its
  spinner about every 100 ms while working (`server/await_idle_handler.go:15-18`),
  so raw bytes of a long session are large; size and retention are O7. What is
  kept today: the server holds an in-memory scrollback per detached session
  (`DetachRingBufferSize`, `server/server.go`), and nothing on disk for
  interactive output (G22: the only disk log is the oneshot path's).
- Removing `task.<id>.log` (G22) and the TUI's permanent log pane (G23)
  happens in the same change as G7; the CLI `logs` verb and the TUI move to
  the per-run record.
- The end reason (G5) appears on every surface; walk
  `surface-parity-checklist`.

## Pillar S — recurring starts

- Schedule definitions are persisted on the server and survive its restart.
- Each firing creates a background task (G8) carrying the schedule's prompt,
  repo, profile, selector, extra agent args (G24) and conditions.
- A firing while the previous run is still queued or running is skipped
  (G28); a firing with no runner available is queued, not refused (G26).
- Open for the pillar spec: whether firings missed while the server was down
  run once on start or are skipped (O3). An agent holding the schedule bit may
  create one (G37), bounded by its own authority (G36); whether spawn limits
  (`2026-11-09-spawn-limits-design.md`) also count schedules or their firings
  is the pillar spec's.

## Pillar B — transcripts

- Fetch by task id (G10), returning the transcripts bound to that task's runs
  (G18). For claude, the directory part of `findClaudeTranscript`
  (`runner/streamagent/claude_transcript.go:80`) resolves the documented
  location (`~/.claude/projects/<encoded-cwd>/`, or under `$CLAUDE_CONFIG_DIR`)
  and is reused; its newest-file rule is not. A sandboxed profile keeps its
  config dir somewhere other than the runner host's `~/.claude`, so the
  location is resolved in the agent's environment, not the runner's.
- **Every agent that writes a transcript is covered, not claude alone (G32).**
  The per-agent part is kept to two facts — where the agent stores a session,
  and how its session id reaches the runner — recorded per profile, as
  `--agent-log-format` is today. Fetching returns the stored file as it is,
  so no per-agent parser is needed to fetch; rendering one is per agent and
  may come later. Today only claude has either fact implemented
  (`runner/streamagent/claude_transcript.go`). Binding past runs (before G18
  existed) is also per agent: claude stores per working directory, so a
  worktree task's directory holds only that task's sessions; another agent
  may store by date and need a field read instead.
- Transcripts live outside the runner's roots. Resolving from the task, never
  from a client-supplied path, is what keeps this from being an arbitrary-file
  read.
- A transcript holds everything the agent saw. Reading one is content-read of
  that task: gated by the task's scope like other content reads.
- A transcript lives on the runner host, so it is unavailable while that host
  is down; today's oneshot log lives on the server and is not. Whether a
  fetched transcript is cached on the server is open (O5).
- The PTY record is built in pillar A (G14, G22), not here.

## Pillar C — host logs

- The runner writes its own log file when given a flag (G12); the server
  likewise. Both are served on a stream (G13), with a tail or range rather
  than the whole file.
- Before anything is exposed, the spec audits what the logs contain: no
  credential may be logged, since a grantable capability (G12) puts them in
  more hands than the host's owner.
- Default for the runner flag and the capability's default grant: open (O6).

## Pillar D — ephemeral runners (later)

The server calls a provisioner to create a runner, sends it the task, and on
completion or timeout cancels the task and reclaims the resource (operator,
2026-11-12). A, S, B and C must leave room for it:

- **Admission must be able to wait for a runner that does not exist yet.**
  Background admission already does (G26): a task with no candidate waits in
  the queue. D adds a provisioner call where that wait begins; the placement
  function (pillar 0) is the one place it would go.
- **The end of a background task is the trigger for reclamation.** G3/G4/G5
  give D its signal.
- **Provisioned resources are recorded durably** and reconciled on server
  start, so a crash cannot orphan a paid resource (anti-goal).
- **The server gains the power to enroll runners** (`2026-10-27-runner-enrollment-design.md`).
  That moves a security boundary; D's spec treats it explicitly.
- **Roots become a property of the provisioned environment**: the provisioner
  gives the runner the task's repo as its root, which exact matching (G9)
  serves.
- Existing material: `scripts/kvm-runner/kvm-runner.py`.

## Open questions for the operator

Settled in the pillar specs; listed here so none is decided by default.

| # | Question | Pillar |
| --- | --- | --- |
| O3 | Firings missed while the server was down: run once, or skip | S |
| O5 | Cache fetched transcripts on the server, trading server storage for availability while a runner is down | B |
| O6 | Default of the runner's log-file and transcript flags, and who holds the new log-read capability by default | B, C |
| O7 | Size limit and retention of PTY records (G11): per-run cap, total cap, how long a finished run's record is kept | A |
| O8 | The server-wide default for PTY recording (G11): on or off, and whether it differs between foreground and background | A |

## Left to the pillar spec authors

Wire shapes, field names, flag spellings, verb names, and the per-surface
changes, provided they stay inside the decisions above. A pillar spec that
needs to reverse a decision here records the reversal in its own Decisions
table, naming the row it replaces and what moved it.
