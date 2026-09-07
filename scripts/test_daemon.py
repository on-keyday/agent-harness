#!/usr/bin/env python3
"""Unit tests for scripts/daemon.py — the child-environment scrub only.

The spawn itself is exercised by standing a daemon up; what can silently go
wrong is WHICH variables reach the child. Two are load-bearing: a leaked
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

sys.path.insert(0, str(Path(__file__).resolve().parent))

import daemon  # noqa: E402


class CleanChildEnvTest(unittest.TestCase):
    def setUp(self) -> None:
        self._saved = dict(os.environ)
        os.environ["CLAUDE_CODE_CHILD_SESSION"] = "1"
        os.environ["CLAUDECODE"] = "1"
        os.environ["HARNESS_OPERATOR_PSK"] = "op"
        os.environ["HARNESS_OPERATOR_PSK_FILE"] = "/tmp/op"
        os.environ["HARNESS_PSK"] = "connect"
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
        self.assertEqual(runner_env.get("HARNESS_PSK"), "connect",
                         "the connect PSK is what agents DO need")


if __name__ == "__main__":
    unittest.main()
