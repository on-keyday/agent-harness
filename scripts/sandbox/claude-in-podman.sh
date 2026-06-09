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
# SCOPE (v1): one-shot / print mode (`-p`). Interactive PTY passthrough and
# network egress restriction are deliberately out of scope here — see README.md.
set -euo pipefail

IMAGE="${HARNESS_SANDBOX_IMAGE:-harness-claude-sandbox:latest}"
WT="$PWD"                                  # the runner sets cwd to the worktree
HOME_DIR="${HOME:-/home/$(id -un)}"

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
exec podman run --rm -i \
  --userns=keep-id \
  --security-opt label=disable \
  -w "$WT" \
  --env HOME="$HOME_DIR" \
  -v "$HOME_DIR/.claude:$HOME_DIR/.claude" \
  "${CLAUDE_JSON[@]}" \
  "${MOUNTS[@]}" \
  "$IMAGE" \
  claude "$@"
