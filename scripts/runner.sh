#!/usr/bin/env bash
# runner.sh — thin wrapper around scripts/runner.py.
#
# runner.py (via daemon.py) is the canonical cross-platform implementation;
# this wrapper exists so the documented command line `scripts/runner.sh up
# --as <tag> ...` keeps working without callers needing to remember which
# extension to use.
#
# Subcommands and flags are forwarded verbatim — see runner.py for the
# authoritative usage.

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
exec "$HERE/runner.py" "$@"
