#!/usr/bin/env python3
"""Unit tests for scripts/stop_build_start.py.

Pure stdlib, imports stop_build_start directly (not build_and_restart_all.py)
so this runs without scripts/.venv or psutil.

Run directly::

    python3 scripts/test_stop_build_start.py
"""

from __future__ import annotations

import sys
import tempfile
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from stop_build_start import Slot, read_plan, run, write_plan  # noqa: E402


def _slots(*names: str) -> list[Slot]:
    return [Slot(slot=n, bin_name="agent-runner", flags=["--max-tasks", "4"], cwd="/w") for n in names]


class Recorder:
    """Stands in for daemon_down / make build / daemon_up and records the order."""

    def __init__(self, *, build_ok: bool = True, fail_down: set[str] = frozenset(),
                 fail_up: set[str] = frozenset()) -> None:
        self.calls: list[str] = []
        self.lines: list[str] = []
        self._build_ok = build_ok
        self._fail_down = fail_down
        self._fail_up = fail_up

    def down(self, s: Slot) -> None:
        self.calls.append(f"down {s.slot}")
        if s.slot in self._fail_down:
            raise RuntimeError("owner mismatch")

    def build(self) -> bool:
        self.calls.append("build")
        return self._build_ok

    def up(self, s: Slot) -> None:
        self.calls.append(f"up {s.slot}")
        if s.slot in self._fail_up:
            raise RuntimeError("failed to start")

    def run(self, slots: list[Slot]):
        return run(slots, down=self.down, build=self.build, up=self.up, emit=self.lines.append)


class RunTest(unittest.TestCase):
    def test_every_slot_is_stopped_before_the_build_and_started_after(self) -> None:
        r = Recorder()
        out = r.run(_slots("a", "b", "self"))
        self.assertEqual(r.calls, ["down a", "down b", "down self", "build", "up a", "up b", "up self"])
        self.assertTrue(out.build_ok)
        self.assertEqual(out.started, ["a", "b", "self"])
        self.assertEqual(out.start_failed, [])

    def test_a_failed_build_still_brings_the_fleet_back(self) -> None:
        # The binaries in bin/ are whatever the failed build left (the old
        # ones, for a target it did not get to): a fleet left down is worse.
        r = Recorder(build_ok=False)
        out = r.run(_slots("a", "b"))
        self.assertEqual(r.calls, ["down a", "down b", "build", "up a", "up b"])
        self.assertFalse(out.build_ok)
        self.assertEqual(out.started, ["a", "b"])
        self.assertTrue(any("build FAILED" in line for line in r.lines), r.lines)

    def test_one_slot_failing_does_not_stop_the_others(self) -> None:
        r = Recorder(fail_down={"a"}, fail_up={"b"})
        out = r.run(_slots("a", "b", "c"))
        self.assertEqual(r.calls, ["down a", "down b", "down c", "build", "up a", "up b", "up c"])
        self.assertEqual(out.stop_failed, ["a"])
        self.assertEqual(out.started, ["a", "c"])
        self.assertEqual(out.start_failed, ["b"])
        self.assertTrue(any("b" in line and "failed to start" in line for line in r.lines), r.lines)


class PlanFileTest(unittest.TestCase):
    def test_round_trip_keeps_argv_exactly(self) -> None:
        # A runner's argv carries --agent-profiles JSON, quotes and braces
        # included; the plan file must hand it back byte for byte.
        slots = [Slot(slot="agent-runner-ghi", bin_name="agent-runner",
                      flags=["--agent-profiles", '[{"name": "codex", "argv": ["{args}"]}]', "--x=a b"],
                      cwd="C:\\work\\repo")]
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "plan.json"
            write_plan(p, slots)
            self.assertEqual(read_plan(p), slots)


if __name__ == "__main__":
    unittest.main()
