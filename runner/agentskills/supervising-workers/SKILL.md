---
name: supervising-workers
description: Use when acting on ANOTHER agent's task from inside a runner-spawned task — spawning / driving / killing worker sessions, one-shot `submit` + `logs` / `watch`, attenuating a child's capabilities with `--caps`, moving files in or out of a worker's worktree, running a command in one with `exec`, or reading a worker's diff. Peer-to-peer messaging, replying and subscriptions live in the `harness-cli` skill instead.
---

# supervising-workers (another agent's task, end to end)

Everything here is about a task acting on ANOTHER task, across its whole
lifetime: creating it, bounding what it may do, steering and watching it,
inspecting what it produced, and tearing it down. If you only need
to talk to a peer that already exists, you want the `harness-cli` skill —
sending, replying with `--in-reply-to`, subscriptions and the handshake
conventions are all there.

Read that skill first regardless: the trust model, the "you may not be the
first process on this task" caveat, and the async-by-default rule apply to
everything below.

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
  "message": "..."
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
**auto permission mode** by forwarding the flag through `--agent-arg`:

```bash
harness-cli session new -d --repo /path/to/repo \
  --agent-arg --permission-mode --agent-arg auto
```

`--permission-mode auto` is **Claude's** flag, and `--agent-arg` forwards it
verbatim to whatever binary the profile runs. Check the profile's `agent=` in
`ls`: a worker on another agent CLI needs that runtime's own equivalent, passed
the same way.

Without this the worker spawns in the default permission mode and will
stall on every permission prompt — and since the worker is detached, no
TTY is attached to answer them. Auto mode lets the worker proceed
through routine tool calls on its own while still respecting harder
safety boundaries (it is not the same as `bypassPermissions`). Use it
as the default for delegated work; reserve narrower modes for cases
where you have an explicit reason.

`--claude-arg` remains accepted as a deprecated alias for older scripts.

### Give the session a PTY size if its agent draws a TUI

```bash
harness-cli session new -d --repo /path/to/repo --rows 40 --cols 150
```

A detached session has **no size at all** until a client attaches, and the size
belongs to the CONTROL attach whenever someone holds it. Two ways to give one a
size without taking it over:

- size it here, at open — always available to the spawner;
- later, with **`harness-cli session resize --size 40x150 <id>`** (or
  `session send --resize 40x150 <id> <text>` to size and drive in one call).
  Needs `exec_resize` on top of `exec_view`, and applies only while **no
  control client is attached** — the unattended worker you spawned with `-d`,
  which is the case that used to have no answer at all. The capability is
  orthogonal to `exec_view`/`exec_cowrite`: ask for it explicitly, it is not
  implied by being allowed to type. The command exits 3 when the size did not
  take, so do not assume it worked.

Without either, `session exec <id> 'stty rows 40 cols 150'` still works when the
foreground is a POSIX shell — but not when it is the full-screen TUI whose size
you were trying to fix. Both flags are required together (one alone is ignored).

**And `stty` does not tell the SERVER**, so a session sized only that way
renders PARTIAL under `snapshot` — which reads as a broken program rather than
an unset size. If you intend to READ the screen, size it with `session resize`
/ `send --resize`. The mechanism and the measurement are in `session-debugging`
under `session resize`; they are not repeated here.

It matters when the worker's agent draws a full-screen TUI: at 0x0 codex and
agy paint literally nothing, so `snapshot` returns a blank screen and the
worker looks hung when it is merely undrawn. A claude worker is unaffected.

### Reuse the same task id with `--resume`

If a worker session gets canceled (failed, killed, you want a clean restart)
and you intend to start another one playing the **same role**, pass
`--resume <task-id>` so the new session keeps the same task id:

```bash
harness-cli session new -d --repo /path/to/repo --resume "$TASK_ID"
```

Same task id means the same `chat.<short-id>` inbound topic, so:

- Other agents that handshook with the previous session can keep talking
  to the new one; there is nothing to re-discover.
- The worktree branch `harness/<task-id>` is reused, so any commits the
  previous session made are still reachable.

But **`--resume` alone only restores the harness-level identity** — same
task id, same topic, same worktree. The new session still boots a fresh
agent process with no memory of the previous conversation. To also resume
the agent's own conversation (so the worker remembers what it was doing),
add `--resume-conversation`:

```bash
harness-cli session new -d --repo /path/to/repo \
  --resume "$TASK_ID" \
  --agent-arg --permission-mode --agent-arg auto \
  --resume-conversation
```

Use the flag, not `--agent-arg --continue`. `--continue` is Claude's
syntax; the flag makes the RUNNER pick the resume form of whichever agent
profile the task runs under (`exec resume --last` for Codex, `--continue`
for Claude, whatever `--agent-resume-*-argv` was configured). Injecting
`--continue` by hand works on Claude and breaks everywhere else.

Three layers move independently, and only two of them are yours to choose:

| Layer | Flag | What it restores |
|-------|------|------------------|
| harness task | `--resume <id>` | task id, chat topic, worktree branch |
| agent conversation | `--resume-conversation` | the agent's own session memory, in its own syntax |
| OS process | — | **nothing. Always new**, with a new auth ticket and possibly a different runner |

You almost always want both flags for a "pick up where it left off"
restart. Use `--resume` alone when you specifically want a clean agent
mind on the same identity (e.g. the previous run got stuck in a confused
state and you want a fresh start without losing the chat topic).

The third row is why a resumed worker cannot be assumed to still hold what
the previous run set up — see "You may not be the first process on this
task" above, which is the same fact seen from inside the resumed session.

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
harness-cli session ls            # JSON Lines: detachable interactive sessions only. Each row shares the `ls --json` task vocabulary (id, status, kind, repo, from, agent, caps, created_by, prompt, exit_code, created_at/started_at/ended_at, last_output_at, output_idle_ms) plus session-only is_attached + ring_buffer_bytes; it differs from `ls --json` ONLY by the interactive filter
harness-cli session kill <id>     # terminate one (alias of `cancel`)
harness-cli session snapshot <id> # PRINT the current screen as text (non-TTY; safe for you)
harness-cli session send -enter <id> "…" # inject input + Enter (non-TTY co-write); flags BEFORE the id
harness-cli session exec <id> <cmd>...  # RUN one shell cmd, wait, return combined output + exit code (POSIX-shell foreground)
harness-cli session await-idle --topic chat.<your-short-id> <id>  # one-shot "tell me when its turn ends"
harness-cli session attach <id>   # HUMAN ONLY (needs a real TTY) — see below
```

### Busy/idle detection — stop polling snapshots

An interactive session's PTY output is byte-quiescent exactly when the
foreground program is waiting for input: an in-flight agent turn repaints its
spinner ~every 100ms, an idle prompt emits nothing at all. Two surfaces:

- **Pull**: `session ls` includes `last_output_at` (unix nanos; 0 = no live
  session output) and `output_idle_ms` (server-clock idle age; only meaningful
  when `last_output_at > 0`), plus an `activity` string (`busy` / `idle:Xs`)
  when output has been seen. The top-level `ls` renders the same as an
  `act=busy` / `act=idle:Xs` badge, and `ls --json` carries the identical
  `activity` / `output_idle_ms` / `last_output_at` fields.
- **Push (one-shot)**: `session await-idle <id>` fires ONCE as soon as the
  session is quiescent for the threshold (default 2500ms; `--threshold-ms N`
  to override), then disarms itself. It is level-triggered, not edge-triggered:
  a session already idle when you arm fires immediately, and worst-case fire
  latency otherwise is threshold + the 500ms watch tick. Sinks:
  - default (no flag): LONG-POLLS — blocks until the fire, prints
    `{"status":"fired",...}` (exit 3 = session ended first). Fine for shell
    scripts; **never use this blocking form from an agent turn** (same rule
    as `agent wait`).
  - `--topic T`: replies `armed` immediately; on fire the server publishes
    `{"kind":"session_idle","task":"<32-hex>","status":"fired"|"session_stopped"}`
    to T. THE agent pattern: arm with your own `chat.<short-id>`, end the
    turn, and the fire wakes you via the inbox hook — replaces snapshot
    polling when babysitting a worker session, but NOT a reply you already
    asked for (below).
  - `--notify`: replies `armed` immediately; on fire the operator gets a
    notification. For "tell the human when it's done" (they may be away).

An idle fire means "waiting for input" — turn finished, permission prompt,
or a menu all look the same at the byte level. `snapshot` once after the
fire to see which; that needs `exec_view`. **Arming the watcher itself needs
no capability at all** — it reports `last_output_at`, which `ls` already gives
you — except `--notify`, which needs `notify` because it reaches the operator's
egress.

**Do not arm it for a peer you asked to report back.** A skill-aware claude
worker runs `agent send` *during* its turn, so its reply reaches you while the
PTY is still emitting; the idle fire lands ~3s later and, per the paragraph
above, cannot even say which kind of "waiting for input" it found. Two wakes
for one event, the second one strictly weaker. And **an armed watcher cannot be
disarmed** — `await-idle` takes only `--threshold-ms` / `--notify` / `--topic`,
and the server-side watcher ends by firing or by the session stopping — so
noticing that the reply came first does not get the wake back.

Arm it when **no reply is coming**:

- the peer has no agentboard path — `agent=bash`, an `agent=claude` without
  `+skills`, or any non-claude peer that must poll its own inbox;
- you drove the session by **keystrokes** (`session send`) rather than by
  message — a `/clear`, a permission answer, a menu selection. Quiescence is
  the only completion edge that exists there;
- `--notify`, to tell the human.

`await-idle` is the completion signal for the keyboard channel. The agentboard
channel already has one: the reply.

**Reading / driving a session as the agent (you have no TTY).** `session attach`
runs `RemoteShell`, which flips the *local* terminal into raw mode and splices it
to the remote PTY — it needs a real interactive TTY, which the human operator has
(TUI / WebUI) but you do not. Your tools are the non-TTY trio above —
`snapshot` (read the screen), `send` (inject keystrokes), `exec` (run one shell
command synchronously) — authenticated by your task ticket, no operator PSK.
They do not share one capability: `snapshot` is a `view` attach (`exec_view`),
`send`/`exec` are `cowrite` attaches (`exec_cowrite`). The caps imply downward,
so `exec_cowrite` covers all three. The full playbook lives in the
**session-debugging** skill (`harness-cli skill session-debugging`, or open
`.claude/skills/session-debugging/SKILL.md` /
`.agents/skills/session-debugging/SKILL.md`): tool choice (exec only fits a
POSIX-shell foreground), the send → snapshot drive loop, screen-reading flags
(`--style`/`--color`/`--raw`), and the footguns (flags go BEFORE `<id>` for
`send`/`exec`; a bare `exit` typed via `exec` kills the session; `exec` on a
TUI/REPL/claude foreground times out by design).

These suit **terminal-level** work (shells, TUIs, REPLs, or watching a screen).
To coordinate a *claude worker* (hand it tasks / corrections), still prefer
**agentboard messages** — the worker's claude reads its inbox; you don't need to
puppeteer its keyboard. (The human may also attach in parallel; that's expected.)

`session ls` lists only detachable interactive sessions; the top-level `ls`
shows every task (including one-shots). When more than one runner can serve the
repo, pin a spawn with `--runner <cid>` / `--host <name>` / `--ip <addr>`.

### Pruning tasks you spawned

`harness-cli prune` asks the server to forget tasks (they vanish from `ls`).
The server now bounds this to your scope — an id outside it is counted as
missing, and the age sweep only reaches tasks you can see — so the rules below
are about not destroying your OWN work, not about staying out of other
people's:

- Prune **only terminal** tasks (Succeeded / Failed / Cancelled) that **you**
  spawned — `by=<your-short-id>` in `ls`.
- `harness-cli prune <id>...` forgets the listed terminal tasks. With **no**
  ids it forgets every terminal task older than `--before` (default 168h)
  within your scope. If your scope is `global`, that is still everyone's tasks:
  do not run the bare/age form.
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

## Capabilities and scope (`--caps` / `--scope`)

`submit`, `interactive`, and `session new` accept both. **`--caps` names the
verbs; `--scope` names which tasks those verbs may be pointed at.** A cap
without a scope is not "may cancel" — it is "may cancel anything on the
server", which is why both exist. `harness-cli caps` lists the capability names
and the scope forms (`--json` for the machine-readable form).

- **Attenuating, never amplifying.** A child receives `creator_caps ∩ requested`
  — you can only grant a subset of what you yourself hold, and caps are
  monotonically non-increasing down a spawn chain.
- **Omitted ⇒ none.** No `--caps` flag means the child gets NOTHING: it is a
  data-plane-only worker (agentboard + its own logs/ls) and every
  control-plane verb answers `permission denied`. Name what it needs —
  `--caps spawn,file_read`, or `--caps all` for an unattenuated child (still
  intersected with your own set). A worker you expect to spawn or supervise
  its own children, attach to a PTY, move files, or notify the operator will
  NOT be able to until you say so. Forgot at spawn time? `caps set <id>
  --caps …` fixes it on the live task without a restart.
- **Operator = full set.** A task launched directly by the human operator (no
  principal task) is the trusted root and holds `all`.
- **`--scope` bounds the targets.** Default `subtree` = yourself and everything
  you spawned, transitively. `none` = yourself only. `global` = every task on
  the server, the explicit opt-out. `ids:<id>[,<id>]` = yourself plus exactly
  those tasks; `subtree+ids:<id>` = both. Every request naming a task — cancel,
  attach, file pull/push, git, await-idle, port forwards, and **resume** — is
  checked against it, and a task outside your scope answers *no such task*, the
  same as one that does not exist. Do not read that as a bug; read it as "not
  yours".
- **Scope attenuates like caps.** A child's base is clamped to yours
  (`none < subtree < global`), and every id you name must already be inside
  your own set at spawn time. An id that is not is an error, not a silent drop.
  You may name your own id, which is how you hand a child access to yourself.
- **Visibility is an axis of the scope, not a cap.** A scope is written
  `[<visibility>/]<action>`; omitting the visibility half makes it follow the
  action half. `--scope global/subtree` sees every task and acts on its own
  subtree — that is the shape the old `info_global` capability produced. It
  widens LOOKING only: you can see a task and still be refused when you try to
  cancel it.
  The reverse — acting wider than you can see — is refused, because a
  successful action discloses existence anyway, so the narrow visibility would
  be decorative.
  `board topics`, `board read` and `board subscribers` are the one thing that
  stayed a capability, now named `board_observe`: the board is keyed by topic,
  which the task hierarchy does not contain, so no task scope can bound it. It
  is NOT needed to send, to subscribe, or to read your own inbox.
- **One capability can be narrowed below the others.** `--scope-for CAPS=SCOPE`
  (repeatable, capability lists must not overlap) gives named bits their own
  target set: `--scope-for exec_cowrite,file_write=descendants` is "may drive
  its workers, may not drive itself". `descendants` is the subtree without
  self. An override never GRANTS a verb — one naming a bit you do not hold sits
  inert until something grants it. `whoami --json` and `ls --json` carry
  `scope_by_cap`, the resolved map, so you never have to work the merge out.
- **You also see your parent, redacted.** `ls` / `session ls` include one row
  for your direct creator so you can tell whether the task driving you is still
  running / idle. That row is deliberately sparse — `repo`, `prompt`,
  `worktree`, `agent` and the assigned runner come back empty. They were
  stripped, not absent; do not report "my parent has no prompt". Its `caps` and
  `created_by` ARE real. One hop only: your grandparent is not listed, and
  `logs` against your parent still returns not-found.
- **The operator can change your caps or scope while you run.** `harness-cli
  caps set <id>` is operator-only and takes effect on your very next request,
  with nothing restarted. If a call that worked a minute ago starts answering
  *no such task* or *permission denied*, re-run `whoami` before assuming a bug —
  and if the change narrowed you, your open attaches and transfers were closed
  with it.
- **Check your own caps with `harness-cli whoami`.** It prints THIS connection's
  own principal task id and the exact capability set and scope the server
  enforces for you
  (`--json` for the machine-readable form). No cap is required — it is
  self-introspection, not a peek at another task's record. Use it when unsure
  whether a denied RPC is a missing cap vs. a real error.

Granular names: `spawn`, `cancel`, `exec_view`, `exec_cowrite`,
`exec_control`, `exec_resize`, `exec_run`, `file_read`, `file_write`,
`forward_local`, `forward_remote`, `notify`, `prune`, `runner_admin`,
`board_observe`, `purge` — plus the aliases `none` / `all`. `harness-cli caps`
prints the authoritative list with a line each; this one can fall behind it.
`exec_resize` and `exec_run` sit BESIDE the three attach caps rather than
inside their ranking — resizing is availability and running a command in the
worktree touches no session at all, so neither is implied by being allowed to
type. The three attach caps are ranked and checked with implication:
`exec_view` reads a session, `exec_cowrite` also types into one, `exec_control`
also takes it over from whoever is driving. Grant a worker you only want to
WATCH `exec_view`; grant one you need to DRIVE `exec_cowrite`. `exec_control`
is for taking a terminal away from someone, which is rarely what you want. When you spawn a worker you
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
cancels the session's per-task context and kills the agent process. Cancel is
idempotent and skips already-terminal tasks. (`prune` / `prune-local` are
post-hoc cleanup of terminal tasks — server-side forget and local worktree
removal respectively — and are kind-agnostic.)

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

## Running a command in a worker's worktree — `harness-cli exec`

`harness-cli exec` runs ONE command in a task's worktree as its **own
process**. It needs `exec_run`.

This is the verb for "build it and tell me if it passes". It is not
`session exec`, and the difference decides which one you want:

| | `exec <id> -- <cmd>` | `session exec <id> <cmd>` |
| --- | --- | --- |
| where it runs | its own process in the worktree | typed into the session's FOREGROUND SHELL |
| needs a POSIX shell in the foreground | no | **yes** — times out against a TUI/REPL/claude |
| stdout vs stderr | separate | merged onto the one PTY stream |
| exit code | the command's own, and yours | parsed out of the combined output |
| visible to whoever is watching that session | no | **yes** — it lands in their scrollback |
| capability | `exec_run` | `exec_cowrite` |

```bash
# Words after -- are the argv, verbatim. Nothing re-splits them, so an
# argument containing a space stays one argument.
harness-cli exec <TASK_ID> -- make test
harness-cli exec <TASK_ID> -- git status
harness-cli exec <TASK_ID> -- sh -c 'go build ./... 2>&1 | head -40'

# Redirect the two streams independently — the reason this verb exists.
harness-cli exec <TASK_ID> -- make test >out.txt 2>err.txt

# What is running right now, and how to stop one.
harness-cli exec ls [-task <TASK_ID>] [--json]
harness-cli exec kill <EXEC_ID>
```

The exit code is the command's own, so `if harness-cli exec "$TASK_ID" -- make
test; then …` works. A command that never started — missing binary, no
worktree, killed — exits **125** with the reason on stderr; 125 rather than 127
because there is no shell here to have that convention.

The target is the **worktree**, not the task's status. A worker that ended with
uncommitted work keeps its tree, so `git status` and `make test` still run in
it, which is exactly when you want them. A worker that ended clean had its tree
removed and is refused.

**It is synchronous and dies with you.** There is no detached form — that is
what spawning a task is for. Do not use it to babysit something long: your turn
ends, the exec is killed. Every client can see it in `exec ls` while it runs,
and each task's row carries `execs=N`.

## Reading a worker's diff — `harness-cli git`

`harness-cli git` reads a task's git state without attaching to its shell, so
you can review what a worker changed while it is still running. It is
read-only: there is no commit, add, checkout or stash. It needs `file_read`.

```bash
# The task's commits.
harness-cli git <TASK_ID> log    [--max N] [-- <PATH>]

# The diff. Revisions are counted the way git counts them:
#   no revision  -> the unstaged change
#   one          -> that revision against the working tree
#   two          -> commit against commit
# --staged puts the index on the right-hand side instead.
harness-cli git <TASK_ID> diff   [--staged] [<BASE>] [<TARGET>] [-- <PATH>]

# One commit and its diff.
harness-cli git <TASK_ID> show   [<REV>] [-- <PATH>]

# Uncommitted and untracked paths, as `git status --porcelain`.
harness-cli git <TASK_ID> status [-- <PATH>]
```

Two things worth knowing before you read the output:

**A worker that has committed shows nothing under a plain `diff`.** That is
git, not a gap. Read `log` first, then name a baseline:

```bash
harness-cli git "$TASK_ID" log                    # find where it started
harness-cli git "$TASK_ID" diff <that-sha>        # everything since, committed or not
```

**Untracked files appear in no diff.** `status` is where a brand-new file
shows up, as a `??` entry; read its contents with `file pull`.

While the task is live the query runs in its worktree, so uncommitted and
untracked state is visible. After it ends the worktree is gone and the query
runs against the retained `harness/<task-id>` branch — committed work only,
and `status` is then empty because there is no working tree left to be dirty.
`HEAD` still means the task's own tip in that case, not the repository's
checkout. The runner that ran the task must still be online.



### One file, whole — `git <TASK_ID> file`

A diff shows the lines that changed; this shows the file they changed.

```bash
harness-cli git <TASK_ID> file <PATH>                # the file on disk
harness-cli git <TASK_ID> file --staged <PATH>       # the staged blob
harness-cli git <TASK_ID> file --rev <REV> <PATH>    # the blob at a revision
```

The path is relative to whichever repository the query is rooted in, so
`--subrepo` composes and a path lifted straight out of a diff header works
unchanged — after `--` as well as as a positional.

In the TUI's git modal, `o` toggles between the diff and the whole file
(scroll with space/f and b, or d/u for half a page — the arrow keys drive the
row picker); in the
WebUI, a file header inside a diff is clickable. Either way the side matches the
diff you were reading: the working tree for a worktree diff, the staged blob for
a staged one, that commit for a commit-to-commit diff or a shown commit.

### Repositories inside the repository

A plain nested repo — a directory with its own `.git` that the outer repo
only ever sees as one `?? nested/` entry — is invisible to every query above.
`--untracked-files=all` does not descend into one, and it appears in no diff
because it is untracked. Run the query inside it instead:

```bash
harness-cli git <TASK_ID> subrepos                    # what is nested in there
harness-cli git <TASK_ID> log  --subrepo pkg/inner    # its own history
harness-cli git <TASK_ID> diff --subrepo pkg/inner    # its own changes
```

`--subrepo` works on `log`, `diff`, `show` and `status`, and composes with
`-- <PATH>`: `--subrepo` chooses WHICH repository, `-- <PATH>` filters within
it. A path that is not a repository root is refused rather than silently
answering about the enclosing one.

Submodules show up in `subrepos` too, so the same route gives you a
submodule's full history. For a single combined diff instead, `--submodule`
on `diff` / `show` inlines the submodule's own file-level changes under its
gitlink entry. It is off by default because that output is no longer an
applyable patch.

A nested repo lives inside the task's worktree, so once the worktree is gone
`--subrepo` answers `no_source` — the retained `harness/<task-id>` branch is
in the outer repository and says nothing about it.

