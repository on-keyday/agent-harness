"""Namespace primitives: entering one, holding one open, and deciding whether
any of it can work on this host.

The lab needs no real root. `unshare --user --map-root-user` maps the caller to
uid 0 inside a fresh user namespace, and that carries CAP_NET_ADMIN over every
network namespace the user namespace owns — enough for veth, tc, iptables and
the per-namespace conntrack sysctls. Measured on Linux 7.1.3; the spec's
Evidence section has the commands.
"""

from __future__ import annotations

import os
import platform
import shutil
import subprocess
import time
from pathlib import Path

REQUIRED_TOOLS = ("ip", "tc", "iptables", "unshare", "nsenter")

# Runs inside a throwaway user namespace during preflight. Its namespace goes
# away when unshare exits, so there is nothing to clean up afterwards.
_PROBE_SH = (
    "ip link add pf0 type veth peer name pf1 && "
    "ip link set pf0 up && "
    "tc qdisc add dev pf0 root netem delay 1ms limit 100"
)


def nsenter_argv(userns_pid: int, netns_pid: int) -> list[str]:
    """The prefix that runs a command inside one of the lab's namespaces.

    `--preserve-credentials` is NOT optional. A user namespace created with
    `--map-root-user` has `/proc/PID/setgroups` set to "deny" — that is what
    makes the unprivileged `gid_map` write legal in the first place — so
    nsenter's default post-entry `setgroups()` can never succeed against it.
    Omitting the flag makes every entry fail with "setgroups failed: Operation
    not permitted", which names neither the namespace nor the flag.

    The two pids differ for `srv` and `cli`, whose network namespaces are held
    by their own processes, and coincide for `rtr`, whose holder also owns the
    user namespace.
    """
    return [
        "nsenter",
        "--preserve-credentials",
        f"--user=/proc/{userns_pid}/ns/user",
        f"--net=/proc/{netns_pid}/ns/net",
    ]


def unshare_holder_argv() -> list[str]:
    """The argv that creates the user namespace and the first network one.

    No `--fork`: unshare(2) then exec, in one process, so the pid Popen reports
    IS the holder. No pid namespace is unshared either, so that pid is
    host-visible and usable as `/proc/PID/ns/...` from outside.
    """
    return ["unshare", "--user", "--map-root-user", "--net"]


def _read_sysctl(name: str) -> int | None:
    path = Path("/proc/sys") / name.replace(".", "/")
    try:
        return int(path.read_text(encoding="utf-8").strip())
    except (OSError, ValueError):
        return None


def _probe_netem(run) -> str:
    """Empty string if a user namespace can create a veth with netem on it,
    otherwise the underlying error."""
    proc = run(
        [*unshare_holder_argv(), "sh", "-c", _PROBE_SH],
        capture_output=True,
        text=True,
        encoding="utf-8",
        errors="replace",
        timeout=30,
    )
    if proc.returncode == 0:
        return ""
    return (proc.stderr or proc.stdout or "").strip() or f"exit {proc.returncode}"


def preflight_problems(run=subprocess.run) -> list[str]:
    """Everything wrong with this host, in the order worth reporting.

    Returns a list rather than raising, so `up` reports all of them at once:
    learning about a missing `tc` only after installing `iproute2` is a second
    round trip for nothing.

    The probe runs LAST and only when the cheap checks pass. It spawns a
    process, and a probe failure on a host that has no `unshare` would report
    the wrong cause.
    """
    system = platform.system()
    if system != "Linux":
        return [
            f"netem-lab needs Linux; this is {system}. Network namespaces, tc "
            "and iptables are Linux interfaces with no equivalent here, so "
            "there is nothing to fall back to."
        ]

    problems: list[str] = []
    maxns = _read_sysctl("user.max_user_namespaces")
    if maxns is None:
        problems.append(
            "cannot read sysctl user.max_user_namespaces; without user "
            "namespaces the lab has no way to get CAP_NET_ADMIN unprivileged"
        )
    elif maxns == 0:
        problems.append(
            "user.max_user_namespaces is 0: unprivileged user namespaces are "
            "disabled on this host. `sudo sysctl -w "
            "user.max_user_namespaces=15000` enables them (and a file under "
            "/etc/sysctl.d/ makes it survive a reboot)."
        )

    missing = [t for t in REQUIRED_TOOLS if shutil.which(t) is None]
    if missing:
        problems.append(f"missing from PATH: {', '.join(missing)}")

    if problems:
        return problems

    err = _probe_netem(run)
    if err:
        problems.append(
            "a user namespace could not create a veth with netem attached: "
            f"{err}\n"
            "If this kernel does not autoload qdisc modules from inside a user "
            "namespace, `sudo modprobe sch_netem sch_htb` once fixes it for "
            "good. That is the only step in this tool that wants root, and it "
            "is not needed on every kernel."
        )
    return problems


def spawn_holder(argv: list[str], log: Path) -> subprocess.Popen:
    """Start a detached namespace holder with its output to `log`.

    Detached (`start_new_session`) because it must outlive `up`: the namespaces
    are addressed by this process's pid from every later invocation.
    """
    fh = log.open("wb")
    return subprocess.Popen(
        [str(a) for a in argv],
        stdout=fh,
        stderr=subprocess.STDOUT,
        stdin=subprocess.DEVNULL,
        start_new_session=True,
        env=os.environ.copy(),
    )


def current_netns() -> str:
    return os.readlink("/proc/self/ns/net")


def wait_for_namespace(pid: int, differs_from: str, timeout: float = 5.0) -> None:
    """Block until `pid`'s network namespace is distinct from `differs_from`.

    Popen returns as soon as the child is forked, which is BEFORE unshare(2)
    runs inside it. Reading /proc/PID/ns/net in that window returns the PARENT's
    namespace — and a veth end moved there would land in the host's network
    namespace, which is the one thing this tool must never touch.
    """
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            if os.readlink(f"/proc/{pid}/ns/net") != differs_from:
                return
        except OSError:
            pass
        time.sleep(0.02)
    raise TimeoutError(
        f"pid {pid} never entered a network namespace of its own within "
        f"{timeout}s; refusing to continue rather than risk operating on the "
        f"host's namespace"
    )
