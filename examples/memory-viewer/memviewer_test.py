"""Tests for the parts of memviewer.py that have no browser to show them.

    python3 -m unittest memviewer_test     # from this directory
"""

import unittest

import memviewer

# What `harness-cli ls --json` printed on stderr from a viewer started by a
# task that had since been resumed: two slog connection lines, then the cause.
STALE = (
    "2026/10/29 02:38:06 INFO new active connection added cid=ws:10.0.0.1:8549-15498\n"
    "2026/10/29 02:38:07 INFO active connection closed cid=ws:10.0.0.1:8549-15498\n"
    "psk auth: psk: server rejected: BadTicket\n"
)


class CliFailureTest(unittest.TestCase):
    # The connection log is noise in a reason: two INFO lines filled the old
    # 200-byte cut and left "server rejected: B" — the cause unreadable.
    def test_drops_the_connection_log(self):
        reason, _ = memviewer.cli_failure(
            "2026/10/29 02:38:06 INFO new active connection added cid=x\n"
            "ls: permission denied\n", "")
        self.assertEqual(reason, "ls: permission denied")

    def test_names_a_stale_ticket(self):
        reason, stale = memviewer.cli_failure(STALE, "")
        self.assertTrue(stale)
        self.assertIn("BadTicket", reason)
        self.assertIn("再起動", reason)

    def test_other_failures_are_not_stale(self):
        _, stale = memviewer.cli_failure("dial tcp: connection refused\n", "")
        self.assertFalse(stale)

    def test_falls_back_to_stdout_and_never_returns_empty(self):
        self.assertEqual(memviewer.cli_failure("", "boom\n")[0], "boom")
        self.assertNotEqual(memviewer.cli_failure("", "")[0], "")


if __name__ == "__main__":
    unittest.main()
