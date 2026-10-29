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

    def test_import_does_not_follow_a_planted_directory_symlink(self):
        # The container can write its sessions dir. A symlink planted there
        # must not lead the host-side clearing into the host store.
        mine = rollout(self.host, DAY + "/rollout-a.jsonl", self.work)
        theirs = rollout(self.host, DAY + "/rollout-b.jsonl", self.other)
        (self.task / "2026").symlink_to(self.host / "2026")
        hw.codex_import(self.task, self.host, str(self.work), self.log)
        self.assertTrue(mine.exists())
        self.assertTrue(theirs.exists())
        self.assertFalse((self.task / "2026").is_symlink())

    def test_export_ignores_symlinks(self):
        outside = rollout(self.root / "elsewhere", DAY + "/rollout-x.jsonl", self.work)
        (self.task / "2026").symlink_to(self.root / "elsewhere/2026")
        hw.codex_export(self.task, self.host, str(self.work), self.log)
        self.assertFalse((self.host / DAY / "rollout-x.jsonl").exists())
        self.assertTrue(outside.exists())

    def test_export_does_not_restore_a_rollout_the_host_removed(self):
        rel = DAY + "/rollout-a.jsonl"
        host_copy = rollout(self.host, rel, self.work)
        hw.codex_import(self.task, self.host, str(self.work), self.log)
        archived = self.root / "archived.jsonl"
        host_copy.rename(archived)  # codex archive moves it out of sessions/
        hw.codex_export(self.task, self.host, str(self.work), self.log)
        hw.codex_import(self.task, self.host, str(self.work), self.log)
        self.assertFalse((self.host / rel).exists())

    def test_import_keeps_a_container_rollout_it_could_not_link(self):
        # If a codex release moved the cwd out of the first line, export would
        # never link the container's rollouts; clearing them at the next start
        # would delete every conversation made in the window, silently.
        rel = "2026/10/26/rollout-unreadable.jsonl"
        (self.task / rel).parent.mkdir(parents=True)
        (self.task / rel).write_text('{"type":"session_meta_v2"}\n')
        hw.codex_import(self.task, self.host, str(self.work), self.log)
        self.assertEqual((self.task / rel).read_text(), '{"type":"session_meta_v2"}\n')
        self.assertIn("kept", self.log.getvalue())

    def test_prune_removes_a_fully_linked_task_dir(self):
        rollout(self.host, DAY + "/rollout-a.jsonl", self.work)
        hw.codex_import(self.task, self.host, str(self.work), self.log)
        rollout(self.task, "2026/10/26/rollout-new.jsonl", self.work)
        hw.codex_export(self.task, self.host, str(self.work), self.log)
        self.assertTrue(hw.codex_prune(self.task, self.host, self.log))
        self.assertFalse(self.task.exists())
        self.assertFalse(hw._manifest(self.task).exists())

    def test_prune_keeps_a_task_dir_holding_an_unlinked_rollout(self):
        rel = "2026/10/26/rollout-unreadable.jsonl"
        (self.task / rel).parent.mkdir(parents=True)
        (self.task / rel).write_text("{}\n")
        self.assertFalse(hw.codex_prune(self.task, self.host, self.log))
        self.assertTrue((self.task / rel).exists())

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

    def test_reindex_requests_scan_and_repair(self):
        seen = self.root / "seen.json"
        fake = self.root / "codex"
        fake.write_text(f"#!{sys.executable}\n" + (
            "import json,sys\n"
            f"open({str(seen)!r}, 'a').write(json.dumps(sys.argv[1:]) + '\\n')\n"
            "import os\n"
            f"open({str(seen)!r}, 'a').write(json.dumps({{'cwd': os.getcwd()}}) + '\\n')\n"
            "for line in sys.stdin:\n"
            "    m = json.loads(line)\n"
            "    if 'id' not in m: continue\n"
            f"    open({str(seen)!r}, 'a').write(json.dumps(m) + '\\n')\n"
            "    print(json.dumps({'id': m['id'], 'result': {'data': []}}), flush=True)\n"))
        fake.chmod(0o755)
        self.assertTrue(hw.codex_reindex(str(fake), str(self.work), self.log))
        lines = [json.loads(l) for l in seen.read_text().splitlines()]
        self.assertEqual(lines[0], ["app-server", "--listen", "stdio://"])
        # The host's codex must not start in the worktree: the container can
        # write there (project-scoped config), and it may already be removed.
        self.assertEqual(lines[1]["cwd"], "/")
        listing = next(l for l in lines[1:] if l.get("method") == "thread/list")["params"]
        self.assertIs(listing["useStateDbOnly"], False)
        self.assertEqual(listing["cwd"], str(self.work))
        self.assertIn("exec", listing["sourceKinds"])

    def test_reindex_reports_a_dead_server(self):
        fake = self.root / "codex"
        fake.write_text("#!/bin/sh\nexit 0\n")
        fake.chmod(0o755)
        self.assertFalse(hw.codex_reindex(str(fake), str(self.work), self.log))
        self.assertIn("reindex", self.log.getvalue())

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
        # `podman info` names the seccomp profile the wrapper derives its own from.
        seccomp = self.tmp / "seccomp.json"
        seccomp.write_text(json.dumps({"syscalls": [{"names": ["clone"], "action": "SCMP_ACT_ALLOW"}]}))
        self.podman_info = f"if sys.argv[1] == 'info': print({str(seccomp)!r}); sys.exit(0)\n"
        executable(tools / "podman", f"#!{sys.executable}\nimport json,sys\n" + self.podman_info
                   + f"open({str(self.capture)!r}, 'w').write(json.dumps(sys.argv[1:]))\n")
        (self.home / ".claude").mkdir()
        (self.home / ".claude/.credentials.json").write_text(json.dumps({"claudeAiOauth": {
            "accessToken": "real-access", "refreshToken": "real-refresh", "subscriptionType": "max",
            "rateLimitTier": "tier-x"}}))
        (self.home / ".claude/CLAUDE.md").write_text("# host instructions\n")
        (self.home / ".claude/settings.json").write_text('{"host": "settings"}')
        (self.home / ".claude/skills").mkdir()
        (self.home / ".claude.json").write_text('{"host": "state"}')
        executable(self.home / ".local/bin/claude", "#!/bin/sh\n")
        # The host relay (sandbox-connect-proxy relay): records its argv, serves
        # the socket the wrapper waits for, and leaves with the wrapper's pid.
        self.relay_argv = self.tmp / "relay-argv.json"
        relay = executable(self.tmp / "fake-relay", f"#!{sys.executable}\n" + (
            "import json,os,socket,sys,time\n"
            "a = sys.argv[1:]\n"
            f"open({str(self.relay_argv)!r}, 'w').write(json.dumps(a))\n"
            "if os.environ.get('FAKE_RELAY_FAIL'): print('FATAL relay: no token', file=sys.stderr); sys.exit(1)\n"
            "s = socket.socket(socket.AF_UNIX); s.bind(a[a.index('--socket') + 1])\n"
            "ppid = int(a[a.index('--parent-pid') + 1])\n"
            "while True:\n"
            "    try: os.kill(ppid, 0)\n"
            "    except OSError: break\n"
            "    time.sleep(0.05)\n"))
        token = self.tmp / "token"
        token.write_text("x")
        pi_pkg = self.tmp / "pi-pkg"
        executable(pi_pkg / "dist/bundle/cli.js", "#!/bin/sh\n")
        (self.home / ".pi/agent").mkdir(parents=True)
        (self.home / ".pi/agent/auth.json").write_text(json.dumps({"openrouter": {"type": "api_key", "key": "test-key"}}))
        (self.home / ".pi/agent/settings.json").write_text(json.dumps({"defaultProvider": "openrouter"}))
        executable(self.home / ".pi/agent/bin/fd", "#!/bin/sh\n")
        self.env = {"HOME": str(self.home), "PATH": str(tools) + os.pathsep + os.environ["PATH"], "TMPDIR": str(self.tmp),
                    "HARNESS_SANDBOX_CLAUDE_TOKEN_FILE": str(token), "HARNESS_SANDBOX_PI_PKG": str(pi_pkg),
                    "HARNESS_SANDBOX_RELAY_BIN": str(relay)}

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
        _, argv = self.ok("claude", "--history-window", "--mount-auth")
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
        _, argv = self.ok("pi", "--history-window", "--mount-auth")
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
        err, argv = self.ok("pi", "--mount-auth")
        self.assertIn(f"{self.home}/.pi:{self.home}/.pi", argv)
        self.assertIn("SANDBOX_HISTORY=shared", argv)
        self.assertIn("history=shared", err)

    def test_container_uses_host_timezone(self):
        # codex names and files rollouts by LOCAL time; a UTC container's
        # rollouts sorted hours behind the host's and `resume --last` on the
        # host never found them (measured 2026-10-26).
        for args in ((), ("--history-window",)):
            with self.subTest(args=args):
                _, argv = self.ok("pi", *args)
                self.assertIn("--tz=local", argv)

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
            ("claude", self.wt, ".credentials.json", self.home / ".claude/.credentials.json", "--mount-auth"),
            # --mount-auth: pi injects by default, and then mounts no auth.json.
            ("pi", self.wt, "auth.json", self.home / ".pi/agent/auth.json", "--mount-auth"),
        ]

    def test_refusals(self):
        for agent, cwd, why, remove, *extra in self.refusal_cases():
            with self.subTest(agent=agent, why=why):
                if remove is not None:
                    remove.unlink()
                rc, err, argv = self.wrap(agent, "--history-window", *extra, cwd=cwd)
                if remove is not None:
                    remove.write_text("x")
                self.assertEqual(rc, 2, err)
                self.assertIn("--history-window", err)
                self.assertIn(why, err)
                self.assertIsNone(argv)

    def test_refusal_leaves_no_temp_root(self):
        for agent, cwd, _, remove, *extra in self.refusal_cases():
            if remove is not None:
                remove.unlink()
            self.wrap(agent, "--history-window", *extra, cwd=cwd)
            if remove is not None:
                remove.write_text("x")
        self.assertEqual(list(self.tmp.glob("harness-sandbox-root.*")), [])

    def inject_root(self, argv):
        return Path(next(a for a in argv if a.endswith(f":{self.home}/.pi")).split(":")[0])

    def test_pi_injects_by_default(self):
        err, argv = self.ok("pi")
        home = self.home
        root = self.inject_root(argv)
        self.assertRegex(str(root), r"/harness-sandbox-root\.[^/]+/\.pi$")
        # The whole sessions store, so every conversation stays resumable.
        self.assertIn(f"{home}/.pi/agent/sessions:{home}/.pi/agent/sessions", argv)
        self.assertIn(f"{home}/.pi/agent/settings.json:{home}/.pi/agent/settings.json:ro", argv)
        self.assertIn(f"{home}/.pi/agent/bin:{home}/.pi/agent/bin:ro", argv)
        cfg = next(a for a in argv if a.endswith(f":{home}/.pi/agent/models.json:ro"))
        models = json.loads(Path(cfg.split(":")[0]).read_text())
        self.assertEqual(models["providers"]["openrouter"],
                         {"baseUrl": "http://127.0.0.1:18081/api/v1", "apiKey": "sandbox-placeholder"})
        # The credential: no file, no value on the argv, only the NAME.
        self.assertFalse(any("auth.json" in a for a in argv))
        self.assertFalse(any("test-key" in a for a in argv), argv)
        self.assertIn("SANDBOX_INJECT_KEY", argv)
        self.assertIn("SANDBOX_INJECT_UPSTREAM=https://openrouter.ai", argv)
        self.assertNotIn(f"{home}/.pi:{home}/.pi", argv)
        self.assertIn("auth=inject", err)
        self.assertIn("SANDBOX_HISTORY=shared", argv)

    def test_pi_injected_key_reaches_podman_env(self):
        env_dump = self.tmp / "podman-env"
        executable(self.tools / "podman", f"#!{sys.executable}\nimport json,os,sys\n" + self.podman_info
                   + f"open({str(self.capture)!r}, 'w').write(json.dumps(sys.argv[1:]))\n"
                   + f"open({str(env_dump)!r}, 'w').write(os.environ.get('SANDBOX_INJECT_KEY', ''))\n")
        self.ok("pi")
        self.assertEqual(env_dump.read_text(), "test-key")

    def test_pi_inject_with_window_binds_this_cwd_only(self):
        _, argv = self.ok("pi", "--history-window")
        home = self.home
        name = "--" + str(self.wt).lstrip("/").replace("/", "-") + "--"
        self.assertIn(f"{home}/.pi/agent/sessions/{name}:{home}/.pi/agent/sessions/{name}", argv)
        self.assertNotIn(f"{home}/.pi/agent/sessions:{home}/.pi/agent/sessions", argv)
        self.assertFalse(any("auth.json" in a for a in argv))
        self.assertIn("SANDBOX_HISTORY=window", argv)

    def test_inject_refusals(self):
        cases = [
            ("pi", ("--firewall",), "--mount-auth"),
            ("pi", ("--no-firewall",), "--mount-auth"),
            ("pi", ("--inject-auth", "--mount-auth"), "contradict"),
            ("pi", ("--inject-auth", "--token-auth"), "contradict"),
            ("claude", ("--firewall",), "--mount-auth"),
            ("claude", ("--inject-auth", "--token-auth"), "contradict"),
            ("codex", ("--inject-auth",), "no credential injection"),
        ]
        for agent, args, why in cases:
            with self.subTest(agent=agent, args=args):
                rc, err, argv = self.wrap(agent, *args)
                self.assertEqual(rc, 2, err)
                self.assertIn(why, err)
                self.assertIsNone(argv)

    def test_inject_refusal_creates_nothing(self):
        (self.home / ".pi/agent/auth.json").write_text(json.dumps({"openrouter": {"type": "oauth"}}))
        rc, err, argv = self.wrap("pi")
        self.assertEqual(rc, 2, err)
        self.assertIn("api_key", err)
        self.assertIn("--mount-auth", err)
        self.assertIsNone(argv)
        self.assertEqual(list(self.tmp.glob("harness-sandbox-root.*")), [])
        self.assertEqual(list(self.tmp.glob("harness-sandbox-inject.*")), [])

    def claude_root(self, argv):
        pair = next(a for a in argv if a.endswith(f":{self.home}/.claude"))
        self.assertRegex(pair, r"/harness-sandbox-root\.[^/]+/\.claude:")
        return Path(pair.split(":")[0]).parent

    def test_claude_injects_by_default_through_the_relay(self):
        err, argv = self.ok("claude")
        home = self.home
        root = self.claude_root(argv)
        # Every conversation, as under mount auth; the credential nowhere.
        self.assertIn(f"{home}/.claude/projects:{home}/.claude/projects", argv)
        self.assertFalse(any(".credentials.json" in a for a in argv), argv)
        self.assertNotIn(f"{home}/.claude:{home}/.claude", argv)
        # Host config: copies the container may write, the rest read-only.
        self.assertEqual((root / ".claude.json").read_text(), '{"host": "state"}')
        self.assertEqual((root / ".claude/settings.json").read_text(), '{"host": "settings"}')
        self.assertIn(f"{home}/.claude/CLAUDE.md:{home}/.claude/CLAUDE.md:ro", argv)
        self.assertIn(f"{home}/.claude/skills:{home}/.claude/skills:ro", argv)
        self.assertFalse(any("/.claude/hooks:" in a for a in argv))  # absent on this host
        self.assertIn("ANTHROPIC_BASE_URL=http://127.0.0.1:18081", argv)
        self.assertIn("CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-sandbox-placeholder", argv)
        # The login's tier, so the agent picks the host's default model.
        self.assertIn("CLAUDE_CODE_SUBSCRIPTION_TYPE=max", argv)
        self.assertIn("CLAUDE_CODE_RATE_LIMIT_TIER=tier-x", argv)
        self.assertFalse(any("real-access" in a or "real-refresh" in a for a in argv))
        self.assertIn("SANDBOX_INJECT_UPSTREAM=unix:/run/harness-relay/sock", argv)
        self.assertTrue(any(re.fullmatch(r".*/harness-sandbox-relay\.[^/]+/sock:/run/harness-relay/sock", a) for a in argv), argv)
        self.assertNotIn("SANDBOX_INJECT_KEY", argv)
        self.assertIn("SANDBOX_SEED_CONFIG=1", argv)
        self.assertIn("auth=inject", err)
        relay = json.loads(self.relay_argv.read_text())
        flag = lambda name: relay[relay.index(name) + 1]
        self.assertEqual(relay[0], "relay")
        self.assertEqual(flag("--kind"), "claude")
        self.assertEqual(flag("--upstream"), "https://api.anthropic.com")
        self.assertEqual(flag("--credentials"), f"{home}/.claude/.credentials.json")
        self.assertEqual(flag("--agent-bin"), str(home / ".local/bin/claude"))

    def test_claude_reaper_carries_the_saved_model_back_to_the_host(self):
        (self.tools / "setsid").unlink()  # the real one: this test needs the reaper
        # podman stands in for the container's claude running `/model sonnet`:
        # it edits the per-run copy of settings.json, and a hook besides.
        executable(self.tools / "podman", f"#!{sys.executable}\n" + (
            "import json,sys,pathlib\n" + self.podman_info +
            "if sys.argv[1] == 'run':\n"
            "    a = sys.argv\n"
            f"    root = next(x.split(':')[0] for x in a if x.endswith(':{self.home}/.claude'))\n"
            "    p = pathlib.Path(root) / 'settings.json'\n"
            "    d = json.loads(p.read_text()); d['model'] = 'sonnet'; d['hooks'] = {'Stop': ['evil']}\n"
            "    p.write_text(json.dumps(d))\n"))
        self.ok("claude")
        host = self.home / ".claude/settings.json"
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline and "sonnet" not in host.read_text():
            time.sleep(0.2)
        log = self.tmp / "sandbox-reaper.log"
        d = json.loads(host.read_text())
        self.assertEqual(d.get("model"), "sonnet", log.read_text() if log.exists() else "")
        self.assertNotIn("hooks", d)
        self.assertEqual(d["host"], "settings")

    def test_claude_inject_with_window_carries_no_host_config(self):
        _, argv = self.ok("claude", "--history-window")
        home, enc = self.home, re.sub(r"[^A-Za-z0-9]", "-", str(self.wt))
        root = self.claude_root(argv)
        self.assertIn(f"{home}/.claude/projects/{enc}:{home}/.claude/projects/{enc}", argv)
        self.assertEqual((root / ".claude.json").read_text().strip(), "{}")
        self.assertFalse((root / ".claude/settings.json").exists())
        self.assertFalse(any(a.endswith(":ro") and "/.claude/" in a for a in argv), argv)
        self.assertFalse(any(".credentials.json" in a for a in argv))
        self.assertIn("SANDBOX_INJECT_UPSTREAM=unix:/run/harness-relay/sock", argv)

    def test_claude_relay_failure_refuses_and_cleans_up(self):
        self.env["FAKE_RELAY_FAIL"] = "1"
        rc, err, argv = self.wrap("claude")
        self.assertEqual(rc, 2, err)
        self.assertIn("relay did not start", err)
        self.assertIn("no token", err)
        self.assertIn("--mount-auth", err)
        self.assertIsNone(argv)
        self.assertEqual(list(self.tmp.glob("harness-sandbox-relay.*")), [])
        self.assertEqual(list(self.tmp.glob("harness-sandbox-root.*")), [])

    def domains(self, argv):
        v = next(a for a in argv if a.startswith("SANDBOX_AGENT_DOMAINS="))
        return v.split("=", 1)[1]

    def test_injected_agents_drop_their_api_domains(self):
        # The injector reaches the provider; the agent has no reason to.
        for agent in ("pi", "claude"):
            with self.subTest(agent=agent):
                self.assertEqual(self.domains(self.ok(agent)[1]), "")
        self.assertEqual(self.domains(self.ok("pi", "--mount-auth")[1]), "openrouter.ai")
        self.assertIn("api.anthropic.com", self.domains(self.ok("claude", "--mount-auth")[1]))

    def test_allow_domain_extends_the_list_in_both_modes(self):
        err, argv = self.ok("claude", "--mount-auth", "--allow-domain=docs.example.com,x.example.org",
                            "--allow-domain=y.example.net")
        self.assertTrue(self.domains(argv).endswith(",docs.example.com,x.example.org,y.example.net"), argv)
        self.assertIn("allow+=docs.example.com,x.example.org,y.example.net", err)
        _, argv = self.ok("pi", "--allow-domain=docs.example.com")
        self.assertEqual(self.domains(argv), "docs.example.com")
        _, argv = self.ok("bash", "--firewall", "--allow-domain=docs.example.com")
        self.assertIn("SANDBOX_FIREWALL=1", argv)
        self.assertEqual(self.domains(argv), "docs.example.com")

    def test_allow_domain_refusals(self):
        for value in ("--allow-domain=bad_name.example", "--allow-domain=", "--allow-domain",
                      "--allow-domain=a.example.com;rm", "--allow-domain=localhost", "--allow-domain=a b.example"):
            with self.subTest(value=value):
                rc, err, argv = self.wrap("bash", value)
                self.assertEqual(rc, 2, err)
                self.assertIn("--allow-domain", err)
                self.assertIsNone(argv)

    def test_allow_domain_under_no_firewall_warns(self):
        err, argv = self.ok("bash", "--no-firewall", "--allow-domain=docs.example.com")
        self.assertIn("--allow-domain ignored", err)
        self.assertFalse(any(a.startswith("SANDBOX_AGENT_DOMAINS=") for a in argv))

    def test_mount_auth_still_mounts_whole_pi(self):
        _, argv = self.ok("pi", "--mount-auth", "--firewall")
        self.assertIn(f"{self.home}/.pi:{self.home}/.pi", argv)
        self.assertFalse(any(a.startswith("SANDBOX_INJECT") for a in argv))

    def codex_fixture(self):
        bundle = self.tmp / "codex-release/bin"
        self.codex_calls = self.tmp / "codex-calls"
        executable(bundle / "codex", f'#!/bin/sh\necho "$*" >>{self.codex_calls}\n')
        executable(bundle / "codex-code-mode-host", "#!/bin/sh\n")
        (self.home / ".local/bin").mkdir(parents=True, exist_ok=True)
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
            "import json,sys,pathlib\n" + self.podman_info +
            "if sys.argv[1] == 'run':\n"
            f"    p = pathlib.Path({str(new)!r}); p.parent.mkdir(parents=True, exist_ok=True)\n"
            f"    p.write_text(json.dumps({{'type':'session_meta','payload':{{'cwd':{str(self.wt)!r}}}}}) + '\\n')\n"))
        self.ok("codex", "--history-window")
        target = self.host_sessions / "2026/10/26/rollout-new.jsonl"
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline and not (target.exists() and not list(self.tmp.glob("harness-sandbox-root.*"))):
            time.sleep(0.2)
        self.assertTrue(target.exists(), (self.tmp / "sandbox-reaper.log").read_text() if (self.tmp / "sandbox-reaper.log").exists() else "")
        self.assertIn(str(self.wt), target.read_text())
        self.assertEqual(list(self.tmp.glob("harness-sandbox-root.*")), [])
        # The derived seccomp profile goes with the container too.
        self.assertEqual(list(self.tmp.glob("sandbox-seccomp.*")), [])
        # Then the host's codex re-indexes this cwd, or `resume --last` on the
        # host never sees a thread the container created.
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline and not self.codex_calls.exists():
            time.sleep(0.2)
        self.assertIn("app-server", self.codex_calls.read_text())
        # And the per-task dir goes once every rollout in it is on the host:
        # its hardlinks would otherwise outlive conversations the host deletes.
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline and d.exists():
            time.sleep(0.2)
        self.assertFalse(d.exists())


if __name__ == "__main__":
    unittest.main()
