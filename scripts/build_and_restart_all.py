#!/usr/bin/env python3
"""build_and_restart_all.py -- make build, then restart every alive
agent-runner slot, restarting the slot that owns the calling process LAST.

Why "self last": when this script is invoked from inside a claude-code agent
spawned by an agent-runner, restarting the runner first would SIGHUP the
caller mid-script. Restarting "self" last lets the script complete the
build + restart of every other slot before tearing itself down. The detached
restart in scripts/restart.py keeps the actual restart sequence alive past
the cascade.

Slot discovery: walks bin/.run/agent-runner*.pid; only restarts slots whose
PID is currently alive AND whose binary verifies as agent-runner. Stale
pid files (process gone) are skipped silently — bring them back up via
the relevant scripts/runner.* helper.

Self-detection: walks the calling process's parent chain via psutil; the
first ancestor whose PID matches an agent-runner pid file is "self". When
no such ancestor exists (e.g. a normal dev shell running this manually),
all slots are restarted in arbitrary order.

Usage:
    python scripts/build_and_restart_all.py
    python scripts/build_and_restart_all.py --skip-build
    python scripts/build_and_restart_all.py --dry-run
"""

from __future__ import annotations

import argparse
import os
import subprocess
import sys
import time
from pathlib import Path

from bootstrap import ensure_venv

ensure_venv()

import psutil

import daemon as _daemon

_HERE = Path(__file__).resolve().parent
_ROOT = _HERE.parent
_RESTART_PY = _HERE / "restart.py"
_AGENT_RUNNER_BIN = "agent-runner"


def _alive_runner_slots() -> dict[int, str]:
    """Return {pid: slot} for every bin/.run/agent-runner*.pid whose process
    is alive and verifies as the agent-runner binary."""
    out: dict[int, str] = {}
    for pid_file in sorted(_daemon.RUN_DIR.glob("agent-runner*.pid")):
        slot = pid_file.stem
        try:
            pid = int(pid_file.read_text().strip())
        except (OSError, ValueError):
            continue
        if not psutil.pid_exists(pid):
            continue
        if not _daemon.verify_owner(pid, _AGENT_RUNNER_BIN):
            continue
        out[pid] = slot
    return out


def _find_self_slot(pid_to_slot: dict[int, str]) -> str | None:
    """Walk the calling process's parent chain. Return the slot whose PID
    appears in our ancestry, or None if no agent-runner ancestor exists."""
    try:
        me = psutil.Process(os.getpid())
    except psutil.Error:
        return None
    for ancestor in me.parents():
        if ancestor.pid in pid_to_slot:
            return pid_to_slot[ancestor.pid]
    return None


def _run_make_build() -> None:
    print("==> make build")
    subprocess.check_call(["make", "build"], cwd=str(_ROOT))


def _restart_slot(slot: str) -> None:
    subprocess.check_call([sys.executable, str(_RESTART_PY), slot], cwd=str(_ROOT))


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument(
        "--skip-build",
        action="store_true",
        help="skip 'make build' (use the binaries already in bin/)",
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="print the plan; do not build or restart anything",
    )
    parser.add_argument(
        "--between-seconds",
        type=float,
        default=2.0,
        help="seconds to sleep between non-self restarts (default: 2)",
    )
    args = parser.parse_args(argv)

    pid_to_slot = _alive_runner_slots()
    if not pid_to_slot:
        print("no alive agent-runner slots under bin/.run/")
        return 0

    self_slot = _find_self_slot(pid_to_slot)
    others = sorted(s for s in pid_to_slot.values() if s != self_slot)
    order = others + ([self_slot] if self_slot else [])

    print(f"==> alive slots:    {sorted(pid_to_slot.values())}")
    print(f"==> self slot:      {self_slot or '(none -- running outside a runner)'}")
    print(f"==> restart order:  {order}")

    if args.dry_run:
        print("(dry-run; exiting)")
        return 0

    if not args.skip_build:
        _run_make_build()

    for i, slot in enumerate(order):
        is_self = slot == self_slot
        print(f"==> restarting {slot}{' (self -- caller will likely die after this)' if is_self else ''}")
        _restart_slot(slot)
        # Brief pause between non-self restarts so the previous one has time
        # to deregister + re-register on the server. Skipped after self
        # because the script will be torn down.
        if i < len(order) - 1:
            time.sleep(args.between_seconds)

    print("==> done")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
