#!/usr/bin/env python3
"""Unit tests for scripts/daemon.py — the child-environment scrub and the
detached-spawn flags.

The spawn itself is exercised by standing a daemon up; what can silently go
wrong is WHICH variables reach the child, and on Windows whether the child
leaves the caller's job. Two are load-bearing: a leaked
CLAUDE_CODE_* marker makes a spawned claude write no transcript, and the
operator secret in a runner reaches every agent it spawns.

Run directly (psutil needed — scripts/.venv has it)::

    scripts/.venv/bin/python scripts/test_daemon.py
"""
from __future__ import annotations

import os
import sys
import unittest
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))

import daemon  # noqa: E402


class CleanChildEnvTest(unittest.TestCase):
    def setUp(self) -> None:
        self._saved = dict(os.environ)
        os.environ["CLAUDE_CODE_CHILD_SESSION"] = "1"
        os.environ["CLAUDECODE"] = "1"
        os.environ["HARNESS_OPERATOR_PSK"] = "op"
        os.environ["HARNESS_OPERATOR_PSK_FILE"] = "/tmp/op"
        os.environ["HARNESS_SERVER_CID"] = "ws:127.0.0.1:8539-*"
        os.environ["CLAUDE_CONFIG_DIR"] = "/tmp/cfg"

    def tearDown(self) -> None:
        os.environ.clear()
        os.environ.update(self._saved)

    def test_claude_markers_always_dropped_config_kept(self) -> None:
        env = daemon._clean_child_env()
        for gone in ("CLAUDE_CODE_CHILD_SESSION", "CLAUDECODE"):
            self.assertNotIn(gone, env)
        self.assertEqual(env.get("CLAUDE_CONFIG_DIR"), "/tmp/cfg")

    def test_operator_secret_reaches_a_server_but_never_a_runner(self) -> None:
        server_env = daemon._clean_child_env(daemon._DROP_FOR_BIN.get("harness-server", frozenset()))
        self.assertEqual(server_env.get("HARNESS_OPERATOR_PSK"), "op",
                         "harness-server takes the operator secret from the environment")
        runner_env = daemon._clean_child_env(daemon._DROP_FOR_BIN["agent-runner"])
        for gone in ("HARNESS_OPERATOR_PSK", "HARNESS_OPERATOR_PSK_FILE"):
            self.assertNotIn(gone, runner_env, f"{gone} would reach every agent the runner spawns")
        self.assertEqual(runner_env.get("HARNESS_SERVER_CID"), "ws:127.0.0.1:8539-*",
                         "what agents DO need stays")


class PopenDetachedTest(unittest.TestCase):
    """Every detached spawn (a runner from daemon_up, restart.py's down/up
    child, build_and_restart_all's sequence) goes through popen_detached, so a
    Windows fix lands on all of them. The one that mattered: a restart run
    through the runner's non-PTY exec sits in the exec's KILL_ON_JOB_CLOSE job
    and died with it unless it asked to break away (restart.py did not)."""

    def _record(self, refuse_breakaway: bool = False):
        calls = []

        def fake_popen(args, **kw):
            calls.append(kw)
            flags = kw.get("creationflags", 0)
            if refuse_breakaway and flags & daemon._CREATE_BREAKAWAY_FROM_JOB:
                raise OSError("job does not allow breakaway")
            return mock.Mock(pid=4242)
        return calls, mock.patch.object(daemon.subprocess, "Popen", side_effect=fake_popen)

    def test_windows_breaks_away_from_the_callers_job(self) -> None:
        calls, patch = self._record()
        with patch:
            daemon.popen_detached(["x"], windows=True)
        flags = calls[0]["creationflags"]
        self.assertTrue(flags & daemon._CREATE_BREAKAWAY_FROM_JOB)
        self.assertTrue(flags & daemon._DETACHED_PROCESS)
        self.assertTrue(flags & daemon._CREATE_NEW_PROCESS_GROUP)

    def test_windows_retries_without_breakaway_when_the_job_refuses(self) -> None:
        calls, patch = self._record(refuse_breakaway=True)
        with patch:
            p = daemon.popen_detached(["x"], windows=True)
        self.assertEqual(p.pid, 4242)
        self.assertEqual(len(calls), 2)
        self.assertFalse(calls[1]["creationflags"] & daemon._CREATE_BREAKAWAY_FROM_JOB)

    def test_posix_starts_a_new_session(self) -> None:
        calls, patch = self._record()
        with patch:
            daemon.popen_detached(["x"], windows=False)
        self.assertTrue(calls[0]["start_new_session"])

    def test_no_script_spawns_detached_by_hand(self) -> None:
        here = Path(__file__).resolve().parent
        for name in ("daemon.py", "restart.py", "build_and_restart_all.py"):
            src = (here / name).read_text()
            if name == "daemon.py":
                start = src.index("def popen_detached(")
                end = src.index("\ndef ", start + 1)
                src = src[:start] + src[end:]
            for token in ("subprocess.DETACHED_PROCESS", "subprocess.CREATE_NEW_PROCESS_GROUP",
                          "start_new_session"):
                code = [l for l in src.splitlines() if token in l and not l.lstrip().startswith("#")]
                self.assertEqual(code, [], f"{name} spawns detached by hand; use daemon.popen_detached")


if __name__ == "__main__":
    unittest.main()
