#!/usr/bin/env python3
"""Tests for scripts/runner-autostart.py's Linux registration (no systemd used)."""
from __future__ import annotations

import importlib.util
import io
import subprocess
import sys
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout
from pathlib import Path
from unittest import mock

_HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(_HERE))
_spec = importlib.util.spec_from_file_location("runner_autostart", _HERE / "runner-autostart.py")
ra = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(ra)


class RegisterLinuxTest(unittest.TestCase):
    """With the user manager unreachable ("Transport endpoint is not
    connected"), register used to write the unit, then hang in `enable --now`
    until it was killed, leaving a unit file that was never enabled and so did
    not start at the next boot either. Nothing said so."""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.unit_dir = Path(self._tmp.name)

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def _register(self, responder):
        calls = []

        def fake_run(cmd, **kw):
            calls.append(cmd)
            return responder(cmd, kw)
        err = io.StringIO()
        with mock.patch.object(ra, "_systemd_unit_dir", return_value=self.unit_dir), \
             mock.patch.object(ra.subprocess, "run", side_effect=fake_run), \
             redirect_stdout(io.StringIO()), redirect_stderr(err):
            rc = ra._register_linux("x", ["--roots", "/w"], True)
        return rc, calls, err.getvalue()

    def unit_file(self) -> Path:
        return self.unit_dir / ra._systemd_unit_name("x")

    def test_unreachable_user_manager_writes_nothing(self) -> None:
        def resp(cmd, kw):
            if "show" in cmd:
                raise subprocess.TimeoutExpired(cmd, kw.get("timeout"))
            return mock.Mock(returncode=0, stdout="", stderr="")
        rc, calls, err = self._register(resp)
        self.assertNotEqual(rc, 0)
        self.assertFalse(self.unit_file().exists(), "a unit was written although it could not be enabled")
        self.assertIn("user manager", err)
        self.assertFalse(any("enable" in c for c in calls))

    def test_every_systemctl_call_has_a_timeout(self) -> None:
        timeouts = []

        def resp(cmd, kw):
            timeouts.append(kw.get("timeout"))
            out = "enabled\n" if "is-enabled" in cmd else "255\n"
            return mock.Mock(returncode=0, stdout=out, stderr="")
        rc, _, err = self._register(resp)
        self.assertEqual(rc, 0, err)
        self.assertTrue(timeouts and all(t for t in timeouts), timeouts)

    def test_a_unit_left_disabled_is_reported(self) -> None:
        def resp(cmd, kw):
            out = "disabled\n" if "is-enabled" in cmd else "255\n"
            return mock.Mock(returncode=0, stdout=out, stderr="")
        rc, _, err = self._register(resp)
        self.assertNotEqual(rc, 0)
        self.assertIn("disabled", err)


class PathDriftTest(unittest.TestCase):
    """A registered unit carries the PATH of the shell that registered it, as a
    literal Environment=PATH= line; restarting the unit restores it verbatim.
    All 12 units once had a stale opencode install ahead of /usr/bin, so every
    runner-spawned opencode stayed the old build after the new one was
    installed. path_drift names what a unit's PATH resolves differently."""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        t = Path(self._tmp.name)
        self.old, self.new = t / "old", t / "new"
        for d in (self.old, self.new):
            d.mkdir()
            exe = d / "tool"
            exe.write_text("#!/bin/sh\n")
            exe.chmod(0o755)

    def tearDown(self) -> None:
        self._tmp.cleanup()

    def test_same_resolution_is_clean(self) -> None:
        self.assertEqual(ra.path_drift(f"{self.new}", f"{self.new}", ["tool"]), [])

    def test_a_shadowing_entry_is_named_with_both_paths(self) -> None:
        problems = ra.path_drift(f"{self.old}:{self.new}", f"{self.new}", ["tool"])
        self.assertEqual(len(problems), 1)
        self.assertIn(str(self.old / "tool"), problems[0])
        self.assertIn(str(self.new / "tool"), problems[0])

    def test_a_missing_directory_alone_is_not_a_problem(self) -> None:
        # lookup skips it, so it changes nothing; reporting it buried the real
        # findings under dead plugin-cache entries on the first live run
        gone = Path(self._tmp.name) / "gone"
        self.assertEqual(ra.path_drift(f"{gone}:{self.new}", f"{self.new}", ["tool"]), [])

    def test_a_tool_found_only_by_the_shell_is_reported(self) -> None:
        problems = ra.path_drift("/nonexistent-dir-xyz", f"{self.new}", ["tool"])
        self.assertTrue(any("not found" in p for p in problems), problems)


if __name__ == "__main__":
    unittest.main()
