"""Offline regressions for Codex bundle mounts and upstream-release validation."""

import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import tarfile
import tempfile
import unittest
from unittest import mock

import check_codex_release as check


def executable(path, body):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("#!/bin/sh\n" + body + "\n")
    path.chmod(0o755)
    return path


def metadata():
    name = "codex-package-x86_64-unknown-linux-musl.tar.gz"
    return {
        "tag_name": "rust-v0.164.0", "draft": False, "prerelease": False,
        "published_at": "2026-10-23T04:48:10Z",
        "html_url": "https://github.com/openai/codex/releases/tag/rust-v0.164.0",
        "assets": [{"name": name, "size": 10, "digest": "sha256:" + "0" * 64,
                    "browser_download_url": "https://github.com/openai/codex/releases/download/rust-v0.164.0/" + name}],
    }


class CodexReleaseTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.binary = executable(self.root / "release/bin/codex", "echo 'codex-cli 0.164.0'")
        executable(self.binary.with_name("codex-code-mode-host"), "echo 'Usage: codex-code-mode-host [OPTIONS]'")

    def test_missing_helper_is_not_a_successful_runtime_check(self):
        self.binary.with_name("codex-code-mode-host").unlink()
        with self.assertRaisesRegex(ValueError, "missing executable siblings"):
            check.inspect_bundle(self.binary)

    def test_version_and_all_siblings_are_reported(self):
        executable(self.binary.with_name("codex-future-helper"), "exit 0")
        report = check.inspect_bundle(self.binary)
        self.assertEqual(report["version"], "0.164.0")
        self.assertIn("codex-future-helper", report["executables"])

    def test_downloaded_bundle_can_be_inspected_without_host_execution(self):
        with mock.patch.object(check.subprocess, "run", side_effect=AssertionError("must not execute on host")):
            report = check.inspect_bundle(self.binary, run_version=False)
        self.assertIsNone(report["version"])
        self.assertIn("codex-code-mode-host", report["executables"])

    def test_wrapper_preserves_the_release_directory_read_only(self):
        home, tools = self.root / "home", self.root / "tools"
        (home / ".local/bin").mkdir(parents=True)
        (home / ".local/bin/codex").symlink_to(self.binary)
        capture = self.root / "podman.json"
        executable(tools / "setsid", "exit 0")
        podman = tools / "podman"
        podman.write_text(f"#!{os.sys.executable}\nimport json,sys\nopen({str(capture)!r}, 'w').write(json.dumps(sys.argv[1:]))\n")
        podman.chmod(0o755)
        env = {"HOME": str(home), "PATH": str(tools) + os.pathsep + os.environ["PATH"]}
        subprocess.run(["bash", str(check.WRAPPER), "--sandbox-agent", "codex", "--omit-harness-cli", "--version"],
                       env=env, cwd=self.root, capture_output=True, check=True, timeout=10)
        args = json.loads(capture.read_text())
        self.assertIn(str(self.binary.parent) + ":/opt/harness-codex-bin:ro", args)
        self.assertIn("SANDBOX_AGENT_BIN=/opt/harness-codex-bin/codex", args)
        self.assertFalse(any(arg.endswith(":/usr/local/bin/codex:ro") for arg in args))

    def test_launcher_exposes_the_selected_bundle_on_path(self):
        self.binary.write_text("#!/bin/sh\ncommand -v codex\n")
        env = {"PATH": os.environ["PATH"], "SANDBOX_AGENT_BIN": str(self.binary)}
        result = subprocess.run(["bash", str(check.WRAPPER.with_name("agent-launch.sh"))],
                                env=env, text=True, capture_output=True, check=True, timeout=10)
        self.assertEqual(result.stdout.strip(), str(self.binary))

    def test_every_agent_keeps_its_binary_mount_strategy(self):
        for agent in ("claude", "codex", "agy", "opencode", "pi", "bash"):
            with self.subTest(agent=agent):
                root = self.root / agent
                home, tools = root / "home", root / "tools"
                binary = executable(home / ".local/bin" / agent, "exit 0")
                pi_package = root / "pi-package"
                executable(pi_package / "dist/bundle/cli.js", "exit 0")
                executable(tools / "setsid", "exit 0")
                capture = root / "args.json"
                podman = tools / "podman"
                podman.write_text(f"#!{os.sys.executable}\nimport json,sys\nopen({str(capture)!r},'w').write(json.dumps(sys.argv[1:]))\n")
                podman.chmod(0o755)
                env = {"HOME": str(home), "PATH": str(tools) + os.pathsep + os.environ["PATH"],
                       "HARNESS_SANDBOX_PI_PKG": str(pi_package), "HARNESS_SANDBOX_OPENCODE_BIN": str(binary)}
                subprocess.run(["bash", str(check.WRAPPER), "--sandbox-agent", agent, "--omit-harness-cli", "--version"],
                               cwd=root, env=env, check=True, capture_output=True, timeout=10)
                args = json.loads(capture.read_text())
                self.assertIn("SANDBOX_AGENT=" + agent, args)
                if agent == "codex":
                    self.assertIn(str(binary.parent) + ":/opt/harness-codex-bin:ro", args)
                elif agent == "pi":
                    self.assertIn(str(pi_package) + ":" + str(pi_package) + ":ro", args)
                elif agent != "bash":
                    self.assertIn(str(binary) + ":/usr/local/bin/" + agent + ":ro", args)
                else:
                    self.assertIn("SANDBOX_AGENT_BIN=/bin/bash", args)

    def test_smoke_uses_real_wrapper_without_auth_mounts_or_network(self):
        tools = self.root / "host-tools"
        capture = self.root / "probe.json"
        podman = tools / "podman"
        podman.parent.mkdir()
        podman.write_text(f"#!{os.sys.executable}\nimport json,sys\nopen({str(capture)!r},'w').write(json.dumps(sys.argv[1:]))\nprint('codex-cli 0.164.0\\nUsage: codex-code-mode-host')\n")
        podman.chmod(0o755)
        executable(tools / "setsid", "exit 0")
        with mock.patch.dict(os.environ, {"PATH": str(tools) + os.pathsep + os.environ["PATH"], "HARNESS_AUTH_TICKET": "must-not-forward"}):
            report = check.smoke_bundle(self.binary, "test-image", self.root / "probe")
        args = json.loads(capture.read_text())
        self.assertIn("--network=none", args)
        self.assertIn(str(self.binary.parent) + ":/opt/harness-codex-bin:ro", args)
        self.assertNotIn("HARNESS_AUTH_TICKET", args)
        self.assertFalse(any(":/" in arg and "/.codex" in arg for arg in args))
        self.assertIn("codex-code-mode-host", report)

    def test_metadata_requires_the_bundle_and_a_digest(self):
        release = metadata()
        self.assertEqual(check.release_asset(release, "x86_64-unknown-linux-musl")["name"], release["assets"][0]["name"])
        release["assets"][0]["digest"] = None
        with self.assertRaisesRegex(ValueError, "SHA-256"):
            check.release_asset(release, "x86_64-unknown-linux-musl")
        release["assets"] = []
        with self.assertRaisesRegex(ValueError, "missing or ambiguous"):
            check.release_asset(release, "x86_64-unknown-linux-musl")

    def test_changed_download_host_is_rejected(self):
        release = metadata()
        release["assets"][0]["browser_download_url"] = "https://example.com/package.tar.gz"
        with self.assertRaisesRegex(ValueError, "unexpected upstream asset URL"):
            check.release_asset(release, "x86_64-unknown-linux-musl")

    def test_prerelease_is_not_reported_as_latest_stable(self):
        release = metadata()
        release["prerelease"] = True
        with mock.patch.object(check.urllib.request, "urlopen", return_value=io.BytesIO(json.dumps(release).encode())):
            with self.assertRaisesRegex(ValueError, "stable release"):
                check.fetch_release("latest")

    def test_download_is_verified_and_layout_changes_fail(self):
        for name, expected in [("package/bin/codex", None), ("new-layout/codex", "layout changed")]:
            with self.subTest(name=name):
                data = io.BytesIO()
                with tarfile.open(fileobj=data, mode="w:gz") as archive:
                    entry = tarfile.TarInfo(name)
                    entry.mode, entry.size = 0o755, 4
                    archive.addfile(entry, io.BytesIO(b"test"))
                content = data.getvalue()
                asset = {"browser_download_url": "https://github.com/openai/codex/test", "size": len(content),
                         "digest": "sha256:" + hashlib.sha256(content).hexdigest()}
                target = self.root / ("valid" if expected is None else "changed")
                target.mkdir()
                with mock.patch.object(check.urllib.request, "urlopen", return_value=io.BytesIO(content)):
                    if expected:
                        with self.assertRaisesRegex(ValueError, expected):
                            check.unpack_release(asset, target)
                    else:
                        self.assertTrue(check.unpack_release(asset, target).is_file())
                asset["digest"] = "sha256:" + "0" * 64
                with mock.patch.object(check.urllib.request, "urlopen", return_value=io.BytesIO(content)):
                    with self.assertRaisesRegex(ValueError, "SHA-256 mismatch"):
                        check.unpack_release(asset, target)


if __name__ == "__main__":
    unittest.main()
