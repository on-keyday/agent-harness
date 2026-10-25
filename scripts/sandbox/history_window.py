#!/usr/bin/env python3
"""Keep a codex task's sessions directory in hardlink sync with the host's.

agent-in-podman.sh --history-window mounts a per-task directory at the
container's ~/.codex/sessions. codex partitions rollouts by DATE
(sessions/YYYY/MM/DD/rollout-*.jsonl) and records the cwd only in the first
line, so there is no per-cwd host directory to bind. Instead this helper
hardlinks the host rollouts of one cwd into the per-task directory before the
container starts (codex-import), and links rollouts the container created back
to the host (codex-export, run by the wrapper's reaper). Both names are one
inode, and codex appends with append(true), so writes reach both sides at once.

The host is the source of truth: nothing under the host directory is ever
written, overwritten or unlinked here, only linked into. See
docs/superpowers/specs/2026-10-26-sandbox-history-window-design.md §7.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from pathlib import Path
from typing import TextIO

# A first line carries base_instructions, so it is large; a rollout whose first
# line has not ended within this many bytes is treated as still being written.
FIRST_LINE_LIMIT = 8 * 1024 * 1024
PATTERN = "[0-9][0-9][0-9][0-9]/[0-9][0-9]/[0-9][0-9]/rollout-*.jsonl"


class CrossDevice(Exception):
    """The per-task directory and the host store cannot share inodes."""


def _device(path: Path) -> int:
    return os.stat(path).st_dev


def _check_device(task_dir: Path, host_sessions: Path) -> None:
    if _device(task_dir) != _device(host_sessions):
        raise CrossDevice(f"{task_dir} and {host_sessions} are on different filesystems")


def rollout_cwd(path: Path) -> str | None:
    """payload.cwd of a session_meta first line; None while it is incomplete or unparseable."""
    try:
        with open(path, "rb") as source:
            line = source.readline(FIRST_LINE_LIMIT)
    except OSError:
        return None
    if not line.endswith(b"\n"):
        return None
    try:
        first = json.loads(line)
    except ValueError:
        return None
    if not isinstance(first, dict) or first.get("type") != "session_meta":
        return None
    payload = first.get("payload")
    cwd = payload.get("cwd") if isinstance(payload, dict) else None
    return cwd if isinstance(cwd, str) else None


def same_dir(a: str, b: str) -> bool:
    return os.path.realpath(a) == os.path.realpath(b)


def _rollouts(root: Path) -> list[Path]:
    return sorted(root.glob(PATTERN))


def codex_export(task_dir: Path, host_sessions: Path, cwd: str, log: TextIO) -> list[Path]:
    """Link rollouts the container created into the host store; never touch an existing host file."""
    _check_device(task_dir, host_sessions)
    linked = []
    for path in _rollouts(task_dir):
        rel = path.relative_to(task_dir)
        target = host_sessions / rel
        try:
            if target.stat().st_ino != path.stat().st_ino:
                print(f"diverged {rel}", file=log)
            continue
        except FileNotFoundError:
            pass
        found = rollout_cwd(path)
        if found is None:
            continue  # first line not flushed yet; the next round sees it
        if not same_dir(found, cwd):
            print(f"refused {rel} cwd={found}", file=log)
            continue
        target.parent.mkdir(parents=True, exist_ok=True)
        try:
            os.link(path, target)
        except FileExistsError:
            continue  # the host created it between the stat and the link
        linked.append(target)
    return linked


def codex_import(task_dir: Path, host_sessions: Path, cwd: str, log: TextIO) -> list[Path]:
    """Rebuild the per-task directory from the host rollouts of cwd.

    Exports first: a rollout a previous run created but never linked (the
    reaper died with the host) would otherwise be lost to the clearing below.
    """
    codex_export(task_dir, host_sessions, cwd, log)
    for path in _rollouts(task_dir):
        path.unlink()
    for directory in sorted((p for p in task_dir.rglob("*") if p.is_dir()), key=lambda p: len(p.parts), reverse=True):
        try:
            directory.rmdir()
        except OSError:
            pass  # not empty: something other than a rollout lives there
    linked = []
    for path in _rollouts(host_sessions):
        found = rollout_cwd(path)
        if found is None or not same_dir(found, cwd):
            continue
        target = task_dir / path.relative_to(host_sessions)
        target.parent.mkdir(parents=True, exist_ok=True)
        os.link(path, target)
        linked.append(target)
    return linked


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("command", choices=("codex-import", "codex-export"))
    parser.add_argument("--task-dir", type=Path, required=True)
    parser.add_argument("--host-sessions", type=Path, required=True)
    parser.add_argument("--cwd", required=True)
    args = parser.parse_args(argv)
    run = codex_import if args.command == "codex-import" else codex_export
    try:
        run(args.task_dir, args.host_sessions, args.cwd, sys.stderr)
    except CrossDevice as error:
        print(f"[agent-in-podman] --history-window: {error}", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
