# Oneshot agent progress streaming

Date: 2026-07-28

## Problem statement

Three defects, discovered together while asking why a oneshot task's log pane
stays empty until the task ends.

**P1 — A oneshot `claude` task emits nothing to its log topic until it exits.**
The runner's plumbing is not at fault: `Process.Run` already scans stdout and
stderr line-by-line and publishes each line to `task.<id>.log`
(`runner/process.go:146-164`, `runner/session.go:515-518`). The cause is the
agent CLI. `claude -p` with the default `--output-format text` buffers
everything and prints only the final assistant text. Measured: a prompt asking
for 15 numbered lines produced all 15 lines within a 52 ms window (pipe drain
speed) after several seconds of silence. Consequence: in every operator surface
(CLI `logs`, TUI log pane, WebUI log pane) a long-running oneshot task is
indistinguishable from a hung one.

**P2 — A oneshot `codex` task hangs until the 30-minute timeout.**
`handleAssign` sets `Process.OnStdinWriter` unconditionally
(`runner/session.go:507-513`), which makes `Process.Run` attach an `io.Pipe` to
the child's stdin (`runner/process.go:100-104`). That pipe's write end is closed
only when the process exits or the task context is cancelled
(`runner/process.go:134-140`), so the child never sees EOF. `codex exec` treats
piped stdin as an appended `<stdin>` block and blocks waiting for it. Measured:
identical prompt completes in 10.4 s with stdin at `/dev/null`, produces no
output for 60 s with a held-open pipe.

The stdin pipe exists only to deliver agentboard wake markers to oneshot tasks
(`runner/process.go:29-34`). That delivery does not work. Measured against
claude 2.1.220: with the first turn artificially extended to ~45 s, a marker
written mid-turn — text, 100 ms gap, lone `\r`, exactly what
`Session.WakeStdin` does (`runner/session.go:791-802`) — produced no second
turn. `claude -p` prints `Warning: no stdin data received in 3s, proceeding
without it.` and does not read stdin again. So P2 is a live agent being broken
by an inert mechanism.

The interactive path is unaffected and stays as-is: it writes to a PTY master
(`runner/session.go:727-735`), where the marker arrives as real keystrokes to
the Ink-based UI. Every design comment about wake behaviour
(`runner/session.go:46-67`) describes that path.

**P3 — The TUI log pane shows the same content concatenated several times.**
`LogHistoryMsg` is guarded only by `msg.TaskID != a.logs.TaskID()`
(`tui/app.go:387`), which always passes when the task did not change.
`followTask` calls `logs.Reset` and then issues a *new* `GetTaskLog`
(`tui/app.go:1406-1423`) with no way to invalidate a response already in
flight. Two calls for the same task therefore fold two full copies of the log
file into the pane:

```
followTask #1 -> Reset -> GetTaskLog (H1)
followTask #2 -> Reset -> GetTaskLog (H2)
H1 arrives -> TaskID matches -> Prepend(whole file)
H2 arrives -> TaskID matches -> Prepend(whole file)   <- second copy
```

`followTask` has two call sites (`tui/app.go:426`, `tui/app.go:1181`). The
everyday trigger is the second one: a large log makes the fetch slow, nothing
appears, the operator presses Enter again. N presses produce N copies.

A related looseness feeds the first call site. `cmd/harness-tui/main.go:80-82`
re-subscribes to the followed task's log on every reconnect, bound to the
persist-loop `runCtx`. `followTask` tracks only its own `a.logsCancel`, so it
cannot stop that subscription: two subscriptions to one topic can coexist,
raising how often `SubscribedMsg{Resubscribed:true}` fires and therefore how
often `followTask` re-runs.

## Non-goals

- Changing anything about interactive (PTY) tasks.
- Per-task or per-surface verbosity control. Adding one would create an
  operator-surface matrix (CLI, TUI keybindings, TUI cmdline, TUI popups, WebUI
  buttons, WebUI cmdline, WASM bridge) for a display preference. Operators who
  want different rendering define another agent profile.
- Rendering agent events structurally (timelines, per-tool panes). The log
  topic carries text; it keeps carrying text.
- A `gemini` profile. No authoritative argv exists for it in this repo.

## Decisions taken

1. **Decode in the runner, not in the UIs.** The alternative — publish raw
   NDJSON and render per surface — needs a renderer in CLI, TUI, WebUI and the
   WASM bridge, and inflates the persisted log file. Decoding in the runner
   means no operator surface changes at all. The cost is that the runner now
   tracks two agent CLIs' JSON schemas.
2. **One decoder per agent, one renderer for all.** The two schemas were
   designed independently, so a single decoder is not possible. Everything
   downstream of the decoder is shared.
3. **Profiles declare their decoder.** `AgentProfile` already carries per-agent
   argv templates; the log format joins them. No `if bin == "codex"` anywhere.
4. **New flags go before `{args}`.** Agent CLIs are last-wins, and per-task
   extras are appended after the profile's own args
   (`runner/session.go:494-506`). Putting the new flags first keeps
   `--claude-args '--output-format text'` working as an override; the decoder
   then sees non-JSON and falls back to raw passthrough.
5. **Delete the oneshot stdin path rather than repair it.** Repair would mean
   `--input-format stream-json`, which also requires JSON-encoding the initial
   prompt and has no codex equivalent. The mechanism has no demonstrated user.
6. **Undecodable lines pass through raw.** Agent-level warnings printed outside
   the JSON stream stay visible.
7. **The TUI owns the log subscription exclusively.** Re-following on reconnect
   moves from `cmd/harness-tui/main.go` into the app's `BindClientMsg`
   handling, so one code path owns the lifecycle.

## Design

### Component 1 — `runner/agentlog` (new package)

The complete interface. Nothing here is added in a later step.

```go
package agentlog

type Kind int

const (
    KindRaw       Kind = iota // line the decoder could not interpret
    KindSessionStart          // claude system/init; codex thread.started
    KindThinking              // reasoning content
    KindToolStart             // tool invocation issued
    KindToolEnd               // tool result observed
    KindText                  // assistant message text
    KindFinish                // turn/run completed
)

// Stats carries whatever the agent reported at finish. Fields absent from a
// given agent stay zero: claude reports duration and cost, codex reports token
// counts. Render prints only non-zero fields.
type Stats struct {
    DurationMS   int64
    CostUSD      float64
    InputTokens  int64
    OutputTokens int64
}

type Event struct {
    Kind     Kind
    Text     string // KindRaw, KindText, KindThinking, KindSessionStart (id)
    Tool     string // KindToolStart, KindToolEnd
    Args     string // KindToolStart: compact JSON of the tool input
    Result   string // KindToolEnd
    ExitCode *int   // KindToolEnd, when the agent reports a process exit code
    IsError  bool   // KindToolEnd, when the agent reports failure without a code
    Stats    Stats  // KindFinish
}

// Decoder converts one line of agent stdout into zero or more events. Content it
// cannot interpret yields exactly one KindRaw event holding the line verbatim;
// a blank or whitespace-only line yields no events (zero-length slice). Decode
// never returns an error: a malformed line must not fail the task.
//
// The claudeStreamJSON decoder drops blank lines as stream artifacts. The
// passthrough decoder does not, preserving all output byte-for-byte when used
// for non-JSON agent output.
type Decoder interface {
    Decode(line []byte) []Event
}

// NewDecoder resolves a profile's declared log format. An empty or unknown
// name yields the passthrough decoder, which emits one KindRaw per line.
func NewDecoder(format string) Decoder

// Render formats one event as a single log line, without a trailing newline.
// The format is identical across agents.
func Render(e Event) string
```

Recognised format names: `""` (passthrough), `"claude-stream-json"`,
`"codex-jsonl"`. An unknown name resolves to passthrough — a profile
misconfiguration degrades to today's behaviour instead of failing the task.

Rendered forms:

```
· thinking
→ Bash: {"command":"echo one","description":"Run echo one"}
← one
done
✓ 5180ms $0.016467
```

`KindThinking` renders as the fixed string `· thinking` and never carries text.
This is forced, not merely preferred. Measured with one prompt across three
models, reading the `thinking` content block out of `--output-format
stream-json`:

| model | `thinking` text | `signature` | final text |
| --- | --- | --- | --- |
| Haiku 4.5 | 2192 chars | — | 775 chars |
| Sonnet 5 | 0 chars | 10260 chars | 664 chars |
| Opus 5 | 0 chars | 3532 chars | 1219 chars |

The empty field is the API's `thinking.display` default (`"omitted"`), not an
absence of content: `display: "summarized"` returns a readable summary. That
default changed silently between generations — Opus 4.6 and Sonnet 4.6 returned
summaries by default, the Claude 5 family does not, and Haiku 4.5 predates the
change, which is why it still returns text. The raw chain of thought is never
returned on any model, so Haiku's 2192 characters are themselves a summary.

What settles it for this harness is narrower: the `claude` CLI exposes no flag
for `thinking.display` (checked against 2.1.220's `--help`), so the runner
cannot request summaries even if it wanted them. A renderer that printed
`.thinking` would emit blank lines for every Claude 5 model.

Two consequences for the decoder: it must emit `KindThinking` on the block's
presence rather than on its content being non-empty, and it must never render
`signature`, which is kilobytes of opaque base64 per turn — the field exists so
a block can be replayed and integrity-checked without being read, not for
display. Where a summary does arrive (Haiku 4.5) it ran roughly 3x the length
of the final answer, so printing it would have tripled log volume on that model
alone.

`KindToolStart`
truncates `Args` and `KindToolEnd` truncates `Result` at 200 bytes on a rune
boundary, with a trailing `…` when truncated. `KindSessionStart` renders the
session/thread id. `KindRaw` renders the line verbatim.

`ExitCode` and `IsError` are separate fields because the two agents report tool
failure differently and neither maps onto the other without inventing
information: codex's `command_execution` carries a real process `exit_code`,
while claude's `tool_result` carries only an `is_error` boolean for tools that
never ran a process. Each decoder sets whichever its agent actually reports and
leaves the other zero.

### Component 2 — profile plumbing

```go
// runner/agent_profile.go
type AgentProfile struct {
    // ... existing fields ...
    LogFormat string // "" | "claude-stream-json" | "codex-jsonl"
}

// agentProfileJSON gains: LogFormat string `json:"logFormat"`
```

```go
// runner/process.go
type Process struct {
    // ... existing fields, minus OnStdinWriter ...
    LogFormat string
}
```

`runner/session.go` `handleAssign` passes `agentProfile.LogFormat` through and
no longer sets `OnStdinWriter`.

`ParseAgentProfilesJSON` carries `logFormat` for extra profiles. The default
profile gets a new `agent-runner` flag `--agent-log-format`. Validation happens
in `NewProfileSet` alongside the existing argv-template checks: an unrecognised
value is accepted and resolves to passthrough, so a misconfigured profile
degrades to today's behaviour instead of refusing to start. `agent-runner` logs
a warning at startup naming the profile and the recognised values, so the
degradation is never silent.

### Component 3 — decode in `Process.Run`

The `scan` closure (`runner/process.go:146-161`) currently prefixes each line
and calls the sink. It gains a decoder parameter:

- stdout: each line goes through the decoder; each resulting event is rendered
  and published as `[out]<rendered>\n`.
- stderr: unchanged, always `[err]<line>` verbatim. Decoding stderr would
  suppress crash output.

A decoder that yields no events for a line (a JSON event type we deliberately
ignore) publishes nothing. Partial trailing lines — the reader returns bytes
with a non-nil error and no newline — are decoded as a final line so nothing is
lost at process exit.

### Component 4 — removing the oneshot stdin path

`Process.OnStdinWriter` and everything it required is deleted:
`stdinPipeW`/`stdinPipeR`, the `procDone` watcher goroutine, `watcherExitCode`,
`watcherDone`, the stdin-closer goroutine, and `isSyscallECHILD`. All of it
exists to keep `cmd.Wait` from deadlocking against the exec-internal
stdin-copier (`runner/process.go:76-143`); with `cmd.Stdin` nil there is no
copier and `cmd.Wait` reports the exit code directly.

`taskEntry.wakeWrite` stays, set only by `handleOpenExec`. `Session.WakeStdin`
stays, and its existing `e.wakeWrite == nil` guard
(`runner/session.go:779-782`) makes a wake aimed at a oneshot task a silent
no-op. The `TaskWake` protocol message and `server/agent_wake.go` are unchanged.

`runner/agentskills/harness-cli/SKILL.md` states that the runner writes a
synthetic wake prompt to the agent's stdin. That claim becomes true only for
interactive sessions and must be corrected in the embed source, with the
`.claude/skills` copy re-synced and the runner rebuilt.

### Component 5 — presets

`scripts/agent_presets.py` `KNOWN_AGENT_PRESETS`:

| name | bin | oneshotArgv | resumeOneshotArgv | logFormat |
| --- | --- | --- | --- | --- |
| claude | claude | `--output-format stream-json --verbose {args} -p {prompt}` | `--output-format stream-json --verbose {args} --continue -p {prompt}` | claude-stream-json |
| codex | codex | `exec --json {args} {prompt}` | `exec resume --last --json {args} {prompt}` | codex-jsonl |
| bash | bash | `{args} -c {prompt}` | `{args} -c {prompt}` | (unset) |

`resumeInteractiveArgv` is unchanged for all three: interactive sessions run
under a PTY and must keep their human-facing rendering.

Both new argv shapes were exercised against the installed CLIs before being
written here (claude 2.1.220, codex-cli 0.145.0): claude accepts
`--output-format stream-json --verbose` ahead of `-p`, and `--json` is an
option of the `codex exec resume` subcommand, so it is valid after `resume`.
Neither was inferred from help text alone.

`expand_agents_preset` emits `--agent-log-format` for the default profile and
`logFormat` inside the `--agent-profiles` JSON for extra profiles.
`.claude/commands/runner-up.md` references this table and must not restate the
argv strings.

### Data flow

```
agent stdout ──► Process.scan ──► Decoder.Decode ──► []Event ──► Render
                                                                   │
                             stderr ─────────────────── verbatim ──┤
                                                                   ▼
                                        sink ──► Sender.Publish(task.<id>.log)
                                                                   │
                          server tap ──► <LogsDir>/<id>.log        │
                                                                   ▼
                                       CLI logs / TUI pane / WebUI pane
                                                (unchanged)
```

### Component 6 — TUI history generation guard

```go
// tui/app.go
type App struct {
    // ... existing fields ...
    logsGen int // incremented by followTask; stamped into each GetTaskLog
}

// tui/events.go
type LogHistoryMsg struct {
    // ... existing fields ...
    Gen int
}

// tui/client.go
func DoGetTaskLogGen(c *cli.Client, taskID string, gen int) tea.Cmd
```

`followTask` increments `a.logsGen` before dispatching and passes the new
value. The `LogHistoryMsg` handler drops any message whose `Gen` differs from
`a.logsGen`, in addition to the existing task-id check. Responses from
superseded requests are discarded instead of being folded in.

`cmd/harness-tui/main.go:80-82` is removed. In its place, the `BindClientMsg`
handler (`tui/app.go:404-406`) re-follows the currently-followed task, so a
reconnect produces exactly one subscription owned by `a.logsCancel`.
`App.FollowingTaskID` remains for tests and callers.

## Testing

**Unit — decoders.** Real captured NDJSON from both CLIs, recorded while
writing this spec, checked in under `runner/agentlog/testdata/`
(`claude-stream-json.jsonl`, `codex-jsonl.jsonl`) with expected rendered output
as golden files. Cases: a tool call with its result, assistant text, thinking,
finish stats, a malformed line, an unknown event type, and a partial trailing
line.

**Unit — profile plumbing.** `ParseAgentProfilesJSON` carries `logFormat`;
`NewProfileSet` accepts an unknown value and resolves it to passthrough;
`--agent-log-format` reaches the default profile.

**Unit — TUI generation guard.** Following `tui/app_noclient_test.go`'s
`New(Config{})` + `Update(msg)` pattern: two `LogHistoryMsg` for the same task
with different `Gen` values must leave exactly one copy in the pane; matching
`Gen` must be applied.

**Python — presets.** `scripts/test_agent_presets.py` asserts the emitted flags
including `--agent-log-format` and the `logFormat` JSON field.

**E2E — local dummy server and runner.** Built from this worktree's own `bin/`,
not the live fleet, whose server runs a different build. Cover:

1. claude oneshot: a rendered progress line reaches the log topic strictly
   before `TaskFinished`, and the final assistant text is present.
2. codex oneshot: the task completes rather than hanging — the P2 regression.
3. bash profile: output is unchanged from today, byte for byte.
4. An operator override (`--claude-args '--output-format text'`) still produces
   a readable log via raw passthrough.

**Negative control.** Before landing, revert the decoder wiring alone and
confirm test 1 fails. A progress-visibility test that passes without the
feature is not a test.

**Interactive regression.** Interactive wake still works after Component 4:
open an interactive session, deliver an agentboard message, confirm the wake
marker reaches the PTY and the agent takes a turn. This is the mechanism the
oneshot deletion must not disturb.

## Verification before landing

- `make check`, `make wasm-check`, `go vet`, `go test` — explicit make targets,
  not ad-hoc `go build ./...`.
- `scripts/wire-skew-check.sh` — a no-op here since no `.bgn` changes, but safe
  to run unconditionally.
- Runner binaries rebuilt (`make build`) and the runner fleet restarted; a
  runner change is not live until `bin/` is refreshed. No server restart is
  required: nothing on the wire changes.
