"""Offline regressions for the sandbox history window (--history-window)."""

import io
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile
import time
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


WRAPPER = Path(__file__).with_name("agent-in-podman.sh")


def executable(path, body):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(body)
    path.chmod(0o755)
    return path


def git(*args):
    subprocess.run(["git", "-c", "user.name=t", "-c", "user.email=t@t", *args], check=True, capture_output=True)


class WrapperTests(unittest.TestCase):
    """The podman argv the wrapper assembles, captured by a podman shim."""

    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.tmp = Path(temp.name)
        self.home = self.tmp / "home"
        self.home.mkdir()
        self.repo, self.wt = self.tmp / "repo", self.tmp / "wt"
        git("init", "-q", str(self.repo))
        git("-C", str(self.repo), "commit", "-q", "--allow-empty", "-m", "init")
        git("-C", str(self.repo), "worktree", "add", "-q", str(self.wt))
        self.plain_dir = self.tmp / "plain"
        self.plain_dir.mkdir()
        self.capture = self.tmp / "capture.json"
        self.tools = tools = self.tmp / "tools"
        executable(tools / "setsid", "#!/bin/sh\nexit 0\n")
        executable(tools / "podman", f"#!{sys.executable}\nimport json,sys\nopen({str(self.capture)!r}, 'w').write(json.dumps(sys.argv[1:]))\n")
        (self.home / ".claude").mkdir()
        (self.home / ".claude/.credentials.json").write_text("x")
        token = self.tmp / "token"
        token.write_text("x")
        pi_pkg = self.tmp / "pi-pkg"
        executable(pi_pkg / "dist/bundle/cli.js", "#!/bin/sh\n")
        (self.home / ".pi/agent").mkdir(parents=True)
        (self.home / ".pi/agent/auth.json").write_text("x")
        (self.home / ".pi/agent/settings.json").write_text("{}")
        executable(self.home / ".pi/agent/bin/fd", "#!/bin/sh\n")
        self.env = {"HOME": str(self.home), "PATH": str(tools) + os.pathsep + os.environ["PATH"], "TMPDIR": str(self.tmp),
                    "HARNESS_SANDBOX_CLAUDE_TOKEN_FILE": str(token), "HARNESS_SANDBOX_PI_PKG": str(pi_pkg)}

    def wrap(self, agent, *args, cwd=None):
        self.capture.unlink(missing_ok=True)
        result = subprocess.run(["bash", str(WRAPPER), "--sandbox-agent", agent, "--omit-harness-cli", *args],
                                env=self.env, cwd=cwd or self.wt, capture_output=True, text=True, timeout=20)
        argv = json.loads(self.capture.read_text()) if self.capture.exists() else None
        return result.returncode, result.stderr, argv

    def ok(self, agent, *args):
        rc, err, argv = self.wrap(agent, *args)
        self.assertEqual(rc, 0, err)
        return err, argv

    def test_claude_window_mounts(self):
        _, argv = self.ok("claude", "--history-window")
        home, enc = self.home, re.sub(r"[^A-Za-z0-9]", "-", str(self.wt))
        root_pair = next(a for a in argv if a.endswith(f":{home}/.claude"))
        self.assertRegex(root_pair, re.escape(str(self.tmp)) + r"/harness-sandbox-root\.[^/]+/\.claude:")
        self.assertTrue(any(re.search(r"/harness-sandbox-root\.[^/]+/\.claude\.json:" + re.escape(f"{home}/.claude.json") + "$", a)
                            for a in argv))
        win = f"{home}/.claude/projects/{enc}:{home}/.claude/projects/{enc}"
        self.assertIn(win, argv)
        self.assertLess(argv.index(root_pair), argv.index(win))
        self.assertIn(f"{home}/.claude/.credentials.json:{home}/.claude/.credentials.json", argv)
        self.assertNotIn(f"{home}/.claude:{home}/.claude", argv)
        self.assertNotIn(f"{home}/.claude.json:{home}/.claude.json", argv)
        self.assertIn("SANDBOX_SEED_CONFIG=1", argv)
        self.assertIn("SANDBOX_HISTORY=window", argv)
        self.assertTrue((home / ".claude/projects" / enc).is_dir())

    def test_claude_window_token_auth(self):
        _, argv = self.ok("claude", "--history-window", "--token-auth")
        home, enc = self.home, re.sub(r"[^A-Za-z0-9]", "-", str(self.wt))
        root_pair = next(a for a in argv if a.endswith(":/home/node/.claude"))
        self.assertIn("/harness-sandbox-root.", root_pair)
        win = f"{home}/.claude/projects/{enc}:/home/node/.claude/projects/{enc}"
        self.assertIn(win, argv)
        self.assertLess(argv.index(root_pair), argv.index(win))
        self.assertFalse(any(".credentials.json" in a for a in argv))

    def test_pi_window_mounts(self):
        _, argv = self.ok("pi", "--history-window")
        home = self.home
        name = "--" + str(self.wt).lstrip("/").replace("/", "-") + "--"
        root_pair = next(a for a in argv if a.endswith(f":{home}/.pi"))
        self.assertIn("/harness-sandbox-root.", root_pair)
        expected = [root_pair,
                    f"{home}/.pi/agent/sessions/{name}:{home}/.pi/agent/sessions/{name}",
                    f"{home}/.pi/agent/auth.json:{home}/.pi/agent/auth.json",
                    f"{home}/.pi/agent/settings.json:{home}/.pi/agent/settings.json:ro"]
        positions = [argv.index(a) for a in expected]
        self.assertEqual(positions, sorted(positions))
        self.assertNotIn(f"{home}/.pi:{home}/.pi", argv)
        # pi's helper binaries (fd): without them pi warns "fd not found" and
        # tries to download one (measured 2026-10-26).
        self.assertIn(f"{home}/.pi/agent/bin:{home}/.pi/agent/bin:ro", argv)

    def test_shared_mode_unchanged_but_labelled(self):
        err, argv = self.ok("pi")
        self.assertIn(f"{self.home}/.pi:{self.home}/.pi", argv)
        self.assertIn("SANDBOX_HISTORY=shared", argv)
        self.assertIn("history=shared", err)

    def refusal_cases(self):
        long_wt = self.tmp / ("w" * 210)
        git("-C", str(self.repo), "worktree", "add", "-q", str(long_wt))
        return [
            ("opencode", self.wt, "no window", None),
            ("agy", self.wt, "no window", None),
            ("bash", self.wt, "no window", None),
            ("claude", self.repo, "linked worktree", None),
            ("claude", self.plain_dir, "linked worktree", None),
            ("claude", long_wt, "200", None),
            ("claude", self.wt, ".credentials.json", self.home / ".claude/.credentials.json"),
            ("pi", self.wt, "auth.json", self.home / ".pi/agent/auth.json"),
        ]

    def test_refusals(self):
        for agent, cwd, why, remove in self.refusal_cases():
            with self.subTest(agent=agent, why=why):
                if remove is not None:
                    remove.unlink()
                rc, err, argv = self.wrap(agent, "--history-window", cwd=cwd)
                if remove is not None:
                    remove.write_text("x")
                self.assertEqual(rc, 2, err)
                self.assertIn("--history-window", err)
                self.assertIn(why, err)
                self.assertIsNone(argv)

    def test_refusal_leaves_no_temp_root(self):
        for agent, cwd, _, remove in self.refusal_cases():
            if remove is not None:
                remove.unlink()
            self.wrap(agent, "--history-window", cwd=cwd)
            if remove is not None:
                remove.write_text("x")
        self.assertEqual(list(self.tmp.glob("harness-sandbox-root.*")), [])

    def codex_fixture(self):
        bundle = self.tmp / "codex-release/bin"
        executable(bundle / "codex", "#!/bin/sh\n")
        executable(bundle / "codex-code-mode-host", "#!/bin/sh\n")
        (self.home / ".local/bin").mkdir(parents=True)
        (self.home / ".local/bin/codex").symlink_to(bundle / "codex")
        (self.home / ".codex").mkdir()
        (self.home / ".codex/auth.json").write_text("x")
        (self.home / ".codex/config.toml").write_text("")
        self.host_sessions = self.home / ".codex/sessions"
        rollout(self.host_sessions, DAY + "/rollout-a.jsonl", self.wt)
        rollout(self.host_sessions, DAY + "/rollout-b.jsonl", self.plain_dir)
        self.task_id = "0" * 31 + "1"
        self.env.update(XDG_STATE_HOME=str(self.tmp / "state"), HARNESS_TASK_ID=self.task_id)
        return self.tmp / "state/harness-sandbox/codex" / self.task_id

    def test_codex_window_mounts_and_imports(self):
        d = self.codex_fixture()
        _, argv = self.ok("codex", "--history-window")
        home = self.home
        self.assertIn(f"{d}:{home}/.codex/sessions", argv)
        self.assertIn(f"{home}/.codex/thread-writer-locks:{home}/.codex/thread-writer-locks", argv)
        self.assertIn(f"{home}/.codex/auth.json:{home}/.codex/auth.json", argv)
        self.assertIn(f"{home}/.codex/config.toml:{home}/.codex/config.toml:ro", argv)
        self.assertNotIn(f"{home}/.codex:{home}/.codex", argv)
        self.assertEqual(sorted(p.name for p in d.rglob("rollout-*.jsonl")), ["rollout-a.jsonl"])

    def test_codex_reaper_exports_after_exit(self):
        d = self.codex_fixture()
        (self.tools / "setsid").unlink()  # the real one: this test needs the reaper
        new = d / "2026/10/26/rollout-new.jsonl"
        executable(self.tools / "podman", f"#!{sys.executable}\n" + (
            "import json,sys,pathlib\n"
            "if sys.argv[1] == 'run':\n"
            f"    p = pathlib.Path({str(new)!r}); p.parent.mkdir(parents=True, exist_ok=True)\n"
            f"    p.write_text(json.dumps({{'type':'session_meta','payload':{{'cwd':{str(self.wt)!r}}}}}) + '\\n')\n"))
        self.ok("codex", "--history-window")
        target = self.host_sessions / "2026/10/26/rollout-new.jsonl"
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline and not (target.exists() and not list(self.tmp.glob("harness-sandbox-root.*"))):
            time.sleep(0.2)
        self.assertTrue(target.exists(), (self.tmp / "sandbox-reaper.log").read_text() if (self.tmp / "sandbox-reaper.log").exists() else "")
        self.assertEqual(target.stat().st_ino, new.stat().st_ino)
        self.assertEqual(list(self.tmp.glob("harness-sandbox-root.*")), [])


if __name__ == "__main__":
    unittest.main()
