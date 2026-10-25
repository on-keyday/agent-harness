"""Release-source adapters and reporting, with all remote IO mocked."""

import base64
import contextlib
import io
import json
import unittest
from unittest import mock

import check_agent_releases as check


def npm_release(version="1.2.3"):
    return {"version": version, "dist": {"tarball": "https://registry.npmjs.org/vendor/-/agent.tgz",
            "integrity": "sha512-" + base64.b64encode(b"x" * 64).decode()}}


class AgentReleaseTests(unittest.TestCase):
    def test_claude_pins_native_package_to_the_cli_release(self):
        cli = npm_release()
        cli["optionalDependencies"] = {"@anthropic-ai/claude-code-linux-x64": "1.2.3"}
        with mock.patch.object(check, "npm_metadata", side_effect=[cli, npm_release()]) as fetch:
            result = check.upstream("claude", "x64")
        self.assertEqual(fetch.call_args.args, ("@anthropic-ai/claude-code-linux-x64", "1.2.3"))
        self.assertEqual(result["version"], "1.2.3")

    def test_pi_uses_the_official_package_scope(self):
        with mock.patch.object(check, "npm_metadata", return_value=npm_release()) as fetch:
            result = check.upstream("pi", "arm64")
        fetch.assert_called_once_with("@earendil-works/pi-coding-agent")
        self.assertEqual(result["binary_name"], "cli.js")

    def test_agy_requires_the_official_payload_and_checksum(self):
        manifest = {"version": "1.2.3", "url": "https://storage.googleapis.com/antigravity-public/antigravity-cli/test.tar.gz", "sha512": "a" * 128}
        with mock.patch.object(check, "fetch_json", return_value=manifest) as fetch:
            self.assertEqual(check.upstream("agy", "x64")["version"], "1.2.3")
            self.assertTrue(fetch.call_args.args[0].endswith("linux_amd64.json"))
            manifest["url"] = "https://example.com/agent.tar.gz"
            with self.assertRaisesRegex(ValueError, "unexpected Antigravity"):
                check.upstream("agy", "x64")

    def test_opencode_selects_baseline_and_checks_digest(self):
        release = {"tag_name": "v1.2.3", "html_url": "https://github.com/anomalyco/opencode/releases/tag/v1.2.3", "assets": [
            {"name": "opencode-linux-x64-baseline.tar.gz", "digest": "sha256:" + "a" * 64,
             "browser_download_url": "https://github.com/anomalyco/opencode/releases/download/v1.2.3/opencode-linux-x64-baseline.tar.gz"}]}
        with mock.patch.object(check, "fetch_json", return_value=release):
            self.assertEqual(check.upstream("opencode", "x64")["version"], "1.2.3")
            release["assets"][0]["digest"] = None
            with self.assertRaisesRegex(ValueError, "digest missing"):
                check.upstream("opencode", "x64")

    def test_metadata_success_does_not_claim_runtime_coverage(self):
        with mock.patch.object(check, "upstream", return_value={"agent": "pi", "source": "test", "version": "1.2.3"}), mock.patch.object(check.codex, "smoke_bundle") as smoke:
            result = check.check_agent("pi", "x64", "image", False)
        self.assertEqual(result["status"], "ok")
        self.assertFalse(result["runtime_tested"])
        smoke.assert_not_called()

    def test_one_upstream_failure_does_not_hide_other_agents(self):
        def inspect(agent, *args):
            return {"agent": agent, "status": "error" if agent == "agy" else "ok"}
        output = io.StringIO()
        with mock.patch.object(check, "check_agent", side_effect=inspect), contextlib.redirect_stdout(output):
            code = check.main(["--agent", "all"])
        self.assertEqual(code, 1)
        self.assertEqual([row["agent"] for row in json.loads(output.getvalue())["checks"]], list(check.AGENTS))

    def test_npm_download_host_and_integrity_are_checked(self):
        release = npm_release()
        self.assertTrue(check.npm_asset(release)["digest"].startswith("sha512:"))
        release["dist"]["tarball"] = "https://example.com/agent.tgz"
        with self.assertRaisesRegex(ValueError, "unexpected npm"):
            check.npm_asset(release)


if __name__ == "__main__":
    unittest.main()
