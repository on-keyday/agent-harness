# Agent sandbox kit (rootless podman)

Run a runner's spawned agent — **claude, codex, agy, opencode, or a plain bash
shell** — confined inside a **rootless podman** container instead of on the host,
to shrink the blast radius of an agent that runs with
`--dangerously-skip-permissions`. No harness core changes: it plugs in through
the existing `--agent-bin` seam.

## Supported agents

Which agent runs is `--sandbox-agent NAME` (default `claude`), and every
per-agent difference lives in ONE table at the top of `agent-in-podman.sh`.
An unknown name exits 2 rather than falling back to claude: a slot quietly
running the wrong agent looks identical to the right one in every listing.

| agent | host binary bridged in | config mounted (mount auth) | token auth | image fallback |
|---|---|---|---|---|
| claude | `~/.local/bin/claude` | `~/.claude` + `~/.claude.json` | yes (`setup-token`) | yes (npm copy) |
| codex | `~/.local/bin/codex` | `~/.codex` | **no** | no |
| agy | `~/.local/bin/agy` | `~/.gemini` | **no** | no |
| opencode | `/usr/bin/opencode` | `~/.config/opencode` + `~/.local/share/opencode` + `~/.cache/opencode` + `~/.local/state/opencode` | **no** | no |
| bash | — (the image's own) | — | — | yes |

No agent is installed into the image: the host binary is bind-mounted over the
container path. Measured 2026-08-18 that all three run on the image's glibc
2.36 — claude is a Bun ELF, agy is glibc-dynamic (197MB), codex is static-pie
musl and so libc-independent by construction; opencode (Bun ELF, added
2026-08-19) likewise. Only claude keeps an image copy, as a fallback; the others
hard-error when their host binary is missing, because exec'ing a container path
that does not exist surfaces several layers from the actual cause.

opencode is the one whose host binary is **not** under your home: the Arch
package installs it at `/usr/bin/opencode`. Nothing in the bridge cares, but
check which one `opencode` resolves to before assuming — the upstream
curl/npm installer puts a *separate* copy at `~/.opencode/bin/opencode` and
prepends that to `PATH` in `~/.bashrc`, so a machine with both silently runs the
older one everywhere, this wrapper included.

**codex, agy and opencode are mount-auth only.** None has a known
revocable-token mode of the kind claude's `setup-token` provides, so running them
here puts that provider's credentials in the container with no narrower option:
`~/.codex/auth.json` for codex, `~/.gemini` for agy — the latter shared with
gemini-cli, so it carries that product's credentials too (tier-ineligible on
this host, but present) — and `~/.local/share/opencode/auth.json` for opencode.
`--firewall-proxy` is the mitigation that matters for them, since it removes
raw-socket egress entirely.

**opencode needs four mounts, and each is load-bearing.** It spreads across all
four XDG directories: config + plugins, `auth.json` + `opencode.db`, the
models.dev catalog cache, and `model.json` + `locks/`. Omitting one is not a
degraded mode but a failed run — HOME is the host path and podman creates mount
parents root-owned, so anything opencode tries to `mkdir` under HOME that the
table does not name aborts before the first token (`EACCES: mkdir
'$HOME/.cache'`). Dropping the data dir would be worse than an abort: it
authenticates fine and then makes `run --continue` find no session, i.e. a
resume that silently starts a new conversation.

**Which agent runs is decided by the BIN, not by a flag.** Each preset points
at that agent's `<agent>-in-podman.sh` symlink, all of which are the same
script; it reads `basename $0`. That is not decoration: the runner builds a
FRESH interactive launch as bin + args with no argv template at all
(`runner/agent_command.go` `buildInteractiveArgs` returns the extra args
unchanged when `resumeConversation` is false), so a selector carried in the
oneshot/resume templates is simply absent on that path. It was, and
`session new --agent sandbox-bash` opened Claude Code while the task row still
read `agent=sandbox-bash`. `--sandbox-agent NAME` still works as a manual
override for direct invocation.

**Addressing the default profile.** The FIRST name in `--agents` becomes the
runner's unnamed default profile, addressed by its bin basename rather than by
the preset name: `--agent claude-in-podman.sh`, not `--agent sandbox-claude`.
`harness-cli session new` without `--agent` lists the exact flag for each
profile when more than one is configured, so this is discoverable, but it is
worth knowing before you type the name you expected to work.

**What each agent actually writes, measured through a real runner slot:**

- **bash**, **claude** and **opencode** write into the task worktree, owned by the
  host user (`kforfk:kforfk`) — this is what `--userns=keep-id` buys, verified end
  to end. opencode measured 2026-08-19: asked for a file in the cwd, it wrote one
  containing `node` and the worktree path, and the host saw it as `kforfk:kforfk`.
  It needs `--auto` (its own auto-approve-permissions flag) to act without a
  prompt in one-shot mode — the caller's business via `--agent-args`, exactly as
  `--dangerously-skip-permissions` is for claude.
- **codex** refused: its own sandbox reports the environment read-only for
  `codex exec`, and its edit tool fails with `codex-code-mode-host` not found
  (that helper is not in the image). Give it its own approval/sandbox flags via
  `--agent-args` if you want it writing in the worktree — the wrapper will not
  decide that for you.
- **agy** created the file in `~/.gemini/antigravity-cli/scratch/`, NOT in the
  worktree — and since mount auth bind-mounts `~/.gemini` read-write, that write
  landed on the **host's real config directory** and outlived the container.
  Expected given the mount, but worth stating plainly: agy's default file
  destination is inside the credential directory you mounted.

**codex brings its own sandbox.** It warns `could not find bubblewrap on PATH …
will use the bundled bubblewrap` and then works — nested inside podman. The
wrapper does not disable it: approval / sandbox flags are the caller's business
via `--agent-args`, exactly as `--dangerously-skip-permissions` is for claude.

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

Produces `harness-agent-sandbox:latest` (override via `HARNESS_SANDBOX_IMAGE`):
`node:22` base + `git` + `ripgrep` + `python3`/`pip`/`venv` + the egress-firewall
tools (`iptables`/`ipset`/`iproute2`/`dnsutils`/`aggregate`/`jq`/`gosu`) +
`@anthropic-ai/claude-code`.

## Use it from a runner

The `sandbox-*` presets carry the wrapper path AND each agent's argv templates,
so a slot gets them without hand-typing `--agent-bin`:

```sh
scripts/runner.sh up --as sandboxed \
  --agents sandbox-claude,sandbox-codex,sandbox-agy,sandbox-opencode,sandbox-bash \
  --roots "$HOME/workspace/<repo>"
```

`sandbox` remains an alias of `sandbox-claude`. Each preset is DERIVED from its
base agent's entry with only `bin` changed and `--sandbox-agent <name>`
prefixed, so a sandboxed slot cannot drift from its unsandboxed twin — the
first version of this preset did drift, and one-shot progress arrived as a
single final blob instead of streamed events.

### Upgrading an existing sandbox slot

The wrapper and image were renamed (`claude-in-podman.sh` →
`agent-in-podman.sh`, `harness-claude-sandbox` → `harness-agent-sandbox`). A
running runner slot records its `--agent-bin` argv and
`scripts/build_and_restart_all.py` **replays the running argv**, so restart-all
will not migrate it — it re-execs the old path. Stop the slot and bring it back
up with the presets above (`systemctl --user restart <unit>` for a registered
slot, or `runner.sh down` then `runner.sh up --agents sandbox-…`).

The wrapper (`agent-in-podman.sh`) bind-mounts, at identical host paths:

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

- **Mount auth (default, and the ONLY mode for codex, agy and opencode):** bind-mounts the
  agent's config paths (see the table above) so the sandbox reuses your host
  login + session resume. Simplest, but the personal **refresh token** is in
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
  sandbox-escape risk above is closed by DEFAULT — an omitted `--caps` grants
  nothing, so a sandboxed task has no control plane to escape through unless
  the spawn asked for one. `submit --caps` / `session new --caps` widen it
  within what the spawner holds (the server enforces `caps_child = caps_parent
  ∩ requested` at spawn time — the sandboxed task's own caps cannot exceed what
  its parent granted). Examples, of which the first is now what you get for
  free:

  - `submit --caps none` (the default) — data-plane only: the agent can still use
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
  (api.github.com/meta) + the shared dev hosts (npm / pypi) + **the running
  agent's own endpoints** (`SANDBOX_AGENT_DOMAINS`, from the agent table — so a
  codex container allowlists chatgpt.com and not anthropic's API) + the harness
  server (**its one port**) + the default-route gateway (**/32**), IPv6 blocked, everything else
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
  Default proxy allowlist = github/npm/pypi (shared) + the agent's own
  domains from the table; extend for
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
  (GitHub ranges + npm/pypi + the agent's own endpoints + the harness server),
  IPv6 blocked, applied
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
  execs (`entrypoint.sh` → `gosu` → `agent-launch.sh` → `claude`), so without
  `--init` **claude itself is PID 1** — and claude is an application, not an
  init: it waits on the pids it spawned and on nothing else, so anything
  *reparented* onto it is never reaped. Every background job whose own parent
  shell exited (agent `run_in_background` commands, nohup'd builds) therefore
  accumulated as `<defunct>` for the container's whole life; observed
  2026-08-18 as three stuck `python3 <defunct>` under a live sandbox agent.
  `--init` puts catatonit at PID 1 and claude under it. The `--firewall-proxy`
  broker needed a second fix: `entrypoint.sh` starts it before its own `exec`,
  so it is a direct child of the pid that *becomes* claude and `--init` cannot
  see it — it is now double-forked (`( … & )`) onto catatonit.
  Verified end-to-end through the wrapper with the **real** binary
  (`claude=host:2.1.234`), one orphaned `setsid sleep 1`: without `--init`,
  `PID1COMM=claude` and 1 zombie; with it, `PID1COMM=podman-init` and 0.
  TTY input, foreground process group (`PGRP == TPGID`), exit-code propagation,
  the `gosu` uid drop and the proxy-mode firewall all re-checked unchanged
  under a real PTY. ✅
  Do not reason about this from claude's install layout: it lives under
  `node_modules` and its binary is named `claude.exe`, but it is a Bun-compiled
  single-file **ELF**, not a node process. A stand-in used to model PID 1 must
  match that — a `bash` stand-in silently invalidates the test, because bash as
  PID 1 *does* reap orphans and shows 0 zombies either way.
