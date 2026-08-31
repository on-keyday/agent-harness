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
import os
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


def ns_run(st: dict, ns: str, argv: list[str], check: bool = True, **kw):
    """Run one command inside one of the lab's namespaces."""
    prefix = nsutil.nsenter_argv(int(st["USERNS_PID"]), int(st[_PID_KEY[ns]]))
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
                  tmp: Path) -> dict:
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

    ns_run(st, "rtr", ["sysctl", "-qw", "net.ipv4.ip_forward=1"])

    st["SUBNET_A"] = subnet_a
    st["SUBNET_B"] = subnet_b
    return st


def apply_shaping(st: dict, knobs: shaping.Knobs) -> None:
    """(Re-)apply the qdiscs and the NAT.

    Shaping goes on the ROUTER side of each veth, in the egress direction. A
    packet cli->srv is delayed once, on v-rtr-s; the reply is delayed once, on
    v-rtr-c. That is why the configured delay is one-way and the round trip is
    twice it.

    Every tc command is `replace`, so this is the same code path for `up` and
    for `shape` on a live lab.
    """
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
    # The server binds its own namespace's address. The runner's DIAL target is
    # derived from that address, never from the bind string — those are
    # different things even when they look alike, and using one for the other
    # has cost this project a cross-OS bug before (rewriteProxyViaForLocalDial).
    cid = f"{args.transport}:{srv_ip}:{port}-*"

    server = _ns_spawn(st, "srv", [
        dh.daemon.bin_path("harness-server"),
        "--listen", f"{srv_ip}:{port}",
        "--udp-listen", f"{srv_ip}:{port}",
        "--psk", psk, "--operator-psk", psk,
        "--data-dir", str(data),
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
        "--server-cid", cid, "--psk", psk, "--roots", str(repo),
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

    st.update({"HARNESS_PSK": psk, "CID": cid, "REPO": str(repo),
               "SERVER_PORT": port})


def cmd_env(name: str) -> int:
    st = read_state(name)
    if st is None:
        die(f"no instance named {name!r}; run 'up' first")
    print(f"unset {_UNSET_IN_CONSUMER}")
    # TMP is deliberately NOT exported; see state_dir() for the leak that one
    # caused in the tool this borrows from.
    for key in ("HARNESS_PSK", "CID", "REPO", "SERVER_PORT"):
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
    st = build_network(knobs, args.subnet_a, args.subnet_b, tmp)
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
          f"srv={st['SRV_IP']}  cli={st['CLI_IP']}")
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
    # main() scrubbed HARNESS_* out of this process. That is right for what
    # `up` spawns and wrong here: the usual reason to run `exec` at all is
    # harness-cli, and a scrubbed environment leaves it with no credential, so
    # the server answers BadPsk — which reads as a WRONG psk rather than an
    # absent one and sends you looking in the wrong place. Re-inject THIS
    # instance's, so `exec` works whether or not the caller ran
    # `eval "$(netem-lab.py env)"` first.
    env = os.environ.copy()
    if st.get("HARNESS_PSK"):
        env["HARNESS_PSK"] = st["HARNESS_PSK"]
    return subprocess.run([*prefix, *argv], env=env).returncode


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

    sub.add_parser("env")
    ex = sub.add_parser("exec")
    ex.add_argument("ns", choices=("srv", "rtr", "cli"))
    sh = sub.add_parser("shape")
    _add_knob_flags(sh)
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
    raise NotImplementedError(f"{args.sub} arrives in a later task")


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
