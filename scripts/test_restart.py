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


if __name__ == "__main__":
    unittest.main()
