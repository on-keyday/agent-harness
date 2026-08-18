# Claude Code sandbox kit (rootless podman)

Run a runner's spawned `claude` confined inside a **rootless podman** container
instead of directly on the host, to shrink the blast radius of an agent that
runs with `--dangerously-skip-permissions`. No harness core changes: it plugs in
through the existing `--agent-bin` seam.

## Why podman (not docker)

The agent must run **non-root** (Claude Code refuses `--dangerously-skip-permissions`
as root), yet its worktree edits must land on disk **owned by the host user** so
you can inspect/prune them. Under rootless *docker* those two are mutually
exclusive (only container-root writes host-owned files). podman's
`--userns=keep-id` maps the host uid into the container unchanged, satisfying
both at once. Verified on this host: in-container `uid=1000(kforfk)`, files
created in a bind mount are owned by `1000:1000` on the host.

## Prerequisites

- `podman` (`sudo pacman -S podman` on Arch). Coexists with docker.
  The wrapper's `--init` needs `catatonit`; Arch's podman hard-depends on it,
  other distros may package it separately.
- `/etc/subuid` + `/etc/subgid` entries for your user (present by default).

## Build the image

```sh
scripts/sandbox/build.sh
# pin the claude version for a reproducible image:
scripts/sandbox/build.sh --build-arg CLAUDE_VERSION=2.1.169
```

Produces `harness-claude-sandbox:latest` (override via `HARNESS_SANDBOX_IMAGE`):
`node:22` base + `git` + `ripgrep` + `python3`/`pip`/`venv` + the egress-firewall
tools (`iptables`/`ipset`/`iproute2`/`dnsutils`/`aggregate`/`jq`/`gosu`) +
`@anthropic-ai/claude-code`.

## Use it from a runner

Point `--agent-bin` at the wrapper; the runner spawns claude through podman:

```sh
scripts/runner.sh up --as sandboxed \
  --agent-bin "$PWD/scripts/sandbox/claude-in-podman.sh" \
  --roots "$HOME/workspace/<repo>"
```

The wrapper (`claude-in-podman.sh`) bind-mounts, at identical host paths:

- the **repo root** (covers the task worktree + the shared `.git`, so git
  worktree links and claude's cwd-hash session resume work);
- **`~/.claude`** (dir) and **`~/.claude.json`** (file) — reuses your host login,
  session store, and config. Without the latter claude warns "config not found"
  and rewrites it every run.

## Security model (what's confined, what isn't)

- **Confined:** filesystem outside the mounted repo, host processes, the rest of
  your home. The agent sees only the repo it's working in (+ `~/.claude`).
- **Exposed (intentional):** the mounted repo worktree (that's where edits go),
  plus — in the default **mount auth** mode — `~/.claude` + `~/.claude.json`,
  bind-mounted **read-write** (login/session/config; the container can read your
  full claude config, **including the permanent refresh token**, and can rewrite
  it). Together with the worktree that is the complete set of writes that outlive
  the container. They cannot be made read-only in this mode — claude writes its
  session store there, which is the whole point of mount auth. Use **token auth**
  (below) to remove the exposure instead; it keeps `~/.claude` inside the image's
  own ephemeral home. Do **not** treat the container as a boundary against a
  hostile agent; it reduces *accidental* blast radius for dogfood use.

### Authentication

- **Mount auth (default):** bind-mounts `~/.claude` so the sandbox reuses your
  host login + session resume. Simplest, but the personal **refresh token** is in
  the container — combined with open egress + untrusted input that's a real
  exfil-to-permanent-compromise risk (mitigate with `--firewall-proxy`).
- **Token auth (hardened, recommended for untrusted work):** put a dedicated,
  revocable token in a file and the wrapper authenticates via
  `CLAUDE_CODE_OAUTH_TOKEN` **without mounting `~/.claude`** — so a leak means
  revoking *that one token*, not your account. Session state is ephemeral, so
  **`--continue` / resume do NOT work** in token auth. Need resume on a given
  task? pass **`--mount-auth`** to force mount auth (host `~/.claude`, resume
  works) even when the token file is present — trading the refresh-token exposure
  back in for that task. One-time setup:

  ```sh
  claude setup-token            # interactive; prints a long-lived token
  mkdir -p ~/.config/harness
  ( umask 077; printf '%s\n' '<the token>' > ~/.config/harness/sandbox-claude-token )
  ```

  The wrapper auto-detects that file (override path with
  `HARNESS_SANDBOX_CLAUDE_TOKEN_FILE`) and switches to token auth; it never reads
  the token's bytes (hands it to podman as an env). The token is *long-lived*, not
  short-TTL — the win is "dedicated + revocable", not "harmless if leaked".
- **harness control plane bridged in (default):** the host `harness-cli` binary
  is mounted onto PATH and the runner's `HARNESS_*` env is forwarded, so the
  confined agent can still `submit` / agentboard / file-transfer. This re-grants
  the harness control plane (a deliberate agent could spawn an unsandboxed task
  and escape) — fine for trusted dogfood, where the goal is preventing *accidental*
  host damage, not adversarial containment.

  **Two-layer model:** the podman sandbox confines local filesystem, processes,
  and network (OS layer); the server-enforced capability bitmask confines what
  the agent can request from the harness control plane (server layer). Neither
  layer configures the other — they compose.

  **Capability middle ground (without removing the control plane):** the
  sandbox-escape risk above is now closable without `--omit-harness-cli`. Spawn
  the sandboxed task with a restricted capability set via `submit --caps` or
  `session new --caps` (the server enforces `caps_child = caps_parent ∩
  requested` at spawn time — the sandboxed task's own caps cannot exceed what
  its parent granted). Examples:

  - `submit --caps none` — data-plane only: the agent can still use
    `agent send` / `inbox` to talk to its parent, but the server denies
    `spawn`, `file_read`, `file_write`, `forward_local`, `forward_remote`,
    `exec_attach`, `notify`, and all other control-plane operations with
    `PermissionDenied`. The escape path (spawn an unsandboxed child) is
    closed server-side regardless of what's in the container.
    Its own task subtree (`ls`, `logs`) stays readable — that needs no cap —
    as does a redacted row for its direct parent: status and busy/idle only,
    with the repo, worktree, prompt and assigned runner stripped.
  - `submit --caps info_global` — widens that to the full-board view (every
    task, every runner, the whole agentboard topic list) without granting
    spawn or file-write.

  For full control-plane removal (no harness-cli at all inside the container),
  use `--agent-arg --omit-harness-cli` / `--agent-args "--omit-harness-cli"`.
  (Bridge assumes the server is directly reachable; behind
  `HARNESS_PROXY_VIA_RUNNER` it would need `--network=host` — not handled yet.)
- **Network: open by default; opt-in egress allowlist via `--firewall`.** Pass
  `--agent-arg --firewall` (or runner `--agent-args "--firewall"`) to apply a
  default-deny iptables+ipset allowlist inside the container — GitHub IP ranges
  (api.github.com/meta) + npm/anthropic/pypi + the harness server (**its one
  port**) + the default-route gateway (**/32**), IPv6 blocked, everything else
  REJECTed. Two more deltas from upstream, both narrowing: upstream's blanket
  `--dport 22 ACCEPT` (ssh to *any* host — a general-purpose tunnel that reopens
  what the default-DROP closes) is dropped, and the harness server is allowed as
  an `ip:proto:port` rule rather than a bare ipset address, which had left every
  other port on that machine — its sshd included — reachable from the sandbox
  (measured 2026-08-18). git-over-ssh to GitHub still works: github.com is in the
  ipset by address, like every other allowlisted service. If the port can't be
  parsed out of `HARNESS_SERVER_CID` the carve-out is skipped entirely
  (harness-cli then fails) rather than widened back to the host.
  Upstream allowlists that gateway's whole /24, which stays inside the bridge
  network under docker; podman rootless defaults to **pasta**, which hands the
  container the host's own interface, so the same rule would allowlist your
  entire LAN. This kit narrows it to the gateway /32 — the harness server is
  reached through its own /32, not through that rule. Adapted from Anthropic's `init-firewall.sh`; runs as
  container-root (needs `--user 0` + `NET_ADMIN`/`NET_RAW`, added automatically)
  then drops to the agent user. **Fail-closed:** if the firewall can't be applied
  the task aborts rather than running unconfined. Two behaviours to know: (1)
  client-side `WebFetch` can only reach allowlisted hosts under `--firewall`
  (server-side `WebSearch`, which goes via `api.anthropic.com`, is unaffected);
  (2) claude's non-essential egress (telemetry → Datadog, statsig feature-flags,
  auto-update, error reporting) is disabled in-container via
  `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`, so the allowlist needn't include
  those endpoints and fail-closed won't stall on them.
- **`--firewall-proxy` (stronger egress):** instead of an IP allowlist, deny ALL
  raw egress for the agent uid (iptables owner-match) and run an in-container
  allowlisting CONNECT proxy (`cmd/sandbox-connect-proxy`) as a separate uid; the agent's
  API/WebFetch are forced through it via `HTTPS_PROXY`. Wins over `--firewall`:
  client-side **WebFetch works** (routed through the proxy), domain-based allowlist
  (no CDN-IP rotation / ipset), and injected code cannot open a raw socket at all.
  Default proxy allowlist = `api.anthropic.com` + github/npm/pypi; extend for
  WebFetch research targets by setting `SANDBOX_PROXY_ALLOW=domain1,domain2` in the
  runner env. harness-cli is unaffected (direct L3 carve-out to the harness
  server's own `ip:proto:port`; it doesn't use `HTTPS_PROXY`). Residual: a CONNECT proxy sees SNI/host
  only (no TLS-body inspection without MITM) — it closes raw-socket and
  non-allowlisted exfil, but cannot stop exfil *to an allowlisted domain* (e.g. a
  GitHub gist, since `github.com` is allowlisted). Trim the allowlist for
  sensitive tasks; full DLP would need a MITM proxy (out of scope).

### Verifying the above, instead of trusting it

`probe.sh` runs **inside** the container and reports the measured state of every
claim in this section. Its first two lines are `config_firewall` / `config_auth`,
naming the configuration the run actually measured — read every later line
against them. The modes are not interchangeable: the ip allowlist leaves udp/53
open to any host and the proxy mode does not, upstream's blanket ssh rule existed
only in ip mode, and `~/.claude` is a host-persistent write surface under mount
auth but absent under token auth. A mode-less "blocked" reads later as "blocked
in every mode", which is how partial coverage passes for full. The rest — what the mounts actually expose, whether host processes
are visible, which egress succeeds (it probes one allowlisted target *and* one
that must be refused), whether the harness-server carve-out is still one port
(`server_other_port` must not read `open` — that is how the host-wide carve-out
above was found, and curl can't see it because it is L4 on a non-HTTP port), and
what caps the server grants the bridged control plane.
Run it as a task on the sandbox slot; `--topic` also publishes the report to the
agentboard so a supervising agent needn't read logs:

```sh
harness-cli submit --repo <sandbox-root> \
  --task 'bash scripts/sandbox/probe.sh --topic chat.<your-task-id>'
```

## Scope / roadmap

- **one-shot / print mode (`claude -p`):** rootless, keep-id, FS confinement.
  Verified end-to-end through a real runner. ✅
- **interactive:** the runner runs the wrapper under a real PTY, so the wrapper
  adds `podman -t` when its stdin is a terminal (and omits it for the `-p` pipe,
  which `-t` would corrupt). Verified end-to-end through a detached session
  (`session new -d`): container comes up with `tty=true`, the TUI renders in
  `session snapshot`, injected keystrokes reach it, and a real instruction
  round-trips — claude ran `id -un`/`pwd` and answered
  `node@…/.harness-worktrees/<task-id>`, i.e. it executed as the container user
  in the bind-mounted worktree. ✅
  It used to open on claude's "Quick safety check … this folder pre-approves 1
  tool permission in `.claude/settings.json`" every time: trust is keyed on the
  **repo root**, while the launcher seeded only `$PWD` (the task worktree). The
  wrapper now passes the mounted roots as `SANDBOX_SEED_PROJECTS` and the
  launcher seeds each. The one-shot warning named the same path all along —
  "set `projects["/…/<repo>"].hasTrustDialogAccepted: true`" — and under `-p`
  it is not a prompt but a silent drop of the injected rule.
  `session snapshot` still warns "reported no terminal size; rendering at
  120x40" for a session opened with no attached terminal; that is the
  renderer's fallback, not the container's PTY.
- **egress firewall (opt-in `--firewall`):** default-deny iptables+ipset allowlist
  (GitHub ranges + npm/anthropic/pypi + the harness server), IPv6 blocked, applied
  as container-root then dropped to the agent user. Adapted from Anthropic's
  `init-firewall.sh`; fail-closed. Verified end-to-end (blocked domains rejected,
  allowed reachable, claude runs + writes the worktree as the host user). ✅
- **proxy-broker egress (opt-in `--firewall-proxy`, stronger):** an in-container
  allowlisting CONNECT proxy (`cmd/sandbox-connect-proxy`) runs as a dedicated uid; iptables
  **owner-match** gives the agent uid NO raw-socket egress, so its API + WebFetch
  funnel through the proxy (domain allowlist, no CDN-IP fragility, **WebFetch
  works** unlike the IP allowlist). harness-cli still reaches the harness server
  via a direct L3 carve-out on that one port (it doesn't honor `HTTPS_PROXY`).
  Verified end-to-end (allowed via proxy, denied refused, raw-socket bypass
  blocked, claude runs + writes the worktree as the host user). ✅
- **PID 1 / zombie reaping (`podman run --init`):** the whole command chain
  execs (`entrypoint.sh` → `gosu` → `claude-launch.sh` → `claude`), so without
  `--init` **claude itself is PID 1** — and node/libuv only `waitpid()`s the pids
  it spawned, never reparented orphans. Every background job whose own parent
  shell exited (agent `run_in_background` commands, nohup'd builds) therefore
  accumulated as `<defunct>` for the container's whole life; observed
  2026-08-18 as three stuck `python3 <defunct>` under a live sandbox agent.
  `--init` puts catatonit at PID 1 and claude under it. The `--firewall-proxy`
  broker needed a second fix: `entrypoint.sh` starts it before its own `exec`,
  so it is a direct child of the pid that *becomes* claude and `--init` cannot
  see it — it is now double-forked (`( … & )`) onto catatonit.
  Verified end-to-end through the wrapper with a node stand-in for claude:
  without `--init`, `PID1=node` and an orphan left 1 zombie; with it,
  `PID1=podman-init` and 0. TTY input, foreground process group
  (`PGRP == TPGID`), exit-code propagation, the `gosu` uid drop and the
  proxy-mode firewall all re-checked unchanged under a real PTY. ✅
