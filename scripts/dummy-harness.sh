#!/usr/bin/env bash
# dummy-harness.sh — thin wrapper around scripts/dummy-harness.py.
#
# dummy-harness.py is the canonical cross-platform implementation; this wrapper
# exists so the documented command line `scripts/dummy-harness.sh up --detach
# --agent fake --name T` keeps working without callers needing to remember which
# extension to use. Same shape as runner.sh and restart.sh.
#
# The shell version WAS the implementation until it turned out not to run on
# Windows at all — MSYS make dropped the Go environment, Git Bash's `-x` would
# not resolve `.exe`, and the rest assumed mktemp / /dev/urandom / kill -0 / ss.
# Which mattered more than usual: this script exists to make a live check
# repeatable, and the check that most needed it was the Windows one.
#
# Subcommands and flags are forwarded verbatim — see dummy-harness.py for the
# authoritative usage.

set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
exec "$HERE/dummy-harness.py" "$@"
