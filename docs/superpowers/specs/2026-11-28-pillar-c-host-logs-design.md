# Pillar C — reading server and runner process logs through the harness

Date: 2026-11-28.

Implements Pillar C ("host logs") of `2026-11-12-run-modes-and-records-grand-design.md`
(cited as **GD**; rows G12, G13, G19, G34, O6, P6).

## Problem

**P6 (GD):** Process logs of `harness-server` and `agent-runner` are only readable
on their own host (typically under `bin/.run/<slot>.log` when launched via
`scripts/daemon.py`, or on stderr when launched interactively). Remote
management needs to read both the server's log and any connected runner's log
through the harness, without SSHing into the host.

## Goals

- Read the server's process log (`server log`) and any connected runner's
  process log (`runner log <runner>`) over a stream, as a tail (default) or a
  byte range (`--offset`, `--max-bytes`).
- Gate both reads on a new grantable, non-scoped capability bit `host_log`
  (G12, G19, G34), default-off (O6) and held by the operator (`Capability_All`).
- Work both under `scripts/daemon.py` (which redirects stdout/stderr to
  `bin/.run/<slot>.log` and injects `--log-file`) and when started manually
  without `--log-file` (falling back to a 1 MiB in-process ring buffer).
- Cover CLI, TUI (cmdline + viewer modal), WebUI (cmdline + viewer modal), and
  MCP.

## Non-goals

- **Live follow (`-f`).** Tail and byte-range snapshots only. Following a
  process's own log over the same `trsf` transport that logs warnings on
  congestion/errors risks a positive feedback loop (log storm).
- **Reading offline runners' logs.** A runner's log lives on the runner host;
  an offline runner answers `runner_offline`.
- **Log rotation or retention management.** `--log-file` appends to the named
  file; external rotation (if any) is outside this spec.

## Decisions

| # | Decision | Status |
|---|---|---|
| C-D1 | **Log source:** `--log-file <path>` on `harness-server` and `agent-runner`. When set, `slog` writes (append) to that file instead of stderr, and `host_log` reads from that file (`source=file`). `scripts/daemon.py` auto-injects `--log-file bin/.run/<slot>.log` (stripping duplicates like `--shutdown-file`); stderr remains redirected to the same file for Go panics/pre-flag errors without duplicating `slog` lines. When `--log-file` is unset, `slog` writes to `os.Stderr` mirrored into a 1 MiB in-process ring buffer (`source=ring`). | DECIDED (operator, 2026-11-28) |
| C-D2 | **Capability & default (O6):** New bit `Capability.host_log = 0x200000000` (`Capability.all` widened to `0x3ffffffff`), no scope (`ScopeNone`, G19), default-off (operator holds it via `all`; tasks receive it only when explicitly granted). | DECIDED (operator, 2026-11-28) |
| C-D3 | **Mode:** Tail (default, last `max_bytes`) and byte-range (`--offset`, `--max-bytes`) snapshots only. No `-f` follow. Default `max_bytes` = 64 KiB, hard cap = 4 MiB. | DECIDED (operator, 2026-11-28) |
| C-D4 | **Command grammar:** `server log [--offset N] [--max-bytes N]` and `runner log <runner> [--offset N] [--max-bytes N]`, under the existing `server` and `runner` verb families. | DECIDED (operator, 2026-11-28) |
| C-D5 | **Runner rollout safety (Pitfall 10):** `RunnerHello` claims 1 bit from its existing `reserved :u5` (`host_logs :u1`, `reserved :u4`), so `RunnerHello` has zero wire-layout change. The server only sends `RunnerRequestType.host_log` to a runner that advertised `host_logs = 1`; older runners answer `runner_unsupported` without being sent an unknown request. | Spec decision |
| C-D6 | **Single stream wire shape (G13):** Both `server log` and `runner log` use `TaskControlKind.host_log` (`HostLogRequest` → `HostLogResponse{status, stream_id}`). On `ok`, the stream carries one `HostLogResult` header followed immediately by `returned_bytes` raw bytes, then EOF. For `runner log`, the server splices the runner's stream straight to the client's stream (`spliceBidiHalfClose`), identical to `transcript` and `git_query`. | Spec decision |

## Credential & secret log audit (Pillar C requirement)

Audited 2026-11-28 across `cmd/harness-server/`, `cmd/agent-runner/`, `server/`,
`runner/`, `agentboard/`, `connauth/`, and `peer/` via:

```
grep -rnE '(slog\.|Logger\.|log\.|fmt\.Fprint|fmt\.Print)' \
  cmd/harness-server/ cmd/agent-runner/ server/ runner/ agentboard/ connauth/ peer/ \
  --include='*.go' | grep -v _test.go \
  | grep -iE 'psk|secret|ticket|token|pass|seed|priv|cred|auth|bearer|cookie|env|header|key'
```

Findings:
- **Operator PSK / server private key / runner private key:** Only file paths
  (`path`) and Ed25519 **public** keys (`pubkey` / `keyPrefix`) are logged. No
  PSK value, private seed, or raw ticket is logged anywhere.
- **Credentials:** `connauth.Credential` implements `slog.LogValuer`
  (`connauth/credential.go:74`) to redact secret material.
- **X11 cookies:** Only absence/failure is logged (`runner/session.go:874-880`),
  never cookie bytes.
- **Notify hook:** `cmd/harness-server/main.go:362` logs the configured
  `--notify-hook` command string; operators should not embed raw secrets in
  hook argv.

No credential leaks were found in `slog` output.

## Wire schema (`runner/protocol/message.bgn`)

1. `Capability`:
   - `host_log = 0x200000000, "host_log"`
   - `all = 0x3ffffffff, "all"`
2. `RunnerHello`:
   - Replace `reserved :u5` with `host_logs :u1` + `reserved :u4`.
3. `RunnerRequestType`:
   - Append `host_log`.
4. `TaskControlKind`:
   - Append `host_log`.
   - `TaskControlRequest`: `TaskControlKind.host_log => host_log :HostLogRequest`
   - `TaskControlResponse`: `TaskControlKind.host_log => host_log :HostLogResponse`
5. Formats:

```bgn
enum HostLogTarget:
    :u8
    server = 0, "server"
    runner = 1, "runner"

enum HostLogMode:
    :u8
    tail  = 0, "tail"
    range = 1, "range"

enum HostLogSource:
    :u8
    file = 0, "file"
    ring = 1, "ring"

enum HostLogStatus:
    :u8
    ok                 = 0, "ok"
    no_such_runner     = 1, "no_such_runner"
    runner_offline     = 2, "runner_offline"
    runner_unsupported = 3, "runner_unsupported"
    unavailable        = 4, "unavailable"
    io_error           = 5, "io_error"
    internal_error     = 6, "internal_error"

format HostLogRequest:
    target    :HostLogTarget
    runner_id :RunnerID       # zero when target == server
    mode      :HostLogMode
    offset    :u64            # start byte offset when mode == range
    max_bytes :u32            # 0 => default (64 KiB); capped at 4 MiB

format HostLogResponse:
    status    :HostLogStatus
    stream_id :u64            # HostLogResult header + raw bytes; 0 unless ok

format RunnerHostLogRequest:
    stream_id :u64
    mode      :HostLogMode
    offset    :u64
    max_bytes :u32

format HostLogResult:
    status         :HostLogStatus
    source         :HostLogSource
    total_size     :u64
    start_offset   :u64
    returned_bytes :u32
    message_len    :u16
    message        :[message_len]u8
```

## Storage & read semantics (`hostlog` package)

- `hostlog.DefaultMaxBytes = 64 << 10` (64 KiB), `hostlog.MaxBytesCap = 4 << 20` (4 MiB), `hostlog.DefaultRingSize = 1 << 20` (1 MiB).
- `hostlog.Setup(logFilePath string) (*hostlog.Reader, func() error, error)`:
  - When `logFilePath != ""`: ensures parent dir exists, opens file `O_CREATE|O_WRONLY|O_APPEND` (`0o600`), installs `slog.SetDefault(slog.New(slog.NewTextHandler(f, nil)))`, returns a file-backed `Reader` and a closer.
  - When `logFilePath == ""`: creates a 1 MiB `Ring`, installs `slog.SetDefault(slog.New(slog.NewTextHandler(io.MultiWriter(os.Stderr, ring), nil)))`, returns a ring-backed `Reader` and a no-op closer.
- `Reader.Read(mode, offset, maxBytes)`:
  - Normalizes `maxBytes`: 0 → 64 KiB, > 4 MiB → 4 MiB.
  - **File mode:** opens `path` read-only, `Stat()`s `totalSize`:
    - `tail`: `start = max(0, totalSize - maxBytes)`, reads up to `totalSize - start` bytes via `ReadAt`.
    - `range`: if `offset >= totalSize`, returns empty slice with `start = totalSize`; else reads `min(maxBytes, totalSize - offset)` bytes at `offset`.
  - **Ring mode:** locks ring, `totalSize = ring.totalWritten`, `earliest = totalSize - len(retained)`:
    - `tail`: `start = max(earliest, totalSize - maxBytes)`, returns bytes `[start, totalSize)`.
    - `range`: clamps `start = max(offset, earliest)`; if `start >= totalSize`, returns empty slice with `start = totalSize`; else returns `min(maxBytes, totalSize - start)` bytes from `start`.

## Surfaces

| Surface | Server log | Runner log |
|---|---|---|
| CLI (`harness-cli`) | `server log [--offset N] [--max-bytes N]` | `runner log <runner> [--offset N] [--max-bytes N]` |
| MCP (`harness-cli mcp`) | `server_log` tool | `runner_log` tool |
| TUI cmdline | `server log [--offset N] [--max-bytes N]` | `runner log <runner> [--offset N] [--max-bytes N]` |
| TUI modal / keybinding | Log modal opened from cmdline or key (`L` on runners panel opens selected runner's log; `server log` opens server log in modal) | `L` on runners table opens selected runner's log modal (and cmdline `runner log <runner>`) |
| WebUI cmdline | `server log [--offset N] [--max-bytes N]` | `runner log <runner> [--offset N] [--max-bytes N]` |
| WebUI controls | "Server log" button in header/admin area opening `<dialog class="picker-modal">` log viewer | "Log" button on each runner row opening the same log viewer dialog |
| Capability catalog & pickers | `host_log` in `cli/verb/caps.go`, TUI authority picker, WebUI cap chips, `whoami`, `ls` | Same |
