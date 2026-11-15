#!/usr/bin/env python3
"""Tests for scripts/check_local_identifiers.py.

The shapes under test are built by concatenation so this file itself never
holds one: it is in the repository the checker guards.
"""
from __future__ import annotations

import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import check_local_identifiers as cli  # noqa: E402

LAN = "192." + "168.7.21"
OTHER_PRIVATE = "172." + "20.1.9"
HOME = "/home/" + "alice/work"


class FindingsTest(unittest.TestCase):
    def find(self, line, local=()):
        return cli.findings(line, set(local))

    def test_private_lan_addresses_are_flagged(self):
        self.assertTrue(self.find(f"connect {LAN}:8540"))
        self.assertTrue(self.find(f"via {OTHER_PRIVATE}"))

    def test_the_repos_lab_and_documentation_ranges_pass(self):
        # 10/8 is this repo's netem-lab / test network; 192.0.2.0/24 is the
        # RFC 5737 documentation range the scrub replaced real addresses with.
        self.assertEqual(self.find("ws:10.200.0.1:1-1 and 192.0.2.14"), [])

    def test_a_named_home_directory_is_flagged_a_placeholder_is_not(self):
        self.assertTrue(self.find(f"cd {HOME}"))
        self.assertEqual(self.find("cd /home/user/work /Users/foo /home/someone"), [])

    def test_this_machines_own_identifiers_are_flagged(self):
        self.assertTrue(self.find("runner on boxname-codex", local={"boxname"}))
        self.assertEqual(self.find("runner on otherbox", local={"boxname"}), [])

    def test_an_explicit_marker_lets_a_line_through(self):
        self.assertEqual(self.find(f"connect {LAN}  # local-identifier-ok: fixture"), [])


class LocalIdentityTest(unittest.TestCase):
    def test_generic_names_are_not_treated_as_identifying(self):
        ids = cli.local_identity(user="user", host="localhost", addrs=["127.0.0.1"], server="")
        self.assertEqual(ids, set())

    def test_user_host_and_non_loopback_addresses_are(self):
        ids = cli.local_identity(user="alice", host="box1.lan", addrs=["127.0.0.1", "203.0.113.7"],
                                 server="ws:203.0.113.9:8539-*")
        self.assertEqual(ids, {"alice", "box1", "203.0.113.7", "203.0.113.9"})


class AddedLinesTest(unittest.TestCase):
    def test_only_added_lines_with_their_file_and_line(self):
        diff = ("diff --git a/x.md b/x.md\n+++ b/x.md\n@@ -1,0 +5,2 @@\n+kept one\n+second\n"
                "@@ -9 +11 @@\n-old\n+changed\n")
        self.assertEqual(list(cli.added_lines(diff)),
                         [("x.md", 5, "kept one"), ("x.md", 6, "second"), ("x.md", 11, "changed")])


class CommitMessageTest(unittest.TestCase):
    """Commit messages are published with the tree: the scrub's own commit
    message once named the subnet it had just removed."""

    def test_a_message_carrying_a_lan_address_fails(self):
        import os, subprocess, tempfile, io
        from contextlib import redirect_stdout, redirect_stderr
        from unittest import mock
        g = ["git", "-c", "user.name=t", "-c", "user.email=t@example.invalid"]
        with tempfile.TemporaryDirectory() as d:
            subprocess.run(g + ["init", "-q", d], check=True)
            for msg in ("base", f"moved off {LAN} for good"):
                open(os.path.join(d, "f"), "a").write("x\n")
                subprocess.run(g + ["-C", d, "add", "f"], check=True)
                subprocess.run(g + ["-C", d, "commit", "-q", "-m", msg], check=True)
            cwd = os.getcwd()
            os.chdir(d)
            try:
                out = io.StringIO()
                with redirect_stdout(out), redirect_stderr(io.StringIO()), \
                        mock.patch.object(cli, "local_identity", return_value=set()):
                    rc = cli.main(["HEAD~1..HEAD"])
            finally:
                os.chdir(cwd)
        self.assertEqual(rc, 1)
        self.assertIn("message", out.getvalue())


if __name__ == "__main__":
    unittest.main()
