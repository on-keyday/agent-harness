"""Offline regressions for the sandbox history window (--history-window)."""

import io
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest import mock

import history_window as hw

DAY = "2026/10/25"


def rollout(root, rel, cwd):
    path = root / rel
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps({"type": "session_meta", "payload": {"cwd": str(cwd), "id": "t"}}) + "\n"
                    + json.dumps({"type": "x"}) + "\n")
    return path


class HelperTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        self.host, self.task = self.root / "host-sessions", self.root / "task"
        self.work, self.other = self.root / "work", self.root / "other"
        for path in (self.host, self.task, self.work, self.other):
            path.mkdir()
        self.log = io.StringIO()

    def test_rollout_cwd_reads_first_line(self):
        self.assertEqual(hw.rollout_cwd(rollout(self.host, DAY + "/rollout-a.jsonl", self.work)), str(self.work))

    def test_rollout_cwd_incomplete_first_line_is_none(self):
        path = self.host / "rollout-partial.jsonl"
        path.write_text('{"type":"session_meta"')
        self.assertIsNone(hw.rollout_cwd(path))

    def test_same_dir_through_symlink(self):
        link = self.root / "link"
        link.symlink_to(self.work)
        self.assertTrue(hw.same_dir(str(link), str(self.work)))
        self.assertFalse(hw.same_dir(str(self.other), str(self.work)))

    def test_import_links_only_this_cwd(self):
        a = rollout(self.host, DAY + "/rollout-a.jsonl", self.work)
        rollout(self.host, DAY + "/rollout-b.jsonl", self.other)
        hw.codex_import(self.task, self.host, str(self.work), self.log)
        self.assertEqual((self.task / DAY / "rollout-a.jsonl").stat().st_ino, a.stat().st_ino)
        self.assertFalse((self.task / DAY / "rollout-b.jsonl").exists())

    def test_import_clears_previous_links(self):
        rel = "2026/10/24/rollout-old.jsonl"
        host_old = rollout(self.host, rel, self.other)
        (self.task / rel).parent.mkdir(parents=True)
        os.link(host_old, self.task / rel)
        hw.codex_import(self.task, self.host, str(self.work), self.log)
        self.assertFalse((self.task / rel).exists())
        self.assertTrue(host_old.exists())

    def test_import_links_unexported_rollout_before_clearing(self):
        rel = "2026/10/26/rollout-new.jsonl"
        new = rollout(self.task, rel, self.work)
        ino = new.stat().st_ino
        hw.codex_import(self.task, self.host, str(self.work), self.log)
        self.assertEqual((self.host / rel).stat().st_ino, ino)

    def test_export_never_overwrites_host(self):
        rel = DAY + "/rollout-a.jsonl"
        host_copy = rollout(self.host, rel, self.work)
        before = host_copy.read_bytes()
        rollout(self.task, rel, self.work).write_text("container rewrite\n")
        hw.codex_export(self.task, self.host, str(self.work), self.log)
        self.assertEqual(host_copy.read_bytes(), before)
        self.assertIn("diverged", self.log.getvalue())

    def test_export_refuses_other_cwd(self):
        rel = DAY + "/rollout-x.jsonl"
        rollout(self.task, rel, self.other)
        hw.codex_export(self.task, self.host, str(self.work), self.log)
        self.assertFalse((self.host / rel).exists())
        self.assertIn("refused", self.log.getvalue())

    def test_export_skips_incomplete_silently(self):
        rel = DAY + "/rollout-p.jsonl"
        (self.task / rel).parent.mkdir(parents=True)
        (self.task / rel).write_text('{"type":"session_meta","payload":{"cwd"')
        hw.codex_export(self.task, self.host, str(self.work), self.log)
        self.assertFalse((self.host / rel).exists())
        self.assertEqual(self.log.getvalue(), "")

    def test_cross_device_raises(self):
        with mock.patch.object(hw, "_device", side_effect=[1, 2]):
            with self.assertRaises(hw.CrossDevice):
                hw.codex_export(self.task, self.host, str(self.work), self.log)

    def test_cli_cross_device_exits_2(self):
        argv = ["codex-import", "--task-dir", str(self.task), "--host-sessions", str(self.host), "--cwd", str(self.work)]
        with mock.patch.object(hw, "_device", side_effect=[1, 2]), mock.patch("sys.stderr", io.StringIO()) as err:
            self.assertEqual(hw.main(argv), 2)
        self.assertIn("different filesystems", err.getvalue())


if __name__ == "__main__":
    unittest.main()
