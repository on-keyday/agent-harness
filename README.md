# agent-harness

Parallel Claude Code CLI harness — local task dispatcher (v1).

A small system for running multiple Claude Code instances in parallel against one or
more git repos, queueing tasks from a CLI, and collecting per-task logs and worktrees.

See `docs/superpowers/specs/2026-04-25-parallel-agent-harness-design.md` for the full
design and `docs/superpowers/plans/2026-04-25-parallel-agent-harness-v1.md` for the
implementation plan.

## Architecture

```
┌──────────────┐         ┌───────────────────┐         ┌──────────────────┐
│ harness-cli  │─ WS ───▶│ harness-server    │◀── WS ──│ agent-runner     │ × N
│  submit/ls/  │         │  Registry         │         │  worktree+claude │
│  logs/cancel │         │  TaskStore (WAL)  │         │  per task        │
└──────────────┘         │  Scheduler        │         └──────────────────┘
                         │  PubSub broker    │
                         │  LogStore         │
                         └───────────────────┘
```

- **Server** (`cmd/harness-server`): hub. Accepts WebSocket connections from runners and
  CLI, queues tasks, dispatches them to idle runners, persists state via JSONL WAL,
  appends per-task logs to `<data-dir>/logs/<task-id>.log`.
- **Runner** (`cmd/agent-runner`): worker. Bound to one repo at startup. On each task,
  creates a `git worktree`, runs `claude -p <prompt>` in it, streams stdout/stderr to
  the server via pubsub, reports the exit code.
- **CLI** (`cmd/harness-cli`): user frontend. Subcommands: `submit`, `ls`, `logs`,
  `cancel`, `prune`. (`watch` to follow.)

Connections use the in-tree `objproto` secure transport (ECDH + AES-128-GCM) over
WebSocket, with the `trsf` stream-multiplexing layer carrying control frames and the
`pubsub` broker fanning out per-task log topics.

## Quick start

Run each command in its own terminal.

```bash
# 1. Start the server
go run ./cmd/harness-server --port 8539 --data-dir ./harness-data

# 2. Start a runner bound to a repo (one process per parallel slot)
go run ./cmd/agent-runner --server localhost:8539 --repo /abs/path/to/repo
go run ./cmd/agent-runner --server localhost:8539 --repo /abs/path/to/repo  # 2 in parallel

# 3. Submit a task (run from inside the repo, or pass --repo)
cd /abs/path/to/repo
go run /elsewhere/agent-harness/cmd/harness-cli submit --task "test task"
# → prints task ID

# 4. Inspect
go run ./cmd/harness-cli ls
go run ./cmd/harness-cli logs <task-id>
go run ./cmd/harness-cli cancel <task-id>
```

## v1 limitations / non-goals

- **Local only.** Server and runners must be reachable on `localhost`. The transport
  supports remote operation; configurable endpoints will land in v2.
- **No auto-commit.** The runner creates a worktree under `<repo>/.harness-worktrees/<task-id>/`
  and leaves any changes uncommitted. You inspect them yourself; `harness-cli prune`
  removes old worktrees with `git worktree remove --force` AND asks the server to
  forget the corresponding task records and per-task log files. Pass `--offline` to
  do only the local worktree pass. The server can also auto-prune by passing
  `harness-server --task-retain=DUR` (e.g. `--task-retain=720h`).
- **One task per runner process.** Spawn N runners against the same repo to get N
  parallel slots — the server schedules FIFO across idle runners.
- **No interactive attach.** Submitted tasks run headless. The "session multiplexer" /
  attach experience is the v2 goal.
- **Claude Code only.** No support for other agents; the runner shells out to `claude -p`.

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
objproto/         encrypted secure session (ECDH + AES-GCM)
trsf/             stream multiplexer (QUIC-like; flow / congestion / MTU)
transport/        WebSocket adapter for objproto
pubsub/           topic broker built on trsf streams
runner/protocol/  bgn-generated wire schema for control / status messages
topics/           topic name constants
server/           harness server: registry / taskstore / scheduler / handlers / WAL / logstore
runner/           harness runner: worktree manager / claude exec / connect loop
cli/              harness CLI library (submit / ls / logs / cancel / prune)
cmd/
  harness-server/   server binary
  agent-runner/     runner binary
  harness-cli/      CLI binary
testdata/         fake-claude.sh used by tests
integration/      end-to-end smoke test (build tag: integration)
```
