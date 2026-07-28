#!/usr/bin/env python3
"""Unit tests for scripts/cgroup_adopt.py.

Pure stdlib, imports cgroup_adopt directly (not daemon.py) so this runs
without scripts/.venv or psutil — see cgroup_adopt.py's module docstring.

The tests that need a real cgroup hierarchy skip themselves when the host
has no ``systemd --user`` with a registered harness runner unit, so this is
safe to run on Windows/macOS runners and in a bare container.

Run directly::

    python3 scripts/test_cgroup_adopt.py
"""

from __future__ import annotations

import os
import signal
import subprocess
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import cgroup_adopt  # noqa: E402
from cgroup_adopt import (  # noqa: E402
    adopt_into_unit_cgroup,
    current_cgroup,
    systemd_unit_name,
    unit_cgroup,
)


def _a_registered_slot() -> str | None:
    """A slot whose systemd user unit currently resolves to a cgroup, or None."""
    if not sys.platform.startswith("linux"):
        return None
    try:
        out = subprocess.run(
            ["systemctl", "--user", "list-unit-files", "harness-agent-runner*", "--no-legend"],
            capture_output=True, text=True, timeout=10,
        )
    except (OSError, subprocess.SubprocessError):
        return None
    for line in out.stdout.splitlines():
        unit = line.split()[0] if line.split() else ""
        if not unit.endswith(".service"):
            continue
        slot = unit[len(cgroup_adopt.UNIT_PREFIX):-len(".service")]
        if unit_cgroup(slot) is not None:
            return slot
    return None


class UnitNameTest(unittest.TestCase):
    def test_default_slot(self) -> None:
        self.assertEqual(systemd_unit_name("agent-runner"), "harness-agent-runner.service")

    def test_tagged_slot(self) -> None:
        self.assertEqual(
            systemd_unit_name("agent-runner-blog"), "harness-agent-runner-blog.service"
        )

    def test_hyphenated_tag_is_not_split(self) -> None:
        self.assertEqual(
            systemd_unit_name("agent-runner-mujo-roles"),
            "harness-agent-runner-mujo-roles.service",
        )


class UnitCgroupResolutionTest(unittest.TestCase):
    def test_unknown_unit_resolves_to_none(self) -> None:
        """A slot with no unit must resolve to None, not to a bogus path —
        this is what keeps the harness-server slot (which has no unit) on the
        no-op path instead of adopting into 'harness-harness-server.service'."""
        self.assertIsNone(unit_cgroup("definitely-not-a-registered-slot"))

    def test_server_slot_has_no_unit(self) -> None:
        self.assertIsNone(unit_cgroup("harness-server"))

    def test_non_linux_short_circuits(self) -> None:
        real = cgroup_adopt.sys.platform
        try:
            cgroup_adopt.sys.platform = "win32"
            self.assertIsNone(unit_cgroup("agent-runner"))
        finally:
            cgroup_adopt.sys.platform = real


class AdoptTest(unittest.TestCase):
    """Live tests: need a real registered+active harness runner unit."""

    def setUp(self) -> None:
        self.slot = _a_registered_slot()
        if self.slot is None:
            self.skipTest("no active harness-agent-runner systemd user unit on this host")
        self.procs: list[subprocess.Popen] = []

    def tearDown(self) -> None:
        for p in self.procs:
            for stream in (p.stdin, p.stdout):
                if stream is not None:
                    stream.close()
            p.kill()
            p.wait()

    def _sleeper(self) -> subprocess.Popen:
        p = subprocess.Popen(["sleep", "60"], start_new_session=True)
        self.procs.append(p)
        return p

    def test_adopts_into_own_unit(self) -> None:
        p = self._sleeper()
        adopt_into_unit_cgroup(p.pid, self.slot)
        self.assertEqual(current_cgroup(p.pid), systemd_unit_name(self.slot))
        self.assertIsNone(p.poll(), "adoption must not kill the process")

    def test_is_idempotent(self) -> None:
        p = self._sleeper()
        adopt_into_unit_cgroup(p.pid, self.slot)
        adopt_into_unit_cgroup(p.pid, self.slot)
        self.assertEqual(current_cgroup(p.pid), systemd_unit_name(self.slot))
        self.assertIsNone(p.poll())

    def test_unregistered_slot_is_a_no_op(self) -> None:
        p = self._sleeper()
        before = current_cgroup(p.pid)
        adopt_into_unit_cgroup(p.pid, "agent-runner-no-such-slot")
        self.assertEqual(current_cgroup(p.pid), before)
        self.assertIsNone(p.poll(), "the no-unit path must never kill the daemon")

    def test_adopted_child_leaves_the_callers_kill_domain(self) -> None:
        """The actual regression: a process spawned from cgroup A and adopted
        into unit B must no longer be in A."""
        p = self._sleeper()
        inherited = current_cgroup(p.pid)
        adopt_into_unit_cgroup(p.pid, self.slot)
        if inherited == systemd_unit_name(self.slot):
            self.skipTest("test process already runs in the target unit's cgroup")
        self.assertNotEqual(current_cgroup(p.pid), inherited)

    def test_descendants_inherit_the_corrected_cgroup(self) -> None:
        """Adopting only the daemon is sufficient because a fork *after*
        adoption inherits the corrected cgroup. That is the assumption
        adopt_into_unit_cgroup's docstring rests on, so pin it: adopt the
        parent first, make it fork only afterwards, and check the child
        without adopting the child itself."""
        p = subprocess.Popen(
            ["sh", "-c", "read x; sleep 60 & echo $!; wait"],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, text=True,
            start_new_session=True,
        )
        self.procs.append(p)
        adopt_into_unit_cgroup(p.pid, self.slot)  # adopt BEFORE the fork
        assert p.stdin and p.stdout
        p.stdin.write("go\n")
        p.stdin.flush()
        child_pid = int(p.stdout.readline().strip())
        self.addCleanup(self._kill_pid, child_pid)
        self.assertEqual(current_cgroup(child_pid), systemd_unit_name(self.slot))

    @staticmethod
    def _kill_pid(pid: int) -> None:
        try:
            os.kill(pid, signal.SIGKILL)
        except OSError:
            pass


if __name__ == "__main__":
    unittest.main(verbosity=2)
