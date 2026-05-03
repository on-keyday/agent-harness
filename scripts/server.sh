#!/usr/bin/env bash
# server.sh — control harness-server as a nohup-detached background process.
#
# Usage:
#   scripts/server.sh up [harness-server flags...]
#   scripts/server.sh down
#
# Examples:
#   scripts/server.sh up --listen :8539 --data-dir ./harness-data
#   scripts/server.sh up --psk-file ./psk
#   scripts/server.sh down
#
# State: bin/.run/harness-server.{pid,log}.
# Build with `make build` before first `up`.

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=_daemon.sh
. "$HERE/_daemon.sh"

cmd="${1:-}"
shift || true
case "$cmd" in
    up)   daemon_up   harness-server "$@" ;;
    down) daemon_down harness-server ;;
    *)    echo "usage: $0 {up [flags...]|down}" >&2; exit 2 ;;
esac
