# `harness-cli session exec` — synchronous command execution over a session PTY

**Date:** 2026-07-01
**Status:** Design approved, pending spec review
**Origin:** Field feedback from the sdplane verification agent (agentboard task `c5dec23f`). Top-priority ask: a synchronous exec that returns structured stdout/stderr/exit-code, eliminating the "guess a `sleep`, re-`snapshot`, `sed`-slice a `###MARK###`" loop that dominated repeated real-hardware verification.

## Problem

The current session-driving primitives are asynchronous and screen-oriented:

- `session send` (cowrite attach) injects keystrokes but has no completion signal.
- `session snapshot` (view attach) renders the whole PTY screen; a specific command's output must be sliced out by hand, and the VT grid render soft-wraps long logical lines (col ~200), splitting tokens like a SID across a wrap and defeating `grep`.

So the operator/agent has to inject a command, guess how long it runs, snapshot, and scrape. Every verification step pays this tax.

## Key constraint that shapes the whole design

The foreground of a session PTY is **not** necessarily a local process. In the originating work the agent typed `ssh` into the child PTY and then drove a **remote** shell reached over that ssh connection (and, in other runs, netns subshells). The real shell can sit an arbitrary number of nesting hops away: `local bash → ssh → remote bash → nsenter/netns subshell → …`.

Consequence: **the command must travel through the foreground PTY.** Any "run it as a separate runner-side process" model (a fresh `exec`, `nsenter --target <pid>`, a second PTY) is fundamentally incapable of reaching a shell that only exists on the far side of an interactive ssh connection. `nsenter` cannot cross an SSH boundary. Therefore the only viable execution model is **inject into the foreground PTY and read the bytes that come back** — which is transport-agnostic (works identically for a local shell, an ssh-remote shell, or a netns subshell, because it only relies on bytes flowing back).

This also means: **stdout and stderr cannot be separated.** A PTY carries one interleaved byte stream, and over ssh there is only one stream anyway. `exec` returns *combined* output plus an exact exit code. The original ask named "stdout/stderr/exit code"; the honest, correct deliverable on a PTY is **combined-output + exit-code**.

## Execution model (approved)

`session exec` is a **client-side orchestration of the existing cowrite attach**. The cowrite attach is already bidirectional — it forwards injected input *and* drains the session's output stream — so no new protocol message, no server handler, and no runner change are required. `protocol/message.bgn`, `server/`, and `runner/` are untouched. This is the toy-scope-appropriate implementation: the transport already does everything needed.

### Mechanism

1. Generate a random `nonce`; derive two sentinels `__HEXEC_<nonce>_S__` (start) and `__HEXEC_<nonce>_E__` (end).
2. Over a single cowrite attach, inject **one physical line**:
   ```sh
   printf '__HEXEC_<nonce>_S__\n'; <cmd>; printf '__HEXEC_<nonce>_E__%s\n' "$?"
   ```
   - It is one physical line (terminated by a single `\r`), so it submits as one shell command list regardless of how deeply nested the foreground shell is. `<cmd>` may itself compose with `;`, `&&`, `||`, `|`, `$(...)`.
   - The trailing `printf` runs as the next element of the list, so `"$?"` is `<cmd>`'s exit status — the exit status of the shell that is *currently in the foreground* (local, ssh-remote, whatever), which is exactly what we want.
3. Read the cowrite output stream, accumulating into a buffer, until a line matching `^__HEXEC_<nonce>_E__(\d+)\r?$` appears. This content match is the **synchronous completion signal** — no `sleep` guessing.
4. Slice the buffer to the region strictly between the *output* occurrence of `__HEXEC_<nonce>_S__` (a line that is exactly the start sentinel, not the echoed `printf '...'` input line) and the end-sentinel line. That region is the pure command output.
5. Interpret the sliced region into the plain text a terminal would have displayed, then emit it line by line. Concretely: strip SGR (color/style) sequences; apply carriage-return (`\r`) overwrites (a `\r` resets to column 0, so text after it overwrites the earlier text on that line — the collapsed final text is kept); apply erase-line sequences (`ESC[K`, `ESC[2K`); keep `\n` as a logical line break; and **impose no terminal width, so nothing is re-wrapped**. Because a PTY does not insert a newline byte at a soft-wrap, not imposing a width means long logical lines stay intact — directly fixing the "wrapped SID defeats grep" pain. This is a line-oriented interpretation of the byte stream, not a fixed-width VT grid render (which is what `snapshot` does and what causes the soft-wrap splitting).
6. Parse the end-sentinel's digits as the exit code.

### Why dual sentinels

An end-only sentinel forces fragile stripping of the command echo. Bounding the output with both a start and an end sentinel means the pure output is exactly the bytes between the two *output* sentinel lines. The echoed input line (which contains `printf '__HEXEC_<nonce>_S__\n'; …`) does not match the anchored `^…$` sentinel-only pattern, so it is never mistaken for a boundary. `nonce` randomness prevents collision with a `PS1` or program output that happens to contain a similar string.

### Frame-spanning sentinels (implementation note)

The cowrite output arrives in PTY frames whose boundaries are arbitrary; a sentinel can be split across two frames. The reader MUST match against the accumulated buffer, never per-frame. This is the single most error-prone part of the implementation and gets dedicated unit coverage.

## Multi-line scope (approved: single logical line for v1)

Bracketed paste mode (`ESC[200~ … ESC[201~`) is the wrong tool here: its *purpose* is to make a pasted block **not** auto-execute embedded newlines — which is very likely the root cause of the peer's "heredoc block sits at the prompt unexecuted" report. So `exec` does not use it.

- **v1: a single logical command line.** Shell composition (`;`, `&&`, `||`, `|`, `$(...)`) is fully available, covering the large majority of verification commands and formalizing the `;`-join the peer already fell back to.
- **Deferred (v2): `--script`** reads a multi-line script from stdin and injects it as a base64 one-liner (`printf %s '<b64>' | base64 -d | bash`) — transport-agnostic but adds a `base64`+`bash` dependency on the target. Out of scope for v1 (YAGNI).

## Non-POSIX-shell foreground (approved: detect via timeout)

The sentinel mechanism assumes the foreground is a POSIX-ish shell where `printf` and `$?` work (bash/zsh/dash/…). If the foreground is a non-shell REPL (sdplane's own console, a `claude` session, a Python REPL), the injected line does not produce the end sentinel, so the read times out. On timeout with no sentinel seen, `exec` returns a clear diagnostic:

> no completion sentinel observed within `<timeout>`; the session foreground may not be a POSIX shell (`exec` requires bash/zsh/sh). Use `session send` + `session snapshot` instead.

No pre-probe is performed (the chosen option); the timeout path is the detection.

## CLI surface

```
harness-cli session exec [flags] <task-id> <cmd>...
```

Flags (placed **before** `<task-id>`, consistent with the `send`/`snapshot` ordering convention; everything after `<task-id>` is the command, ssh-joined like `session send`):

| Flag | Default | Meaning |
|------|---------|---------|
| `--timeout DUR` | `30s` | Max wait for the end sentinel. On expiry, return partial output flagged `timed_out` and exit `124`. |
| `--json` | off | Emit `{"exit":N,"output":"…","timed_out":bool,"duration_ms":N}` — the structured form the feedback asked for. |
| `--exit-only` | off | Suppress captured output; report only the exit code. |
| `--raw` | off | Skip the interpretation in step 5; return the verbatim bytes of the command-output region. Parity with `snapshot --raw`, but sliced to just this command's output rather than the whole screen. |

Behavioral contract:

- The `session exec` **process exits with the remote command's exit code**, so it composes in shell: `if harness-cli session exec <id> test -f /flag; then …`. Timeout exits `124` (matching `timeout(1)`). Internal/transport errors exit `125`. A non-POSIX/no-sentinel timeout is the `124` path with the diagnostic above on stderr.
- Human-readable (non-`--json`) mode prints the cleaned combined output to stdout; the exit code is conveyed via the process exit status (and stderr note on failure), not mixed into stdout.

## Surfaces / reuse

- Implemented as `cli.SessionExec(ctx, taskIDHex, cmd, opts)` plus a `cli.SessionExecWith(client, …)` variant that takes a caller-provided long-lived `*cli.Client`, mirroring the existing `SessionSend` structure (`cli/cowrite_native.go`) and honoring the "reuse the long-lived client" rule.
- `exec` is a CLI/programmatic primitive in the same family as `send`/`snapshot` (non-TTY callers: agents, scripts). A human on the TUI/WebUI already has a live interactive terminal, so a synchronous-exec-return has no distinct human UI affordance there; `SessionExecWith(client)` is nonetheless exported so a future TUI/WebUI caller could reuse it. This is the one deliberate deviation from "every feature spans all three UIs," and it is functional, not an omission.

## Foreground shell exiting mid-command (detected, distinct from timeout)

If the command terminates the foreground shell itself — it runs `exit`, `exec`,
kills its own shell, etc. — the final sentinel `printf` never runs and the PTY
stream closes. The session then goes terminal (a later exec gets
`already_terminal`). exec distinguishes this from a real timeout by *how* the
read ended: the reader returning on stream-close (no sentinel) → `ShellExited`
(exit 126, "the foreground shell exited … did the command run exit/exec?");
the timer firing while the reader still runs → `TimedOut` (exit 124). Reporting
a shell-death as a timeout would misdirect diagnosis, so the two are separate
result flags and separate exit codes. Note: interactive bash prints its own
`exit` farewell line as it leaves, so it may appear in the partial output — that
is bash's message, not a sentinel leak. (v1 deliberately does NOT reject
exit/exec or subshell-wrap the command: subshell wrapping would break the cd/env
persistence that sharing one live foreground shell provides.)

## Known limitations (documented, not solved in v1)

- **Combined output only** — stdout/stderr are not separable on a PTY (see the key constraint). Inherent.
- **Serial-PTY concurrency (#6 from the feedback)** — `exec` briefly co-writes to the shared PTY. If a human is simultaneously typing into the same session, keystrokes interleave. Same constraint the existing `send` already has; not addressed in v1. Documented: do not `exec` against a session a human is actively driving.
- **Injected `printf` is visible** — the sentinel `printf`s appear in the session's scrollback/screen, exactly as a manually typed marker would. Cosmetic.
- **Foreground must be a POSIX shell** — otherwise the timeout diagnostic fires (above).

## Testing

- **Parser unit tests** (pure, on canned byte streams): start/end sentinel slicing; SGR stripping; carriage-return (`\r`) overwrite collapsed to the final text; erase-line (`ESC[K`/`ESC[2K`) applied; logical lines preserved with no re-wrap; sentinel split across frame boundaries; `CRLF` vs `LF`; echoed-input line not mistaken for a boundary; exit-code extraction incl. multi-digit and `130` (signal) cases.
- **Integration tests** against a live `bash` runner session (a cheap PTY, per the existing snapshot/verify pattern): `echo hi` → output `hi`, exit `0`; `false` → exit `1`; a command emitting ANSI color → color stripped, text intact; `sleep 5` with `--timeout 1s` → exit `124`, `timed_out` set; a wide single logical line → returned un-wrapped.

## Out of scope for v1

- `--script` / multi-line injection (v2).
- stdout/stderr separation (impossible on a PTY; not revisited).
- Any protocol/server/runner change (the design deliberately requires none).
- A locking/arbitration mechanism for concurrent human+exec drivers (#6).
