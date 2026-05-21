#!/usr/bin/env bash
# server.sh — thin wrapper around scripts/server.py.
#
# server.py (via daemon.py) is the canonical cross-platform implementation;
# this wrapper exists so the documented command line `scripts/server.sh up
# [--as <tag>] ...` keeps working without callers needing to remember
# which extension to use.
#
# Subcommands and flags are forwarded verbatim — see server.py for the
# authoritative usage.

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
exec "$HERE/server.py" "$@"
