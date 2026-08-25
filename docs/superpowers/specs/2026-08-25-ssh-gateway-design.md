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

Each line is a decision, not a preference to be re-litigated by an implementer.
`DECIDED (operator)` means the human chose it in the brainstorming session;
`DECIDED (author)` means it was decided while writing this spec and is flippable
by the operator on request.

| # | Decision | Status |
| --- | --- | --- |
| D1 | The listener runs client-side, in a harness client process — not in `harness-server` | DECIDED (operator) |
| D2 | Both a CLI verb and TUI hosting ship in v1 | DECIDED (operator) |
| D3 | The ssh user name selects the task | DECIDED (operator) |
| D4 | The listener's lifetime is its host process's lifetime | DECIDED (operator) |
| D5 | Public-key authentication is required; no password, no open loopback | DECIDED (author) |
| D6 | One ssh session per task per gateway; a second is refused | DECIDED (author) |
| D7 | `Ctrl+]` is NOT intercepted; detach = end the ssh session | DECIDED (author) |
| D8 | On detach the gateway writes the same terminal-mode resets a CLI attach writes | DECIDED (author) |
| D9 | No `.bgn` / wire change | DECIDED (author) |

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
ssh -p 2222 <32-hex-task-id>@127.0.0.1        # control attach
ssh -p 2222 <32-hex-task-id>.view@127.0.0.1   # view attach
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
| `--authorized-keys` | `<dir of .harness/config>/ssh_authorized_keys` | accepted client public keys, OpenSSH `authorized_keys` format |

The key paths sit next to the workspace config because that is this project's
one existing per-repo client-state location (`cli/workspace/locate.go:15`,
`DefaultPath = ".harness/config"`). The repo has no home-directory convention —
there is no `os.UserHomeDir` / `os.UserConfigDir` call anywhere in it — so
defaulting into `~/.harness/` or reading `~/.ssh/authorized_keys` would invent
one. Both defaults are derived from the *resolved* config path, so `--config` /
`HARNESS_CONFIG` moves them together.

Host-key generation follows the `--psk-file` precedent
(`cmd/harness-server/main.go:35`): the file is the origin, created on first run
and reused afterwards. It must be stable — a regenerated host key makes every
subsequent `ssh` print a host-key-changed warning and refuse to connect.

## Authentication (D5)

Public key only. `ssh.ServerConfig` with `PublicKeyCallback` checking the
presented key against `--authorized-keys`; `PasswordCallback` and
`KeyboardInteractiveCallback` are left nil, and `NoClientAuth` stays false.

If the authorized-keys file is missing or contains no usable key, the gateway
**refuses to start** and prints the command that fixes it. It does not fall back
to accepting any key: a silent fallback that widens authentication is the
failure mode Pitfall-9's reviewer checklist calls out ("if a config is set, does
any combination cause it to be ignored without log?"), inverted.

Why not "loopback is enough, skip auth": the gateway process holds operator
authority — it is a client that has already proven `--operator-psk` — and
agents on this host run outside any network namespace in the non-sandboxed
launch path, so `127.0.0.1:2222` is reachable from them. An unauthenticated
loopback listener would therefore be an agent → operator escalation of exactly
the shape that `project_cap_escape_kind_client_operator` records as a fixed bug.
The sandboxed launch path is narrower (`scripts/sandbox/init-firewall.sh` allows
only the harness server's single port), but the gateway must not depend on which
launch path an agent happened to take.

## SSH surface

Accepted:

- channel type `session` — one per ssh connection
- request `pty-req` — the initial window size is taken from it
- request `shell` — starts the attach
- request `window-change` — forwarded as a size update
- reply `exit-status` — sent before the channel closes

Refused, each with a human-readable message written to the channel's stderr
before rejection, so the ssh client prints a reason rather than a bare failure:

- `exec` (`ssh host <command>`) — there is no command surface here; the session
  runs whatever the task already runs
- `subsystem` (sftp, and therefore sftp-backed scp) — see Non-goals
- any channel type other than `session` — notably `direct-tcpip`, i.e.
  `ssh -L` through the gateway, which would be a second, redundant path to
  `harness-cli forward`

`TERM` from `pty-req` is read and discarded: the runner-side PTY's `TERM` is
fixed when the session is created, and re-negotiating it mid-session would
change what the already-running agent renders.

## User name → target (D3)

`cli/sshgw/user.go` maps the ssh user name:

| User name | Result |
| --- | --- |
| 32 lowercase hex chars | that task, `protocol.AttachMode_Control` |
| 32 lowercase hex chars + `.view` | that task, `protocol.AttachMode_View` |
| anything else | authentication succeeds, the session channel is rejected with a message naming the two accepted forms |

The rejection deliberately happens at channel open, not at authentication:
failing authentication for a malformed user name makes ssh retry keys and then
report a credentials problem, which points the operator at the wrong thing.

`AttachMode_Cowrite` is not exposed. It exists (`runner/protocol/message.go:20327`)
and would need a third user-name form; nothing in P1–P4 asks for it, and adding
a form is a one-line change if it is ever wanted.

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
4. Pump: channel → `stream.Stdin()`, `stream.Stdout()` → channel,
   `stream.Stderr()` → the channel's extended data (stderr) stream.
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

The gateway therefore keeps a map of task id → live session and **refuses the
second one**, writing a message that names the already-connected session. It
does not supersede: a control attach evicts the previous one server-side
(`server/session_mux.go:373`), so superseding would let a second `ssh` silently
steal the seat from the operator's own TUI or CLI attach. A refusal is visible;
a theft is not.

Two ssh sessions on *different* tasks are unaffected, as are a `.view` session
and a control session on the same task — view attaches take no seat
(`cli/streamattach_native.go:22`).

## Detach and terminal restoration (D7, D8)

Ending the ssh session closes the attach stream, which the server treats as a
detach (`server/session_mux.go:657`, `detachOnly`); the task stays alive in
`Detached`. That makes the ordinary ssh disconnect — `~.`, closing the window,
`exit` in the ssh client — the detach gesture.

`Ctrl+]` is **not** intercepted (D7). `RemoteShell` scans for it
(`exec/exec_shell.go` `detachIndex`) because a local tty in raw mode has no other
way out; an ssh client has `~.` and its own disconnect. `0x1D` is forwarded to
the session like any other byte. This diverges from the CLI and TUI, where
`Ctrl+]` detaches, and the divergence is deliberate: intercepting it would make
that byte unreachable for whatever runs in the session, to duplicate an escape
ssh already provides.

Before closing the channel the gateway writes the terminal-mode resets (D8). The
ssh client's terminal is a real terminal with the same residue problem a local
attach has: a full-screen app that is detached mid-run never gets to tear down
the alternate screen, mouse reporting, or its scroll region, and OpenSSH restores
termios on exit but emits no escape sequences.

Today a CLI attach emits **two** groups, from two places, back to back
(`cli/attach_native.go:72–74`):

- `RemoteShell`'s deferred write — Win32 input mode, `modifyOtherKeys`, alt
  screen, cursor visibility, mouse modes, bracketed paste, `\x1b[r` (DECSTBM)
  and `\x1b[?6l` (DECOM)
- `cli.RestoreLocalInputModes` — `cli.InputModeReset`, the input-sending modes
  (`cli/terminal_input_modes.go:33`)

The gateway writes both groups into the ssh channel. The channel is the only
pipe to the ssh client's terminal, so the bytes a local attach writes to
`os.Stdout` go there instead — the ssh client prints them to its own terminal,
which is what needs resetting. Both groups, not some merged subset: they overlap
(`\x1b[?1000l`, `?1002l`, `?1003l`, `?1006l`, `?2004l` are in each), and turning
off a mode that is already off is a no-op, so there is nothing to reconcile and
no order to get right. The result is that an ssh detach leaves a terminal in the
state a CLI detach leaves it in. `cli.InputModeReset` is already
exported and is used directly. The other group is unexported inside `objtrsf`,
so a second const — `cli.ScreenModeReset`, next to `InputModeReset` — carries
it, with a comment naming the objtrsf site it duplicates and why the copy exists
(publishing a new `objtrsf` and bumping `go.mod` to export one string is a
cross-repo round trip for something that must be verified here first). The
comment on `InputModeReset` explaining why screen modes are *excluded from that
const* stays accurate and is not rewritten; the new const's comment explains why
both are written together at this call site.

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
| Shared `cli/` | **Implemented** — `cli/sshgw` package plus `cli.ScreenModeReset` |
| `server/`, `runner/`, protocol | **Not applicable** — no change (D9) |
| Workspace config (`.harness/config`) | **Intentionally omitted** — a workspace records forwards so they can be re-established on reconnect; a gateway is not per-task and has one address, and `--listen` in the alias the operator already wrote is the same information |

The `surface-parity-checklist` skill is walked item by item during plan writing,
not summarized; this table is the design-level answer, not a substitute for it.

## Errors

Every refusal writes a sentence to the channel's stderr before closing, because
an ssh client shows that text and shows nothing useful otherwise. The set:
unknown user-name form, task not found / already finished (mapped from
`cli.attachStatusError`, `cli/attach.go:80`), wrong task kind, task already
connected through this gateway, `exec` / `subsystem` / non-session channel.

Startup failures — bind refused, missing authorized-keys, unreadable host key —
are returned from `Run` and surface as a CLI exit with a message, and in the TUI
as the same result line the forward commands use.

## Testing

Unit:

- user-name mapping: 32-hex, 32-hex + `.view`, uppercase hex, wrong length,
  empty, a name that merely contains a hex run
- authorized-keys: accepted key, unknown key, comment / options lines, empty
  file, missing file (must be a startup refusal, not an allow)
- host key: created on first run with mode 0600, byte-identical on second run
- `pty-req` payload decode including the zero-size case

Integration (`integration/`), using `golang.org/x/crypto/ssh` as the *client* so
the suite does not depend on an `ssh` binary being installed:

- connect → `pty-req` → `shell` → the replay burst arrives on the channel
- `window-change` reaches the session (assert against the size the session
  reports, not against the frame having been written)
- second connection to the same task is refused with the expected message
- a `.view` connection alongside a control connection is accepted
- closing the channel leaves the task alive and `Detached`
- the mode-reset bytes are the last thing written before close

Live (`dummy-harness` skill, two-instance topology): a real `ssh` client from a
real terminal, plus tmux, driven with actual keystrokes — rendering is not proof
that input works (`feedback_verify_interactive_input_not_just_render`). Confirm:
resize reflows the agent, detach leaves the session running and reattachable from
the TUI, and detaching out of a full-screen app (`htop`) leaves the ssh client's
terminal usable — which is the observable D8 predicts, and the one that decides
whether the two reset groups were both needed.

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
