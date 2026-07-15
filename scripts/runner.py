#!/usr/bin/env python3
"""runner.py — control agent-runner as a detached background process.

Cross-platform port of ``scripts/runner.sh``. State (pid/log files) and
slot naming are interchangeable with the Bash version.

Usage::

    python scripts/runner.py up [--as TAG] [--agents NAME,NAME,...] [--dry-run] [agent-runner flags...]
    python scripts/runner.py down [--as TAG]

Without ``--as``, the slot is ``agent-runner`` (single primary instance).
With ``--as TAG``, the slot is ``agent-runner-<TAG>``, letting you run
several concurrent runners on the same host (e.g. pinned to different
roots, or just for extra parallel slots beyond a single process's
``--max-tasks`` cap). Each slot has its own ``bin/.run/<slot>.{pid,log}``;
up / down / restart act on whichever slot ``--as`` selects.

``--agents NAME,NAME,...`` (``up`` only) is a preset shortcut over the raw
``--agent-bin`` / ``--agent-*-argv`` / ``--agent-profiles`` flags this
script otherwise forwards verbatim. The first name becomes the default
profile (``--agent-bin`` + argv-template flags); any remaining names are
serialized into a single ``--agent-profiles`` JSON flag. Only names with a
known, authoritative argv shape are supported (currently ``claude``,
``codex``, ``bash`` — see ``scripts/agent_presets.py`` and
``.claude/commands/runner-up.md``); an unknown name (e.g. ``gemini``, which
has no built-in preset anywhere in this repo) is a hard error telling you
to pass ``--agent-profiles`` JSON directly instead of guessing. ``--agents``
also refuses to run alongside an explicit ``--agent-bin`` / ``--agent-*-argv``
/ ``--agent-profiles`` flag in the same invocation — pass one or the other,
not both.

``--dry-run`` (``up`` only) prints the fully expanded flag list (after
``--agents`` expansion) instead of spawning the runner. Useful to inspect
what a preset expands to before committing to it.

Examples::

    python scripts/runner.py up --server-cid ws:127.0.0.1:8539-* --roots "$PWD"
    python scripts/runner.py up --as 2 --server-cid ws:127.0.0.1:8539-* \
        --roots "$PWD" --max-tasks 2
    python scripts/runner.py up --agents claude,codex \
        --server-cid ws:127.0.0.1:8539-* --roots "$PWD"
    python scripts/runner.py up --agents claude,codex --dry-run
    python scripts/runner.py down
    python scripts/runner.py down --as 2

State: ``bin/.run/<slot>.{pid,log}``. Build with ``make build`` before
first ``up``.
"""

from __future__ import annotations

import shlex
import sys

from agent_presets import AgentsPresetError, expand_agents_preset
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


def _parse_agents(args: list[str]) -> tuple[str, list[str]]:
    """Extract ``--agents VALUE`` / ``--agents=VALUE`` from *args*, wherever
    it appears (unlike ``--as``, which must lead); return
    (value-or-"", remaining-args-with-it-removed)."""
    out: list[str] = []
    value = ""
    i = 0
    while i < len(args):
        a = args[i]
        if a == "--agents":
            if i + 1 >= len(args):
                sys.stderr.write(
                    "usage: --agents requires a value, e.g. --agents claude,codex\n"
                )
                sys.exit(2)
            value = args[i + 1]
            i += 2
            continue
        if a.startswith("--agents="):
            value = a[len("--agents=") :]
            i += 1
            continue
        out.append(a)
        i += 1
    return value, out


def _usage_and_exit() -> None:
    sys.stderr.write(
        "usage: runner.py {up [--as TAG] [--agents NAME,NAME,...] [--dry-run] "
        "[flags...]|down [--as TAG]}\n"
    )
    sys.exit(2)


def main(argv: list[str]) -> int:
    if not argv:
        _usage_and_exit()
    cmd, rest = argv[0], argv[1:]
    slot, rest = _parse_tag(rest)
    if cmd == "up":
        agents_csv, rest = _parse_agents(rest)
        if agents_csv:
            try:
                rest = expand_agents_preset(agents_csv, rest) + rest
            except AgentsPresetError as exc:
                sys.stderr.write(f"runner.py: {exc}\n")
                sys.exit(2)
        dry_run = "--dry-run" in rest
        if dry_run:
            rest = [a for a in rest if a != "--dry-run"]
            print(" ".join(shlex.quote(a) for a in rest))
            return 0
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
