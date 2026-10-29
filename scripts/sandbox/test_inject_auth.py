"""Offline regressions for inject_auth.py (agent-in-podman.sh --inject-auth)."""

import contextlib
import io
import json
from pathlib import Path
import tempfile
import unittest

import inject_auth as ia


class PiTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.dir = Path(temp.name)
        self.write("settings.json", {"defaultProvider": "openrouter"})
        self.write("auth.json", {"openrouter": {"type": "api_key", "key": "k-1"}})
        self.out = self.dir / "out.json"

    def write(self, name, obj):
        (self.dir / name).write_text(json.dumps(obj))

    def run_main(self):
        stdout, stderr = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            rc = ia.main(["pi", "--agent-dir", str(self.dir), "--config-out", str(self.out), "--port", "18081"])
        return rc, stdout.getvalue(), stderr.getvalue()

    def test_builtin_provider(self):
        rc, out, err = self.run_main()
        self.assertEqual(rc, 0, err)
        self.assertEqual(out, "https://openrouter.ai\nk-1\n")
        self.assertEqual(json.loads(self.out.read_text()),
                         {"providers": {"openrouter": {"baseUrl": "http://127.0.0.1:18081/api/v1",
                                                       "apiKey": "sandbox-placeholder"}}})
        self.assertNotIn("k-1", self.out.read_text())

    def test_host_models_json_is_kept_and_its_base_url_used(self):
        self.write("models.json", {"providers": {
            "openrouter": {"baseUrl": "https://gw.example/or/v1", "models": [{"id": "m"}]},
            "local": {"baseUrl": "http://127.0.0.1:8080/v1", "api": "openai-completions"}}})
        rc, out, err = self.run_main()
        self.assertEqual(rc, 0, err)
        self.assertEqual(out.splitlines()[0], "https://gw.example")
        cfg = json.loads(self.out.read_text())
        self.assertEqual(cfg["providers"]["openrouter"]["baseUrl"], "http://127.0.0.1:18081/or/v1")
        self.assertEqual(cfg["providers"]["openrouter"]["models"], [{"id": "m"}])
        self.assertEqual(cfg["providers"]["local"]["baseUrl"], "http://127.0.0.1:8080/v1")

    def assertRefused(self, why):
        rc, out, err = self.run_main()
        self.assertEqual(rc, 2)
        self.assertEqual(out, "")
        self.assertIn(why, err)
        self.assertFalse(self.out.exists())

    def test_oauth_refused(self):
        self.write("auth.json", {"openrouter": {"type": "oauth", "access": "a", "refresh": "r"}})
        self.assertRefused("only an api_key")

    def test_no_default_provider_refused(self):
        self.write("settings.json", {})
        self.assertRefused("defaultProvider")

    def test_provider_without_credential_refused(self):
        self.write("auth.json", {"anthropic": {"type": "api_key", "key": "x"}})
        self.assertRefused("no credential for openrouter")

    def test_unknown_provider_refused(self):
        self.write("settings.json", {"defaultProvider": "mystery"})
        self.write("auth.json", {"mystery": {"type": "api_key", "key": "x"}})
        self.assertRefused("no known base URL")

    def test_model_level_base_url_refused(self):
        # It would send that model's requests past the injector, with the
        # placeholder, to wherever the host config points.
        self.write("models.json", {"providers": {"openrouter": {"models": [{"id": "m", "baseUrl": "https://x/v1"}]}}})
        self.assertRefused("bypass the injector")

    def test_plain_http_base_refused(self):
        self.write("models.json", {"providers": {"openrouter": {"baseUrl": "http://gw.example/v1"}}})
        self.assertRefused("not https")

    def test_missing_auth_file_refused(self):
        (self.dir / "auth.json").unlink()
        self.assertRefused("no pi credential file")


class ClaudeTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        self.creds = Path(temp.name) / ".credentials.json"

    def run_main(self):
        stdout, stderr = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            rc = ia.main(["claude", "--credentials", str(self.creds)])
        return rc, stdout.getvalue(), stderr.getvalue()

    def test_env_carries_the_tier_and_no_token(self):
        self.creds.write_text(json.dumps({"claudeAiOauth": {
            "accessToken": "a-secret", "refreshToken": "r-secret", "subscriptionType": "max",
            "rateLimitTier": "default_claude_max_5x"}}))
        rc, out, err = self.run_main()
        self.assertEqual(rc, 0, err)
        self.assertEqual(out, "CLAUDE_CODE_SUBSCRIPTION_TYPE=max\nCLAUDE_CODE_RATE_LIMIT_TIER=default_claude_max_5x\n")
        self.assertNotIn("secret", out)

    def test_missing_tier_fields_are_left_out(self):
        self.creds.write_text(json.dumps({"claudeAiOauth": {"accessToken": "a"}}))
        rc, out, _ = self.run_main()
        self.assertEqual((rc, out), (0, ""))

    def test_refusals(self):
        for body, why in ((None, "no claude credential file"), ("not json", "cannot read"),
                          (json.dumps({"other": {}}), "no claudeAiOauth login")):
            with self.subTest(why=why):
                if body is None:
                    self.creds.unlink(missing_ok=True)
                else:
                    self.creds.write_text(body)
                rc, out, err = self.run_main()
                self.assertEqual(rc, 2)
                self.assertEqual(out, "")
                self.assertIn(why, err)


if __name__ == "__main__":
    unittest.main()
