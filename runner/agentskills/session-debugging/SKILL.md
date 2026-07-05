---
name: session-debugging
description: Use when observing or driving a live harness session's terminal from an agent — reading what a shell / TUI / REPL / claude session is showing, injecting keystrokes, running a shell command inside a live PTY, or diagnosing a worker that looks stuck or unresponsive (harness-cli session snapshot / send / exec).
---

# Debugging live sessions (snapshot / send / exec)

## Overview

You have no TTY, so `session attach` is not for you (it splices a real local
terminal to the remote PTY). The non-TTY trio gives you eyes and hands on any
live interactive session:

```bash
harness-cli session snapshot <id>            # SEE the current screen (read-only)
harness-cli session send -enter <id> <text>  # TYPE keystrokes (co-write, no takeover)
harness-cli session exec <id> <cmd>...       # RUN one shell cmd, wait, get output + exit code
```

All three authenticate with your task ticket's `exec_attach` capability — no
operator PSK. `snapshot` is a read-only `view` attach and never disturbs
whoever is driving; `send`/`exec` co-write into the live PTY without taking
over the human controller and without resizing. The human may be attached in
parallel — that is expected.

Get `<id>` from `harness-cli session ls` (interactive sessions) or `ls`
(all tasks) — pipe it into a variable; never hand-type a 32-hex id.

## Choosing the tool

- Foreground is a **POSIX shell** (bash/zsh/sh — incl. one reached over ssh or
  inside a netns) → **`exec`**: synchronous, greppable output, real exit code.
- Foreground is **anything else** (TUI, REPL, claude, full-screen app) →
  **`send` + `snapshot` loop**. `exec` on a non-shell foreground finds no
  completion marker and times out with a diagnostic — that timeout is your
  signal to switch.
- Target is a **claude worker you're coordinating** (tasks, corrections) →
  the **agentboard**, not the keyboard. Reserve send/snapshot for
  terminal-level inspection and unsticking (below).

## The drive loop (send → snapshot → assert)

Poll for the expected render instead of guessing a sleep:

```bash
harness-cli session send -enter "$ID" ./mytui
for i in $(seq 20); do
  harness-cli session snapshot "$ID" | grep -q 'Main Menu' && break
  sleep 0.5
done
harness-cli session snapshot "$ID"   # then read the state you asserted on
```

- **Verify INPUT, not just the render.** A correct-looking screen proves output
  plumbing, not input plumbing. After sending keys, assert the program
  *responded*: the screen changed, a cursor moved, or — in a shell — echo a
  nonce round-trip (`send -enter "$ID" echo pong-$RANDOM`, then snapshot for it).
- **Control keys** via `-e`, which interprets `\n \r \t \e \xHH \\`:
  `send -e "$ID" '\x03'` = Ctrl-C, `'\x1b'` = Esc, `'\x1b[A'`…`'\x1b[D'` =
  arrow keys, `'\t'` = Tab, `'\r'` = Enter.
- `-enter` appends a carriage return — a CR, so it submits on Windows cmd.exe
  too.

## Command reference

### `session snapshot [--style|--color|--raw] [--rows N --cols N] <id>`

Renders the session's current screen to plain text via a headless VT emulator
(`--rows/--cols` are a fallback if the session reports no size). Each snapshot
already waits `--settle-ms` (default 1500) collecting output before rendering —
factor that beat into poll loops.

- The plain render **drops SGR**, so a *faint* placeholder / ghost-autocomplete
  / dim hint looks identical to real input. **`--style`** prints a
  `--- styles ---` section listing faint/bold/etc. spans
  (`r<row> c<a>-<b> faint: "..."`) — an input-box line that shows up as `faint`
  is a placeholder, not something that was typed.
- **`--color`** additionally reports fg/bg as hex (`fg#ff87af: "Error: ..."` —
  error-red, status colors). Verbose (most cells carry a color), separate
  opt-in. CJK/wide runs are coalesced, not split per character.
- **`--raw`** instead writes the verbatim PTY replay bytes (escapes intact) —
  `cat` it into a real terminal to reproduce the screen exactly, or inspect the
  actual bytes when the rendered text looks wrong. Not combinable with
  `--style`/`--color`; `--rows/--cols` are ignored.
- The grid is **width-wrapped**: a long line (a SID, a URL) is split across
  rows, so a grep can miss it. For greppable logical lines use `exec` (below)
  or `--raw`.

### `session send [-enter] [-e] [--flush-ms MS] <id> <text>...`

Injects keystrokes via a `cowrite` attach. **Flags go BEFORE `<id>`;
everything after `<id>` is the text**, joined ssh-style
(`send -enter <id> echo hello world` sends `echo hello world` — no quoting
needed; quote as one argument to preserve exact whitespace). A `-enter` placed
AFTER the text is taken as literal text — you will see it *typed* in the
snapshot instead of submitting.

### `session exec [--timeout D] [--json] [--exit-only] [--raw] <id> <cmd>...`

The synchronous shortcut for send + snapshot + guess-a-sleep when the
foreground is a POSIX shell. Injects the command (flags before `<id>`, rest
joined ssh-style), WAITS (default `--timeout 30s`), and returns the command's
**combined stdout+stderr**
as logical lines — SGR stripped, `\r`-overwrite/erase-line applied, NOT
re-wrapped — plus its exit code. The `exec` process exits with the command's
exit code (124 timeout, 125 error, 126 the foreground shell exited), so it
composes:

```bash
if harness-cli session exec "$ID" test -f /tmp/flag; then …; fi
harness-cli session exec --timeout 60s "$ID" 'make test 2>&1 | tail -20'
harness-cli session exec --json "$ID" ps aux   # {exit,output,timed_out,shell_exited,duration_ms}
```

Footguns:

1. It types into the **LIVE foreground shell**: state persists across calls
   (`cd`/`export` carry over) AND shell-terminating commands bite — a bare
   `exit`/`exec` ends the shell and KILLS the session. To test an exit code,
   wrap it: `(exit N)` or `bash -c 'exit N'`.
2. stdout/stderr can't be separated (one PTY).
3. Single logical line only (`;` `&&` `|` `$()` compose fine; a literal
   newline is rejected).
4. Non-POSIX-shell foreground (a claude/REPL prompt) → no completion marker →
   timeout with a diagnostic (exit 124). Use send/snapshot there. The typed
   bytes are NOT rolled back — the command text has already landed in the
   foreground program as input (a REPL will show it as a syntax error);
   clear the line (`send -e <id> '\x03'`) before driving on.

### Flag ordering

`snapshot` / `attach` flags are order-free (their only positional is the
`<id>`). `send` / `exec` are NOT — their text/cmd is free-form, so flags must
stay before `<id>`.

## Diagnosing a stuck claude worker

`snapshot` first — the screen tells you which case you're in:

- **Permission prompt** (worker spawned without auto mode) → answer it:
  `send -enter "$ID" 1` (or the option shown). For the future, respawn with
  `--agent-arg --permission-mode --agent-arg auto`.
- **Menu / "resume" style prompt** → drive it with arrows + Enter via
  `send -e`.
- **Spinner / "thinking"** → not stuck; long autonomous stretches are normal.
  Poll snapshot; don't interrupt mid-think.
- **Runaway turn you must interrupt** → `send -e "$ID" '\x1b'` (Esc).

Unsticking the terminal is keyboard work; handing the worker new instructions
afterwards still belongs on the agentboard.

## Parsing raw PTY bytes

A PTY echoes an injected Enter as a **bare `\r` with no LF**. When grepping
`--raw` output or captured PTY bytes, treat `\r`, `\n`, and buffer edges all
as line boundaries — matching on `\n`-terminated lines alone misses markers.
(The rendered `snapshot`/`exec` outputs already normalize this.)

## Common mistakes

| Symptom | Cause → fix |
|---------|-------------|
| `-enter` shows up typed on screen | Flag placed after `<id>` → flags before `<id>` |
| `exec` times out on a REPL/TUI/claude | Foreground isn't a POSIX shell → send + snapshot |
| Session died after an `exec` | Bare `exit`/`exec` killed the shell → `(exit N)` |
| Snapshot shows "input" nobody sent | Faint placeholder/ghost text → confirm with `--style` |
| grep misses a long line in snapshot | Width-wrapped grid → `exec` (logical lines) or `--raw` |
| Screen unchanged after `send` | Render lag (poll longer) or input plumbing broken → nonce echo round-trip |
| Screen looks garbled in snapshot | Render artifact vs real bytes → `--raw` and inspect escapes |
