#!/usr/bin/env python3
"""Unit tests for scripts/dummy-harness.py.

Only the pure parts — the ones that decide WHERE state lives and WHAT gets
scrubbed. Standing an instance up is a manual check by construction (that is
what the script is for), but these two are exactly the pieces whose failures
are silent, and one of them already cost a leaked server and runner.

Run directly::

    python3 scripts/test_dummy_harness.py
"""

from __future__ import annotations

import importlib.util
import os
import sys
import tempfile
import unittest
from pathlib import Path

_HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(_HERE))

# The module's filename has a hyphen, so it cannot be imported by name.
_spec = importlib.util.spec_from_file_location("dummy_harness", _HERE / "dummy-harness.py")
assert _spec and _spec.loader
dummy_harness = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(dummy_harness)


class TmpRootTest(unittest.TestCase):
    """The state directory must not be resolvable from a variable this script
    EXPORTS, or its own output poisons its own bookkeeping.

    The bug: `env` prints `export TMP=<the instance's dir>`. Anything using
    tempfile.gettempdir() then resolves the state directory INSIDE the instance
    after one `eval "$(dummy-harness.py env)"`, so `down` in that same shell
    finds no state file, says "nothing to stop", and leaves a server and a
    runner running with their state orphaned. Observed, not hypothetical.
    """

    def setUp(self) -> None:
        self._saved = {k: os.environ.get(k) for k in ("TMP", "TEMP", "TMPDIR")}

    def tearDown(self) -> None:
        for k, v in self._saved.items():
            if v is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = v

    @unittest.skipIf(os.name == "nt", "POSIX resolution order")
    def test_tmp_is_ignored_on_posix(self) -> None:
        # The directory must EXIST for this to reproduce anything.
        # tempfile.gettempdir() only adopts a candidate it can actually write
        # in, so pointing TMP at a missing path falls back to /tmp and the old
        # implementation would have passed this test while still being broken.
        # The real failure had TMP pointing at a live instance directory.
        with tempfile.TemporaryDirectory(prefix="harness-dummy.") as inst:
            os.environ.pop("TMPDIR", None)
            os.environ["TMP"] = inst
            self.assertNotEqual(
                dummy_harness.tmp_root(), Path(inst),
                "tmp_root() followed TMP, which this script exports as the instance dir",
            )
            self.assertNotIn(inst, str(dummy_harness.state_dir()))

    @unittest.skipIf(os.name == "nt", "POSIX resolution order")
    def test_tmpdir_is_honoured_on_posix(self) -> None:
        os.environ["TMPDIR"] = "/var/tmp"
        self.assertEqual(dummy_harness.tmp_root(), Path("/var/tmp"))

    def test_state_path_is_per_name(self) -> None:
        a = dummy_harness.state_path("host")
        b = dummy_harness.state_path("target")
        self.assertNotEqual(a, b)
        self.assertTrue(str(a).endswith("host.json"))


class ScrubTest(unittest.TestCase):
    """Both scrubs are load-bearing and both failures are silent: a stale
    HARNESS_AUTH_TICKET fails against the dummy server as `BadTicket`, which
    reads like a PSK mistake, and a leaked CLAUDE_CODE_* makes a spawned claude
    treat itself as a child session and write no transcript at all."""

    def setUp(self) -> None:
        self._saved = dict(os.environ)

    def tearDown(self) -> None:
        os.environ.clear()
        os.environ.update(self._saved)

    def test_scrub_removes_harness_and_claude_markers(self) -> None:
        os.environ["HARNESS_AUTH_TICKET"] = "stale"
        os.environ["HARNESS_TASK_ID"] = "abc"
        os.environ["CLAUDE_CODE_CHILD_SESSION"] = "1"
        os.environ["CLAUDECODE"] = "1"
        os.environ["AI_AGENT"] = "1"
        os.environ["KEEP_ME"] = "yes"
        os.environ["CLAUDE_CONFIG_DIR"] = "/cfg"  # real config, not a marker

        dummy_harness.scrub_own_env()

        for gone in ("HARNESS_AUTH_TICKET", "HARNESS_TASK_ID",
                     "CLAUDE_CODE_CHILD_SESSION", "CLAUDECODE", "AI_AGENT"):
            self.assertNotIn(gone, os.environ, f"{gone} survived the scrub")
        self.assertEqual(os.environ.get("KEEP_ME"), "yes")
        self.assertEqual(os.environ.get("CLAUDE_CONFIG_DIR"), "/cfg",
                         "CLAUDE_CONFIG_DIR is configuration, not a session marker")


if __name__ == "__main__":
    unittest.main()
