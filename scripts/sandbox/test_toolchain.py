"""Offline regressions for the toolchain resolver (toolchain.py)."""

import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

import toolchain as tc

HELPER = Path(tc.__file__)


class ToolchainTests(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.tmp = Path(self._tmp.name)
        self.goroot = self.tmp / "goroot"
        self.mod = self.tmp / "mod"
        self.goroot.mkdir()
        self.mod.mkdir()
        self.probe = self.tmp / "probe.py"
        self.probe.write_text(
            "import json\n"
            f"print(json.dumps({{'GOROOT': {str(self.goroot)!r}, 'GOMODCACHE': {str(self.mod)!r}}}))\n"
        )
        self.config = self.tmp / "toolchains.json"

    def tearDown(self):
        self._tmp.cleanup()

    def entry(self, **overrides):
        base = {
            "probe": {"command": sys.executable, "args": [str(self.probe)]},
            "mounts": [
                {"path": "{GOROOT}", "mode": "ro"},
                {"path": "{GOMODCACHE}", "mode": "ro"},
            ],
            "env": {"GOROOT": "{GOROOT}", "GOMODCACHE": "{GOMODCACHE}"},
            "path_prepend": ["{GOROOT}/bin"],
            "require": ["GOROOT", "GOMODCACHE"],
        }
        base.update(overrides)
        return base

    def write(self, config):
        self.config.write_text(json.dumps(config))
        return self.config

    def invoke(self, *names, config=None, extra=()):
        config = config or self.write({"go": self.entry()})
        return subprocess.run(
            [sys.executable, str(HELPER), "--config", str(config), "--name", ",".join(names), *extra],
            capture_output=True,
            text=True,
            timeout=60,
        )

    def refused(self, *names, config=None, extra=(), reason=None):
        result = self.invoke(*names, config=config, extra=extra)
        self.assertEqual(result.returncode, 2, result.stderr)
        self.assertTrue(result.stderr.startswith("[agent-in-podman] --toolchain: "), result.stderr)
        if reason:
            self.assertIn(reason, result.stderr)
        return result

    # --- config loading -------------------------------------------------

    def test_missing_config_is_a_refusal(self):
        self.refused("go", config=self.tmp / "absent.json", reason="cannot read toolchain config")

    def test_bad_json_is_a_refusal(self):
        self.config.write_text("{not json")
        self.refused("go", config=self.config, reason="not valid JSON")

    def test_config_must_be_an_object(self):
        self.refused("go", config=self.write([]), reason="keyed by toolchain name")

    # --- names ----------------------------------------------------------

    def test_unknown_toolchain(self):
        self.refused("rust", reason="unknown toolchain: rust")

    def test_empty_name(self):
        self.refused("go,,rust", reason="empty toolchain name")

    # --- probe ----------------------------------------------------------

    def test_probe_command_missing(self):
        self.refused("go", config=self.write({"go": self.entry(probe={"command": "/nonexistent/probe"})}),
                     reason="probe command not found")

    def test_probe_non_json(self):
        bad = self.tmp / "bad.py"
        bad.write_text("print('not json')\n")
        self.refused("go", config=self.write({"go": self.entry(probe={"command": sys.executable, "args": [str(bad)]})}),
                     reason="not JSON")

    def test_probe_nonzero_exit(self):
        bad = self.tmp / "fail.py"
        bad.write_text("import sys\nsys.exit(3)\n")
        self.refused("go", config=self.write({"go": self.entry(probe={"command": sys.executable, "args": [str(bad)]})}),
                     reason="probe exited 3")

    def test_probe_value_must_be_a_string(self):
        bad = self.tmp / "num.py"
        bad.write_text("import json\nprint(json.dumps({'GOROOT': 1, 'GOMODCACHE': '/m'}))\n")
        self.refused("go", config=self.write({"go": self.entry(probe={"command": sys.executable, "args": [str(bad)]})}),
                     reason="is not a string")

    # --- placeholders ---------------------------------------------------

    def test_unknown_placeholder(self):
        self.refused("go", config=self.write({"go": self.entry(env={"X": "{NOPE}"})}),
                     reason="unknown placeholder {NOPE}")

    def empty_probe(self):
        empty = self.tmp / "empty.py"
        empty.write_text("import json\nprint(json.dumps({'GOROOT': '', 'GOMODCACHE': '/m'}))\n")
        return {"command": sys.executable, "args": [str(empty)]}

    def test_empty_required_placeholder(self):
        # require is checked before substitution, so this is the message.
        self.refused("go", config=self.write({"go": self.entry(probe=self.empty_probe())}),
                     reason="requires {GOROOT}")

    def test_empty_placeholder_in_env(self):
        config = self.write({"go": self.entry(probe=self.empty_probe(), require=[], env={"X": "{GOROOT}"})})
        self.refused("go", config=config, reason="is empty")

    def test_require_missing(self):
        self.refused("go", config=self.write({"go": self.entry(require=["GOROOT", "GOCACHE"])}),
                     reason="requires {GOCACHE}")

    def test_home_placeholder(self):
        config = self.write({"go": self.entry(env={"H": "{HOME}"})})
        result = self.invoke("go", config=config)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("--env", result.stdout.split("\0"))

    # --- mounts ---------------------------------------------------------

    def test_relative_mount_is_refused(self):
        self.refused("go", config=self.write({"go": self.entry(mounts=[{"path": "rel", "mode": "ro"}])}),
                     reason="must be absolute")

    def test_colon_mount_is_refused(self):
        self.refused("go", config=self.write({"go": self.entry(mounts=[{"path": "/a:b", "mode": "ro"}])}),
                     reason="colon or newline")

    def test_create_with_ro_is_refused(self):
        self.refused("go", config=self.write({"go": self.entry(mounts=[{"path": "/tmp/x", "mode": "ro", "create": True}])}),
                     reason="create:true with mode ro")

    def test_missing_mount_is_refused(self):
        self.refused("go", config=self.write({"go": self.entry(mounts=[{"path": "/nonexistent/toolchain", "mode": "ro"}])}),
                     reason="does not exist")

    def test_same_path_both_modes_is_refused(self):
        config = self.write({"go": self.entry(mounts=[
            {"path": "{GOROOT}", "mode": "ro"},
            {"path": "{GOROOT}", "mode": "rw"},
        ])})
        self.refused("go", config=config, reason="requested both")

    def test_same_path_same_mode_is_deduped(self):
        config = self.write({"go": self.entry(mounts=[
            {"path": "{GOROOT}", "mode": "ro"},
            {"path": "{GOROOT}", "mode": "ro"},
        ])})
        result = self.invoke("go", config=config)
        self.assertEqual(result.returncode, 0, result.stderr)
        tokens = result.stdout.split("\0")
        self.assertEqual(tokens.count("-v"), 1)

    def test_parent_child_same_mode_is_allowed(self):
        # GOROOT lives under GOMODCACHE on the host; both ro is the shipped case.
        self.mod.mkdir(exist_ok=True)
        inner = self.mod / "golang"
        inner.mkdir()
        self.probe.write_text(
            "import json\n"
            f"print(json.dumps({{'GOROOT': {str(inner)!r}, 'GOMODCACHE': {str(self.mod)!r}}}))\n"
        )
        result = self.invoke("go")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.split("\0").count("-v"), 2)

    def test_parent_child_different_mode_is_refused(self):
        inner = self.mod / "golang"
        inner.mkdir()
        self.probe.write_text(
            "import json\n"
            f"print(json.dumps({{'GOROOT': {str(inner)!r}, 'GOMODCACHE': {str(self.mod)!r}}}))\n"
        )
        config = self.write({"go": self.entry(mounts=[
            {"path": "{GOROOT}", "mode": "rw"},
            {"path": "{GOMODCACHE}", "mode": "ro"},
        ])})
        self.refused("go", config=config, reason="is inside")

    def test_create_makes_the_directory(self):
        cache = self.tmp / "cache" / "go-build"
        config = self.write({"go": self.entry(mounts=[{"path": str(cache), "mode": "rw", "create": True}])})
        result = self.invoke("go", config=config)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(cache.is_dir())

    # --- env / PATH -----------------------------------------------------

    def test_env_conflict_between_toolchains(self):
        config = self.write({
            "go": self.entry(env={"CC": "go-cc"}),
            "rust": self.entry(env={"CC": "rust-cc"}),
        })
        self.refused("go,rust", config=config, reason="different values")

    def test_one_path_token_for_two_toolchains(self):
        config = self.write({
            "go": self.entry(path_prepend=["{GOROOT}/bin"]),
            "rust": self.entry(path_prepend=["/rust/bin"]),
        })
        result = self.invoke("go,rust", config=config, extra=("--container-path", "/base"))
        self.assertEqual(result.returncode, 0, result.stderr)
        tokens = result.stdout.split("\0")
        paths = [t for t in tokens if t.startswith("PATH=")]
        self.assertEqual(len(paths), 1, tokens)
        self.assertEqual(paths[0], f"PATH={self.goroot}/bin:/rust/bin:/base")

    def test_argv_is_nul_separated(self):
        result = self.invoke("go")
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(result.stdout.endswith("\0"))
        tokens = result.stdout.split("\0")[:-1]
        self.assertIn("-v", tokens)
        self.assertIn("--env", tokens)
        self.assertIn(f"GOROOT={self.goroot}", tokens)

    # --- domains --------------------------------------------------------

    def test_domains_are_printed(self):
        config = self.write({"go": self.entry(allow_domains=["proxy.golang.org", "sum.golang.org"])})
        result = self.invoke("go", config=config, extra=("--domains",))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), "proxy.golang.org,sum.golang.org")

    def test_domains_dedupe_across_toolchains(self):
        config = self.write({
            "go": self.entry(allow_domains=["a.example"]),
            "rust": self.entry(allow_domains=["a.example", "b.example"]),
        })
        result = self.invoke("go,rust", config=config, extra=("--domains",))
        self.assertEqual(result.stdout.strip(), "a.example,b.example")

    def test_bad_domain_is_refused(self):
        config = self.write({"go": self.entry(allow_domains=["not a host"])})
        self.refused("go", config=config, extra=("--domains",), reason="not a host name")

    def test_domains_mode_does_not_run_the_probe(self):
        bad = self.tmp / "fail.py"
        bad.write_text("import sys\nsys.exit(3)\n")
        config = self.write({"go": self.entry(
            probe={"command": sys.executable, "args": [str(bad)]},
            allow_domains=["example.com"],
        )})
        result = self.invoke("go", config=config, extra=("--domains",))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), "example.com")

    def test_no_domains_prints_an_empty_line(self):
        result = self.invoke("go", extra=("--domains",))
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(result.stdout.strip(), "")


if __name__ == "__main__":
    unittest.main()
