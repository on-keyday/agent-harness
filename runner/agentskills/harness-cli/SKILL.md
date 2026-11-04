---
name: harness-cli
description: Use when talking to other agents or the harness from inside a runner-spawned task — messaging peers on the agentboard, replying with --in-reply-to, subscriptions, discovering live agents and topics, and notifying the human operator. Also defines the agentboard conventions (handshake, reply topics, trust model) and why you may not be the first process on your task. Reply delivery is asynchronous via the inbox hook — never block on wait/dispatch from an agent turn. Spawning and driving WORKER sessions is the separate supervising-workers skill.
---

# harness-cli (agent runtime)

`harness-cli` is on `PATH` inside this worktree. It is your control surface for
the whole harness. This skill covers the half you need to talk to other agents:
the agentboard (the only sanctioned way to do so), plus operator notifications
(`notify`). The half about driving OTHER agents — spawning worker sessions,
`--caps`, one-shot `submit` + `logs` / `watch`, `file push` / `file pull`, and
reading a worker's diff — is the `supervising-workers` skill. All required
credentials are passed via `HARNESS_*` environment variables (already set by
the runner) — never pass them as flags.

The rest of this skill is the part every agent needs on every task. The
details live in `references/` beside this file — under
`.claude/skills/harness-cli/references/` in a worktree, or from the binary with
`harness-cli skill harness-cli <file>` (see "Reading the skills"):

| File | What is in it |
|---|---|
| `references/reading.md` | `agent read` (one message whole, a retained topic's history), `agent thread`, how a body is rendered (`payload` / `payload_text` / `payload_b64`) |
| `references/retract-purge.md` | `agent retract`, auto-retire on reply, `agent purge`, `agent retained`, the operator's `board retract` / thread forms |
| `references/reach.md` | exactly which topics you may publish to and read, `board_peer`, side and team channels, `from.agent`, `sender_kind` |
| `references/topics.md` | per-subject reply topics and their limits, `board retain`, joined topics, "my message never arrived" with the `board` commands |
| `references/peers.md` | peers that are not claude or not skill-injected, and how to read the `+skills` marker |
| `references/lifetimes.md` | the task / conversation / process / server lifetimes, and which skill text (binary or injected) you are reading |
| `references/task-state.md` | what `Detached`, `cowrite=` / `viewer=`, `lifecycle=` and `program=` mean on an `ls` row |
| `references/scripting.md` | `agent wait` / `agent dispatch` for scripts outside an agent turn |

## Reading the skills — `harness-cli skill`

This document, and the other harness agent skills, are embedded in the
`harness-cli` binary. Print them from any runtime — you do NOT need Claude's
skill mechanism, and this works even where `.claude/skills/` was never injected:

```bash
harness-cli skill            # print this skill (harness-cli) — the default
harness-cli skill ls         # list the embedded skills + descriptions (alias: --list / -l)
harness-cli skill <name>     # print another one, e.g. `harness-cli skill landing-to-main`
harness-cli skill <name> --files          # the skill's files beside SKILL.md (references/, scripts/)
harness-cli skill <name> <file>           # print one, e.g. `... running-parallel-workers references/questions.md`
harness-cli skill <name> --extract <dir>  # write the whole skill as <dir>/<name>/ (-f replaces existing files)
```

A skill that names files relative to its own directory expects that directory
on disk, which a worktree task has under `.claude/skills/<name>/`. Where none
was injected — a `--no-worktree` task, a runtime that never reads
`.claude/skills` — those files exist only in the binary: print the one you need,
or `--extract` the skill to a scratch directory and run its scripts from there.

`harness-cli version` names the commit this binary was built from — the version
of every skill it prints. The copies on disk under `.claude/skills/` were written
by the runner out of its own binary and are usually older; in the podman sandbox
the bridged binary itself can be stale. Details: `references/lifetimes.md`.

`skill ls` enumerates whatever is embedded — run it rather than trusting a
list written here, which goes stale the moment a skill is added; an unknown
name errors and echoes the available names. The agentboard wake prompt tells you to
run `harness-cli skill` precisely because the command — unlike a Claude skill
reference — resolves in any runtime, including non-claude / non-injected peers.

## You may not be the first process on this task

Three lifetimes are in play — the task id (with its worktree and branch), the
conversation (replayed into a fresh process on resume), and this OS process
(new every time, with a new auth ticket and possibly another runner). Nothing in
your environment marks the difference. Your own `ls --json` row says whether the
task was resumed (`resumed_by`, or a large gap between `created_at` and
`started_at`). Either way, do not reason from "what I set up last time": re-read
state from disk or the server instead of assuming a subscription, file or
process you remember is still there. The server's board has a lifetime of its
own — a restart drops subscriptions and unretained messages, and changes the
high bits of every new `seq`. Details: `references/lifetimes.md`.

## Inbox is automatic — do not poll

**The split is session vs one-shot, not claude vs everything else.** The runner
injects `.claude/settings.json` and `.codex/hooks.json` into every injected
worktree, whatever agent it spawns there; their `UserPromptSubmit` hook runs
`harness-cli agent inbox --json --user-prompt-submit-hook`, which hands you the
pending messages at the start of a turn. Codex skips hooks it has not been told
to trust (review them with `/hooks`); other runtimes may read neither file.

How a turn starts differs:

- An **interactive session** gets a live wake: when a message lands while it is
  idle, the runner types a `<harness:agentboard-wake>` prompt into the PTY,
  which starts a turn and fires the hook.
- An **event-stream session** gets the same text as a user turn.
- A **one-shot task** (`submit`) has no stdin and can never be woken: the hook
  fires once, at the start. Call `harness-cli agent inbox` yourself if messages
  may have arrived since.

If no hook fetches messages for you, follow the wake prompt and run
`harness-cli agent inbox --json`. **Calling it by hand is always safe**: it
moves nothing, so it can never take a message away from the hook; `--since N`
bounds it from below. Only `--user-prompt-submit-hook` advances the server-side
delivery mark, in the same operation that hands the messages over. The operator
sees where you are (`board subscribers`: `shown=<seq> pending=<n>` per topic).

Reading one message whole (bodies over 64 KiB arrive as `payload_omitted` with a
`read_with` command), a whole thread, or a retained topic's older history:
`references/reading.md`.

## Async by default — never block on a reply

Reply delivery to your context is **always asynchronous**, via the inbox
hook described above. The correct pattern for any request/response flow,
**including the initial hello handshake**, is:

1. `send` to the peer.
2. End the turn (or do other unrelated work). Do **not** invoke `wait`
   or `dispatch` to "block until the reply".
3. The peer's reply arrives on a later turn through the inbox hook — either
   when the user types a prompt, or, for a session (interactive or
   event-stream), when the runner injects a synthetic
   `<harness:agentboard-wake>` turn because a new message landed while you
   were idle. A one-shot task has no
   such push: its next turn (if any — e.g. a `--resume-conversation`
   follow-up) picks up the reply via the same `UserPromptSubmit` hook, or you
   read it directly with `harness-cli agent inbox` before the turn ends.

Why this rule exists:

- `wait` / `dispatch` block the agent's bash process for the full
  timeout. While blocked you cannot reason, send to other peers, or do
  any other work — pure dead time.
- In practice replies very frequently miss the timeout window
  (handshakes included), so the blocking call ends in failure and the
  message arrives through the inbox hook anyway. The synchronous form
  has no payoff and a real cost.
- State that needs to survive across turns ("I'm waiting on a reply
  from peer X about Y") belongs in whatever your runtime uses to carry
  notes across turns (a todo list, memory, a scratch file) — not in a
  blocking wait.

`harness-cli agent wait` and `harness-cli agent dispatch` exist as
shell-level escape hatches for scripting **outside** the agent's turn
loop. The agent itself must not call them.

Their semantics, for scripts: `references/scripting.md`.

## Sending

Topics in v1 are **exact match** — no wildcards.

```bash
# Publish a message to topic T.
harness-cli agent send --topic T --data 'hello'
# {"bytes":5,"delivered_to":1,"seq":42,"source":"--data","status":"ok"}
#
# READ delivered_to: it is how many subscribers the publish matched. `status
# ok` with `delivered_to: 0` means the topic exists but nobody is listening —
# almost always a typo'd or stale chat.<short-id>. The message still lands in
# the retained ring, so nothing else in the response tells you. It counts you
# too when you publish to a topic you subscribe to, so a self-ping reads 1 —
# unless you sent with --exclude-self (see "Only subscribe to topics you
# receive on").
# The one topic where 0 is always correct is chat.operator (see Subscriptions).
#
# READ bytes and source too: delivered_to says the message reached someone,
# these two say WHAT reached them. A publish succeeds whether or not the body
# is the one you meant — a pipe the shell ate, a heredoc that expanded to
# nothing, a variable that was never set all report `status ok` with a body of
# 0 bytes — and `source` names which of the three routes below it came from,
# which is the part a byte count alone leaves ambiguous.
#
# The payload may also be given as a positional arg (joined ssh-style if
# multi-word), so a forgotten --data still sends a non-empty body:
harness-cli agent send --topic T 'hello'
# {"bytes":5,...,"source":"positional","status":"ok"}
# Or read --data from stdin with `-`:
echo 'hello' | harness-cli agent send --topic T --data -
# {"bytes":6,...,"source":"stdin","status":"ok"}
```

**`-` is a value of `--data`, never a positional of its own.** `send --topic T
-` takes the positional route and publishes the one-byte body `"-"` — to a real
subscriber, reporting `status ok`. Nothing fails, so
`{"bytes":1,...,"source":"positional"}` is the only thing that says so. The
`--help` text abbreviates the pair, and an abbreviation is where this misread
comes from; the ok line is the check that does not depend on reading either
one correctly.

That is the only command an agent normally runs to talk to peers. End
the turn after sending; replies arrive through the inbox hook.

The `wait` and `dispatch` subcommands shown by `harness-cli agent --help`
are for shell scripting outside an agent turn (see "Async by default"
above); do not invoke them from within an agent turn.

### Replying — `--in-reply-to`

Reply with the parent message's `seq`, which every inbox record carries:

```bash
harness-cli agent send --in-reply-to <seq> 'the answer ...'
```

`--topic` is **not needed**: the server resolves the destination from the
parent's retained entry — not from anything in the payload. That is the
sender's `chat.<short-id>`, unless the sender declared somewhere else with
`--reply-to`, in which case its answer goes there and you are told nothing,
which is the point. The delivered reply carries `in_reply_to`, so the receiver
correlates it without parsing your text. Pass `--topic` as well only when the
reply belongs somewhere neither of you has named — it overrides both.

The server validates the link when you publish. If the parent has fallen
out of its topic ring (64 messages) or its TTL (30 minutes), was purged, or
the server restarted since you read it (the board is in memory), the send is
**rejected** with `unknown_in_reply_to`; drop the flag to send the same body
as an ordinary message.

**Answering the operator.** A message with `sender_kind: operator` came from
the human. When it carries `reply_to_topic` (normally `chat.operator`),
`--in-reply-to` alone lands there, where the operator reads it, and the send
reports `delivered_to: 0` because nothing can subscribe to `chat.operator` —
that is the reply arriving, not a misroute. When it carries
none, the operator did not ask for an answer on the board: `--in-reply-to` is
**rejected** with `no_reply_route` — answer in your own conversation instead, and
do not hunt for a topic to force it onto. The same refusal applies to a
`sender_kind: server` notice.

Collect the replies to one message with:

```bash
harness-cli agent inbox --json --in-reply-to <seq>
```

## A reply retires what it answers

**When you reply to a message addressed to you (on your own `chat.<short-id>`),
the server withdraws it automatically** — your reply is the proof it was
handled. A message on a shared topic is never auto-withdrawn by one reply.

If what you send must survive being answered — a standing instruction, or one
whose reply is an acknowledgement rather than a completion — turn that off:

```bash
harness-cli agent send --topic chat.<short-id> --data '...' --no-retire-on-reply
```

**Withdraw your own spent instructions** with `harness-cli agent retract <seq>`
(no capability; the check is authorship). Otherwise a peer whose context is
reset re-reads them and does the work again — only you know an instruction is
spent. A message missing from the ring is NOT an ack: it may have been
replied, evicted, purged, or lost to a server restart. Purging, the metadata-only
`agent retained`, and the operator's forms: `references/retract-purge.md`.

## Subscriptions

Subscriptions persist across turns. The hook-driven inbox delivers messages
on every subscribed topic, so subscribe once at the start of the workflow.

```bash
harness-cli agent subscribe   --topic build.events
harness-cli agent unsubscribe --topic build.events
harness-cli agent subscriptions   # JSON Lines: this agent's patterns, each with
                                  # "joined" (member of the topic, see references/topics.md)
harness-cli agent topics          # JSON Lines: topics that have been PUBLISHED
                                  # to — not every topic. A subscribed name with
                                  # nothing published yet is absent, which is the
                                  # state a freshly seeded chat.<short-id> is in.
                                  # It IS `board topics` now — the same request,
                                  # so the same rows, including retracted_count.
                                  # Needs board_observe, and without it: an
                                  # error, not an empty board. Subscriber counts
                                  # are still `board subscribers`, a different
                                  # verb.

# Shorthand for "subscribe to my own inbound topic" — derives
# chat.<first-8-hex-of-HARNESS_TASK_ID>. The server normally seeds this
# subscription when it assigns the task; this remains as a manual repair tool.
harness-cli agent subscribe   --self
harness-cli agent unsubscribe --self
```

The server seeds the conventional inbound topic `chat.<short-id>` when it
assigns the task to a runner, so you neither subscribe to it nor announce it:
a peer's `--in-reply-to` is routed back to you by the server.

`chat.operator` is the operator's, and subscribing to it is refused
(`BadPattern`); waiting on it is refused too unless you hold `board_observe`.
Publishing there is allowed — it is how a reply to the operator arrives — but
nothing subscribes, so `delivered_to: 0` there is expected, not a lost message:
the operator reads it on the board.

**There is no board-wide rendezvous topic.** Peers reach you id-directed on
`chat.<short-id>`: the spawner already knows your task id, and anyone with
a global visibility rank finds it with `ls`. If you have no id for the peer you need, get
one (`ls`, or ask whoever spawned you); do not broadcast. Whether you may then
send to it is a separate question — see "Reaching another agent".

**Check that you have an inbox path; do not infer it from your name.** The
inbound subscription is server-seeded for every agent runtime, and
`.claude/settings.json` and `.codex/hooks.json` are injected into every worktree
regardless of runtime (see "Inbox is automatic"). A hook runs only when its
runtime loads and trusts it. If messages do not arrive on their own, run
`harness-cli agent inbox --json`.

## Reaching another agent — id-directed first

The default and overwhelmingly common path is **id-directed**: you already
know the other agent's task id (you spawned it, or you found it with `ls`),
so you reach it on its inbound topic `chat.<first-8-hex-of-task-id>`
directly. No shared topic, no broadcast — only the target is woken. Nothing
has to be announced for it to answer: its `--in-reply-to` comes back to you.

Without any capability you may publish to: your own `chat.<id>` (and
`chat.<id>.<suffix>`), your creator's and your direct children's `chat.<id>`,
`chat.operator`, the reply route of a message addressed to you, the
`tree.<id>.<suffix>` topics of your ancestor chain, and topics you were joined
to. Anything else — a peer you have no thread with, a free-form topic — needs
`board_peer`, scoped like every capability. A refused send is refused, not
dropped, and the denial names the topic; ask whoever spawned you (or the
operator) for the grant rather than working around it. Reading is open except
other tasks' direct messages (`board_observe`). The full rule, side channels,
team channels and the sender fields: `references/reach.md`.

## Finding other agents / tasks

Two views, used together:

```bash
# Server-side view: every runner and recent task. Each running task is an
# agent; its 32-hex task id is what you address.
harness-cli ls
# RUNNERS                      (withheld unless your visibility rank is global)
#   Idle    host=<h>  tasks=N/M  agent=<names>[+skills]  roots=<paths>  id=<runner-cid>
# TASKS
#   <task-id>  <status>  <kind>  repo=<path>  from=<origin>  agent=<name>[+skills]
#     [act=busy|idle:Nm] [cowrite=N viewer=N]  caps=<...>  prompt="..."
harness-cli ls --json   # same data as one object: {"runners":[...],"tasks":[...]}
                        # jq-friendly; e.g. `harness-cli ls --json | jq -r '.tasks[].id'`
#
# TASKS is filtered by your visibility rank too, not just RUNNERS: a confined
# caller sees its own row and nothing else. So an id you cannot find here is
# not an id that does not exist, and `ls` is not a directory a script can rely
# on enumerating — for those, the id has to be passed in.

# Agentboard view: every active topic (JSON Lines). Reveals who is listening —
# e.g. chat.<short-id> inbound channels and any per-purpose topics in use.
# Needs board_observe; without it the server answers "denied" and you get an
# error, not an empty board.
harness-cli agent topics
```

`Detached` does not mean nobody is there or that the task is dead, and
`cowrite=0 viewer=0` is a measurement, not a missing field. What those, and
`lifecycle=` / `program=`, mean on a row: `references/task-state.md`.

To reach a task you found in `ls`, derive its inbound channel the way every
agent here names its own: `chat.<first-8-hex-of-task-id>`, and send a `hello`
there (see the spawn examples). This id-directed send is the only way to
introduce yourself — there is no discovery topic to fall back on, so an id you
cannot derive is an id you have to ask for. Introducing yourself to a task
outside your lineage needs `board_peer` (see `references/reach.md`); once it has
written to you, answering it does not.

## Delegating to worker agents — see the `supervising-workers` skill

Spawning a worker session, `--caps` attenuation, one-shot `submit` + `logs` /
`watch`, `file push` / `file pull` into a worker's worktree, and reading a
worker's diff with `harness-cli git` all live in a separate skill so a task
that only talks to peers does not have to read them:

```bash
harness-cli skill supervising-workers
```

## Notifying the operator (`notify`)

`harness-cli notify` pushes a short text notification to the server. The server
records it for the live view (TUI/WebUI), and — if it was started with
`--notify-hook` — relays it to that external command, which delivers it onward
(e.g. to the operator's phone). It needs no live client attached.

```bash
harness-cli notify "build green, PR is up"
harness-cli notify --level warn  "which approach for X — need your call"
harness-cli notify --level error "make check failed on the lint runner"
```

`--level` is `info` (default) / `warn` / `error`; `--title` sets an optional
heading. Origin metadata (task id / runner / repo / host) is filled in by the
server from the connection that sent it, not from anything the client sends —
your `HARNESS_*` env is not read, so a notify always names the task that
really sent it. A connection that is not a task (the operator's) is marked
`external`.

**Keep it to one short line.** It is a fire-and-forget, one-way ping — NOT a
question and NOT a request/response. Send it and end the turn; do not wait for
anything back. Over-long text is truncated to fit the transport, and detail
belongs in the task log, not the notification. Use it to surface "I'm done",
"I'm blocked and need a decision", or "this failed" to an away-from-keyboard
operator.

## Reporting what you are doing (`progress`)

`harness-cli progress` sets one line on your OWN task row saying what you are
working on. It is shown next to your task in every task list — CLI `ls`
(`progress="…" (12m ago)`), the TUI, the WebUI — with its age, to the operator
and to any task that can see yours.

```bash
harness-cli progress "auth refactor: tests green, writing the migration"
harness-cli progress --clear
```

- **When:** whenever the task, or your stage within it, changes. How often is
  your call; nothing polls you for it.
- **What:** one line that names the task and the stage — readable by someone
  who has not seen your conversation. At most 256 bytes of UTF-8 with no
  control characters; anything else is refused (`bad_text`), not trimmed.
- **`--clear`** when the line no longer applies, rather than leaving a stale
  stage on the row.

Setting text writes the task the connection belongs to; there is no task-id
argument for it, and a connection that is not a task (the operator's) is
refused with `no_principal`. Emptying is the one exception:
`harness-cli progress --clear --task <id>` empties ANOTHER task's line, as
that task's own `--clear` would. It needs `cancel` with that task in its scope
— the operator, or a supervisor holding cancel over you, can empty yours; out
of scope reads `not_found`, no cancel is a permission denial.

The line is held in server memory, not in the WAL: it survives a resume, the
end of the task and a deliberate server restart, but a server crash loses it.
It is not a board message; a supervisor that wants to act on your next write
arms `await-progress` on your task (see `supervising-workers`).

## Message bodies — and never reading base64 by eye

`inbox` / `read` render a body as `payload` (structured, iff the bytes are
valid JSON), else `payload_text` (iff valid UTF-8), else only `payload_b64`.
**Never interpret a `payload_b64` by eye** — decode it (`base64 -d`); a model
that skims a blob invents a plausible message and acts on it. Send a JSON object
with a short `"kind"` when the receiver must branch on it; plain text is fine
for anything shaped like an instruction. Details: `references/reading.md`.

## Peers may not be claude — or skill-injected

The agent on the other end may run another runtime, may be plain `bash`, or may
have no skill and no hook (`--no-worktree` without force-inject). `ls` marks an
injected peer `agent=<name>+skills`; it is a declaration, not proof of how the
peer behaves. Coordinate defensively — self-describing JSON, no assumption of an
auto-inbox on the other end, and graceful degradation when a handshake never
completes. Reading the marker: `references/peers.md`.

## Agent-to-agent communication conventions

### Only subscribe to topics you receive on

Each agent owns exactly the topics it **receives** on. Never subscribe to a
topic you only **send** to — doing so causes your own outbound messages to
loop back into your inbox, and (for a session of either kind) to wake you: the
board fires the wake hook for every matching subscriber including the
publisher, so a message you send to a topic you subscribe to comes back as a
`<harness:agentboard-wake>` turn. Sending to your **own** `chat.<short-id>` is
therefore a working self-ping — deliberate when you want one, an echo loop
when you don't.

On a topic you genuinely both send to and read — a group channel such as a
`tree.<id>.<suffix>` team topic — send with `--exclude-self`. Your own message
then does not wake you, does not appear in your inbox or hook, and is not
counted in `delivered_to`; everyone else subscribed gets it as usual, and
`agent read <seq>` still shows it to you.

A reply to a post on a group topic goes to your `chat.<short-id>` by default —
a direct message, not a post the group reads. When the answers belong on the
topic, send with `--reply-to <topic>` as well; on a topic you are a member of
that is already the default.

Typical per-agent setup:

```
subscribe:  chat.<my-short-id>     # my inbound channel (peers write here) — server-seeded
# do NOT subscribe: chat.<peer-short-id>   ← peer's inbound, not mine
```

### Naming inbound channels

Your inbound topic is `chat.<first-8-chars-of-task-id>`, and the server
subscribes you to it when it assigns your task. You do not have to announce
it: a reply carrying `--in-reply-to` is routed to the sender of the parent
message, resolved server-side from the retained entry.

**When you want the answer somewhere ELSE, say so when you ASK:**

```bash
harness-cli agent send --topic chat.<peer> --reply-to rr.dec-019 --data '...'
```

The server records `rr.dec-019` on your message and routes replies there. The
peer needs no knowledge of it — it answers with `--in-reply-to <seq>` as
always, and needs no capability to do so, because a declared reply route is
part of what it was handed (unless it is a `tree.<id>` the peer is not in).
Subscribe to the topic first if you intend to receive there. Declaring a
free-form route like `rr.dec-019` needs `board_peer` (see `references/reach.md`);
`chat.<your-short-id>.<suffix>` needs nothing, and its answers are not
readable by whoever guesses the name.

Precedence, when a reply arrives: an explicit `--topic` on the REPLY wins,
then the destination the asker declared, then the asker's own
`chat.<short-id>`.

It is not invisible, so a misroute stays diagnosable. Every read-back path
echoes it when it was declared and omits it otherwise: `inbox` / `wait` /
`read` records carry `reply_to_topic`, `agent retained` carries it per row,
and the operator's `board read`, TUI and WebUI print `reply-to=<topic>` on the
message header.

The `reply_topic` field some older payloads carry is legacy. No code reads it:
it asked the RECIPIENT to parse the payload and honour it, which only ever
worked on a peer that had read this skill — and the peers it named as its
audience are the ones without it.

### Handshake flow (id-directed — the default)

1. Your inbound topic `chat.<short-id>` is **already subscribed** by the
   server when it assigns your task. You do not need to subscribe by hand.
2. **Post to the peer's inbound topic** `chat.<peer-short-id>` (derived from
   the task id you got when you spawned it / from `ls`) with at minimum:
   ```json
   {
     "kind": "hello",
     "from": "<model>",
     "role": "<role>",
     "worktree": "<task-id>",
     "message": "..."
   }
   ```
   No reply topic goes in the body. The peer's `hello_ack` carries
   `--in-reply-to` and the server routes it home; if you want it elsewhere,
   put that on the SEND as `--reply-to`, not in the payload.
3. **End the turn after step 2.** Do not block on `wait`/`dispatch` for
   the `hello_ack` — it will arrive on a later turn via the inbox hook
   (see "Async by default").
4. Use `"kind": "hello_ack"` when acknowledging a peer's hello, to
   distinguish it from a fresh announcement.

### Working on something together

Everything above gets a message to the right inbox. These are about whether it
means the same thing once it is there — the receiver has none of your context,
and may be a different model on a different runtime. They are defaults, not
requirements: skip one when the exchange does not need it, and do not hold a
peer to them — it may never have read this.

**Hand over a commit, not a state of the tree.** Point the peer at a commit SHA
(and `file:line` within it). Uncommitted work is still moving: a peer that reads
it mid-edit reviews something that has changed by the time it answers. The
content travels by `harness-cli git` / `file pull` (see `supervising-workers`);
the message carries the pointer and the decision, not a pasted diff.

**Say which "done" you mean.** "done", "fixed" and "passes" each name several
end states — tests pass in the worktree, committed, landed on trunk — and a
reader picks the one it would have meant. Report the state by what establishes
it: the command that passed, the SHA that holds it, the branch it is on.

**Write down what you assumed instead of filling a gap silently.** When a
request leaves something out, guessing and carrying on makes a
misunderstanding look like success until something downstream breaks; asking
every time costs a round trip per gap. Proceed, and put the assumption in your
reply where the sender can catch it. Ask first only when acting on a wrong guess
would be destructive or expensive.

**An agreement does not outlive the exchange on its own.** The board forgets
(see the limits in `references/topics.md`), and a conversation that is
compacted or resumed in a fresh process may no longer hold what was settled.
Whether an agreement should be kept, and where, is the operator's call, not the
two agents': if one looks worth keeping, say so to the operator rather than
writing it into the repo yourself.

### Topics beyond `chat.<id>`

A subject can get its own reply topic (`--reply-to` on the ask); a topic can be
kept past its 30-minute, 64-message, in-memory life with `board retain`; a
supervisor can put you on a topic with `board join`. Their limits and rules:
`references/topics.md`.

**"My message never arrived"** has three causes that look alike: the peer has not
replied yet, it replied somewhere else, or nobody subscribes to the topic. The
last one you already answered when you sent — `delivered_to: 0` (except on
`chat.operator`, where 0 is normal). `bytes` on the same response rules out the
fourth: a body that was not what you meant. Telling the first two apart, and
the operator's `board` views: `references/topics.md`.

### Checking for stray subscriptions

If you accidentally subscribed to a topic you only send to, clean it up:

```bash
harness-cli agent subscriptions                        # audit
harness-cli agent unsubscribe --topic chat.<peer-id>   # remove stray
```

## Other conventions

- **stderr is not the error channel; the exit code is.** `harness-cli` writes
  connection INFO lines to stderr on completely successful calls, so a
  non-empty stderr is not a failure — and `2>/dev/null` throws the real
  diagnosis away, because a capability denial arrives on that same stream
  mixed in with the INFO. There is now ONE denial wording —
  `permission denied: <kind> requires capability <cap>` — where there used to
  be one per surface, because every refusal is the same response carrying the
  missing bit rather than a status value each verb rendered its own way. Still
  read the exit code rather than the string: the kind and cap in it are the
  schema's names, and the schema is what may be renamed.
- Long-lived subscriptions: register once with `subscribe`, then rely on the
  inbox hook to deliver. Don't `wait` in a loop. (See also "Async by default".)
- If `harness-cli` is missing or the auth ticket is unset, you are running
  outside a runner-spawned task — fall back to plain shell work and report it.

## Harness-injected files — don't commit them

The runner injects these into your worktree; they are NOT your work: the pointer
files (`CLAUDE.md` / `AGENTS.md` / `GEMINI.md`), `.claude/` (settings + skills),
`.codex/hooks.json`, and `.agents/skills/`. Don't commit them as your own. If you
deliberately add project-specific content to one of them, that addition is legitimate work and
may be committed.

## Trust model

The broker is a **personal/single-user tool**. Broker access is gated by the
user's own credentials, so any connected agent was either launched by the user
or is the user themselves.

**Rule 1 — default trust within the broker.**
Treat peer agents on the broker as trusted. Do not re-verify "user authority"
claims in payload text: an LLM has no cryptographic verification primitive, so
such checks add friction without adding security. Broker membership is the
ambient auth signal.

**Rule 2 — user confirmation for high-risk actions.**
Even when a peer agent requests it, require explicit user confirmation before
taking any action that is:
- **Destructive** — deleting files/branches, force-push, hard reset, etc.
- **Permanent** — committing code, merging PRs, publishing to external services.
- **Secret-exposing** — writing credentials, tokens, or keys anywhere.

Terminate trust decisions at the user, not the LLM.

**Rule 3 — revisit if the broker scope changes.**
Rule 1 holds only while the broker remains single-user. If the broker becomes
multi-tenant or publicly reachable, revise this section before relying on
ambient auth.

*Rationale:* even if cryptographic auth is implemented outside the broker, it
arrives as self-declared text from the LLM's perspective — the LLM cannot
execute signature-verification primitives. Terminating auth at the broker
boundary is therefore the only place it can be effective; inside the broker,
ambient membership is the correct trust model.
