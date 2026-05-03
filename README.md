# agent-harness

Parallel Claude Code CLI harness — task dispatcher with multi-agent
messaging.

A system for running multiple Claude Code instances in parallel against
one or more git repos. Submit tasks from a CLI / TUI / WebUI, attach
interactively to running agents, and let agents talk to each other over
a per-task broker.

The original design lives in
`docs/superpowers/specs/2026-04-25-parallel-agent-harness-design.md`;
follow-up specs covering TUI, multi-task scheduling, agent-to-agent
messaging, WASM transport, PSK auth, etc. are alongside it under
`docs/superpowers/specs/`.

## Architecture

```
┌─────────────────────┐
│ harness-cli (CLI)   │─┐
│ harness-tui (TUI)   │ │      ┌──────────────────────┐         ┌────────────────────────┐
│ harness-webui (WASM)│ ├─WS──▶│ harness-server       │◀── WS ──│ agent-runner           │ × N
└─────────────────────┘ │      │  Registry            │         │  worktree mgr          │
                        │      │  TaskStore (WAL)     │         │  claude exec (× M)     │
                        │      │  Scheduler           │         │  per-task PTY for      │
                        │      │  pubsub  (task log/  │         │   `interactive`        │
                        │      │           status)    │         └────────────────────────┘
                        │      │  agentboard (agent ↔ │
                        │      │              agent)  │
                        │      │  LogStore            │
                        │      └──────────────────────┘
```

- **Server** (`cmd/harness-server`): hub. Listens on WebSocket; accepts
  connections from clients (CLI / TUI / WebUI WASM) and runners. Queues
  tasks, dispatches them to idle runners by repo affinity, persists task
  state via JSONL WAL, appends per-task logs to
  `<data-dir>/logs/<task-id>.log`. Hosts two distinct brokers: `pubsub`
  for task-log and runner/task-status fanout, and `agentboard` for
  agent-to-agent messaging keyed by `(runner_id, task_id)`.
- **Runner** (`cmd/agent-runner`): worker. Started with a list of repo
  roots (`--roots`) it is allowed to serve and a per-process concurrency
  cap (`--max-tasks`). For each assigned task, creates a `git worktree`
  under `<repo>/.harness-worktrees/<task-id>/`, runs `claude` (or PTY
  for `interactive`) in it, streams stdout/stderr through the server,
  reports the exit code. Injects `HARNESS_*` env vars into the agent
  subprocess so the agent can reach back via `harness-cli agent ...`.
- **Clients**:
  - `cmd/harness-cli` (`submit`, `ls`, `logs`, `cancel`, `prune`,
    `prune-local`, `watch`, `interactive`, plus the `agent {send | wait
    | inbox | dispatch | subscribe | unsubscribe | topics |
    subscriptions}` family used from inside agent sessions).
  - `cmd/harness-tui`: Bubble Tea interactive frontend (sections below).
  - `cmd/harness-webui-wasm`: in-browser WebUI compiled to WASM, served
    by `harness-server`.

Connections use the in-tree `objproto` secure transport (ECDH +
AES-128-GCM) over WebSocket, with the `trsf` stream-multiplexing layer
carrying control frames. PSK pre-authentication (`--psk` /
`--psk-file`, env `HARNESS_PSK` / `HARNESS_PSK_FILE`) gates incoming
connections before the secure session starts. Server and runner can run
on different hosts — the `--server-cid` / `HARNESS_SERVER_CID` is a
ConnectionID (`ws:host:port-id`) that the runner / clients dial.

## Quick start

Run each command in its own terminal. `make build` produces all four
binaries under `bin/`; the examples below assume that.

```bash
# 1. Start the server. --listen accepts host:port (use :8539 to dual-stack on
# all interfaces; loopback by default). PSK file is auto-generated on first
# run if --psk-file is unset.
bin/harness-server --listen :8539 --data-dir ./harness-data

# 2. Start a runner. --roots is a comma-separated list of repo paths this
# runner is allowed to serve (matched verbatim against submit --repo).
# --max-tasks N lets one runner process handle N concurrent tasks.
bin/agent-runner --server-cid 'ws:HOSTNAME:8539-*' \
                 --roots /abs/path/to/repo,/abs/path/to/other-repo \
                 --max-tasks 4

# 3. Submit a task. --repo is required (or set HARNESS_REPO_PATH); it must
# match a runner's --roots entry verbatim (no client-side normalisation).
bin/harness-cli --server-cid 'ws:HOSTNAME:8539-*' \
                submit --repo /abs/path/to/repo --task "test task"
# → prints task ID

# 4. Inspect / control
bin/harness-cli ls
bin/harness-cli logs <task-id>          # stream the task's log
bin/harness-cli watch                   # stream task / runner status events
bin/harness-cli cancel <task-id>
bin/harness-cli prune --before 168h     # forget terminal tasks older than 7d
bin/harness-cli prune-local --before 168h   # remove old local worktrees

# 5. Attach interactively (allocates a PTY claude on an idle runner, splices
# your terminal stdin / stdout / SIGWINCH to it).
bin/harness-cli interactive --repo /abs/path/to/repo
```

`scripts/runner.sh` and `scripts/server.sh` wrap the binaries as
`nohup`-detached daemons (state under `bin/.run/<slot>.{pid,log}`); use
`scripts/restart.sh <slot>` to restart a daemon while inheriting its
flags + CWD from `/proc/<pid>`. Pass `--as <tag>` to `up` / `down` to
run multiple instances of the same daemon side by side
(e.g. `scripts/runner.sh up --as 2 --max-tasks 2 ...` registers an
extra runner alongside the primary one, with its own
`bin/.run/agent-runner-2.{pid,log}` slot).

## TUI

`cmd/harness-tui` is an interactive Bubble Tea frontend that bundles
`submit / ls / logs / cancel / prune / watch / interactive` into one
screen.

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
tab focus · s submit · enter follow · c cancel · ? help · q quit
```

Keys:

| Key | Action |
|---|---|
| `Tab` / `Shift+Tab` | Cycle focus runners → tasks → cmdline |
| `s` | Open the multi-line submit popup (`Ctrl+J` / `Ctrl+Enter` to send, `Esc` to cancel) |
| `Enter` (tasks focus) | Follow the selected task's log |
| `c` (tasks focus) | Cancel the selected task |
| `q`, `Ctrl+C` | Quit |

The cmdline accepts `submit / cancel / prune / clear / help / quit` (use
`harness-cli prune-local` for local-only worktree cleanup; the TUI's
`prune` command is server-only). slog output (transport / pubsub / etc.)
is folded into the cmdresult pane with a `[log]` prefix so it never
scribbles over the alt screen.

## Non-goals / current limitations

- **No auto-commit.** The runner creates a worktree under
  `<repo>/.harness-worktrees/<task-id>/` and leaves any changes
  uncommitted. You inspect them yourself; `harness-cli prune-local`
  removes old worktrees with `git worktree remove --force`, and
  `harness-cli prune` asks the server to forget terminal task records
  and per-task log files. The server can auto-prune via
  `harness-server --task-retain=DUR` (e.g. `--task-retain=720h`).
- **No sandbox between agent and host.** Spawned agents run with
  user-level filesystem and network access — the worktree is the CWD,
  not a chroot. Single-user dogfood deployments only; do not point the
  broker at networks you do not control. See the trust model section in
  `runner/agentskills/harness-cli/SKILL.md`.
- **Claude Code only.** No support for other agents; the runner shells
  out to `claude` (configurable via `--claude-bin`).

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

```
objproto/             encrypted secure session (ECDH + AES-GCM)
trsf/                 stream multiplexer (QUIC-like; flow / congestion / MTU)
transport/            WebSocket adapter for objproto (incl. WASM build)
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
                      writes into each worktree's .claude/skills/
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
scripts/              up/down/restart wrappers around the daemons
testdata/             fake-claude.sh used by tests
integration/          end-to-end smoke test (build tag: integration)
docs/superpowers/     specs/ and plans/ for design history
```
