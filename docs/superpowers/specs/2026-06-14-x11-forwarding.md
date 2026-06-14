# X11 Forwarding (session-coupled) — Spec

**Status:** backfilled from the design conversation; authoritative problem statement + decisions for `docs/superpowers/plans/2026-06-14-x11-forwarding.md`.

## Problem statement

A GUI (X11) program started inside a harness interactive session runs on the
**runner**, which has no display. There is currently no way to render it on the
human operator's screen. SSH solves the analogous problem with `-X`/`-Y`
forwarding. We want the equivalent for `harness-cli`: run `xterm`/`xeyes`/etc.
in a `session new` shell and have the window appear on the operator's **local**
X server, with input and output tunneled over the existing harness transport.

Concretely the feature must:

1. Make the spawned shell's environment carry a working `DISPLAY` (and matching
   `XAUTHORITY`) so any X client launched in it connects to the operator's X
   server through the tunnel.
2. Carry the X protocol byte stream between the runner (where the X client runs)
   and the operator's real X server (on the client host).
3. Authenticate the X client to the operator's X server (MIT-MAGIC-COOKIE-1).
4. Work for the CLI/TUI client path on Linux/Windows/macOS operator desktops
   that have a real X server reachable via `$DISPLAY`.

## Mechanism (plain terms, no invented labels)

X is network-transparent: an X client connects to whatever socket `DISPLAY`
names. The runner is given `DISPLAY=127.0.0.1:N`; it listens on TCP
`127.0.0.1:(6000+N)` and tunnels every accepted connection — reusing the
existing `-R` remote-forward machinery (runner listens → bytes relayed to
client → client dials its local X server). The client copies its local X
server's cookie to the runner so the tunneled X client authenticates. This is
SSH **trusted** (`-Y`) forwarding: the real cookie is reused, no translation.

## Decisions taken

- **Client picks the display number N** (`--x11`, default 10; `--x11-display N`).
  The `OpenInteractive` response is returned before the runner runs
  (`server/task_handler.go:681` fire-and-forwards `OpenExec`), so the runner
  cannot report an allocated N synchronously. Client-chosen N avoids adding a
  response round-trip; collisions surface as a `-R` BindFailed error.
- **Runner listens TCP `127.0.0.1:(6000+N)`**, `DISPLAY=127.0.0.1:N`. Avoids
  UNIX-socket listening on the runner.
- **Client-side dial supports UNIX sockets.** Linux X servers default to
  `-nolisten tcp`, so the client must reach `/tmp/.X11-unix/X0`. One generic
  field (`RemoteForwardSpec.DialNetwork`) added; `-R` keeps defaulting to TCP.
- **Trusted forwarding only** (real cookie copied). No SECURITY-extension /
  untrusted cookie translation. Justified by toy/dogfood scope
  (`project_protocol_stack_scope`, `feedback_individual_dogfood`).
- **Schema: one shared `X11Forward` format**, embedded under `if x11_enabled
  == 1` in both `OpenInteractiveRequest` and `OpenExecRunnerRequest`. No bytes
  on the wire when X11 is off (`feedback_no_schema_invisible_bytes`); single
  source for the byte layout (`feedback_no_split_schemas`).
- **Cookie optional — no-auth fallback.** When the client can extract a cookie
  (`xauth list $DISPLAY`), it is shipped and the runner registers it (trusted
  `-Y` forwarding). When no cookie is available (xauth absent, or the local X
  server runs without access control — e.g. VcXsrv "Disable access control",
  common on Windows), the client warns and forwards WITHOUT authentication: it
  sends an empty cookie, and the runner injects `DISPLAY` but not `XAUTHORITY`
  (no xauth registration). This makes the cross-OS Windows+VcXsrv path usable
  (`project_deployment_topology`). Safety: a cookieless session only "succeeds
  insecurely" against an already-unauthenticated server; a secured server
  rejects the cookieless client, so the fallback cannot downgrade a secured
  connection. The fallback is announced on stderr, not silent.

## Out of scope

- Untrusted forwarding / X SECURITY extension cookie translation.
- Runner-side display-number allocation (would need a server→client response
  field the current `OpenInteractive` flow lacks).
- UNIX-socket *listening* on the runner.
- WebUI clients (no port-forward wiring there; no X server in a browser).
- `--x11` with `--detach` (a detached session has no client to host the tunnel).

## Known limitation

Two runner processes on one host both using display N collide on
`127.0.0.1:(6000+N)`, surfaced as a `-R` BindFailed error. Consistent with
`project_runner_ambiguous_same_host_roots`. Mitigation deferred (see Out of
scope: runner-side allocation).

## Verification

Cannot be unit-tested end-to-end (needs a live X server). Manual E2E: `xeyes`
in an `--x11` session must appear on the operator's screen and track the mouse
(confirms the bidirectional tunnel). Pure helpers (DISPLAY parsing, cookie
parsing, env emission, unix dial) are unit-tested. Per
`feedback_verify_before_memory_writes`, no "X11 works" memory until E2E passes.
