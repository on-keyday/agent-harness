# TUI: the X11 tunnel outlives a detach, and one key reopens a task with X11

Status: draft, awaiting the operator's approval (2026-11-25). Small change;
TUI only.

## Why

Two costs in today's X11 forwarding, both hit by the TUI workflow where
Ctrl+] is an everyday keystroke:

1. **A detach kills every GUI program in the session.** The -R forward that
   carries X11 runs under a context the detach cancels: the TUI calls
   `a.x11Cancel()` on `InteractiveDoneMsg` (`tui/app.go`), the forward's control
   stream closes, the server tells the runner to stop listening on
   `127.0.0.1:6000+N`, and the spliced connections end with it (they run under
   the same ctx, `cli/port_forward.go` `dialAndSplice`). Xlib's default IO
   error handler exits the client, so an open window dies on detach. DISPLAY
   stays set, so a GUI started after reattaching fails to connect.
2. **X11 can only be had at `session new`.** A session opened without
   `--x11` (the usual case) gets X only by typing
   `session new --x11 --resume <id> --resume-conversation`.

## Goals

- In the TUI, an X11 tunnel lives until its task ends or the TUI exits, not
  until the next detach.
- One TUI key gives the selected task an X11 session, whatever its state.

## Non-goals

- Adding X11 to a running agent process without restarting it. The agent's
  environment is fixed at spawn (`runner/agentenv.go`); the only way around
  that is to reserve a display for every session (approach C below), which
  was declined: X11 is occasional.
- The CLI. `harness-cli` exits on detach, so it has no process to hold the
  tunnel; `session new --x11` keeps today's behaviour, and the README says so.
- The WebUI (no local X server for the tunnel to end at; the verb table
  already records this as `x11SurfaceReason`).
- Re-sending a cookie. The runner's xauth file keeps the first cookie for the
  session's life; a reattach from a different X server is out of scope.

## Approaches

| | What | Verdict |
|---|---|---|
| A | `session attach --x11 [--x11-display N]` re-opens the tunnel on attach | Rejected: a TUI key reattach (r/R) would not carry it, and a wrong N breaks silently |
| B | The server records each task's X11 display; every Control attach re-opens the tunnel automatically | Rejected for now: a `.bgn` field to recover from a detach that need not tear anything down. Still the answer for "TUI restarted", if that ever matters |
| C | Every PTY session gets `HARNESS_X11_DISPLAY` / `HARNESS_X11_XAUTHORITY` at spawn; an attach later fills the cookie and opens the tunnel | Rejected: wire change and per-runner display allocation, for an occasional feature |
| **D** | **The TUI owns the tunnel past a detach; one key resumes the task with X11, restarting a live one after a confirm** | **Chosen**: client only, no wire change |

## Design

### Tunnel ownership

- `App.x11Cancel` (one field) becomes a map keyed by task id. A detach
  (`InteractiveDoneMsg`) no longer cancels the forward.
- A forward stops when its task leaves the live states (the tasks table
  already receives the status), when its forward reports closed (the runner's
  `TaskGone` / `RunnerGone`, or `x` in the `f` forwards pane), or when the TUI
  exits.
- The display number: today every `--x11` open defaults to 10, so a second
  X11 task on the same runner cannot bind `6010`. The TUI picks the lowest N
  from 10 that none of its own live tunnels on that runner uses. Another
  client's tunnel on the same runner is not visible to it; a bind failure is
  reported in the cmdresult, not retried.

### The key: `D` (DISPLAY), tasks scope

`X` is taken (`ProgressClear`), and `x` means delete or kill in every pane
that binds it (forwards, board, schedules). `D` is unbound in the main view;
only the file picker, a modal with its own keys, uses it.

| Selected task | `D` does |
|---|---|
| Finished (Succeeded / Failed / Cancelled) | Resume with X11, conversation kept (`DoOpenX11Session` with the resume id, as `r` does without X11) |
| Live, and this TUI holds its tunnel | Reattach (`DoAttachSession`, Control) |
| Live, no tunnel here | Confirm popup: "restarts the agent (conversation kept); a turn in flight is lost". On yes: cancel, wait for the task to reach a finished status, then resume with X11 |
| Anything else | A hint saying why, as `r` does (`resumeReattachAction`) |

The confirm follows the sibling rule in `tui/taskaction.go`: a path that kills
an in-flight turn is never hidden behind a single unconfirmed key.

## Left to the builder

- The `keys.go` row and its help line (`keys_test` holds the pair).
- Whether a cmdline verb mirrors the key (walk `surface-parity-checklist`).
- How the cancel → resume wait observes the finished status (tasks.status
  event vs. a bounded poll), and what the TUI shows if the resume is refused.

## Verification

- Unit: detach leaves the forward running; task end and TUI exit stop it;
  the display-number pick; the key's four rows.
- Live, dummy harness, TUI inside a host session. The tunnel is what is under
  test, not an X server, so a stand-in will do where no X server runs: a
  `socat` listener on `/tmp/.X11-unix/X<n>` as the "local X server", a cookie
  for `:<n>` added with `xauth` so the open finds one, and a connection held
  open through the runner's `127.0.0.1:6000+N`. Ctrl+], reattach: the held
  connection is still up. The key on a live non-X11 session: confirm, the
  task restarts with DISPLAY set and the conversation kept. A real GUI
  program (xclock) needs an X server, e.g. Xvfb, which is not installed here.
