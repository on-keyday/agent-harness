#!/usr/bin/env python3
"""Tests for scripts/land.py against throwaway git repositories (no network)."""
from __future__ import annotations

import io
import os
import subprocess
import sys
import tempfile
import unittest
from contextlib import redirect_stderr, redirect_stdout
from pathlib import Path
from unittest import mock

sys.path.insert(0, str(Path(__file__).resolve().parent))

import land  # noqa: E402

GIT_ID = ["-c", "user.name=t", "-c", "user.email=t@example.invalid", "-c", "init.defaultBranch=main"]


def g(cwd: Path, *args: str) -> str:
    return subprocess.run(["git", *GIT_ID, "-C", str(cwd), *args], check=True,
                          capture_output=True, text=True).stdout.strip()


def commit(cwd: Path, rel: str, text: str) -> str:
    p = cwd / rel
    p.parent.mkdir(parents=True, exist_ok=True)
    p.write_text(text)
    g(cwd, "add", rel)
    g(cwd, "commit", "-q", "-m", f"change {rel}")
    return g(cwd, "rev-parse", "HEAD")


class LandTest(unittest.TestCase):
    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        t = Path(self._tmp.name)
        self.origin, self.main, self.wt = t / "origin.git", t / "main", t / "wt"
        subprocess.run(["git", *GIT_ID, "init", "-q", "--bare", str(self.origin)], check=True)
        subprocess.run(["git", *GIT_ID, "clone", "-q", str(self.origin), str(self.main)],
                       check=True, capture_output=True)
        g(self.main, "checkout", "-q", "-b", "main")
        commit(self.main, "README", "x\n")
        g(self.main, "push", "-q", "origin", "main")
        g(self.main, "worktree", "add", "-q", "-b", "harness/x", str(self.wt))
        self.head = commit(self.wt, "server/a.go", "package server\n")
        self.env = mock.patch.dict(os.environ, {"LAND_SKEW_CMD": "true",
                                                "LAND_BUILD_CMD": "sh -c 'touch built'"})
        self.env.start()

    def tearDown(self) -> None:
        self.env.stop()
        self._tmp.cleanup()

    def run_land(self) -> tuple[int, str, str]:
        out, err = io.StringIO(), io.StringIO()
        with redirect_stdout(out), redirect_stderr(err), mock.patch.object(land.Path, "cwd", return_value=self.wt):
            rc = land.cli([])
        return rc, out.getvalue(), err.getvalue()

    def origin_main(self) -> str:
        return g(self.origin, "rev-parse", "main")

    def test_lands_fast_forwards_the_main_checkout_and_builds_there(self) -> None:
        rc, out, err = self.run_land()
        self.assertEqual(rc, 0, err)
        self.assertEqual(self.origin_main(), self.head)
        self.assertEqual(g(self.main, "rev-parse", "HEAD"), self.head)
        self.assertTrue((self.main / "built").exists(), "make build did not run in the main checkout")
        self.assertIn("restart the server", out)

    def test_refuses_when_origin_moved_on(self) -> None:
        g(self.main, "checkout", "-q", "-b", "elsewhere")
        other = commit(self.main, "other", "y\n")
        g(self.main, "push", "-q", "origin", "elsewhere:main")
        g(self.main, "checkout", "-q", "main")
        rc, _, err = self.run_land()
        self.assertEqual(rc, 1)
        self.assertIn("rebase onto origin/main", err)
        self.assertEqual(self.origin_main(), other)

    def test_a_failed_skew_check_pushes_nothing(self) -> None:
        before = self.origin_main()
        with mock.patch.dict(os.environ, {"LAND_SKEW_CMD": "false"}):
            rc, _, err = self.run_land()
        self.assertEqual(rc, 1)
        self.assertIn("wire skew check failed", err)
        self.assertEqual(self.origin_main(), before)
        self.assertFalse((self.main / "built").exists())

    def test_refuses_when_the_main_checkout_is_not_on_main(self) -> None:
        g(self.main, "checkout", "-q", "-b", "side")
        before = self.origin_main()
        rc, _, err = self.run_land()
        self.assertEqual(rc, 1)
        self.assertIn("not main", err)
        self.assertEqual(self.origin_main(), before)


class RestartHintsTest(unittest.TestCase):
    def test_only_running_parts_need_a_restart(self) -> None:
        self.assertEqual(land.restart_hints(["cli/x.go", "docs/a.md"]), [])
        hints = land.restart_hints(["server/a.go", "runner/b.go", "webui/static/main.js"])
        self.assertEqual(len(hints), 3)


if __name__ == "__main__":
    unittest.main()
