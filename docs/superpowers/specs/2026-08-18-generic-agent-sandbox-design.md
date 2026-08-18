# Generic agent sandbox — one podman wrapper for claude / codex / agy / bash

## Problem

`scripts/sandbox/` confines exactly one agent. The kit's expensive parts are
agent-independent — rootless `--userns=keep-id`, the detached container reaper,
`--init` for orphan reaping, the TTY gate, the harness-cli bridge, two egress
firewall modes, `probe.sh` — but they are reachable only through
`claude-in-podman.sh`, and every claude-specific decision is inlined into that
one script. The runner already knows four agents (`scripts/agent_presets.py`:
`claude`, `codex`, `agy`, `bash`); three of them can only run unconfined on the
host, which is the opposite of the risk ordering — a shell profile and a
second-party CLI are not safer than claude, they are less inspected.

## Decision

One wrapper, `scripts/sandbox/agent-in-podman.sh`, parameterised by an **agent
table**. The agent is selected by a control flag consumed from the argv stream,
using the mechanism the wrapper already has for `--firewall` /
`--firewall-proxy` / `--mount-auth` / `--image-claude` (`claude-in-podman.sh`
lines 51-60): flags the wrapper owns are filtered out of `"$@"` before the rest
is passed through untouched.

Rejected alternative: one wrapper per agent over a shared `sandbox-lib.sh`.
Rejected because the differences between agents are *data* (a binary path, a
config directory, a domain list), and shell has no type system to keep four
copies of the control flow in step — the same reason the `sandbox` preset is
derived from the `claude` preset today rather than copied.

## Measured basis (2026-08-18, this host)

Run before designing, against the existing `harness-claude-sandbox:latest`
(node:22-bookworm-slim, glibc 2.36):

| agent | host binary | starts in image | auth via mounted config | under `--init` + `no-new-privileges` |
|---|---|---|---|---|
| codex 0.147.0 | `~/.codex/packages/standalone/current/bin/codex`, static-pie musl | yes | yes (`~/.codex`) — real model round-trip | yes |
| agy 1.1.13 | `~/.local/bin/agy`, glibc-dynamic, 197 MB | yes | yes (`~/.gemini`) — real model round-trip | yes |
| claude 2.1.234 | `~/.local/bin/claude` → `~/.local/share/claude/versions/<v>`, Bun ELF | yes (already shipping) | yes (`~/.claude` + `~/.claude.json`) | yes |
| bash | in the image | n/a | n/a | n/a |

So the host-binary bridge — already the DEFAULT for claude — generalises: no
agent needs to be installed into the image. The glibc concern about agy is
refuted, not assumed away.

**codex ships its own sandbox.** It warns `could not find bubblewrap on PATH …
will use the bundled bubblewrap` and then works. Inside podman that is nested
confinement. This spec does not disable it: the wrapper stays a pure
pass-through and agent arguments are the caller's business (`--agent-args` /
`submit --claude-arg`), exactly as `--dangerously-skip-permissions` is today.
It is documented so an operator hitting friction knows which layer to look at.

## The agent table (the single source of truth for per-agent differences)

One entry per agent, in `agent-in-podman.sh`. Every per-agent fact lives here
and nowhere else:

| field | claude | codex | agy | bash |
|---|---|---|---|---|
| `host_bin` | `~/.local/bin/claude` | `~/.local/bin/codex` | `~/.local/bin/agy` | — (image) |
| `container_bin` | `/usr/local/bin/claude` | `/usr/local/bin/codex` | `/usr/local/bin/agy` | `/bin/bash` |
| `image_fallback` | yes (npm copy in image) | no | no | n/a |
| `config_mounts` (mount auth) | `~/.claude`, `~/.claude.json` | `~/.codex` | `~/.gemini` | none |
| `token_auth` | `CLAUDE_CODE_OAUTH_TOKEN` from `~/.config/harness/sandbox-claude-token` | none (mount only) | none (mount only) | n/a |
| `ephemeral_home` (token auth) | `/home/node` | n/a | n/a | n/a |
| `seed_config` | yes (onboarding / theme / trust-this-folder) | no | no | no |
| `telemetry_env` | `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1`, `DISABLE_AUTOUPDATER=1` | none | none | none |
| `egress_domains` | anthropic set (current list) | measured (below) | measured (below) | none |

### agy's config directory, and why the whole of it is mounted

`~/.gemini` is **shared with gemini-cli**, not owned by agy: it holds
`google_accounts.json`, `gemini-credentials.json`, `projects.json`, `history/`
and `skills/` alongside agy's own `antigravity-cli/`. Measured 2026-08-18,
three runs:

- whole `~/.gemini` mounted → real model round-trip succeeds;
- nothing mounted → `Authentication required`, prints an accounts.google.com
  OAuth URL and times out (so the directory is genuinely load-bearing, not a
  fallback for something in the env);
- only `~/.gemini/antigravity-cli` mounted → also succeeds, i.e. the OAuth
  token of record is `~/.gemini/antigravity-cli/antigravity-oauth-token`.

**Decision: mount the whole `~/.gemini`.** The narrow mount's appeal was not
exposing a second product's credentials, and that product is already dead here
— the individual Code Assist tier gemini-cli used is discontinued and the
installed CLI fails auth with `IneligibleTierError`, which is the same fact
`scripts/agent_presets.py` records as the reason there is no `gemini` preset.
Two costs bought nothing in return: the narrow mount needs `~/.gemini` to
pre-exist as a writable directory in the image (podman otherwise creates the
intermediate path as container-root and agy dies with `create projects dir
/home/node/.gemini/config: permission denied`), and its behaviour under an
INTERACTIVE session is unverified — `settings.json`, `trustedFolders.json` and
`installation_id` live at the `~/.gemini` top level and would be absent, the
same class of problem as claude's trust-this-folder dialog.

Residual, stated once: the wide mount does put gemini-cli's `google_accounts`
/ `gemini-credentials` OAuth material in the container, tier-ineligible but not
formally revoked. If that ever matters, the narrow mount above is measured to
work for one-shots and needs only the writable-parent fix plus an interactive
check.

`telemetry_env` is empty for codex and agy on purpose: claude's entry exists
because its telemetry endpoints were measured to stall a fail-closed allowlist,
and no equivalent has been measured for the other two. If a refusal log (below)
shows one, the fix is an entry in this column — not an entry in the allowlist,
which would grant the egress rather than stop needing it.

`host_bin` is resolved with `readlink -f`; codex's resolves *into* `~/.codex`,
so its config mount already covers its binary — the bridge still mounts the
resolved path explicitly so the container path does not depend on the host's
package layout.

Agents with no token-auth mode are **mount-auth only**, and the README must say
so plainly: running codex or agy in the sandbox puts that provider's OAuth
credentials inside the container, with no revocable-token alternative of the
kind claude's `setup-token` provides. Whether such an alternative exists for
either CLI is unknown and out of scope here; it is not asserted either way.

### Measuring `egress_domains` instead of guessing

Do not transcribe domain lists from provider docs. `cmd/sandbox-connect-proxy`
refuses a non-allowlisted CONNECT and logs the host it refused, so the
allowlist for a new agent is *read off* a run: start the agent under
`--firewall-proxy` with only the shared dev hosts allowed, drive one trivial
prompt, and collect the refusals. The same procedure re-validates a list when a
provider rotates endpoints. Record the resulting list in the table above and in
`scripts/sandbox/README.md` with the date it was measured.

## Wrapper interface (final)

```
scripts/sandbox/agent-in-podman.sh [--sandbox-agent NAME] [existing flags] -- <agent args…>
```

- `--sandbox-agent NAME` — `claude` (default, for compatibility) | `codex` |
  `agy` | `bash`. Consumed from argv like the existing control flags; an
  unknown NAME is a hard error, never a silent fallback to claude.
- `--omit-harness-cli`, `--firewall`, `--firewall-proxy`, `--mount-auth`,
  `--image-claude` keep their current meaning. `--image-claude` becomes
  `--image-agent` (claude keeps the only image fallback; the old spelling is
  accepted as a deprecated alias for one release of this repo's own use, since
  it can appear in a live runner's recorded argv).
- Everything else passes through to the agent unchanged.

`claude-in-podman.sh` is **renamed**, not duplicated. A live runner slot records
its `--agent-bin` argv, and `scripts/build_and_restart_all.py` replays the
RUNNING argv, so a rename alone does not migrate existing slots: landing this
requires `systemctl --user restart` of the sandbox slots (or re-`up` with the
new preset). That step belongs in the implementation plan, not in a footnote.

## Presets

`scripts/agent_presets.py` gains `sandbox-claude`, `sandbox-codex`,
`sandbox-agy`, `sandbox-bash`, each derived from its base agent's entry with
only `bin` replaced (the existing `sandbox` derivation, generalised into a loop
so a future base-preset edit cannot reach the direct entry and miss the
sandboxed one). `sandbox` stays as an alias of `sandbox-claude`.

The per-agent `--sandbox-agent` flag is carried in the preset's argv templates,
not in `bin` — `bin` is a plain path and the runner's `ResolveBinPaths` expects
one. Since the wrapper filters its own flags out of `"$@"`, prefixing each
template is sufficient and leaves `{args}` (the caller's `--agent-args`)
last-wins as it is today.

## Launcher and firewall

- `claude-launch.sh` → `agent-launch.sh`. The config seeding stays behind the
  existing `SANDBOX_SEED_CONFIG` gate, which the table sets only for claude, so
  no other agent's home is written to.
- `init-firewall.sh`'s fixed domain list and `sandbox-connect-proxy`'s
  `defaultAllow` split into a shared base (github / npm / pypi — the dev hosts
  every agent needs) plus the table's `egress_domains`, injected as an env var.
  The harness-server carve-out, the gateway /32 rule, the IPv6 block and the
  fail-closed behaviour are untouched.

## probe.sh

Gains a `config_agent` line beside `config_firewall` / `config_auth`, for the
same reason those exist: a report that does not name the configuration it
measured reads later as a claim about every configuration. Its auth-exposure
checks become table-driven (which config paths are mounted for THIS agent)
rather than hard-coded to `~/.claude`.

## Non-goals

- Installing codex or agy into the image. The host bridge is measured to work,
  and baking in three CLIs would triple the image's staleness surface.
- A token-auth mode for codex or agy (see above — existence unknown).
- Changing what any agent is *allowed to do* inside the container. That is the
  capability layer, which is now default-deny
  (`2026-06-20-harness-cli-capabilities-design.md`, Amendment 2026-08-18) and
  composes with this without either layer configuring the other.

## Testing

1. Per agent, through a REAL runner slot: one oneshot task that writes a file
   in the worktree, asserting the file lands owned by the host uid.
2. Per agent, one interactive session (`session new -d`) driven with
   `session snapshot` / `session send`, asserting the TUI renders and a typed
   instruction round-trips — rendering alone is not proof that input works.
3. `probe.sh` under each of `--firewall` and `--firewall-proxy`, for at least
   claude and one other agent, asserting `server_other_port` does not read
   `open` and that the measured `config_agent` matches the slot.
4. `scripts/test_agent_presets.py` extended for the four new preset names,
   including that each derives its base's argv/logFormat rather than restating
   them.
