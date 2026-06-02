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
    - Interactive: `interactive` (PTY spliced to the client; client
      disconnect kills claude — legacy), `session new` (detachable PTY:
      client disconnect leaves claude running on the runner),
      `session attach <id>`, `session ls`, `session kill <id>`.
    - File transfer: `file ls`, `file push`, `file pull`, `file delete`
      against a task's worktree (recursive variants via `-r`, force
      overwrite via `-f`; paths are confined to the worktree root).
    - Port forwarding: `forward <task-id> -L [bind:]lport:rhost:rport`
      (SSH `-L` style — the runner dials `rhost:rport`, bytes relayed
      over the same transport; `-L` repeatable, foreground until Ctrl-C).
    - Agent runtime (called from inside agent sessions):
      `agent {send | wait | inbox | dispatch | subscribe | unsubscribe
      | topics | subscriptions}`. See `runner/agentskills/harness-cli/
      SKILL.md` for conventions.
  - `cmd/harness-tui`: Bubble Tea interactive frontend (sections below).
  - `cmd/harness-webui-wasm`: in-browser WebUI compiled to WASM, served
    by `harness-server`.

Connections use the in-tree `objproto` secure transport (ECDH +
AES-128-GCM) on top of one of two underlays — **WebSocket**
(default, `--listen host:port` on the server) and **UDP**
(`--udp-listen host:port`, which uses the project's own
QUIC-like layering in `trsf`). Both can run simultaneously
(WS+UDP dualstack) on a single server. The `trsf`
stream-multiplexing layer carries control / data frames on top of
either. PSK pre-authentication (`--psk` / `--psk-file`, env
`HARNESS_PSK` / `HARNESS_PSK_FILE`) gates incoming connections
before the secure session starts. Server and runner can run on
different hosts — the `--server-cid` / `HARNESS_SERVER_CID` is a
ConnectionID (`ws:host:port-id` or `udp:host:port-id`) that the
runner / clients dial; the transport prefix selects the underlay.

## Quick start

Run each command in its own terminal. `make build` produces all four
binaries under `bin/`; the examples below assume that.

```bash
# 1. Start the server. --listen accepts host:port (use :8539 to dual-stack on
# all interfaces; loopback by default). PSK file is auto-generated on first
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

# 5a. Attach interactively (legacy: PTY claude on an idle runner, spliced
# to your terminal stdin / stdout / SIGWINCH; client disconnect kills claude).
bin/harness-cli interactive --repo /abs/path/to/repo

# 5b. Open a detachable session instead (claude survives disconnect; reattach
# from any client via `session attach <id>`). `-d` returns immediately
# without splicing the local terminal — useful when you only want to spawn.
bin/harness-cli session new --repo /abs/path/to/repo
bin/harness-cli session ls                       # detachable sessions only
bin/harness-cli session attach <task-id>
bin/harness-cli session kill   <task-id>

# 6. File transfer against a running task's worktree (paths are confined
# to the worktree root; `..` escapes are rejected).
bin/harness-cli file ls     <task-id> [subdir]
bin/harness-cli file push   <task-id> ./local.txt rel/path.txt
bin/harness-cli file pull   <task-id> rel/path.txt ./local.txt
bin/harness-cli file delete <task-id> rel/path.txt
# Recursive directory transfer (tar over the wire) and force overwrite:
bin/harness-cli file push -r -f <task-id> ./local-dir/ rel/dir
bin/harness-cli file pull -r -f <task-id> rel/dir ./local-dir/

# 7. Port-forward a runner-side port to your machine (SSH -L style). The runner
# dials remote-host:remote-port; bytes relay over the harness transport. Handy
# for reaching a dev server the agent started inside its worktree. Foreground;
# Ctrl-C tears down. bind defaults to 127.0.0.1; -L is repeatable.
bin/harness-cli forward <task-id> -L 3000:127.0.0.1:3000
```

### Daemon lifecycle helpers

`scripts/{runner,server,restart}.sh` are thin shims over the
canonical Python implementations `scripts/{runner,server,restart}.py`
(`bootstrap.py` provisions `scripts/.venv` with psutil on first call,
so the .py side runs cross-platform). State files
(`bin/.run/<slot>.{pid,log}`) are shared between the bash and python
entry points, so a daemon started via one can be controlled by the
other. Pass `--as <tag>` to `up` / `down` to run multiple instances
of the same daemon side by side (e.g.
`scripts/runner.sh up --as 2 --max-tasks 2 ...` registers an extra
runner alongside the primary one, with its own
`bin/.run/agent-runner-2.{pid,log}` slot).

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
  `--claude-bin bash`). Disables `.claude/settings.json` and
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
tab focus · s submit · enter follow · c cancel · ? help · q quit
```

Keys:

| Key | Action |
|---|---|
| `Tab` / `Shift+Tab` | Cycle focus runners → tasks → logs → cmdresult → cmdline |
| `s` | Open the multi-line submit popup (`Ctrl+J` / `Ctrl+Enter` to send, `Esc` to cancel) |
| `S` | Open a detachable session in the default repo (equivalent to `harness-cli session new`) |
| `i` (tasks focus) | Attach interactively to the focused task, or open a new interactive session in the default repo if no task is focused. On a Detached row, reattaches via `session attach`. |
| `r` / `R` (tasks focus) | `r`: reattach a Detached / Running detachable session, or resume a finished task with `--continue`. `R`: resume fresh (no `--continue`). |
| `F` (tasks focus) | Open the file browser for the selected task's worktree (push / pull / delete). |
| `p` / `P` (tasks focus) | `p`: open the port-forward prompt (enter a `-L` spec) for the selected task; the forward runs in the background. `P`: stop that task's active forward. |
| `d` | Detail popup for the focused row (runners or tasks) |
| `Enter` (tasks focus) | Follow the selected task's log |
| `c` (tasks focus) | Cancel the selected task |
| `/` (logs focus) | Enter/edit filter; `Esc` clears |
| `q`, `Ctrl+C` | Quit |

The cmdline accepts `submit / interactive / session {new,attach,ls,kill}
/ cancel / prune / repo / clear / help / quit`. `session new` supports
`--host NAME | --runner HEX | --ip ADDR` for runner-pinning (mutually
exclusive), plus `--detach` to spawn-and-exit without splicing the
local terminal. Use `harness-cli prune-local` for local-only worktree
cleanup; the TUI's `prune` command is server-only. slog output
(transport / pubsub / etc.) is folded into the cmdresult pane with a
`[log]` prefix so it never scribbles over the alt screen.

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
- **No sandbox between agent and host.** Spawned agents run with
  user-level filesystem and network access — the worktree is the CWD,
  not a chroot. Single-user dogfood deployments only; do not point the
  broker at networks you do not control. See the trust model section in
  `runner/agentskills/harness-cli/SKILL.md`.
- **Built around Claude Code.** The runner spawns `claude` by default
  and the integration assumes its CLI surface (worktree →
  `--resume` / `--continue`, session storage keyed by cwd hash, etc.).
  `--claude-bin` accepts any executable, and `--no-worktree
  --claude-bin {bash,cmd.exe,powershell.exe}` is a supported pattern
  for generic-process sandbox slots — but you trade away the
  claude-specific niceties (worktree-based isolation, session resume
  across runner restart). No protocol-level integration with other
  agent CLIs (Aider, Cursor, etc.); they would have to be treated as
  opaque `--claude-bin` targets the same way.

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
scripts/              {runner,server,restart}.{py,sh} daemon lifecycle helpers (sh
                      is a thin shim over py); daemon.py + bootstrap.py provide
                      the cross-platform up/down/respawn primitives via psutil.
                      runner-autostart.py wraps register/unregister of systemd
                      user units (Linux) / Task Scheduler tasks (Windows) for
                      boot/login persistence. build_and_restart_all.py rebuilds
                      and restarts every alive runner, self last.
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

