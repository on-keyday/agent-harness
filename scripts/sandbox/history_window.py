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
After the container is gone the reaper also has the host's codex re-index the
cwd (codex-reindex), because the host's state DB never saw the threads the
container created.

The host is the source of truth: nothing under the host directory is ever
written, overwritten or unlinked here, only linked into. See
docs/superpowers/specs/2026-10-26-sandbox-history-window-design.md §7.
"""

from __future__ import annotations

import argparse
import json
import os
import select
import shutil
import stat
import subprocess
import sys
import time
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
    """Regular files at YYYY/MM/DD/rollout-*.jsonl, never through a symlink.

    The per-task directory is the container's sessions dir, so the container
    can plant a symlink in it; Path.glob follows symlinked directories, and the
    clearing in codex_import then deleted host rollouts of every cwd.
    """
    found = []
    for directory, subdirs, files in os.walk(root, followlinks=False):
        rel = Path(directory).relative_to(root)
        for name in files:
            path = Path(directory) / name
            if (len(rel.parts) == 3 and (rel / name).match(PATTERN)
                    and stat.S_ISREG(os.lstat(path).st_mode)):
                found.append(path)
    return sorted(found)


def _manifest(task_dir: Path) -> Path:
    """What the last import linked from the host. Kept beside the per-task
    directory, not in it, because the container can write the directory."""
    return task_dir.with_name(task_dir.name + ".imported")


def _imported(task_dir: Path) -> set[str]:
    try:
        return set(_manifest(task_dir).read_text().splitlines())
    except FileNotFoundError:
        return set()


def codex_export(task_dir: Path, host_sessions: Path, cwd: str, log: TextIO) -> list[Path]:
    """Link rollouts the container created into the host store; never touch an existing host file."""
    _check_device(task_dir, host_sessions)
    from_host = _imported(task_dir)
    linked = []
    for path in _rollouts(task_dir):
        rel = path.relative_to(task_dir)
        if str(rel) in from_host:
            continue  # came from the host; if it is gone there, the host removed it (archive, delete)
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
            os.link(path, target, follow_symlinks=False)
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
    # Removes symlinks themselves, never what they point at.
    shutil.rmtree(task_dir)
    task_dir.mkdir()
    linked = []
    for path in _rollouts(host_sessions):
        found = rollout_cwd(path)
        if found is None or not same_dir(found, cwd):
            continue
        target = task_dir / path.relative_to(host_sessions)
        target.parent.mkdir(parents=True, exist_ok=True)
        os.link(path, target, follow_symlinks=False)
        linked.append(target)
    _manifest(task_dir).write_text("".join(f"{p.relative_to(task_dir)}\n" for p in linked))
    return linked


# exec/src/lib.rs all_thread_source_kinds(): every kind, so exec threads count.
SOURCE_KINDS = ["cli", "vscode", "exec", "appServer", "subAgent", "subAgentReview", "subAgentCompact",
                "subAgentThreadSpawn", "subAgentOther", "unknown"]


def reindex_params(cwd: str) -> dict:
    """The thread/list that makes codex scan and repair its DB for cwd.
    codex_storage_probe.py sends exactly this, so a release that stops
    honouring it fails the upstream probe."""
    return {"limit": 100, "cwd": cwd, "useStateDbOnly": False, "sortKey": "updated_at",
            "sourceKinds": SOURCE_KINDS}


def codex_reindex(codex: str, cwd: str, log: TextIO, timeout: float = 30) -> bool:
    """Make the host's state DB index this cwd's rollouts, including linked ones.

    `codex exec resume --last` (and the TUI's) asks the state DB first and treats
    any match for the cwd as authoritative; it scans rollout files only when the
    DB has none (exec/src/lib.rs resolve_resume_thread_id, codex 0.164.0). A
    thread the container created reached the host as a linked file the host's DB
    never saw, so the host resumed an older thread instead (measured
    2026-10-26). A thread/list with useStateDbOnly=false is codex's own
    scan-and-repair, which adds such rollouts to the DB.
    """
    env = {k: v for k, v in os.environ.items() if not k.startswith("HARNESS_")}
    try:
        # cwd "/", not the worktree: the container can write the worktree (a
        # project-scoped codex config there would run in the host's codex),
        # and the runner may already have removed it. thread/list names the
        # cwd itself.
        proc = subprocess.Popen([codex, "app-server", "--listen", "stdio://"], stdin=subprocess.PIPE,
                                stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, text=True, cwd="/", env=env, bufsize=1)
    except OSError as error:
        print(f"reindex: cannot start {codex}: {error}", file=log)
        return False
    deadline = time.monotonic() + timeout

    def rpc(request_id: int, method: str, params: dict) -> dict | None:
        try:
            proc.stdin.write(json.dumps({"id": request_id, "method": method, "params": params}) + "\n")
            proc.stdin.flush()
        except OSError:
            return None
        while time.monotonic() < deadline:
            if not select.select([proc.stdout], [], [], 0.5)[0]:
                continue
            line = proc.stdout.readline()
            if not line:
                return None
            try:
                reply = json.loads(line)
            except ValueError:
                continue
            if reply.get("id") == request_id:
                return reply
        return None

    try:
        listed = rpc(1, "initialize", {"clientInfo": {"name": "harness_history_window", "version": "0"}}) is not None
        if listed:
            proc.stdin.write('{"method":"initialized"}\n')
            proc.stdin.flush()
            reply = rpc(2, "thread/list", reindex_params(cwd))
            listed = reply is not None and "result" in reply
    except OSError:
        listed = False
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
    if not listed:
        print(f"reindex: {codex} app-server gave no thread/list for {cwd}", file=log)
    return listed


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("command", choices=("codex-import", "codex-export", "codex-reindex"))
    parser.add_argument("--task-dir", type=Path)
    parser.add_argument("--host-sessions", type=Path)
    parser.add_argument("--codex", help="codex-reindex: the host's codex binary")
    parser.add_argument("--cwd", required=True)
    args = parser.parse_args(argv)
    if args.command == "codex-reindex":
        if not args.codex:
            parser.error("codex-reindex needs --codex")
        return 0 if codex_reindex(args.codex, args.cwd, sys.stderr) else 1
    if args.task_dir is None or args.host_sessions is None:
        parser.error(f"{args.command} needs --task-dir and --host-sessions")
    run = codex_import if args.command == "codex-import" else codex_export
    try:
        run(args.task_dir, args.host_sessions, args.cwd, sys.stderr)
    except CrossDevice as error:
        print(f"[agent-in-podman] --history-window: {error}", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
