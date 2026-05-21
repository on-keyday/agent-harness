#!/usr/bin/env bash
# restart.sh — thin wrapper around scripts/restart.py.
#
# restart.py is the canonical cross-platform implementation (reads argv,
# cwd, and exe via psutil so it works without /proc); this wrapper exists
# so the documented command line `scripts/restart.sh <slot>` keeps
# working without callers needing to remember which extension to use.
#
# The <slot> argument is forwarded verbatim — see restart.py for the
# authoritative usage.

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
exec "$HERE/restart.py" "$@"
