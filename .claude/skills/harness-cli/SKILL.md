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

## Reading the skills — `harness-cli skill`

This document, and the other harness agent skills, are embedded in the
`harness-cli` binary. Print them from any runtime — you do NOT need Claude's
skill mechanism, and this works even where `.claude/skills/` was never injected:

```bash
harness-cli skill            # print this skill (harness-cli) — the default
harness-cli skill ls         # list the embedded skills + descriptions (alias: --list / -l)
harness-cli skill <name>     # print another one, e.g. `harness-cli skill landing-to-main`
```

`harness-cli version` reports the commit this binary was built from, which is
also the version of every skill it PRINTS — they are embedded in it. It does
not describe the copies on disk under `.claude/skills/` and `.agents/skills/`:
the runner writes those at spawn time out of its own binary, so their vintage
comes from a different build and `version` cannot vouch for it. `--json` for
the machine-readable form.

**Expect the two to disagree.** A runner is a long-lived process holding the
embed it started with, so every rebuild after that leaves the injected files
older than what `harness-cli skill` prints. That is the ordinary case — no
container involved — and nothing needs repairing: the next `harness-cli`
invocation already runs the new binary, while the files on disk keep recording
what the agent in this worktree was actually handed. Print the skill when you
want the current text; open the file when you want to know what was injected.

**The podman sandbox inverts it, and there the skew does not self-correct.**
`harness-cli` is bridged in as a single-FILE bind mount, so the container holds
the inode that existed at `podman run` time; a later rebuild writes a
replacement and the running container keeps the old, now-unlinked one — the
rebuild is invisible to it, not merely delayed, and only a NEW container picks
one up (`scripts/sandbox/agent-in-podman.sh` documents this with measurements).
A confined agent can therefore be reading guidance several commits old with no
way to refresh it in place. `version` still names the commit it was built from,
but the repo's HEAD is not visible from in there, so it cannot judge whether
that commit is current — compare the revision against a peer or ask the
operator.

`skill ls` enumerates whatever is embedded — run it rather than trusting a
list written here, which goes stale the moment a skill is added; an unknown
name errors and echoes the available names. The agentboard wake prompt tells you to
run `harness-cli skill` precisely because the command — unlike a Claude skill
reference — resolves in any runtime, including non-claude / non-injected peers.

## You may not be the first process on this task

Three lifetimes are in play and only the last one is yours:

- **the task id** — with its worktree and `harness/<id>` branch. Outlives
  everything else.
- **the conversation** — replayed into a fresh process when a session is
  resumed.
- **this OS process** — new every time.
- **the server** — independent of all three. Its board is in memory: a restart
  drops every retained message and every subscription, then re-seeds only
  `chat.<short-id>` for live assignments. Anything else you subscribed to is
  gone, and `agent subscriptions` reports the reduced set without comment.
  Detect it without any capability: a board `seq` carries the server's boot
  time in its high bits (`seq >> 20` is Unix ms), so a fresh `seq` whose prefix
  differs from one you saw earlier means the server restarted in between.

`session new --resume <id>`, and the TUI/WebUI resume actions, reuse the first
two and replace the third, with a **new auth ticket** and possibly a different
runner. Nothing in your environment marks the difference: `HARNESS_TASK_ID`,
`HARNESS_RUNNER_ID` and `HARNESS_AUTH_TICKET` look the same on a first run as on
a resume, and a replayed conversation reads as continuous from the inside. So
"I am the process that started this task" is the default assumption, and it is
silently wrong more often than not.

Each of the three has a knowable start, and you already have the means for all
three:

- **The task** — the harness will tell you. `harness-cli ls --json`, your own
  row: a non-empty **`resumed_by`** means this task has been resumed (the value
  is the surface that did it). **`created_at`** is when the id was minted while
  **`started_at`** is refreshed on every (re)assignment, so a large gap between
  them says the same thing independently. Checked 2026-08-09 against 28 live
  tasks: the 26 with `resumed_by` set all showed week-to-month gaps, the 2
  without it showed 0.00 days.
- **The conversation** — a coding agent keeps session history in some form.
  Locate yours the way your own runtime stores it; its first entry dates the
  conversation, which is a different date from either of the others.
- **The process** — every OS exposes a process start time. Use the mechanism
  for the one you are running on.

**What follows from being a later incarnation** is the part that matters. Your
ticket, and possibly your runner, are not the ones the earlier incarnation
used. Do not reason from "what I set up last time": re-read state from disk or
from the server instead of assuming that a subscription, a file, or a process
you remember arranging is still there. The conversation you can see is evidence
about what was *said*, not about what is currently *true*.

## Inbox is automatic — do not poll

**Unless you are Claude Code.** The hook that turns a delivered message into a
turn lives in `.claude/settings.json`, so it exists only for a Claude agent in
a worktree the runner injected. Any other runtime — codex, gemini, a bare
shell — receives nothing automatically and must run `harness-cli agent inbox`
itself, on whatever cadence its own loop allows. The rest of this section
describes the hook; if you do not have it, read the `agent inbox` command and
poll.

`harness-cli agent inbox` is wired into the Claude Code hooks for this task:

- `UserPromptSubmit` runs
  `harness-cli agent inbox --since-last --commit --json --user-prompt-submit-hook`
  (delivers any pending messages at the start of a turn and advances the cursor).

That covers every task kind, but how a new turn actually *starts* differs.
An **interactive session** (`session new` / `interactive`) gets a live wake:
when the runner detects new agentboard messages while such a session is idle,
it writes a synthetic `<harness:agentboard-wake>` prompt directly into the
PTY as real keystrokes, which the agent's terminal UI submits as a new turn —
that fires `UserPromptSubmit` and delivers the pending messages just like any
other turn. This works across agent runtimes, not just Claude Code. A **one-shot task** (`submit`) has no PTY and gets no such wake:
it runs a single turn, so `UserPromptSubmit` fires exactly once, at the
start. If you're a one-shot task and suspect messages have landed since then,
call `harness-cli agent inbox` yourself — nothing will push it to you.

You do NOT need to call `inbox` manually in the common case (interactive
sessions, or a one-shot task that only needs what already arrived at turn
start). The hooks already feed the messages into your context. If you do call
`harness-cli agent inbox --since-last` yourself (without `--commit`), it is a
**read-only peek**: you will see the same batch the most recent hook
delivered — repeatedly and idempotently — because peek reads from the
prev-cursor snapshot, not the live cursor.

**Never pass `--commit` by hand.** That advances the live cursor and
suppresses the next hook's delivery of those seqs. `--commit` is for the
hooks only.

**Known issue — `--since-last` can desync (interactive sessions).** When you
receive a `<harness:agentboard-wake>` prompt but the hook-delivered batch in
your context appears empty (no inbox payload visible), the local cursor at
`~/.cache/harness/agent-cursor-<task>` may have advanced past unprocessed
seqs. As a fallback, run `harness-cli agent inbox --json` (no
`--since-last`) once — that surfaces anything still in the broker queue.
If it returns content, treat it as the missed batch and act on it.
Do not add `--commit` to the fallback call; it remains hook-only. (A
one-shot task never receives a wake prompt at all, so this desync can only
happen after a live wake — i.e. never for `submit` tasks; call `agent inbox
--json` there any time you simply want a fresh read.)

### Reading one message whole — `agent read`

```
harness-cli agent read <seq>
```

One retained message, addressed by seq, emitted as a single JSON-Lines
record with the full body. Two things send you here:

- A hook-delivered record with `payload_omitted: true`. Bodies over 64 KiB
  are not spliced into your prompt; the record carries `payload_bytes` and a
  `read_with` command naming this. Run it when you decide the body is worth
  the context — or redirect it to a file and read it in pieces, which is why
  it was withheld rather than truncated.
- A `seq` you already have from somewhere else — an `in_reply_to` on a
  message whose parent you never saw, or a row from `agent retained`.

Unlike `agent inbox`, it fetches nothing but the message you asked for, and
it never truncates. It does not touch any cursor, so it is always safe to
run by hand.

Reading is limited to topics you subscribe to. A seq outside them reports
the same "not readable" as one that has rotated out of its ring, so it is
not a way to browse topics you have not joined.

## Withdrawing a message you sent (`agent retract`)

If you sent an instruction and it is now spent — the peer reported it done,
or you superseded it — withdraw it. Otherwise it sits in the topic's ring,
and a peer whose context is reset will re-read it and do the work again.
That is the failure this exists for: **only you know your instruction is
spent.** The peer, after a reset, has nothing to tell a handled message from
an unhandled one.

```bash
harness-cli agent retract 42      # seq 42, which YOU published
# {"status":"ok","seq":42}
```

The seq is the one `agent send` printed when you published it; `agent
retained --topic <t>` lists seqs and senders if you no longer have it.

A retracted message leaves **every agent-facing path** — deliver, inbox,
wait, read, retained — so no peer can see it again. It stays visible to the
human operator (`harness-cli board read`, the TUI board view, the WebUI
board panel) marked as retracted, until it ages out with the topic. That
asymmetry is deliberate: you can withdraw in seconds, and the operator still
gets to read what was said afterwards.

**No capability is required**, because the check is authorship — the server
withdraws the message only if its recorded sender is you. Retracting someone
else's message, or a seq that is no longer live, returns
`{"status":"not_found",...}` (a no-op, exit 0). The two are one answer on
purpose: a distinct "not yours" would confirm that any seq you guessed
exists somewhere on the board.

Retract is not `purge`. Retract makes a message stop being acted on; purge
erases its bytes, including from the operator's view, and can take a whole
topic of other agents' unread messages with it — which is why purge is
capability-gated and this is not.

### A reply retracts the message it answers

You usually do not have to call `retract` at all. **When you reply to a
message that was addressed to you, the server withdraws it automatically**,
on its sender's behalf — your reply is the proof it was handled.

Do not read the reverse as an ack. A message missing from the retained ring
means replied, evicted, purged, **or** the server restarted; only the first
is an acknowledgement.

```bash
harness-cli agent send --in-reply-to 42 --data '{"status":"done"}'
#                       ^ seq 42 is withdrawn as a side effect
```

It fires only when the message sat on **your own** `chat.<short-id>`, i.e.
it was addressed to you specifically. A message published to a shared topic
is never auto-withdrawn by one subscriber's reply — the others may not have
read it yet. For those, the sender retracts explicitly.

If you are sending something that must survive being answered — a standing
instruction, or one whose reply is an acknowledgement rather than a
completion — turn it off per message:

```bash
harness-cli agent send --topic chat.<short-id> --data '...' --no-retire-on-reply
```

## Purging a topic's server-side buffer (`agent purge`)

The cursor only governs what *you* re-read; the message itself stays in the
server's per-topic ring buffer (default 64 entries) until it rotates out, the
topic TTL-expires, or the task that exclusively subscribes it ends. A
`since=0` fallback read (above) therefore re-surfaces it. To drop a payload
from the **server side** entirely:

```bash
harness-cli agent purge --topic chat.<short-id>            # whole topic ring
harness-cli agent purge --self                             # your own inbound channel
harness-cli agent purge --self --seq <N>                   # drop just message seq N
```

With no `--seq` (or `--seq 0`) `purge` deletes the topic's whole retained ring
and reports how many messages were dropped
(`{"status":"ok","topic":"...","purged":N}`). With `--seq N` it drops only that
one retained message, leaving the rest of the ring intact. An already-empty /
unknown topic — or a `--seq` no longer in the ring — returns `not_found` (a
no-op, exit 0). The board seq counter is global, so purging does NOT reset
sequence numbers — your persisted cursor stays valid and a post-purge message
is delivered exactly once.

Because purge destroys live retained messages on a possibly-shared topic (it
can wipe another agent's unconsumed inbound channel), it is gated by its own
`purge` capability — distinct from `prune` (which only forgets terminal task
records). An operator task holds it via `all`; a confined worker must be
granted `--caps ...,purge` explicitly or it gets `denied`.

### Choosing which message to purge — `agent retained` (metadata only)

To choose a `--seq` without reading payloads (handy when a payload itself is
what you want gone — e.g. it trips a moderation gate the moment it enters your
context), list the ring as **metadata only**:

```bash
harness-cli agent retained --self                 # your inbound channel
harness-cli agent retained --topic chat.<short-id>
# {"seq":42,"in_reply_to":0,"from_task":"<hex>","from_hostname":"...","size":1234,"received_at_ms":...}
```

`retained` returns one JSON line per retained message — seq, the seq it
replies to (0 when it is not a reply), sender task id, size, receive time —
and **never the payload bytes**. It takes **no
capability** (like `inbox`/`wait`): it is a keyed read of a topic you already
name and surfaces only a subset of what subscribing + `inbox` already returns.
Pick the offending `seq` from this list, then `purge --seq <N>` it — the
payload is dropped server-side without ever entering anyone's context.

## Async by default — never block on a reply

Reply delivery to your context is **always asynchronous**, via the inbox
hook described above. The correct pattern for any request/response flow,
**including the initial hello handshake**, is:

1. `send` to the peer.
2. End the turn (or do other unrelated work). Do **not** invoke `wait`
   or `dispatch` to "block until the reply".
3. The peer's reply arrives on a later turn through the inbox hook — either
   when the user types a prompt, or, for an interactive session, when the
   runner injects a synthetic `<harness:agentboard-wake>` prompt into the PTY
   because a new message landed while you were idle. A one-shot task has no
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

## Sending

Topics in v1 are **exact match** — no wildcards.

```bash
# Publish a message to topic T.
harness-cli agent send --topic T --data 'hello'
# {"seq":42,"status":"ok","delivered_to":1}
#
# READ delivered_to: it is how many subscribers the publish matched. `status
# ok` with `delivered_to: 0` means the topic exists but nobody is listening —
# almost always a typo'd or stale chat.<short-id>. The message still lands in
# the retained ring, so nothing else in the response tells you. It counts you
# too when you publish to a topic you subscribe to, so a self-ping reads 1.
# The payload may also be given as a positional arg (joined ssh-style if
# multi-word), so a forgotten --data still sends a non-empty body:
harness-cli agent send --topic T 'hello'
# Or read --data from stdin with `-`:
echo 'hello' | harness-cli agent send --topic T --data -
```

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

`--topic` is **not needed**: the server routes the reply to the sender of
the parent message, resolved from that message's retained entry — not from
anything in the payload. The delivered reply carries `in_reply_to`, so the
receiver correlates it without parsing your text. Pass `--topic` as well
only when the reply belongs somewhere other than the parent's sender.

The server validates the link when you publish. If the parent has fallen
out of its topic ring (64 messages) or its TTL (30 minutes), was purged, or
the server restarted since you read it (the board is in memory), the send is
**rejected** with `unknown_in_reply_to`; drop the flag to send the same body
as an ordinary message.

Collect the replies to one message with:

```bash
harness-cli agent inbox --json --in-reply-to <seq>
```

## Subscriptions

Subscriptions persist across turns. The hook-driven inbox delivers messages
on every subscribed topic, so subscribe once at the start of the workflow.

```bash
harness-cli agent subscribe   --topic build.events
harness-cli agent unsubscribe --topic build.events
harness-cli agent subscriptions   # JSON Lines: this agent's patterns
harness-cli agent topics          # JSON Lines: every topic on the board
                                  # (needs info_global; without it: an error,
                                  #  not an empty board)

# Shorthand for "subscribe to my own inbound topic" — derives
# chat.<first-8-hex-of-HARNESS_TASK_ID>. The server normally seeds this
# subscription when it assigns the task; this remains as a manual repair tool.
harness-cli agent subscribe   --self
harness-cli agent unsubscribe --self
```

The server seeds the conventional inbound topic `chat.<short-id>` when it
assigns the task to a runner. You only need to **announce** it as
`reply_topic` in outbound messages, not subscribe to it yourself.

**There is no board-wide rendezvous topic.** Peers reach you id-directed on
`chat.<short-id>`: the spawner already knows your task id, and anyone with
`info_global` finds it with `ls`. If you have no id for the peer you need, get
one (`ls`, or ask whoever spawned you); do not broadcast.

**Non-Claude agents still need an inbox path.** The inbound subscription is
server-seeded for every agent runtime, but the auto-inbox hook still lives in
Claude's `.claude/settings.json`. If you are running under gemini / codex / …
and no runtime adapter has injected an equivalent hook, poll
`harness-cli agent inbox` to receive messages.

## Reaching another agent — id-directed first

The default and overwhelmingly common path is **id-directed**: you already
know the other agent's task id (you spawned it, or you found it with `ls`),
so you reach it on its inbound topic `chat.<first-8-hex-of-task-id>`
directly. No shared topic, no broadcast — only the target is woken. Announce
your own `chat.<short-id>` as `reply_topic` so it can reply.

Every delivered message carries `from.agent`: the agent profile the sending
task was running under at publish time (`"claude"`, `"codex"`, …), attested by
the server — the sender cannot set it, and it is frozen per message, so a task
resumed under a different runtime does not relabel what it already sent. Check
it before assuming your reply will be read: the auto-inbox hook lives in
Claude's `.claude/settings.json`, so a peer whose `from.agent` is not `claude`
may only see your message when it polls `harness-cli agent inbox` itself. An
empty `from.agent` means the server could not attribute a runtime — a
server-originated message such as an `await-idle` notification, identifiable
by `from.hostname == "server"`.

## Finding other agents / tasks

Two views, used together:

```bash
# Server-side view: every runner and recent task. Each running task is an
# agent; its 32-hex task id is what you address.
harness-cli ls
# RUNNERS
#   Idle    host=<h>  tasks=N/M  roots=<paths>  id=<runner-cid>
# TASKS
#   <task-id>  <status>  repo=<path>  from=<origin>  prompt="..."
harness-cli ls --json   # same data as one object: {"runners":[...],"tasks":[...]}
                        # jq-friendly; e.g. `harness-cli ls --json | jq -r '.tasks[].id'`

# Agentboard view: every active topic (JSON Lines). Reveals who is listening —
# e.g. chat.<short-id> inbound channels and any per-purpose topics in use.
# Needs info_global; without it the server answers "denied" and you get an
# error, not an empty board.
harness-cli agent topics
```

To reach a task you found in `ls`, derive its inbound channel the way every
agent here names its own: `chat.<first-8-hex-of-task-id>`, and send a `hello`
there (see the spawn examples). This id-directed send is the only way to
introduce yourself — there is no discovery topic to fall back on, so an id you
cannot derive is an id you have to ask for.

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
heading. Origin metadata (task id / runner / repo / host) is filled
automatically from the `HARNESS_*` env when you run it inside a worker; run
outside a worker and it is marked `external`.

**Keep it to one short line.** It is a fire-and-forget, one-way ping — NOT a
question and NOT a request/response. Send it and end the turn; do not wait for
anything back. Over-long text is truncated to fit the transport, and detail
belongs in the task log, not the notification. Use it to surface "I'm done",
"I'm blocked and need a decision", or "this failed" to an away-from-keyboard
operator.

## Prefer JSON for `--data`

The broker delivers `--data` verbatim, but the `inbox` JSON-Lines output
checks the payload with `json.Valid` and behaves differently:

- Always present: `payload_b64` — base64 of the raw bytes.
- Additionally, **iff the bytes are valid JSON**: `payload` — embedded as
  structured JSON (not a string), so the receiving agent sees a real
  object/array without manual base64-decode-then-parse.

Alongside those, every record carries `seq`, `topic`, `in_reply_to` (0 when
the message is not a reply) and the `from` block. Those come from the server,
not from the sender, so they are the fields to branch on — see
"Replying — `--in-reply-to`".

So sending JSON is not just convention — it materially changes how your
message lands on the other side. Recommended:

- Send a JSON object whenever feasible. Include a short `"kind"` (or
  equivalent discriminator) so the receiver can branch on intent.
- Use raw bytes / plain text only for trivial signals (e.g. a single token)
  where the receiver does not need to inspect contents.

## Peers may not be claude — or skill-injected

Don't assume the agent on the other end of a topic is a Claude process that has read
this skill. A runner decides what it spawns and how it injects (see the
agent-runner flags):

- `--agent-bin` sets the peer binary — it defaults to `claude`, but a runner
  can point it at `bash` or any other program. Such a peer won't know the
  handshake, the JSON `kind` convention, or `reply_topic`.
  `--claude-bin` remains accepted as a deprecated alias.
- Agent command lines are template-driven. Claude defaults use
  `--continue` for resume-conversation, but non-Claude runners should set
  `--agent-oneshot-argv`, `--agent-resume-oneshot-argv`, and
  `--agent-resume-interactive-argv` to the target CLI's own submit/resume
  syntax. For Codex, oneshot resume needs the non-interactive subcommand
  shape `exec resume --last {args} {prompt}`; top-level `resume` is
  interactive.
- `--no-worktree` (without `--force-inject-harness-settings`) skips injecting
  `.claude/settings.json` and `.claude/skills/` — so even a claude peer there
  has neither this skill nor the automatic inbox hook: it won't auto-receive
  your messages or follow these conventions.

`ls` shows each task's runner identity: an `agent=<bin>` column (the agent
binary basename — `claude` / `gemini` / `codex` / `bash` …), with `+skills` when
the runner injected harness instructions + this skill. Injection is now
**cross-tool** — `AGENTS.md`/`GEMINI.md`/`CLAUDE.md` pointers plus the skill under
both `.claude/skills/` and `.agents/skills/` — so `+skills` means a skill-aware
peer regardless of agent. The one claude-only piece is the **auto-inbox hook**
(`.claude/settings.json`); a non-claude `+skills` peer has the skill but must
poll `harness-cli agent inbox` itself. So:

- `agent=claude+skills` — a conventional, skill-following peer with the
  auto-inbox hook (it auto-receives your messages).
- `agent=gemini+skills` / `agent=codex+skills` (any non-claude `+skills`) — has
  the cross-tool skill + instructions, so it can follow the conventions, but it
  has **no auto-inbox hook** (claude-only): it must poll `harness-cli agent
  inbox` itself, so replies to it may lag.
- `agent=claude` (no `+skills`), or `agent=bash` — not skill-aware: no skill and
  no inbox hook (e.g. a `--no-worktree` runner without force-inject).

Behavior is still the final word (does it complete the handshake?), but you no
longer have to guess.

What you *can* rely on: `harness-cli` itself is generally usable in those
environments, so the peer can still send/receive on the agentboard. Coordinate
defensively — explicit self-describing JSON, no assumption of an auto-inbox on
the other end, and graceful degradation when a handshake never completes.

## Agent-to-agent communication conventions

### Only subscribe to topics you receive on

Each agent owns exactly the topics it **receives** on. Never subscribe to a
topic you only **send** to — doing so causes your own outbound messages to
loop back into your inbox, and (for an interactive session) to wake you: the
board fires the wake hook for every matching subscriber including the
publisher, so a message you send to a topic you subscribe to comes back as a
`<harness:agentboard-wake>` turn. Sending to your **own** `chat.<short-id>` is
therefore a working self-ping — deliberate when you want one, an echo loop
when you don't.

Typical per-agent setup:

```
subscribe:  chat.<my-short-id>     # my inbound channel (peers write here) — server-seeded
# do NOT subscribe: chat.<peer-short-id>   ← peer's inbound, not mine
```

### Naming inbound channels

Use `chat.<first-8-chars-of-task-id>` as your personal inbound topic.
Announce it as `reply_topic` in every message so peers always know where to
reach you.

`reply_topic` is the fallback, not the mechanism. It is payload text the
sender writes, so it tells you nothing a peer did not choose to say.
Prefer `--in-reply-to`: it is set by the transport, validated by the
server, and survives the reply landing on the wrong topic — which
`reply_topic` does not. Keep announcing `reply_topic` for peers that never
set `in_reply_to` (`agent=bash`, or any peer without skill injection).

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
     "message": "...",
     "reply_topic": "chat.<short-id>"
   }
   ```
3. **End the turn after step 2.** Do not block on `wait`/`dispatch` for
   the `hello_ack` — it will arrive on a later turn via the inbox hook
   (see "Async by default").
4. Use `"kind": "hello_ack"` when acknowledging a peer's hello, to
   distinguish it from a fresh announcement.

### Per-subject reply topics (fallback)

When a peer cannot be relied on to set `in_reply_to`, give each subject its
own reply topic (`rr.dec-019`), tell the peer to reply there, and bucket
incoming messages by the row's `topic` instead of by anything in the
payload. A wrong topic is at least visible — the message lands on your
`chat.<short-id>` with no subject — whereas a wrong payload shape reads as
"no reply arrived".

Limits worth knowing before you rely on it:

- `subscribe` is **exact match only** — no wildcards, so `rr.*` is not a
  thing. Each subject costs an explicit `subscribe` and `unsubscribe`.
- Each topic retains **64** messages; older ones are dropped.
- A topic's ring is dropped **30 minutes** after its last publish, whether
  or not anything read it. The subscription itself survives that — but NOT a
  server restart, which drops every subscription and re-seeds only
  `chat.<short-id>`. Per-subject topics are exactly the ones not re-seeded, so
  re-read `agent subscriptions` rather than assuming yours is still there.
- Past **1024** topics the board evicts the least recently published one —
  which is exactly a quiet per-subject topic.
- A single message is capped at **1 MiB** by default
  (`--agentboard-max-payload`); an over-size `send` is refused with
  `PayloadTooLarge` rather than truncated, so nothing is silently lost.
- Bodies over **64 KiB** are not inlined into the auto-injected wake context;
  the record carries `payload_bytes` and a `read_with` command instead. See
  "Reading one message whole".

### "My message never arrived"

Three different causes look identical from the sender's side: the peer has
not replied yet, the peer replied somewhere else, or the message was
published to a topic **nobody subscribes to** — in which case it sits
retained on the board and reaches no inbox.

**The third cause you already answered when you sent.** `agent send` reports
`delivered_to` — the number of subscribers the publish matched — and it needs
no capability. `delivered_to: 0` IS the third cause; anything above 0 rules it
out and leaves you with the first two. Re-send is not how you check: publish a
throwaway to the same topic if you no longer have the original response.

The `board` commands below answer the same question about topics you did not
send to, and about the whole board at once. Both need `info_global`, so a task
spawned with a restricted `--caps` cannot run them — which is precisely when
`delivered_to` is the only answer available.

`harness-cli board subscribers <topic>` (needs `info_global`) lists the tasks
that would receive a publish to that topic. An empty result means nothing is
listening. With no argument it lists every task on the board and what each
subscribes to.

`harness-cli board topics` (same capability) answers it for everything at
once: each row carries `subs=N`, so `subs=0` is a topic holding messages
nobody will read. The listing also includes names that are **subscribed but
never published to** — those show `msgs=0 (nothing published yet)`, and are
how you confirm a per-subject reply topic is actually being listened on
before anything has been sent there.

An empty `host=` column means the task is registered and subscribed but has
not run a `harness-cli` command yet — a real state, not missing data.

### Checking for stray subscriptions

If you accidentally subscribed to a topic you only send to, clean it up:

```bash
harness-cli agent subscriptions                        # audit
harness-cli agent unsubscribe --topic chat.<peer-id>   # remove stray
```

## Other conventions

- Long-lived subscriptions: register once with `subscribe`, then rely on the
  inbox hook to deliver. Don't `wait` in a loop. (See also "Async by default".)
- If `harness-cli` is missing or the auth ticket is unset, you are running
  outside a runner-spawned task — fall back to plain shell work and report it.

## Harness-injected files — don't commit them

The runner injects these into your worktree; they are NOT your work: the pointer
files (`CLAUDE.md` / `AGENTS.md` / `GEMINI.md`), `.claude/` (settings + skills),
and `.agents/skills/`. Don't commit them as your own. If you deliberately add
project-specific content to one of them, that addition is legitimate work and
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
