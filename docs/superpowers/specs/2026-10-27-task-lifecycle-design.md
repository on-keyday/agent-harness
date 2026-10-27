# Task lifecycle and program status in task lists — design

Line references are to `main` at `4c5d2cd7`.

## Problem

- **P1.** A human scanning a task list cannot tell *why* a running agent is
  quiet. Lists show `output_idle_ms` (byte quiescence) and the self-reported
  `progress` line ([2026-10-19-task-progress-design.md](2026-10-19-task-progress-design.md)).
  Neither separates the four cases the operator acts on differently:
  - the model is thinking or running a long tool (leave it);
  - it is blocked on a permission prompt or a question (go answer);
  - its turn ended and it waits for the next instruction (go instruct);
  - it stopped on an API error.

  The operator's next action in the second and third cases already exists:
  attach and type. What is missing is knowing that a given task is in one of
  them.
- **P2.** `progress` cannot fill this gap. The agent writes progress when it
  decides to, and an agent blocked on a prompt cannot write anything.
- **P3.** Claude Code (2.1.295 and later, verified in the 2.1.311 binary)
  reports its own state as OSC 7501 ("Program Status Protocol") into the PTY
  stream. It does so only after the terminal answers its startup probe
  `OSC 7501 ; ?`. In our live path the operator's terminal answers terminal
  queries (Windows Terminal or xterm.js), and neither knows 7501, so claude
  never emits it today. The server will answer it instead (D18). Prior art and the binary reading are recorded in the
  operator's notes on Orca (`src/main/agent-hooks/README.md`,
  `src/shared/program-status/protocol.ts` in stablyai/orca `4c66f91`).

## Decisions taken

**operator** = the human chose it in conversation (2026-10-27). **this spec** =
the author chose it while writing; those rows are the ones to check.

| # | Decision | Decided by |
| --- | --- | --- |
| D1 | The reader is the operator scanning a task list to spot a task that is stopped and needs them. The follow-up action (attach, instruct) is out of scope because it exists | operator |
| D2 | **Lifecycle** is a new field beside `progress`. Agent hooks write it mechanically, without the agent's judgment. Progress stays the agent's own words. Neither replaces the other | operator |
| D3 | **Program status** (OSC 7501, observed in the PTY stream) is a SEPARATE field. It is a sibling of `title` (observed and live-only), not merged into lifecycle. No precedence rule between the two: a disagreement is shown, not resolved | operator |
| D4 | The hooks are added to the existing worktree `.claude/settings.json` injection (`harnessHookEntries`, `runner/settings.go:34`), not to the user's global config. Only a task's worktree carries them, so they are inert everywhere else | operator (pointed out the existing injection) |
| D5 | Claude Code only in this change. Other agents report nothing; their lifecycle stays "never written" | operator |
| D6 | A reporting `Stop` hook is safe. The objection in `WriteAgentSettings`' doc comment is about a re-entry Stop hook that blocked stdin. Wakes now arrive as ordinary user turns, so a Stop hook that reports and exits 0 does not interact with them | operator |
| D7 | Working name of the verb: `harness-cli lifecycle <state>`. The operator said "lifecycle みたいなの"; rename freely before landing | this spec |
| D8 | States: `working`, `waiting` (blocked on the human inside the turn: permission or question), `idle` (turn ended, waiting for the next prompt), `error` (turn ended on an API or model error). Plus the implicit "never written" (`lifecycle_at == 0`). Orca's turn-outliving `blocked` (sign-in) is left out: no claude hook reports it | this spec |
| D9 | Hook mapping (claude event → state), all hooks `"async": true`. `SessionStart` → idle. `UserPromptSubmit` → working. `PermissionRequest` → waiting. `Elicitation` → waiting. `PreToolUse` matcher `AskUserQuestion` → waiting (D22). `PostToolUse` → working (the only signal that a waiting prompt was answered). `Stop` → idle. `StopFailure` → error. `PostToolUseFailure` → working. `SubagentStart` → working. `SubagentStop` → idle. Events from a subagent are NOT dropped for `PermissionRequest` / `Elicitation`: the human still has to answer them. The CLI folds these events into a per-task state (D26), so the word in each hook's command line is only the fallback | this spec, amended by D26 (operator) and the final review (PostToolUseFailure) |
| D10 | Hooks run `async` (claude's hook option "runs in background without blocking", verified in the 2.1.311 schema), so a dial to a remote server never delays the agent. The cost is reordering: two async hooks can land out of order. The CLI stamps `reported_at` (unix nanos, runner-host clock) when the hook process starts. The server drops a report whose `reported_at` is older than the stored one. Between resumes, every report for one task comes from the same host, so cross-host skew does not apply (D23 covers a resume) | this spec |
| ~~D11~~ | ~~The CLI keeps the last state it sent; the file is an optimization only~~ — **withdrawn** after the final review. It recorded the word of a report the server answered `stale`, so a later real report of that word was skipped and a whole turn could read `ready`. Replaced by D11′ | this spec, withdrawn |
| D11′ | `PostToolUse` fires on every tool call, so a repeat of the last ACCEPTED report is skipped. The per-task state file (D26) records a report only on `ok`, and only when its stamp is newer than the recorded one. A `stale` answer, or a send whose outcome is unknown, clears the record, so the next report is sent. The reporter stamp is taken INSIDE the state lock, so the order hooks compute states is the order the server sees stamps | this spec (final review) |
| D26 | **Subagents keep a task `working`.** Measured on claude 2.1.311 (2026-10-27): launching a subagent ends the parent's turn (`Stop`, no `agent_id`) right away. The subagent then works on its own and hands back in a new turn (`UserPromptSubmit` … `Stop`). Reporting each hook's own word showed `ready` while the subagent worked. The CLI therefore reads each hook's stdin (`hook_event_name`, `agent_id`, `is_interrupt`) and keeps per-task state under an exclusive lock: the live subagents and whether the parent is inside a turn. `SubagentStart` adds a subagent. `SubagentStop` removes one (a stop for a subagent never seen changes nothing) and reads `idle` only when none remain and the parent's turn is over. `Stop` reads `idle` only when no subagent is live. `SessionStart` resets. The state file lives under the user cache dir. Any error with it runs on a fresh state and still sends | operator (approved the approach, 2026-10-27) |
| D27 | **The hook command names the agent: `harness-cli lifecycle --hook claude <state>`.** Which agent's JSON is on stdin is never guessed from the JSON or from env: a nested agent inherits the env, and one agent can replay another's hook config. Without `--hook` (a hand-run command, a bash task) stdin is not read and the argv word is reported. Adding another agent means its own `--hook` value, its own stdin reading and its own event table | this spec (operator asked how agents would be told apart) |
| D12 | The write targets the caller's principal task, like `progress_set`. No capability. An operator connection is refused | this spec (mirrors progress D7) |
| D13 | Storage: memory-only, carried across a DELIBERATE restart by the existing `<data-dir>/progress.json` snapshot. Each entry gains `lifecycle`, `lifecycle_at` and `lifecycle_reported_at`, and the file name is unchanged. The snapshot's entry filter (`progressLines`, `server/taskstore.go:401`, today `ProgressAt != 0`) widens to `ProgressAt != 0 \|\| LifecycleAt != 0`. A crash loses it. A held task keeps running across a held restart, so its lifecycle must survive one. `SessionStart` (D9) resets it whenever claude itself restarts | this spec |
| D21 | `SessionStart` uses the matcher `startup\|resume\|clear`. It fires with `source=compact` after an auto-compact in the middle of a turn, which would show `ready` while the model keeps working. `harnessHookEntries` therefore gains a `Matcher string` field, and `mergeHarnessHooks` writes it when non-empty. Dedup stays (event, command) | this spec (builder check) |
| D22 | Claude's own question tool `AskUserQuestion` gets its own hook: `PreToolUse` with the matcher `AskUserQuestion` → waiting. Its `PostToolUse` (after the answer) → working through the generic hook. The operator runs claude in auto mode, where permission dialogs rarely appear, so an explicit question is the main source of `wait`. Orca records only that it "can arrive as PermissionRequest" (`src/main/agent-hooks/server/server-claude-status-rules.ts:85` at `4c66f91`); "can" is no guarantee in auto mode. `PreToolUse` fires before the tool runs, whatever the permission mode. If the question is dismissed, claude may omit the final hook (Orca `server-status-inference.ts:224`), and `wait` then stays until the next prompt, as D25 (2). The dummy E2E includes one `AskUserQuestion` run in auto mode | operator (runs auto mode) + this spec |
| D23 | D10's "same host" premise is false across a resume: a resume can bind to a different runner (`handleSubmitResume`). The stored reporter stamp is reset to 0 when a task is resumed, so the new host's first report is never dropped as stale | this spec (builder check) |
| D24 | Every task kind gets the same lifecycle hooks, event-stream included. `WriteAgentSettings` already injects identically for oneshot (`handleAssign`), interactive PTY and event-stream (both `handleOpenExec`, `runner/session.go:969`), and nothing changes there. Unverified: whether `PermissionRequest` fires for event-stream claude (`--permission-prompt-tool stdio`). If it does not, only `wait` is missing for those tasks; `working`/`ready`/`err` still come from the other hooks. The E2E checks this for one event-stream task. **Result (2026-10-27):** `waiting` appeared for an `AskUserQuestion` in an event-stream task. A permission prompt in stream mode was not exercised, so it remains unverified. A first draft excluded event-stream; that was withdrawn because "unverified" is a reason to verify, not to exclude. If `wait` turns out missing there, the follow-up is to have the stream adapter report it from its pending table, and that needs its own design, since it makes a second writer | this spec |
| D19 | Display words differ from wire names so they cannot be confused with the existing output-activity badge (`busy` / `idle:3m`, `cli.ActivityStr`). `waiting` → `wait`, `idle` → `ready`, `error` → `err`. ~~`working` and unset show the existing activity badge unchanged~~ — **amended by D28**: `working` → `work`, and unset is shown too. An interrupted turn stuck at `working` (D15) still reads, because the activity badge stays beside the lifecycle one (`work:12m` next to `idle:12m`) | this spec, amended by D28 (operator) |
| D28 | **Every lifecycle state is on the list, beside the program badge.** Reversal of D19's hiding and of the Phase 2 "WebUI task sheet only" row. What moved it: the operator found the WebUI fragmented. The sheet repeated the row's badge (`ready:42s` on the row, `lifecycle ready:42s` under the id), and the program status sat on a third line of plain text. Nothing showed at all unless the task was `ready`, `wait` or `err`. That last point also hid the fleet-wide failure seen on 2026-10-27, when runners older than the hooks left every task unreported and the lists looked the same as before. Now: (1) `work:5s` / `wait:3m` / `ready:12m` / `err:30s`, and `lc:-` for a live task that never reported (`lifecycle=-` in `ls`, `- (never reported)` in the TUI detail popup); nothing for a finished task (D14 holds). (2) The WebUI row shows the program badge right after it (`prog blocked:question`, kind only with blocked, the full line with msg as its tooltip). The sheet no longer repeats either. (3) Colors: red for `wait`/`err`/`prog blocked`/`prog error`, default blue for `ready`, gray for everything else. (4) The TUI gives lifecycle its own `Life` column before `Act`. Act keeps the activity badge, so the D15 reading stays possible. Life comes and goes with `Obs` on the same panel-width threshold; below it the Act cell carries the lifecycle badge, since there is room for only one | operator (1)–(3), 2026-10-27; this spec (4): it keeps D15 readable in a one-cell column |
| D20 | Lifecycle survives parent-hop redaction (`redactParentTaskInfo`). It is liveness like `output_idle_ms`, not content like progress and title | this spec |
| D14 | Lifecycle is shown only while the task is non-terminal. It is not cleared when the task ends. Surfaces stop rendering it, so a finished task does not read "working" | this spec |
| D15 | Known gap, narrowed by the final review: an Esc while a TOOL runs fires `PostToolUseFailure` with `is_interrupt`, which ends the turn (measured: `idle` within ~2 s). An Esc while the model is still thinking fires no claude hook, so the field stays `working` until the next prompt. Surfaces show the lifecycle age next to the existing output-idle age, and the reader judges (same stance as progress D4) | this spec |
| D25 | Lifecycle is last-write-wins: any `PostToolUse` sets `working`, even while a permission prompt raised elsewhere is still open. Known gaps, accepted: (1) with subagents running in parallel, a sibling's finished tool hides an open `wait`. This is rare in practice: the operator runs auto mode, where permission prompts seldom appear, and a pending question blocks the turn that asked it; (2) a denied permission fires nothing, so `wait` stays until the next prompt (claude is then at its input prompt, so "go to it" is still the right reading). Today nothing distinguishes these states at all, so the gaps are not regressions. If (1) bites, the next step is a `wait` held until its own tool finishes, correlated per (agent_id, tool_name) in the CLI's local file. `PermissionRequest` carries no `tool_use_id` (Orca `server-claude-status-rules.ts:211`) | operator ("今区別してました?": nothing distinguishes them today) |
| D16 | No automatic notification on any lifecycle edge. Notifications stay on demand, per the operator's standing preference. A watcher (`await-lifecycle`, shaped like `await-progress`) is a natural follow-up and out of scope here | this spec |
| D17 | Program status (D3) is parsed by the server's `vtgrid.Terminal` beside the title, from the root record only (`id` absent): state (`idle\|working\|done\|blocked\|error`), kind (`permission\|question\|auth`, only with blocked), and `msg` (base64, decoded, sanitized like the title, cut to 256 bytes). Child records are ignored. Live-only, like the title | this spec |
| D29 | **No program status on a Windows runner, for now.** Lifecycle works there; the OSC 7501 report never starts. Measured 2026-10-27 on Windows 10.0.26200 with claude 2.1.311: (1) claude sends DA1 (`CSI c`) right after `OSC 7501 ; ?` as a sentinel, and settles "unsupported" when the DA1 answer comes first (read in claude's bundled JS); (2) the OS inbox ConPTY (go-pty calls `CreatePseudoConsole` with flags 0) answers DA1 itself, at +0 ms with nobody attached; (3) the server's reply arrives after a network round trip, so it always loses. The raw reply is also dropped by conhost's input parser; the same bytes sent as win32-input-mode key records (`CSI 0;0;<byte>;1;0;1 _`) do reach the program, but too late. Rejected for now: shipping Windows Terminal's `conpty.dll` + `OpenConsole.exe` through a go-pty fork (distribution and regression cost, and it is unverified whether that ConPTY forwards DA1), and the server reply in key-record form alone (no effect while DA1 wins). Revisit if claude gains a way to force program status on (an env variable), or if the ConPTY in use stops answering DA1 itself. The surfaces need no change: a program that never reports shows no `prog` badge (D28) | operator, 2026-10-27 |
| D18 | The server's `SessionMux` answers the 7501 probe for every terminal session, attached or not. The operator's terminals do not answer it, and claude probes only once, at startup, often while nobody is attached. Accepted consequence: claude's title loses its `◐`/`✳` prefix everywhere, the operator's tab included | operator (chose the server over the client after "the local terminal does not answer") |

## Non-goals

- Agents other than Claude Code (D5).
- A history of lifecycle transitions.
- Notifying on a lifecycle edge (D16).
- Reconciling lifecycle with program status (D3).
- Detecting an interrupt (D15).
- `--no-worktree` tasks. They get no injected settings (unless
  `--force-inject-harness-settings`), so their lifecycle is never written.

## Phase 1 — lifecycle

### Wire (`runner/protocol/message.bgn`)

Appended, like every enum change here:

```
TaskControlKind += lifecycle_set

enum LifecycleState:
    :u8
    unset = 0      # never written
    working = 1
    waiting = 2
    idle = 3
    error = 4

format LifecycleSetRequest:
    state :LifecycleState      # unset is refused (bad_request)
    reported_at :u64           # unix nanos, reporter's clock (D10)

enum LifecycleSetStatus:
    :u8
    ok = 0
    no_principal = 1
    bad_request = 2
    not_found = 3
    stale = 4                  # reported_at older than the stored one; dropped (D10)

format LifecycleSetResponse:
    status :LifecycleSetStatus
    lifecycle_at :u64          # SERVER-clock stamp written; 0 unless ok
```

`TaskInfo` gains the following after the title. `TaskStatusEvent` gets the same fields after its progress block:

```
    lifecycle :LifecycleState
    lifecycle_at :u64          # SERVER clock of the last accepted write. 0 = never
    lifecycle_age_ms :u64      # server-computed, as progress_age_ms
```

`StatusEventKind += task_lifecycle`, published after every accepted write.
`stale` is not an error to the hook: exit 0 silently.

### Server

`server/lifecycle.go` follows `server/progress.go` (`handleProgressSet` at
`:45`):
- The principal comes from `lookupPrincipal`.
- `TaskStore.SetLifecycle(id, state, reportedAt, at)` compares `reportedAt` against
  the stored reporter stamp under the store lock (D10). `lifecycle_at` is the
  server clock, monotonic, as progress D23.
- The handler publishes `task_lifecycle`.
- Event visibility follows progress (`PublishSelect`). A parent-hop row loses
  progress and title but keeps lifecycle (D20).

### Runner

Append the D9 hooks to `harnessHookEntries`. Each command starts with
`harness-cli lifecycle `, so `pruneStaleHarnessHooks` manages it.

`harnessHookEntries` gains an `Async bool` field, and `mergeHarnessHooks`
writes `"async": true` for those entries. Today it writes only `type` and
`command` (`runner/settings.go` `mergeHarnessHooks`).

Two entries carry a matcher (D21, D22): `SessionStart` with
`startup|resume|clear`, and `PreToolUse` with `AskUserQuestion`. The others
write none. Claude treats an omitted matcher on a tool event as "every tool",
and the dummy-harness E2E (§ Rollout) is what confirms this for `PostToolUse`
and `PermissionRequest`.

Dedup stays keyed on (event, command). `hookCommandPresent` already scopes its
search to one event's groups, so `harness-cli lifecycle working` can appear
under both `UserPromptSubmit` and `PostToolUse`.

### CLI

`harness-cli lifecycle <working|waiting|idle|error>`:
- With `--hook claude` it reads the hook's stdin JSON and folds the event into
  the task's state under the lock (D26, D27). It stamps `reported_at` inside
  that lock, skips a repeat of the last accepted report (D11′), then sends.
- Without `--hook`, stdin is not read and the argv word is the state.
- It is silent on success and on `stale`, because a hook's stdout can end up in
  claude's context.
- It exits 0 even on failure, so a down server never surfaces as a hook error
  inside the agent. It does write a line to stderr.

### Surfaces

Walk `.claude/skills/surface-parity-checklist` item by item. Minimum:

| Surface | Display |
| --- | --- |
| CLI `ls` text (`taskLine`) | `lifecycle=waiting (3m ago)` while non-terminal, `lifecycle=-` when never reported (D28); wire names in this machine-adjacent surface |
| CLI `ls --json` / `session ls` | `lifecycle` (wire name string, `""` when unset), `lifecycle_at`, `lifecycle_age_ms`, always present |
| TUI task table, `Life` column (`tui/tasks.go`, flat and tree) | D19/D28 words with the age, e.g. `work:5s`, `WAIT:3m`, `ready:12m`, `err:1m`, `lc:-`; width 10. `wait` is upper-cased, not styled, because bubbles may cut an ANSI sequence when it truncates a cell. Shown with `Obs` above the same panel width; below it the `Act` cell carries the badge instead of `cli.ActivityStr` (D28 (4)) |
| TUI detail popup | `lifecycle: waiting (3m ago)` or `lifecycle: - (never reported)` beside progress age and output-idle age |
| WebUI task list row, tree node | a badge with the D19/D28 word and age after the status label. `wait` and `err` use `taskStatusColor("Failed")` (`#f14c4c`, `webui/static/main.js` `taskStatusColor`). `ready` uses the default `#61afef`; `work` and `lc:-` are gray. The tree node shows the badge in its status line. The task sheet does not repeat it (D28) |

The point of D1 is that `wait` and `ready` stand out at a glance in a list of
working rows. Color does that; `work` and `lc:-` are shown in gray so that a
task whose hooks never fire can be told from a quiet one (D28).

### Agent skill

None. The agent never calls `lifecycle` itself. If the harness-cli skill lists
every verb, mark this one "written by hooks; do not call".

## Phase 2 — program status (OSC 7501)

### Observation (decided: D3, D17)

- `vtgrid.Terminal` parses `ESC ] 7501 ; <body> ST|BEL`. The grammar is in
  stablyai/orca `src/shared/program-status/protocol.ts`, MIT:
  - `:`-separated `key=value` pairs;
  - `msg` and `title` are base64;
  - limits are 4096 B per sequence and 2048 B decoded for `msg`;
  - a decoded control character voids the report.
- `TaskInfo` / `TaskStatusEvent` gain `program_state`, `program_kind` and
  `program_msg`, live-only and gated on `live_terminal` like the title. A change
  rides the next event, like the title.
- The replay already strips OSC `?`-queries (9580515), so a recorded probe is
  never re-answered on attach.

Surfaces. The point is to show a disagreement with lifecycle (D3), so the
program badge sits right beside the lifecycle badge (D28):

| Surface | Display |
| --- | --- |
| CLI `ls` text | `program=blocked:permission` when `live_terminal` and a state was ever reported; `msg` is not printed |
| CLI `ls --json` / `session ls` | `program_state`, `program_kind` (wire names, `""` when absent) and `program_msg`, always present |
| TUI task table | nothing: the table cannot afford another column at the widths it supports. The `d` detail popup carries it |
| TUI detail popup | `program: blocked (permission) — <msg>` on its own line, under the lifecycle line |
| WebUI task list row | `prog blocked:permission` right after the lifecycle badge, red for `blocked` / `error`, gray otherwise; the full line with `msg` as its tooltip. ~~WebUI task sheet: the same line under the lifecycle badge~~ — replaced by D28. Not on the tree node: its status line is 132 px wide |

### The probe (D18)

Without an answer, claude emits nothing (P3).

**The answer.** When the server's output scanner sees `ESC ] 7501 ; ? ST|BEL`
in a session's live PTY output, `SessionMux` writes the reply into that PTY's
input:

```
ESC ] 7501 ; ?:states=idle,working,done,blocked,error:kinds=permission,question,auth ESC \
```

This is the first answer the harness synthesizes itself. Every other terminal
query still round-trips to the client.

The reply above is copied from Orca's implementation. The protocol is new: it
was published 2026-10-06 (mitchellh.com/writing/program-status-osc7501), and
third-party write-ups cite revisions 0.1 and 0.4. Check the reply and the
report grammar against the current revision of the spec when implementing.

**Where it runs:**
- In the live output path only, never during replay. The replay already strips
  `?`-queries (9580515).
- Once per occurrence of the query.
- Regardless of attached clients.

**The client copy.** An attached client still receives the query bytes and
ignores them, since neither Windows Terminal nor xterm.js knows 7501. If a
client terminal ever learns 7501 it would answer too. Claude has settled its
capability by then, so the second reply is input noise at the prompt. Accepted
until such a terminal exists.

**Effects**, verified in the 2.1.311 binary:
- Claude's title loses its `◐`/`◑`/`✳` prefix. The title text is unchanged, and
  the operator's terminal tab is the same title.
- `cli/detect_rules.json` `title_spinner_working` goes blind. That rule is not
  load-bearing.

`CLAUDE_CODE_DISABLE_TERMINAL_TITLE` settles claude's capability to "no" and
also stops title writes, so it is not a per-task opt-out.

## Rollout and verification

- No skew guard (progress D16). Server first, then clients and the runner
  host's `harness-cli` (`make build` + restart). Run
  `scripts/wire-skew-check.sh`.
- Old runners leave existing worktrees' settings without the new hooks. The
  next `WriteAgentSettings` (new task or resume) adds them.
- Tests at the operator's entry layer against `scripts/dummy-harness.sh`.
  First, drive a real claude in a task: prompt → `working`; a Write needing
  permission → `waiting`; approve → `working`; turn end → `idle`. A write is
  needed for `waiting`: read-only tools rarely prompt (herdr addendum I).
  Repeat the sequence once for an event-stream task (D24), and include one
  `AskUserQuestion` (D22).
- A unit test that a report with an older `reported_at` is answered `stale`
  and leaves the field unchanged.
- A test that a hook run with the server down exits 0 and prints nothing to
  stdout.
