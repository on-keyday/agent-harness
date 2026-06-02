---
name: harness-cli
description: Use when sending messages to other agents on the agentboard or managing topic subscriptions. Reply delivery is asynchronous via the inbox hook — `send` and end the turn; replies arrive on a later turn. Provides reference for the `harness-cli agent` subcommands available inside this task.
---

# harness-cli (agent runtime)

`harness-cli` is on `PATH` inside this worktree. It is the only sanctioned way
to talk to other agents on the agentboard. All required credentials are passed
via `HARNESS_*` environment variables (already set by the runner) — never pass
them as flags.

## Inbox is automatic — do not poll

`harness-cli agent inbox` is wired into the Claude Code hooks for this task:

- `UserPromptSubmit` runs `harness-cli agent inbox --since-last --commit --json`
  (delivers any pending messages on each user prompt and advances the cursor).

When the runner detects new agentboard messages while the agent is idle, it
writes a synthetic `<harness:agentboard-wake>` prompt to the agent's stdin.
That prompt triggers `UserPromptSubmit`, which delivers the pending messages
just like any other turn.

You do NOT need to call `inbox` manually. The hooks already feed the messages
into your context. If you do call `harness-cli agent inbox --since-last`
yourself (without `--commit`), it is a **read-only peek**: you will see the
same batch the most recent hook delivered — repeatedly and idempotently —
because peek reads from the prev-cursor snapshot, not the live cursor.

**Never pass `--commit` by hand.** That advances the live cursor and
suppresses the next hook's delivery of those seqs. `--commit` is for the
hooks only.

**Known issue — `--since-last` can desync.** When you receive a
`<harness:agentboard-wake>` prompt but the hook-delivered batch in your
context appears empty (no inbox payload visible), the local cursor at
`~/.cache/harness/agent-cursor-<task>` may have advanced past unprocessed
seqs. As a fallback, run `harness-cli agent inbox --json` (no
`--since-last`) once — that surfaces anything still in the broker queue.
If it returns content, treat it as the missed batch and act on it.
Do not add `--commit` to the fallback call; it remains hook-only.

## Async by default — never block on a reply

Reply delivery to your context is **always asynchronous**, via the inbox
hook described above. The correct pattern for any request/response flow,
**including the initial `harness.hello` handshake**, is:

1. `send` to the peer.
2. End the turn (or do other unrelated work). Do **not** invoke `wait`
   or `dispatch` to "block until the reply".
3. The peer's reply arrives on a later turn through the inbox hook —
   either when the user types a prompt, or when the runner injects a
   synthetic `<harness:agentboard-wake>` prompt because a new message
   landed while you were idle.

Why this rule exists:

- `wait` / `dispatch` block the agent's bash process for the full
  timeout. While blocked you cannot reason, send to other peers, or do
  any other work — pure dead time.
- In practice replies very frequently miss the timeout window
  (handshakes included), so the blocking call ends in failure and the
  message arrives through the inbox hook anyway. The synchronous form
  has no payoff and a real cost.
- State that needs to survive across turns ("I'm waiting on a reply
  from peer X about Y") belongs in `TodoWrite` or memory, not in a
  blocking wait.

`harness-cli agent wait` and `harness-cli agent dispatch` exist as
shell-level escape hatches for scripting **outside** the agent's turn
loop. The agent itself must not call them.

## Sending

Topics in v1 are **exact match** — no wildcards.

```bash
# Publish a message to topic T.
harness-cli agent send --topic T --data 'hello'
# Or read --data from stdin with `-`:
echo 'hello' | harness-cli agent send --topic T --data -
```

That is the only command an agent normally runs to talk to peers. End
the turn after sending; replies arrive through the inbox hook.

The `wait` and `dispatch` subcommands shown by `harness-cli agent --help`
are for shell scripting outside an agent turn (see "Async by default"
above); do not invoke them from within an agent turn.

## Subscriptions

Subscriptions persist across turns. The hook-driven inbox delivers messages
on every subscribed topic, so subscribe once at the start of the workflow.

```bash
harness-cli agent subscribe   --topic build.events
harness-cli agent unsubscribe --topic build.events
harness-cli agent subscriptions   # JSON Lines: this agent's patterns
harness-cli agent topics          # JSON Lines: every topic on the board

# Shorthand for "subscribe to my own inbound topic" — derives
# chat.<first-8-hex-of-HARNESS_TASK_ID>. Used by the SessionStart hook
# below; rarely needed by hand.
harness-cli agent subscribe   --self
harness-cli agent unsubscribe --self
```

The runner installs two `SessionStart` hooks: one subscribes to
`harness.hello` and one runs `subscribe --self`. So the conventional
inbound topic `chat.<short-id>` is already live by the time your first
turn starts — you only need to **announce** it as `reply_topic` in
outbound messages, not subscribe to it yourself.

## Handshake on `harness.hello`

The broker has no schema or capability registry. To keep multi-agent work
from depending on out-of-band convention, there is exactly one reserved
well-known topic: **`harness.hello`**.

- On startup, subscribe to `harness.hello` and announce yourself there
  (role, the topic(s) you will accept work on, and any payload hints other
  agents need). Read other agents' announcements on the same topic.
- Once two agents have agreed on a per-pair / per-purpose topic via
  `harness.hello`, switch the actual conversation to that topic and stop
  posting traffic on `harness.hello`.
- `harness.hello` is for meeting, not for ongoing chat. Treat it as the
  one channel guaranteed to exist; everything else is negotiated.

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

# Agentboard view: every active topic (JSON Lines). Reveals who is listening —
# e.g. chat.<short-id> inbound channels and any per-purpose topics in use.
harness-cli agent topics
```

To reach a task you found in `ls`, derive its inbound channel the way every
agent here names its own: `chat.<first-8-hex-of-task-id>`, and send a `hello`
there (see the handshake / spawn examples). `ls` tells you *which* agents exist
and their status; `harness.hello` is how you introduce yourself to one that is
listening.

## Spawning a worker agent

When you need to delegate work to another agent that you intend to keep
talking to, prefer **`harness-cli session new -d`** over `submit`.

```bash
# Spawn a detached interactive PTY agent on a specific repo. Prints the
# new task id on stdout; the agent stays alive in the background.
TASK_ID=$(harness-cli session new -d --repo /path/to/repo)

# Reach it on the agentboard. The agent's inbound channel is
# chat.<first-8-chars-of-TASK_ID> — same convention this skill uses for
# every agent's "naming inbound channels" rule.
SHORT_ID=${TASK_ID:0:8}
harness-cli agent send --topic "chat.$SHORT_ID" --data "$(cat <<'JSON'
{
  "kind": "hello",
  "from": "<your role>",
  "message": "...",
  "reply_topic": "chat.<your-short-id>"
}
JSON
)"
```

Why detached sessions over `submit`:

- `submit` enqueues a **one-shot** task — claude runs to completion with
  the prompt you supplied and then exits. Once it is running, neither you
  nor the user can step in to adjust direction, answer a clarifying
  question, or feed it new context. That makes it a bad fit for any
  collaborative workflow.
- `session new -d` keeps the worker alive between turns, so you can drive
  it iteratively via agent messages and the user can also intervene at
  any time (attach with `session attach <task-id>`, send corrections via
  the agentboard, etc.).
- `submit` still has a place for genuinely one-shot, narrow tasks ("give
  me a one-line summary of X") where mid-task intervention is not needed
  — but treat it as the exception.

### Use auto mode for complex delegated work

For any non-trivial worker — anything that will require multiple tool
calls, file edits, or long autonomous stretches — start the worker in
**auto permission mode** by forwarding the flag through `--claude-arg`:

```bash
harness-cli session new -d --repo /path/to/repo \
  --claude-arg --permission-mode --claude-arg auto
```

Without this the worker spawns in the default permission mode and will
stall on every permission prompt — and since the worker is detached, no
TTY is attached to answer them. Auto mode lets the worker proceed
through routine tool calls on its own while still respecting harder
safety boundaries (it is not the same as `bypassPermissions`). Use it
as the default for delegated work; reserve narrower modes for cases
where you have an explicit reason.

### Reuse the same task id with `--resume`

If a worker session gets canceled (failed, killed, you want a clean restart)
and you intend to start another one playing the **same role**, pass
`--resume <task-id>` so the new session keeps the same task id:

```bash
harness-cli session new -d --repo /path/to/repo --resume "$TASK_ID"
```

Same task id means the same `chat.<short-id>` inbound topic, so:

- Other agents that handshook with the previous session can keep talking
  to the new one without re-discovering it via `harness.hello`.
- The worktree branch `harness/<task-id>` is reused, so any commits the
  previous session made are still reachable.

But **`--resume` alone only restores the harness-level identity** — same
task id, same topic, same worktree. The new session still boots a fresh
claude process with no memory of the previous conversation. To also
resume at the claude conversation level (so the worker remembers what it
was doing), pass `--claude-arg --continue` as well:

```bash
harness-cli session new -d --repo /path/to/repo \
  --resume "$TASK_ID" \
  --claude-arg --permission-mode --claude-arg auto \
  --claude-arg --continue
```

Think of it as two independent layers:

| Layer | Flag | What it restores |
|-------|------|------------------|
| harness task | `--resume <id>` | task id, chat topic, worktree branch |
| claude conversation | `--claude-arg --continue` | claude's in-directory session memory |

You almost always want both for a "pick up where it left off" restart.
Use `--resume` alone only when you specifically want a clean claude
mind on the same identity (e.g. the previous run got stuck in a
confused state and you want a fresh start without losing the chat
topic).

Without `--resume` you get a fresh task id and the peers' link to the
previous identity is dead — they will need a new hello round.

## Moving files in / out of a worker's worktree

`harness-cli file` reads and writes files inside a task's **worktree** — the
per-task `harness/<task-id>` checkout the runner created for it, not arbitrary
host paths. Use it to seed a worker you spawned with input files, or to collect
its artifacts. `WORKTREE_REL_*` paths are POSIX and relative to the worktree
root.

```bash
# List one directory (default: worktree root).
harness-cli file ls     <TASK_ID> [WORKTREE_REL_DIR]

# Copy a local file INTO the worktree (-r: directory tree).
# Default is O_EXCL — refuses to overwrite; -f permits replacement.
harness-cli file push   [-r] [-f] <TASK_ID> <LOCAL_SRC> <WORKTREE_REL_DST>

# Copy a worktree file OUT to a local path (-r: directory tree).
# Default refuses to overwrite the local target; -f permits replacement.
harness-cli file pull   [-r] [-f] <TASK_ID> <WORKTREE_REL_SRC> <LOCAL_DST>

# Remove a file. -r targets a directory (dir_delete); -r -f removes a
# non-empty directory (RemoveAll). Without -r a directory is refused.
harness-cli file delete [-r] [-f] <TASK_ID> <WORKTREE_REL_PATH>
```

`<TASK_ID>` is the 32-hex id from `session new` / `submit` (the same id behind
the `chat.<short-id>` topic). Typical seed → run → collect flow with a worker:

```bash
TASK_ID=$(harness-cli session new -d --repo /path/to/repo)
harness-cli file push "$TASK_ID" ./spec.md docs/spec.md     # hand it inputs
# ... drive it via the agentboard; let it work ...
harness-cli file pull "$TASK_ID" out/report.md ./report.md  # collect outputs
```

Prefer this over having the worker paste large files through agentboard
messages: `file` streams the bytes directly and keeps the agentboard for
coordination, not bulk transfer.

## Prefer JSON for `--data`

The broker delivers `--data` verbatim, but the `inbox` JSON-Lines output
checks the payload with `json.Valid` and behaves differently:

- Always present: `payload_b64` — base64 of the raw bytes.
- Additionally, **iff the bytes are valid JSON**: `payload` — embedded as
  structured JSON (not a string), so the receiving agent sees a real
  object/array without manual base64-decode-then-parse.

So sending JSON is not just convention — it materially changes how your
message lands on the other side. Recommended:

- Send a JSON object whenever feasible. Include a short `"kind"` (or
  equivalent discriminator) so the receiver can branch on intent.
- Use raw bytes / plain text only for trivial signals (e.g. a single token)
  where the receiver does not need to inspect contents.

## Agent-to-agent communication conventions

### Only subscribe to topics you receive on

Each agent owns exactly the topics it **receives** on. Never subscribe to a
topic you only **send** to — doing so causes your own outbound messages to
loop back into your inbox.

Typical per-agent setup after a handshake:

```
subscribe:  harness.hello          # always — for new-agent discovery
subscribe:  chat.<my-short-id>     # my inbound channel (peers write here)
# do NOT subscribe: chat.<peer-short-id>   ← peer's inbound, not mine
```

### Naming inbound channels

Use `chat.<first-8-chars-of-task-id>` as your personal inbound topic.
Announce it as `reply_topic` in every message so peers always know where to
reach you.

### Full handshake flow

1. Both `harness.hello` and your inbound topic are **already subscribed**
   by the runner's SessionStart hooks (see Subscriptions). You do not need
   to subscribe by hand.
2. **Post to `harness.hello`** with at minimum:
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
   (see "Async by default"). When it does, switch all further
   conversation to the pair topics; stop using `harness.hello` for
   ongoing chat.
4. Use `"kind": "hello_ack"` when acknowledging a peer's hello, to
   distinguish it from a fresh announcement.

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
