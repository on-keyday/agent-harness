"""The sequence behind ``build_and_restart_all.py --stop-first``.

Stop every slot, build, then start every slot again. This is for hosts where a
running binary cannot be overwritten (Windows: ``make build`` fails on
``bin/agent-runner.exe`` while a runner is running from it).

The sequence runs in one detached process, not in the caller. The caller is
normally an agent inside one of the runners being stopped, and that agent dies
when its runner does. Starting the slots again has to happen after that point.

Pure stdlib with the side effects passed in, so the order of operations is
testable without psutil or a fleet (``test_stop_build_start.py``).
"""

from __future__ import annotations

import json
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Callable


@dataclass(frozen=True)
class Slot:
    """One runner slot as it was running before the stop: everything
    ``daemon_up`` needs to start it again identically."""

    slot: str
    bin_name: str
    flags: list[str]
    cwd: str


@dataclass
class Outcome:
    build_ok: bool = False
    stop_failed: list[str] = field(default_factory=list)
    started: list[str] = field(default_factory=list)
    start_failed: list[str] = field(default_factory=list)


def write_plan(path: Path, slots: list[Slot]) -> None:
    """Hand the slots to the detached process through a file, not argv.
    A runner's argv carries the whole ``--agent-profiles`` JSON (several KB),
    and a fleet's worth of them overflows Windows' 32K command line."""
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps([asdict(s) for s in slots], indent=1), encoding="utf-8")


def read_plan(path: Path) -> list[Slot]:
    rows = json.loads(path.read_text(encoding="utf-8"))
    return [Slot(slot=r["slot"], bin_name=r["bin_name"], flags=list(r["flags"]), cwd=r["cwd"]) for r in rows]


def run(
    slots: list[Slot],
    *,
    down: Callable[[Slot], None],
    build: Callable[[], bool],
    up: Callable[[Slot], None],
    emit: Callable[[str], None],
) -> Outcome:
    """Stop all, build, start all. Neither a slot that fails to stop nor a
    build that fails skips the start step: the slots come back on whatever
    binaries are in bin/, because a fleet left down is the worse outcome."""
    out = Outcome()
    for s in slots:
        try:
            down(s)
        except Exception as e:  # noqa: BLE001 -- one slot must not strand the rest
            out.stop_failed.append(s.slot)
            emit(f"[{s.slot}] stop failed: {e}")

    out.build_ok = build()
    if not out.build_ok:
        emit("build FAILED; starting every slot again on the binaries already in bin/")

    for s in slots:
        try:
            up(s)
            out.started.append(s.slot)
        except Exception as e:  # noqa: BLE001
            out.start_failed.append(s.slot)
            emit(f"[{s.slot}] failed to start: {e}")
    return out
