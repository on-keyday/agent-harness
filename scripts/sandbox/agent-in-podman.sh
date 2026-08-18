#!/usr/bin/env bash
# agent-in-podman.sh — an `--agent-bin` target that runs a real coding agent
# (claude / codex / agy, or a plain bash shell) inside a ROOTLESS podman
# container, confining its command execution while keeping worktree edits owned
# by the host user.
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
#   --sandbox-agent N   which agent to run: claude (default) | codex | agy | bash.
#                       An unknown name is a hard error, never a silent fallback
#                       to claude — a slot that quietly ran the wrong agent would
#                       look like the right one in every listing.
#   --omit-harness-cli  run with NO harness control plane in the container (full
#                       isolation); default is to bridge harness-cli + HARNESS_* in.
#   --firewall          apply the iptables+ipset egress allowlist
#                       (init-firewall.sh); default is an open network.
#   --firewall-proxy    stronger egress: deny-all + an in-container allowlisting
#                       CONNECT proxy (cmd/sandbox-connect-proxy); the agent gets no raw
#                       egress and its API/WebFetch funnel through the proxy.
#                       Takes precedence over --firewall if both are given.
#   --mount-auth        force MOUNT auth (bind-mount the host agent config) even
#                       when a token file exists. Mount auth persists session
#                       state, so --continue / resume work — at the cost of
#                       exposing the refresh token. Use it for trusted, resumable
#                       tasks; leave it off for untrusted work (token auth,
#                       ephemeral). Only claude HAS token auth; for the others
#                       mount auth is the only mode and this flag is a no-op.
#   --image-agent       run the image's own copy of the agent instead of bridging
#                       the host binary in (see "Host binary bridge" below). Only
#                       claude has an image copy. --image-claude is accepted as a
#                       deprecated alias: it can still appear in a live runner
#                       slot's recorded --agent-args.
AGENT=claude
bridge_cli=1
firewall=0
firewall_proxy=0
force_mount=0
image_agent=0
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
    --mount-auth)       force_mount=1 ;;
    --image-agent|--image-claude) image_agent=1 ;;
    *)                  ARGS+=( "$a" ) ;;
  esac
done
[ "$want_agent" = 1 ] && { echo "[agent-in-podman] --sandbox-agent needs a value" >&2; exit 2; }
if [ "${#ARGS[@]}" -gt 0 ]; then set -- "${ARGS[@]}"; else set --; fi

# --- agent table -------------------------------------------------------------
# The ONE place per-agent differences live.
#
#   A_HOST_BIN       host path bridged in (readlink -f'd below); "" = image only
#   A_CONTAINER_BIN  where the agent is exposed inside the container
#   A_IMAGE_FALLBACK 1 = the image ships a usable copy if the host bin is absent
#   A_CONFIG_MOUNTS  host paths bind-mounted at IDENTICAL paths in mount auth
#   A_TOKEN_FILE     revocable-token file; "" = this agent has no token auth
#   A_TOKEN_ENV      env var the token is handed to podman as
#   A_ALWAYS_ENV     NAME=VALUE passed on every run
#   A_FW_ENV         NAME=VALUE passed only when a firewall mode is on
#   A_DOMAINS        comma-separated egress domains this agent needs
#   A_HOME           HOME inside the container when NOT using token auth
#   A_EPHEMERAL_HOME HOME under token auth (the image's own writable home)
declare -a A_CONFIG_MOUNTS=() A_ALWAYS_ENV=() A_FW_ENV=()
A_EPHEMERAL_HOME=""
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
    ;;
  codex)
    # The host launcher symlinks into ~/.codex, so the config mount already
    # covers the binary; bridge the resolved path anyway so the container path
    # does not depend on the host's package layout.
    A_HOST_BIN="$HOME_DIR/.local/bin/codex"
    A_CONTAINER_BIN=/usr/local/bin/codex
    A_IMAGE_FALLBACK=0
    A_CONFIG_MOUNTS=( "$HOME_DIR/.codex" )
    A_TOKEN_FILE=""
    A_TOKEN_ENV=""
    A_DOMAINS=""
    A_HOME="$HOME_DIR"
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
    A_DOMAINS=""
    A_HOME="$HOME_DIR"
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
    echo "[agent-in-podman] unknown --sandbox-agent '$AGENT' (want: claude|codex|agy|bash)" >&2
    exit 2
    ;;
esac

# Bind-mount at IDENTICAL host paths so (a) claude's cwd-hash session resume and
# (b) git's worktree gitdir link both resolve inside the container. We mount the
# repo root (which covers the worktree + the shared .git) and, only if the
# worktree lives outside that root, the worktree itself.
declare -a MOUNTS=() MOUNT_PATHS=()
add_mount() {
  local p="$1" m
  if [ "${#MOUNT_PATHS[@]}" -gt 0 ]; then
    for m in "${MOUNT_PATHS[@]}"; do
      case "$p/" in "$m"/*) return ;; esac # already covered by an outer mount
    done
  fi
  MOUNT_PATHS+=( "$p" )
  MOUNTS+=( -v "$p:$p" )
}
if common=$(git -C "$WT" rev-parse --git-common-dir 2>/dev/null); then
  add_mount "$(dirname "$(cd "$WT" && readlink -f "$common")")"
fi
add_mount "$WT"

# Auth. Two modes, and which are AVAILABLE is a property of the agent (table):
#
#  (a) Token (preferred, hardened): if the agent has a token file — claude's is
#      ~/.config/harness/sandbox-claude-token, override with
#      HARNESS_SANDBOX_CLAUDE_TOKEN_FILE — auth via the agent's token env (a
#      DEDICATED, revocable `claude setup-token`) and DO NOT mount the personal
#      config dir (which holds the *permanent* refresh token). The agent runs
#      from the image's own writable home, so its config is ephemeral per run
#      (no host-session resume — accepted trade for not exposing the refresh
#      token). We never read the token's bytes; podman receives it as an env.
#
#      ONLY claude has this. codex and agy have no known revocable-token mode,
#      so their table entries leave A_TOKEN_FILE empty and they always land on
#      (b) — which the README states plainly rather than implying.
#
#  (b) Mount (fallback): reuse the host login by bind-mounting the agent's
#      config paths. This exposes that provider's credentials to the container
#      — see the README security section.
TOKEN_FILE="$A_TOKEN_FILE"
AGENT_HOME="$A_HOME"
declare -a AUTH=()
auth_mode="mount"
if [ -n "$TOKEN_FILE" ] && [ -s "$TOKEN_FILE" ] && [ "$force_mount" != 1 ]; then
  auth_mode="token"
  AGENT_HOME="$A_EPHEMERAL_HOME"
  # SANDBOX_SEED_CONFIG tells the in-container launcher to pre-seed onboarding +
  # trust-this-folder for the worktree (ephemeral home re-prompts otherwise).
  # SANDBOX_SEED_PROJECTS carries the mounted roots, newline-separated. claude
  # keys "trust this folder" on the REPO ROOT, not the cwd: seeding only $PWD
  # (the task worktree) left every session opening on the safety-check dialog,
  # and answering it wrote an entry for the repo root. The launcher seeds each
  # of these plus its own $PWD.
  AUTH=(
    --env "$A_TOKEN_ENV=$(cat "$TOKEN_FILE")"
    --env SANDBOX_SEED_CONFIG=1
    --env SANDBOX_SEED_PROJECTS="$(printf '%s\n' "${MOUNT_PATHS[@]}")"
  )
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
# Pure pass-through: claude args (incl. --dangerously-skip-permissions, which the
# runner forwards via --claude-args / submit --claude-arg) arrive in "$@". The
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
# (spawn, file_read/write, forward, exec_attach, ...) server-side.
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

# Egress firewall (opt-in via --firewall). Start as container-root with the caps
# the iptables setup needs; the entrypoint applies the allowlist then drops to
# the keep-id host user to run claude. The harness server (parsed from
# HARNESS_SERVER_CID) is allowlisted so the bridged harness-cli still reaches it.
declare -a FW=()
if [ "$firewall" = 1 ] || [ "$firewall_proxy" = 1 ]; then
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
  if [ "$firewall_proxy" = 1 ]; then
    FW+=( --env SANDBOX_FIREWALL_PROXY=1 )
    # Extend the proxy's domain allowlist (comma-separated) for WebFetch research
    # targets via SANDBOX_PROXY_ALLOW in the runner's env.
    [ -n "${SANDBOX_PROXY_ALLOW:-}" ] && FW+=( --env SANDBOX_PROXY_ALLOW="$SANDBOX_PROXY_ALLOW" )
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
# (static-pie musl, so libc-independent by construction).
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
    HOSTBIN+=( -v "$hbin:$A_CONTAINER_BIN:ro" )
    agent_src="host:$(basename "$hbin")"
  elif [ "$A_IMAGE_FALLBACK" != 1 ]; then
    echo "[agent-in-podman] $AGENT: no host binary at $A_HOST_BIN and no image fallback" >&2
    exit 2
  fi
elif [ -z "$A_HOST_BIN" ] && [ "$A_IMAGE_FALLBACK" != 1 ]; then
  echo "[agent-in-podman] $AGENT: no host binary and no image fallback" >&2
  exit 2
fi

# One-line summary of the chosen modes → the runner log (token VALUE never shown).
fw_mode="none"; [ "$firewall" = 1 ] && fw_mode="ip"; [ "$firewall_proxy" = 1 ] && fw_mode="proxy"
echo "[agent-in-podman] agent=$AGENT auth=$auth_mode firewall=$fw_mode harness-cli=$([ "$bridge_cli" = 1 ] && echo on || echo off) bin=$agent_src image=$IMAGE" >&2

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
  wrapper_pid="$1"; cidfile="$2"
  log="${TMPDIR:-/tmp}/sandbox-reaper.log"
  # cd out of the inherited cwd (= the task worktree): the runner deletes the
  # worktree on session kill, and podman aborts on a vanished cwd ("error
  # getting current working directory") before it ever touches the container.
  cd /
  while kill -0 "$wrapper_pid" 2>/dev/null; do sleep 0.5; done
  echo "$(date "+%F %T") reaper: wrapper $wrapper_pid gone cid=$(head -c12 "$cidfile" 2>/dev/null || echo no-cidfile)" >>"$log"
  deadline=$((SECONDS + 60))
  while [ -e "$cidfile" ] && [ "$SECONDS" -lt "$deadline" ]; do
    err=$(timeout 10 podman rm -f -i -t 1 --cidfile "$cidfile" 2>&1) && { echo "$(date "+%F %T") reaper: removed" >>"$log"; break; }
    echo "$(date "+%F %T") reaper: rm rc=$? ${err}" >>"$log"
    sleep 1
  done
  rm -f "$cidfile"
' _ "$$" "$cidfile" </dev/null >/dev/null 2>&1 &

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
exec podman run --rm -i "${TTY[@]}" \
  --init \
  --userns=keep-id \
  --security-opt label=disable \
  --security-opt no-new-privileges \
  --cidfile "$cidfile" \
  -w "$WT" \
  --env HOME="$AGENT_HOME" \
  --env SANDBOX_AGENT="$AGENT" \
  --env SANDBOX_AGENT_BIN="$A_CONTAINER_BIN" \
  "${AUTH[@]}" \
  "${CLI[@]}" \
  "${FW[@]}" \
  "${HOSTBIN[@]}" \
  "${MOUNTS[@]}" \
  "$IMAGE" \
  /usr/local/bin/sandbox-agent-launch.sh "$@"
