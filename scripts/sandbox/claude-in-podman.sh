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

# Consume our own control flag from the arg stream (NOT a claude flag, so it must
# not reach claude). Pass `--claude-arg --omit-harness-cli` (or runner
# --claude-args) to run with NO harness control plane in the container = full
# isolation. Default: bridge harness-cli + the HARNESS_* env in (see below).
bridge_cli=1
declare -a ARGS=()
for a in "$@"; do
  if [ "$a" = "--omit-harness-cli" ]; then bridge_cli=0; else ARGS+=( "$a" ); fi
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

# HOME/.claude (dir) is bind-mounted so the container reuses the host login +
# session store (claude resumes a worktree by cwd hash). claude ALSO keeps a
# top-level ~/.claude.json (settings, per-project state) — mount it too when it
# exists, else claude warns "config not found" every run and rewrites it.
# Everything else under HOME is ephemeral per run.
declare -a CLAUDE_JSON=()
if [ -f "$HOME_DIR/.claude.json" ]; then
  CLAUDE_JSON=( -v "$HOME_DIR/.claude.json:$HOME_DIR/.claude.json" )
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

exec podman run --rm -i "${TTY[@]}" \
  --userns=keep-id \
  --security-opt label=disable \
  -w "$WT" \
  --env HOME="$HOME_DIR" \
  -v "$HOME_DIR/.claude:$HOME_DIR/.claude" \
  "${CLAUDE_JSON[@]}" \
  "${CLI[@]}" \
  "${MOUNTS[@]}" \
  "$IMAGE" \
  claude "$@"
