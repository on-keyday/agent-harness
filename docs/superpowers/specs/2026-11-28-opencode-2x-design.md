# opencode 2.x support

Status: DECIDED (operator, 2026-11-28) — 2.x only, no 1.x/2.x dual support.

## Why

Arch's `extra/opencode` is 2.4.2 (built 2026-11-26). The next system upgrade
on a runner host moves opencode to 2.x, and two things break at that moment:

- `opencode export <id>` no longer exists (2.x: `opencode session export`), so
  `transcript get` for an opencode task fails.
- 2.x attaches to one per-user background server (`opencode serve --service`)
  by default. That is the shape codex's daemon took in 0.157
  (`scripts/agent_presets.py`, codex `--no-daemon`), and the same fix applies:
  opt out with `--standalone`.

GitHub's `releases/latest` still names v1.19.6, so a check that follows it
tests a version no runner host will run.

## Approaches

1. **2.x only (chosen).** Templates carry `--standalone`, the transcript store
   calls `session export`. Every host running an opencode slot must be on 2.x;
   one left on 1.x fails loudly (1.18.18 exits 1 on the unknown `--standalone`).
2. **Both versions.** The runner reads `opencode --version` at startup and picks
   templates and export verb from it. Rejected: the presets are a static argv
   table, and a version switch is new machinery to carry a version the package
   manager is already replacing.

## Measurements (opencode 2.4.2, linux-x64 release tarball, SHA-256 checked)

Run with `HOME`, `XDG_CONFIG_HOME`, `XDG_DATA_HOME`, `XDG_CACHE_HOME`,
`XDG_STATE_HOME`, `XDG_RUNTIME_DIR` all pointed into a scratch directory, a
scratch git repo with two linked worktrees A and B, model
`opencode/deepseek-v4.1-flash-free` (anonymous Zen). The tool probe was a
script the agent was told to run, writing `$OC_PROBE_MARKER`, `$PWD` and its
parent's argv to a file.

| Question | Command shape | Result |
|---|---|---|
| Does the tool see the client's env or the service's? | `opencode service start` with `OC_PROBE_MARKER=SERVICE`, then `OC_PROBE_MARKER=A opencode run --auto …` | Tool saw `A`; its parent was `opencode-pty daemon`, a child of `serve --service`. With the var unset on the client the tool saw it empty: the client env REPLACES the service's. |
| Does anything start the service on its own? | `opencode service status` before/after each command | `run`: no (it attaches only if one is running). TUI (`opencode --prompt …` in a PTY): yes. `session list` and `session export`: yes. The service outlived the TUI (killed by `timeout`), reparented to `systemd --user`, holding the starting process's env and the `opencode-pty` that parents tool processes. |
| `--standalone` | same TUI and `run` with `--standalone` | Child `opencode serve --stdio --port 0`; no service started; nothing survived the TUI. |
| `--continue` scope | seed `ZEBRA-A`/`ZEBRA-B` in A/B, then `run --standalone --continue` in each | A recalled `ZEBRA-A`, B `ZEBRA-B`. |
| `session list --format json` | in worktree A; outside any project | Rows keep `id`, `directory`, `created`, `updated` (ms); adds `projectId`, `title`. Outside a project: `[]`. Default `--max-count` is 100; `-n 0` is rejected ("greater than or equal to 1"); `-n 2147483647` and `-n 9007199254740991` accepted. |
| `session export` piped vs to a file | session with six 47700-byte tool outputs | 302096 bytes both ways, exit 0. 1.18.18's 128KiB pipe cut is gone. Shape `{info, messages}`. |
| Reads a 1.x database | copy of a 1.18.18 data dir, `session list` + `session export` | Same session ids listed; export exit 0. |
| `{file:…}` in `opencode.json` | openai-compatible provider whose `apiKey` is `{file:<path>}`, pointed at a local HTTP listener | Request carried `Authorization: Bearer <file contents>`. |
| Tool helper origin | `strings` on the binary | `opencode-pty` is embedded (`/$bunfs/root/opencode-pty-…`), extracted to `~/.cache/opencode/bin/`; no download. |

Why `--standalone` even though the env does not leak: under the service, tool
processes are children of a process outside the task, so cancelling the task
does not stop them, and the service keeps the first caller's environment
(including its `HARNESS_*` ticket) alive after that task ends.

## Changes

1. `scripts/agent_presets.py`, `opencode`: `--standalone` on all four
   templates (`run` too: it attaches to a running service). The `sandbox-`
   twin derives from it. A test asserts all four carry it.
2. `runner/transcript/opencode.go`: `session list --standalone --format json
   -n 2147483647` and `session export --standalone <id>`. `--standalone` so the
   runner never starts a service holding the runner's env; the `-n` because
   every task worktree of a repo is one opencode project, and the default 100
   newest would drop older tasks' sessions before the directory filter runs.
   The export still goes to a file and is still checked for one complete JSON
   value: it costs nothing and guards the same failure.
3. `scripts/sandbox/check_agent_releases.py`: pick the newest stable `v*`
   release instead of `releases/latest`.
4. The preset's "verified against" note records 2.4.2.

## Rollout

Branch first; then the operator upgrades opencode on each host with an
opencode slot; then a real opencode task and a `sandbox-opencode` task are run
before landing.

As run (2026-11-28, Arch `opencode 2.4.2-1` on the runner host,
`scripts/dummy-harness.sh up --agent fake -- --agent-profiles <bash +
opencode / sandbox-opencode from agent_presets.agent_profiles_json>`), for
both `opencode` and `sandbox-opencode`: a one-shot whose tool call saw the
task's own `HARNESS_TASK_ID` and `PWD`; a `--resume-conversation` one-shot and
interactive open that recalled the previous turn; `transcript ls` while an
interactive task was live (no hang) and `transcript get` of `export.json`; a
cancel during a tool call that left no opencode, `opencode-pty` or tool
process (and, sandboxed, no container). `opencode service status` read
`stopped` throughout. The sandbox proxy logged only `ALLOW opencode.ai:443`
and `ALLOW models.opencode.ai:443`, no DENY, so `A_DOMAINS` needs nothing new.

## Not measured

- Whether 1.x can still read a data dir after 2.x opened it.
