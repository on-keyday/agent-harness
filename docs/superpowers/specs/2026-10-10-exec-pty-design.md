# `exec` under a PTY — design

## Problem

A remote-development editor's terminal does not work through the ssh gateway.
It opens, shows a cursor, and never prints a prompt.

The editor (Zed's SSH remote, read from `crates/remote/src/transport/ssh.rs`)
opens a terminal as a SEPARATE ssh connection: `ssh -t <host>` with the remote
command `cd <dir>; exec env … <shell> -l`. `-t` makes the client send `pty-req`
before `exec`. The editor's own protocol (a long-lived `ssh host cmd` speaking
length-prefixed protobuf over stdio) is unaffected; `exec tap` on it showed its
requests and replies crossing normally, `GetTerminalShell` among them.

The gateway accepts `pty-req`, records the size, and then runs the `exec` over
pipes (`cli/sshgw/session.go:115-123`, `:127-166`, `:240-257`). Neither
`ExecRunRequest` nor `RunnerExecRunRequest` has a way to ask for a terminal, and
the runner hard-codes `false, // no PTY` (`runner/exec_run.go:210`). A login
shell whose stdin is a pipe is non-interactive: no prompt, no echo, no line
editing. That is the screen the operator sees.

Every layer below the request already supports a PTY. objtrsf's
`ExecuteCommandWithOption` takes `ptyEnabled`, and resizes through a
`TerminalWindowSize` control frame on the same data stream — the path every
interactive task uses. The server's exec splice relays control frames
untouched (it scans frames for `exec tap` and counts only stdin / stdout /
stderr / synth payload).

## Decisions taken

**operator** = the human chose it in conversation; **this spec** = the author
chose it while writing — those rows are the ones worth a second look.

| # | Decision | Decided by |
| --- | --- | --- |
| D1 | An exec can run under a PTY: a `pty` bit on the request, honoured by the runner | operator |
| D2 | Surfaces: the ssh gateway (`pty-req` + `exec`), `harness-cli exec -t`, and an `io=` marker on `exec ls` in CLI / TUI / WebUI. No interactive PTY exec in the TUI or WebUI | operator |
| D3 | The initial size travels as the first `TerminalWindowSize` control frame after the data stream opens, as interactive sessions already do — not in the request | operator |
| D4 | `TERM` travels in the request and becomes the child's `TERM` | operator |
| D5 | No new capability. A PTY exec is gated by `exec_run` like any exec | this spec |
| D6 | `pty=1` with `stdin_enabled=0` is refused, as is a `term` without `pty`, and a `term` that is not 1–255 bytes of printable non-space ASCII | this spec |
| D7 | `exec -t` refuses when this process's stdin is not a terminal. There is no `-tt` | this spec |
| D8 | `exec -t` runs through objtrsf's `RemoteShell`, so **Ctrl+] ends the exec** — a change from the design as presented in conversation, which said "no detach key" | this spec |
| D9 | The gateway's PTY exec does NOT intercept Ctrl+]: the byte reaches the child, as it would under a real sshd | this spec |
| D10 | Under a PTY, stdout and stderr are one stream; `exec ls` shows `stderr` at 0 and nothing tries to separate them | this spec |

## Why the size is a frame and not a field (D3)

objtrsf starts the child and only then reads the stream, so the child runs for
a few milliseconds at the PTY's creation size before the first frame lands:
80×25 on Windows (`go-pty pty_windows.go:51`), and whatever `pty.New` leaves on
Unix — it sets none, so presumably the kernel's 0×0 (inferred, not measured).

Interactive tasks have lived with exactly this since they existed:
`applyInitialWindowSize` (`cli/initial_winsize.go`) sends one frame right after
the stream opens, the gateway's `shell` path does the same
(`cli/sshgw/session.go:298-305`), and `NewSessionMux` renders 80×24 until a size
arrives (`server/session_mux.go:284-291`). An interactive shell redraws on the
`SIGWINCH` the resize delivers.

What this costs: a program that reads its size ONCE at start and never handles
`SIGWINCH` renders at the wrong size. The case that motivates this work is
`bash -l`, which is not that program. A request field plus an objtrsf
`ExecuteOption` that sizes the PTY before `Start` is the fix if one turns up,
and nothing here forecloses it.

## Why `TERM` is a field (D4)

`pty-req` carries the client's `TERM`, and sshd puts it in the child's
environment. The gateway discards it today. Without it the child inherits the
RUNNER's `TERM`, which may be absent — and an agent with no `TERM` has been
observed rendering monochrome on this fleet. A fixed runner-side default
(`xterm-256color`) was the alternative; it can name a terminal the far end
does not implement.

`TERM` is applied only under a PTY, overriding whatever `BuildAgentEnv`
produced. A command line that sets its own (`exec env TERM=… sh`) still wins,
because it runs later.

## Wire

`ExecRunRequest` (client → server) and `RunnerExecRunRequest` (server → runner)
change identically. `pty` takes the next reserved bit, AFTER `sshd_parent` —
`shell_line`'s comment records what happened the one time a flag went first.
`term` is appended after the flag byte.

```
    stdin_enabled :u1
    shell_line :u1
    sshd_parent :u1
    # pty=1 runs the child under a pseudo-terminal: one output stream, the
    # child's stdin is the terminal, and TerminalWindowSize control frames on
    # the data stream resize it. Requires stdin_enabled=1.
    pty :u1
    reserved :u4
    # The child's TERM, applied only when pty=1. Empty = leave the
    # environment's TERM alone. Printable non-space ASCII only.
    term_len :u8
    term :[term_len]u8
```

`ExecRunInfo` (one `exec ls` row) gains the mode after `taps`:

```
    pty :u1        # the exec runs under a PTY (its stderr is always 0)
    reserved :u7
```

The server copies `pty` and `term` from one request to the other, next to
`SetShellLine` / `SetSshdParent` / `SetStdinEnabled` (`server/exec_run.go:151-153`),
and records `pty` on the `execRun` so `exec ls` can report it.

### Skew

`RunnerExecRunRequest` is reachable from `RunnerRequest`, so this is a format
the runner decodes: server and runners must both be restarted, and
`scripts/wire-skew-check.sh` — which builds both sides, starts them, and
asserts the skewed pair recovers — is run before landing. `ExecRunRequest` and
`ExecRunInfo` are client ↔ server; clients are restarted with the server, as
for every landing.

## Runner

`handleExecRun` (`runner/exec_run.go`):

1. Refuse, with `ExecEventKind_Failed` and a detail naming the rule, when
   `pty=1 && stdin_enabled=0`, when `term_len > 0 && pty=0`, or when `term`
   holds a byte outside `0x21..0x7e`. objtrsf already refuses the first
   (`exec: StdinDevNull with ptyEnabled`); refusing here gives the reason in the
   runner's words and does not depend on objtrsf's text.
2. When `pty=1` and `term` is non-empty, append `TERM=<term>` to `env` after
   `BuildAgentEnv` / `AgentCwdEnv`. The last duplicate wins on both platforms:
   go-pty starts the child through `os/exec` on Unix (`cmd_unix.go`), which
   dedups that way, and runs `dedupEnvCase` on Windows (`cmd_windows.go:134`).
3. Pass `req.Pty()` as `ptyEnabled`. The comment there ("no PTY: separate stdout
   and stderr is the point") becomes the rule for the default and names the
   exception.

`shell_line` and `sshd_parent` compose with `pty` unchanged: the shell line
becomes the PTY child's argv.

**Known limit.** objtrsf's PTY branch does not apply `KillProcessTree`. A kill
cancels the context, which kills the PTY's child (the shell); go-pty starts it
as a session leader with the PTY as its controlling terminal (`Setsid` +
`Setctty`, `cmd_unix.go`), so the kernel
hangs up the foreground process group when it exits. Background jobs that
ignore `SIGHUP` can outlive the exec. This is the interactive tasks' behaviour
too, and changing it is objtrsf work, not this spec's.

## Client (`cli.ExecRun`)

`ExecRunOpts` gains:

- `Pty bool` and `Term string` — set on the request. `cli.ExecRun` refuses
  `Pty` without `Stdin`, and `Term` without `Pty`, before sending anything (the
  `SshdParent`-needs-`ShellLine` check is the precedent).
- An initial size (rows, cols, and pixel width/height) sent as the first
  control frame once the data stream is visible, both-or-nothing as
  `applyInitialWindowSize` does.
- A way for a caller to resize later: a callback, invoked once with a resize
  function when the data stream is open (the `OnStarted` shape).
- A terminal mode for `exec -t`: instead of its own stdin/stdout pumps,
  `ExecRun` hands the stream to objtrsf's `RemoteShell`, which puts the local
  terminal in raw mode, sends the size, forwards `SIGWINCH` (polling on
  Windows), pumps, and writes the terminal reset on the way out. The outcome
  stream is then read exactly as today.

The exact names are the plan's to fix.

## ssh gateway (`cli/sshgw/session.go`)

When `exec` arrives after a successful `pty-req`:

- `runExec` sets `Pty`, `Term` (from the `pty-req`, which `parsePtyReq`
  decodes and currently discards — it keeps discarding it for `shell`, where
  the session's `TERM` is fixed), and the initial size if both rows and cols
  are non-zero.
- The goroutine that today refuses every request after `exec` handles
  `window-change` by resizing, when the exec has a PTY. Everything else is
  still refused.
- `Stderr` stays wired; under a PTY nothing arrives on it.

Without a `pty-req`, nothing changes. `shell` is not touched.

## CLI (`harness-cli exec -t`)

A `-t` flag on the `exec` row of `cli/verb/table.go`, `Flag.Surfaces = CLI`
with a `SurfaceReason`: the TUI and WebUI run an exec with no stdin path, so a
terminal has nothing to read from.

- `-t` with a stdin that is not a terminal is an error before dialing (D7).
  Today a terminal stdin is deliberately NOT forwarded (`cmd/harness-cli/exec.go`
  — "an interactive invocation that piped the terminal in would leave the child
  waiting on a tty nobody is typing into"); `-t` is the case where the child
  IS meant to read it.
- `TERM` is this process's `$TERM`; empty if unset.
- Ctrl-C under raw mode is a byte to the child, not a signal here — as with
  `ssh -t`. **Ctrl+] ends the exec** (D8): `RemoteShell`'s detach key
  half-closes the stream, the runner's input pump ends, and objtrsf sends the
  child `SIGHUP`. Writing a second terminal pump without the detach key would
  duplicate `RemoteShell`'s Windows handling (Win32 input mode, the Ctrl+Z read
  artefact) for one keystroke; `-t` documents the key instead.
- The exit code is the child's, as for every `exec`.

## Display

`exec ls` shows `io=pty` or `io=pipe` on every row — a mode with two values,
both printed, so a row never omits it (surface-parity item 31):

- CLI human row and `--json` (`"pty": true|false`), via `cli/exec_run.go`'s
  shared row builders.
- TUI execs modal and WebUI exec list, through the same snapshot row the
  traffic line uses (`cli/exec_snapshot_row.go`, the wasm conversion).

`exec tap` needs no change. Under a PTY its stdin records are keystrokes and
its stdout records include their echo; that is what crossed the harness.

## Capability and scope

No new capability (D5). A PTY exec runs the same commands, as the same user, in
the same worktree as a pipe exec; the terminal adds no reach. `TERM` is one
environment variable with a restricted alphabet and cannot introduce another.

## Surfaces

| Surface | Change |
| --- | --- |
| ssh gateway | `pty-req` + `exec` → PTY exec with size and `TERM`; `window-change` resizes |
| CLI `exec` | `-t` |
| CLI `exec ls` (row, `--json`) | `io=` / `"pty"` |
| TUI execs modal | `io=` |
| WebUI exec list | `io=` |
| TUI / WebUI `exec` command line | `-t` not offered (`Flag.Surfaces`, with reason) |
| `README.md`, `supervising-workers` skill | `exec -t`, and what a PTY exec gives up |

## Testing

- **Runner** (Linux): a PTY exec of `sh -c 'test -t 0 && echo tty'` prints
  `tty`; `term="xterm-test"` reaches the child as `$TERM`; a resize frame shows
  in `stty size`; each D6 refusal returns `Failed` with its reason; without
  `pty` the existing tests hold.
- **Server**: `pty` and `term` reach `RunnerExecRunRequest`; `exec ls` reports
  `pty`.
- **Gateway**: `pty-req` then `exec` opens a PTY exec carrying the size and
  `TERM`; `window-change` during it resizes; `exec` alone is unchanged.
- **CLI**: `-t` with a non-terminal stdin errors; `Pty` without `Stdin` and
  `Term` without `Pty` are refused by `ExecRun`; the `exec ls` row and JSON
  carry `io=`.
- **Live**, dummy harness: `ssh -t -p <port> <task>@127.0.0.1 'exec bash -l'`
  through a TUI-hosted gateway prints a prompt, echoes, and follows a resize;
  `harness-cli exec -t <task> -- bash -l` does the same; `exec ls` shows
  `io=pty` for both.
- `scripts/wire-skew-check.sh`.

## Non-goals

- An interactive PTY exec in the TUI or WebUI (D2).
- Sizing the PTY before the child starts (D3).
- Process-tree kill under a PTY (Runner, known limit).
- Signals other than through the terminal (`ssh` `signal` requests stay
  refused).
- A Windows-runner live check: the dummy harness is Linux. ConPTY is the path
  interactive tasks on Windows runners already take.
