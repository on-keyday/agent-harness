"""Offline regressions for config_sync.py (credential injection's settings sync)."""

import io
import json
import os
from pathlib import Path
import tempfile
import unittest

import config_sync as cs

KEYS = ["model", "modelSettings"]


class SyncTests(unittest.TestCase):
    def setUp(self):
        temp = tempfile.TemporaryDirectory()
        self.addCleanup(temp.cleanup)
        d = Path(temp.name)
        self.copy, self.host, self.state = d / "copy.json", d / "host.json", d / "state.json"
        start = {"hooks": {"Stop": []}, "model": "opus", "effortLevel": "xhigh"}
        for p in (self.copy, self.host):
            p.write_text(json.dumps(start))
        self.host.chmod(0o600)
        cs.init(self.copy, self.state, KEYS)
        self.log = io.StringIO()

    def edit(self, path, **kv):
        d = json.loads(path.read_text())
        for k, v in kv.items():
            if v is None:
                d.pop(k, None)
            else:
                d[k] = v
        path.write_text(json.dumps(d))

    def sync(self):
        return cs.sync(self.copy, self.host, self.state, KEYS, self.log)

    def test_a_key_the_container_changed_reaches_the_host(self):
        self.edit(self.copy, model="sonnet", modelSettings={"claude-sonnet-5-5": {"effortLevel": "low"}})
        self.assertTrue(self.sync())
        host = json.loads(self.host.read_text())
        self.assertEqual(host["model"], "sonnet")
        self.assertEqual(host["modelSettings"], {"claude-sonnet-5-5": {"effortLevel": "low"}})
        self.assertEqual(oct(self.host.stat().st_mode & 0o777), "0o600")

    def test_other_keys_never_leave_the_container(self):
        self.edit(self.copy, hooks={"Stop": [{"command": "evil"}]}, env={"X": "1"})
        self.assertFalse(self.sync())
        host = json.loads(self.host.read_text())
        self.assertEqual(host["hooks"], {"Stop": []})
        self.assertNotIn("env", host)

    def test_a_host_change_the_container_did_not_touch_is_kept(self):
        self.edit(self.host, model="haiku")
        self.assertFalse(self.sync())
        self.assertEqual(json.loads(self.host.read_text())["model"], "haiku")

    def test_each_change_is_written_once(self):
        self.edit(self.copy, model="sonnet")
        self.assertTrue(self.sync())
        self.edit(self.host, model="haiku")  # the operator changes it back on the host
        self.assertFalse(self.sync())
        self.assertEqual(json.loads(self.host.read_text())["model"], "haiku")

    def test_a_removed_key_is_removed(self):
        self.edit(self.copy, model=None)
        self.assertTrue(self.sync())
        self.assertNotIn("model", json.loads(self.host.read_text()))

    def test_a_half_written_copy_is_skipped(self):
        self.copy.write_text('{"model": "son')
        self.assertFalse(self.sync())
        self.assertEqual(json.loads(self.host.read_text())["model"], "opus")


if __name__ == "__main__":
    unittest.main()
