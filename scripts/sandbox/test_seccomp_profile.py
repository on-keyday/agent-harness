"""Offline regressions for the derived seccomp profile (seccomp_profile.py)."""

import errno
import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

import seccomp_profile as sp

USERNS = sp.CLONE_NEWUSER


def base():
    return {
        "defaultAction": "SCMP_ACT_ERRNO",
        "defaultErrnoRet": 38,
        "archMap": [{"architecture": "SCMP_ARCH_X86_64"}],
        "syscalls": [
            {"names": ["read", "clone", "clone3", "unshare", "setns"], "action": "SCMP_ACT_ALLOW", "args": []},
            {"names": ["unshare", "mount"], "action": "SCMP_ACT_ALLOW", "args": [],
             "includes": {"caps": ["CAP_SYS_ADMIN"]}},
            {"names": ["clone"], "action": "SCMP_ACT_ERRNO", "errnoRet": 1, "args": []},
        ],
    }


class DenyUsernsTests(unittest.TestCase):
    def setUp(self):
        self.derived = sp.deny_userns(base())
        self.rules = self.derived["syscalls"]

    def rules_naming(self, name):
        return [r for r in self.rules if name in r["names"]]

    def test_the_rest_of_the_profile_is_kept(self):
        self.assertEqual({k: v for k, v in self.derived.items() if k != "syscalls"},
                         {k: v for k, v in base().items() if k != "syscalls"})
        self.assertIn({"names": ["read", "setns"], "action": "SCMP_ACT_ALLOW", "args": []}, self.rules)
        self.assertIn({"names": ["mount"], "action": "SCMP_ACT_ALLOW", "args": [],
                       "includes": {"caps": ["CAP_SYS_ADMIN"]}}, self.rules)

    def test_only_the_appended_rules_decide_clone_and_unshare(self):
        # Including the cap-gated ALLOW: with CAP_SYS_ADMIN it would still let
        # CLONE_NEWUSER through.
        for name in ("clone", "unshare"):
            with self.subTest(name=name):
                rules = self.rules_naming(name)
                self.assertEqual(len(rules), 2)
                allow, deny = rules
                self.assertEqual(allow["action"], "SCMP_ACT_ALLOW")
                self.assertEqual(allow["args"], [{"index": 0, "value": USERNS, "valueTwo": 0, "op": "SCMP_CMP_MASKED_EQ"}])
                self.assertEqual(deny["action"], "SCMP_ACT_ERRNO")
                self.assertEqual(deny["errnoRet"], errno.EPERM)
                self.assertEqual(deny["args"], [{"index": 0, "value": USERNS, "valueTwo": USERNS, "op": "SCMP_CMP_MASKED_EQ"}])

    def test_clone3_falls_back_to_clone(self):
        self.assertEqual(self.rules_naming("clone3"),
                         [{"names": ["clone3"], "action": "SCMP_ACT_ERRNO", "errnoRet": errno.ENOSYS}])

    def test_cli_writes_the_profile_to_stdout(self):
        with tempfile.TemporaryDirectory() as d:
            src = Path(d) / "seccomp.json"
            src.write_text(json.dumps(base()))
            out = subprocess.run([sys.executable, str(Path(sp.__file__)), str(src)],
                                 capture_output=True, text=True, check=True, timeout=10)
        self.assertEqual(json.loads(out.stdout), self.derived)

    def test_cli_fails_on_an_unreadable_profile(self):
        result = subprocess.run([sys.executable, str(Path(sp.__file__)), "/nonexistent/seccomp.json"],
                                capture_output=True, text=True, timeout=10)
        self.assertNotEqual(result.returncode, 0)


if __name__ == "__main__":
    unittest.main()
