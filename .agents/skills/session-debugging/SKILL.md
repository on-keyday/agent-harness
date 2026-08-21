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
harness-cli session resize --size 40x150 <id>  # SIZE the PTY (no takeover)
```

All three authenticate with your task ticket — no operator PSK — but they do
**not** need the same capability, because they are not the same power:

| tool | attach mode | capability |
|------|-------------|------------|
| `snapshot` | `view` | `exec_view` |
| `send` / `exec` | `cowrite` | `exec_cowrite` |
| `resize` | `view` | `exec_view` **+ `exec_resize`** |

`snapshot` is a read-only attach and never disturbs whoever is driving;
`send`/`exec` co-write into the live PTY without taking over the human
controller and without resizing. The human may be attached in parallel — that
is expected.

The caps are checked with implication (each accepts itself or anything
stronger), so `exec_cowrite` alone covers all three and `exec_control` covers
everything. If you were granted only `exec_view` you can look but not type —
that is a deliberate grant, not a misconfiguration: ask your spawner for
`exec_cowrite` rather than assuming it was an oversight.

Get `<id>` from `harness-cli session ls` (interactive sessions) or `ls`
(all tasks) — pipe it into a variable; never hand-type a 32-hex id.

## Choosing the tool

- Foreground is a **POSIX shell** (bash/zsh/sh — incl. one reached over ssh or
  inside a netns) → **`exec`**: synchronous, greppable output, real exit code.
- Foreground is **anything else** (TUI, REPL, claude, full-screen app) →
  **`send` + `snapshot` loop**. `exec` on a non-shell foreground finds no
  completion marker and times out with a diagnostic — that timeout is your
  signal to switch.
- Target is a **claude worker you're coordinating** → the **agentboard** for
  anything expressible as prompt text (tasks, corrections, questions), and the
  keyboard ONLY for what is not: **client-level control** — `/clear`,
  `/compact` and other slash commands, an Esc interrupt, a permission-prompt
  answer, a menu selection (see "Diagnosing a stuck claude worker" below). An
  agentboard payload reaches a claude peer as prompt *text*: the runner types
  only the fixed `<harness:agentboard-wake>` marker into the PTY, and the
  payload arrives through the `UserPromptSubmit` inbox hook. So `agent send
  --data '/clear'` clears nothing — the worker reads the two words as content.
  That is structural, not a gap to route around.
- **Completion is signalled per channel too.** An agentboard instruction ends
  with the peer's reply; a keystroke-level one has no reply coming, so its only
  edge is PTY quiescence (`session await-idle` — see `harness-cli skill
  supervising-workers`). Do not arm an idle watcher for a peer you already
  asked to report back on the agentboard.

## The drive loop (send → snapshot → assert)

For a single step, `--snapshot` does both halves in one call — it sends, lets
the input drain, then renders the screen to stdout (the summary stays on
stderr, so a pipe gets only the screen):

```bash
harness-cli session send -enter --snapshot "$ID" 'echo pong-$RANDOM'
harness-cli session send -e --snapshot --style "$ID" '\x1b[B'   # ↓, then look
```

`--settle-ms` (default 1500) is the window the program gets to react before the
render; `--rows/--cols/--style/--color/--json` mean what they mean on
`session snapshot`, and all six are refused without `--snapshot` rather than
silently ignored.

When you are waiting for something rather than reading one result, poll for the
expected render instead of guessing a sleep:

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
- **Give an agent TUI its Enter in a SECOND call.** Whether a CR arriving in
  the same burst as the text submits is the foreground program's decision, not
  the harness's — the harness is a byte pipe that neither inspects nor alters
  it. It differs between agents, and it has changed between versions of the
  same agent, so there is nothing here worth learning as a fact: the two-call
  form is correct either way and survives the next upgrade. `send <id> <text>`
  (no `-enter`), snapshot to confirm the text is in the box, then
  `send -e <id> '\r'` separately. Plain shells and classic line-editors are
  fine with `-enter` in one call.
- **One call per keypress in a TUI.** Printable runes that arrive in a single
  write are delivered to the program as ONE key event, so `send <id> 'jjj'`
  reaches a TUI as the single key `"jjj"`, which matches no binding: the cursor
  does not move three rows, it does not move at all (verified live against a
  bubbletea table). Repeat the call per keypress instead of batching. This is
  not a harness artifact — key-repeat and paste batch the same way, which is
  why a widget that means to accept a burst has to read it rune by rune, and
  not every widget does. A shell is unaffected: it consumes the whole write as
  typed text.

## Command reference

### `session snapshot [--style] [--color] [--json|--raw] [--rows N --cols N] <id>`

Renders the session's current screen to plain text via a headless VT emulator
(`--rows/--cols` are a fallback if the session reports no size). Note they only
affect *rendering*: a session started without an attached terminal may have no
size at all, and a full-screen program in it refuses to draw ("terminal too
small" — codex and agy paint nothing whatsoever at 0x0).

Each snapshot already waits `--settle-ms` (default 1500) collecting output
before rendering — factor that beat into poll loops.

**`--rows/--cols` here are NOT the same flags as on `session new`.** Same
spelling, different subject: on `snapshot` they size the offscreen renderer and
never touch the PTY; on `session new` they size the PTY itself. To actually
give a session a size, use **`session resize`**:

```bash
harness-cli session resize --size 40x150 "$ID"      # standalone
harness-cli session send -enter --resize 40x150 "$ID" ./mytui   # size, THEN type
```

`--resize` on `send` applies before the text, which matters: a full-screen
program that receives keystrokes while it is still 0x0 paints nothing, and that
reads like the send having failed.

It needs `exec_resize` on top of `exec_view`, and applies only while **no
control client is attached** — the size belongs to the control seat whenever
someone holds it. Both refusals look the same from here (the server discards
the frame either way), so the command reports "not applied" and **exits 3**
rather than pretending; do not treat a silent success as proof.

Sizing at open (`session new -d --rows 40 --cols 150`) is still the route
always available to the spawner and needs no extra capability.

For a running session whose agent is a shell, `session exec <id> 'stty rows 40
cols 140'` also works with only `exec_cowrite` — it types the resize rather
than claiming it, so it is unaffected by who holds the seat, but it only works
when the foreground IS a shell.

- The plain render **drops SGR**, so a *faint* placeholder / ghost-autocomplete
  / dim hint looks identical to real input. **`--style`** prints a
  `--- styles ---` section listing faint/bold/etc. spans
  (`r<row> c<a>-<b> faint: "..."`) — an input-box line that shows up as `faint`
  is a placeholder, not something that was typed.
- The same blindness hides **which pane has focus and which row is selected** in
  a full-screen TUI: both are drawn with SGR (usually `reverse`), so the plain
  text is identical whether or not anything is selected. Before sending Tab or
  Enter into a TUI, find the `reverse` span in `--style` and see where the
  cursor actually is. Guessing costs more keystrokes than looking, and a wrong
  guess can move focus AWAY from the panel you wanted.
- **`--color`** additionally reports fg/bg as hex (`fg#ff87af: "Error: ..."` —
  error-red, status colors). Verbose (most cells carry a color), separate
  opt-in. CJK/wide runs are coalesced, not split per character.
- **`--json`** emits one object instead of text — same screen, different
  encoding: `{task, rows, cols, attrs, color, lines[], spans[]}`. `lines` is the
  grid one row per entry and is always exactly `rows` long, and each span is
  `{row, start, end, attrs[], fg, bg, text}`, so `lines[span.row]` gives you the
  row a span sits on and you never parse the `--- styles ---` text. `attrs` and
  `color` report which style dimensions were COLLECTED, which is what separates
  "`spans` is empty because I did not ask" from "asked, and the screen has
  none" — spans stay empty unless you also pass `--style`/`--color`.
  `start`/`end` are grid COLUMNS, not offsets into that string: they coincide
  with character positions only while every earlier cell on the row is
  single-width, so slice by them at your own risk on a line holding CJK or
  emoji, and use the span's own `text` instead.
- **`--raw`** instead writes the verbatim PTY replay bytes (escapes intact) —
  `cat` it into a real terminal to reproduce the screen exactly, or inspect the
  actual bytes when the rendered text looks wrong. Not combinable with
  `--style`/`--color`/`--json` (those describe the VT render; this is a
  different artifact); `--rows/--cols` are ignored.
- The grid is **width-wrapped**: a long line (a SID, a URL) is split across
  rows, so a grep can miss it. `--json` does NOT fix this — `lines` is the same
  wrapped grid. For greppable logical lines use `exec` (below) or `--raw`.

### `session send [-enter] [-e] [--flush-ms MS] [--snapshot [--rows N] [--cols N] [--settle-ms MS] [--style] [--color] [--json]] <id> <text>...`

Injects keystrokes via a `cowrite` attach. **Flags go BEFORE `<id>`;
everything after `<id>` is the text**, joined ssh-style
(`send -enter <id> echo hello world` sends `echo hello world` — no quoting
needed; quote as one argument to preserve exact whitespace). A `-enter` placed
AFTER the text is taken as literal text — you will see it *typed* in the
snapshot instead of submitting.

`--snapshot` renders the screen after sending, on the same connection: one call
instead of send + snapshot + a guessed sleep. The screen goes to **stdout** and
the "N bytes sent" summary to **stderr**, so a pipe gets only the screen, and
`-quiet` (which drops that summary) composes with it. The six snapshot-only
flags are refused without `--snapshot` instead of being ignored. They carry
`session snapshot`'s meanings unchanged, `--json` included, so one drive step
can return structured screen data without a second dial.

### `session resize --size ROWSxCOLS [--wait-ms MS] [-quiet] <id>`

Sets the PTY size from a view attach — no takeover, the controlling client (if
any) keeps its stream. Needs `exec_view` + `exec_resize`.

The acknowledgement is real, not assumed: an accepted size is fanned back out
to every observer including the sender, so the command waits for its own size
to come back (`--wait-ms`, default 2000). No echo → **exit 3** and a message
naming both possible reasons. `--size` is ONE flag spelled `ROWSxCOLS`
deliberately — `--rows/--cols` already mean two other things nearby (the
offscreen render on `snapshot`, the PTY on `session new`).

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
  `send "$ID" 1` (menu digits usually act alone; if it still sits there,
  follow with `send -e "$ID" '\r'` as its own call — see the delayed-Enter
  note above for why the CR goes separately). For the future, respawn with
  `--agent-arg --permission-mode --agent-arg auto`.
- **Menu / "resume" style prompt** → drive it with arrows + Enter via
  `send -e`.
- **Spinner / "thinking"** → not stuck; long autonomous stretches are normal.
  Poll snapshot; don't interrupt mid-think.
- **Runaway turn you must interrupt** → `send -e "$ID" '\x1b'` (Esc).

Unsticking the terminal is keyboard work; handing the worker new instructions
afterwards still belongs on the agentboard.

### After a keystroke-level reset

`/clear` and `/compact` leave the worker with no memory of what you agreed on,
while its retained inbound messages stay on the board. Re-brief it over the
agentboard afterwards — and note that an instruction it already carried out
will be re-read as pending unless you retracted it (`harness-cli skill
harness-cli` → "Withdrawing a message you sent").

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
| Regex-parsing the `--- styles ---` report | It is a projection, not the source → `--json --style` and read `spans[]` |
| grep misses a long line in snapshot | Width-wrapped grid → `exec` (logical lines) or `--raw` |
| Text sits in the input box, never submits | THIS agent/version ignores a same-burst CR (they differ, and change) → send text, then `send -e <id> '\r'` separately |
| Screen unchanged after `send` | Render lag (poll longer) or input plumbing broken → nonce echo round-trip |
| A repeated key in one `send` does nothing | Runes in one write arrive as ONE key event (`"jjj"` matches no binding) → one call per keypress |
| Screen looks garbled in snapshot | Render artifact vs real bytes → `--raw` and inspect escapes |
| `/clear` sent over the agentboard did nothing | Payload lands as prompt text, never as a slash command → `session send` |
| `await-idle` woke you with nothing to do | The peer's agentboard reply was already the completion edge → don't arm one for a peer you asked to report back |
