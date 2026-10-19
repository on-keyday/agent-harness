# Task progress and window title in task lists — design

## Problem

- **P1.** A task's window title never reaches a task list. The server holds it:
  every live terminal session has a `vtgrid.Terminal` (`SessionMux.screen`,
  `server/session_mux.go:186-196`) that parses OSC 0/2 from the first byte,
  survives a deliberate restart through `heldCapture`/`loadScreen`, and
  re-emits the title in every attach repaint (`vtgrid/repaint.go:128`). But
  nothing in `server/` reads `Title()` into `TaskInfo` or `TaskStatusEvent`, so
  the only way to see it is to attach or run `session snapshot`.
- **P2.** The title is not what the agent is doing now. Claude Code generates
  it from the first prompt (operator's observation), and the work drifts away
  from that prompt as the session goes on. There is no way for an agent to say
  "I am on task X, at stage Y" where a human looking at the list can see it.
- **P3.** An agent supervising another agent can only learn its stage by
  agentboard messaging, which costs both sides a turn. There is nothing to read
  on demand and nothing to wait on short of a board message.
  `session await-idle` waits for output quiescence, not for a change of stage.
- **P4.** `notify` takes the sender's identity from the client. `harness-cli
  notify` copies `HARNESS_TASK_ID` / `HARNESS_RUNNER_ID` / `HARNESS_REPO_PATH`
  / `HARNESS_HOSTNAME` into `NotifyRequest.worker` (`cli/notify.go:38-55`), and
  `handleNotify` forwards it unchecked (`server/task_handler.go:2437-2484`). Any
  connection can notify in another task's name. It predates per-connection
  principals: the env path landed 2026-06-08 (eee75f1f), `lookupPrincipal` after
  2026-06-18.
- **P5.** `tasks.status` and `task.<id>.status` are published to every
  subscriber with no visibility filter (`server/server.go:461-462`, plain
  `Publish`), while `conns.status`, `forwards.status` and `execs.status` use
  `PublishFiltered` with the same predicate as their listings
  (`server/server.go:533`, `:626`, `:632`). Today the event carries only ids,
  status and counters. Adding text to it (P1, P2) without a filter would hand
  that text to connections whose `ls` would not show the task.

## Decisions taken

**operator** = the human chose it in conversation (2026-10-19). **this spec** =
the author chose it while writing; those rows are the ones to check.

| # | Decision | Decided by |
| --- | --- | --- |
| D1 | The reader is a human scanning a task list (TUI / WebUI / CLI `ls`), and secondarily an agent supervising another | operator |
| D2 | Two separate fields: the window title (written by the agent PROGRAM, e.g. Claude Code) and a self-reported progress line (written by the agent, in its own words: which task, which stage). Neither replaces the other | operator |
| D3 | The agent writes progress explicitly with a `harness-cli` verb, prompted by the harness-cli skill. Not derived from Claude Code's todo/task tools (those are off by default on current models — code.claude.com/docs/en/tools-reference, "Task tool availability") | operator |
| D4 | Progress is never cleared automatically when the agent goes idle. Lists show it next to its age and the session's last-output age, and the reader judges staleness | operator (accepted the author's recommendation) |
| D5 | The verb is named `progress` | operator |
| D6 | `notify` derives the sender identity on the server, as `progress` does (§ notify) | operator |
| D7 | The progress write targets the caller's PRINCIPAL task (`lookupPrincipal`), never a task id supplied by the client. A connection with a zero principal (operator) is refused | this spec (operator asked for notify to match, D6) |
| D8 | `tasks.status` / `task.<id>.status` become filtered by the same predicate as `handleList` (§ Events) | this spec; operator did not object when told it was included |
| D9 | A one-shot watcher that fires on the target's next progress write, shaped like `await-idle` | operator asked for "something like await-idle"; the shape is this spec |
| D10 | CLI spelling: `harness-cli progress <text>...` writes; `harness-cli progress --clear` clears; `harness-cli await-progress [--notify] [--topic T] <task-id>` arms, with `await-progress ls` / `await-progress kill`. Top-level rather than the `session await-progress` shown in conversation, because a oneshot task has no session and may report progress too | this spec |
| ~~D11~~ | ~~Progress is stored (WAL record `task_progress`), survives restart, resume and task end~~ — **withdrawn** 2026-10-19 after landing, replaced by D26: a WAL record per write grew the log without bound, and the line is state, not history | this spec, withdrawn by operator |
| D11′ | Title is live-only: read from the mux at List / event time, empty when there is no live terminal session. Progress disappears with prune | this spec |
| D12 | Progress text: at most 256 bytes, valid UTF-8, no C0 / DEL / C1 control characters. A violation is refused, not sanitized, so the agent learns its text did not land | this spec |
| D13 | Title on the wire: C0 / DEL / C1 removed and truncated to 256 bytes on a UTF-8 boundary, server-side | this spec |
| D14 | Parent-hop rows (`redactParentTaskInfo`) lose progress and title. They are content, like the prompt that row already loses, not liveness | this spec |
| D15 | One watcher registry for both conditions (idle, progress). Wire kinds `await_idle_list` / `await_idle_kill` are renamed `await_list` / `await_kill` (positions unchanged) and their rows gain a `condition` | this spec |
| D16 | No skew guard and no compat shim. Rollout: server first, then every client and the runner host's `harness-cli` (`make build` + restart) | this spec |
| D17 | `await-progress` takes a baseline `since_progress_at`: it fires at once when the stored `progress_at` is newer, so a write landing between the caller's `ls` and its arm is not missed. 0 = "the next write after arming". The response carries the firing text | this spec (builder check) |
| D18 | D8 promises exactly "`tasks.status` / `task.<id>.status` deliver to the subscribers `ls` would show the task to". It does not cover the `notifications` topic, which is unfiltered today for every notify (`server/server.go:375-381`, plain `Publish` plus a 64-entry replay ring). The notify sink of `await-progress` includes the progress text, like any other notify | this spec (builder check) |
| D19 | `task_pruned` stays unfiltered. It carries only the id and kind, and the filter cannot be evaluated after the entry is gone (`OnPrune` runs after removal), so a filtered prune would leave stale rows in confined clients | this spec (builder check) |
| D20 | notify's hostname comes from the runner registry entry of the task's `AssignedTo` (`server/registry.go:47`, from the authenticated `RunnerHello`), not from the agent hello, whose hostname the client supplies. Empty when that runner is not connected | this spec (builder check) |
| D21 | A presence bit `live_terminal` on `TaskInfo` and `TaskStatusEvent` separates "live terminal session, untitled" from "no live terminal session"; clients gate the title on it rather than re-deriving it from status and kind | this spec (builder check) |
| D22 | The TUI's last column (was Prompt, `<interactive>` for every session) becomes **Doing**: the progress line with its age, else the title prefixed `(title) `, else the prompt. Replaces the matrix's earlier "one more column, title rendered dim": a new column squeezes Repo/Agent at 120 columns, and bubbles truncates a cell by width, which can cut an ANSI style mid-sequence | this spec (implementation) |
| D23 | `progress_at` only ever increases: `SetProgress` stamps under the store lock (a later write never stores an earlier clock reading) and writes the WAL record under the same lock. A stored stamp ahead of the server clock reports age 0 | this spec (mid-plan review) |
| D24 | Clients age `progress_age_ms` locally from the receipt time, as the idle badge is, and every surface strips control characters from progress and title itself — the event path can carry bytes a subscriber published (see Non-goals) | this spec (final review) |
| D25 | The WebUI arms `await-progress` from each non-terminal task's sheet (oneshot included) as well as from the attached-session button; the WebUI passes `progress_at` as a decimal string, since a nanosecond stamp does not fit a JS number | this spec (final review) |
| D26 | Progress is memory-only. A DELIBERATE stop (held or full) writes every task's line to `<data-dir>/progress.json` beside the held captures; the next server applies it after the WAL replay and deletes it. A crash loses the lines | operator (chose this over WAL-per-write and over WAL-at-stop; "a crash loses them" accepted) |
| ~~D28~~ | ~~A cleared line shows nothing on the human surfaces~~ — **withdrawn**, replaced by D29: the author misread the problem; seeing that a line was cleared is wanted, being unable to tell the marker from a task that wrote "(cleared)" was the defect | operator, withdrawn by operator |
| D29 | A cleared line stays visible, unambiguously: CLI `ls` and the TUI detail print `progress="" (age)` (the quotes separate it from a written "(cleared)"); the WebUI draws a gray italic "(cleared)" marker as its own styled element (`task-progress-cleared`), while any text a task writes keeps the normal color; the TUI Doing column falls back to the title / prompt | operator |
| D27 | Another task's line can be EMPTIED (never written) by a caller holding `cancel` with the target in cancel's scope — whoever may end the task may empty its status line; the operator holds every bit. New kind `progress_clear {task_id}`, answered with `ProgressSetResponse`; out of scope is `not_found`, a missing bit `permission_denied`. It acts as the task's own `--clear` (fresh stamp, event, watchers fire). Surfaces: `progress --clear --task <id>`, TUI `X`, WebUI sheet "🧹 進捗を消す". Replaces the Non-goal "writing another task's progress, including by the operator" for the clearing half | operator (the operator-only first draft was withdrawn: it had no reason beyond not touching the caps model) |

## Non-goals

- A history of progress lines. The board is the place for a log.
- Writing another task's progress, including by the operator. (Emptying it is
  allowed, under D27.)
- Fixing `await-idle` for agents that spin the title while blocked on input
  (the output never goes quiescent). Raised in the same conversation; deferred
  by the operator.
- The pubsub hole where any subscriber can publish onto a topic it joined
  (`pubsub/pubsub.go:168`, the reader goroutine in `Subscribe` calls
  `ps.Publish` with whatever the subscriber writes). It means a displayed
  progress line can be forged on the event path until the next `ls` snapshot
  corrects it. Out of scope here; recorded so nobody reads D8 as closing it.
- Filtering the `notifications` topic (D18).

## Wire (`runner/protocol/message.bgn`)

All enum additions are appended; every enum here is positional.

### TaskInfo

One of the existing spare bits becomes the presence bit (D21):

```
    is_attached :u1
    skills_injected :u1
    live_terminal :u1   # the task has a live session mux WITH a terminal; the
                        # title field below is meaningful only when set
    reserved :u5
```

After `hold_deadline_ns`:

```
    progress_len :u16
    progress :[progress_len]u8   # self-reported by the task (D2). Empty =
                                 # never set or cleared; progress_at separates them.
    progress_at :u64             # unix nanos, SERVER clock, of the last write
                                 # (set or clear). 0 = never written.
    progress_age_ms :u64         # server-computed now - progress_at; consumers
                                 # use this, not their own clock, as with
                                 # output_idle_ms. Meaningless when progress_at == 0.
    title_len :u16
    title :[title_len]u8         # live window title from SessionMux.screen (D11,
                                 # D13). Read only when live_terminal = 1.
```

`ListResultBody` already travels on a send stream, so the extra bytes do not
touch the datagram budget.

### TaskStatusEvent (after `cowriters`)

The same progress and title fields plus `live_terminal :u1` and `reserved :u7`, stamped on
EVERY task event, as `last_output_at` and the observer counts are. A title change does not emit an event by itself (Claude
Code rewrites the title on every spinner tick); it rides the next event,
including the `task_activity` busy/idle edges.

`StatusEventKind` gains `task_progress`: published after every successful
progress write.

### Progress write

```
TaskControlKind += progress_set

format ProgressSetRequest:
    clear :u1          # 1 = clear; text must then be empty
    reserved :u7
    text_len :u16
    text :[text_len]u8

enum ProgressSetStatus:
    :u8
    ok = 0
    no_principal = 1   # caller is not an agent connection (operator)
    bad_text = 2       # > 256 bytes, invalid UTF-8, a control char, or clear=1 with text
    not_found = 3      # principal's task is no longer in the store

format ProgressSetResponse:
    status :ProgressSetStatus
    progress_at :u64   # the stamp written; 0 unless ok
```

No capability (added to the no-cap set beside `whoami`): it writes only the
caller's own row. The request is at most ~270 bytes, inside one datagram.

### Await-progress

```
TaskControlKind += await_progress
rename AwaitIdleSink -> AwaitSink      # shared by both conditions
rename await_idle_list -> await_list, await_idle_kill -> await_kill  (D15)

enum AwaitCondition:
    :u8
    idle = 0
    progress = 1

format AwaitProgressRequest:
    task_id :TaskID
    since_progress_at :u64   # D17: fire at once if the stored progress_at is
                             # newer; 0 = wait for the next write
    sink :AwaitSink
    topic_len :u16
    topic :[topic_len]u8

enum AwaitProgressStatus:
    :u8
    fired = 0          # the target's progress_at is newer than the baseline
    armed = 1          # sink=notify/board registered
    task_ended = 2     # target is terminal at arm time, or ended / was pruned while armed
    not_found = 3      # unknown or not visible
    bad_request = 4
    cancelled = 5

format AwaitProgressResponse:
    status :AwaitProgressStatus
    progress_at :u64   # the firing write's stamp; 0 otherwise
    progress_len :u16
    progress :[progress_len]u8   # the firing text; empty otherwise (D17)
    watcher_id :u64

AwaitIdleWatcherInfo += condition :AwaitCondition   # threshold_ms is 0 for progress
```

### NotifyRequest (D6)

`origin` and the `worker` union arm are removed from `NotifyRequest`; it keeps
`level`, `title`, `text`. `NotifyEvent` is unchanged and the server fills its
`origin` / `worker` (§ notify).

## Server

### Progress

- `TaskEntry` gains `Progress string` and `ProgressAt int64`, memory-only
  (D26). `TaskStore.SetProgress(id, text, at)` stamps monotonically (D23) and
  writes nothing to the WAL. `server/progress_snapshot.go` writes and loads
  `<data-dir>/progress.json`; the write is a deferred hook on `serve` gated on
  ctx cancellation OR `shuttingDown`, because a held stop leaves the loop
  through the http-server exit before the root context is cancelled.
- `handleProgressSet` takes the principal from `lookupPrincipal(cid)`, validates
  the text (D12), calls `SetProgress`, publishes `task_progress`, then fires any
  progress watchers on that task.
- `toTaskInfo` maps the stored fields. `redactParentTaskInfo` clears progress
  and title (D14).

### Title

- `SessionMux` gains `Title() string`, read under `screenMu`, returning "" for a
  mux without a terminal (`m.terminal == false`, the event-stream kind).
- The sanitizer (D13) lives in `vtgrid` beside `titleOSC`, which already drops
  C0 and DEL, so the repaint and the wire share one rule.
- `handleList`'s live enrichment and `publishTaskEvent` both set it, at the
  point where they already read `LastOutputUnixNano`.

### Events (D8)

`publishTaskEvent` filters with the `handleList` rule. Because a parent-hop
subscriber must see a redacted row (D14), each subscriber gets one of two
payloads, chosen per subscriber by `pubsub.PublishSelect` (taps receive the
full payload once):

1. the full payload, to subscribers where `visibleToCaller` returns `all` or
   the task is in `allowed`;
2. a payload with progress and title emptied, to subscribers whose
   `listVisibleToCaller` `parentHex` equals the task.

(Planned as two `PublishFiltered` calls; implemented as one `PublishSelect` so
a tap does not see every event twice.)

The operator passes (1) for every task, so operator clients see no change in
what they receive. `task_pruned` is the exception and stays a plain `Publish`
(D19).

### Watchers (D9, D15)

- `idleWatcher` gains `condition`. The registry, visibility predicate
  (`idleWatcherVisibleTo`), list, kill and `DropIdleWatchersForConn` are shared.
- `handleAwaitProgress` follows `handleAwaitIdle`: target visible through
  `inScope(…, Capability_None, …)`; sink=notify requires `notify`; sink=board
  requires `Board.Publishable` for a non-operator. Unlike idle it needs no live
  mux, so a oneshot task can be watched.
- Firing: at arm time, a target whose stored `progress_at` exceeds a non-zero
  `since_progress_at` fires immediately (D17). Otherwise the next
  `handleProgressSet` on the target (set or clear) fires every progress watcher
  on it. The target going terminal or being pruned fires `task_ended`. A target
  already terminal at arm time answers `task_ended`, after the baseline check,
  so a final write before the end is still delivered.
- The CLI's `await-progress --since <progress_at>` sets the baseline from a
  value read off `ls --json`; without the flag it is 0.
- Board sink payload:
  `{"kind":"task_progress","task":"<hex>","progress":"<text>","progress_at_unix_ms":N,"status":"fired|task_ended"}`.
- Not persisted, like idle watchers.

### notify (D6)

The request no longer carries any identity. For a non-zero principal
`handleNotify` sets `origin = worker` and fills `WorkerInfo` from: the
principal (task id), the connection's `boardState` identity (runner id, which
the auth ticket check validated), the runner registry entry of the task's
`AssignedTo` (hostname, D20), and `TaskEntry.RepoPath`. The agent hello's
hostname is not used: the client fills it from `HARNESS_HOSTNAME`
(`server/agent_handler.go:83`) and nothing checks it. A zero principal is `origin = external`, as an operator
notify is today. `cli/notify.go` stops reading the env; its size guard still
measures the request.

## Surfaces

Walk `.claude/skills/surface-parity-checklist` item by item; the matrix below
is the minimum, not a substitute.

| Surface | Progress / title display | Write | Await-progress |
| --- | --- | --- | --- |
| CLI `ls` text (`cli/list.go` `taskLine`) | `progress="…" (12m ago)` once written, a cleared line as `progress="" (…)` (D29); `title="…"` when `live_terminal` is set, even if empty | — | — |
| CLI `ls --json` / `session ls` (`taskJSON`) | `progress`, `progress_at`, `progress_age_ms`, `live_terminal`, `title`, always present | — | — |
| CLI verbs | — | `progress`, `progress --clear`; `progress --clear --task <id>` empties another task's line (D27) | `await-progress`, `ls`, `kill` |
| TUI task table (`tui/tasks.go`) | the Doing column (D22): progress with its locally aged age (D24), else `(title) …`, else the prompt | — | `m` arms it (`M` via notify); the `I` modal lists both conditions with a "waits for" column |
| TUI detail popup (`tui/detail.go`) | both, with progress age next to last-output age | — | — |
| TUI cmdline (`tui/cmdline.go`) | — | — | `await-progress` wherever `await-idle` is accepted |
| WebUI task list (`webui/static/main.js` `renderTaskList`) + task sheet | progress line under the row, title next to it; age shown | — | the attached-session button and every non-terminal task's sheet (D25); the watcher panel shows the condition |
| WebUI cmd input (`runCmd`) + wasm bridge | — | — | `await-progress` wherever `await-idle` is accepted |

Writing progress has no TUI / WebUI surface: the operator's connection has no
principal (D7), so it would always be refused. That omission is deliberate.
Emptying one does (D27): TUI `X` on the selected task, and the WebUI task
sheet's "🧹 進捗を消す" when the task has a non-empty line.

Every rendering of progress and title goes through the surface's existing
escaping for untrusted text; the server-side rules (D12, D13) are not a
substitute for it.

## Agent skill

`runner/agentskills/harness-cli/SKILL.md` gains a section on `progress`: write
it when the task or stage changes, in one line, naming the task and stage; it
is visible to the operator and to any task that can see yours; `--clear` when
the line no longer applies. How often is left to the agent. The copies in
`.claude/skills/harness-cli/` and `.agents/skills/harness-cli/` are kept
byte-identical. `runner/agentskills/supervising-workers/SKILL.md` gains
`await-progress` and the new `ls --json` fields beside its `last_output_at`
text.

## Rollout and verification

- Server first, then clients and the runner host's binaries (D16). Run
  `scripts/wire-skew-check.sh`; it asserts a skewed pair fails recoverably, not
  that it works.
- An old `harness-cli notify` against the new server sends bytes the new
  `NotifyRequest` does not describe. That is the accepted skew cost of D16.
- A WAL written by a build that still logged `task_progress` (landed as
  4d1735e2) replays without error: the record type is unknown to the replay
  switch, which has no `default`, so it is skipped and those lines are lost.
- Tests at the operator's entry layer: the `progress` and `await-progress`
  argv as the help prints it, against `scripts/dummy-harness.sh`
  (implementation-pitfalls, Pitfall 13).
- A test that a notify sent with a forged `HARNESS_TASK_ID` and
  `HARNESS_HOSTNAME` is attributed to the real principal and the runner's
  registered hostname.
- A test that `await-progress --since` fires at once for a write made before
  arming, and that a write made just before the target ends is delivered
  rather than `task_ended`.
- A test that a subscriber outside a task's visibility receives no
  `task_progress` event for it, and a parent-hop subscriber receives one with
  progress and title empty.
