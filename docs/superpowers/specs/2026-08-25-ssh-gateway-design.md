# SSH gateway — an `ssh` front door to harness interactive sessions — design

## Problem

The harness borrows SSH's vocabulary throughout — `forward -L` / `-R` / `-W`
(`cmd/harness-cli/main.go:1096`), X11 forwarding modelled on `ssh -Y`
(`docs/superpowers/specs/2026-06-14-x11-forwarding.md`), `session send` /
`session exec` joining their trailing words ssh-style
(`cmd/harness-cli/session.go:145`) — but there is no way to reach a session
*with an actual `ssh` client*. Every route into a live PTY is a harness
binary: `harness-cli session attach`, the TUI, or the WebUI's xterm.

That closes off the operator's existing tooling:

- **P1.** `~/.ssh/config` aliases. There is no `Host` entry that lands in a
  harness session, so a session cannot be reached the way every other machine
  the operator works with is reached.
- **P2.** Terminal multiplexer integration. `tmux new-window 'ssh t1'` and
  friends need a command that connects and stays connected; the harness verbs
  qualify only if the harness binary is present and configured on that host.
- **P3.** Scripted / non-interactive drivers that already speak ssh.
- **P4.** `mosh`, which is layered on an ssh login and has no harness equivalent.

Non-problem, stated so the Implementation section is not read as narrowing it:
this is about **reaching an interactive session's PTY**. File access to a
worktree (scp / sftp / rsync / VS Code Remote-SSH) is a different feature with a
different data plane, and `file push` / `file pull` already cover that ground.
It is out of scope here and its absence is a decision, not an oversight (§ Non-goals).

## Decisions taken

Every line here is decided — none of it is a preference for an implementer to
settle later (Pitfall 7). The third column records only *who* decided, which
matters for review rather than for implementation: **operator** means the human
chose it in conversation, **this spec** means the author chose it while writing,
so those are the rows the operator has never actually ruled on and the ones
worth a second look. A "this spec" row is flipped by saying so, not by
re-deciding it during implementation.

| # | Decision | Decided by |
| --- | --- | --- |
| D1 | The listener runs client-side, in a harness client process — not in `harness-server` | operator |
| D2 | Both a CLI verb and TUI hosting ship in v1 | operator |
| D3 | The ssh user name selects the task | operator |
| D4 | The listener's lifetime is its host process's lifetime | operator |
| D5 | No ssh authentication on a loopback bind; a non-loopback bind requires `--authorized-keys` | operator — **reverses** an earlier D5, see § Authentication |
| D6 | One **control** ssh session per task per gateway; a second control session is refused. Cowriters and viewers coexist freely | this spec |
| D7 | `Ctrl+]` **is** intercepted — it is this system's detach gesture (tmux `Ctrl+b d`), and SSH offers only disconnect, never detach | this spec — **reverses** an earlier D7 in this spec, see § Detach |
| D8 | The terminal-mode resets are written on the two ends the gateway observes in time, and are impossible on a client-initiated disconnect — both groups, minus any group the negative control in § Testing shows is not needed | this spec |
| D9 | No `.bgn` / wire change | this spec |
| D10 | The pump and **both** reset strings live in `objtrsf/exec` — the screen group exported from `RemoteShell`'s literal, the input group *moved* out of this repo — and objtrsf lands and publishes first, then a `go.mod` bump | operator |
| D11 | The bare user name is a **cowrite** attach, so arriving over ssh never evicts an attached client; `.control` and `.view` are the other two forms | operator |

## Why client-side and not in the server

The server is the trusted hub. Giving it an ssh listener would add a second
authentication surface next to the PSK handshake — host key material, an
authorized-keys policy, and a mapping from ssh identities onto the operator
authority that `--operator-psk` currently gates
(`cmd/harness-server/main.go:36`). A client-side listener adds none of that to
the server: it is an ordinary harness client that already holds operator
credentials, and it speaks the existing `AttachSession` RPC.

The cost is reachability: an ssh client can only use a gateway it can open a TCP
connection to. With the default loopback bind that means the same machine. That
is the accepted trade for P1–P4, all of which are same-machine cases.

## Shape

```
ssh -p 2222 <32-hex-task-id>@127.0.0.1           # cowrite: type, evict nobody
ssh -p 2222 <32-hex-task-id>.control@127.0.0.1   # take the seat (owns the PTY size)
ssh -p 2222 <32-hex-task-id>.view@127.0.0.1      # watch only
```

```
                         ┌─────────────────────────────────┐
 ssh client ──TCP/SSH──▶ │ gateway (cli/sshgw)             │
 (tmux, mosh,            │  x/crypto/ssh server            │
  ssh config alias)      │  session channel ↔ attach stream│
                         └───────────────┬─────────────────┘
                                         │ existing *cli.Client
                                         │ AttachSession RPC + trsf stream
                                         ▼
                                   harness-server ──▶ runner PTY
```

The gateway is the same *kind* of thing as the WebUI's in-browser terminal: a
front end that is not a local tty, owning a control attach, feeding input bytes
in, painting output bytes out, and reporting its own size. That is the sibling to
follow (`cli/open_interactive_wasm.go`, especially `SetTerminalWindowSize` at
:387 and the single-writer discussion at :52–65) — **not**
`(*CommandExecutionStream).RemoteShell()`, which is the local-tty end and is what
the gateway replaces.

## Package layout

New package `cli/sshgw`. Every file carries `//go:build !js`: package `cli` is
compiled for `js/wasm` (`cli/attach_js.go`, `cli/open_interactive_wasm.go`), and
`golang.org/x/crypto/ssh` has no place in that build.

```
cli/sshgw/gateway.go     Run(ctx, c *cli.Client, opts Options) error
cli/sshgw/auth.go        host key load-or-create, authorized_keys parsing
cli/sshgw/user.go        ssh user name → (taskID, protocol.AttachMode)
cli/sshgw/session.go     one ssh session channel ↔ one attach stream
```

Import direction is `cmd/harness-cli → cli/sshgw → cli` and
`tui → cli/sshgw → cli`. Package `cli` never imports `cli/sshgw`.

`Run` takes an already-connected `*cli.Client` rather than a server CID, so the
TUI passes its long-lived `a.client` the way every other `Do*` does
(`feedback_reuse_long_lived_client`, Pitfall 3). `harness-cli` dials one and
passes it in; there is no `*With` split to make, because there is no short-lived
form of a listener.

`golang.org/x/crypto` moves from the indirect to the direct require block in
`go.mod`. It is already in `go.sum` at v0.52.0 and present in the module cache;
no network fetch is needed.

## Options

| Option | Default | Meaning |
| --- | --- | --- |
| `--listen` | `127.0.0.1:2222` | bind address of the ssh listener |
| `--host-key` | `<dir of .harness/config>/ssh_host_ed25519_key` | server host key; generated on first run if absent, mode 0600 |
| `--authorized-keys` | unset | accepted client public keys, OpenSSH `authorized_keys` format. Optional on a loopback bind, **required** otherwise (D5) |

The host-key path sits next to the workspace config because that is this
project's one existing per-repo client-state location
(`cli/workspace/locate.go:15`, `DefaultPath = ".harness/config"`). The repo has
no home-directory convention — there is no `os.UserHomeDir` /
`os.UserConfigDir` call anywhere in it — so defaulting into `~/.harness/` would
invent one. The default is derived from the *resolved* config path, so
`--config` / `HARNESS_CONFIG` moves it. `--authorized-keys` has no default at
all: it is opt-in, and a path is given explicitly when it is used.

Host-key generation follows the `--psk-file` precedent
(`cmd/harness-server/main.go:35`): the file is the origin, created on first run
and reused afterwards. It must be stable — a regenerated host key makes every
subsequent `ssh` print a host-key-changed warning and refuse to connect.

## Authentication (D5)

On a loopback bind there is none: `NoClientAuth` is true, and anything that can
open a TCP connection to the listener is served.

**Reversal.** An earlier D5 required public keys, arguing that the gateway holds
operator authority and that a non-sandboxed agent on this host can reach
`127.0.0.1`, so an open port would be an agent → operator escalation. The second
half is true and the conclusion still did not follow: **an agent started by the
runner runs as the same UID as the operator**, so it can read
`~/.ssh/id_ed25519` and authenticate as the operator. Public keys stop a
different-UID process, and no different-UID process is in the picture — the
sandboxed launch path is confined by `scripts/sandbox/init-firewall.sh` to the
harness server's single port and cannot reach the listener at all. Requiring
keys would have bought setup friction and no confinement.

What that argument does **not** license is an unauthenticated non-loopback bind:
there, a different-UID reader on another machine is exactly the adversary, and
the same reasoning inverts. So `--listen` and authentication are coupled in code
— a bind address outside `127.0.0.0/8` / `::1` is refused unless
`--authorized-keys` names a file with at least one usable key, at which point
`PublicKeyCallback` gates every connection against it. `PasswordCallback` and
`KeyboardInteractiveCallback` are nil in both configurations.

The refusal is a startup error, not a warning-and-continue: quietly serving
`0.0.0.0:2222` with no authentication is the widest possible reading of a
mistyped flag.

## SSH surface

Accepted:

- channel type `session` — one per ssh connection
- request `pty-req` — the initial window size is taken from it
- request `shell` — starts the attach
- request `window-change` — forwarded as a size update
- reply `exit-status` — sent before the channel closes

Not served, each saying why:

- `exec` (`ssh host <command>`) — there is no command surface here; the session
  runs whatever the task already runs. **Accepted and then answered** with the
  explanation on stderr and exit status 1, rather than refused. Refusing a
  *request* delivers no reason: the client's `Start` fails and it tears the
  session down without ever draining stderr, so the sentence would be written
  and never read. Measured against `x/crypto/ssh` as the client while writing
  the end-to-end test, which asserted the reason arrives and caught the
  original refuse-with-a-message shape.
- `subsystem` (sftp, and therefore sftp-backed scp) — refused, with the same
  message written first on the chance a client shows it. Refused rather than
  accepted because an sftp client handed an acceptance waits for a protocol
  that is never coming; "subsystem request failed" is the better outcome.
- any channel type other than `session` — rejected at channel open, where the
  reason travels in the rejection itself and the client does print it. Notably
  `direct-tcpip`, i.e. `ssh -L` through the gateway, which would be a second,
  redundant path to `harness-cli forward`.

`TERM` from `pty-req` is read and discarded: the runner-side PTY's `TERM` is
fixed when the session is created, and re-negotiating it mid-session would
change what the already-running agent renders.

## User name → target (D3)

`cli/sshgw/user.go` maps the ssh user name:

| User name | Result |
| --- | --- |
| 32 lowercase hex chars | that task, `protocol.AttachMode_Cowrite` |
| 32 lowercase hex chars + `.control` | that task, `protocol.AttachMode_Control` |
| 32 lowercase hex chars + `.view` | that task, `protocol.AttachMode_View` |
| anything else | authentication succeeds, the session channel is rejected with a message naming the accepted forms |

The rejection deliberately happens at channel open, not at authentication:
failing authentication for a malformed user name makes ssh retry keys and then
report a credentials problem, which points the operator at the wrong thing.

**The bare form is cowrite, so `ssh <id>@…` never takes the seat** (D11).
`SessionMux.Attach` is a takeover — it cancels the previous controller and
`CloseBoth`s its stream (`server/session_mux.go:410–428`) — so a control default
would mean that reaching a task over ssh silently detaches whatever the operator
had attached in the TUI. Arriving somewhere should not evict you from it. The
takeover is still available, spelled out, as `.control`.

The cost of the default is the PTY size, and only while someone else holds the
seat. Two gates decide an observer's resize and both must pass: `allowResize`,
resolved per caller from `Capability_ExecResize` (`server/task_handler.go:1401`
— the gateway passes it, since `callerCaps` returns `Capability_All` for a
connection with no principal task, `server/capabilities.go:94–98`), and then
`applyObserverWinSize`, which honours the frame **only while the control seat is
empty**. Last-writer-wins was considered there and rejected after use: an
observer resize redrew a human's terminal at a size they had not chosen, and
their next SIGWINCH silently undid it.

So:

| State of the task | `ssh <id>@…` (cowrite) |
| --- | --- |
| Nobody holds the seat — the ordinary state of a detached session | the ssh window sizes the PTY itself; everything renders at its own size |
| A controller is attached (the TUI, a CLI attach, a `.control` ssh session) | the ssh window renders at *that* client's size, and a differently-sized window shows a broken screen |

The second row is the case where `.control` is the answer, and the operator
reaching for it is choosing the eviction rather than being handed it.

## Data plane

Per accepted `shell` request:

1. `c.AttachSessionWithReplayLimit(ctx, taskID, mode, 0)` — full replay ring, the
   same as `session attach` (`cli/attach_native.go:33`).
2. If the returned `protocol.TaskKind` is `TaskKind_Stream`, write the same
   explanation `SessionAttach` gives (`cli/attach_native.go:57`) to the channel
   and close with a non-zero exit status: an event-stream task carries NDJSON,
   not terminal bytes, and painting one as the other shows garbage.
3. Send the `pty-req` size via `SetTerminalWindowSize(rows, cols, widthPx, heightPx)`.
   `pty-req` carries pixel dimensions too, so unlike `applyInitialWindowSize`
   (`cli/initial_winsize.go:20`, which has only rows/cols and passes 0/0) all
   four values are forwarded.
4. `stream.PumpTerminalIO(channel, channel)` (D10) — channel → `stream.Stdin()`
   with detach-key interception, `stream.Stdout()` → channel. `stream.Stderr()`
   → the channel's extended data (stderr) stream is copied alongside it by the
   gateway, since the pump handles only stdin/stdout.
5. `window-change` → `SetTerminalWindowSize` with the new values.
6. On either direction ending: § Detach below, then `exit-status` and close.

A zero row or column count in `pty-req` is treated the way
`applyInitialWindowSize` treats a half-specified size — the size is not sent at
all, rather than half-sent — and a line is written to the channel's stderr
saying the session will render at whatever size it already had.

## Single writer (D6)

`cli/open_interactive_wasm.go:52–65` records the invariant this feature breaks:
the native CLI and TUI get single-writer-at-a-time for free, because each runs
one `RemoteShell` against the one real terminal, while the browser — one shared
xterm fed by per-session goroutines — had to enforce it explicitly with a
generation guard, after residual frames from a superseded session desynced the
xterm parser badly enough to force a page reload.

A gateway is on the browser's side of that line: nothing stops two `ssh` windows
from naming the same task.

What is scarce is the control seat, and only it. The gateway therefore keeps a
map of task id → live **control** session and **refuses a second control
session** on that task, writing a message that names the one already connected.
It does not supersede: `SessionMux.Attach` evicts the previous controller
server-side (`server/session_mux.go:373`), so superseding would let a second
`ssh` silently take the seat from the operator's own TUI or CLI attach. A
refusal is visible; a theft is not.

Cowriters and viewers are not counted or refused. Coexisting is what those modes
are for — the mux fans output out to all of them and forwards a cowriter's input
without touching the seat (`server/session_mux.go:488`,
`cli/streamattach_native.go:22`) — and since the default user-name form is
cowrite (D11), the ordinary case of two `ssh` windows on one task is simply
allowed.

## Detach and terminal restoration (D7, D8)

**Detach and disconnect are different actions, and SSH offers only the second
one.** `Ctrl+]` is this system's detach gesture — the equivalent of tmux's
`Ctrl+b d`: leave the session running, hand the terminal back, return to the
shell you came from. `RemoteShell` implements it by scanning the input stream
for a single `0x1d` (`exec/exec_shell.go`, `detachIndex`; a one-shot byte, not a
prefix), and every attach surface advertises it (`cli/attach_native.go:64`,
`tui/app.go:1055`).

`~.` is not that gesture. It tears the connection down from the client side,
which is the equivalent of closing the terminal window — the harness sees a
dropped stream and marks the task `Detached`, but the operator got there by
yanking the cable, not by leaving.

So the gateway intercepts `0x1d` and ends the ssh session cleanly on it: the
task detaches, `exit-status` 0 goes back, and `ssh` exits normally. Without it,
an operator who reached a session through ssh has no way to leave one except by
killing their connection, which is a capability the CLI and TUI both have and
this surface would lack.

**Reversal.** An earlier draft of this spec decided the opposite — do not
intercept, because `~.` exists. That treated disconnect and detach as the same
action; they are not, and the paragraph above is why.

The interception's cost is unchanged and real: `0x1d` no longer reaches whatever
runs in the session.

### What this means for the resets

Terminal-mode resets can only be written while the ssh client is still reading,
which sorts the ways a session ends into two groups:

| How the session ends | Resets land? |
| --- | --- |
| The detach key — a byte arriving on a live channel | **Yes** |
| The far end ends it — the task exits, the attach stream errors, another client takes the control seat | **Yes**: the ssh client is still connected and reading |
| The ssh client leaves — `~.`, the terminal window closes, the link drops, the process is killed | **No** |

The third row is not a gap to be closed in a later version, and it is not the
reason the detach key is intercepted — that reason is the paragraph above, and
it holds whether or not any reset byte is ever written. Nothing in the SSH
protocol lets a server write to a client that has stopped reading, so an
operator who disconnects with `~.` out of a full-screen app gets the same
terminal a killed `ssh` to any other host leaves behind. The remedies are
operator-side and outside this design: `reset`, or an alias that appends the
resets itself. This is documented in the CLI usage text and the TUI's
`ssh-gateway` output so it is discoverable at the point of use rather than only
here.

What the resets are for: the ssh client's terminal is a real terminal with the
same residue problem a local attach has — a full-screen app detached mid-run
never gets to tear down the alternate screen, mouse reporting, or its scroll
region, and SSH restores termios on exit while emitting no escape sequences.

Today a CLI attach emits **two** groups, from two places, back to back
(`cli/attach_native.go:72–74`). D10 puts both in `objtrsf/exec` and has
`RemoteShell` write them together; the names below are the ones after that move:

- **screen group** — `exec.ScreenModeReset`, today an unexported literal in
  `RemoteShell`'s `defer`: Win32 input mode, `modifyOtherKeys`, alt screen,
  cursor visibility, mouse modes, bracketed paste, `\x1b[r` (DECSTBM) and
  `\x1b[?6l` (DECOM)
- **input group** — `exec.InputModeReset`, today `cli.InputModeReset` written by
  `cli.RestoreLocalInputModes` (`cli/terminal_input_modes.go:36`): the
  input-sending modes

On either end it can serve, the gateway writes both groups into the ssh channel
before closing it. The channel is the only pipe to the ssh client's terminal, so
the bytes a local attach writes to `os.Stdout` go there instead — the ssh client
prints them to its own terminal, which is what needs resetting. Both groups, not
some merged subset: they overlap (`\x1b[?1000l`, `?1002l`, `?1003l`, `?1006l`,
`?2004l` are in each), and turning off a mode that is already off is a no-op, so
there is nothing to reconcile and no order to get right. The result is that an
ssh detach leaves a terminal in the state a CLI detach leaves it in.

Neither group is copied into the gateway: both are taken from `objtrsf/exec`
(D10).

## Shared code in objtrsf (D10)

Three things the gateway needs live inside `objtrsf/exec` or should, and all
three are reachable today only through `RemoteShell`, which owns a local tty and
therefore cannot serve an ssh channel:

| Symbol | Where it is today | Change |
| --- | --- | --- |
| `exec.ScreenModeReset` | an unexported literal in `RemoteShell`'s `defer` | extracted to a named exported const |
| `exec.InputModeReset` | `cli.InputModeReset` in **this** repo (`cli/terminal_input_modes.go:36`) | **moved** to objtrsf, doc comment and all |
| `exec.WriteTerminalReset(w io.Writer)` | the `defer` body itself | new: writes both consts to `w`, so "the full reset" is composed in one place |
| `(*CommandExecutionStream).PumpTerminalIO(in io.Reader, out io.Writer) error` | unexported `pumpTerminalIO` | exported; the body does not change |

`WriteTerminalReset` exists so the *composition* has one home too, not just the
two strings: `RemoteShell`'s `defer` calls it with `os.Stdout` and the gateway
calls it with the ssh channel. Without it, a later third group added in objtrsf
would reach `RemoteShell` and silently not reach the gateway — the same drift
the move is meant to end, one level up. The consts stay exported alongside it
because the negative control writes them individually.

The input group moves rather than staying put because the split between the two
was historical, not semantic: **every** call site in this repo writes them back
to back, and there is no site that writes one without the other —
`cli/attach_native.go:73–74`, `cli/open_interactive_native.go:144–145`,
`cli/x11.go:173–174`, `tui/interactive.go:352–357`, which with the definition
file is the complete set of references. Two consts a repository apart, whose
byte ranges overlap and which are only ever emitted together, are two chances to
edit one and miss the other.

They stay two named consts rather than becoming one string. The reason is in
their doc comments: one stops the terminal from *sending* things, the other puts
the *screen* back, and the harness-side comment records why the boundary was
drawn there (it mirrors the server's `excludedFromPreamble` split). Collapsing
them would flatten that into one undifferentiated wall of escapes — and would
also make the negative control in § Testing impossible to run per group.

`RemoteShell` writes both in its `defer`, so the four harness call sites lose
their trailing `RestoreLocalInputModes` line and `cli/terminal_input_modes.go` is
deleted. That also brings the input group inside the raw-mode window that
objtrsf's comment says it wants for the screen group — today it is emitted after
`term.Restore`, in cooked mode. Whether that ordering ever mattered has not been
observed; the alignment is a consequence of the move, not a fix being claimed.

`pumpTerminalIO` already takes `io.Reader` / `io.Writer` and is already exercised
with non-tty ends by objtrsf's own tests, so exporting it is a rename plus a doc
comment. `RemoteShell` becomes: raw mode, the reset `defer`, the SIGWINCH
forwarder, then `PumpTerminalIO(os.Stdin, os.Stdout)` — the tty-specific parts
stay where they are, and the part that is not tty-specific becomes callable.

Reusing it also gets the detach *semantics* right rather than approximately
right. On the detach byte the pump calls `w.BidirectionalStream.Close()`, a
half-close of the send side, and the comment there records why the obvious
alternative is wrong: `stdinWrapper.Close()` sends a zero-length Stdin frame,
which the runner delivers to the agent as EOF and kills bash. A gateway that
hand-rolled its own detach would have had that choice to make blind.

One property checked rather than assumed, because it decides whether a network
end can be passed at all: the input pump's `swallowLocalReadEOF` absorbs a
Windows console artefact, and `platformSwallowLocalReadEOF`
(`exec/stdin_eof_windows.go`) is narrowed to an `in` that is an `*os.File` and a
console handle. An ssh channel is neither, so it returns false and a genuine EOF
on the channel ends the pump immediately, on Windows as everywhere else.

Sequencing: the objtrsf change lands and is published first, then this repo's
`go.mod` is bumped to it, and the gateway is written against the bumped version.
objtrsf's landing policy is local-trunk FF push (`landing-policy-objtrsf`), and
no `replace` directive is used at any point.

## Not a wire change (D9)

No `.bgn` file is touched. The gateway uses `AttachSession` and `AttachMode`
exactly as they exist. So `scripts/wire-skew-check.sh` is a no-op for this work
and no server restart is required to deploy it (Pitfall 10 does not apply). The
`go.mod` change (x/crypto indirect → direct) affects only the client binaries.

## Operator surface matrix

Filled per Pitfall 9 — every cell has a verdict, and omissions are decisions.

| Surface | Verdict |
| --- | --- |
| CLI binary (`cmd/harness-cli/main.go` dispatch + `usage()`) | **Implemented** — `ssh-gateway` verb, foreground until Ctrl-C, three flags above. Registered alongside `forward` (`main.go:604`), documented in the usage block that lists `forward` (`main.go:1095`) |
| TUI keybindings (`tui/app.go`) | **Intentionally omitted** — every task-pane key acts on the selected task; a gateway is process-scoped, not task-scoped, so there is no task for a key to act on |
| TUI command line (`tui/cmdline.go`) | **Implemented** — `ssh-gateway [listen]` starts, `ssh-gateway stop` stops, `ssh-gateway` alone reports state. Parsed next to `parseForward` (`tui/cmdline.go:1069`) |
| TUI popups/forms | **Intentionally omitted** — `PortForwardModal` exists because a forward spec is four fields with no default; a gateway takes one optional address |
| TUI display | **Implemented** — start/stop/state are reported through the same result-line path the forward commands use (`tui/app.go:535`). The gateway is *not* added to `activeForwards`: that map is keyed per forward session and drives the `P`/`B` task-scoped stop keys and workspace capture, none of which apply |
| WebUI buttons/forms, WebUI command line, WASM bridge | **Structurally impossible** — a page's JavaScript has no API for accepting an inbound TCP connection, so no wasm build can host a listener. The WebUI's equivalent of "reach this session from this device" is the WebUI itself |
| Shared `cli/` | **Implemented** — the `cli/sshgw` package. `cli/terminal_input_modes.go` is deleted and its four call sites drop a line (D10) |
| `objtrsf/exec` (separate module) | **Implemented** — `InputModeReset`, `ScreenModeReset`, `WriteTerminalReset` and `PumpTerminalIO` (D10), landed and published before this repo's work starts |
| `server/`, `runner/`, protocol | **Not applicable** — no change (D9) |
| Workspace config (`.harness/config`) | **Intentionally omitted** — a workspace records forwards so they can be re-established on reconnect; a gateway is not per-task and has one address, and `--listen` in the alias the operator already wrote is the same information |

The `surface-parity-checklist` skill is walked item by item during plan writing,
not summarized; this table is the design-level answer, not a substitute for it.

## Errors

Every refusal says why, but *where* the sentence goes depends on what is being
refused, because an ssh client only reads some of those places:

- **At channel open** — unknown user-name form, task already holding a control
  session through this gateway, non-session channel type. The reason rides the
  rejection and the client prints it.
- **On the channel's stderr, with the channel live** — task not found / already
  finished (mapped from `cli.attachStatusError`, `cli/attach.go:80`), wrong task
  kind, an unusable `pty-req` size, a failed initial resize. The client is
  attached and reading, so the text lands.
- **Never on the stderr of a refused request.** A client whose request is
  refused tears the session down without draining stderr. This is why `exec` is
  accepted and answered instead (§ SSH surface).

Startup failures — bind refused, a non-loopback `--listen` with no usable
`--authorized-keys` (D5), unreadable host key —
are returned from `Run` and surface as a CLI exit with a message, and in the TUI
as the same result line the forward commands use.

## Testing

Unit:

- user-name mapping: 32-hex, 32-hex + `.view`, uppercase hex, wrong length,
  empty, a name that merely contains a hex run
- authorized-keys: accepted key, unknown key, comment / options lines, empty
  file
- the bind/auth coupling (D5): loopback with no keys starts, non-loopback with
  no usable key is refused at startup, non-loopback with keys starts and gates
- host key: created on first run with mode 0600, byte-identical on second run
- `pty-req` payload decode including the zero-size case

Integration (`integration/`), using `golang.org/x/crypto/ssh` as the *client* so
the suite does not depend on an `ssh` binary being installed:

- connect → `pty-req` → `shell` → the replay burst arrives on the channel
- `window-change` from a bare (cowrite) connection with the seat empty reaches
  the session — assert against the size the session reports, not against the
  frame having been written
- the same `window-change` with a controller attached does **not** change the
  size: the seat rule (D11), which is the half a frame-was-written assertion
  would miss entirely
- a second bare connection to the same task is accepted; a second `.control`
  connection is refused with the expected message
- a `.control` connection evicts a control attach held elsewhere, and a bare
  one does not
- closing the channel leaves the task alive and `Detached`
- the mode-reset bytes are the last thing written before close

Live (`dummy-harness` skill, two-instance topology): a real `ssh` client from a
real terminal, plus tmux, driven with actual keystrokes — rendering is not proof
that input works (`feedback_verify_interactive_input_not_just_render`). Confirm
that resize reflows the agent and that detach leaves the session running and
reattachable from the TUI.

D8's resets get a **negative control**, not a confirmation. Detaching with the
resets written and finding the terminal healthy proves nothing: healthy is also
what "they were never needed" looks like. So the measurement is the other way
round — detach with the resets suppressed, per group, and record what breaks:

| Run | Suppressed | Scenario |
| --- | --- | --- |
| 1 | `exec.InputModeReset` | attach to a session whose app had mouse tracking on, detach with `Ctrl+]`, move the mouse at the local shell prompt |
| 2 | `exec.ScreenModeReset` | attach while a full-screen app (`htop`) runs, detach with `Ctrl+]` mid-run, then use the local shell |
| 3 | both | either scenario |

Every run leaves with the detach key, never with `~.`: the disconnect path
writes nothing at all, so it would show a broken terminal whether or not the
resets exist and can measure nothing about them. It is exercised once on its
own — `~.` out of `htop` — to check that the limitation documented in § Detach
is the observed behaviour and not an assumption about what OpenSSH does on the
way out.

A group whose suppression leaves the terminal usable in its scenario is not
carrying its weight and is dropped from the gateway path, with the observation
recorded here. What the mechanism predicts, so that a null result is recognised
as surprising rather than as a pass: run 1 should break, because the server
replays every tracked mode on attach and `excludedFromPreamble`
(`server/mode_tracker.go:60`) excludes only the alternate-screen family and 2026
— so mouse and bracketed-paste modes reach the ssh client's terminal on every
attach regardless of what is running, and SSH restores termios on exit without
emitting escape sequences. Run 2 depends on bytes from the live app rather than
on the replay, so it is the genuinely open one; a null there means the OpenSSH
client cleans up more than expected, which is worth knowing either way.

Verification runs through the `make` targets (`make check`, `make wasm-check`,
`make vet`, `make test`), not ad-hoc `go build ./...`; `wasm-check` is the one
that catches a missing `//go:build !js`.

## Non-goals

- **scp / sftp / rsync / VS Code Remote-SSH.** A different data plane (file
  transfer, and for Remote-SSH a server binary staged on the far side), already
  served by `file push` / `file pull` / `git`. `subsystem` is refused explicitly
  so the failure names itself.
- **`ssh -L` / `-R` through the gateway.** `direct-tcpip` is refused; the harness
  has `forward` for this and a second path would drift.
- **A server-hosted sshd.** § Why client-side and not in the server.
- **Surviving its host process.** D4: the listener dies with the CLI process or
  the TUI. Making it a persistent daemon is a separate change that adds one
  caller, not a different design.
- **Agent-facing use.** This is an operator surface. Nothing is added to
  `runner/agentskills/`.
