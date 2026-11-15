#!/usr/bin/env python3
"""Refuse lines that would publish this machine's environment.

    python3 scripts/check_local_identifiers.py [<base>..<head>]   (default origin/main..HEAD)

The repository is public. Agents write from inside their environment, so a
worktree path, a hostname from `ls` output or a LAN address pasted as an
example lands in a plan, a skill or a test unless something stops it. This
checks only the lines a range ADDS, and the range's commit messages (what is
already there was scrubbed once and is not re-litigated on every landing), and
fails on:

- this machine's own identifiers, read at run time and never written down:
  the login name, the short host name, its non-loopback IPv4 addresses and the
  address of the harness server it talks to (HARNESS_SERVER_CID);
- private LAN shapes: 192.168.x.x and 172.16-31.x.x (10/8 is this repo's lab
  and test network, and 192.0.2.0/24 the documentation range, so both pass);
- a home directory with a real-looking name (/home/<name>, /Users/<name>,
  C:\\Users\\<name>); placeholders such as user, u, foo, someone pass.

A line that must carry one says `local-identifier-ok` on the same line.
"""
from __future__ import annotations

import getpass
import os
import re
import socket
import subprocess
import sys

MARKER = "local-identifier-ok"
PLACEHOLDER_NAMES = {"user", "u", "x", "foo", "bar", "someone", "op", "me", "you", "name",
                     "node", "runner", "root", "...", "<user>", "<name>", "username", "example", "pi"}
GENERIC = {"user", "root", "runner", "localhost", "ubuntu", "admin", "node", ""}

_OCT = r"(?:25[0-5]|2[0-4]\d|1\d\d|[1-9]?\d)"
PRIVATE_LAN = re.compile(r"\b(?:192\.168|172\.(?:1[6-9]|2\d|3[01]))\." + _OCT + r"\." + _OCT + r"\b")
HOME = re.compile(r"(?:/home/|/Users/|\\Users\\)([A-Za-z0-9_.<>-]+)")


def local_identity(user: str | None = None, host: str | None = None,
                   addrs: list[str] | None = None, server: str | None = None) -> set[str]:
    """The strings that name THIS machine, collected at run time."""
    user = getpass.getuser() if user is None else user
    host = socket.gethostname() if host is None else host
    if addrs is None:
        addrs = _local_addrs()
    if server is None:
        server = os.environ.get("HARNESS_SERVER_CID", "")
    ids = set()
    for v in (user, host.split(".")[0]):
        if v and v.lower() not in GENERIC and len(v) >= 3:
            ids.add(v)
    ids |= {a for a in addrs if a and not a.startswith("127.")}
    m = re.match(r"^[a-z]+:([0-9.]+):", server)
    if m and not m.group(1).startswith("127."):
        ids.add(m.group(1))
    return ids


def _local_addrs() -> list[str]:
    try:
        out = subprocess.run(["ip", "-4", "-o", "addr"], capture_output=True, text=True, timeout=5).stdout
    except (OSError, subprocess.SubprocessError):
        return []
    return re.findall(r"inet (\d+\.\d+\.\d+\.\d+)", out)


def findings(line: str, local: set[str]) -> list[str]:
    if MARKER in line:
        return []
    out = []
    for ident in local:
        if re.search(r"(?<![A-Za-z0-9])" + re.escape(ident) + r"(?![A-Za-z0-9])", line):
            out.append(f"this machine's identifier {ident!r}")
    for m in PRIVATE_LAN.finditer(line):
        out.append(f"a private LAN address {m.group(0)}")
    for m in HOME.finditer(line):
        if m.group(1).lower() not in PLACEHOLDER_NAMES:
            out.append(f"a named home directory {m.group(0)}")
    return out


def added_lines(diff: str):
    """(path, line number, text) of each added line in a unified diff."""
    path, n = None, 0
    for line in diff.splitlines():
        if line.startswith("+++ "):
            path = line[6:] if line.startswith("+++ b/") else None
        elif line.startswith("@@"):
            m = re.search(r"\+(\d+)", line)
            n = int(m.group(1)) if m else 0
        elif line.startswith("+") and path:
            yield path, n, line[1:]
            n += 1
        elif not line.startswith("-") and path and not line.startswith("\\"):
            n += 1


def main(argv: list[str]) -> int:
    rng = argv[0] if argv else "origin/main..HEAD"
    p = subprocess.run(["git", "diff", "-U0", "--no-color", "--no-ext-diff", rng],
                       capture_output=True, text=True)
    if p.returncode != 0:
        print(f"check_local_identifiers: git diff {rng}: {p.stderr.strip()}", file=sys.stderr)
        return 2
    local = local_identity()
    bad = 0
    for path, n, text in added_lines(p.stdout):
        for what in findings(text, local):
            print(f"{path}:{n}: {what}")
            bad += 1
    # Commit messages are published with the tree.
    log = subprocess.run(["git", "log", "--format=%H%n%B%n\x1e", rng], capture_output=True, text=True)
    for block in log.stdout.split("\x1e"):
        lines = block.strip("\n").splitlines()
        if not lines:
            continue
        for text in lines[1:]:
            for what in findings(text, local):
                print(f"commit {lines[0][:12]} message: {what}")
                bad += 1
    if bad:
        print(f"check_local_identifiers: {bad} line(s) would publish this environment. Replace "
              "them (/home/user, host1, 192.0.2.x), or mark a deliberate one with "
              f"`{MARKER}` on the same line.", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
