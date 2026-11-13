"""Tests for the parts of memviewer.py that have no browser to show them.

    python3 -m unittest memviewer_test     # from this directory
"""

import unittest
from pathlib import Path
import tempfile
from unittest import mock

import memviewer

# What `harness-cli ls --json` printed on stderr from a viewer started by a
# task that had since been resumed: two slog connection lines, then the cause.
STALE = (
    "2026/10/29 02:38:06 INFO new active connection added cid=ws:10.0.0.1:8549-15498\n"
    "2026/10/29 02:38:07 INFO active connection closed cid=ws:10.0.0.1:8549-15498\n"
    "psk auth: psk: server rejected: BadTicket\n"
)


class NavigationTest(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.root = Path(temp.name)
        patch = mock.patch.object(memviewer, "MEMORY_DIRS", [self.root])
        patch.start()
        self.addCleanup(patch.stop)

    def write(self, name, text):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text, encoding="utf-8")

    def checks(self):
        return memviewer.analyze(memviewer.scan()[0])

    def test_isolated_archive_cycle_is_unreachable(self):
        self.write("MEMORY.md", "# Memory\n")
        self.write("old/a.md", "[[b]]")
        self.write("old/b.md", "[[a]]")
        self.assertEqual(self.checks()["unreachable"], ["a", "b"])

    def test_index_entry_makes_a_cycle_reachable(self):
        self.write("MEMORY.md", "- [a](old/a.md) — Start here\n")
        self.write("old/a.md", "[[b]]")
        self.write("old/b.md", "[[a]]")
        self.assertEqual(self.checks()["unreachable"], [])

    def test_only_first_200_lines_are_entry_points(self):
        self.write("MEMORY.md", "# filler\n" * 199 + "- [a](a.md)\n- [b](b.md)\n")
        self.write("a.md", "A")
        self.write("b.md", "B")
        self.assertEqual(self.checks()["unreachable"], ["b"])

    def test_byte_limit_applies_to_utf8_not_character_count(self):
        self.write("MEMORY.md", "あ" * (memviewer.INDEX_LIMIT_BYTES // 3 + 1) + "\n- [a](a.md)\n")
        self.write("a.md", "A")
        self.assertEqual(self.checks()["unreachable"], ["a"])

    def test_topic_needs_a_link_from_the_entry_index(self):
        self.write("MEMORY.md", "# Memory\n")
        self.write("topic/INDEX.md", "- [a](a.md)\n")
        self.write("topic/a.md", "A")
        self.assertEqual(self.checks()["unreachable"], ["a"])
        self.write("MEMORY.md", "- [Topic](topic/INDEX.md)\n")
        checks = self.checks()
        self.assertEqual(checks["unreachable"], [])
        self.assertEqual(checks["missing_file"], [])

    def test_existing_bold_topic_pointer_is_an_entry_point(self):
        self.write("MEMORY.md", "Read **topic/INDEX.md** when working on this topic.\n")
        self.write("topic/INDEX.md", "- [a](a.md)\n")
        self.write("topic/a.md", "A")
        self.assertEqual(self.checks()["unreachable"], [])

    def test_nested_relative_markdown_links(self):
        self.write("MEMORY.md", "- [Topic](topic/deep/INDEX.md)\n")
        self.write("topic/deep/INDEX.md", "- [a](a.md)\n")
        self.write("topic/deep/a.md", "See [b](../../old/b.md#details).")
        self.write("old/b.md", "B")
        checks = self.checks()
        self.assertEqual(checks["unreachable"], [])
        self.assertEqual(checks["missing_file"], [])

    def test_missing_lower_index_target_is_reported(self):
        self.write("MEMORY.md", "- [Topic](topic/INDEX.md)\n")
        self.write("topic/INDEX.md", "- [missing](missing.md)\n")
        self.write("topic/a.md", "A")
        self.assertEqual(self.checks()["missing_file"], ["topic/missing.md"])

    def test_wiki_format_and_index_have_no_startup_truncation(self):
        self.write("_index.md", "# filler\n" * 205 + "[[a]] — When to read A\n")
        self.write("SKILL.md", "Start at _index.md.")
        self.write("_schema.md", "Facts need evidence.")
        self.write("a.md", "A")
        project = memviewer.scan()[0]
        self.assertEqual(project["index_name"], "_index.md")
        self.assertEqual([m.name for m in project["memories"]], ["a"])
        checks = memviewer.analyze(project)
        self.assertEqual(checks["unreachable"], [])
        self.assertEqual(checks["missing_line"], [])
        self.assertEqual(checks["missing_file"], [])
        row = memviewer.index_by_file(project)["a.md"]
        self.assertEqual(row["hook"], "When to read A")
        self.assertEqual(row["indexName"], "_index.md")

    def test_an_unlinked_sub_index_does_not_seed_a_wiki(self):
        self.write("_index.md", "# Wiki\n")
        self.write("topic/_index.md", "[[a]] — A\n")
        self.write("topic/a.md", "A")
        self.assertEqual(self.checks()["unreachable"], ["a"])
        self.write("_index.md", "- [Topic](topic/_index.md)\n")
        self.assertEqual(self.checks()["unreachable"], [])

    def test_small_injected_index_can_link_to_a_full_wiki_catalog(self):
        self.write("MEMORY.md", "- [Wiki](_index.md) — Read for implementation questions\n")
        self.write("_index.md", "# filler\n" * 205 + "[[a]] — A\n")
        self.write("a.md", "A")
        checks = self.checks()
        self.assertEqual(checks["unreachable"], [])
        self.assertEqual(checks["missing_line"], [])
        self.assertEqual(checks["missing_file"], [])
        with mock.patch.object(memviewer, "send_targets", return_value={}):
            project = memviewer.build_payload()["projects"][0]
        self.assertEqual(project["indexSources"]["MEMORY.md"]["rows"][0]["target"], "_index.md")
        self.assertEqual(project["memories"][0]["index"]["indexName"], "_index.md")

    def test_ambiguous_wiki_slug_does_not_choose_an_unrelated_topic(self):
        self.write("_index.md", "[[a]] — Ambiguous\n")
        self.write("one/a.md", "One")
        self.write("two/a.md", "Two")
        self.assertEqual(self.checks()["missing_file"], ["a.md"])
        self.write("_index.md", "- [One](one/a.md)\n- [Two](two/a.md)\n")
        self.assertEqual(self.checks()["unreachable"], [])

    def test_analysis_does_not_accumulate_inbound_links(self):
        self.write("MEMORY.md", "- [a](a.md)\n")
        self.write("a.md", "[[b]]")
        self.write("b.md", "B")
        project = memviewer.scan()[0]
        memviewer.analyze(project)
        project["memories"][0].links = []
        memviewer.analyze(project)
        self.assertEqual(project["memories"][1].inbound, [])

    def test_japanese_descriptions_produce_duplicate_candidates(self):
        self.write("MEMORY.md", "- [a](a.md)\n- [b](b.md)\n- [c](c.md)\n")
        for name, description in (("a", "作業ブランチを最新化する手順と確認事項"),
                                  ("b", "作業ブランチを最新化する手順と注意事項"),
                                  ("c", "請求書の支払い期限と銀行口座")):
            self.write(name + ".md", f"---\ndescription: {description}\n---\nBody")
        similar = self.checks()["similar"]
        self.assertEqual([(p["a"], p["b"]) for p in similar], [("a", "b")])

    def test_explicit_directory_does_not_include_other_projects(self):
        self.write("MEMORY.md", "- [a](a.md)\n")
        self.write("a.md", "A")
        with mock.patch.object(memviewer, "PROJECTS", self.root / "elsewhere"):
            self.assertEqual(len(memviewer.scan()), 1)

    def test_payload_and_send_lookup_use_the_actual_index_name(self):
        self.write("_index.md", "[[a]] — When to read A\n")
        self.write("a.md", "A")
        with mock.patch.object(memviewer, "send_targets", return_value={}):
            project = memviewer.build_payload()["projects"][0]
        self.assertEqual(project["indexName"], "_index.md")
        self.assertFalse(project["indexLimited"])
        _, row, index_name = memviewer.find_memory(str(self.root), "", "a.md")
        self.assertEqual(index_name, "_index.md")
        self.assertEqual(row["hook"], "When to read A")

    def test_explicit_roots_with_the_same_basename_stay_distinct(self):
        roots = [self.root / "one/memory", self.root / "two/memory"]
        for root in roots:
            root.mkdir(parents=True)
            (root / "MEMORY.md").write_text("- [a](a.md)\n")
            (root / "a.md").write_text("A")
        with mock.patch.object(memviewer, "MEMORY_DIRS", roots), \
                mock.patch.object(memviewer, "send_targets", return_value={}):
            projects = memviewer.build_payload()["projects"]
        self.assertNotEqual(projects[0]["label"], projects[1]["label"])
        self.assertEqual({m["project"] for m in projects[-1]["memories"]}, {str(r) for r in roots})


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
