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
   - A hostile agent: per-tenant `board_observe` / `purge` / `global` all
     become required, and so does keeping each runner's key out of its
     agents' reach: an agent outside a sandbox runs as its runner's uid and
     can read the key file, and with it register a runner of its own.

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

## Task labels

**What.** Operator-chosen labels on a task, for finding tasks in a long list.
Step 3 of `2026-11-16-task-list-order-design.md`. Steps 1 (ordering modes) and
2 (the u64 widening of `Capability`, `2026-11-16-capability-u64-design.md`)
have landed; no labels spec exists.

**Already decided** (operator, 2026-11-16):

- Labels are held by the server, not per client.
- A task can carry several.
- Setting and removing them is gated by a new `label` cap bit. It would be the
  31st bit; the u64 widening made room, and the budget is now 53 bits
  (`TestCapabilityStaysExactInTheWebUI`).
- A child inherits its parent's labels at spawn.
- Labels are **not** an authority target: no scope, grant or reach decision
  reads them.

**Proposed scope, not yet agreed:**

- Set and remove labels from the CLI, the TUI and the WebUI.
- Show labels in task rows.
- A `label` mode in `cli.OrderTasks` (`cli/task_order.go`). A task with several
  labels appears under each one.
- `ls --label X`, with the same filter in the TUI and the WebUI.
- No bulk cancel or prune by label.

**Open.** What labels will actually name (a topic of work, a waiting state, …),
and therefore who mostly sets them, the operator or agents. That answer decides
how much inheritance at spawn is worth and which surface has to be cheapest.

**Why deferred.** After trying the ordering modes in the TUI, the operator found
them enough for now (2026-11-16). Revisit when `live` / `state` / `repo`
ordering stops finding the tasks that matter.

---

## "Operator" is the absence of a principal, not a recorded identity

**What.** The server decides whether a connection is the operator by finding
no principal task for it. `lookupPrincipal` (`server/task_handler.go:289-293`)
is a map read, `h.principals[connID]`. A connection that has no entry in the map
gets the zero `TaskID`. `callerCaps` (`server/capabilities.go:189`) answers
`Capability_All` for that zero. So "authenticated as the operator" and "missing
from the map" are the same value, and a miss is a grant. The task record uses
the same encoding: an all-zero `creator_task_id` means "created by the operator"
(`TaskInfo`, `runner/protocol/message.bgn`).

**Evidence** (2026-11-09):

```
$ grep -rn 'lookupPrincipal(.*)\.Id [!=]= (\[16\]byte{})\|pid.Id == (\[16\]byte{})' server --include=*.go | grep -v _test | wc -l
12
```

All 12 sites were read.

- Two of them reserve a verb to the operator: `caps set` and `caps set-parent`.
- The other sites do one of two things:
  - apply "the operator is the root and holds everything", or
  - refuse something a principal-less connection has no task for (progress,
    lifecycle).

Authentication itself is explicit. The handshake keys an agent's binder to its
ticket and an operator client's binder to the operator PSK
(`server/psk.go:186, 263`). Only the authorization lookup afterwards reads
absence as operator. A principal whose task record is gone fails CLOSED: it
gets `Capability_None`.

**Not established.** Whether any path lets an agent connection reach
`TaskHandler` without a `principals` entry: before the hello is recorded, on
reconnect, or through the runner proxy. This entry records the shape: a missed
registration fails open. It does not record a known exploit.

**The likely shape of the fix.** Record the principal at authentication as an
explicit value, "operator" or "task X". Refuse a connection that has no record.

**Why deferred.** Found while designing spawn limits
(`2026-11-09-spawn-limits-design.md`). That design adds no branch on operator
identity, so it does not depend on this change. Changing how every handler
resolves its caller is its own change and needs its own verification across
connection paths.

---

## The MCP body-hygiene guard stops at `cmd/harness-cli`

**What.** One `harness-cli mcp` process serves many tool calls, with stdin and
stdout carrying the JSON-RPC stream, so no tool body may end the process
(`os.Exit`, `die`), read fd 0 or write fd 1
(`2026-11-22-harness-cli-mcp-surface-design.md` D16).
`TestToolBodiesDoNotTouchProcessStreams`
(`cmd/harness-cli/body_hygiene_test.go`) enforces that, but only over the
functions of `cmd/harness-cli`. A helper in `cli/`, `cli/agent` or
`runner/agentskills` that a body calls is not scanned: if one of them writes
`os.Stdout` or exits, the server's stream is corrupted or the server ends, and
no test goes red.

**Evidence** (2026-11-23, at `c4fdf963`):

```
$ grep -rnE 'os\.(Exit|Stdin|Stdout)\b|fmt\.Print(f|ln)?\(' cli runner/agentskills --include=*.go \
    | grep -v _test.go | grep -v cli/verb/gen/
cli/cmd_board.go:762     (a comment)
cli/forward_stdio.go:87  spliceStdio(ctx, rc, os.Stdin, os.Stdout)   -- forward, not a tool
```

So nothing outside the package does it today. The guard has already missed one
case in its own package: it found `os.Exit` by name, `die` (which calls it) was
allowlisted as the process exit, and `spawnOpts` called `die` from the
`session_new` tool body. A bad `--runner` ended the server
(found by review, fixed in `c4fdf963`, and the guard now refuses calls to
`die` as well).

**Shape.** Scan the packages a tool body can reach the same way, with the
not-a-tool callers (`forward`'s `spliceStdio`) allowlisted by name and reason.
Following calls across packages by hand is the cheap form; the exact form is
the call graph from the `cliVerbs` methods (`golang.org/x/tools/go/callgraph`),
which is a new dependency.

**Why deferred.** Found while landing the MCP third pass; nothing reachable
from a tool does it today, and choosing between a package list and a call
graph is a decision of its own (operator, 2026-11-23).

---

## `OpenInteractiveRequest` says the task kind with a bool

**What.** `session new` carries the event-stream choice as the bit
`event_stream` on `OpenInteractiveRequest`; `interactiveKind`
(`server/task_handler.go:2629`) turns it into a `TaskKind`. A schedule says
the same choice with `kind :TaskKind` (`2026-11-17-schedule-stream-kind-design.md`,
K2, operator's choice of the enum over bits). Two spawn wires now encode one
choice two ways. Move the open to `kind :TaskKind` too.

**Why deferred.** A wire change to the open path that nothing is blocked on;
found while designing the schedule fix, and kept out of it (operator,
2026-11-17).

---

## Let A message B, from two clicks on the WebUI tree

**What.** In the WebUI task tree, pick a task A, then a task B. A gets
`board_peer` scoped to B (`--scope-for board_peer=ids:<B>`), so A may publish
to `chat.<B>`. The tree also offers the reverse operation: removing B again.
The first case was a task in another repository that should report harness
bugs to the harness agent. The feature is not limited to that case.

Today this takes the task sheet's 🔑 re-grant dialog. The operator adds the
`board_peer` chip and writes a `--scope-for` row. Every existing override must
be kept, and `--keep-conns` must be on.

**Evidence** (2026-11-24, read, not exercised):

- Publishing to another task's `chat.<id>` outside the base set needs
  `board_peer`, and its target set must contain that task. A's creator and
  A's children need no bit (`2026-10-16-agentboard-reach-design.md` §2, D16).
- `SetCapsRequest` (`runner/protocol/message.bgn`) writes scope and overrides
  together under `scope_present`. So adding one target is a read-modify-write
  of the task's whole scope. Without `keep_conns`, a narrowing closes the
  task's connections.
- `cli.BuildTaskGrants` (`cli/tasktree.go:392`) already draws id-named grants
  as edges on the tree.
- A one-way grant is enough for a conversation that A starts. When B reads A's
  message, B gains `chat.<A>` as a contact (reach spec §3). A contact goes
  away in four cases:
  - B's task finishes: `Revoke` (`agentboard/board.go:266`) drops the whole
    `taskState`. A detach does not drop it.
  - It is pushed out by the 64-entry bound (`agentboard/taskstate.go:65`).
  - The operator runs `board clear-contacts`.
  - The server restarts. Contacts are not in the store: `storeRecord`
    (`agentboard/filestore.go:72`) has no contact record.

  A contact made by a direct message never comes back after a restart, because
  `chat.*` topics cannot be retained (`Retainable`,
  `agentboard/retention.go:24`). One kind of contact can come back. It needs a
  reply to the task's own message, sitting on a retained group topic. The
  replied-to message must also be restored into the ring. If the task then
  reads that reply again (the second branch of `addressedTo`,
  `agentboard/reach.go:306`), the contact is made again. That includes a
  contact cleared before the restart: the clear floor lives in memory only.

**DECIDED (operator, 2026-11-24): scope.** The feature adds and removes the
grant `board_peer ids:<B>`. Two things were proposed and declined: telling A
where B is (a message on `chat.<A>`), and choosing one-way or two-way in the
dialog.

**NOT DECIDED: the rest.**

- *How to merge into A's current authority* (proposed):
  - A holds no `board_peer`: add the bit, plus an override with base `none`
    and `ids:<B>`. The override stops A inheriting a `global` base scope.
  - A's base clause carries `board_peer`: copy that clause into an override
    and add B.
  - `board_peer` shares an override mask with other bits: split it out.
  - A already reaches B: send nothing, and say so.
  - Removal deletes B from the ids. Drop the bit together with an override
    that becomes empty: dropping only the override would fall back to the base
    scope, which is wider. Offer no removal when B is reached through
    `subtree` / `global` rather than `ids:`.
  - Send with `keep_conns`.
- *Where the merge lives* (proposed: 1):
  1. One Go function, reached through a `caps peer add/rm <A> <B>` verb (CLI,
     TUI and WebUI command lines) and a wasm bridge for the tree gesture.
     Sends the existing `SetCapsRequest`. It can overwrite a `caps set` made
     inside the WebUI snapshot's ~5 s staleness.
  2. WebUI JavaScript only. This duplicates the scope merge in JS and leaves
     the TUI and CLI without it.
  3. A server-side add/remove request. This removes the race, but it is a
     `.bgn` change with a server-first rollout.
- *Whether removal also clears the matching contacts on both sides.* Without
  that, a route A or B already holds survives the removal until one of the
  four cases above.

**Why deferred.** The operator judged it not needed yet (2026-11-24). Nothing
is blocked on it: the re-grant dialog does the same thing by hand.
