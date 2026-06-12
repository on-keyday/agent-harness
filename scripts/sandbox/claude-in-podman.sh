#!/usr/bin/env bash
# claude-in-podman.sh — a `--claude-bin` target that runs the real Claude Code
# inside a ROOTLESS podman container, confining the agent's command execution
# while keeping worktree edits owned by the host user.
#
# Why podman + --userns=keep-id: Claude Code refuses --dangerously-skip-permissions
# when running as root, but rootless *docker* can only write host-owned bind
# mounts as container-root. podman keep-id maps the host user (e.g. uid 1000)
# into the container unchanged, so claude runs NON-root (flag accepted) AND the
# worktree edits stay owned by the host user on disk.
#
# The runner invokes this with cwd = the task worktree and forwards claude's
# args (including `--dangerously-skip-permissions ... -p <prompt>`) as "$@".
#
# SCOPE: one-shot (`-p`) verified end-to-end; interactive gets a container TTY
# when stdin is one. harness-cli + the HARNESS_* env are bridged in by default so
# the confined agent keeps the control plane (--omit-harness-cli to disable).
# Network egress is open (allowlist firewall = TODO). See README.md.
set -euo pipefail

IMAGE="${HARNESS_SANDBOX_IMAGE:-harness-claude-sandbox:latest}"
WT="$PWD"                                  # the runner sets cwd to the worktree
HOME_DIR="${HOME:-/home/$(id -un)}"

# Consume our own control flags from the arg stream (NOT claude flags, so they
# must not reach claude). Pass them via `--claude-arg` / runner `--claude-args`:
#   --omit-harness-cli  run with NO harness control plane in the container (full
#                       isolation); default is to bridge harness-cli + HARNESS_* in.
#   --firewall          apply the iptables+ipset egress allowlist
#                       (init-firewall.sh); default is an open network.
#   --firewall-proxy    stronger egress: deny-all + an in-container allowlisting
#                       CONNECT proxy (connect-proxy.py); the agent gets no raw
#                       egress and its API/WebFetch funnel through the proxy.
#                       Takes precedence over --firewall if both are given.
#   --mount-auth        force MOUNT auth (bind-mount the host ~/.claude) even when
#                       a token file exists. Mount auth persists session state, so
#                       --continue / resume work — at the cost of exposing the
#                       refresh token. Use it for trusted, resumable tasks; leave
#                       it off for untrusted work (token auth, ephemeral).
bridge_cli=1
firewall=0
firewall_proxy=0
force_mount=0
declare -a ARGS=()
for a in "$@"; do
  case "$a" in
    --omit-harness-cli) bridge_cli=0 ;;
    --firewall)         firewall=1 ;;
    --firewall-proxy)   firewall_proxy=1 ;;
    --mount-auth)       force_mount=1 ;;
    *)                  ARGS+=( "$a" ) ;;
  esac
done
if [ "${#ARGS[@]}" -gt 0 ]; then set -- "${ARGS[@]}"; else set --; fi

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

# Auth. Two modes:
#
#  (a) Token (preferred, hardened): if a token file exists — default
#      ~/.config/harness/sandbox-claude-token, override with
#      HARNESS_SANDBOX_CLAUDE_TOKEN_FILE — auth via CLAUDE_CODE_OAUTH_TOKEN (a
#      DEDICATED, revocable `claude setup-token`) and DO NOT mount the personal
#      ~/.claude (which holds the *permanent* refresh token). claude runs from the
#      image's own writable home (/home/node), so ~/.claude is ephemeral per run
#      (no host-session resume — accepted trade for not exposing the refresh
#      token). We never read the token's bytes; podman receives it as an env.
#
#  (b) Mount (fallback): reuse the host login by bind-mounting ~/.claude (+
#      ~/.claude.json). This exposes the permanent refresh token to the container
#      — see the README security section.
TOKEN_FILE="${HARNESS_SANDBOX_CLAUDE_TOKEN_FILE:-$HOME_DIR/.config/harness/sandbox-claude-token}"
CLAUDE_HOME="$HOME_DIR"
declare -a AUTH=()
auth_mode="mount"
if [ -s "$TOKEN_FILE" ] && [ "$force_mount" != 1 ]; then
  auth_mode="token"
  CLAUDE_HOME="/home/node"
  # SANDBOX_SEED_CONFIG tells the in-container launcher to pre-seed onboarding +
  # trust-this-folder for the worktree (ephemeral home re-prompts otherwise).
  AUTH=( --env CLAUDE_CODE_OAUTH_TOKEN="$(cat "$TOKEN_FILE")" --env SANDBOX_SEED_CONFIG=1 )
else
  AUTH=( -v "$HOME_DIR/.claude:$HOME_DIR/.claude" )
  [ -f "$HOME_DIR/.claude.json" ] && AUTH+=( -v "$HOME_DIR/.claude.json:$HOME_DIR/.claude.json" )
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
  server_ip=$(printf '%s' "${HARNESS_SERVER_CID:-}" | sed -E 's#^[a-z]+:##; s#[:-].*##')
  FW=(
    --user 0
    --cap-add=NET_ADMIN --cap-add=NET_RAW
    --env DROP_UID="$(id -u)" --env DROP_GID="$(id -g)"
    --env SANDBOX_SERVER_IP="$server_ip"
    # Disable claude's non-essential egress (telemetry → datadog, statsig
    # feature-flags, auto-update, error reporting). Verified A/B that this drops
    # http-intake.logs.us5.datadoghq.com etc. — so neither the allowlist nor the
    # proxy needs those telemetry CDNs, and fail-closed won't stall on them.
    --env CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1
    --entrypoint /usr/local/bin/sandbox-entrypoint.sh
  )
  if [ "$firewall_proxy" = 1 ]; then
    FW+=( --env SANDBOX_FIREWALL_PROXY=1 )
    # Extend the proxy's domain allowlist (comma-separated) for WebFetch research
    # targets via SANDBOX_PROXY_ALLOW in the runner's env.
    [ -n "${SANDBOX_PROXY_ALLOW:-}" ] && FW+=( --env SANDBOX_PROXY_ALLOW="$SANDBOX_PROXY_ALLOW" )
  else
    FW+=( --env SANDBOX_FIREWALL=1 )
  fi
fi

# One-line summary of the chosen modes → the runner log (token VALUE never shown).
fw_mode="none"; [ "$firewall" = 1 ] && fw_mode="ip"; [ "$firewall_proxy" = 1 ] && fw_mode="proxy"
echo "[claude-in-podman] auth=$auth_mode firewall=$fw_mode harness-cli=$([ "$bridge_cli" = 1 ] && echo on || echo off) image=$IMAGE" >&2

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

exec podman run --rm -i "${TTY[@]}" \
  --userns=keep-id \
  --security-opt label=disable \
  --security-opt no-new-privileges \
  --cidfile "$cidfile" \
  -w "$WT" \
  --env HOME="$CLAUDE_HOME" \
  "${AUTH[@]}" \
  "${CLI[@]}" \
  "${FW[@]}" \
  "${MOUNTS[@]}" \
  "$IMAGE" \
  /usr/local/bin/sandbox-claude-launch.sh "$@"
