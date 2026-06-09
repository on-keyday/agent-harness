# Claude Code sandbox kit (rootless podman)

Run a runner's spawned `claude` confined inside a **rootless podman** container
instead of directly on the host, to shrink the blast radius of an agent that
runs with `--dangerously-skip-permissions`. No harness core changes: it plugs in
through the existing `--claude-bin` seam.

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

Point `--claude-bin` at the wrapper; the runner spawns claude through podman:

```sh
scripts/runner.sh up --as sandboxed \
  --claude-bin "$PWD/scripts/sandbox/claude-in-podman.sh" \
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
  plus — in the default **mount auth** mode — `~/.claude` + `~/.claude.json`
  (login/session/config; the container can read your full claude config,
  **including the permanent refresh token**). Use **token auth** (below) to remove
  that exposure. Do **not** treat the container as a boundary against a hostile
  agent; it reduces *accidental* blast radius for dogfood use.

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
  host damage, not adversarial containment. Disable per task or per runner with
  `--claude-arg --omit-harness-cli` / `--claude-args "--omit-harness-cli"` for full
  isolation. (Bridge assumes the server is directly reachable; behind
  `HARNESS_PROXY_VIA_RUNNER` it would need `--network=host` — not handled yet.)
- **Network: open by default; opt-in egress allowlist via `--firewall`.** Pass
  `--claude-arg --firewall` (or runner `--claude-args "--firewall"`) to apply a
  default-deny iptables+ipset allowlist inside the container — GitHub IP ranges
  (api.github.com/meta) + npm/anthropic/pypi + the harness server, IPv6 blocked,
  everything else REJECTed. Adapted from Anthropic's `init-firewall.sh`; runs as
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
  allowlisting CONNECT proxy (`connect-proxy.py`) as a separate uid; the agent's
  API/WebFetch are forced through it via `HTTPS_PROXY`. Wins over `--firewall`:
  client-side **WebFetch works** (routed through the proxy), domain-based allowlist
  (no CDN-IP rotation / ipset), and injected code cannot open a raw socket at all.
  Default proxy allowlist = `api.anthropic.com` + github/npm/pypi; extend for
  WebFetch research targets by setting `SANDBOX_PROXY_ALLOW=domain1,domain2` in the
  runner env. harness-cli is unaffected (direct L3 carve-out to the harness
  server; it doesn't use `HTTPS_PROXY`). Residual: a CONNECT proxy sees SNI/host
  only (no TLS-body inspection without MITM) — it closes raw-socket and
  non-allowlisted exfil, but cannot stop exfil *to an allowlisted domain* (e.g. a
  GitHub gist, since `github.com` is allowlisted). Trim the allowlist for
  sensitive tasks; full DLP would need a MITM proxy (out of scope).

## Scope / roadmap

- **one-shot / print mode (`claude -p`):** rootless, keep-id, FS confinement.
  Verified end-to-end through a real runner. ✅
- **interactive:** the runner runs the wrapper under a real PTY, so the wrapper
  adds `podman -t` when its stdin is a terminal (and omits it for the `-p` pipe,
  which `-t` would corrupt). TTY plumbing verified; live TUI rendering/resize
  through podman still wants real-world confirmation. ⚠️
- **egress firewall (opt-in `--firewall`):** default-deny iptables+ipset allowlist
  (GitHub ranges + npm/anthropic/pypi + the harness server), IPv6 blocked, applied
  as container-root then dropped to the agent user. Adapted from Anthropic's
  `init-firewall.sh`; fail-closed. Verified end-to-end (blocked domains rejected,
  allowed reachable, claude runs + writes the worktree as the host user). ✅
- **proxy-broker egress (opt-in `--firewall-proxy`, stronger):** an in-container
  allowlisting CONNECT proxy (`connect-proxy.py`) runs as a dedicated uid; iptables
  **owner-match** gives the agent uid NO raw-socket egress, so its API + WebFetch
  funnel through the proxy (domain allowlist, no CDN-IP fragility, **WebFetch
  works** unlike the IP allowlist). harness-cli still reaches the harness server
  via a direct L3 carve-out (it doesn't honor `HTTPS_PROXY`). Verified end-to-end
  (allowed via proxy, denied refused, raw-socket bypass blocked, claude runs +
  writes the worktree as the host user). ✅
