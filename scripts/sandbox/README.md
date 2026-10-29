# Agent sandbox kit (rootless podman)

Run a runner's spawned agent — **claude, codex, agy, opencode, pi, or a plain
bash shell** — confined inside a **rootless podman** container instead of on the host,
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
| codex | resolved release `bin/` directory, including helpers | `~/.codex` | **no** | no |
| agy | `~/.local/bin/agy` | `~/.gemini` | **no** | no |
| opencode | `/usr/bin/opencode` | `~/.config/opencode` + `~/.local/share/opencode` + `~/.cache/opencode` + `~/.local/state/opencode` | **no** | no |
| pi | the `pi-coding-agent` package directory (ro) | `~/.pi` | **no** | no |
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
`HARNESS_SANDBOX_OPENCODE_BIN` can select an exact candidate binary without
replacing the installed `/usr/bin/opencode` (the upstream probe uses this).

Codex is a release bundle rather than a single executable. The wrapper resolves
`~/.local/bin/codex` and mounts its containing directory read-only at
`/opt/harness-codex-bin`. Codex then finds `codex-code-mode-host` beside itself;
mounting only the main executable at `/usr/local/bin/codex` left tool calls
failing before a command could run. Other executable siblings in the same
release follow it automatically. The launcher puts the selected executable's
directory first on `PATH` for subprocesses too.

pi is a node **package**, not one binary: its `dist/bundle/cli.js` loads its
siblings and `node_modules/` by relative path, so the whole package directory
is mounted read-only at its host path and `cli.js` runs there on the image's
node (v22.22.3; pi requires >= 22.19). The package is
`HARNESS_SANDBOX_PI_PKG` if set, else the one the `pi` on `PATH` links to, else
the highest-versioned copy under `~/.nvm/versions/node/*` — the case where `pi`
is a wrapper script pinning an nvm interpreter rather than npm's symlink.
`--print`, `--continue` and an interactive session were verified on pi 1.0.3
(2026-10-17), as was `--firewall-proxy`, through which pi needs only its
provider host. **The allowlisted `openrouter.ai` is the provider of the host
this was measured on, not pi's**: on a host whose `~/.pi/agent` uses another
provider, set `HARNESS_SANDBOX_PI_DOMAINS=<host>[,<host>…]` in the runner's
environment. It replaces the list in both firewall modes (`SANDBOX_PROXY_ALLOW`
would reach `--firewall-proxy` only). To find the name, run once and read the
`DENY` lines in the proxy log (the runner log's `[agent-in-podman]` line names
the file). The wrapper turns off pi's version check on every run
(`PI_SKIP_VERSION_CHECK`; the read-only package cannot be updated from inside)
and its remaining automatic traffic under a firewall (`PI_OFFLINE`).

**codex, agy, opencode and pi are mount-auth only.** None has a known
revocable-token mode of the kind claude's `setup-token` provides, so running them
here puts that provider's credentials in the container with no narrower option:
`~/.codex/auth.json` for codex, `~/.gemini` for agy — the latter shared with
gemini-cli, so it carries that product's credentials too (tier-ineligible on
this host, but present) — `~/.local/share/opencode/auth.json` for opencode, and
`~/.pi/agent/auth.json` for pi. The `~/.pi` mount is also read-write for
`settings.json` and `extensions/`, which the host's own pi loads and runs — the
same host-execution surface `~/.claude`'s hooks are under mount auth.
The default `--firewall-proxy` mode is the mitigation that matters for them,
since it removes raw-socket egress entirely; `--no-firewall` gives that up.

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

- **bash**, **claude**, **opencode** and **pi** write into the task worktree,
  owned by the host user (`kforfk:kforfk`) — this is what `--userns=keep-id`
  buys, verified end to end. pi measured 2026-10-17 the same way as opencode
  below, with no prompt to answer. opencode measured 2026-08-19: asked for a file in the cwd, it wrote one
  containing `node` and the worktree path, and the host saw it as `kforfk:kforfk`.
  It needs `--auto` (its own auto-approve-permissions flag) to act without a
  prompt in one-shot mode; the `sandbox-opencode` preset passes it.
- **codex** previously failed to start tools because the single-file bridge
  omitted `codex-code-mode-host`. The release-directory mount fixes that missing
  executable. Codex 0.164.0 and its helper both start inside the image in the
  credential-free runtime probe. This is a packaging/startup test, not a claim
  that an authenticated model session has exercised editing. Since 2026-10-17
  the `sandbox-codex` preset passes
  `--dangerously-bypass-approvals-and-sandbox`; the outer container remains the
  confinement boundary.
- **agy** created the file in `~/.gemini/antigravity-cli/scratch/`, NOT in the
  worktree — and since mount auth bind-mounts `~/.gemini` read-write, that write
  landed on the **host's real config directory** and outlived the container.
  Expected given the mount, but worth stating plainly: agy's default file
  destination is inside the credential directory you mounted.

**codex brings its own sandbox, and it cannot start in here.** It runs
bubblewrap, which needs a user namespace, and the container refuses those by
default (see "User namespaces" under the security model). It also needs a
`bwrap` binary: the image ships none, and codex 0.165.0 bundles none either
(measured 2026-10-29: `codex sandbox -- …` panics with `bubblewrap is
unavailable`). The bundled bubblewrap codex used on 2026-08-18 is gone. The
`sandbox-codex` preset turns codex's sandbox off with the flag above, so neither
gap reaches it.

**Each agent's act-without-asking flag is on the PRESET, not the slot.** Every
`sandbox-*` preset carries it as the profile's `agentArgs` (`_SANDBOX_AGENT_ARGS`
in `scripts/agent_presets.py`); no unsandboxed preset carries one. It used to be
the slot's `--agent-args`, which agent-runner applies to the default profile
only, so an extra `sandbox-codex` profile never received its flag. The same
limit still applies to the wrapper controls below: runner `--agent-args
"--firewall"` reaches the default profile only, while a per-task `--agent-arg`
reaches whichever profile the task runs.

pi's is `--approve`, and it answers a different question: pi asks nothing per
tool call, but asks whether to trust the project folder, and the
`.agents/skills` the runner injects into every worktree is behind that gate.
Without it an interactive task opened on pi's "Trust project folder?" dialog,
and `--print`, which cannot show the dialog, skips the skills silently unless
`~/.pi/agent/trust.json` already trusts the path. Answering the dialog writes
the worktree's path into that file, on the host, once per task.

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
- `python3` on the host: the wrapper derives the container's seccomp profile
  with it on every start (`--allow-userns` skips that).

## Regression and upstream release checks

`make scripts-test` runs the offline preset, wrapper-mount, launcher-PATH, and
release-source regressions. Both `make test` and the ordinary CI test job run
this target. These tests use temporary fixtures and make no network requests or
model calls; they do not require podman or installed agents.

To inspect the official current release metadata for all agents:

```sh
python3 scripts/sandbox/check_agent_releases.py --agent all
```

To download exact release candidates and test startup through the production
wrapper and sandbox image:

```sh
scripts/sandbox/build.sh
make sandbox-upstream
# One agent only:
python3 scripts/sandbox/check_agent_releases.py --agent codex --smoke
# Check the installed local Codex bundle instead of downloading one:
python3 scripts/sandbox/check_codex_release.py --smoke
```

The upstream sources are OpenAI's Codex GitHub Releases, Anthropic's official
npm packages, Antigravity's official updater manifest (the endpoint used by
`https://antigravity.google/cli/install.sh`), OpenCode's GitHub Releases, and
`@earendil-works/pi-coding-agent` on npm. Bash is supplied by the image and has
no separate agent-release query. Linux x86_64 and aarch64 are supported.

Downloads are checksum-verified and extracted into temporary directories; the
installed host agents are not updated. Pi's declared production dependencies
are prepared there using npm with lifecycle scripts disabled. Probes use an
empty, disposable HOME, `--omit-harness-cli`, and `--network=none`, then run the
image launcher with `--version`; Codex additionally runs its code-mode helper
with `--help`. Host credentials and conversation directories are not mounted.
This catches missing binaries, changed package layouts, incompatible runtime
dependencies, and startup failures. It does not test login, inference, or the
semantics of model-driven tool calls.

For codex the probe also runs `codex_storage_probe.py` inside the container:
the storage behaviour `--history-window` depends on, checked with synthetic
rollouts in both on-disk formats against a disposable `CODEX_HOME`. A fresh
state DB must list and resume them, filtered by cwd. It must pick up a rollout
added after startup. A hardlinked rollout must keep its inode. Compression and
the paginated migration must still be off by default. A failure fails the
probe. Nothing runs these on a schedule, so **after updating codex on a host,
run `python3 scripts/sandbox/check_codex_release.py --smoke`** before relying
on codex's history window.

JSON reports distinguish `runtime_tested: false` (metadata only) from an actual
successful startup probe. An unavailable registry or an unrecognized package
is an error, not a compatibility success, and one agent's failure does not hide
the remaining agents' results. New unrecognized companion artifacts require
updating the adapter; the Codex bin-directory mount already carries new sibling
executables automatically.

The **Sandbox upstream compatibility** GitHub Actions workflow runs these
checks on sandbox-related pull requests/main pushes and can be dispatched
manually with one agent or all agents. It builds the image from this checkout.
It is separate from offline regressions so external release availability does
not affect ordinary local unit tests. No periodic schedule or host auto-update
is configured.

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
  --agents sandbox-claude,sandbox-codex,sandbox-agy,sandbox-opencode,sandbox-pi,sandbox-bash \
  --roots "$HOME/workspace/<repo>"
```

`sandbox` remains an alias of `sandbox-claude`. Each preset is DERIVED from its
base agent's entry with only `bin` and `agentArgs` changed, so a sandboxed slot
cannot drift from its unsandboxed twin — the
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

- the **task worktree** (rw) and the shared **`.git`**, so git worktree links
  and claude's cwd-hash session resume work. The `.git` is **read-only** except
  `objects/`, `refs/`, `logs/` and this worktree's own gitdir; the worktree's
  `.git` file and its gitdir's `commondir`/`gitdir` are read-only too. The repo
  root itself is not mounted: the main checkout and the other tasks' worktrees
  are not in the container. See "git directories" under the security model;
- **`~/.claude`** (dir) and **`~/.claude.json`** (file) — reuses your host login,
  session store, and config. Without the latter claude warns "config not found"
  and rewrites it every run.

## Security model (what's confined, what isn't)

- **Confined:** filesystem outside the task worktree and the repo's `.git`, host
  processes, the rest of your home. The agent sees only its own worktree (+
  `~/.claude`).
- **git directories (read-only where host git would execute them):** every git
  directory the container can write is one the host later runs git in — your
  push and `make build` in the main checkout, the runner's git in the worktree.
  git runs what a git directory names (`hooks/`, `core.hooksPath` and
  `core.fsmonitor` in `config`) and follows what it points at (the worktree's
  `.git` file, a gitdir's `commondir`), so a writable one is code execution on
  the host with the host's credentials. Hence the read-only `.git` above; commit,
  stash, rebase, reset and fetch still work (measured 2026-10-16, podman 6.0.1,
  git 2.55.0). **Cost:** deleting a ref fails inside the container
  (`git branch -d`, `git tag -d`) — the files backend takes
  `.git/packed-refs.lock` for every deletion, and a writable `.git` would let
  the container create `.git/commondir` and redirect the main checkout's git.
  Delete branches from the host. `git config` writes fail by design. Under the
  runner's NoWorktree mode the cwd *is* the checkout the host builds, so there
  is no boundary: only `hooks/` and `config` are made read-only there.
  `probe.sh`'s `git_exec_surfaces` line reports each of these.
- **User namespaces: refused unless `--allow-userns`.** podman's default
  seccomp profile lets a process create user namespaces, nested ones included
  (measured two levels below the container, uid 0 in each). Inside one, an
  unprivileged process holds `CAP_SYS_ADMIN` over kernel code that otherwise
  only root reaches. So every container starts with podman's own profile minus
  that: `clone`/`unshare` carrying `CLONE_NEWUSER` get `EPERM`, and `clone3`
  gets `ENOSYS`, whose flags seccomp cannot read, so libc falls back to `clone`.
  `seccomp_profile.py` derives it on each start from the file `podman info`
  names, so it follows podman upgrades. If podman names no file, the wrapper
  exits 2 instead of starting without one. Measured unaffected (2026-10-29):
  threads, subprocesses, node workers, git, the proxy firewall, the startup of
  all six agents, and a claude one-shot running a Bash tool. **Cost:** nothing
  that needs a user namespace runs inside, including bubblewrap (codex's own
  sandbox, above), rootless podman and `unshare`. The netem lab refuses to
  start, and `integration/ip_link_test.go` skips. `--agent-arg --allow-userns`
  gives one task podman's default profile back. The runner log line carries
  `userns=deny|allow`, and `probe.sh`'s `userns_create` measures it.
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

- **Mount auth (default, and the ONLY mode for codex, agy, opencode and pi):** bind-mounts the
  agent's config paths (see the table above) so the sandbox reuses your host
  login + session resume. Simplest, but the personal **refresh token** is in
  the container — combined with egress + untrusted input that's a real
  exfil-to-permanent-compromise risk. The default `--firewall-proxy` mode
  limits egress to allowlisted hosts; `--no-firewall` removes that limit.
  `--mount-auth` names this mode explicitly and changes nothing.
- **Token auth (`--token-auth`, claude only, opt-in):** put a dedicated,
  revocable token in a file and the wrapper authenticates via
  `CLAUDE_CODE_OAUTH_TOKEN` **without mounting `~/.claude`** — so a leak means
  revoking *that one token*, not your account. Session state is ephemeral, so
  **`--continue` / resume do NOT work** in token auth. Until 2026-10-25 the
  wrapper switched to it on its own whenever the token file existed, and
  `--mount-auth` was how a task got resume back; now it is used only when a task
  or slot passes `--token-auth`. `--token-auth` exits 2 for an agent without
  token auth, when the token file is missing or empty, and together with
  `--mount-auth`. One-time setup:

  ```sh
  claude setup-token            # interactive; prints a long-lived token
  mkdir -p ~/.config/harness
  ( umask 077; printf '%s\n' '<the token>' > ~/.config/harness/sandbox-claude-token )
  ```

  `--token-auth` reads that file (override the path with
  `HARNESS_SANDBOX_CLAUDE_TOKEN_FILE`); the wrapper never reads
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
    the three `exec_*` attach caps, `notify`, and all other control-plane
    operations with
    `PermissionDenied`. The escape path (spawn an unsandboxed child) is
    closed server-side regardless of what's in the container.
    Its own task subtree stays visible in `ls` — that needs no cap; reading a
    task's log does (`exec_view`) — as does a redacted row for its direct parent: status and busy/idle only,
    with the repo, worktree, prompt and assigned runner stripped.
  - `submit --scope global/subtree` — widens what it SEES to every task and
    every runner without granting anything to act with. The agentboard topic
    list is a separate bit, `board_observe`.

  For full control-plane removal (no harness-cli at all inside the container),
  use `--agent-arg --omit-harness-cli` / `--agent-args "--omit-harness-cli"`.
  (Bridge assumes the server is directly reachable; behind
  `HARNESS_PROXY_VIA_RUNNER` it would need `--network=host` — not handled yet.)
- **Network: the `--firewall-proxy` mode (below) by default.** Pass
  `--agent-arg --firewall` for the ip allowlist described here instead, or
  `--agent-arg --no-firewall` for an open network (exits 2 together with
  either firewall flag). Before 2026-10-25 the network was open unless a flag
  asked otherwise. `--firewall` applies a
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
- **`--firewall-proxy` (the default; stronger egress):** instead of an IP allowlist, deny ALL
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
  The proxy logs every connection (`ALLOW` / `DENY` / `BUSY`), by default to
  a host file, `${TMPDIR:-/tmp}/sandbox-proxy-<task-id>.log`, or to PATH with
  `--proxy-log=PATH`. **`--no-proxy-log`** sends them to the container's stderr
  instead (exits 2 together with `--proxy-log`). In an interactive session that
  stderr is the agent's PTY, so the lines are drawn straight over the TUI. agy
  makes this unusable, because it retries
  `POST https://play.googleapis.com/log` (Clearcut) twice every 5s and every
  attempt is a `DENY`. Each container start truncates the file and writes a
  header line naming the agent and task id, so it holds the current run only:
  the file is bind-mounted into the container, and the agent can read it.
  The runner log's `[agent-in-podman]` summary line names the file. Nothing
  deletes these files, and since 2026-10-25 every proxy-mode task writes one.
  The redirect lives in `entrypoint.sh`, so it takes effect only after
  `build.sh` rebuilds the image.

### History window (`--history-window`, claude / pi / codex, opt-in)

Mount auth puts the agent's whole config directory in the container: every
conversation you ever had with that agent, plus the settings and hooks your
host agent later loads. With `--history-window` the container instead sees
**only this task's conversations** (the ones whose cwd is this worktree), and
those stay in the host's normal store. A later run without the flag, or your
own unsandboxed agent in that worktree, resumes them as if the sandbox had not
been there.

| agent | config dir in the container | the window (host, rw) | from the host, singly |
|---|---|---|---|
| claude | empty per run (`~/.claude`, `~/.claude.json` = `{}`, seeded like token auth) | `~/.claude/projects/<cwd with every non-alphanumeric byte as ->` | `.credentials.json` (rw; not under `--token-auth`) |
| pi | empty per run (`~/.pi`) | `~/.pi/agent/sessions/--<cwd, / \ : as ->--` | `auth.json` (rw), `settings.json` and `agent/bin` (ro) |
| codex | empty per run (`~/.codex`) | a per-task directory at `~/.codex/sessions` (below) | `auth.json` (rw), `config.toml` (ro), `thread-writer-locks/` (rw) |

The empty config dir is a host temp dir (`${TMPDIR:-/tmp}/harness-sandbox-root.*`)
that the reaper removes. Everything else the agent keeps there — claude's
auto memory, prompt history, `/rewind` checkpoints, user `CLAUDE.md`, skills,
plugins, MCP config — starts empty every run and is discarded.

- **Enabling it:** per task, `--agent-arg --history-window` (the CLI, the TUI
  submit popup's args field, the WebUI). A slot's `--agent-args` does not work:
  it reaches the default profile only. **A plain resume does not carry it**:
  only `--exact` reuses a task's recorded args, so resuming without the flag
  runs that turn shared. Nothing fails; the runner log's `history=shared` is the
  only trace.
- **Refused (exit 2, no container):** opencode (one SQLite DB for every
  session), agy (not measured), bash; a cwd that is not a linked worktree
  (under `--no-worktree` the window would hold every task's conversations);
  a claude directory name over 200 bytes (claude hashes those); a missing
  credential file; for codex, a state dir on another filesystem from
  `~/.codex`. Never a silent fallback to the shared mode.
- **claude's credential file is mounted on its own.** claude replaces
  `.credentials.json` on every write, which a single-file mount cannot take,
  but its writer then falls back to writing in place (`EBUSY` is in its
  fallback set; read from claude 2.1.310, and measured on `~/.claude.json`,
  2026-10-26). Accepted risk: a crash between the truncate and the write leaves
  an empty credential file and you log in again. Re-check after a claude
  update (spec gate V2).
- **codex has no per-cwd directory** (rollouts are filed by date, with the cwd
  in their first line). The window is
  `${XDG_STATE_HOME:-~/.local/state}/harness-sandbox/codex/<task-id>/`,
  rebuilt at every start from hardlinks to the host rollouts of this cwd and
  removed by the reaper after the run — unless it holds a rollout that never
  reached the host, which is then kept and logged rather than deleted.
  The reaper links the container's new rollouts back every 5 s and once more
  after exit. It never overwrites a host file, never follows a symlink in the
  per-task directory, does not put back a rollout you archived or deleted on
  the host, and links a file only if its first line names this cwd. Then it has the **host's** codex re-index this cwd
  (`codex app-server`, one `thread/list` with scan-and-repair), because
  `resume --last` trusts the host's state DB and that DB never saw a thread
  the container created. Limits: a host `resume --last` issued *while* the
  window task runs does not see its new thread yet; after a backtrack (revert)
  in an interactive codex session in the window, the host may continue the
  pre-revert file; the shared lock dir shows every host thread's id (not its
  content); a container can rewrite a rollout's first line after it was
  linked. The last two sit inside this kit's threat model (accidental blast
  radius, not a hostile agent).
- **Every container runs in the host's timezone** (`--tz=local`, all modes,
  when the host has `/etc/localtime`):
  codex files rollouts by local time, and from the image's UTC they sorted
  hours behind the host's.

Design and measurements: `docs/superpowers/specs/2026-10-26-sandbox-history-window-design.md`.

### Verifying the above, instead of trusting it

`probe.sh` runs **inside** the container and reports the measured state of every
claim in this section. Its first two lines are `config_firewall` / `config_auth`,
naming the configuration the run actually measured — read every later line
against them. The modes are not interchangeable: the ip allowlist leaves udp/53
open to any host and the proxy mode does not, upstream's blanket ssh rule existed
only in ip mode, and `~/.claude` is a host-persistent write surface under mount
auth but absent under token auth. A mode-less "blocked" reads later as "blocked
in every mode", which is how partial coverage passes for full. `config_auth`
reads `window-root` under `--history-window`, and `config_history` names the
declared mode and lists the conversation directories actually visible, each
host-mounted one marked `(host)` (codex: the rollout count and their cwds).
The rest — what the mounts actually expose, whether host processes
are visible, whether a user namespace can be created, which egress succeeds (it probes one allowlisted target *and* one
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
- **egress firewall (`--firewall`):** default-deny iptables+ipset allowlist
  (GitHub ranges + npm/pypi + the agent's own endpoints + the harness server),
  IPv6 blocked, applied
  as container-root then dropped to the agent user. Adapted from Anthropic's
  `init-firewall.sh`; fail-closed. Verified end-to-end (blocked domains rejected,
  allowed reachable, claude runs + writes the worktree as the host user). ✅
- **proxy-broker egress (`--firewall-proxy`, the default, stronger):** an in-container
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
