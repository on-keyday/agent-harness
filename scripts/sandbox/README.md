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
`node:22` base + `git` + `ripgrep` + `@anthropic-ai/claude-code`.

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
  plus `~/.claude` + `~/.claude.json` (login/session/config — the container can
  read and modify your full claude config). Do **not** treat this as a boundary
  against a hostile agent — it reduces *accidental* blast radius for dogfood use.
  A stricter setup (creds-only mount + dedicated config) is a v2 hardening.
- **Network: open (v1).** Egress is unrestricted for now.

## Scope / roadmap

- **one-shot / print mode (`claude -p`):** rootless, keep-id, FS confinement.
  Verified end-to-end through a real runner. ✅
- **interactive:** the runner runs the wrapper under a real PTY, so the wrapper
  adds `podman -t` when its stdin is a terminal (and omits it for the `-p` pipe,
  which `-t` would corrupt). TTY plumbing verified; live TUI rendering/resize
  through podman still wants real-world confirmation. ⚠️
- **v2 (TODO):** egress allowlist firewall (adapt Anthropic's `init-firewall.sh`,
  iptables in the container netns). Network is currently open.
