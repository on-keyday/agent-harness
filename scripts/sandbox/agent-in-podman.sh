#!/usr/bin/env bash
# agent-in-podman.sh — an `--agent-bin` target that runs a real coding agent
# (claude / codex / agy / opencode / pi, or a plain bash shell) inside a ROOTLESS
# podman container, confining its command execution while keeping worktree edits
# owned by the host user.
#
# Which agent is selected by --sandbox-agent (default claude, the only agent
# this wrapper ran when it was called claude-in-podman.sh). EVERY per-agent
# difference lives in the agent table below and nowhere else: if you find
# yourself writing `case "$AGENT"` further down, the field belongs in the table.
#
# Why podman + --userns=keep-id: Claude Code refuses --dangerously-skip-permissions
# when running as root, but rootless *docker* can only write host-owned bind
# mounts as container-root. podman keep-id maps the host user (e.g. uid 1000)
# into the container unchanged, so the agent runs NON-root (flag accepted) AND
# the worktree edits stay owned by the host user on disk.
#
# The runner invokes this with cwd = the task worktree and forwards the agent's
# args (including `--dangerously-skip-permissions ... -p <prompt>`) as "$@".
#
# SCOPE: one-shot (`-p`) verified end-to-end; interactive gets a container TTY
# when stdin is one. harness-cli + the HARNESS_* env are bridged in by default so
# the confined agent keeps the control plane (--omit-harness-cli to disable).
# Capability confinement of the bridged control plane is set at spawn time by the
# parent: `session new --caps <names>` / `submit --caps <names>` (server-enforced
# bitmask; the container itself plays no role). --omit-harness-cli is the
# full-removal option (no harness-cli in the container at all). See README.md.
set -euo pipefail

IMAGE="${HARNESS_SANDBOX_IMAGE:-harness-agent-sandbox:latest}"
WT="$PWD"                                  # the runner sets cwd to the worktree
HOME_DIR="${HOME:-/home/$(id -un)}"

# Consume our own control flags from the arg stream (NOT agent flags, so they
# must not reach the agent). Pass them via `--agent-arg` / runner `--agent-args`:
#   --sandbox-agent N   which agent: claude | codex | agy | bash | opencode | pi. Normally
#                       you do NOT pass this: the agent comes from the name this
#                       script was invoked as (<agent>-in-podman.sh symlinks),
#                       which is the only selector every runner launch path
#                       carries. See "agent selection" below. An unknown name is
#                       a hard error, never a silent fallback to claude — a slot
#                       that quietly ran the wrong agent would look like the
#                       right one in every listing.
#   --omit-harness-cli  run with NO harness control plane in the container (full
#                       isolation); default is to bridge harness-cli + HARNESS_* in.
#   --firewall-proxy    the DEFAULT egress mode: deny-all + an in-container
#                       allowlisting CONNECT proxy (cmd/sandbox-connect-proxy); the
#                       agent gets no raw egress and its API/WebFetch funnel
#                       through the proxy. Passing it changes nothing, except
#                       that it wins over --firewall.
#   --firewall          the iptables+ipset egress allowlist (init-firewall.sh)
#                       instead of the proxy.
#   --no-firewall       an open network. Exits 2 together with --firewall or
#                       --firewall-proxy.
#   --proxy-log[=PATH]  write the proxy's ALLOW/DENY/BUSY lines to a HOST file
#                       instead of the container's stderr — PATH, or
#                       ${TMPDIR:-/tmp}/sandbox-proxy-<task>.log, which is also
#                       where proxy mode writes them when neither this nor
#                       --no-proxy-log is given. Each container start truncates it.
#                       Under an interactive session that stderr IS the agent's
#                       PTY, so every line is drawn over the TUI; agy retries
#                       play.googleapis.com/log twice every 5s and its UI becomes
#                       unusable. Ignored (with a warning) outside proxy mode.
#   --no-proxy-log      leave the proxy's lines on the container's stderr. Exits 2
#                       together with --proxy-log.
#   --mount-auth        MOUNT auth (bind-mount the host agent config), which is
#                       the default; passing it changes nothing. Mount auth
#                       persists session state, so --continue / resume work — at
#                       the cost of exposing the refresh token.
#   --token-auth        claude only: authenticate with the revocable token in
#                       A_TOKEN_FILE from an ephemeral home instead (no refresh
#                       token in the container, no resume). Exits 2 for an agent
#                       without token auth, for a missing or empty token file,
#                       and together with --mount-auth.
#   --history-window    claude, pi, codex: the container sees only THIS task's
#                       conversations (the ones whose cwd is this worktree) plus
#                       the agent's credential file, not the rest of its config
#                       dir. Those conversations stay in the host's normal store,
#                       so a later run without the flag, or the host's own
#                       agent, resumes them. Exits 2 for any other agent, outside
#                       a linked worktree, and when the window cannot be built.
#                       See "History window" below.
#   --image-agent      run the image's own copy of the agent instead of bridging
#                       the host binary in (see "Host binary bridge" below). Only
#                       claude has an image copy. --image-claude is accepted as a
#                       deprecated alias: it can still appear in a live runner
#                       slot's recorded --agent-args.
# Agent selection comes from ARGV[0], not from a flag, and that is load-bearing.
# The runner builds a fresh interactive launch as bin + args with NO argv
# template at all (runner/agent_command.go buildInteractiveArgs returns `extra`
# unchanged when resumeConversation is false), so a selector living in the
# oneshot/resume templates is simply absent on that path. It was: `session new
# --agent sandbox-bash` opened Claude Code, with the task row still reading
# agent=sandbox-bash. The BIN is the one thing every path carries, so the
# symlink name decides, and --sandbox-agent stays only as a manual override for
# direct invocation.
case "$(basename "$0")" in
  claude-in-podman.sh)   AGENT=claude ;;
  codex-in-podman.sh)    AGENT=codex ;;
  agy-in-podman.sh)      AGENT=agy ;;
  bash-in-podman.sh)     AGENT=bash ;;
  opencode-in-podman.sh) AGENT=opencode ;;
  pi-in-podman.sh)       AGENT=pi ;;
  *)                     AGENT=claude ;;   # agent-in-podman.sh itself
esac
bridge_cli=1
firewall=0
firewall_proxy=0
no_firewall=0
mount_auth=0
token_auth=0
history_window=0
image_agent=0
default_proxy_log="${TMPDIR:-/tmp}/sandbox-proxy-${HARNESS_TASK_ID:-$$}.log"
proxy_log=""
no_proxy_log=0
declare -a ARGS=()
want_agent=0
for a in "$@"; do
  if [ "$want_agent" = 1 ]; then AGENT="$a"; want_agent=0; continue; fi
  case "$a" in
    --sandbox-agent)    want_agent=1 ;;
    --sandbox-agent=*)  AGENT="${a#*=}" ;;
    --omit-harness-cli) bridge_cli=0 ;;
    --firewall)         firewall=1 ;;
    --firewall-proxy)   firewall_proxy=1 ;;
    --no-firewall)      no_firewall=1 ;;
    --proxy-log)        proxy_log="$default_proxy_log" ;;
    --proxy-log=*)      proxy_log="${a#*=}" ;;
    --no-proxy-log)     no_proxy_log=1 ;;
    --mount-auth)       mount_auth=1 ;;
    --token-auth)       token_auth=1 ;;
    --history-window)   history_window=1 ;;
    --image-agent|--image-claude) image_agent=1 ;;
    *)                  ARGS+=( "$a" ) ;;
  esac
done
[ "$want_agent" = 1 ] && { echo "[agent-in-podman] --sandbox-agent needs a value" >&2; exit 2; }
if [ "${#ARGS[@]}" -gt 0 ]; then set -- "${ARGS[@]}"; else set --; fi

# A contradictory pair is refused rather than settled by order: these flags
# arrive from a slot's recorded --agent-args AND a task's --agent-arg, and with
# last-wins the log line would be the only place saying which one lost.
conflict() { echo "[agent-in-podman] $1 and $2 contradict each other" >&2; exit 2; }
[ "$no_firewall" = 1 ] && [ "$firewall" = 1 ] && conflict --no-firewall --firewall
[ "$no_firewall" = 1 ] && [ "$firewall_proxy" = 1 ] && conflict --no-firewall --firewall-proxy
[ "$no_proxy_log" = 1 ] && [ -n "$proxy_log" ] && conflict --no-proxy-log --proxy-log
[ "$token_auth" = 1 ] && [ "$mount_auth" = 1 ] && conflict --token-auth --mount-auth
fw_mode=proxy
if [ "$firewall_proxy" = 1 ]; then fw_mode=proxy
elif [ "$firewall" = 1 ]; then fw_mode=ip
elif [ "$no_firewall" = 1 ]; then fw_mode=none
fi
if [ -n "$proxy_log" ] && [ "$fw_mode" != proxy ]; then
  echo "[agent-in-podman] --proxy-log ignored: there is no proxy in firewall=$fw_mode" >&2
  proxy_log=""
fi
if [ "$fw_mode" = proxy ] && [ "$no_proxy_log" != 1 ] && [ -z "$proxy_log" ]; then
  proxy_log="$default_proxy_log"
fi

# --- agent table -------------------------------------------------------------
# The ONE place per-agent differences live.
#
#   A_HOST_BIN       host path bridged in (readlink -f'd below); "" = image only
#   A_CONTAINER_BIN  where the agent is exposed inside the container
#   A_BIN_BUNDLE     1 = bridge the resolved binary's directory, including helpers
#   A_IMAGE_FALLBACK 1 = the image ships a usable copy if the host bin is absent
#   A_CONFIG_MOUNTS  host paths bind-mounted at IDENTICAL paths in mount auth
#   A_RO_MOUNTS      host paths bind-mounted read-only at IDENTICAL paths on
#                    every run: an agent that is a package directory, not one file
#   A_TOKEN_FILE     revocable-token file; "" = this agent has no token auth
#   A_TOKEN_ENV      env var the token is handed to podman as
#   A_ALWAYS_ENV     NAME=VALUE passed on every run
#   A_FW_ENV         NAME=VALUE passed only when a firewall mode is on
#   A_DOMAINS        comma-separated egress domains this agent needs
#   A_HOME           HOME inside the container when NOT using token auth
#   A_EPHEMERAL_HOME HOME under token auth (the image's own writable home)
# --history-window fields; every path is relative to the agent's home:
#   A_HISTORY        dir = bind the host's per-cwd directory; sync = codex's
#                    hardlink-synced sessions dir (history_window.py); "" = no
#                    window, the flag exits 2
#   A_HISTORY_STORE  where the agent keeps conversations
#   history_name     function: the per-cwd directory name under the store
#   A_HISTORY_NAME_MAX  longest name the wrapper can compute (claude hashes
#                    longer ones in a way not documented); 0 = no limit
#   A_HISTORY_ROOTS  config dirs replaced by empty per-run ones
#   A_HISTORY_ROOT_FILES  config files replaced by per-run ones holding {}
#   A_CRED_FILES     credential files mounted singly rw (not under token auth)
#   A_RO_CONFIG      config files mounted singly ro, when the host has them
#   A_RO_DIRS        dirs mounted ro when the host has them: helper binaries
#                    the agent installed for itself, neither history nor config
#   A_SHARED_DIRS    host dirs mounted rw because the host and the container
#                    must see the same one (codex's writer locks)
#   A_HISTORY_SEED   1 = the ephemeral config needs the launcher's first-run
#                    seeding, as under token auth
declare -a A_CONFIG_MOUNTS=() A_ALWAYS_ENV=() A_FW_ENV=() A_RO_MOUNTS=()
declare -a A_HISTORY_ROOTS=() A_HISTORY_ROOT_FILES=() A_CRED_FILES=() A_RO_CONFIG=() A_RO_DIRS=() A_SHARED_DIRS=()
A_EPHEMERAL_HOME=""
A_BIN_BUNDLE=0
A_HISTORY=""
A_HISTORY_STORE=""
A_HISTORY_NAME_MAX=0
A_HISTORY_SEED=0
case "$AGENT" in
  claude)
    A_HOST_BIN="$HOME_DIR/.local/bin/claude"
    A_CONTAINER_BIN=/usr/local/bin/claude
    A_IMAGE_FALLBACK=1
    A_CONFIG_MOUNTS=( "$HOME_DIR/.claude" "$HOME_DIR/.claude.json" )
    A_TOKEN_FILE="${HARNESS_SANDBOX_CLAUDE_TOKEN_FILE:-$HOME_DIR/.config/harness/sandbox-claude-token}"
    A_TOKEN_ENV=CLAUDE_CODE_OAUTH_TOKEN
    A_ALWAYS_ENV=( DISABLE_AUTOUPDATER=1 )
    # Only under a firewall: disabling non-essential egress (telemetry ->
    # datadog, statsig feature-flags, error reporting) is what keeps a
    # fail-closed allowlist from having to include those CDNs. Verified A/B.
    A_FW_ENV=( CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 )
    A_DOMAINS="api.anthropic.com,platform.claude.com,console.anthropic.com,statsig.anthropic.com,statsig.com,sentry.io"
    A_HOME="$HOME_DIR"
    A_EPHEMERAL_HOME=/home/node
    # projects/<enc>: every byte that is not an ASCII letter or digit becomes
    # '-' (runner/streamagent/claude_transcript.go reads the same name).
    A_HISTORY=dir
    A_HISTORY_STORE=.claude/projects
    history_name() { printf %s "$1" | LC_ALL=C sed 's/[^A-Za-z0-9]/-/g'; }
    A_HISTORY_NAME_MAX=200
    A_HISTORY_ROOTS=( .claude )
    A_HISTORY_ROOT_FILES=( .claude.json )
    A_CRED_FILES=( .claude/.credentials.json )
    A_HISTORY_SEED=1
    ;;
  codex)
    # Codex resolves codex-code-mode-host relative to its executable. Mounting
    # only codex at /usr/local/bin left tools unable to start even with Codex's
    # own sandbox disabled. Keep the release's bin directory together, read-only;
    # future sibling executables then follow the same release automatically.
    A_HOST_BIN="$HOME_DIR/.local/bin/codex"
    A_CONTAINER_BIN=/opt/harness-codex-bin/codex
    A_BIN_BUNDLE=1
    A_IMAGE_FALLBACK=0
    A_CONFIG_MOUNTS=( "$HOME_DIR/.codex" )
    A_TOKEN_FILE=""
    A_TOKEN_ENV=""
    # MEASURED 2026-08-18 by running codex under --firewall-proxy with this
    # list empty and reading the proxy's 403s: every call it made
    # (backend-api/codex/responses over wss, .../models, .../ps/mcp) is under
    # chatgpt.com, which suffix-matching covers. NOT included because this run
    # never exercised them: auth.openai.com / api.openai.com (token refresh and
    # API-key auth). If one is needed the proxy will refuse it BY NAME, which is
    # how this list was built in the first place — extend it then, don't guess.
    A_DOMAINS="chatgpt.com"
    A_HOME="$HOME_DIR"
    # Rollouts are partitioned by DATE with the cwd only in their first line,
    # so there is no per-cwd directory to bind: see "History window" below.
    # The writer locks are the host's because a host `codex resume` and this
    # container may append to the same (hardlinked) rollout; only one lock dir
    # serialises them (rollout/src/writer_lock.rs, flock).
    A_HISTORY=sync
    A_HISTORY_STORE=.codex/sessions
    A_HISTORY_ROOTS=( .codex )
    A_CRED_FILES=( .codex/auth.json )
    A_RO_CONFIG=( .codex/config.toml )
    A_SHARED_DIRS=( .codex/thread-writer-locks )
    ;;
  agy)
    A_HOST_BIN="$HOME_DIR/.local/bin/agy"
    A_CONTAINER_BIN=/usr/local/bin/agy
    A_IMAGE_FALLBACK=0
    # The whole ~/.gemini, not just its antigravity-cli/ subtree: the narrow
    # mount authenticates fine for one-shots but needs ~/.gemini to pre-exist
    # writable in the image and is unverified for interactive sessions
    # (settings.json / trustedFolders.json live at the top level). See the spec.
    A_CONFIG_MOUNTS=( "$HOME_DIR/.gemini" )
    A_TOKEN_FILE=""
    A_TOKEN_ENV=""
    # MEASURED 2026-08-18, same procedure as codex above, and it took three
    # rounds because agy's startup "eligibility check" fans out: the model host
    # (daily-cloudcode-pa), then oauth2/v2/userinfo on www.googleapis.com, then
    # — fatally — the user's PROFILE PICTURE on lh3.googleusercontent.com
    # ("Eligibility check failed: failed to get profile picture"). All three are
    # hard requirements; with them the run succeeds.
    # Left DENIED on purpose, because the run completes without them:
    # antigravity-unleash.goog (feature flags) and play.googleapis.com. That is
    # the same non-essential traffic claude suppresses with
    # CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC; agy has no such knob, so it is
    # dropped at the allowlist instead and shows up as DENY lines in the log.
    # Deliberately NOT widened to googleapis.com (that would allowlist every
    # Google API).
    # oauth2.googleapis.com added 2026-10-15: once the access token expired
    # mid-session, every refresh (`POST /token`) was refused and every model
    # call failed with it ("token refresh failed: … Forbidden" in agy's cli.log);
    # a fresh token never hits it, which is why the first measurement missed it.
    # Still absent, never seen: accounts.google.com. Still denied, seen once and
    # harmless: www.gstatic.com.
    A_DOMAINS="daily-cloudcode-pa.googleapis.com,www.googleapis.com,lh3.googleusercontent.com,oauth2.googleapis.com"
    A_HOME="$HOME_DIR"
    ;;
  opencode)
    # The only agent here installed system-wide rather than under the user's
    # home (Arch package -> /usr/bin/opencode). Nothing in the bridge cares —
    # A_HOST_BIN is readlink -f'd and bind-mounted from wherever it is — but it
    # is the reason this entry does not follow the "$HOME_DIR/.local/bin/…"
    # shape the other three share. Measured 2026-08-19: the packaged binary
    # (Bun-compiled ELF, libc/libm/libpthread/libdl only) runs on this image's
    # glibc and reports 1.18.18 inside the container.
    A_HOST_BIN="${HARNESS_SANDBOX_OPENCODE_BIN:-/usr/bin/opencode}"
    A_CONTAINER_BIN=/usr/local/bin/opencode
    A_IMAGE_FALLBACK=0
    # THREE paths, each load-bearing for a different reason:
    #   ~/.config/opencode      config + installed plugins
    #   ~/.local/share/opencode auth.json AND opencode.db. The db is where
    #                           sessions live, so omitting it would
    #                           authenticate fine and then make
    #                           `run --continue` find nothing to continue — a
    #                           resume that silently starts a new conversation.
    #   ~/.cache/opencode       the models.dev catalog + plugin node_modules.
    #                           Not just a speed-up: opencode refreshes that
    #                           catalog over the network at startup, so an
    #                           unmounted cache would add a models.dev egress
    #                           requirement to every run.
    #   ~/.local/state/opencode model.json (last model) and locks/. Sharing the
    #                           lock dir is deliberate: concurrent tasks on one
    #                           project must contend for the same lock, which is
    #                           what keeps them off the same db concurrently.
    # An omitted path is not a degraded mode but a failed one. HOME is the host
    # path and podman creates its mount parents root-owned, so anything opencode
    # tries to mkdir under HOME that we did not name aborts the run before the
    # first token: measured 2026-08-19, first `EACCES: mkdir '$HOME/.cache'`,
    # then `EACCES: mkdir '$HOME/.local/state'`. The full set was then taken in
    # one shot by running opencode against a virgin HOME on the host and listing
    # what it created — do that, rather than discovering them one abort at a
    # time. The fifth path it creates there, ~/.npm/_cacache, is npm's own cache
    # for the plugin install, which does not run here because
    # ~/.config/opencode/node_modules arrives already populated.
    A_CONFIG_MOUNTS=(
      "$HOME_DIR/.config/opencode"
      "$HOME_DIR/.local/share/opencode"
      "$HOME_DIR/.cache/opencode"
      "$HOME_DIR/.local/state/opencode"
    )
    A_TOKEN_FILE=""
    A_TOKEN_ENV=""
    # MEASURED 2026-08-19, same procedure as codex and agy above: run under
    # --firewall-proxy with this list empty and read the proxy's DENY lines.
    # Two hosts: models.opencode.ai (the model catalog, fetched at startup) and
    # opencode.ai itself (the OpenCode Zen gateway that serves the completion).
    #
    # BOTH are listed even though the proxy's suffix matching would cover the
    # subdomain from the parent alone, because the two firewall modes consume
    # this list differently and only one of them suffix-matches. --firewall
    # (ip) runs `dig A` per entry and ipsets the addresses (init-firewall.sh),
    # so a name absent here is simply unresolved: opencode.ai answers
    # 172.65.90.20-23 and models.opencode.ai answers 104.20.32.17 /
    # 172.66.173.149 — disjoint, so the parent entry allowlists none of the
    # catalog's addresses. A list written against proxy-mode behaviour alone
    # fails closed in ip mode, on a host nothing here named.
    #
    # NOT included because this run never exercised them: any provider host a
    # BYOK credential would dial (api.anthropic.com, api.openai.com, …) and any
    # token-refresh endpoint. This host's opencode has no credentials
    # configured, so it authenticated anonymously against Zen and nothing else
    # was reached. Add a host when the proxy refuses it BY NAME — that is how
    # this entry was built — rather than pre-allowing providers on a guess.
    A_DOMAINS="opencode.ai,models.opencode.ai"
    A_HOME="$HOME_DIR"
    ;;
  pi)
    # pi is a node PACKAGE, not a single binary: dist/bundle/cli.js loads its
    # siblings and node_modules/ by relative path, so the one-file bridge the
    # others use cannot carry it. The whole package directory is mounted ro at
    # its host path and cli.js is exec'd there; its `#!/usr/bin/env node` finds
    # the image's node (v22.22.3, inside pi's engines >= 22.19.0).
    #
    # Which package: HARNESS_SANDBOX_PI_PKG if set; else the one `pi` on PATH
    # links to (npm's global install makes bin/pi a symlink to cli.js); else,
    # when `pi` is a wrapper script pinning an interpreter (as under nvm), the
    # highest-versioned copy among the nvm installs.
    pi_pkg="${HARNESS_SANDBOX_PI_PKG:-}"
    if [ -z "$pi_pkg" ] && pi_cli=$(readlink -f "$(command -v pi 2>/dev/null)" 2>/dev/null); then
      case "$pi_cli" in */dist/bundle/cli.js) pi_pkg="${pi_cli%/dist/bundle/cli.js}" ;; esac
    fi
    if [ -z "$pi_pkg" ]; then
      pi_pkg=$(for d in "$HOME_DIR"/.nvm/versions/node/*/lib/node_modules/@earendil-works/pi-coding-agent; do
                 v=$(sed -n 's/^[[:space:]]*"version": "\([^"]*\)".*/\1/p' "$d/package.json" 2>/dev/null | head -n1)
                 [ -n "$v" ] && printf '%s %s\n' "$v" "$d"
               done | sort -V | tail -n1 | cut -d' ' -f2-)
    fi
    if [ -z "$pi_pkg" ] || [ ! -f "$pi_pkg/dist/bundle/cli.js" ]; then
      echo "[agent-in-podman] pi: no pi-coding-agent package found (set HARNESS_SANDBOX_PI_PKG)" >&2
      exit 2
    fi
    A_HOST_BIN=""
    A_CONTAINER_BIN="$pi_pkg/dist/bundle/cli.js"
    A_IMAGE_FALLBACK=0
    A_RO_MOUNTS=( "$pi_pkg" )
    # ~/.pi holds auth.json, settings.json, trust.json and sessions/<cwd>/,
    # which --continue reads.
    A_CONFIG_MOUNTS=( "$HOME_DIR/.pi" )
    A_TOKEN_FILE=""
    A_TOKEN_ENV=""
    # The pi.dev version check, on every run: the ro package cannot be updated
    # from in here, so the "Update Available" banner it produces is noise. Under
    # a firewall also PI_OFFLINE, pi's own switch for its remaining automatic
    # traffic (the model catalog refresh), which plays the role
    # CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC does for claude.
    A_ALWAYS_ENV=( PI_SKIP_VERSION_CHECK=1 )
    A_FW_ENV=( PI_OFFLINE=1 )
    # MEASURED 2026-10-17 (pi 1.0.3), same procedure as codex above: with this
    # list empty, the proxy's only DENY lines were openrouter.ai:443 — the
    # provider this host's ~/.pi/agent/settings.json names — and, from an
    # interactive session, pi.dev:443 (the version check the env above now
    # turns off). openrouter.ai is THIS host's provider, not pi's: pi dials
    # whichever provider ~/.pi/agent is configured for, so the list is a
    # per-host setting. HARNESS_SANDBOX_PI_DOMAINS (comma-separated, in the
    # runner's env) REPLACES it, for both firewall modes; SANDBOX_PROXY_ALLOW
    # would cover --firewall-proxy only. Find the host to name by running once
    # under --firewall-proxy --proxy-log and reading the DENY lines.
    A_DOMAINS="${HARNESS_SANDBOX_PI_DOMAINS:-openrouter.ai}"
    A_HOME="$HOME_DIR"
    # sessions/--<cwd without its leading />-- with / \ : turned into '-'
    # (pi 1.0.3 dist/core/session-manager.js:290-294). settings.json names the
    # provider pi dials, so it is needed, read-only. agent/bin holds the fd
    # binary pi fetched for itself; without it pi warns "fd not found" and,
    # offline, cannot fetch another (measured 2026-10-26, pi 1.0.3).
    A_HISTORY=dir
    A_HISTORY_STORE=.pi/agent/sessions
    history_name() { local p="${1#/}"; p="${p//\//-}"; p="${p//\\/-}"; p="${p//:/-}"; printf -- '--%s--' "$p"; }
    A_HISTORY_ROOTS=( .pi )
    A_CRED_FILES=( .pi/agent/auth.json )
    A_RO_CONFIG=( .pi/agent/settings.json )
    A_RO_DIRS=( .pi/agent/bin )
    ;;
  bash)
    # A shell sandbox, not a conversational agent: nothing to bridge, nothing to
    # authenticate. HOME is the image's own so a stray write lands somewhere
    # that exists rather than at a host path nobody mounted.
    A_HOST_BIN=""
    A_CONTAINER_BIN=/bin/bash
    A_IMAGE_FALLBACK=1
    A_TOKEN_FILE=""
    A_TOKEN_ENV=""
    A_DOMAINS=""
    A_HOME=/home/node
    ;;
  *)
    echo "[agent-in-podman] unknown --sandbox-agent '$AGENT' (want: claude|codex|agy|bash|opencode|pi)" >&2
    exit 2
    ;;
esac

# Bind-mount at IDENTICAL host paths so (a) claude's cwd-hash session resume and
# (b) git's worktree gitdir link both resolve inside the container.
#
# What is writable is narrower than "the repo", because every git directory the
# container can write is one the HOST later runs git in: the operator's push and
# `make build` in the main checkout, the runner's and the operator's git in this
# worktree. git executes what a git directory names (hooks/, core.hooksPath and
# core.fsmonitor in config) and follows what it points at (a worktree's `.git`
# file, a gitdir's `commondir` — which may name a directory holding its own
# config). A container that can write any of those runs code on the host, with
# the host's credentials, the next time anything there runs `git status`.
#
# So for a linked worktree (the runner's default):
#   - the common dir (<root>/.git) is mounted READ-ONLY, and the repo root is not
#     mounted at all: the main checkout's files and the other tasks' worktrees
#     are not in the container;
#   - objects/ refs/ logs/ and this worktree's own gitdir are re-mounted rw —
#     everything commit / stash / rebase / reset / fetch write;
#   - that gitdir's commondir + gitdir files and the worktree's `.git` file are
#     re-mounted ro (a file bind mount cannot be renamed over either: EBUSY).
# Measured 2026-10-16 (podman 6.0.1, git 2.55.0) on a scratch repo with two
# worktrees: those operations work in the container, and writes to hooks/,
# config, <common>/commondir, <gitdir>/commondir and <worktree>/.git are refused.
#
# The cost: deleting a ref fails in here (`git branch -d`, `git tag -d`), because
# the files backend takes <common>/packed-refs.lock for every deletion, and the
# common dir is the one thing that cannot be writable — that would let the
# container CREATE <common>/commondir and redirect the main checkout's git to a
# directory of its choosing. `git config` writes fail too, by design.
#
# Not a linked worktree (NoWorktree mode: the cwd IS the checkout, its .git a
# directory inside it): the agent edits the very files the host builds, so there
# is no boundary to keep. hooks/ and config go ro so git itself is not the
# vector; the rest stays writable.
declare -a MOUNTS=() SEED_PATHS=()
linked_wt=0
mount_rw() { MOUNTS+=( -v "$1:$1" ); }
mount_ro() { MOUNTS+=( -v "$1:$1:ro" ); }
if gitdir=$(git -C "$WT" rev-parse --absolute-git-dir 2>/dev/null) &&
   common=$(git -C "$WT" rev-parse --path-format=absolute --git-common-dir 2>/dev/null); then
  gitdir=$(readlink -f "$gitdir"); common=$(readlink -f "$common")
  root=$(dirname "$common")
  SEED_PATHS+=( "$root" )
  if [ "$gitdir" != "$common" ]; then
    linked_wt=1
    # git creates logs/ on the first reflog write, which it cannot do under a ro
    # common dir.
    mkdir -p "$common/logs"
    mount_ro "$common"
    mount_rw "$common/objects"; mount_rw "$common/refs"; mount_rw "$common/logs"
    mount_rw "$gitdir"
    for f in commondir gitdir; do [ -f "$gitdir/$f" ] && mount_ro "$gitdir/$f"; done
    mount_rw "$WT"
    [ -f "$WT/.git" ] && mount_ro "$WT/.git"
  else
    mount_rw "$root"
    case "$WT/" in "$root"/*) ;; *) mount_rw "$WT" ;; esac
    mount_ro "$common/config"
    [ -d "$common/hooks" ] && mount_ro "$common/hooks"
  fi
else
  SEED_PATHS+=( "$WT" )
  mount_rw "$WT"
fi

# Auth. Two modes, and which are AVAILABLE is a property of the agent (table):
#
#  (a) Mount (default): reuse the host login by bind-mounting the agent's
#      config paths. Session state persists, so --continue / resume work. This
#      exposes that provider's credentials to the container — see the README
#      security section.
#
#  (b) Token (--token-auth): auth via the agent's token env from its token
#      file — claude's is ~/.config/harness/sandbox-claude-token, override with
#      HARNESS_SANDBOX_CLAUDE_TOKEN_FILE, holding a DEDICATED, revocable
#      `claude setup-token` — and DO NOT mount the personal config dir (which
#      holds the *permanent* refresh token). The agent runs from the image's
#      own writable home, so its config is ephemeral per run (no host-session
#      resume). We never read the token's bytes; podman receives it as an env.
#      It used to be chosen automatically whenever the token file existed, and
#      --mount-auth was the way out; opt-in since 2026-10-25 (operator), since
#      a mode without resume was rarely the one wanted.
#
#      ONLY claude has this. codex, agy, opencode and pi have no known
#      revocable-token mode, so their table entries leave A_TOKEN_FILE empty and
#      --token-auth is an error for them — which the README states plainly
#      rather than implying.
TOKEN_FILE="$A_TOKEN_FILE"
AGENT_HOME="$A_HOME"
declare -a AUTH=()
auth_mode="mount"
if [ "$token_auth" = 1 ]; then
  if [ -z "$TOKEN_FILE" ]; then
    echo "[agent-in-podman] --token-auth: $AGENT has no token auth" >&2
    exit 2
  fi
  if [ ! -s "$TOKEN_FILE" ]; then
    echo "[agent-in-podman] --token-auth: no token in $TOKEN_FILE" >&2
    exit 2
  fi
fi
if [ "$token_auth" = 1 ]; then
  auth_mode="token"
  AGENT_HOME="$A_EPHEMERAL_HOME"
  # SANDBOX_SEED_CONFIG tells the in-container launcher to pre-seed onboarding +
  # trust-this-folder for the worktree (ephemeral home re-prompts otherwise).
  # SANDBOX_SEED_PROJECTS carries the repo root, newline-separated. claude
  # keys "trust this folder" on the REPO ROOT, not the cwd: seeding only $PWD
  # (the task worktree) left every session opening on the safety-check dialog,
  # and answering it wrote an entry for the repo root. The root is no longer
  # mounted (see the workspace mounts above), but the key is a path string.
  # The launcher seeds each of these plus its own $PWD.
  AUTH=(
    --env "$A_TOKEN_ENV=$(cat "$TOKEN_FILE")"
    --env SANDBOX_SEED_CONFIG=1
    --env SANDBOX_SEED_PROJECTS="$(printf '%s\n' "${SEED_PATHS[@]}")"
  )
elif [ "$history_window" = 1 ]; then
  : # the history window below mounts the agent's state piece by piece
else
  # Mount every config path the table names, skipping ones the host lacks (a
  # missing ~/.claude.json is normal on a fresh login; podman would otherwise
  # create it as a DIRECTORY and claude would fail to parse it).
  for cfg in "${A_CONFIG_MOUNTS[@]}"; do
    [ -e "$cfg" ] && AUTH+=( -v "$cfg:$cfg" )
  done
  # An agent with nothing to mount has no auth at all (bash). Reporting "mount"
  # there would put a credential-exposure mode in the log line for a container
  # that exposes no credentials.
  [ "${#A_CONFIG_MOUNTS[@]}" -eq 0 ] && auth_mode="none"
fi

# History window (--history-window). Instead of the whole config dir, the
# container gets:
#  - an EMPTY config root per run, taken from a host temp dir. Not left to
#    podman: podman creates a missing mount parent root-owned, and the agent's
#    first mkdir beside it then aborts (measured for opencode 2026-08-19:
#    `EACCES: mkdir '$HOME/.cache'`). A host dir with every mount target
#    pre-created is owned by the user. The reaper removes it.
#  - the window: the host's directory for THIS cwd under the agent's store,
#    bind-mounted, so what the agent writes is in the host's normal store at
#    once and a later run without the flag, or the host's own agent, resumes it.
#  - the credential file(s), singly. claude replaces .credentials.json on every
#    write (birth time == modify time, 2026-10-26), which a single-file mount
#    cannot take; but its atomic writer falls back to truncating and writing in
#    place on EXDEV/EPERM/EEXIST/EBUSY (read from claude 2.1.310), and a
#    single-file-mounted ~/.claude.json took a container's write that way, same
#    inode, same day. Accepted: a crash between the truncate and the write
#    leaves the host an empty credential file and a re-login.
#  - read-only config the agent cannot start without.
# Refused outside a linked worktree: there the cwd is the shared checkout, so
# the window would hold every task's conversations (and claude's auto memory).
# A refusal never falls back to the shared mode, and creates nothing.
# See docs/superpowers/specs/2026-10-26-sandbox-history-window-design.md.
history_mode=shared
history_root=""
history_helper="" hist_sync="" hist_host="" hist_codex=""
if [ "$history_window" = 1 ]; then
  hw_refuse() { echo "[agent-in-podman] --history-window: $1" >&2; exit 2; }
  [ -n "$A_HISTORY" ] || hw_refuse "$AGENT has no window (no way to show one task's conversations alone)"
  [ "$linked_wt" = 1 ] || hw_refuse "$WT is not a linked worktree; the window would hold every task's conversations"
  if [ "$A_HISTORY" = sync ]; then
    hist_store="$A_HISTORY_STORE"
  else
    hist_store="$A_HISTORY_STORE/$(history_name "$WT")"
    hist_len=$(printf %s "${hist_store##*/}" | wc -c)
    if [ "$A_HISTORY_NAME_MAX" -gt 0 ] && [ "$hist_len" -gt "$A_HISTORY_NAME_MAX" ]; then
      hw_refuse "the history directory name for $WT is $hist_len bytes; over $A_HISTORY_NAME_MAX $AGENT shortens it with a hash this wrapper does not compute"
    fi
  fi
  declare -a hist_creds=()
  [ "$token_auth" = 1 ] || hist_creds=( "${A_CRED_FILES[@]}" )
  for f in "${hist_creds[@]}"; do
    [ -f "$HOME_DIR/$f" ] || hw_refuse "no credential file $HOME_DIR/$f (is $AGENT logged in on this host?)"
  done
  declare -a hist_ro=()
  for f in "${A_RO_CONFIG[@]}"; do [ -f "$HOME_DIR/$f" ] && hist_ro+=( "$f" ); done
  declare -a hist_ro_dirs=()
  for d in "${A_RO_DIRS[@]}"; do [ -d "$HOME_DIR/$d" ] && hist_ro_dirs+=( "$d" ); done
  hist_src="$HOME_DIR/$hist_store"
  if [ "$A_HISTORY" = sync ]; then
    # codex: rollouts live at sessions/YYYY/MM/DD/ with the cwd only in their
    # first line, and a rollout placed at any other depth is resumable by id
    # but never listed (measured, codex 0.164.0), so `resume --last` would not
    # find it. The window is a per-task directory, outside ~/.codex so the host
    # never lists a rollout twice, rebuilt here from hardlinks to the host
    # rollouts of this cwd; the reaper links the container's new ones back.
    # Kept across runs only so a rollout the reaper never got to link (it died
    # with the host) is linked by the next start before the rebuild.
    history_helper="$(dirname "$(readlink -f "$0")")/history_window.py"
    hist_sync="${XDG_STATE_HOME:-$HOME_DIR/.local/state}/harness-sandbox/codex/${HARNESS_TASK_ID:-cwd-$(printf %s "$WT" | sha256sum | cut -c1-32)}"
    hist_host="$HOME_DIR/$hist_store"
    mkdir -p "$hist_sync" "$hist_host"
    python3 "$history_helper" codex-import --task-dir "$hist_sync" --host-sessions "$hist_host" --cwd "$WT" || exit $?
    hist_src="$hist_sync"
    # The host's own codex, for the reaper's re-index (history_window.py
    # codex_reindex): the host's state DB never sees a thread created in here.
    hist_codex=$(readlink -f "$A_HOST_BIN" 2>/dev/null || true)
  fi

  history_mode=window
  history_root=$(mktemp -d "${TMPDIR:-/tmp}/harness-sandbox-root.XXXXXX")
  for d in "${A_HISTORY_ROOTS[@]}" "$hist_store" "${hist_ro_dirs[@]}" "${A_SHARED_DIRS[@]}"; do mkdir -p "$history_root/$d"; done
  for f in "${A_HISTORY_ROOT_FILES[@]}"; do echo '{}' >"$history_root/$f"; done
  for f in "${hist_creds[@]}" "${hist_ro[@]}"; do mkdir -p "$(dirname "$history_root/$f")"; : >"$history_root/$f"; done
  mkdir -p "$HOME_DIR/$hist_store"
  # Parents before what sits inside them: the config root, then the window in it.
  for x in "${A_HISTORY_ROOTS[@]}" "${A_HISTORY_ROOT_FILES[@]}"; do AUTH+=( -v "$history_root/$x:$AGENT_HOME/$x" ); done
  AUTH+=( -v "$hist_src:$AGENT_HOME/$hist_store" )
  for f in "${hist_creds[@]}"; do AUTH+=( -v "$HOME_DIR/$f:$AGENT_HOME/$f" ); done
  for f in "${hist_ro[@]}" "${hist_ro_dirs[@]}"; do AUTH+=( -v "$HOME_DIR/$f:$AGENT_HOME/$f:ro" ); done
  for d in "${A_SHARED_DIRS[@]}"; do mkdir -p "$HOME_DIR/$d"; AUTH+=( -v "$HOME_DIR/$d:$AGENT_HOME/$d" ); done
  if [ "$A_HISTORY_SEED" = 1 ] && [ "$token_auth" != 1 ]; then
    AUTH+=( --env SANDBOX_SEED_CONFIG=1 --env SANDBOX_SEED_PROJECTS="$(printf '%s\n' "${SEED_PATHS[@]}")" )
  fi
fi
# Pure pass-through: claude args (incl. --dangerously-skip-permissions, which the
# runner forwards via --agent-args / submit --agent-arg) arrive in "$@". The
# container is the confinement boundary; the runner owns claude's arg policy.
#
# TTY: the runner runs an interactive session under a real PTY (exec.ExecuteCommand
# ptyEnabled=true), so our stdin is a terminal — allocate a TTY inside the
# container too (-t), else claude's TUI aborts with "stdin is not a TTY". One-shot
# (`-p`) runs under a pipe (stdin not a tty), where -t would corrupt the captured
# byte stream — so gate -t on stdin being a terminal.
declare -a TTY=()
[ -t 0 ] && TTY=( -t )

# harness control plane (default ON; --omit-harness-cli disables). The runner
# already set HARNESS_* in OUR env and put harness-cli on PATH — forward both in
# so the confined agent can still submit / agentboard / file-transfer. Works when
# the server is directly reachable; behind HARNESS_PROXY_VIA_RUNNER a
# --network=host shim would be needed (left for later).
#
# Control-plane authority is server-enforced via capabilities, not by what's
# in the container: use `submit --caps none` (or other restricted sets) when
# spawning this sandboxed task to close specific control-plane operations
# (spawn, file_read/write, forward, exec_view/cowrite/control, ...) server-side.
#
# The bridged binary is FROZEN for the container's lifetime. This is a
# single-FILE bind mount, so the container holds the inode that existed at
# `podman run` time; `make build` writes a replacement and the running
# container keeps the old, now-unlinked one — the rebuild is invisible to it,
# not merely delayed. Measured 2026-08-18: a long-running sandbox task reported
# harness-cli mtime 04:38 and a 692-line embedded skill while the host was at
# 04:50 / 720 lines, and a container started at that moment saw 04:50 / 720.
# That matters because harness-cli carries the agent skills (`harness-cli
# skill`), so a confined agent can be reading guidance several commits old.
# It can at least SAY which: `harness-cli version` prints the commit the binary
# was built from (go build stamps vcs.* into it), and the sandbox task read
# exactly that from inside a container with no go and no strings. What it
# cannot do is judge whether that commit is current — the repo's HEAD is not
# visible from in there — so the revision has to be compared against a peer or
# the operator. Only a NEW container picks up a rebuild.
declare -a CLI=()
if [ "$bridge_cli" = 1 ]; then
  hcli=$(command -v harness-cli 2>/dev/null)
  [ -n "$hcli" ] && CLI+=( -v "$(readlink -f "$hcli"):/usr/local/bin/harness-cli:ro" )
  while IFS='=' read -r name _; do
    case "$name" in HARNESS_*) CLI+=( --env "$name" ) ;; esac
  done < <(env)
fi

# Egress firewall (on unless --no-firewall). Start as container-root with the
# caps the iptables setup needs; the entrypoint applies the allowlist then drops
# to the keep-id host user to run claude. The harness server (parsed from
# HARNESS_SERVER_CID) is allowlisted so the bridged harness-cli still reaches it.
declare -a FW=()
if [ "$fw_mode" != none ]; then
  # Split HARNESS_SERVER_CID into the three fields the harness-server carve-out
  # needs. Format is objproto ConnectionID.String(): "<transport>:<host>:<port>-<id>"
  # (the id may be a literal "*"). Both firewall modes used to pass only the IP
  # and allow the whole host: measured 2026-08-18 from inside a --firewall-proxy
  # container, every port on the server machine was reachable, its sshd included.
  # The server is an ordinary LAN box, not a single-purpose appliance, so the
  # carve-out has to name the one port harness-cli actually dials.
  cid="${HARNESS_SERVER_CID:-}"
  server_hostport="${cid#*:}"; server_hostport="${server_hostport%-*}"
  server_ip="${server_hostport%:*}"
  server_port="${server_hostport##*:}"
  case "${cid%%:*}" in udp) server_proto=udp ;; *) server_proto=tcp ;; esac
  # An unparsable port means NO carve-out (the firewall scripts warn and skip):
  # losing the control plane is the fail-closed outcome, re-opening the host is not.
  [[ "$server_port" =~ ^[0-9]+$ ]] || server_port=""
  FW=(
    --user 0
    --cap-add=NET_ADMIN --cap-add=NET_RAW
    --env DROP_UID="$(id -u)" --env DROP_GID="$(id -g)"
    --env SANDBOX_SERVER_IP="$server_ip"
    --env SANDBOX_SERVER_PORT="$server_port"
    --env SANDBOX_SERVER_PROTO="$server_proto"
    # Disable claude's non-essential egress (telemetry → datadog, statsig
    # feature-flags, auto-update, error reporting). Verified A/B that this drops
    # http-intake.logs.us5.datadoghq.com etc. — so neither the allowlist nor the
    # proxy needs those telemetry CDNs, and fail-closed won't stall on them.
    --entrypoint /usr/local/bin/sandbox-entrypoint.sh
  )
  # Per-agent: the telemetry-silencing env (so a fail-closed allowlist needn't
  # include those CDNs) and the agent's own endpoints, which BOTH firewall
  # modes read instead of carrying a hard-coded provider list.
  for e in "${A_FW_ENV[@]}"; do FW+=( --env "$e" ); done
  FW+=( --env SANDBOX_AGENT_DOMAINS="$A_DOMAINS" )
  if [ "$fw_mode" = proxy ]; then
    FW+=( --env SANDBOX_FIREWALL_PROXY=1 )
    # Extend the proxy's domain allowlist (comma-separated) for WebFetch research
    # targets via SANDBOX_PROXY_ALLOW in the runner's env.
    [ -n "${SANDBOX_PROXY_ALLOW:-}" ] && FW+=( --env SANDBOX_PROXY_ALLOW="$SANDBOX_PROXY_ALLOW" )
    if [ -n "$proxy_log" ]; then
      # Created here, as the host user, so the bind mount has a FILE to land on
      # (podman would otherwise create a directory). Truncated, not appended:
      # the agent can read the bind-mounted file, and a resumed task reuses its
      # path, so appending let every incarnation read the earlier ones' traffic
      # and grew the file without bound.
      if ! echo "$(date "+%F %T") agent=$AGENT task=${HARNESS_TASK_ID:-?}" >"$proxy_log"; then
        echo "[agent-in-podman] proxy log: cannot write $proxy_log (--no-proxy-log keeps it on stderr)" >&2
        exit 2
      fi
      FW+=( -v "$(readlink -f "$proxy_log"):/run/sandbox-proxy.log"
            --env SANDBOX_PROXY_LOG=/run/sandbox-proxy.log )
    fi
  else
    FW+=( --env SANDBOX_FIREWALL=1 )
  fi
fi

# Host binary bridge (default ON; --image-agent opts out where a copy exists).
# The image bakes in an npm-installed claude that goes stale within days (claude
# releases often) and then nags about upgrading, while the mounted-ro binary
# can't self-update. Instead bind-mount the host's auto-updated binary over the
# image path — measured 2026-08-18 that all three host binaries run on the
# image's glibc 2.36: claude (Bun ELF), agy (glibc-dynamic, 197MB) and codex
# (static-pie musl, so libc-independent by construction); opencode (Bun ELF,
# Arch-packaged) added 2026-08-19 and likewise runs there.
# DISABLE_AUTOUPDATER (claude's A_ALWAYS_ENV) silences update attempts either
# way: the ro mount can't be replaced, and the image copy is fixed until the
# next image build.
#
# An agent with NO image fallback and no host binary is a hard error. Falling
# through would exec a container path that does not exist, which surfaces as an
# opaque podman/exec failure several layers from the actual cause.
declare -a HOSTBIN=()
for e in "${A_ALWAYS_ENV[@]}"; do HOSTBIN+=( --env "$e" ); done
agent_src="image"
if [ -n "$A_HOST_BIN" ] && [ "$image_agent" != 1 ]; then
  hbin="$(readlink -f "$A_HOST_BIN" 2>/dev/null || true)"
  if [ -n "$hbin" ] && [ -x "$hbin" ]; then
    if [ "$A_BIN_BUNDLE" = 1 ]; then
      HOSTBIN+=( -v "$(dirname "$hbin"):$(dirname "$A_CONTAINER_BIN"):ro" )
    else
      HOSTBIN+=( -v "$hbin:$A_CONTAINER_BIN:ro" )
    fi
    agent_src="host:$(basename "$hbin")"
  elif [ "$A_IMAGE_FALLBACK" != 1 ]; then
    echo "[agent-in-podman] $AGENT: no host binary at $A_HOST_BIN and no image fallback" >&2
    exit 2
  fi
elif [ -z "$A_HOST_BIN" ] && [ "$A_IMAGE_FALLBACK" != 1 ] && [ "${#A_RO_MOUNTS[@]}" -eq 0 ]; then
  echo "[agent-in-podman] $AGENT: no host binary and no image fallback" >&2
  exit 2
fi
for m in "${A_RO_MOUNTS[@]}"; do
  HOSTBIN+=( -v "$m:$m:ro" )
  agent_src="host:$m"
done

# One-line summary of the chosen modes → the runner log (token VALUE never shown).
echo "[agent-in-podman] agent=$AGENT auth=$auth_mode firewall=$fw_mode harness-cli=$([ "$bridge_cli" = 1 ] && echo on || echo off) bin=$agent_src image=$IMAGE history=$history_mode${proxy_log:+ proxy-log=$proxy_log}" >&2

# Container lifecycle. We MUST `exec` podman so it stays the foreground owner of
# the TTY — otherwise interactive keystrokes never reach claude. But with exec,
# when the runner kills this process the podman client dies while conmon keeps the
# container (and its claude) alive — orphaned, accumulating across --continue
# re-spawns. So fork a detached reaper that force-removes the container (via
# --cidfile) once this process is gone. It polls, so it catches even SIGKILL
# (which a trap can't), and it never touches the terminal (stdin /dev/null), so it
# doesn't interfere with claude's TTY input.
#
# setsid is load-bearing: the runner starts this script as a PTY session leader,
# and a plain `( ... ) &` subshell stays in the leader's session + foreground
# process group — when the leader (the exec'd podman client) dies, the kernel
# HUPs that whole foreground group and the reaper dies BEFORE its podman rm ever
# runs (observed 2026-06-12: 8 orphaned containers, each with its cidfile still
# in /tmp because the trailing rm never executed). Redirecting stdio is not
# detaching; only a new session escapes the PTY hangup.
# The retry budget is ~60s, not a few seconds: right after the podman client
# dies, the container goes through a stop/cleanup window during which
# `podman rm -f` blocks or fails ("Stopping" state / cleanup lock); observed
# live 2026-06-13 — six 0.3s-spaced attempts all lost to that window, while a
# manual rm ~90s later succeeded instantly. Each attempt is capped with
# `timeout 10` so a wedged podman call cannot pin the reaper forever.
# Reaper diagnostics go to a log, not /dev/null — when a container outlives
# its session anyway, the per-attempt podman errors there are the only
# post-mortem evidence of why.
cidfile="$(mktemp -u "${TMPDIR:-/tmp}/sandbox-cid.XXXXXX")"
setsid bash -c '
  wrapper_pid="$1"; cidfile="$2"; history_root="$3"
  helper="$4"; sync_dir="$5"; host_sessions="$6"; wt="$7"; host_codex="$8"
  log="${TMPDIR:-/tmp}/sandbox-reaper.log"
  # codex history window: link the rollouts the container created into the
  # host store, every 5s while it runs (so the host can list and resume the
  # conversation during the run) and once more after the container is gone.
  export_rollouts() {
    [ -n "$sync_dir" ] && python3 "$helper" codex-export --task-dir "$sync_dir" --host-sessions "$host_sessions" --cwd "$wt" >>"$log" 2>&1
  }
  # cd out of the inherited cwd (= the task worktree): the runner deletes the
  # worktree on session kill, and podman aborts on a vanished cwd ("error
  # getting current working directory") before it ever touches the container.
  cd /
  polls=0
  while kill -0 "$wrapper_pid" 2>/dev/null; do
    sleep 0.5
    polls=$((polls + 1))
    [ $((polls % 10)) -eq 0 ] && export_rollouts
  done
  echo "$(date "+%F %T") reaper: wrapper $wrapper_pid gone cid=$(head -c12 "$cidfile" 2>/dev/null || echo no-cidfile)" >>"$log"
  deadline=$((SECONDS + 60))
  removed=0
  [ -e "$cidfile" ] || removed=1   # podman never created a container
  while [ -e "$cidfile" ] && [ "$SECONDS" -lt "$deadline" ]; do
    err=$(timeout 10 podman rm -f -i -t 1 --cidfile "$cidfile" 2>&1) && { echo "$(date "+%F %T") reaper: removed" >>"$log"; removed=1; break; }
    echo "$(date "+%F %T") reaper: rm rc=$? ${err}" >>"$log"
    sleep 1
  done
  rm -f "$cidfile"
  export_rollouts
  # ...and have the codex of the host index them: resume --last asks its state
  # DB first, and that DB never saw a thread the container created.
  [ -n "$sync_dir" ] && [ -n "$host_codex" ] &&
    timeout 60 python3 "$helper" codex-reindex --codex "$host_codex" --cwd "$wt" >>"$log" 2>&1
  # The history window root is the live config dir of a container that is
  # still there, so it goes only once the container is gone.
  if [ -n "$history_root" ]; then
    if [ "$removed" = 1 ]; then
      case "$history_root" in "${TMPDIR:-/tmp}"/harness-sandbox-root.*) rm -rf -- "$history_root" ;; esac
    else
      echo "$(date "+%F %T") reaper: kept $history_root" >>"$log"
    fi
  fi
' _ "$$" "$cidfile" "$history_root" "$history_helper" "$hist_sync" "$hist_host" "$WT" "$hist_codex" </dev/null >/dev/null 2>&1 &

# --init is load-bearing, not hygiene. Every layer of our command chain execs
# (entrypoint.sh -> gosu -> agent-launch.sh -> the agent), so the AGENT itself ends
# up as the container's PID 1 — and it is an application, not an init: it waits
# on the child pids it spawned and on nothing else. A process REPARENTED onto it
# (any background job whose own parent shell exited: `run_in_background` bash
# tools, nohup'd builds, the --firewall-proxy broker) is unknown to it and stays
# <defunct> for the container's whole life. Seen in the wild 2026-08-18 as 3
# stuck `python3 <defunct>` under a live sandbox agent; A/B'd through this
# wrapper with the REAL binary (claude=host:2.1.234) and one orphaned
# `setsid sleep 1`: PID1COMM=claude -> 1 zombie, PID1COMM=podman-init -> 0.
# (claude is a Bun-compiled single-file ELF, not a node process — the
# node_modules path it installs under says nothing about its runtime, and no
# app runtime does the blanket waitpid(-1) that reaping orphans requires.)
# --init makes catatonit PID 1 (claude becomes its child), which reaps orphans
# and forwards signals; it costs one ~800KB binary bind-mounted at
# /run/podman-init and does not disturb the TTY (the child stays in the
# foreground process group).
#
# --tz=local: the image runs in UTC, and an agent that names or files its
# history by LOCAL time writes it hours "earlier" than the host does. codex
# does both (sessions/YYYY/MM/DD/rollout-<local time>-…): a rollout written in
# here sorted behind the host's, the host's state DB never picked it up, and
# `codex exec resume --last` on the host continued an older thread (measured
# 2026-10-26, codex 0.164.0, host JST).
exec podman run --rm -i "${TTY[@]}" \
  --init \
  --tz=local \
  --userns=keep-id \
  --security-opt label=disable \
  --security-opt no-new-privileges \
  --cidfile "$cidfile" \
  -w "$WT" \
  --env HOME="$AGENT_HOME" \
  --env SANDBOX_AGENT="$AGENT" \
  --env SANDBOX_AGENT_BIN="$A_CONTAINER_BIN" \
  --env SANDBOX_HISTORY="$history_mode" \
  "${AUTH[@]}" \
  "${CLI[@]}" \
  "${FW[@]}" \
  "${HOSTBIN[@]}" \
  "${MOUNTS[@]}" \
  "$IMAGE" \
  /usr/local/bin/sandbox-agent-launch.sh "$@"
