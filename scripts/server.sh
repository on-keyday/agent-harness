#!/usr/bin/env bash
# server.sh — control harness-server as a nohup-detached background process.
#
# Usage:
#   scripts/server.sh up [--as TAG] [harness-server flags...]
#   scripts/server.sh down [--as TAG]
#
# --as is supported for symmetry with runner.sh, in case you want to run
# multiple servers on the same host (e.g. listening on different ports,
# different data-dirs); without --as the slot is "harness-server".
#
# Examples:
#   scripts/server.sh up --listen :8539 --data-dir ./harness-data
#   scripts/server.sh up --psk-file ./psk
#   scripts/server.sh up --as alt --listen :8540 --data-dir ./harness-data-alt
#   scripts/server.sh down
#   scripts/server.sh down --as alt
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
slot="harness-server${tag:+-$tag}"

case "$cmd" in
    up)   daemon_up   "$slot" harness-server "$@" ;;
    down) daemon_down "$slot" harness-server ;;
    *)    echo "usage: $0 {up [--as TAG] [flags...]|down [--as TAG]}" >&2; exit 2 ;;
esac
