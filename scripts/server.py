#!/usr/bin/env python3
"""server.py — control harness-server as a detached background process.

Cross-platform port of ``scripts/server.sh``. State (pid/log files) and
slot naming are interchangeable with the Bash version.

Usage::

    python scripts/server.py up [--as TAG] [harness-server flags...]
    python scripts/server.py down [--as TAG]

``--as`` is supported for symmetry with ``runner.py``, in case you want
to run multiple servers on the same host (e.g. listening on different
ports, different data-dirs); without ``--as`` the slot is
``harness-server``.

Examples::

    python scripts/server.py up --listen :8539 --data-dir ./harness-data
    python scripts/server.py up --psk-file ./psk
    python scripts/server.py up --as alt --listen :8540 --data-dir ./harness-data-alt
    python scripts/server.py down
    python scripts/server.py down --as alt

State: ``bin/.run/<slot>.{pid,log}``. Build with ``make build`` before
first ``up``.
"""

from __future__ import annotations

import sys

from bootstrap import ensure_venv

ensure_venv()

from daemon import daemon_down, daemon_up

_BIN = "harness-server"
_DEFAULT_SLOT = "harness-server"


def _parse_tag(args: list[str]) -> tuple[str, list[str]]:
    tag = ""
    if args and args[0] == "--as":
        if len(args) < 2 or not args[1]:
            sys.stderr.write("usage: server.py {up|down} --as TAG [...]\n")
            sys.exit(2)
        tag = args[1]
        args = args[2:]
    slot = f"{_DEFAULT_SLOT}-{tag}" if tag else _DEFAULT_SLOT
    return slot, args


def _usage_and_exit() -> None:
    sys.stderr.write(
        "usage: server.py {up [--as TAG] [flags...]|down [--as TAG]}\n"
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
