#!/usr/bin/env python3
"""A shaped WAN path between harness-server and agent-runner, on one machine.

Three network namespaces under one user namespace: `srv` holds the server,
`cli` holds the runner, and `rtr` sits between them carrying the shaping, the
NAT and — when asked — a path-MTU black hole. No real root: `unshare --user
--map-root-user` supplies CAP_NET_ADMIN over the namespaces it owns.

Why this is not part of dummy-harness.py: that script starts the server and
the runner from ONE process on loopback, so both land in the same network
namespace and nothing can be inserted between them. Shaping loopback instead
would shape every other loopback user on the host, the operator's own terminal
sessions included.

Usage:
  netem-lab.py [--name N] up    [--profile P] [knobs...] [-- <runner flags>]
  netem-lab.py [--name N] env
  netem-lab.py [--name N] exec  {srv|rtr|cli} -- <cmd...>
  netem-lab.py [--name N] shape [--profile P] [knobs...]
  netem-lab.py [--name N] path  {1|2} {up|down}
  netem-lab.py [--name N] failover [--leg N] [--timeout S]
  netem-lab.py [--name N] show
  netem-lab.py [--name N] down

Design: docs/superpowers/specs/2026-08-31-netem-lab-design.md
"""

from __future__ import annotations

import argparse
import getpass
import importlib.util
import ipaddress
import json
import math
import os
import re
import statistics
import secrets
import shutil
import subprocess
import sys
import tempfile
import time
from pathlib import Path

_HERE = Path(__file__).resolve().parent
_SCRIPTS = _HERE.parent
sys.path.insert(0, str(_HERE))
sys.path.insert(0, str(_SCRIPTS))

import shaping  # noqa: E402  (path set above)
import nsutil  # noqa: E402

from bootstrap import ensure_venv  # noqa: E402  (stdlib-only, safe above venv)

ensure_venv()

# Only below here are we inside scripts/.venv. dummy-harness.py imports daemon,
# which imports psutil at MODULE level, so loading it above the bootstrap would
# make this script fail to start on a host without a system-wide psutil. Same
# ordering every entry script in scripts/ uses.

_DH_PATH = _SCRIPTS / "dummy-harness.py"


def _load_dummy_harness():
    """Borrow dummy-harness.py's pure helpers without modifying or copying it.

    Its filename has a hyphen, so it cannot be imported by name; this is the
    same importlib route scripts/test_dummy_harness.py already uses. Its own
    ensure_venv() returns immediately because ours already ran — the check is
    on sys.prefix, not on sys.executable (scripts/bootstrap.py).
    """
    spec = importlib.util.spec_from_file_location("dummy_harness", _DH_PATH)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


dh = _load_dummy_harness()


def die(msg: str) -> "NoReturn":  # type: ignore[valid-type]
    print(f"netem-lab: {msg}", file=sys.stderr)
    raise SystemExit(1)


def setup_err(msg: str) -> "NoReturn":  # type: ignore[valid-type]
    print(f"netem-lab: SETUP: {msg}", file=sys.stderr)
    raise SystemExit(2)


def state_dir() -> Path:
    """Where instance records live.

    dh.tmp_root() reads TMPDIR (POSIX) or TEMP (Windows) — deliberately NOT
    tempfile.gettempdir(), which consults TMP first. dummy-harness.py's `env`
    exports TMP set to its own instance directory, and sourcing that in a shell
    once made every later call resolve its state directory inside the instance:
    `down` reported nothing to stop and left a server and a runner running with
    their state file orphaned.
    """
    return dh.tmp_root() / f"harness-netem-{getpass.getuser()}"


def state_path(name: str) -> Path:
    return state_dir() / f"{name}.json"


def write_state(name: str, st: dict) -> None:
    state_dir().mkdir(parents=True, exist_ok=True)
    state_path(name).write_text(json.dumps(st, indent=2), encoding="utf-8")


def read_state(name: str) -> dict | None:
    path = state_path(name)
    if not path.is_file():
        return None
    return json.loads(path.read_text(encoding="utf-8"))


# Killed in this order: the workload first so it stops using the namespaces,
# then the holders. Killing the user-namespace holder does NOT cascade — the
# other holders are separate processes, and a namespace lives as long as any
# process remains in it — so every one is named here explicitly.
_KILL_ORDER = ("RUNNER_PID", "SERVER_PID", "CLI_PID", "SRV_PID", "USERNS_PID")


def cmd_down(name: str) -> int:
    st = read_state(name)
    if st is None:
        print("netem-lab: nothing to stop")
        return 0
    for key in _KILL_ORDER:
        pid = st.get(key)
        if pid:
            dh.kill_pid(int(pid))
    tmp = st.get("TMP")
    if tmp and Path(tmp).is_dir():
        shutil.rmtree(tmp, ignore_errors=True)
    state_path(name).unlink(missing_ok=True)
    print("netem-lab: stopped")
    return 0


def _add_knob_flags(p: argparse.ArgumentParser) -> None:
    """The shaping knobs, shared by `up` and `shape`.

    Every default is None, which resolve_knobs() reads as "not given on the
    command line". A default of 0 would erase a profile's value for anyone who
    did not repeat it.
    """
    p.add_argument("--profile", default=None)
    p.add_argument("--delay", dest="delay_ms", type=float, default=None)
    p.add_argument("--jitter", dest="jitter_ms", type=float, default=None)
    p.add_argument("--loss", dest="loss_pct", type=float, default=None)
    p.add_argument("--reorder", dest="reorder_pct", type=float, default=None)
    p.add_argument("--rate", default=None)
    p.add_argument("--limit", type=int, default=None)
    p.add_argument("--mtu", type=int, default=None)
    p.add_argument("--pmtu-blackhole", dest="pmtu_blackhole",
                   action="store_const", const=True, default=None)
    p.add_argument("--conntrack-udp-timeout", dest="conntrack_udp_timeout",
                   type=int, default=None)
    p.add_argument("--no-nat", dest="nat", action="store_const",
                   const=False, default=None)


_KNOB_KEYS = ("delay_ms", "jitter_ms", "loss_pct", "reorder_pct", "rate",
              "limit", "mtu", "pmtu_blackhole", "conntrack_udp_timeout", "nat")


def knobs_from_args(args) -> shaping.Knobs:
    overrides = {k: getattr(args, k) for k in _KNOB_KEYS}
    try:
        return shaping.resolve_knobs(args.profile, **overrides)
    except ValueError as e:
        die(str(e))


# Device names, (endpoint side, router side). The kernel caps an interface name
# at 15 bytes; these stay well under while still saying which side of which leg
# they are.
_DEV = {
    "srv": ("v-srv", "v-rtr-s"),
    "cli": ("v-cli", "v-rtr-c"),
}

# The optional SECOND server leg (`up --paths 2`), for testing a runner whose
# --server-cid lists more than one address. Kept out of _DEV because that map is
# keyed by NAMESPACE and every loop over it means "once per endpoint"; this is a
# second leg of ONE endpoint.
#
# Deliberately UNSHAPED and un-NAT'd: apply_shaping only touches the devices in
# _DEV, and the MASQUERADE rule names leg 1's device, so the second path carries
# no delay, loss or MTU narrowing and no address translation. It shows: a runner
# that has failed over appears at the CLIENT's own address rather than the
# router's. This leg exists to test reachability — whether a candidate list
# fails over — not to compare two shaped paths. Shaping both would need a
# per-leg knob set, which is a different feature and would make every existing
# measurement ambiguous about which path it was taken on.
_SRV_LEG2 = ("v-srv2", "v-rtr-s2")


def leg_devs(leg: int) -> tuple[str, str]:
    """(srv-side device, rtr-side device) for one server leg."""
    if leg not in (1, 2):
        die(f"leg must be 1 or 2, got {leg}")
    return _DEV["srv"] if leg == 1 else _SRV_LEG2

_PID_KEY = {"rtr": "USERNS_PID", "srv": "SRV_PID", "cli": "CLI_PID"}


def _addrs(cidr: str) -> tuple[str, str, int]:
    """(router address, endpoint address, prefix length) for one leg."""
    try:
        net = ipaddress.ip_network(cidr, strict=False)
    except ValueError as e:
        die(f"bad subnet {cidr!r}: {e}")
    hosts = list(net.hosts())
    if len(hosts) < 2:
        die(f"subnet {cidr} has fewer than two usable addresses")
    return str(hosts[0]), str(hosts[1]), net.prefixlen


def ns_env(st: dict) -> dict:
    """The environment for anything run inside the lab.

    main() scrubs HARNESS_* out of this process, which is right for what `up`
    spawns and wrong for everything run against the lab afterwards: the usual
    thing to run is harness-cli, and a scrubbed environment leaves it with no
    credential, so the server answers BadPsk — an error that reads as a WRONG
    psk rather than an absent one.

    This lives in the shared helper rather than in one verb. It was in cmd_exec
    alone at first, and `bench` — a second caller, added later — hit the same
    BadPsk on its first run. A guard belongs where every caller passes.
    """
    env = os.environ.copy()
    if st.get("HARNESS_PSK"):
        env["HARNESS_PSK"] = st["HARNESS_PSK"]
    return env


def ns_run(st: dict, ns: str, argv: list[str], check: bool = True, **kw):
    """Run one command inside one of the lab's namespaces."""
    prefix = nsutil.nsenter_argv(int(st["USERNS_PID"]), int(st[_PID_KEY[ns]]))
    kw.setdefault("env", ns_env(st))
    proc = subprocess.run(
        [*prefix, *[str(a) for a in argv]],
        capture_output=True, text=True, encoding="utf-8", errors="replace",
        **kw,
    )
    if check and proc.returncode != 0:
        die(f"in ns {ns}: {' '.join(str(a) for a in argv)}\n"
            f"{(proc.stderr or proc.stdout).strip()}")
    return proc


def build_network(knobs: shaping.Knobs, subnet_a: str, subnet_b: str,
                  tmp: Path, subnet_c: str | None = None) -> dict:
    """Create the three namespaces, wire them, address them, and route them.

    Returns the pid and address fields for the state dict. Every namespace is
    held by a detached process; the holder for `rtr` also owns the user
    namespace that owns the other two, which is what makes moving a veth end
    between them legal without real root.
    """
    host_ns = nsutil.current_netns()

    rtr = nsutil.spawn_holder(
        [*nsutil.unshare_holder_argv(), "sleep", "infinity"], tmp / "rtr.log")
    nsutil.wait_for_namespace(rtr.pid, host_ns)
    st: dict = {"USERNS_PID": rtr.pid}
    rtr_ns = os.readlink(f"/proc/{rtr.pid}/ns/net")

    for ns in ("srv", "cli"):
        # `unshare --net` run INSIDE the user namespace, so the new network
        # namespace is OWNED by it. One created outside would belong to the
        # host's user namespace and be unreachable without real root.
        holder = nsutil.spawn_holder(
            [*nsutil.nsenter_argv(rtr.pid, rtr.pid),
             "unshare", "--net", "sleep", "infinity"],
            tmp / f"{ns}.log")
        nsutil.wait_for_namespace(holder.pid, rtr_ns)
        st[_PID_KEY[ns]] = holder.pid

    for ns, cidr in (("srv", subnet_a), ("cli", subnet_b)):
        end_dev, rtr_dev = _DEV[ns]
        rtr_ip, end_ip, plen = _addrs(cidr)

        ns_run(st, "rtr", ["ip", "link", "add", rtr_dev,
                           "type", "veth", "peer", "name", end_dev])
        ns_run(st, "rtr", ["ip", "link", "set", end_dev,
                           "netns", st[_PID_KEY[ns]]])
        ns_run(st, "rtr", ["ip", "addr", "add", f"{rtr_ip}/{plen}",
                           "dev", rtr_dev])
        ns_run(st, "rtr", ["ip", "link", "set", rtr_dev, "up"])
        ns_run(st, ns, ["ip", "addr", "add", f"{end_ip}/{plen}", "dev", end_dev])
        ns_run(st, ns, ["ip", "link", "set", end_dev, "up"])
        ns_run(st, ns, ["ip", "link", "set", "lo", "up"])
        # A default route through the router, so each endpoint reaches the
        # other leg's subnet without a per-subnet route.
        ns_run(st, ns, ["ip", "route", "add", "default", "via", rtr_ip])
        st[f"{ns.upper()}_IP"] = end_ip
        st[f"{ns.upper()}_RTR_IP"] = rtr_ip

    if subnet_c:
        # A second leg to the SAME server, so `--server-cid A,B` has two real
        # addresses to choose between. Built after the loop above because it is
        # not a third endpoint: srv already has its address and its route.
        end_dev, rtr_dev = _SRV_LEG2
        rtr_ip, end_ip, plen = _addrs(subnet_c)
        ns_run(st, "rtr", ["ip", "link", "add", rtr_dev,
                           "type", "veth", "peer", "name", end_dev])
        ns_run(st, "rtr", ["ip", "link", "set", end_dev,
                           "netns", st[_PID_KEY["srv"]]])
        ns_run(st, "rtr", ["ip", "addr", "add", f"{rtr_ip}/{plen}",
                           "dev", rtr_dev])
        ns_run(st, "rtr", ["ip", "link", "set", rtr_dev, "up"])
        ns_run(st, "srv", ["ip", "addr", "add", f"{end_ip}/{plen}", "dev", end_dev])
        ns_run(st, "srv", ["ip", "link", "set", end_dev, "up"])
        # A SECOND default route at a higher metric, so srv's replies follow
        # whichever leg is up. The kernel withdraws a route whose link is down,
        # which is what makes `path 1 down` promote this one with no extra step
        # — the same way a host with two uplinks behaves. The first leg's route
        # is left exactly as the one-path lab writes it.
        ns_run(st, "srv", ["ip", "route", "add", "default",
                           "via", rtr_ip, "metric", "200"])
        st["SRV_IP2"] = end_ip
        st["SRV_RTR_IP2"] = rtr_ip
        st["SUBNET_C"] = subnet_c

    ns_run(st, "rtr", ["sysctl", "-qw", "net.ipv4.ip_forward=1"])

    st["SUBNET_A"] = subnet_a
    st["SUBNET_B"] = subnet_b
    return st


def apply_middlebox(st: dict, knobs: shaping.Knobs) -> None:
    """The knobs that model a broken path, all acting in `rtr`.

    None is set by any profile: each of these produces a stall that would
    otherwise read as a congestion-control result, so it has to be switched on
    deliberately.

    Everything is rebuilt from scratch on each call rather than appended to, so
    `shape` can turn one OFF on a live lab instead of only ever adding a rule.
    """
    # The MTU goes on the SRV LEG only, both ends of it — a narrow link BEYOND
    # the router, which is the shape path-MTU discovery is actually about.
    # Narrowing the cli leg too would make v-rtr-c drop cli's oversized frames
    # on receive, before any routing decision: a black hole by accident, and
    # one that --pmtu-blackhole could then take the credit for.
    srv_end, srv_rtr = _DEV["srv"]
    mtu = knobs.mtu if knobs.mtu is not None else 1500
    ns_run(st, "rtr", ["ip", "link", "set", srv_rtr, "mtu", str(mtu)])
    ns_run(st, "srv", ["ip", "link", "set", srv_end, "mtu", str(mtu)])

    # OUTPUT, not FORWARD. The ICMP fragmentation-needed is GENERATED BY the
    # router when it cannot forward an oversized packet, so it leaves through
    # OUTPUT; a FORWARD rule would only see ICMP transiting from somewhere
    # else and would never fire here.
    ns_run(st, "rtr", ["iptables", "-F", "OUTPUT"])
    if knobs.pmtu_blackhole:
        ns_run(st, "rtr", ["iptables", "-A", "OUTPUT", "-p", "icmp",
                           "--icmp-type", "fragmentation-needed", "-j", "DROP"])

    if knobs.conntrack_udp_timeout is not None:
        # Per-network-namespace, so this leaves the host's values untouched.
        # Setting it below the transport's keepalive interval is how a NAT
        # mapping is made to expire under a live connection — the failure that
        # on a real path takes minutes of idling and here takes seconds.
        for key in ("nf_conntrack_udp_timeout",
                    "nf_conntrack_udp_timeout_stream"):
            ns_run(st, "rtr", ["sysctl", "-qw",
                               f"net.netfilter.{key}="
                               f"{knobs.conntrack_udp_timeout}"])


def apply_shaping(st: dict, knobs: shaping.Knobs) -> None:
    """(Re-)apply the middlebox behaviour, the qdiscs and the NAT.

    Shaping goes on the ROUTER side of each veth, in the egress direction. A
    packet cli->srv is delayed once, on v-rtr-s; the reply is delayed once, on
    v-rtr-c. That is why the configured delay is one-way and the round trip is
    twice it.

    Every tc command is `replace`, so this is the same code path for `up` and
    for `shape` on a live lab.
    """
    apply_middlebox(st, knobs)
    for ns in ("srv", "cli"):
        _, rtr_dev = _DEV[ns]
        for cmd in shaping.qdisc_commands(rtr_dev, knobs):
            ns_run(st, "rtr", cmd)

    # NAT is rebuilt from scratch each time rather than appended to, so
    # `shape --no-nat` on a live lab REMOVES it instead of stacking a rule.
    ns_run(st, "rtr", ["iptables", "-t", "nat", "-F", "POSTROUTING"])
    if knobs.nat:
        _, srv_rtr_dev = _DEV["srv"]
        ns_run(st, "rtr", ["iptables", "-t", "nat", "-A", "POSTROUTING",
                           "-o", srv_rtr_dev, "-j", "MASQUERADE"])


# What cmd_env unsets in the CONSUMING shell. Same list dummy-harness.py uses
# and for the same reason: inside a harness task the shell still carries
# HARNESS_AUTH_TICKET for the LIVE server, and harness-cli prefers a ticket
# over the PSK, so every call would fail BadTicket while looking like a PSK
# problem.
_UNSET_IN_CONSUMER = (
    "HARNESS_AUTH_TICKET HARNESS_TASK_ID HARNESS_RUNNER_ID HARNESS_SERVER_CID "
    "HARNESS_WS_PATH HARNESS_REPO_PATH HARNESS_HOSTNAME"
)

_LISTEN_PROBE = (
    "import socket,sys\n"
    "try:\n"
    "    socket.create_connection((sys.argv[1], int(sys.argv[2])),\n"
    "                             timeout=0.5).close()\n"
    "except OSError:\n"
    "    raise SystemExit(1)\n"
)


def _ns_listening(st: dict, ns: str, ip: str, port: int) -> bool:
    """Whether something accepts a TCP connection at ip:port inside `ns`.

    A connect rather than a parse of `ss -ltn`: it needs no extra tool and it
    tests the property actually wanted. Run through this interpreter, which is
    reachable at the same path inside the namespace because only the user and
    network namespaces were unshared — the mount namespace is shared, so every
    filesystem path means the same thing on both sides.
    """
    return ns_run(st, ns, [sys.executable, "-c", _LISTEN_PROBE, ip, str(port)],
                  check=False).returncode == 0


def _ns_spawn(st: dict, ns: str, argv: list[str], log: Path) -> subprocess.Popen:
    """Start a long-lived process inside one of the lab's namespaces."""
    prefix = nsutil.nsenter_argv(int(st["USERNS_PID"]), int(st[_PID_KEY[ns]]))
    fh = log.open("wb")
    return subprocess.Popen(
        [*prefix, *[str(a) for a in argv]],
        stdout=fh, stderr=subprocess.STDOUT, stdin=subprocess.DEVNULL,
        start_new_session=True, env=os.environ.copy(),
    )


def _tail(path: Path, n: int) -> str:
    try:
        return path.read_text(encoding="utf-8", errors="replace")[-n:]
    except OSError:
        return f"(no {path.name})"


def start_harness(st: dict, args, extra: list[str]) -> None:
    """Start harness-server in `srv` and agent-runner in `cli`.

    make build, not go build: harness-server embeds webui/static/main.wasm and
    refuses to start without it. A server that never starts would make every
    later check trivially pass, which is worse than a loud failure.
    """
    dh.build()

    tmp = Path(st["TMP"])
    repo = tmp / "repo"
    data = tmp / "data"
    repo.mkdir(parents=True, exist_ok=True)
    data.mkdir(parents=True, exist_ok=True)
    subprocess.run(["git", "-C", str(repo), "init", "-q"], check=True)
    subprocess.run(
        ["git", "-C", str(repo), "-c", "user.email=lab@example.invalid",
         "-c", "user.name=lab", "commit", "-q", "--allow-empty",
         "-m", "netem-lab repo"],
        check=True,
    )

    port = dh.pick_port()
    psk = "netem-" + secrets.token_urlsafe(12).replace("-", "").replace("_", "")
    srv_ip = st["SRV_IP"]
    legs = [srv_ip] + ([st["SRV_IP2"]] if st.get("SRV_IP2") else [])
    # The server binds its own namespace's address. The runner's DIAL target is
    # derived from that address, never from the bind string — those are
    # different things even when they look alike, and using one for the other
    # has cost this project a cross-OS bug before (rewriteProxyViaForLocalDial).
    #
    # CID stays ONE address because harness-cli takes one; only the runner takes
    # a list (runner.ServerCandidates). Keeping them separate is also what lets
    # `bench` and the `exec` recipes go on working unchanged with two legs.
    cid = f"{args.transport}:{srv_ip}:{port}-*"
    runner_cid = ",".join(f"{args.transport}:{ip}:{port}-*" for ip in legs)

    # Two legs means the server has to answer on BOTH of its addresses, so it
    # binds every interface. That also gives it a loopback route, which is the
    # only address `failover` can ask `ls` over while one leg is down. One leg
    # keeps the narrower per-address bind this lab has always used.
    bind = f":{port}" if len(legs) > 1 else f"{srv_ip}:{port}"

    server = _ns_spawn(st, "srv", [
        dh.daemon.bin_path("harness-server"),
        "--listen", bind,
        "--udp-listen", bind,
        "--psk", psk, "--operator-psk", psk,
        "--data-dir", str(data),
        *getattr(args, "server_args", []),
    ], tmp / "server.log")
    st["SERVER_PID"] = server.pid

    ready = False
    for _ in range(60):
        if server.poll() is not None:
            break
        if _ns_listening(st, "srv", srv_ip, port):
            ready = True
            break
        time.sleep(0.25)
    if not ready:
        sys.stderr.write(_tail(tmp / "server.log", 2000))
        dh.kill_pid(server.pid)
        setup_err(f"server never listened on {srv_ip}:{port}")

    runner_args = [
        dh.daemon.bin_path("agent-runner"),
        "--server-cid", runner_cid, "--psk", psk, "--roots", str(repo),
        "--no-worktree", "--max-tasks", "4",
    ]
    # No profile at all beats one that registers and then fails per task; an
    # absent `bash` agent is rejected at submit with a name you can act on.
    bash = dh.bash_bin()
    if bash:
        runner_args += ["--agent-profiles", dh.bash_profile(bash)]
    if args.agent == "claude":
        runner_args += [
            "--agent-bin", "claude",
            "--claude-args", f"--model {args.model}",
            "--agent-oneshot-argv",
            "--output-format stream-json --verbose {args} -p {prompt}",
            "--agent-resume-oneshot-argv",
            "--output-format stream-json --verbose {args} --continue -p {prompt}",
            "--agent-resume-interactive-argv", "{args} --continue",
            "--agent-log-format", "claude-stream-json",
        ]
    else:
        fake = tmp / "fake-claude.py"
        fake.write_text(dh.FAKE_AGENT, encoding="utf-8")
        runner_args += [
            "--agent-bin", sys.executable,
            "--agent-oneshot-argv",
            f"{fake} --output-format stream-json --verbose {{args}} -p {{prompt}}",
            "--agent-resume-oneshot-argv",
            f"{fake} --output-format stream-json --verbose {{args}} "
            f"--continue -p {{prompt}}",
            "--agent-resume-interactive-argv", f"{fake} {{args}} --continue",
            "--agent-log-format", "claude-stream-json",
        ]
    runner_args += extra

    runner = _ns_spawn(st, "cli", runner_args, tmp / "runner.log")
    st["RUNNER_PID"] = runner.pid

    os.environ["HARNESS_PSK"] = psk  # harness-cli's operator binder falls back to it
    cli_bin = str(dh.daemon.bin_path("harness-cli"))
    registered = False
    for _ in range(80):
        if runner.poll() is not None:
            break
        out = ns_run(st, "cli", [cli_bin, "--server-cid", cid, "ls"], check=False)
        if "agent=" in out.stdout:
            registered = True
            break
        time.sleep(0.25)
    if not registered:
        sys.stderr.write(_tail(tmp / "runner.log", 2000))
        dh.kill_pid(runner.pid)
        dh.kill_pid(server.pid)
        setup_err("runner never registered")

    st.update({"HARNESS_PSK": psk, "CID": cid, "RUNNER_CID": runner_cid,
               "REPO": str(repo), "SERVER_PORT": port})


def cmd_env(name: str) -> int:
    st = read_state(name)
    if st is None:
        die(f"no instance named {name!r}; run 'up' first")
    print(f"unset {_UNSET_IN_CONSUMER}")
    # TMP is deliberately NOT exported; see state_dir() for the leak that one
    # caused in the tool this borrows from.
    for key in ("HARNESS_PSK", "CID", "REPO", "SERVER_PORT"):
        print(f"export {key}='{st[key]}'")
    # RUNNER_CID can be a LIST and CID never is: harness-cli takes one address,
    # the runner takes candidates. Exported separately so neither is pasted
    # where the other belongs.
    for key in ("RUNNER_CID", "SRV_IP2"):
        if st.get(key):
            print(f"export {key}='{st[key]}'")
    print(f"export NETEM_LAB_NAME='{name}'")
    print("# harness-cli must run INSIDE the lab: the host namespace has no")
    print(f"# route to {st['SRV_IP']}, so a call from here TIMES OUT rather")
    print("# than saying anything. Use:")
    print(f"#   scripts/netem-lab/netem-lab.py --name {name} exec cli -- \\")
    print("#     harness-cli --server-cid \"$CID\" ls")
    return 0


def calibrate(st: dict, knobs: shaping.Knobs) -> None:
    """Ping the server address from the client namespace and print measured
    against configured RTT.

    A lab whose measured RTT does not match its profile is not a lab, and the
    cheapest moment to find that out is before any measurement is taken.
    """
    proc = ns_run(st, "cli",
                  ["ping", "-c", "3", "-q", "-W", "5", st["SRV_IP"]],
                  check=False)
    line = next((l for l in proc.stdout.splitlines()
                 if "rtt" in l or "round-trip" in l), "")
    if proc.returncode != 0 or not line:
        die("calibration ping failed; the lab is wired but not carrying "
            f"traffic:\n{proc.stdout}{proc.stderr}")
    want = knobs.delay_ms * 2
    print(f"netem-lab: configured RTT {want:g}ms "
          f"(2 x {knobs.delay_ms:g}ms one-way)")
    print(f"netem-lab: measured   {line.strip()}")


def cmd_up(args, extra: list[str]) -> int:
    problems = nsutil.preflight_problems()
    if problems:
        setup_err("\n         ".join(problems))
    if read_state(args.name) is not None:
        die(f"an instance named {args.name!r} is already recorded at "
            f"{state_path(args.name)}; run 'down' first")

    knobs = knobs_from_args(args)
    tmp = Path(tempfile.mkdtemp(prefix="harness-netem.", dir=str(dh.tmp_root())))
    st = build_network(knobs, args.subnet_a, args.subnet_b, tmp,
                       args.subnet_c if args.paths == 2 else None)
    st["TMP"] = str(tmp)
    st["PROFILE"] = args.profile or ""
    apply_shaping(st, knobs)
    # Written BEFORE anything that can fail from here on: the state file is the
    # only record of what to kill, and losing it strands three namespace
    # holders with nothing naming them.
    write_state(args.name, st)

    calibrate(st, knobs)
    start_harness(st, args, extra)
    write_state(args.name, st)
    print(f"netem-lab: up  name={args.name}  "
          f"srv={st['SRV_IP']}  cli={st['CLI_IP']}"
          + (f"  srv2={st['SRV_IP2']} (leg 2, UNSHAPED)" if st.get("SRV_IP2") else ""))
    if st.get("SRV_IP2"):
        print("netem-lab: 'path 1 down' kills the first leg; 'failover' measures "
              "the runner moving off it")
    print("netem-lab: 'netem-lab.py exec cli -- <cmd>' runs inside the lab; "
          "'down' stops it")
    return 0


def cmd_exec(name: str, ns: str, argv: list[str]) -> int:
    st = read_state(name)
    if st is None:
        die(f"no instance named {name!r}; run 'up' first")
    if not argv:
        die("exec needs a command after `--`")
    prefix = nsutil.nsenter_argv(int(st["USERNS_PID"]), int(st[_PID_KEY[ns]]))
    # ns_env re-injects this instance's PSK; see there for why. It means `exec`
    # works whether or not the caller ran `eval "$(netem-lab.py env)"` first.
    return subprocess.run([*prefix, *argv], env=ns_env(st)).returncode


def cmd_show(name: str) -> int:
    st = read_state(name)
    if st is None:
        die(f"no instance named {name!r}; run 'up' first")
    for ns in ("srv", "cli"):
        _, rtr_dev = _DEV[ns]
        print(f"--- rtr:{rtr_dev} (egress toward {ns}) ---")
        # -s carries sent / dropped / overlimits / backlog, which is what
        # separates "the bottleneck queue overflowed" from "netem dropped it".
        print(ns_run(st, "rtr",
                     ["tc", "-s", "qdisc", "show", "dev", rtr_dev]).stdout)
    print("--- rtr: conntrack entries ---")
    ct = ns_run(st, "rtr", ["conntrack", "-C"], check=False)
    print(ct.stdout.strip() if ct.returncode == 0 else "(conntrack-tools absent)")
    print("--- rtr: nat ---")
    print(ns_run(st, "rtr",
                 ["iptables", "-t", "nat", "-S", "POSTROUTING"]).stdout.strip())
    print("--- addresses ---")
    for ns in ("srv", "cli"):
        print(f"{ns}: {st[f'{ns.upper()}_IP']} via {st[f'{ns.upper()}_RTR_IP']}")
    if st.get("SRV_IP2"):
        print(f"srv leg 2: {st['SRV_IP2']} via {st['SRV_RTR_IP2']} (unshaped)")
        print("--- server legs ---")
        for leg in (1, 2):
            srv_dev, rtr_dev = leg_devs(leg)
            state = ns_run(st, "rtr", ["ip", "-br", "link", "show", rtr_dev],
                           check=False).stdout.strip()
            print(f"leg {leg}: {state or '(absent)'}")
    return 0


def _pin(st: dict, cpus: list[int]) -> str:
    """Pin the server and the runner to cores of their own.

    CPU affinity is not namespaced, so this is set from the host against the
    pids the state file already records. Returns a line describing what was
    done, or why it was not.
    """
    if len(cpus) < 3:
        return f"pinning skipped: {len(cpus)} cpu(s) available, need 3"
    placed = []
    for key, cpu in (("SERVER_PID", cpus[0]), ("RUNNER_PID", cpus[1])):
        pid = st.get(key)
        if not pid:
            continue
        proc = subprocess.run(["taskset", "-cp", str(cpu), str(pid)],
                              capture_output=True, text=True)
        if proc.returncode == 0:
            placed.append(f"{key.split('_')[0].lower()}=cpu{cpu}")
    placed.append(f"client=cpu{cpus[2]}")
    return "pinned " + " ".join(placed)


def _bench_payload(st: dict, size_mb: int) -> Path:
    """The file every run pushes. Generated once and reused: regenerating it
    per run would put `dd` and the page cache inside the measurement."""
    path = Path(st["TMP"]) / f"bench-{size_mb}mb.bin"
    if not path.is_file() or path.stat().st_size != size_mb * 1024 * 1024:
        with path.open("wb") as f:
            chunk = os.urandom(1024 * 1024)
            for _ in range(size_mb):
                f.write(chunk)
    return path


def cmd_bench(args) -> int:
    """Push the same file N times and report the spread as well as the middle.

    The spread is the point. A single run of this workload was measured
    spanning 6.76-12.70 MB/s on one unchanged build — so a lone number carries
    no information about a change smaller than about 2x, and two of this
    project's "findings" were once exactly that. The resolution line at the
    bottom says what this particular run can actually distinguish.
    """
    st = read_state(args.name)
    if st is None:
        die(f"no instance named {args.name!r}; run 'up' first")
    if not st.get("CID"):
        die("this lab has no harness running; `bench` needs one")

    cpus = sorted(os.sched_getaffinity(0))
    pin_note = _pin(st, cpus) if args.pin else "pinning disabled (--no-pin)"
    payload = _bench_payload(st, args.size_mb)
    size = payload.stat().st_size
    cli_bin = str(dh.daemon.bin_path("harness-cli"))

    task = ns_run(st, "cli", [cli_bin, "--server-cid", st["CID"], "submit",
                              "--repo", st["REPO"], "--agent", "bash",
                              "--task", "sleep 3600"]).stdout
    task_id = next((ln.strip() for ln in task.splitlines()
                    if len(ln.strip()) == 32 and all(c in "0123456789abcdef"
                                                     for c in ln.strip())), "")
    if not task_id:
        die("could not start the sink task; does this lab have a `bash` agent "
            f"profile?\n{task}")
    time.sleep(4)  # let it reach Running, or file push refuses

    prefix: list[str] = []
    if args.pin and len(cpus) >= 3:
        prefix = ["taskset", "-c", str(cpus[2])]

    print(f"netem-lab: bench  name={args.name}  profile={st.get('PROFILE') or '(knobs)'}  "
          f"size={args.size_mb}MB  runs={args.runs}(+1 warm-up)")
    print(f"netem-lab: {pin_note}")

    rates: list[float] = []
    for i in range(args.runs + 1):
        start = time.monotonic()
        proc = ns_run(st, "cli", [*prefix, cli_bin, "--server-cid", st["CID"],
                                  "file", "push", "-f", task_id,
                                  str(payload), f"bench{i}"], check=False)
        elapsed = time.monotonic() - start
        if proc.returncode != 0:
            ns_run(st, "cli", [cli_bin, "--server-cid", st["CID"],
                               "cancel", task_id], check=False)
            die(f"run {i} failed: {(proc.stderr or proc.stdout).strip()[:400]}")
        rate = size / elapsed / 1e6
        if i == 0:
            # Discarded: the first push pays page-cache misses on the payload
            # and whatever the connection does on its first bulk transfer.
            print(f"  warm-up  {rate:6.2f} MB/s  (discarded)")
        else:
            rates.append(rate)
            print(f"  run {i:<4} {rate:6.2f} MB/s")

    ns_run(st, "cli", [cli_bin, "--server-cid", st["CID"], "cancel", task_id],
           check=False)
    print(_bench_summary(rates))
    return 0


def _bench_summary(rates: list[float]) -> str:
    """Format the run set, ending with what it can resolve.

    The resolution figure is 2 standard errors of a difference between two
    such run sets — 2*s*sqrt(2/n) — expressed against the median. Below it,
    two builds cannot be told apart by this benchmark, and reporting a smaller
    difference as an improvement is reporting noise.
    """
    if not rates:
        return "  (no successful runs)"
    med = statistics.median(rates)
    if len(rates) < 2:
        return (f"  n=1  {med:.2f} MB/s — one run resolves nothing; "
                f"use --runs 5 or more")
    sd = statistics.stdev(rates)
    resolvable = 2 * sd * math.sqrt(2 / len(rates)) / med * 100
    return (
        f"  ---\n"
        f"  n={len(rates)}  median={med:.2f}  mean={statistics.mean(rates):.2f}  "
        f"stdev={sd:.2f} ({sd / med * 100:.0f}%)  "
        f"min={min(rates):.2f}  max={max(rates):.2f}  "
        f"spread={max(rates) / min(rates):.2f}x\n"
        f"  RESOLUTION: this run can distinguish differences larger than about "
        f"{resolvable:.0f}%.\n"
        f"  A change measuring smaller than that has not been shown to do "
        f"anything — raise --runs to narrow it."
    )


def cmd_shape(args) -> int:
    """Re-shape a running lab without restarting anything.

    Path conditions that change mid-transfer cannot be tested by a tool that
    only sets conditions at startup, and re-running `up` would restart the
    processes under test — losing exactly the connection whose behaviour is in
    question.

    Nearly free: every tc command was already `replace`, and every iptables
    chain is flushed before being rebuilt, so this reuses `up`'s path whole.
    """
    st = read_state(args.name)
    if st is None:
        die(f"no instance named {args.name!r}; run 'up' first")
    knobs = knobs_from_args(args)
    apply_shaping(st, knobs)
    st["PROFILE"] = args.profile or ""
    write_state(args.name, st)
    print(f"netem-lab: reshaped  delay={knobs.delay_ms:g}ms one-way  "
          f"rate={knobs.rate or 'unlimited'}  loss={knobs.loss_pct:g}%  "
          f"limit={knobs.limit}  nat={'on' if knobs.nat else 'off'}")
    return 0


def cmd_path(name: str, leg: int, action: str) -> int:
    """Take one server leg down, or bring it back.

    BOTH ends of the leg, deliberately. Downing only the router side leaves the
    srv end merely carrier-less, and carrier loss does NOT withdraw srv's route
    — so replies would keep leaving through a dead link and every failure would
    present as a blackhole regardless of which one you meant to model. With both
    ends down it is unambiguous: rtr has no route to that server address, so the
    next dial to it fails outright, and srv's higher-metric default takes over.
    """
    st = read_state(name)
    if st is None:
        die(f"no instance named {name!r}; run 'up' first")
    if not st.get("SRV_IP2"):
        die("this lab has one path; `up --paths 2` builds the second")
    srv_dev, rtr_dev = leg_devs(leg)
    ns_run(st, "rtr", ["ip", "link", "set", rtr_dev, action])
    ns_run(st, "srv", ["ip", "link", "set", srv_dev, action])
    print(f"netem-lab: leg {leg} {action}  ({rtr_dev} + {srv_dev})")
    return 0


def _runner_row(st: dict) -> tuple[str, str] | None:
    """(RunnerID, the runner's connection id as the SERVER sees it), or None.

    Asked over loopback from inside `srv`, which is the only address that stays
    reachable whichever leg is down — that is what the all-interfaces bind of
    `up --paths 2` is for. `ws:` regardless of the lab's --transport, because
    the server binds both legs' listeners to the same address either way.

    The connection id is the discriminator rather than the address in it: with
    NAT on it also names the leg (the router's address on the leg the packets
    arrived through), and with --no-nat it does not — but a RECONNECT always
    produces a different connection id, which is the property being measured.
    """
    cli_bin = str(dh.daemon.bin_path("harness-cli"))
    lo_cid = f"ws:127.0.0.1:{st['SERVER_PORT']}-*"
    out = ns_run(st, "srv", [cli_bin, "--server-cid", lo_cid, "ls"],
                 check=False).stdout or ""
    body = out.split("TASKS")[0]
    m = re.search(r"\bid=([0-9a-f]{32})\b.*?\bcid=(\S+)", body, re.S)
    return (m.group(1), m.group(2)) if m else None


def cmd_failover(args) -> int:
    """Kill the leg the runner is using and measure it arriving on the other.

    This is the measurement `--server-cid A,B` exists for, and it cannot be made
    on loopback: it needs two paths that can be taken away independently, which
    is what this lab has and dummy-harness.py by construction does not.

    Two assertions, and the second is the one the feature was designed around:
    the runner comes back, and the server keeps it as the SAME RunnerID —
    Registry.Add's takeover, rather than a second row for one process.
    """
    st = read_state(args.name)
    if st is None:
        die(f"no instance named {args.name!r}; run 'up' first")
    if not st.get("SRV_IP2"):
        die("failover needs two paths; `up --paths 2`")
    if not st.get("CID"):
        die("this lab has no harness running; `failover` needs one")

    before = _runner_row(st)
    if before is None:
        die("no runner is registered, so there is nothing to fail over")
    rid, cid_before = before
    srv_dev, rtr_dev = leg_devs(args.leg)
    print(f"netem-lab: runner {rid[:8]} on conn {cid_before}")
    print(f"netem-lab: taking leg {args.leg} down, waiting up to "
          f"{args.timeout:g}s for it to move")

    ns_run(st, "rtr", ["ip", "link", "set", rtr_dev, "down"])
    ns_run(st, "srv", ["ip", "link", "set", srv_dev, "down"])
    t0 = time.monotonic()
    moved = None
    while time.monotonic() - t0 < args.timeout:
        row = _runner_row(st)
        if row is not None and row[1] != cid_before:
            moved = row
            break
        time.sleep(1.0)
    elapsed = time.monotonic() - t0

    # Restored before reporting: a run that failed its assertion should leave a
    # lab you can look at, not one with a leg still down.
    ns_run(st, "rtr", ["ip", "link", "set", rtr_dev, "up"], check=False)
    ns_run(st, "srv", ["ip", "link", "set", srv_dev, "up"], check=False)

    if moved is None:
        print(f"netem-lab: FAILED — still on {cid_before} after {elapsed:.0f}s")
        print("netem-lab: leg restored; 'show' for the link states")
        return 1
    rid_after, cid_after = moved
    print(f"netem-lab: moved to {cid_after} in {elapsed:.0f}s")
    if rid_after != rid:
        print(f"netem-lab: FAILED — RunnerID changed {rid[:8]} -> "
              f"{rid_after[:8]}; the server took it for a new process")
        return 1
    print(f"netem-lab: RunnerID survived ({rid[:8]}) — the registry treated it "
          "as the same runner on a new connection")
    print("netem-lab: the WAIT is set by objproto's connection GC "
          "(connectionTimeout = 1 min, runner.Connect), not by --ping-interval: "
          "neither a withdrawn route nor a blackhole makes a send FAIL, so "
          "CannotSend never fires. ~70s measured for both.")
    return 0


def main(argv: list[str]) -> int:
    dh.survive_undisplayable_output()
    dh.scrub_own_env()

    if "--" in argv:
        cut = argv.index("--")
        argv, extra = argv[:cut], argv[cut + 1:]
    else:
        extra = []

    p = argparse.ArgumentParser(prog="netem-lab.py", add_help=True)
    p.add_argument("--name", default="default")
    sub = p.add_subparsers(dest="sub", required=True)

    up = sub.add_parser("up")
    _add_knob_flags(up)
    up.add_argument("--agent", default="fake", choices=("claude", "fake"))
    up.add_argument("--model", default="claude-haiku-4-5-20251001")
    up.add_argument("--transport", default="udp", choices=("udp", "ws"))
    up.add_argument("--subnet-a", dest="subnet_a", default="10.90.0.0/24")
    up.add_argument("--subnet-b", dest="subnet_b", default="10.91.0.0/24")
    # A second, UNSHAPED leg to the same server, so --server-cid can carry two
    # candidates. Default 1 leaves the topology byte-identical to before.
    up.add_argument("--paths", type=int, choices=(1, 2), default=1)
    up.add_argument("--subnet-c", dest="subnet_c", default="10.92.0.0/24")
    # Words after `--` go to the RUNNER, which left the server with no way to
    # be configured at all — and the server is the process a transfer spends
    # most of its CPU in, so it is the one you want --pprof-listen on.
    # Repeatable; appended after this script's own server flags.
    #
    # Use the `=` form for anything starting with a dash —
    # `--server-arg=--pprof-listen --server-arg=10.90.0.2:6060`. Without it
    # argparse reads the next token as a flag of its own and refuses.
    up.add_argument("--server-arg", dest="server_args", action="append",
                    default=[], metavar="FLAG",
                    help="extra flag for harness-server; repeatable. Use "
                         "--server-arg=--flag for dash-leading values.")

    sub.add_parser("env")
    ex = sub.add_parser("exec")
    ex.add_argument("ns", choices=("srv", "rtr", "cli"))
    sh = sub.add_parser("shape")
    _add_knob_flags(sh)
    bench = sub.add_parser("bench")
    bench.add_argument("--runs", type=int, default=6)
    bench.add_argument("--size-mb", dest="size_mb", type=int, default=100)
    bench.add_argument("--no-pin", dest="pin", action="store_false", default=True)
    pa = sub.add_parser("path")
    pa.add_argument("leg", type=int, choices=(1, 2))
    pa.add_argument("action", choices=("up", "down"))
    fo = sub.add_parser("failover")
    fo.add_argument("--leg", type=int, choices=(1, 2), default=1)
    fo.add_argument("--timeout", type=float, default=150.0)
    sub.add_parser("show")
    sub.add_parser("down")

    args = p.parse_args(argv)

    if args.sub == "down":
        return cmd_down(args.name)
    if args.sub == "up":
        return cmd_up(args, extra)
    if args.sub == "exec":
        return cmd_exec(args.name, args.ns, extra)
    if args.sub == "show":
        return cmd_show(args.name)
    if args.sub == "env":
        return cmd_env(args.name)
    if args.sub == "shape":
        return cmd_shape(args)
    if args.sub == "bench":
        return cmd_bench(args)
    if args.sub == "path":
        return cmd_path(args.name, args.leg, args.action)
    if args.sub == "failover":
        return cmd_failover(args)
    die(f"unhandled subcommand {args.sub!r}")


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
