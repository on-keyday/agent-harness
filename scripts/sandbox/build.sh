#!/usr/bin/env bash
# Build the Claude Code sandbox image used by claude-in-podman.sh.
#
#   scripts/sandbox/build.sh                       # :latest, claude latest
#   scripts/sandbox/build.sh --build-arg CLAUDE_VERSION=2.1.169
#   HARNESS_SANDBOX_IMAGE=foo:dev scripts/sandbox/build.sh
set -euo pipefail
DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
IMAGE="${HARNESS_SANDBOX_IMAGE:-harness-claude-sandbox:latest}"
exec podman build -t "$IMAGE" "$@" -f "$DIR/Containerfile" "$DIR"
