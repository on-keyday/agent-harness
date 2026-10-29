#!/usr/bin/env python3
"""Print podman's seccomp profile with user namespace creation denied.

podman's default profile allows clone, clone3 and unshare whatever flags they
carry, so a process in the sandbox could create user namespaces, nested ones
included (measured 2026-10-29, podman 6.0.1: two levels below the container,
uid 0 in each). Inside a user namespace an unprivileged process gets
CAP_SYS_ADMIN over it and reaches kernel code that is otherwise reachable only
by root. agent-in-podman.sh derives the profile with this helper before every
container start, unless it is given --allow-userns.

Usage: seccomp_profile.py BASE_PROFILE > DERIVED_PROFILE
"""

from __future__ import annotations

import errno
import json
import sys

CLONE_NEWUSER = 0x10000000
# clone and unshare take the namespace flags in argument 0 on x86_64 and
# aarch64, the architectures this kit runs on (s390x swaps clone's first two).
FLAGGED = ["clone", "unshare"]


def deny_userns(profile: dict) -> dict:
    """The same profile, with CLONE_NEWUSER refused with EPERM.

    Every rule naming one of these syscalls loses the name, so the three rules
    appended here are the only ones that decide them.
    """
    rules = []
    for rule in profile["syscalls"]:
        names = [n for n in rule["names"] if n not in FLAGGED and n != "clone3"]
        if names:
            rules.append({**rule, "names": names})
    flag = {"index": 0, "value": CLONE_NEWUSER, "op": "SCMP_CMP_MASKED_EQ"}
    rules += [
        {"names": FLAGGED, "action": "SCMP_ACT_ALLOW", "args": [{**flag, "valueTwo": 0}]},
        {"names": FLAGGED, "action": "SCMP_ACT_ERRNO", "errnoRet": errno.EPERM,
         "args": [{**flag, "valueTwo": CLONE_NEWUSER}]},
        # clone3 passes its flags in a struct seccomp cannot read. ENOSYS makes
        # glibc and other runtimes fall back to clone, where the rules above apply.
        {"names": ["clone3"], "action": "SCMP_ACT_ERRNO", "errnoRet": errno.ENOSYS},
    ]
    return {**profile, "syscalls": rules}


def main(argv: list[str]) -> int:
    if len(argv) != 1:
        print(__doc__.strip().splitlines()[-1], file=sys.stderr)
        return 2
    with open(argv[0], encoding="utf-8") as f:
        profile = json.load(f)
    json.dump(deny_userns(profile), sys.stdout)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
