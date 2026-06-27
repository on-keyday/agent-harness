---
name: harness-cli
description: Use when working with other agents or the harness from inside a runner-spawned task — messaging peers on the agentboard, spawning / driving / killing worker agent sessions, delegating one-shot tasks, moving files in or out of a task's worktree, notifying the human operator, or discovering live agents and topics. Also defines the agentboard conventions (handshake, reply topics, trust model). Reply delivery is asynchronous via the inbox hook — never block on wait/dispatch from an agent turn.
---

# harness-cli (agent runtime)

`harness-cli` is on `PATH` inside this worktree. It is your control surface for
the whole harness: the agentboard (the only sanctioned way to talk to other
agents), worker-session lifecycle (`session new -d` / `session kill`), one-shot
`submit` + `logs` / `watch`, worktree file transfer (`file push` / `file pull`),
and operator notifications (`notify`). All required credentials are passed
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

### Targeting a single message — `agent retained` (metadata only)

To choose a `--seq` without reading payloads (handy when a payload itself is
what you want gone — e.g. it trips a moderation gate the moment it enters your
context), list the ring as **metadata only**:

```bash
harness-cli agent retained --self                 # your inbound channel
harness-cli agent retained --topic chat.<short-id>
# {"seq":42,"from_task":"<hex>","from_hostname":"...","size":1234,"received_at_ms":...}
```

`retained` returns one JSON line per retained message — seq, sender task id,
size, receive time — and **never the payload bytes**. It takes **no
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

The runner installs one `SessionStart` hook that runs `subscribe --self`.
So the conventional inbound topic `chat.<short-id>` is already live by the
time your first turn starts — you only need to **announce** it as
`reply_topic` in outbound messages, not subscribe to it yourself.

The runner does **not** auto-subscribe you to `harness.hello`. Peers reach
you id-directed on `chat.<short-id>` (the spawner already knows your task
id; humans use `ls` to find it), so a global topic everyone subscribed to
only turned every peer introduction into broadcast wake-noise. If you
genuinely need to discover a peer you have no prior id for, subscribe to
`harness.hello` yourself — see below.

**Non-claude agents must subscribe themselves.** That `SessionStart` hook
lives in the claude-only `.claude/settings.json`, so a peer running a different
agent (gemini / codex / …) does NOT get it — even with this skill present via
cross-tool injection. If that is you, run `harness-cli agent subscribe --self`
yourself at startup; otherwise peers can't reach you on `chat.<short-id>`.
(You also have no auto-inbox hook — poll `harness-cli agent inbox` to
receive.)

## Reaching another agent — id-directed first

The default and overwhelmingly common path is **id-directed**: you already
know the other agent's task id (you spawned it, or you found it with `ls`),
so you reach it on its inbound topic `chat.<first-8-hex-of-task-id>`
directly. No shared topic, no broadcast — only the target is woken. Announce
your own `chat.<short-id>` as `reply_topic` so it can reply.

### Opt-in discovery on `harness.hello`

For the rarer case where you need to find a peer you have **no prior id
for**, there is one reserved rendezvous topic: **`harness.hello`**. It is
opt-in — you are NOT auto-subscribed to it.

- Only if you need discovery: `subscribe --topic harness.hello`, announce
  yourself there (role, the topic(s) you accept work on, payload hints),
  and read others' announcements.
- Once two agents have agreed on a per-pair / per-purpose topic, switch the
  conversation to that topic and stop posting on `harness.hello`. It is for
  meeting, not ongoing chat.
- Don't reflexively announce on `harness.hello` at startup. If your peer
  relationship is already established by id (the usual case), skip it
  entirely — a startup broadcast to a topic nobody needs is pure noise.

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
there (see the spawn examples). This id-directed send is the normal way to
introduce yourself; `harness.hello` is only for the case where you have no id
to derive a channel from.

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

**Reuse > re-spawn, and never hand-type the id.** When you mean the *same
role*, resume the existing task instead of spawning a fresh one — it keeps
the chat topic and worktree (above) and avoids littering `ls` with dead
identities. Always take the task id from the spawn command's stdout or from
`ls` and keep it in a variable (`TASK_ID=$(harness-cli session new -d …)`);
do **not** transcribe a 32-hex id by hand — a mistyped/merged id silently
targets the wrong task or none. When a peer sends you an id, reconcile it
against `ls` before acting on it.

### Listing and killing your sessions

```bash
harness-cli session ls            # JSON Lines: detachable interactive sessions only (id, status, runner)
harness-cli session kill <id>     # terminate one (alias of `cancel`)
harness-cli session snapshot <id> # PRINT the current screen as text (non-TTY; safe for you)
harness-cli session send -enter <id> "…" # inject input + Enter (non-TTY co-write); flags BEFORE the id
harness-cli session attach <id>   # HUMAN ONLY (needs a real TTY) — see below
```

**Reading / driving a session as the agent (you have no TTY).** `session attach`
runs `RemoteShell`, which flips the *local* terminal into raw mode and splices it
to the remote PTY — it needs a real interactive TTY, which the human operator has
(TUI / WebUI) but you do not. For your own observation and driving, use the
non-TTY pair instead (both authenticate with your task ticket's `exec_attach`
capability — no operator PSK):

- **`session snapshot <id>`** renders the session's current screen to plain text
  via a headless VT emulator. It is a read-only `view` attach — it never disturbs
  whoever is driving. Use it to SEE what a shell / TUI / REPL / claude session is
  showing (`--rows/--cols` are a fallback if the session reports no size).
- **`session send [-enter] [-e] [--flush-ms MS] <id> <text>...`** injects
  keystrokes via a `cowrite` attach: it forwards your input WITHOUT taking over
  the human controller and WITHOUT resizing the PTY. `-enter` appends a carriage
  return (Enter) — a CR, so it submits on Windows cmd.exe too. `-e` interprets
  `\n \r \t \e \xHH \\` for control keys (e.g. `-e '\x03'` = Ctrl-C, `-e '\x1b'`
  = Esc). A stateless drive loop is just: `send`, then `snapshot` to read it.

  **`send` flags go before `<id>`; everything after `<id>` is the text**, joined
  ssh-style (`ssh host cmd args...`) — so multi-word input needs no quoting
  (`send -enter <id> echo hello world` sends `echo hello world`). Quote it as one
  argument to preserve exact whitespace. Keep flags before `<id>`: a `-enter`
  placed AFTER the text is taken as literal text (you'll see it typed in the
  snapshot instead of submitting), so it won't act as the Enter flag.

- **`session snapshot` / `session attach` flags are order-free** — their only
  positional is the `<id>` (never `-`-leading), so `snapshot <id> --rows 50` and
  `snapshot --rows 50 <id>` are equivalent.

These suit **terminal-level** work (shells, TUIs, REPLs, or watching a screen).
To coordinate a *claude worker* (hand it tasks / corrections), still prefer
**agentboard messages** — the worker's claude reads its inbox; you don't need to
puppeteer its keyboard. (The human may also attach in parallel; that's expected.)

`session ls` lists only detachable interactive sessions; the top-level `ls`
shows every task (including one-shots). When more than one runner can serve the
repo, pin a spawn with `--runner <cid>` / `--host <name>` / `--ip <addr>`.

### Pruning tasks you spawned

`harness-cli prune` asks the server to forget tasks (they vanish from `ls`).
Conventions — pruning is shared-state surgery, so stay narrow:

- Prune **only terminal** tasks (Succeeded / Failed / Cancelled) that **you**
  spawned — `by=<your-short-id>` in `ls`. Leave the user's tasks, and any task
  you did not create, alone.
- `harness-cli prune <id>...` forgets the listed terminal tasks. With **no**
  ids it forgets every terminal task older than `--before` (default 168h) — do
  **not** run that bare/age form on a shared server; it sweeps everyone's tasks.
- `--force` also forgets **non-terminal** tasks (Queued / Running / Detached).
  Those are live or resumable, so `--force` is destructive and hard to reverse
  — use it only with an explicit, current reason (e.g. the user asked you to
  clear specific stale Detached workers), never as a reflex to get past a skip.
- After verification / probe work, prune the throwaway tasks you spawned so
  `ls` stays readable.

Pipe ids straight from output; never hand-type a 32-hex id:

```bash
# forget the terminal tasks you spawned under a given repo
harness-cli ls | sed -n '/^TASKS/,$p' | grep "$REPO" \
  | grep -wE 'Failed|Cancelled|Succeeded' | grep "by=$MY_SHORT" \
  | awk '{print $1}' | xargs -r harness-cli prune
```

## Capabilities (`--caps`) — attenuate what a child task may do

`submit`, `interactive`, and `session new` all accept `--caps <names>` to bound
what the task you spawn may do on the control plane. This is the harness's
privilege-attenuation seam for delegated work — list the names and what each
authorizes with `harness-cli caps` (`--json` for the machine-readable form).

- **Attenuating, never amplifying.** A child receives `creator_caps ∩ requested`
  — you can only grant a subset of what you yourself hold, and caps are
  monotonically non-increasing down a spawn chain.
- **Omitted ⇒ inherit-all.** No `--caps` flag means the child inherits every cap
  you hold (the server intersects with your set). Pass `--caps none` for a
  data-plane-only worker (agentboard + its own logs/ls), or a comma list like
  `--caps spawn,file_read` to grant exactly those.
- **Operator = full set.** A task launched directly by the human operator (no
  principal task) is the trusted root and holds `all`.
- **Visibility is a cap too.** Without `info_global`, a confined task's `ls` and
  `agent topics` show only its own task subtree (itself + descendants), not the
  whole board; `info_global` (part of `all`) lifts that.

Granular names: `spawn`, `cancel`, `exec_attach`, `file_read`, `file_write`,
`forward_local`, `forward_remote`, `notify`, `prune`, `runner_admin`,
`info_global`, `purge` — plus the aliases `none` / `all`. When you spawn a worker you
intend to keep driving, grant the narrowest set that lets it finish, and widen
only if it hits a capability-denied error.

## One-shot tasks & monitoring (`submit`, `logs`, `watch`)

`submit` is the fire-and-forget counterpart to `session new -d`: it enqueues a
one-shot task that runs to completion and exits, with no way to step in mid-run
(see "Why detached sessions over `submit`" above). **Prefer `session new -d`**
for anything collaborative; reach for `submit` only for genuinely narrow,
no-intervention jobs.

```bash
harness-cli submit --repo /path/to/repo --task "one-line job ..."
```

Because a submitted task gives you no live channel, you observe it from outside:

```bash
harness-cli logs [-f|--follow] <TASK_ID>   # dump log history; -f streams live until the task is terminal
harness-cli watch                          # stream task + runner status events (all tasks)
harness-cli cancel <TASK_ID>               # cancel a queued/running task
harness-cli prune [--before DUR] [-f] [TASK_ID ...]   # ask the server to forget terminal tasks
```

**`logs` and `watch` only cover one-shot (`submit`) tasks.** A submitted task's
stdout is captured to a server-side log (`logs` reads it) and its queue →
assigned → ended transitions are published as status events (`watch` reports
them). An **interactive session (`session new` / `interactive`) has neither**:
its output streams over the PTY and is replayed from a ring buffer on attach —
it is never written to the task log — and it is opened directly rather than
through the queue/dispatch lifecycle, so it emits no `watch` events. Observe an
interactive worker over the **agentboard** — it reports back to you there.
(`session attach` is a human/PTY tool, not for you — see "Listing and killing
your sessions" above; `logs` / `watch` don't apply to interactive tasks.)

`cancel <id>` and `session kill <id>` (its alias), by contrast, **do** work on
interactive sessions: they route a `CancelTask` to the assigned runner, which
cancels the session's per-task context and kills the claude process. Cancel is
idempotent and skips already-terminal tasks. (`prune` / `prune-local` are
post-hoc cleanup of terminal tasks — server-side forget and local worktree
removal respectively — and are kind-agnostic.)

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

## Peers may not be claude — or skill-injected

Don't assume the agent on the other end of a topic is a claude that has read
this skill. A runner decides what it spawns and how it injects (see the
agent-runner flags):

- `--claude-bin` sets the peer binary — it defaults to `claude`, but a runner
  can point it at `bash` or any other program. Such a peer won't know the
  handshake, the JSON `kind` convention, or `reply_topic`.
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
loop back into your inbox.

Typical per-agent setup:

```
subscribe:  chat.<my-short-id>     # my inbound channel (peers write here) — auto via --self
subscribe:  harness.hello          # OPT-IN, only if you need peer discovery
# do NOT subscribe: chat.<peer-short-id>   ← peer's inbound, not mine
```

### Naming inbound channels

Use `chat.<first-8-chars-of-task-id>` as your personal inbound topic.
Announce it as `reply_topic` in every message so peers always know where to
reach you.

### Handshake flow (id-directed — the default)

1. Your inbound topic `chat.<short-id>` is **already subscribed** by the
   runner's `--self` SessionStart hook. You do not need to subscribe by hand.
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

### Discovery variant (only when you have no peer id)

`subscribe --topic harness.hello`, post the same `hello` payload there
instead of to a peer topic, then end the turn. When a peer answers, switch
to the pair topics and stop posting on `harness.hello`.

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
