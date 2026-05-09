#!/usr/bin/env python3
"""runner.py — control agent-runner as a detached background process.

Cross-platform port of ``scripts/runner.sh``. State (pid/log files) and
slot naming are interchangeable with the Bash version.

Usage::

    python scripts/runner.py up [--as TAG] [agent-runner flags...]
    python scripts/runner.py down [--as TAG]

Without ``--as``, the slot is ``agent-runner`` (single primary instance).
With ``--as TAG``, the slot is ``agent-runner-<TAG>``, letting you run
several concurrent runners on the same host (e.g. pinned to different
roots, or just for extra parallel slots beyond a single process's
``--max-tasks`` cap). Each slot has its own ``bin/.run/<slot>.{pid,log}``;
up / down / restart act on whichever slot ``--as`` selects.

Examples::

    python scripts/runner.py up --server-cid ws:127.0.0.1:8539-* --roots "$PWD"
    python scripts/runner.py up --as 2 --server-cid ws:127.0.0.1:8539-* \
        --roots "$PWD" --max-tasks 2
    python scripts/runner.py down
    python scripts/runner.py down --as 2

State: ``bin/.run/<slot>.{pid,log}``. Build with ``make build`` before
first ``up``.
"""

from __future__ import annotations

import sys

from bootstrap import ensure_venv

ensure_venv()

# Anything below here runs inside scripts/.venv/. psutil-touching modules
# may safely be imported.
from daemon import daemon_down, daemon_up

_BIN = "agent-runner"
_DEFAULT_SLOT = "agent-runner"


def _parse_tag(args: list[str]) -> tuple[str, list[str]]:
    """Strip a leading ``--as TAG`` from *args*; return (slot, remaining-args)."""
    tag = ""
    if args and args[0] == "--as":
        if len(args) < 2 or not args[1]:
            sys.stderr.write("usage: runner.py {up|down} --as TAG [...]\n")
            sys.exit(2)
        tag = args[1]
        args = args[2:]
    slot = f"{_DEFAULT_SLOT}-{tag}" if tag else _DEFAULT_SLOT
    return slot, args


def _usage_and_exit() -> None:
    sys.stderr.write(
        "usage: runner.py {up [--as TAG] [flags...]|down [--as TAG]}\n"
    )
    sys.exit(2)


def main(argv: list[str]) -> int:
    if not argv:
        _usage_and_exit()
    cmd, rest = argv[0], argv[1:]
    slot, rest = _parse_tag(rest)
    if cmd == "up":
        try:
            daemon_up(slot, _BIN, *rest)
        except (FileNotFoundError, RuntimeError):
            return 1
        return 0
    if cmd == "down":
        try:
            daemon_down(slot, _BIN)
        except RuntimeError:
            return 1
        return 0
    _usage_and_exit()
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
