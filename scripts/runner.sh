#!/usr/bin/env bash
# runner.sh — control agent-runner as a nohup-detached background process.
#
# Usage:
#   scripts/runner.sh up [--as TAG] [agent-runner flags...]
#   scripts/runner.sh down [--as TAG]
#
# Without --as, the slot is "agent-runner" (single primary instance).
# With --as TAG, the slot is "agent-runner-<TAG>", letting you run several
# concurrent runners on the same host (e.g. pinned to different roots, or
# just for extra parallel slots beyond a single process's --max-tasks cap).
# Each slot has its own bin/.run/<slot>.{pid,log}; up / down / restart act
# on whichever slot --as selects.
#
# Examples:
#   scripts/runner.sh up --server-cid ws:127.0.0.1:8539-* --roots "$PWD"
#   scripts/runner.sh up --as 2 --server-cid ws:127.0.0.1:8539-* --roots "$PWD" --max-tasks 2
#   scripts/runner.sh up --server-cid ws:harness.host:8539-* \
#                        --roots /srv/repos/foo,/srv/repos/bar \
#                        --psk-file ./psk --max-tasks 4
#   scripts/runner.sh down
#   scripts/runner.sh down --as 2
#
# State: bin/.run/<slot>.{pid,log}.
# Build with `make build` before first `up`.

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=_daemon.sh
. "$HERE/_daemon.sh"

cmd="${1:-}"
shift || true

tag=""
if [[ "${1:-}" == "--as" ]]; then
    shift
    tag="${1:-}"
    if [[ -z "$tag" ]]; then
        echo "usage: $0 $cmd --as TAG [...]" >&2
        exit 2
    fi
    shift
fi
slot="agent-runner${tag:+-$tag}"

case "$cmd" in
    up)   daemon_up   "$slot" agent-runner "$@" ;;
    down) daemon_down "$slot" agent-runner ;;
    *)    echo "usage: $0 {up [--as TAG] [flags...]|down [--as TAG]}" >&2; exit 2 ;;
esac
