# agent-harness

Parallel Claude Code CLI harness — task dispatcher with multi-agent
messaging.

A system for running multiple Claude Code instances in parallel against
one or more git repos. Submit tasks from a CLI / TUI / WebUI, attach
interactively to running agents, and let agents talk to each other over
a per-task broker.

> **⚠️ Toy / dogfood scope.** This is a single-developer personal tool,
> published as-is. It is **not** production-hardened. In particular the
> custom transport stack (`objproto` ECDH + AES-GCM, the `trsf` stream
> multiplexer, PSK pre-auth, congestion control — maintained in the
> companion module
> [objtrsf](https://github.com/on-keyday/objtrsf)) is deliberately
> toy-scope: it exists to learn and to dogfood, not to be a vetted
> security boundary. The server is a **trusted hub** that handles task
> logs, file contents, PTY streams and port-forward bytes in plaintext;
> features such as port forwarding dial arbitrary `host:port` from the
> runner with no sandboxing. Run it only on networks and hosts you
> control. No stability, security, or support guarantees.

The original design lives in
`docs/superpowers/specs/2026-04-25-parallel-agent-harness-design.md`;
follow-up specs covering TUI, multi-task scheduling, agent-to-agent
messaging, WASM transport, PSK auth, etc. are alongside it under
`docs/superpowers/specs/`.

## Architecture

```
┌──────────────────────┐
│ harness-cli   (CLI)  │─┐
│ harness-tui   (TUI)  │─┤       ┌──────────────────────┐         ┌────────────────────┐
│ harness-webui (WASM) │─┴──WS──▶│ harness-server       │◀── WS ──│ agent-runner       │ × N
└──────────────────────┘         │  Registry            │         │  worktree mgr      │
                                 │  TaskStore (WAL)     │         │  claude exec (× M) │
                                 │  Scheduler           │         │  per-task PTY for  │
                                 │  pubsub  (task log/  │         │   `interactive`    │
                                 │           status)    │         └────────────────────┘
                                 │  agentboard (agent ↔ │
                                 │              agent)  │
                                 │  LogStore            │
                                 └──────────────────────┘
```

- **Server** (`cmd/harness-server`): hub. Listens on WebSocket (and
  optionally UDP via `--udp-listen`); accepts connections from clients
  (CLI / TUI / WebUI WASM) and runners. Queues tasks, dispatches them
  to idle runners by repo affinity, persists task state via JSONL WAL,
  appends per-task logs to `<data-dir>/logs/<task-id>.log`. Hosts two
  distinct brokers: `pubsub` for task-log and runner/task-status
  fanout, and `agentboard` for agent-to-agent messaging keyed by
  `(runner_id, task_id)`. Buffers per-session scrollback for detached
  interactive tasks so `session attach` can replay context on
  reconnect.
- **Runner** (`cmd/agent-runner`): worker. Started with a list of repo
  roots (`--roots`) it is allowed to serve and a per-process concurrency
  cap (`--max-tasks`). For each assigned task, creates a `git worktree`
  under `<repo>/.harness-worktrees/<task-id>/`, runs `claude` (or PTY
  for `interactive` / `session new`) in it, streams stdout/stderr
  through the server, reports the exit code. Injects `HARNESS_*` env
  vars into the agent subprocess so the agent can reach back via
  `harness-cli agent ...`.
- **Clients**:
  - `cmd/harness-cli` — request/control surface:
    - Task lifecycle: `submit`, `ls`, `logs`, `cancel`, `prune`,
      `prune-local`, `watch`.
    - Interactive: `session {new,attach,ls,kill}`, `interactive`
      (semantics in Quick start 5).
    - File transfer: `file ls`, `file push`, `file pull`, `file delete`
      against a task's worktree (recursive variants via `-r`, force
      overwrite via `-f`; paths are confined to the worktree root).
    - Port forwarding: `forward <task-id> -L [bind:]lport:rhost:rport`
      (SSH `-L` style — the runner dials `rhost:rport`, bytes relayed
      over the same transport; `-L` repeatable, foreground until Ctrl-C).
    - Commands in a worktree: `exec <task-id> -- <cmd> [args...]` runs one
      command in the task's worktree as its own process — separate stdout and
      stderr, and the command's exit code becomes yours. `exec ls` / `exec
      kill <exec-id>` list and stop the running ones. See **Running a command
      in a task's worktree** below.
    - SSH front door: `ssh-gateway [--listen 127.0.0.1:2222]` serves ssh,
      so `ssh -p 2222 <32-hex-task-id>@127.0.0.1` attaches to that session
      from any ssh client — an `~/.ssh/config` alias, tmux, mosh, a script
      — with no harness binary on that host. See **SSH gateway** below.
    - Agent runtime (called from inside agent sessions):
      `agent {send | wait | inbox | dispatch | subscribe | unsubscribe
      | topics | subscriptions}`. See `runner/agentskills/harness-cli/
      SKILL.md` for conventions.
    - Workspace config: `workspace {save,ls,show}` reads and writes
      `.harness/config`, and the global `--config` / `--workspace` add a
      third resolution tier below flag and env for `server-cid` /
      `ws-path` / `repo`. See **Workspace config** below.
    - Capabilities: `submit` / `session new` / `interactive` take
      `--caps NAMES` to grant a spawned task a restricted capability set
      (`caps_child = caps_parent ∩ requested`, server-enforced) and
      `--scope SPEC` to bound WHICH tasks those capabilities may target
      (omitted `--caps` = **none**: authority is granted, never
      inherited); `caps` lists the grantable names and scope forms, and
      `caps set` re-grants both on a live task. See **Capabilities and
      scope** below.
  - `cmd/harness-tui`: Bubble Tea interactive frontend (sections below).
  - `cmd/harness-webui-wasm`: in-browser WebUI compiled to WASM, served
    by `harness-server`.

Connections use the `objproto` secure transport (ECDH +
AES-128-GCM) from the companion module
[objtrsf](https://github.com/on-keyday/objtrsf) on top of one of two
underlays — **WebSocket** (default, `--listen host:port` on the server)
and **UDP** (`--udp-listen host:port`, which uses objtrsf's own
QUIC-like layering in `trsf`). Both can run simultaneously
(WS+UDP dualstack) on a single server. The `trsf`
stream-multiplexing layer carries control / data frames on top of
either. PSK pre-authentication gates incoming connections before the
secure session starts. The server takes the PSK via `--psk` (or env
`HARNESS_PSK`) or `--psk-file` — the file is the PSK *origin*:
auto-generated on first run if absent, then persisted there. Clients
and runners *consume* an already-established PSK via `--psk` / `--psk-file`
or env `HARNESS_PSK` / `HARNESS_PSK_FILE` (env `HARNESS_PSK_FILE` is
read-only — it is not honored by the server, which only generates to the
`--psk-file` flag path). The PSK handshake is 1-RTT and carries the
connecting party's identity (role + principal) in the same message, so
the server knows whether a connection is a runner, an in-task agent, or
an operator surface before the secure session opens. Operator surfaces
(CLI / TUI / WebUI) prove a **separate** operator secret via
`harness-server --operator-psk` (env `HARNESS_OPERATOR_PSK` /
`HARNESS_OPERATOR_PSK_FILE`), which is deliberately never injected into
agents — without it an in-task agent could drop its capability ticket,
reconnect as a plain client, and escalate to operator. Leaving
`--operator-psk` empty keeps the legacy behaviour (operator surfaces
validated against `--psk`) with a startup warning. Server and runner can
run on different hosts — the `--server-cid` / `HARNESS_SERVER_CID` is a
ConnectionID (`ws:host:port-id` or `udp:host:port-id`) that the
runner / clients dial; the transport prefix selects the underlay.

## Quick start

Run each command in its own terminal. `make build` produces all five
binaries under `bin/`; the examples below assume that.

```bash
# 1. Start the server. --listen accepts host:port (use :8539 to bind all
# interfaces; defaults to 127.0.0.1:8539 / loopback). PSK file is auto-generated on first
# run if --psk-file is unset. The WebUI is mounted on the same HTTP listener,
# so http://<server-host>:8539/ in a browser gives you the WASM frontend.
bin/harness-server --listen :8539 --data-dir ./harness-data
# Optional: add UDP underlay alongside WS (or use UDP only by leaving --listen
# empty — but UDP-only disables the WebUI).
# bin/harness-server --listen :8539 --udp-listen :8540 --data-dir ./harness-data

# 2. Start a runner. --roots is a comma-separated list of repo paths this
# runner is allowed to serve (matched verbatim against submit --repo).
# --max-tasks N lets one runner process handle N concurrent tasks.
bin/agent-runner --server-cid 'ws:HOSTNAME:8539-*' \
                 --roots /abs/path/to/repo,/abs/path/to/other-repo \
                 --max-tasks 4
# Non-Claude agents can be wired with argv templates, for example:
#   --agent-bin codex
#   --agent-oneshot-argv 'exec --json {args} {prompt}'
#   --agent-resume-oneshot-argv 'exec --json resume --last {args} {prompt}'
#   --agent-resume-interactive-argv 'resume --last {args}'
#   --agent-log-format codex-jsonl
# (--json + --agent-log-format make the runner render codex's event stream as
#  progress lines in the task log; omit both to get its raw output verbatim.)

# 3. Submit a task. --repo is required (or set HARNESS_REPO_PATH); it must
# match a runner's --roots entry verbatim (no client-side normalisation).
bin/harness-cli --server-cid 'ws:HOSTNAME:8539-*' \
                submit --repo /abs/path/to/repo --task "test task"
# → prints task ID

# 4. Inspect / control
bin/harness-cli ls
bin/harness-cli ls --tree               # order tasks under whoever spawned them
bin/harness-cli logs <task-id>          # stream the task's log
bin/harness-cli watch                   # stream task / runner status events
bin/harness-cli cancel <task-id>
bin/harness-cli prune --before 168h     # forget terminal tasks older than 7d
bin/harness-cli prune-local --before 168h   # remove old local worktrees

# 5. Open a detachable session: a PTY claude spliced to your terminal that
# SURVIVES client disconnect (Detached) — reattach from any client via
# `session attach <id>`. `-d` spawns without splicing the local terminal.
# `interactive` is shorthand: session new + attach in one step.
bin/harness-cli session new --repo /abs/path/to/repo
# --rows/--cols size the session's PTY at open. Only worth it with -d: a
# detached session has NO size until someone attaches, and only a CONTROL
# attach carries size authority, which needs exec_control — the strongest of
# the three attach caps and one the spawner may not hold. Without them a
# full-screen TUI in that session draws nothing.
bin/harness-cli session new -d --repo /abs/path/to/repo --rows 40 --cols 150
bin/harness-cli session resize --size 40x150 <task-id>   # size a live session
                                                 # without taking it over
                                                 # (needs exec_resize; only
                                                 #  while no control attach)
bin/harness-cli session ls                       # interactive sessions
bin/harness-cli session attach <task-id>
bin/harness-cli session kill   <task-id>
bin/harness-cli interactive --repo /abs/path/to/repo

# 5b. Event-stream sessions (TaskKind stream): same lifecycle as a PTY session,
# but the data plane is structured agent events (NDJSON from the profile's
# stream adapter, --agent-stream-adapter) instead of terminal bytes. The
# events also render into the task log, so `logs` / the TUI logs pane / the
# WebUI log view follow one without attaching. The attach response carries the
# task's kind, so a PTY verb pointed at a stream task refuses and names the
# right one (and vice versa); `session resize` / `session exec` refuse (no
# PTY, no shell), `session send` stays the raw low-level escape hatch for both
# kinds. `session stream requests` / `snapshot` are specified in the
# event-stream design spec and not built yet; until they are,
# `session snapshot --raw` reads this kind's stream verbatim, which is where a
# pending approval's tool input can be read whole (the task log renders a
# request as a one-liner and truncates a tool's arguments at 200 bytes).
# Only a profile that NAMES an adapter serves this kind; the rest refuse the
# task rather than handing it a PTY. `scripts/runner.sh up --agents claude`
# (also sandbox-claude) supplies bin/harness-stream-adapter, which `make build`
# writes; a runner configured by hand passes --agent-stream-adapter itself.
# The adapter speaks claude's protocol specifically, so pointing that flag at
# another agent's binary is not how that agent gains the kind.
bin/harness-cli session new --stream --repo /abs/path/to/repo   # open + follow
bin/harness-cli session new --stream -d --repo /abs/path/to/repo # open detached
bin/harness-cli session stream attach <task-id>  # follow events (read-only,
                                                 #  Ctrl+C detaches)
# Driving one. Each verb writes ONE line of the adapter protocol and appends the
# newline that frames it; `session send` stays the raw route and appends nothing.
bin/harness-cli session stream turn <task-id> what changed in this repo?
bin/harness-cli session stream approve <task-id> <request-id> --allow
bin/harness-cli session stream approve <task-id> <request-id> --deny \
    --message "use the Makefile instead"      # the reason reaches the AGENT
bin/harness-cli session stream interrupt <task-id>   # abandon the running TURN
bin/harness-cli session stream finish    <task-id>   # close its stdin: the turn
                                                 #  completes and the agent
                                                 #  exits 0 (not a kill)
# In the TUI, `r` on a live stream task opens a chat screen that does all of the
# above with keys: enter sends, a/d answer a pending approval (with the tool's
# input shown whole), ctrl+x interrupts, ctrl+d finishes, esc leaves. Whether an
# approval is ever RAISED depends on the agent's own permission mode — a
# ~/.claude/settings.json with defaultMode "auto" pre-approves ordinary tools, so
# pass `--agent-arg --permission-mode --agent-arg default` to a session you want
# the gate armed on.
# Running vs Detached tracks ONLY the control attach (the sole writer). A
# read-only viewer or an input-forwarding cowriter — the WebUI preview, a TUI
# grid pane — takes no writer slot, so a session several people are watching
# still reads Detached. `ls` therefore shows `cowrite=N viewer=N` on every row
# that has a session, zeros included (`viewers` / `cowriters` in `ls --json`
# and `session ls`); the TUI `d` popup spells it out as
# `attached: no control, 1 cowrite, 2 viewer`. Detached ≠ abandoned.

# 6. File transfer against a running task's worktree (paths are confined
# to the worktree root; `..` escapes are rejected).
bin/harness-cli file ls     <task-id> [subdir]
bin/harness-cli file push   <task-id> ./local.txt rel/path.txt
bin/harness-cli file pull   <task-id> rel/path.txt ./local.txt
bin/harness-cli file delete <task-id> rel/path.txt
# Recursive directory transfer (tar over the wire) and force overwrite:
bin/harness-cli file push -r -f <task-id> ./local-dir/ rel/dir
bin/harness-cli file pull -r -f <task-id> rel/dir ./local-dir/

# 6b. Read a task's git state without touching its shell — the way to review
# what an agent changed while it is still running. Read-only (no commit / add /
# checkout); requires the file_read capability. Revisions are counted the way
# git counts them: none = the unstaged change, one = that revision against the
# working tree, two = commit against commit.
bin/harness-cli git <task-id> log    [--max N] [-- <path>]
bin/harness-cli git <task-id> diff   [--staged] [<base>] [<target>] [-- <path>]
bin/harness-cli git <task-id> show   [<rev>] [-- <path>]
bin/harness-cli git <task-id> status [-- <path>]
bin/harness-cli git <task-id> subrepos
bin/harness-cli git <task-id> file [--staged | --rev REV] <path>
# An agent that has committed shows nothing under a plain `diff` — that is git,
# not a gap. Read `log`, then name a baseline: `git <task-id> diff <sha>` shows
# everything since it, committed or not. Untracked files appear in no diff;
# `status` is where a brand-new file shows up, as a `??` entry.
#
# While the task lives the query runs in its worktree. After it ends the
# worktree is gone and the query runs against the retained harness/<task-id>
# branch: committed work only, and `status` is empty because no working tree is
# left to be dirty. The runner that ran the task must still be online.
#
# A repository INSIDE the repository is invisible to all of the above: a plain
# nested repo (its own .git, not a submodule) collapses to one `?? nested/`
# status entry and appears in no diff, because it is untracked. `subrepos`
# lists them and `--subrepo <dir>` runs any of the four inside one — its own
# history, its own baseline. `--subrepo` chooses WHICH repository; `-- <path>`
# still only filters within it. Submodules are listed too, so the same route
# browses a submodule's history; `--submodule` on diff/show instead inlines a
# submodule's changes into one combined diff (off by default: that output is
# not an applyable patch).
#
# `file` shows one file WHOLE rather than the lines that changed, from the
# side you name (working tree by default). The same view is on `G` in the
# TUI (keys in its `?` help) and the Git tab in the WebUI.

# 7. Port-forward a runner-side port to your machine (SSH -L style). The runner
# dials remote-host:remote-port; bytes relay over the harness transport. Handy
# for reaching a dev server the agent started inside its worktree. Foreground;
# Ctrl-C tears down. bind defaults to 127.0.0.1; -L is repeatable.
bin/harness-cli forward <task-id> -L 3000:127.0.0.1:3000

# 8. Notifications. Push a short status ping — from inside a task or by hand —
# that shows in the TUI / WebUI notification feed and `notify-watch`. Fire-and-
# forget; --level is info|warn|error, --title is optional. Origin (task / runner
# / repo / host) is auto-filled from HARNESS_* when run inside a worker.
bin/harness-cli notify --level warn "need a decision on approach X"
bin/harness-cli notify-watch          # stream notifications (ring backlog + live)

# External delivery: give the SERVER a notify-hook command line. The server
# runs it per notification (stdin: JSON event; env: HARNESS_NOTIFY_*); it
# relays the event onward (phone, chat, …) — no live client needed. The command
# is whitespace-split into executable + args. Resolution order: --notify-hook
# flag → HARNESS_NOTIFY_HOOK env → first non-# line of <data-dir>/notify-hook.
# Flag/env are forgotten on a restart from a clean shell — write the file once
# instead and every future restart keeps the hook:
#   echo '/abs/examples/notify-hooks/discord.py --url-file /secret/discord-url' \
#     > harness-data/notify-hook
# examples/notify-hooks/discord.py is a ready Discord-webhook hook (URL from
# --url / --url-file args or DISCORD_WEBHOOK_URL / _FILE env).
```

### Running a command in a task's worktree

`exec` runs one command in a task's worktree as **its own process**. It is not
the session's shell: nothing is typed into the agent's terminal, nothing appears
in anyone's scrollback, and the two output streams stay apart.

```bash
bin/harness-cli exec <task-id> -- git status          # words after -- are the argv
bin/harness-cli exec --shell <task-id> -- 'ls | wc -l'   # one line for the RUNNER's shell
bin/harness-cli exec <task-id> -- make test           # exits with make's status
bin/harness-cli exec <task-id> -- sh -c 'echo out; echo err 1>&2' 2>/dev/null
bin/harness-cli exec --shell --detach <task-id> -- 'start-server &'  # survives the exec
bin/harness-cli exec ls [-task <task-id>] [--json]    # what is running right now
bin/harness-cli exec kill <exec-id>                   # stop one
```

What it gives you that `session exec` does not:

- **Separate stdout and stderr.** `session exec` types a line into the session's
  foreground shell, so its output comes back merged on the one PTY stream. Here
  the two are distinct all the way to your terminal, which is what makes
  `2>/dev/null` and `> out.txt` mean what they usually mean.
- **The command's own exit code.** `exec` exits with it, so `&&` and `if` work.
  A command that never started (missing binary, no worktree, killed) exits
  **125** with the reason on stderr — 125 rather than 127, which is a shell's
  convention where there is no shell.
- **No side effect on the session.** The agent's terminal is untouched, so this
  is safe to run against a session someone is watching.
- **Stopping it stops what it started.** `exec kill`, Ctrl-C, or a dropped ssh
  session ends the whole process tree, not just the first process — unless you
  ask for the opposite with `--detach`, below.

Words after `--` are an argv and reach the child untouched — no shell re-reads
a quote. `--shell` is the other mode: the words become ONE line for the
**runner's** shell (`sh -c` on unix, `cmd /c` on Windows), which is what makes a
pipe or a redirect mean anything. The runner chooses, because a caller cannot
know what the far side has — `harness-cli ls` shows each runner's `os=`.

The target is the **worktree**, not the task's status: a task that ended with
uncommitted work keeps its tree — `git status` and `make test` in it are exactly
what you want then — while a task that ended clean had its tree removed and is
refused. On a `--no-worktree` runner every task runs in the repo itself, and so
does its exec.

It is **synchronous and dies with its caller**: Ctrl-C stops the child, and so
does the whole-tree kill above. `--detach` lifts only the second half — the call
still blocks until the command returns, but whatever the command left running
stays running. That is for a command whose POINT is to leave a server behind,
and without it such a command is impossible rather than awkward: on Windows the
group is a kill-on-close job, so even a deliberately detached child dies the
instant the exec returns. Nothing reaps what is left, so a process with no
shutdown path of its own is a leak. For a whole background *job* the answer is
still a task — `submit --agent bash --task '…'`.

`--sshd-parent` is the neighbouring option and needs `--shell`: it runs the line
under a process **named** `sshd`, for a client that decides whether it is
talking to an SSH server by walking its own ancestry and comparing process
names. Windows only — elsewhere the runner refuses rather than running the
command without the property and reporting success. A long one is
visible to every client in `exec ls` and stoppable from any of them, and each
task's row shows how many are running: `execs=N` in `ls` and the WebUI, `Nx` in
the TUI Obs column, `execs: N running` in the TUI `d` popup, `exec_count` in
`ls --json`.

Available from all three surfaces and over ssh: the TUI cmdline takes the same
`exec` / `exec ls` / `exec kill` words, the WebUI has them on its command line
and as 「⌨ コマンド実行」 on the task sheet, and `ssh <task-id>@host <command>`
maps to it (see **SSH gateway**).

### SSH gateway

`harness-cli ssh-gateway` (or `ssh-gateway start` in the TUI) serves the SSH
protocol from an ordinary harness client, so an `ssh` client can reach an
interactive session. Nothing changes on the server: it is the existing attach,
with an ssh channel where a local terminal would be.

```
ssh -p 2222 <32-hex-task-id>@127.0.0.1           # cowrite: type, evict nobody
ssh -p 2222 <32-hex-task-id>.control@127.0.0.1   # take the seat (owns the PTY size)
ssh -p 2222 <32-hex-task-id>.view@127.0.0.1      # watch only
ssh -p 2222 <32-hex-task-id>.detach@127.0.0.1    # execs leave what they start running
ssh -p 2222 <32-hex-task-id>.detach,sshd-parent@127.0.0.1   # …and run under a parent named sshd
```

The user name names the task and picks the mode. The **bare form is cowrite**
so that arriving over ssh never detaches whatever you already had attached in
the TUI; `.control` asks for that takeover explicitly, and is the form that
owns the PTY size. A cowrite session's own resize only takes effect while no
control client holds the seat — otherwise it renders at the controller's size.

`Ctrl+]` detaches, the same gesture the CLI and TUI use, and leaves the session
running. ssh's own `~.` is a *disconnect*, not a detach: the session survives
that too, but the gateway is already gone by the time it happens and cannot
reset your terminal's modes on the way out — `reset` fixes a terminal left on
an alternate screen or emitting mouse reports.

**Authentication follows the bind address.** On loopback there is none: an
agent this harness starts runs as your UID and could read your ssh private key
anyway, so keys would buy nothing. Off loopback that reasoning inverts, so
`--listen` on any other address requires `--authorized-keys FILE` and refuses
to start without it. The host key is generated once beside `.harness/config`
and reused, so `known_hosts` stays valid.

**A gateway is only as alive as the harness connection under it**, the same rule
a forward follows. A server restart or a dropped link ends it: the listener
closes, the ssh connections it accepted are dropped, and `harness-cli
ssh-gateway` exits non-zero instead of holding the port with a dead client
behind it. The TUI reconnects on its own; its gateway does not come back with
the connection, so `ssh-gateway status` will say it is not running and
`ssh-gateway start` is what re-binds it.

**`ssh -L` and `ssh -W` tunnel through it.** The ssh client keeps its own
listener and opens a `direct-tcpip` channel per connection; the **runner** dials
the target, over the same data plane `forward -W` uses. Each forwarded
connection is therefore an ordinary `forward ls` row while it lasts, stoppable
from any client — which is the only place it can be seen, since the listener
itself lives in the ssh client and the harness never learns of it.

```bash
ssh -p 2222 -N -L 3000:127.0.0.1:3000 <task-id>@127.0.0.1   # runner-side :3000, locally
ssh -J <task-id>@127.0.0.1:2222 user@runner-host            # jump through it
```

This too was refused for one release, on the grounds that `forward` already did
it and a second path would drift. It is not a second path — it is the same
`OpenRawForward` behind a door an ssh client can find, which is what the refusal
actually cost: every tool that does its own forwarding instead of calling a
harness binary.

Still not served: **scp / sftp** (`file push` / `file pull` cover that ground,
and the subsystem refusal says so) and **`ssh -R`** (`forward -R`). `-R` is a
global request, and SSH gives a global request no field to put a reason in, so
that one fails without a sentence attached.

`ssh <task>@host <command>` runs the command in the task's **worktree**, through
the same path as `harness-cli exec` (see **Running a command in a task's
worktree**): its own process, stdout and stderr kept apart, the command's exit
code returned as ssh's, and the session it belongs to never touched.

```bash
ssh -p 2222 <task-id>@127.0.0.1 'make test'    # exits with make's status
ssh -p 2222 <task-id>@127.0.0.1 'git status' > out.txt 2> err.txt
```

This was refused for one release, and the reason it stopped being refused is
worth stating: the only command surface then was `session exec`, which types
into the session's foreground shell — it needs a POSIX shell to be there, merges
stdout and stderr, and shows up in the scrollback of whoever is watching. `ssh
host cmd` promises none of that, so mapping to it would have been a lie. `exec`
is the construct with matching semantics, so the mapping is now honest.

The command line reaches a shell intact, so quoting and redirection work — and
it is the **runner's** shell, chosen from the runner's own platform (`sh -c` or
`cmd /c`). The gateway does not pick: it is handed an opaque string by the ssh
client and has no way to know what the far side has.

The `.control` / `.view` suffix does not gate it — those choose how a *shell*
session attaches, and this never attaches — and an exec does not hold the
control seat while it runs. Interrupting the ssh client stops the command,
including anything it forked.

The suffix does, however, **configure** an exec. It is a comma-separated list,
and alongside at most one attach mode it takes `detach` and `sshd-parent` —
the two `exec` options above, applied to every command this connection runs.
They live in the user name because that is the only thing that reaches the
gateway from a client building its own ssh invocation: a `~/.ssh/config` `User`
line is written once and covers every exec the connection makes, which is the
right granularity for a property of what the connection is *for*. Order does
not matter and the modes compose, so `.control,detach` is a valid name.

### X11 forwarding

`harness-cli session new --x11 --repo <path>` injects `DISPLAY`/`XAUTHORITY`
into the session so GUI programs render on your local X server (SSH `-Y`
equivalent; trusted forwarding). Requires `xauth` on both the client and the
runner, a Linux runner (or a runner with X11 client libraries), and a running
local X server (Linux with `$DISPLAY`, or Windows/macOS with VcXsrv/XQuartz
exported as `$DISPLAY`). Override the display number with `--x11-display N`
(default 10). Not available with `--detach` or for the WebUI client.

### Daemon lifecycle helpers

Run the server and runner as **detached background daemons** instead of
the foreground invocations shown in Quick start. Any args after
`up` are forwarded verbatim to the underlying binary.

```bash
# Start (build first: `make build`). Same flags as the Quick start binaries.
scripts/server.sh up --listen :8539 --data-dir ./harness-data
scripts/runner.sh up --server-cid 'ws:HOSTNAME:8539-*' --roots /abs/repo --max-tasks 4

# Stop
scripts/server.sh down
scripts/runner.sh down

# Restart in place — reuses the running daemon's flags / CWD (read via psutil).
# <slot> is the pid-file name: the binary name, or <binary>-<tag> when tagged.
scripts/restart.sh agent-runner

# Run several instances of one daemon side by side with --as <tag>
# (each gets its own bin/.run/<binary>-<tag>.{pid,log} slot):
scripts/runner.sh up --as 2 --server-cid 'ws:HOSTNAME:8539-*' --roots /abs/repo --max-tasks 2
scripts/runner.sh down    --as 2
scripts/restart.sh agent-runner-2
```

Implementation notes: the `.sh` entry points are thin shims over the
canonical cross-platform Python (`scripts/{runner,server,restart}.py`);
`bootstrap.py` provisions `scripts/.venv` (psutil) on first call. pid /
log state lives in `bin/.run/<slot>.{pid,log}` and is shared between the
bash and python entry points, so a daemon started via one can be stopped
via the other.

For boot/login persistence, `scripts/runner-autostart.py register
--tag <tag> [runner.py flags...]` registers an OS-level autostart
entry — a systemd user service on Linux
(`~/.config/systemd/user/harness-agent-runner[-<tag>].service`,
`Type=oneshot` + `RemainAfterExit=yes`), or a Task Scheduler task
on Windows (AtLogOn trigger, `RestartCount=3 RestartInterval=PT5M`).
The action calls `runner.py up`, so the runner's actual lifecycle
is still owned by `daemon.py` and the pid/log invariants are
unchanged. Symmetric `unregister` removes the entry and stops the
daemon; `--no-start` / `--no-stop` opt out of the immediate
spawn / shutdown.

## Operating modes

By default the runner creates a `git worktree` per task under
`<repo>/.harness-worktrees/<task-id>/` and runs the agent in that
isolated checkout. Two flags adjust this:

- `--no-worktree`: skip worktree creation and run each task directly
  in the bound repo path (the request's `--repo`, which must match
  `--roots`). Intended for generic-process workloads (e.g.
  `--agent-bin bash`). Disables `.claude/settings.json` and
  `.claude/skills/` injection by default — agentboard hooks are not
  auto-installed in this mode. The user's repo is left untouched on
  task end (no `git worktree remove` is ever called). `HARNESS_*`
  environment variables are still injected into every spawned process.

- `--force-inject-harness-settings`: only meaningful with
  `--no-worktree`. Re-enables `.claude/settings.json` and
  `.claude/skills/` injection at the bound repo path, so agentboard
  hooks fire even without a per-task worktree. The injected files
  persist after task end (no auto-cleanup); manage them manually if
  desired.

However these two resolve, the runner declares the outcome
(`!--no-worktree || --force-inject-harness-settings`) in its
`RunnerHello`. The server stamps that bit onto **every task it assigns**
to the runner, so it is visible per task as the `+skills` suffix on the
`agent=` column in `ls`, the TUI Agent column and its `d` detail popup,
and the WebUI task rows — plus a per-task `skills_injected` bool in
`ls --json` / `session ls`. It rides on the task rather than being
joined from the RUNNERS section because a caller whose visibility rank
is below `global` is served no runners at all, and a confined agent judging whether a peer
follows the agentboard conventions is the main reader. Two limits worth
knowing: it is a *declaration* of how the runner is configured (the
injection write itself is warn-only), and a task with no runner yet
carries no marker — absent is "unknown", not "bare agent".

### Where the injected skills come from

By default the skills written into `.claude/skills/` and
`.agents/skills/` are the copy embedded in the runner binary
(`runner/agentskills`, `//go:embed`). That copy is frozen when the
process starts: `make build` replaces `bin/agent-runner` on disk, but a
running runner keeps the binary it already loaded, so an edited
`SKILL.md` does not reach new tasks until the runner is restarted.

`--agentskills-dir DIR` (env `HARNESS_AGENTSKILLS_DIR`) points the
runner at a directory on disk instead. It is read on **every task
assign**, so an edited `SKILL.md` reaches the next task with no rebuild
and no restart — the same trade the server's `--webui-dir` makes for
WebUI assets:

```bash
scripts/runner.sh up --server-cid 'ws:HOSTNAME:8539-*' --roots /abs/repo \
  --agentskills-dir /abs/path/to/agent-harness/runner/agentskills
```

- A subdirectory counts as a skill only if it contains a `SKILL.md`.
  That filter is why the checkout's `runner/agentskills` is a safe
  target even though it also holds `embed.go` and the package's tests.
- The directory **replaces** the embedded set rather than layering over
  it, and a path that resolves to zero skills fails at startup — skill
  injection is otherwise a warn-only step in the task path, so a typo
  would silently give every task no skills at all.
- The pointed-at directory is read as it is on disk, committed or not.
  Uncommitted edits there are handed to every task the runner starts,
  which is the point during development and worth remembering
  afterwards.
- `harness-cli skill <name>` is unaffected: it prints the copy embedded
  in the `harness-cli` binary, which `make build` refreshes without any
  restart.

## Capabilities and scope

Each task carries a **capability set** — a server-enforced bitmask of
what control-plane operations it may request (spawn, cancel, the three
session-attach powers, file read / write, local / remote port-forward,
notify, prune, purge, runner-admin, global info) — and a **target scope**
bounding WHICH tasks those capabilities may be pointed at.

Attaching to a session is three capabilities, not one, because the three
attach modes hand over three different powers. They are checked with
implication (each accepts itself or anything stronger), so a grant of the
stronger one never needs the weaker one alongside it:

| cap | what it allows |
| --- | --- |
| `exec_view` | observe an agent's output, live or recorded — `session snapshot`, TUI grid panes, `session attach --view`, `session stream attach`, `logs` |
| `exec_cowrite` | additionally type into a session someone else is driving, without evicting them — `session send`, `session exec`, the WebUI preview |
| `exec_control` | additionally take the session over as sole writer, evicting whoever holds it, and own its size — `session attach`, TUI `r` |

`exec_resize` sits beside those three rather than inside them: resizing is
availability (a wrong size makes a full-screen program refuse to draw) while
typing is integrity (it runs commands as that session), so "may drive this
worker but must not resize it" and "may make it readable but must not type into
it" are both grantable, and neither implies the other. It is honoured only
while **no control client is attached** — the size belongs to the control seat
whenever someone holds it, and this lets an observer stand in for an unattended
session rather than fight the human whose terminal defines the size. A control
attach needs no such bit; owning the size is what control means.

`exec_control` is the former `exec_attach` under a name that says which of
the three it is; it keeps the same bit, so every task already holding it
kept exactly the power it had. Watching a worker no longer implies being
able to type into it or kick the operator off it.
`harness-cli caps` lists the grantable names and the scope forms with a
one-line description of each; `caps --json` emits the machine-readable
catalog.

By default a spawned task gets **no capabilities at all**, scoped to
**subtree** (itself + everything it spawns). An omitted `--caps` grants
nothing — the task can still use the agentboard and read its own
subtree's logs / `ls`, but every control-plane verb (spawn, cancel,
attach, file read/write, forwards, notify, prune, purge, runner-admin,
global info) answers `permission denied` until something grants it.
`--caps NAMES` and `--scope SPEC` (on `submit` / `session new` /
`interactive`, also in the TUI and WebUI spawn surfaces) widen or
redirect that within what the spawner itself holds: the server grants
`caps_child = caps_parent ∩ requested` and clamps the scope to the
spawner's own reach, so a task can never exceed its parent. A supervisor
that spawns and drives workers therefore has to say so —
`--caps spawn,exec_cowrite,notify` — and anything missed afterwards is
`caps set <id> --caps …` on the live task, no restart. `NAMES`
is comma-separated (e.g. `spawn,file_read`, subtractive `all,-spawn`);
`SPEC` is `[<visibility>/]<action>` where each rank is
`subtree | none | global`, the action side also accepting `descendants`
(the subtree without self) or `<base>-self`, plus
`[+ids:<task-id>[,…]]` and `[+vis-ids:<task-id>[,…]]`. `ids:` naming
specific tasks is the everyday form ("this worker may touch exactly
that sibling"); `vis-ids:` is the view-only version of it ("watch that
sibling, touch nothing"). Out-of-scope targets answer *no such task*.

A **visibility rank wider than the action rank** is how a task sees the
whole server while acting on its own subtree — `--scope global/subtree`.

The reverse is allowed too, and deliberately. An earlier version required
`action <= visibility` so that `ls` would be a complete statement of what
a task can reach; it was dropped because it conflated two things this
design keeps apart — *un-enumerable* and *invisible* — and because it
forbade a shape that is actually wanted: an agent that acts only on ids
handed to it over the agentboard and must NOT be able to list the server.
That is `--scope none --scope-for exec_view=global`: cannot enumerate
anything, can look at what it is told about. Under the old rule the only
way to get it was a global visibility rank, which grants exactly the
enumeration the operator was withholding. Task ids are 128 random bits,
so a wide action rank buys no discovery on its own —
`server/scope.go`'s `validateScope` carries the full reasoning, and it
compares the two ranks nowhere.

Both halves are editable from every surface: `--scope` on the CLI and the
TUI cmdline take the whole grammar, and the **WebUI spawn form / re-grant
dialog** and the **TUI `a` picker** each carry a visibility rank row
(`base に従う` / `subtree` / `none` / `global`, the first meaning "not
stated — follows the action rank") beside the action one, plus a second
task checklist for `+vis-ids:`. In the TUI picker that second set is the
`v` key on a task row, which is why its rows show two boxes.

`--scope-for CAPS=SCOPE` narrows ONE capability, or a comma-separated
list of them, below the task's own scope — `--scope-for
exec_cowrite,file_write=descendants` is "may drive its workers, may not
drive itself". Repeatable; the capability lists must not overlap. It
cannot carry a visibility half, which belongs to the task rather than
to a verb, and it never GRANTS a verb: an override for a bit the task
does not hold sits inert until `caps set` grants it.

All of it is visible per task in `ls`, `whoami`, the TUI detail popup
and the WebUI task rows; `ls --json` and `whoami --json` additionally
carry `scope_by_cap`, the fully resolved capability→scope map, so a
machine reader never redoes the override merge.

Not everything an agent can destroy is a capability. `agent retract
<seq>` withdraws a message the caller itself published — the server
checks authorship, not a bit, because the operation cannot reach bytes
the caller did not write. A withdrawn message disappears from every
agent-facing path but stays on the operator surfaces (`board read`, the
TUI board view, the WebUI board panel) marked as retracted, so an agent
withdrawing in seconds cannot shrink the window a human has to audit it.
`purge` remains capability-gated: it erases the bytes outright,
including from that operator view.

The operator withdraws somebody else's message with `board retract
<topic> --seq N` (TUI board view: `w`; WebUI board panel: ⊘ on a
message). It is the same move without the authorship check, gated on
`purge` — no new bit, because that bit already reaches the same message
through `board purge --seq N` and destroys it outright. This is the half
that leaves the payload behind for reading: the agents lose it now, the
operator keeps it until the topic ages out. `--seq` is required and
purge's "seq 0 means the whole topic" shorthand is deliberately not
inherited, so a mistyped flag costs one message rather than a
conversation. Which check a withdrawal passed is on the row —
`RETRACTED at=… by=author` against `by=purge_cap:<task-id|operator>` —
because with two verbs able to withdraw, a bare marker would credit the
author for what somebody else did.

Usually nothing calls `retract` by hand: **replying to a message that was
addressed to you withdraws it**, since the reply is proof it was handled.
That only applies point-to-point (the message sat on the replier's own
`chat.<short-id>`) — one subscriber's answer never withdraws a shared-topic
publish the others may not have read. `agent send --no-retire-on-reply`
opts a message out.

```bash
bin/harness-cli caps                       # capability names + scope forms
bin/harness-cli submit --repo /abs/repo --task "..." --caps none
bin/harness-cli session new --repo /abs/repo --caps cancel --scope ids:<sibling>
bin/harness-cli caps set <task-id> --caps all,-spawn --scope subtree \
    [--cascade] [--keep-conns]             # operator-only LIVE re-grant
```

`caps set` rewrites a live task's authority with no restart — effective
on its next request; `--cascade` clamps its descendants too, and a
narrowing drops the affected tasks' open connections unless
`--keep-conns`. The TUI task list binds this to `a` (a selection
picker; `A`/`N` = all/none quick-set) and the WebUI task sheet to
「🔑 caps/scope 再付与」.

The **subtree** walk follows each task's parent link (who spawned whom,
`by=` in `ls`). The link itself is operator-mutable on a live task:

```bash
bin/harness-cli caps set-parent <task-id> --parent <task-id>  # move under another task
bin/harness-cli caps set-parent <task-id> --none              # detach to the root
bin/harness-cli caps set-parent <task-id> --swap              # invert with its current parent
```

`--swap` is for the mid-flight role reversal: the task takes its
parent's place (inheriting the parent's own parent, possibly the root)
and the former parent becomes its child — applied atomically, so no
half-swapped state is ever visible. Caps and scope are untouched
(combine with `caps set` to actually shift authority), no connection is
dropped, and cycles are rejected. Task ids, worktrees and conversation
history stay put — that is the point: the alternative was destroying
and recreating both tasks. The TUI binds this to `A` (root / swap /
task picker) and the WebUI task sheet to 「⇄ 親タスク変更」; both also
accept the typed `caps set-parent` / `set-parent` command forms.

On **resume**, the task's persisted authority is kept by default; the
two halves re-grant independently — `--caps` re-grants the mask,
`--scope` re-grants the scope (each only when literally given; WebUI
gates both behind the apply-on-resume checkbox). This composes with the
podman sandbox (see **Sandboxing** below) as a server-layer
confinement: `--caps none` closes the control-plane escape path (an
agent spawning an unsandboxed child) regardless of what is inside the
container.

## Sandboxing (rootless podman)

An **opt-in** kit under `scripts/sandbox/` runs a runner's spawned agent
— **claude, codex, agy, or a plain bash shell** — inside a **rootless
podman** container instead of directly on the host, to shrink the blast
radius of an agent run with `--dangerously-skip-permissions`. It needs
no harness core changes: it plugs in through the existing `--agent-bin`
seam, via presets that carry the wrapper path and each agent's argv.

```bash
scripts/sandbox/build.sh                       # build harness-agent-sandbox:latest
scripts/runner.sh up --as sandboxed \
  --agents sandbox-claude,sandbox-codex,sandbox-agy,sandbox-bash \
  --roots "$HOME/workspace/<repo>"
```

Which agent runs is `--sandbox-agent NAME`, and every per-agent
difference (host binary, config mounts, auth mode, egress domains) lives
in one table at the top of `agent-in-podman.sh`. No agent is installed
into the image — the host binary is bind-mounted over the container
path.

`--userns=keep-id` maps the host uid into the container so worktree
edits land on disk owned by the host user (podman, not docker, makes
non-root + host-owned writes coexist). The wrapper bind-mounts the repo
root (worktree + shared `.git`) and, in the default **mount auth** mode,
the agent's config paths (host login / session resume — note this
exposes that provider's credentials to the container). **Token auth** (a
dedicated revocable `CLAUDE_CODE_OAUTH_TOKEN`, no `~/.claude` mount)
removes that exposure but disables resume, and **only claude has it**:
codex and agy are mount-auth only. Egress is open by default;
`--agent-arg --firewall` applies a default-deny iptables+ipset
allowlist, and `--firewall-proxy` routes egress through an in-container
allowlisting CONNECT proxy (raw sockets blocked, WebFetch works).

This is the **OS-layer** half of a two-layer model; the **server-layer**
half is the capability bitmask (**Capabilities and scope** above), which
now closes the control-plane escape path by default — an omitted
`--caps` grants nothing, so a sandboxed task has no control plane to
escape through unless the spawn asked for one. Neither layer
configures the other. Full details, security model, and verification
status are in [`scripts/sandbox/README.md`](scripts/sandbox/README.md).

## TUI

`cmd/harness-tui` is an interactive Bubble Tea frontend that bundles
`submit / ls / logs / cancel / prune / watch / interactive / session`
into one screen.

```bash
bin/harness-tui --server-cid 'ws:HOSTNAME:8539-*' --repo /abs/path/to/repo
```

Layout:

```
┌── Runners ────────┐ ┌── Tasks ──────────────────────┐
│ Idle  /home/foo   │ │ Queued  9d50  prompt...        │
│ Busy  /home/foo   │ │ Running abcd  prompt...        │
└────────────────────┘ └────────────────────────────────┘
┌── Log: <selected task> ──────────────────────────────┐
│ [out] hello                                           │
│ [err] ...                                             │
└───────────────────────────────────────────────────────┘
┌── Last command output ───────────────────────────────┐
│ submitted: 9d508...                                   │
│ [log] 11:06AM INFO ws session started ...             │
└───────────────────────────────────────────────────────┘
> [cmdline]
tab focus · s submit · enter follow · c cancel · T tree · ? help · q quit
```

Keys for orientation — `Tab` cycles focus, `s` submit popup, `S`/`i` open
a detachable session, `r`/`R` reattach/resume the selected task, `Enter`
follows its log, `d` detail, `c` cancel, `a` re-grant caps/scope, `G`
git, `F` files, `q` quit. **The authoritative list is the TUI itself**:
the footer shows the keys that work in the focused pane and `?` shows
everything — both render from the same table the dispatcher uses
(`tui/keys.go`, test-enforced), so unlike a README table they cannot go
stale.

The cmdline accepts `submit / interactive / session {new,attach,ls,kill}
/ session stream attach / file {ls,push,pull,delete} / git / exec / forward
/ grid / caps / scope
/ caps set / caps set-parent / workspace {save,apply,ls,show}
/ server dial-runner / ssh-gateway / cancel / prune / repo / clear / help / quit`.
`exec <task-id> [--] <cmd>...` runs a command in that task's worktree and prints
its output into the cmdresult pane, stdout marked `1|` and stderr `2|`;
`exec ls` / `exec kill <exec-id>` list and stop the running ones.
`ssh-gateway [start [bind:port] | stop]` hosts the SSH front door from the TUI
itself (see **SSH gateway**); with no argument it reports the address it is
listening on, or that it is not running. It dies with the TUI, and with the
server connection — a reconnect does not bring it back, `ssh-gateway start`
does.
`session new --stream -d` opens an event-stream session (detached only in
the TUI — there is no terminal to splice); `session stream attach <id>`
follows its events in the logs pane, which is where this kind's events
render anyway. `caps NAMES`
/ `scope SPEC` set the session-default authority for subsequent spawns
(no argument opens the selection picker); per-spawn `--caps` / `--scope`
override it, and on a resume re-grant only what was literally typed.
`session new` supports
`--host NAME | --runner HEX | --ip ADDR` for runner-pinning (mutually
exclusive), plus `--detach` to spawn-and-exit without splicing the
local terminal. `grid` opens the live session viewer over a chosen set:
bare for every live session (the `g` key), `grid <id>...` for exactly
those, `grid --under <id>` for one task's working set — its subtree plus
whatever its own scope names individually (`z`) — and `--descendants` to
leave that task itself out, for when it is already on screen in another
terminal (`Z`). Use `harness-cli prune-local` for local-only worktree
cleanup; the TUI's `prune` command is server-only. slog output
(transport / pubsub / etc.) is folded into the cmdresult pane with a
`[log]` prefix so it never scribbles over the alt screen.

## Workspace config

Starting a client — and every reconnect after a drop — otherwise repeats the
same steps by hand: pass the connection flags, bring the long-lived task back if
it died, re-establish its port forwards one spec at a time, and reopen the grid.
The forwards are not optional work after a reconnect: a forward's lifetime is
the lifetime of the client connection holding its control stream, so a client
that goes away takes every forward with it.

A **workspace** is a named set of those values in `.harness/config`. Both
`harness-tui` and `harness-cli` read it; the TUI is what applies one.

```
# .harness/config   (gitignored — it holds a LAN server-cid and local paths)

[workspace default]
server-cid = ws:HOSTNAME:8539-*
repo       = /abs/path/to/repo
grid       = --under 3f2a9c…            # the `grid` command's arguments, verbatim

[workspace default task 3f2a9c…]
resume  = continue                      # no (default) | continue (r/u) | fresh (R/U)
runner  = assigned                      # assigned (r/R, default) | any (u/U)
forward = -L 3000:127.0.0.1:3000
forward = -R 8080:127.0.0.1:8080
```

The file is resolved as `--config <path>` → `HARNESS_CONFIG` →
`./.harness/config` (current directory only; the search does not walk up). An
explicitly named file that is missing is an error; a missing default is silent.
**It is not read at all when `HARNESS_AUTH_TICKET` is set** — the sandbox
wrapper forwards environment by `HARNESS_*` prefix, so an in-task agent must
never pick up an operator's workspace.

An unknown key, header or enum value is a parse error rather than a skipped
line, so a typo'd `fowardd` cannot silently establish nothing. `forward` and
`grid` values are handed to the same parsers `harness-cli forward` and the
`grid` command use, so the config can never accept a spelling the command line
rejects. There is no key for the PSK: the secret keeps its two existing homes.

Nothing here is a wire change. Both forward directions terminate in a client
process, so a declaration held server-side could never enact itself — which is
why this is a client-side file.

```bash
bin/harness-tui --workspace default          # applies on start and every reconnect
```

Inside the TUI, `workspace apply [name]` re-applies on demand, `workspace save
<name>` writes the current state back, and `workspace ls` / `show [name]`
inspect the file. From the CLI:

`workspace rm <name>` deletes one workspace — its own lines only; other
workspaces and every comment stay as they were.

```bash
bin/harness-cli workspace save <name> [--task <32-hex>] [--repo PATH]
bin/harness-cli workspace rm <name>
bin/harness-cli workspace ls
bin/harness-cli workspace show [<name>]
bin/harness-cli --workspace default ls       # server-cid / ws-path / repo only
```

The CLI has no picker — nothing to draw it on — so its `save` records every task
the registry reports a forward for, or just the one `--task` names. `--task` is
also how a task's forwards get CLEARED after you stop them: the registry reports
presence, never absence, so a save can only clear what it was pointed at.

Saving is how the file gets written — no task id or forward spec is meant to be
typed by hand. In the TUI, `workspace save <name>` opens a **picker**: every
live session, every task the workspace already declares (still listed when it is
no longer running, so dropping one is a decision rather than a side effect),
every task that could be resumed — a finished one is exactly what `resume` is
for, so those are offered unticked, most recent first — and any task that only
has a forward. Space includes or excludes a task, `r` cycles
its `resume` (no / continue / fresh), `u` its `runner` (assigned / any), `f`
edits that task's `forward` lines (comma-separated, parsed by the same parser
`harness-cli forward` uses, so the picker cannot write a spec the command line
would reject), `g` cycles the `grid` line (keep / this session's selection /
none), `a`/`n` tick all or none, Enter writes and Esc cancels.

The grid is chosen HERE rather than read from whether the grid is open, because
it cannot be: the grid is a full-screen overlay that intercepts every key, so
the command line is unreachable while it is up and the grid is always closed by
the time a save runs. Likewise `f` exists because a workspace's forwards were
otherwise real-time only — the list came from the registry, so declaring a
forward for a task that is not running meant editing the file. Which tasks belong in a
workspace is a statement, not something a rule can infer from what happens to be
running — `workspace save <name> --all` takes every live session without asking,
for when pressing Enter is the only thing the picker would do.

A save **merges**: task blocks it did not list are left alone, and an existing
block's `resume` / `runner` are never reset — those are yours. The picker starts
from what the file already says, so a second save defaults to "what I said last
time", not to whatever is running now.

Both save paths read the server-side forward registry and write every forward
whose client endpoint is an OS socket, skipping the in-process ones (a raw `t`
pane, a WebUI preview pin) with a count: those bind nothing locally, so no
`-L`/`-R` line describes them. Reading the registry means a save also captures a
forward established from a `harness-cli forward` in another terminal — and that
the next apply will then contend for that port with the process still holding
it, reported as an ordinary bind conflict.

Applying **reconciles** rather than restarts. A declared forward already running
is left alone, a missing one is started, and one no longer declared is stopped;
forwards the operator started by hand are never touched. That is what makes
`workspace apply` the recovery from a port conflict — freeing the port and
re-applying starts only the forward that failed, leaving the ones that were
working connected. A failed forward is not retried automatically and does not
fall back to another local port; it reports one line and the apply continues to
the next item.

The resume brings a task back **detached** — nothing takes over the terminal;
the grid is what restores the screen. It fires on start, on reconnect and on
`workspace apply` alike, but only for a task the snapshot shows in a terminal
state. A network blip leaves the task alive, so it cannot spawn anything there;
a server restart marks an interrupted task Failed, which is exactly the case it
exists for. One consequence worth expecting: a session ended on purpose exits 0,
which is Succeeded, so the next apply resumes it — `resume = no`, or dropping
the task block, is the answer.

The WebUI has no workspace: a browser has no file to read, and it cannot listen,
so `-L` has no equivalent there (its analogue is the preview pin, which binds
nothing locally).

## WebUI

`cmd/harness-webui-wasm` compiles to WASM (`make webui-build`) and is
embedded into the server binary via `webui.FS` (an `embed.FS`). When
`harness-server` is running with a WebSocket listener (default
`--listen host:port`), it serves the WebUI itself at:

- `GET /` — `index.html` (Bubble-Tea-like list of runners / tasks)
- `GET /static/*` — JS / WASM / xterm assets
- `GET /ws` — the WebSocket endpoint the WASM client dials over
  `objproto`

So pointing a browser at `http://server-host:port/` gives you the
same submit / list / cancel / interactive surface as the CLI and TUI,
plus a **Host pin** dropdown for routing to a specific runner by
hostname. The xterm-based interactive view splices the runner's PTY
into the browser tab the same way the TUI does into its terminal.

The page is organised into tabs (端末 / タスク / ファイル / 通知):

- **Tasks** — runners + task list, the submit / compose form (with a
  `--caps` selector and effective-set readout), and a command line.
- **Files** — a per-task worktree browser: navigate directories, push
  (upload a local file), pull (download), delete, and **Preview** a
  selected file in a modal. `.html` files render in a **sandboxed
  `<iframe>`** (`sandbox` with no `allow-same-origin` / scripts) with a
  *View source* toggle to flip between the rendered page and its text.
- **Notifications** — the live notification feed (ring backlog + live),
  plus a form to post one by hand (level / title / body).
- **Terminal** — the interactive PTY view with on-screen key helpers.

UDP-only servers (when `--listen` is empty and only `--udp-listen` is
configured) **do not serve the WebUI** — there is no HTTP listener.
Run WS+UDP dualstack if you want both.

- **No auto-commit.** The runner creates a worktree under
  `<repo>/.harness-worktrees/<task-id>/` and leaves any changes
  uncommitted. You inspect them yourself; `harness-cli prune-local`
  removes old worktrees with `git worktree remove --force`, and
  `harness-cli prune` asks the server to forget terminal task records
  and per-task log files. The server can auto-prune via
  `harness-server --task-retain=DUR` (e.g. `--task-retain=720h`).
- **No sandbox between agent and host *by default*.** Spawned agents run
  with user-level filesystem and network access — the worktree is the
  CWD, not a chroot. Single-user dogfood deployments only; do not point
  the broker at networks you do not control. See the trust model section
  in `runner/agentskills/harness-cli/SKILL.md`. An **opt-in** rootless
  podman confinement is available via the `--agent-bin` seam — see
  **Sandboxing** below.
- **Agent CLI integration is argv-template based.** The runner spawns
  `claude` by default and its default templates target Claude Code
  (`{args} -p {prompt}`, `{args} --continue -p {prompt}`, `{args}` and
  `{args} --continue`). Non-Claude agents can use `--agent-bin`,
  `--agent-oneshot-argv`, `--agent-resume-oneshot-argv`,
  `--agent-interactive-argv` and `--agent-resume-interactive-argv` to map
  harness submit/resume intent onto their own CLI surface — one template
  per launch mode, so every mode a profile can be launched in is one the
  profile can describe. `--agent-interactive-argv` defaults to the bare
  binary (`{args}`), which is what claude / codex / agy / a shell all
  want; set it for an agent whose interactive entry point needs a
  subcommand, and the runner then also requires
  `--agent-resume-interactive-argv` so the resume open cannot silently
  drop that subcommand. `--claude-bin` remains as a
  deprecated alias for `--agent-bin`; `--no-worktree --agent-bin
  {bash,cmd.exe,powershell.exe}` is also a supported pattern for
  generic-process sandbox slots.

## Testing

```bash
# Unit tests across the whole module
go test ./...

# With race detector
go test ./... -race

# Integration smoke (uses testdata/fake-claude.sh)
go test -tags integration ./integration/... -v
```

## Layout

The transport stack — `objproto` (encrypted secure session, ECDH +
AES-GCM), `trsf` (QUIC-like stream multiplexer; flow / congestion /
MTU) and `transport` (WebSocket adapter, incl. WASM build) — lives in
the companion module
[github.com/on-keyday/objtrsf](https://github.com/on-keyday/objtrsf).

```
appwire/              bgn-generated app-layer wire types (AppKind payload ids,
                      PSK auth status) carried over the objtrsf transport
peer/                 Conn + Dial + bidi stream lookup on top of objproto
exec/                 PTY plumbing for `interactive` (frame mux, stream splice)
pubsub/               topic broker for task-log / status fanout
agentboard/           topic broker for agent-to-agent messaging
runner/protocol/      bgn-generated wire schema for control / status messages
topics/               topic name constants
server/               harness server: registry / taskstore / scheduler / WAL / logstore /
                      pubsub + agentboard wiring
runner/               harness runner: worktree manager / claude exec / connect loop /
                      agent env injection / settings.json + skills materialisation
runner/agentskills/   embedded skill files (e.g. harness-cli SKILL.md) the runner
                      writes into each worktree's .claude/skills/; also the
                      directory to hand --agentskills-dir for hot-reload
cli/                  harness client library
cli/agent/            harness-cli `agent ...` subcommands (broker IO from agent side)
tui/                  Bubble Tea TUI components and event loop
webui/                in-browser WebUI (HTML + WASM client)
cmd/
  harness-server/       server binary
  agent-runner/         runner binary
  harness-cli/          CLI binary (user + agent)
  harness-tui/          TUI binary
  harness-webui-wasm/   WASM build target served by harness-server
  harness-stream-adapter/ event-stream adapter: runs an agent behind the neutral
                      NDJSON protocol (runner/streamagent); named per profile via
                      --agent-stream-adapter
scripts/              {runner,server,restart}.{py,sh} daemon lifecycle helpers (sh
                      is a thin shim over py); daemon.py + bootstrap.py provide
                      the cross-platform up/down/respawn primitives via psutil.
                      runner-autostart.py wraps register/unregister of systemd
                      user units (Linux) / Task Scheduler tasks (Windows) for
                      boot/login persistence. build_and_restart_all.py rebuilds
                      and restarts every alive runner, self last.
scripts/sandbox/      opt-in rootless-podman confinement kit for spawned claude
                      (Containerfile + agent-in-podman.sh wrapper + egress
                      firewall / CONNECT-proxy); plugs in via --agent-bin
examples/             notify-hook samples (e.g. Discord webhook relay)
testdata/             fake-claude.sh used by tests
integration/          end-to-end smoke test (build tag: integration)
docs/superpowers/     specs/ and plans/ for design history
```

## License

MIT — see [`LICENSE`](LICENSE). Copyright (c) 2026 on-keyday.

The in-browser WebUI vendors third-party assets under `webui/static/`
(xterm.js + `addon-fit.js` / `xterm.css`, MIT; `wasm_exec.js` from the Go
distribution, BSD-3-Clause). Their license texts and copyright notices are
reproduced in [`THIRD-PARTY-NOTICES.md`](THIRD-PARTY-NOTICES.md).
