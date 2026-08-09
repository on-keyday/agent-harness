---
name: supervising-workers
description: Use when acting on ANOTHER agent's task from inside a runner-spawned task — spawning / driving / killing worker sessions, one-shot `submit` + `logs` / `watch`, attenuating a child's capabilities with `--caps`, moving files in or out of a worker's worktree, or reading a worker's diff. Peer-to-peer messaging, replying and subscriptions live in the `harness-cli` skill instead.
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
**auto permission mode** by forwarding the flag through `--agent-arg`:

```bash
harness-cli session new -d --repo /path/to/repo \
  --agent-arg --permission-mode --agent-arg auto
```

Without this the worker spawns in the default permission mode and will
stall on every permission prompt — and since the worker is detached, no
TTY is attached to answer them. Auto mode lets the worker proceed
through routine tool calls on its own while still respecting harder
safety boundaries (it is not the same as `bypassPermissions`). Use it
as the default for delegated work; reserve narrower modes for cases
where you have an explicit reason.

`--claude-arg` remains accepted as a deprecated alias for older scripts.

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
- **Edge (one-shot)**: `session await-idle <id>` fires ONCE when the session
  next goes quiescent for the threshold (default 2500ms; `--threshold-ms N`
  to override), then disarms itself. Sinks:
  - default (no flag): LONG-POLLS — blocks until the fire, prints
    `{"status":"fired",...}` (exit 3 = session ended first). Fine for shell
    scripts; **never use this blocking form from an agent turn** (same rule
    as `agent wait`).
  - `--topic T`: replies `armed` immediately; on fire the server publishes
    `{"kind":"session_idle","task":"<32-hex>","status":"fired"|"session_stopped"}`
    to T. THE agent pattern: arm with your own `chat.<short-id>`, end the
    turn, and the fire wakes you via the inbox hook — replaces snapshot
    polling when babysitting a worker session.
  - `--notify`: replies `armed` immediately; on fire the operator gets a
    notification. For "tell the human when it's done" (they may be away).

An idle fire means "waiting for input" — turn finished, permission prompt,
or a menu all look the same at the byte level. `snapshot` once after the
fire to see which; requires `exec_attach` (same cap as snapshot/send).

**Reading / driving a session as the agent (you have no TTY).** `session attach`
runs `RemoteShell`, which flips the *local* terminal into raw mode and splices it
to the remote PTY — it needs a real interactive TTY, which the human operator has
(TUI / WebUI) but you do not. Your tools are the non-TTY trio above —
`snapshot` (read the screen), `send` (inject keystrokes), `exec` (run one shell
command synchronously) — all authenticated by your task ticket's `exec_attach`
capability, no operator PSK. The full playbook lives in the
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
  whole board; `info_global` (part of `all`) lifts that. `info_global` also
  gates the operator board reads — `board topics`, `board read` and
  `board subscribers`.
- **You also see your parent, redacted.** `ls` / `session ls` include one row
  for your direct creator so you can tell whether the task driving you is still
  running / idle. That row is deliberately sparse — `repo`, `prompt`,
  `worktree`, `agent` and the assigned runner come back empty. They were
  stripped, not absent; do not report "my parent has no prompt". Its `caps` and
  `created_by` ARE real. One hop only: your grandparent is not listed, and
  `logs` against your parent still returns not-found.
- **Check your own caps with `harness-cli whoami`.** It prints THIS connection's
  own principal task id and the exact capability set the server enforces for you
  (`--json` for the machine-readable form). No cap is required — it is
  self-introspection, not a peek at another task's record. Use it when unsure
  whether a denied RPC is a missing cap vs. a real error.

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

