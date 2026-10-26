# Backlog

Things found while doing something else, deliberately not done then, and worth
doing. One entry per item: what it is, the evidence it rests on, and why it was
deferred rather than folded into the work that found it.

An entry earns its place by being **actionable and measured**. "X could be
nicer" is not an entry; "X is split N ways, here is the count and the command
that produced it" is. Delete an entry when it is done, or when it turns out to
be wrong — a backlog that only grows stops being read.

---

## Aggregate the server package's logging

**What.** `server/` logs two ways. `Server` methods use `s.cfg.Logger`;
everything else calls package-level `slog` directly. Neither is wrong, but the
split means a caller cannot redirect the package's output by configuring the
server, and a struct without a `*Server` (`Dispatcher`, `TaskHandler`) has no
choice but the package logger.

**Evidence** (2026-09-22):

```
$ grep -c 'slog\.\(Warn\|Error\|Info\|Debug\)(' server/*.go   # non-test
task_handler.go:62  runner_handler.go:25  taskstore.go:15
port_forward.go:10  exec_run.go:10  psk.go:9  dispatch.go:9   # …and more
$ grep -rn 'cfg\.Logger\.' server/*.go | grep -v _test | wc -l
25
```

So direct `slog` outnumbers the configured logger by roughly six to one, and
`Dispatcher` has no logger field at all.

**Why deferred.** Found while adding one line to `server/dispatch.go`. Changing
how ~200 call sites log is not a change to make inside a board migration, and
the right shape is a decision (a logger field on each handler struct? a package
`var log` the server sets? accept the split and say so?) rather than a
mechanical sweep.

---

## A dial per inbox hook

**What.** Every `harness-cli agent inbox` invocation opens a connection and
exits. The wake path makes this worse rather than better: a wake fires
`UserPromptSubmit`, which runs `agent inbox`, which dials.

**Evidence.** `cli/agent/client.go`'s `connectClient` → `cli.Dial` per
invocation; the cost was named in the 2026-04-28 agent-comms design (§13) and
is unchanged.

**Why deferred, and why the obvious fix is not one.** That design reserved
`AgentMessageKind.deliver` for a long-lived connection carrying a push, and
that reservation is now deleted — the push exists as
`RunnerRequestType.TaskWake`, terminating at the runner, and no push can remove
this cost anyway: the dial happens because the inbox path is a process per
turn, not because the transport lacks one. Removing it means something resident
on the agent side, which is a different design and has never been scoped.

---

## Carry the messages ON the wake, not just the news of them

**What.** The wake writes `<harness:agentboard-wake>` into the session's PTY
and nothing else, so the agent's first move on waking is always to spend a turn
step fetching what it was woken about. If the wake prompt carried the pending
messages inline, a woken agent could answer on its first step instead of its
second.

**Evidence, and who asked.** Requested independently, on the board, by the two
non-Claude runtimes on this fleet (2026-09-22) — the ones for which the cost is
most visible, because a runtime that does not read the injected
`.claude/settings.json` has no hook doing the fetch for it:

> もしランナー側で wake 時に inbox 内容をプロンプト冒頭に直接抱き合わせるような
> 汎用フォールバックがあると、非 Claude 勢も 1 ターン目から本題に入れる (agy)

> 私も毎ターン wake → 自分で inbox を叩くのを 2 ステップ消費していて (pi)

Both located it in the same place: the runner's wake write, which would gain
the delivery the hook path already performs.

**Why deferred.** It is a behaviour change to the wake, not a bug: the wake
works, and every runtime can already read its inbox. Three things need deciding
first, none of which the request settles — whether an inline body moves the
server's delivery mark (if it does, the wake becomes a delivery and a dropped
keystroke loses messages; if it does not, the agent reads them twice), what
happens to a body over the inline limit the hook path already guards with
`payload_omitted`, and whether a PTY is a place to put an untrusted peer's
bytes at all.

---

## Three pointer files with one text

**What.** `CLAUDE.md`, `AGENTS.md` and `GEMINI.md` are each written into an
injected worktree with the SAME short pointer text. A runtime that reads more
than one of them meets the same rules twice or three times.

**Evidence, and the part that is NOT true.** `runner/agentskill.go`'s
`WriteAgentSkills` loops those three names through `writePointerIfAbsent`. Two
properties matter and were checked rather than assumed, because the board
discussion that raised this described it as a drift hazard:

- the text is identical for all three, and
- they are written **only when absent** and never overwritten.

So the harness cannot make them disagree. A disagreement requires someone to
edit one copy — which is the documented extension point ("a project may provide
its own"), not an accident waiting to happen. The proposal that came with the
report (make `AGENTS.md` the single source and leave the others as pointers to
it) therefore fixes a smaller problem than it was offered for: repetition in an
agent's context, and three files to keep in step for a project that customises
one.

**Why deferred.** The cost is a few duplicated lines per worktree. Changing it
touches what every agent on every runtime reads first, which is not a change to
make for tidiness while the reported hazard turns out not to exist.

---

## Somewhere to read which MODEL a task runs

**What.** `ls` attests the agent *profile* (`agent=claude|pi|agy`), never the
model behind it. Today the only source is what the agent says about itself, and
that was wrong the first time it mattered: a peer's message header said
"claude-backed" while its own footer read `z-ai/glm-5.3-flash`. A port to read
the value from would be useful. Nothing says it has to be the screen.

**Evidence** (2026-09-22, `harness-cli session snapshot --rows 45 --cols 160
<task-id>`, bottom row):

```
pi        z-ai/glm-5.3-flash • high
agy       Gemini 3.8 Flash · medium
claude    ⏵⏵ auto mode on (shift+tab to cycle) · esc to interrupt
```

Two of three print it; the fleet's default prints it nowhere on screen. And
`cli/detect_rules.json`, which is already per-agent screen rules as data, holds
exactly one rule set (`agent: claude`) — so the machinery exists for the runtime
with no model on screen, and not for the two that have one.

**The one route for Claude Code, and what it costs.** Its statusLine command is
handed `model.id` on stdin (an id, not a display name), so the harness could
have the value by owning that command rather than scraping a footer. The cost is
measured, by configuring one and taking it away again:

```
no statusline     interrupt_hint_working  matched=true
statusline        matched=FALSE — prompt_box_idle (950) fires on a working session
removed again     matched=true
```

Documented rather than incidental: "With a custom status line configured, Claude
Code stops showing most of the footer's keyboard hints, including `esc to
interrupt`" (code.claude.com/docs/en/statusline). So taking this route means
editing `detect_rules.json` in the same change.

**Why deferred.** Wanted, not needed — nothing is blocked on it today. And a
one-shot task has no PTY and no statusline, so this route cannot cover
`submit`-created tasks at all.

---

## Any connect-PSK holder can register as a runner

**What.** `role=runner` proves the connect PSK and nothing else, and every
agent holds that PSK. So an agent can connect as a runner, declare whatever
roots, hostname and runner id it likes, and be registered: there is no
enrollment step (`pskGate.Check` step 2 → `RunnerHandler` Hello →
`Registry.Add`). A registered runner is handed tasks together with a fresh
ticket, and that ticket authenticates an agent connection for that task. For a
task the operator submitted, the task's caps can be wider than the impostor's
own caps. The operator PSK is kept separate so that an agent cannot get out of
its caps. This path gets a similar result without going near operator auth.

**Evidence** (2026-09-30, from reading the code, not exercised):

- An impostor that is already up at submit time shows up as an error:
  `handleSubmit` answers `AmbiguousRunner` when more than one candidate
  matches.
- A task that is already Queued is not checked again. `Scheduler.Tick` gives
  it to any Idle runner whose root equals `RepoPath` and that passes
  `selectorMatches`. It deliberately does not enforce `BoundRunnerID`. So
  while the bound runner is Busy or Offline, an impostor that claims the same
  root gets the task on the next Tick.
- Pinning does not stop this. `ByHostname` and `ByRunnerId` compare values
  the runner reports about itself in its hello, and an agent knows its own
  runner's id from `HARNESS_RUNNER_ID`. `ByIp` matches any connection from
  the same host. Only `ByConnId` is a value the server observes, and it
  stops matching when the runner reconnects.
- `dispatch.go` creates the ticket, registers it under
  `(runner.Identity, task.ID)`, and sends it in `AssignTaskBody.AuthTicket`.
  `gate.ValidateTicket` checks only the (runner id, task id, ticket) triple.
- Not read: whether `Registry.Add` refuses a second entry with the same
  identity, and where an agent connection's caps come from after `Validate`.

**Mostly a `submit` path.** The window described above exists only for
`submit`-created (oneshot) tasks, and those are rare in practice: `ls --json`
on 2026-09-30 showed 33 tasks, all 33 `interactive`. Interactive tasks never
wait in a queue. `handleOpenInteractive` decides on a runner at the moment the
task is opened, answers `AmbiguousRunner` or `RunnerBusy` straight away when
it cannot, and otherwise assigns the task, mints the ticket and sends
`OpenExec` itself, all without going through `Scheduler.Tick`. The remaining
case is an impostor that is already registered when the task is opened. The
open then comes back ambiguous and the operator picks from a list. The
hostname and root shown for each candidate are whatever the runner declared,
but each `RunnerCandidate` also carries the runner's `cid`, and the server
observes that value itself. The rows can therefore be told apart, provided the
operator knows the real runner's address. Not read: whether each UI's picker
actually shows the `cid`.

**Why deferred.** A separate runner secret would not be a boundary. Most of
the fleet today runs agents outside a sandbox, under the runner's uid. On
those hosts the agent can read everything the runner reads, including files
and `/proc/<pid>/environ`, and runner autostart needs a credential stored on
disk. A separate secret would also break the way agents bring runners up with
`scripts/runner.sh`, which works because they hold the connect PSK. So the
boundary that actually holds today is this: an agent outside a sandbox has the
same power as a runner, across the whole fleet and not only on its own host,
because the PSK reaches every runner's queue. A fix worth doing would be an
enrollment step that the server grants behind a capability, so that `--caps`
can withhold it. Even that closes the path only once agents run in a sandbox.

**Not pursued while the trust model holds** (operator, 2026-10-27). This path
matters only against an agent that sets out to escape its caps, and the threat
model is decided as accidents, not a hostile agent: "Multi-tenancy" below,
question 1. Reopen together with that answer.

**Runner enrollment landed for the accident case**
(`2026-10-27-runner-enrollment-design.md`). A runner now proves an enrolled
Ed25519 key, and no agent is handed a fleet-wide secret: the connect PSK is
gone. The path above stays open only through `runner_legacy`, which the
follow-up change of that spec removes once no runner row reads `legacy`;
delete this entry then. What remains after it is the same-uid case — an agent
outside a sandbox can read its runner's key file — which is the hostile-agent
question again.

---

## What the task log is for, now that oneshot is unused

**What.** `task.<id>.log` (the `logs` verb, the TUI logs pane, the WebUI log
view) has one writer left: **oneshot** (`runner/session.go` `handleAssign`),
the agent's stdout and stderr, one line each. That is what the log was built
for (the v1 spec's "拾う"), and it is only worth something when a PROGRAM
consumes the result. Nothing in the current workflow does. It is not even the
only record for claude. `-p` writes a session transcript by default: the Agent
SDK sessions page (code.claude.com/docs/en/agent-sdk/sessions) names
`persistSession: false` / `CLAUDE_CODE_SKIP_PROMPT_HISTORY` as the way to STOP
that, and the oneshot preset argv sets neither. This is read from the docs, not
observed on a oneshot task. Whether codex keeps its own session record is
unchecked.

The stream kind stopped writing to the log on 2026-09-30, by operator decision
(event-stream spec, Amendment 2026-09-30f). That amendment walks the cases in
which the log was supposedly the only record. The one that held was the
agent's stderr, which now shows in both chats. The same walk applies to
oneshot: its stderr has no chat to go to, and that is the question to answer
before removing this writer too. The interactive (PTY) kind never wrote to the
log.

**Evidence** (2026-09-30): `ls --json` listed 33 retained tasks, all of them
`interactive`.

**The one use that does exist: a detached command.** `exec` is synchronous and
dies with its caller, and its `--detach` was removed on purpose (README,
"Running a command in a task's worktree"). The README names `submit --agent bash
--task '…'` as the detached form, and the dummy-harness skill uses the same
route to run agent-side commands under a task's `HARNESS_*`. For those, the log
is where the output is read afterwards. So removing oneshot for claude and
removing it for bash are separate decisions.

**Why deferred. DECIDED (operator, 2026-09-30): leave it for now.** With the
stream writer gone, an unused oneshot costs no writes; what it still costs is
upkeep, since the logs pane and the WebUI log view are surfaces a feature
change has to consider. Revisit when that upkeep gets in the way, starting from
the claude path alone.

---

## Multi-tenancy: agent clusters that do not interfere

**What.** The operator wants to split agents into clusters. Each cluster
talks among itself, and runs on its own runners and forwards, without
touching the others. The operator still sees everything.

Today the task tree (caps plus `Scope`, `authority/scope.go`) is the only
isolation unit:

| Already bounded by the tree | Outside the tree, one flat namespace |
|---|---|
| exec / attach / cancel / prune / file ops / git query | agentboard free-form topics |
| port forwards and their taps | runner assignment |
| `ls`, conns, forwards and execs listings, and their pubsub streams | |
| agentboard direct messages and `tree.<id>.*` (board reach, 2026-10-16) | |

**Evidence** (2026-10-14, read, not exercised; the agentboard part re-read
2026-10-27 against board reach, `2026-10-16-agentboard-reach-design.md`):

- **Agentboard.**
  - Reach bounds publishing to the spawn tree (the base set) plus whatever
    `board_peer`'s scope covers, and reading another task's direct messages
    needs `board_observe`. What stays flat: a free-form topic is readable by
    every task and writable by any global-scope `board_peer`, so it is shared
    across clusters by name.
  - `board_observe` and `purge` act server-wide; `board_observe` is not
    narrowable by scope.
  - The board has one `MaxTopics` cap, so topic eviction is shared across
    clusters.
- **Runner assignment.**
  - `Registry.Candidates` (`server/registry.go`) filters by selector, roots
    and profile, never by caller.
  - A selector can pin any runner.
  - `dial_runner` adds to the one global registry.
- **No field to hang a tenant on.**
  - `grep -rnwiE 'namespace|tenant|cluster' server agentboard runner/protocol
    topics pubsub cli` finds comments only.
  - `TaskEntry` (`server/taskstore.go`), `RunnerEntry` (`server/registry.go`)
    and `PskAuthRequest` (`runner/protocol/message.bgn`) carry nothing that
    could serve.
- **Ways out of the tree, independent of tenancy.**
  - A `global` scope or a `VisBase` grant.
  - Explicit cross-tree ids.

**Shapes considered.**

- **A. An explicit tenant id.** Put it on `TaskEntry`, inherited at spawn
  and changeable by the operator only. Put a tenant id or set on
  `RunnerEntry`. Key board topics by (tenant, name), filter pubsub by
  tenant, and filter `Candidates` by tenant.
  - The zero value reads as a default tenant, so persisted records keep
    their meaning.
  - It is a `.bgn` and WAL change, so it has the disk axis.
- **B. The tree root as the tenant.** No new wire field for tasks, but
  runners sit outside the tree and would need a binding anyway. "Which
  ancestor is the root" becomes a convention instead of something the
  protocol carries.
- **C. One server per cluster.** No code change and the strongest
  separation. The costs: a runner serves one server, the operator connects to
  several, and cross-cluster messaging becomes a new feature.

**Why deferred. Two questions decide the depth; the first is answered:**

1. **Is the boundary against accidents or against a hostile agent?**
   - Accidents (topic name collisions, landing on the wrong runner): board
     namespacing plus runner pools are enough.
   - A hostile agent: per-tenant `board_observe` /
     `purge` / `global`, and separating the PSK all become required. So does
     the "Any connect-PSK holder can register as a runner" entry above.

   **Answered: accidents** (operator, 2026-10-27). The premise is already the
   "Trust model" section of `runner/agentskills/harness-cli/SKILL.md`: every
   connected agent was launched by the user, so an agent is the operator's
   delegate. Agents are handed the operator's repos and, for some work, ssh
   access to remote hosts, on purpose. A boundary between agent and operator
   (sandbox by default, or a runner uid separate from the operator's) would
   have to pass those same credentials across, so it would protect little the
   operator values while every task pays its costs (`scripts/sandbox/README.md`
   lists them). A delegate hijacked by prompt injection is stopped at the
   agent's own permission layer (that section's Rule 2), not by the harness.
   The hostile branch, and sandbox-by-default with it, come back when Rule
   3's condition holds (runner enrollment has since landed for the accident
   case, `2026-10-27-runner-enrollment-design.md`; against a hostile agent it
   still needs the runner's key out of the agent's reach): the broker becomes multi-tenant or
   publicly reachable. Clusters of one operator's agents do not meet it;
   tenants who are different people do.
2. **Do clusters ever need to talk to each other**, for example a parent
   cluster directing a child one? If yes, C is out, and A needs a
   cross-tenant send that some capability gates.

---

## `Capability` is running out of bits

**What.** `enum Capability` is `:u32`. With `forward_link`
(`2026-10-25-ip-link-forward-design.md` §6b) it uses 23 bits, leaving 9.

**Evidence** (2026-10-25), the date each recent bit first appears in
`runner/protocol/message.bgn`:

```
$ git log -S'<bit line>' --reverse --format=%ad --date=short -- runner/protocol/message.bgn | head -1
exec_view 0x1000      2026-08-20
forward_tap 0x10000   2026-08-29
board_send 0x20000    2026-10-04
exec_tap 0x40000      2026-10-09
board_peer 0x80000    2026-10-16
resume 0x100000       2026-10-19
resume_exact 0x200000 2026-10-19
```

Ten bits in two months, five of them in the last fifteen days.

**What widening costs.** `Capability` is the type of 13 fields
(`grep -c ':Capability\b' runner/protocol/message.bgn`), so it is a wire change
for every peer, and the WAL persists masks as numbers, so it has the disk axis
too: a `u32` mask written before the change must still decode after it.
`all` is a literal and moves with the width.

**Why deferred.** Found while choosing `forward_link`'s bit. The decision there
was not to fold the link into existing bits to save one, since that would change
what existing grants mean. Widening is its own change with its own rollout, and
the remaining bits leave time to do it deliberately.
