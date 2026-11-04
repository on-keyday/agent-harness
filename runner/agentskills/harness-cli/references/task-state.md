# What `ls` says a task is doing

Read with "Finding other agents" in the harness-cli SKILL.md.

## `Detached` does not mean nobody is there

**`Detached` does not mean nobody is there.** Running vs Detached tracks ONE
thing: whether a *control* attach holds the writer slot. A read-only viewer or
an input-forwarding cowriter — the WebUI preview, a TUI grid pane — does not
take that slot, so a session three people are watching still reads `Detached`.
The `cowrite=N viewer=N` pair is what actually says who is on it. It is printed
on every row that HAS a session, zeros included — `cowrite=0 viewer=0` means
nobody is attached, and it is never dropped just because it is zero, so you
never have to wonder whether a row is silent or empty. (`viewers` / `cowriters`
in `--json`, likewise always present.) Do not read
`Detached` as "abandoned", and do not read it as "dead" either: the task is
alive and its agent may be mid-turn — that is what `act=` is for.

## What `ls` says a task is doing (`lifecycle=`, `program=`)

Two more fields sit beside `progress=` on a live task's row. Neither is
yours to write.

- **`lifecycle=<state> (<age>)`** is written by the hooks the runner injects into
  `.claude/settings.json` and `.codex/hooks.json`. Those hooks run
  `harness-cli lifecycle --hook claude <state>` or
  `harness-cli lifecycle --hook codex <state>` on the respective runtime events.
  The CLI reads each event from the hook's stdin, and it keeps a task `working` while a subagent it launched is still
  running. **Do not call `harness-cli lifecycle` yourself**: the
  next hook overwrites it, and in the meantime your row says something your
  runtime did not.
  - `working`: a turn is running.
  - `waiting`: blocked on a human inside the turn, on a permission prompt or a
    question.
  - `idle`: the turn ended and the task waits for its next prompt.
  - `error`: Claude Code reported `StopFailure` (API or model error). Codex
    has no equivalent hook in this configuration.
  - `-`: no hook has reported. Possible causes include an unsupported runtime,
    untrusted Codex hooks, a `--no-worktree` task without injected settings, or
    a runner started before the hooks existed. It says nothing about whether
    the task is busy; read `act=` for that.
- **`program=<state>[:<kind>]`** is the program's own report (OSC 7501), read
  off its terminal. It is shown only while the task has a live terminal.

The two come from different sources and are not reconciled, so they can
disagree. An Esc while a tool runs ends the turn and reads `idle`. An Esc
while the model is still thinking fires no Claude hook, so that row stays
`working`; Codex reports `Interrupt` and returns to `idle` unless a subagent
is still running.
Read it against `act=` (output age) before concluding a worker is busy. `ls --json`
carries both fields on every row, at zero when never reported: `lifecycle`,
`lifecycle_at`, `lifecycle_age_ms`, `program_state`, `program_kind`,
`program_msg`.
