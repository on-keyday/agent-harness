#!/usr/bin/env bash
# runner.sh — control agent-runner as a nohup-detached background process.
#
# Usage:
#   scripts/runner.sh up [agent-runner flags...]
#   scripts/runner.sh down
#
# Examples:
#   scripts/runner.sh up --server-cid ws:127.0.0.1:8539-* --roots "$PWD"
#   scripts/runner.sh up --server-cid ws:harness.host:8539-* \
#                        --roots /srv/repos/foo,/srv/repos/bar \
#                        --psk-file ./psk --max-tasks 4
#   scripts/runner.sh down
#
# State: bin/.run/agent-runner.{pid,log}.
# Build with `make build` before first `up`.

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=_daemon.sh
. "$HERE/_daemon.sh"

cmd="${1:-}"
shift || true
case "$cmd" in
    up)   daemon_up   agent-runner "$@" ;;
    down) daemon_down agent-runner ;;
    *)    echo "usage: $0 {up [flags...]|down}" >&2; exit 2 ;;
esac
