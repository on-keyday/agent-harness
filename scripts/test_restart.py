#!/usr/bin/env python3
"""Unit tests for scripts/restart.py — where a restart's flags come from, and
how overrides replace them. The detached down/up itself is exercised by
restarting a daemon.

Run directly (psutil needed — scripts/.venv has it)::

    scripts/.venv/bin/python scripts/test_restart.py
"""
from __future__ import annotations

import io
import os
import stat
import sys
import tempfile
import unittest
from unittest import mock
from contextlib import redirect_stderr, redirect_stdout
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import daemon  # noqa: E402
import restart  # noqa: E402


class _RunDir(unittest.TestCase):
    """Points daemon.RUN_DIR at a fresh temp dir for each test."""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self._saved = daemon.RUN_DIR
        daemon.RUN_DIR = Path(self._tmp.name)

    def tearDown(self) -> None:
        daemon.RUN_DIR = self._saved
        self._tmp.cleanup()

    def resolve(self, slot: str):
        out, err = io.StringIO(), io.StringIO()
        with redirect_stdout(out), redirect_stderr(err):
            state = restart._resolve_state(slot)
        return state, out.getvalue(), err.getvalue()


class RecordArgvTest(_RunDir):
    def test_round_trip_and_mode(self) -> None:
        daemon.record_argv("harness-server", "harness-server", ["--listen", ":8549"])
        bin_name, cwd, args = daemon.recorded_argv("harness-server")
        self.assertEqual((bin_name, args), ("harness-server", ["--listen", ":8549"]))
        self.assertEqual(cwd, str(daemon._ROOT), "every daemon starts in _ROOT")
        if os.name != "nt":
            mode = stat.S_IMODE(daemon.argv_file("harness-server").stat().st_mode)
            self.assertEqual(mode, 0o600, "the args can carry --operator-psk VALUE")

    def test_absent(self) -> None:
        self.assertIsNone(daemon.recorded_argv("nope"))


class ResolveStateTest(_RunDir):
    def test_nothing_running_nothing_recorded(self) -> None:
        state, _, err = self.resolve("harness-server")
        self.assertIsNone(state)
        self.assertIn("no recorded start", err)

    def test_dead_pid_falls_back_to_record(self) -> None:
        daemon.record_argv("harness-server", "harness-server", ["--listen", ":8549"])
        # A pid that cannot be alive: past the default pid_max on Linux.
        daemon.pid_file("harness-server").write_text("4194305")
        state, out, _ = self.resolve("harness-server")
        self.assertEqual(state, ("harness-server", ["--listen", ":8549"], str(daemon._ROOT)))
        self.assertIn("recorded at its last start", out)

    def test_no_pid_file_falls_back_to_record(self) -> None:
        daemon.record_argv("agent-runner-2", "agent-runner", ["--max-tasks", "4"])
        state, _, _ = self.resolve("agent-runner-2")
        self.assertEqual(state[:2], ("agent-runner", ["--max-tasks", "4"]))

    def test_reused_pid_is_not_the_daemon(self) -> None:
        # The pid file names a live process that is NOT the recorded binary.
        daemon.record_argv("harness-server", "harness-server", ["--listen", ":8549"])
        daemon.pid_file("harness-server").write_text(str(os.getpid()))
        state, _, _ = self.resolve("harness-server")
        self.assertEqual(state[1], ["--listen", ":8549"], "must not inherit this process's argv")

    def test_live_process_wins_over_record(self) -> None:
        # No record: a live pid is read as before.
        daemon.pid_file("x").write_text(str(os.getpid()))
        state, _, _ = self.resolve("x")
        self.assertIsNotNone(state)
        self.assertEqual(state[2], os.getcwd())


class MergeFlagsTest(unittest.TestCase):
    def test_override_replaces(self) -> None:
        self.assertEqual(
            restart._merge_flags(["--max-tasks", "1", "--roots", "/a"], ["--max-tasks", "4"]),
            ["--roots", "/a", "--max-tasks", "4"],
        )

    def test_psk_file_override_drops_the_inherited_value(self) -> None:
        merged = restart._merge_flags(
            ["--listen", ":8549", "--operator-psk", "OLD"], ["--operator-psk-file", "/p"]
        )
        self.assertEqual(merged, ["--listen", ":8549", "--operator-psk-file", "/p"])

    def test_psk_value_override_drops_the_inherited_file(self) -> None:
        merged = restart._merge_flags(["--operator-psk-file=/p"], ["--operator-psk", "NEW"])
        self.assertEqual(merged, ["--operator-psk", "NEW"])

    def test_empty_value_override_strips_it(self) -> None:
        merged = restart._merge_flags(["--operator-psk", "OLD", "--x"], ["--operator-psk="])
        self.assertEqual(merged, ["--x", "--operator-psk="])

    def test_unset_strips_the_group_and_adds_nothing(self) -> None:
        overrides, unset = restart._split_unset(["--unset", "operator-psk", "--max-tasks", "4"])
        self.assertEqual((overrides, unset), (["--max-tasks", "4"], ["operator-psk"]))
        merged = restart._merge_flags(
            ["--listen", ":8549", "--operator-psk", "OLD", "--operator-psk-file=/p"], overrides, unset
        )
        self.assertEqual(merged, ["--listen", ":8549", "--max-tasks", "4"])

    def test_unset_equals_form(self) -> None:
        self.assertEqual(restart._split_unset(["--unset=--roots"]), ([], ["roots"]))

    def test_unset_without_a_name_is_refused(self) -> None:
        for bad in (["--unset"], ["--unset", "--x"], ["--unset="]):
            with self.assertRaises(ValueError, msg=bad):
                restart._split_unset(bad)


class RegisteredUnitTest(unittest.TestCase):
    """A slot registered as a systemd user unit is restarted THROUGH the unit.

    Replaying the running argv (the path for unregistered slots) re-launches
    the flags the slot was started with, so a change to scripts/agent_presets.py
    never reached it: the unit's ExecStart re-expands --agents at start time.
    That froze a preset change fleet-wide twice (2026-07-28, 2026-11-14).
    """

    UNIT = "harness-agent-runner-x.service"

    def _main(self, argv, *, own: bool = False):
        calls = []

        def fake_run(cmd, **kw):
            calls.append(cmd)
            return mock.Mock(returncode=0, stdout="", stderr="")
        out, err = io.StringIO(), io.StringIO()
        with mock.patch.object(restart, "_registered_unit", return_value=self.UNIT), \
             mock.patch.object(restart, "_runs_inside_unit", return_value=own), \
             mock.patch.object(restart.subprocess, "run", side_effect=fake_run), \
             mock.patch.object(restart, "_spawn_detached_child") as replay, \
             redirect_stdout(out), redirect_stderr(err):
            rc = restart.main(argv)
        return rc, calls, replay, out.getvalue() + err.getvalue()

    def test_another_slot_is_restarted_by_systemctl(self) -> None:
        rc, calls, replay, _ = self._main(["agent-runner-x"])
        self.assertEqual(rc, 0)
        self.assertEqual(calls, [["systemctl", "--user", "restart", self.UNIT]])
        replay.assert_not_called()

    def test_the_callers_own_slot_is_restarted_by_a_timer(self) -> None:
        # A direct restart from inside the unit kills the caller mid-command;
        # a transient timer belongs to the user manager and survives it.
        rc, calls, replay, out = self._main(["agent-runner-x"], own=True)
        self.assertEqual(rc, 0)
        self.assertEqual(len(calls), 1)
        self.assertEqual(calls[0][:3], ["systemd-run", "--user", f"--on-active={restart._SELF_RESTART_DELAY_S}"])
        self.assertEqual(calls[0][-3:], ["--user", "restart", self.UNIT])
        replay.assert_not_called()
        self.assertIn(str(restart._SELF_RESTART_DELAY_S), out)

    def test_overrides_are_refused_for_a_registered_slot(self) -> None:
        rc, calls, replay, out = self._main(["agent-runner-x", "--max-tasks", "8"])
        self.assertEqual(rc, 2)
        self.assertEqual(calls, [])
        replay.assert_not_called()
        self.assertIn("runner-autostart.py register", out)

    def test_an_unregistered_slot_still_replays_its_argv(self) -> None:
        with mock.patch.object(restart, "_registered_unit", return_value=None), \
             mock.patch.object(restart, "_resolve_state", return_value=("agent-runner", ["--max-tasks", "4"], "/w")), \
             mock.patch.object(restart, "_spawn_detached_child", return_value=99) as replay, \
             redirect_stdout(io.StringIO()):
            rc = restart.main(["agent-runner-y"])
        self.assertEqual(rc, 0)
        replay.assert_called_once()


class RegisteredUnitLookupTest(unittest.TestCase):
    def _show(self, state: str):
        return mock.patch.object(restart.subprocess, "run",
                                 return_value=mock.Mock(returncode=0, stdout=state + "\n"))

    @unittest.skipUnless(sys.platform.startswith("linux"), "systemd units are Linux-only")
    def test_loaded_unit_is_registered_and_not_found_is_not(self) -> None:
        with self._show("loaded"):
            self.assertEqual(restart._registered_unit("agent-runner-x"), "harness-agent-runner-x.service")
        with self._show("not-found"):
            self.assertIsNone(restart._registered_unit("agent-runner-x"))


if __name__ == "__main__":
    unittest.main()
