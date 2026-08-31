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
import json
import os
import shutil
import sys
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
    raise NotImplementedError(f"{args.sub} arrives in a later task")


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
