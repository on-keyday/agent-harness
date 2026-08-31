"""Profile table and `tc` argv generation. Pure: no subprocess, no filesystem.

Split out from the rest of the lab because this is where a mistake is silent.
A netem argv that lost its `limit` drops packets nobody asked for, and the
result reads as loss on the link rather than as a bug in this file. Everything
here is a value transformation, so it is tested without a namespace.
"""

from __future__ import annotations

from dataclasses import dataclass, replace

# netem's own default `limit` is 1000 packets. At 115 ms and 100 Mbit/s the
# in-flight count exceeds that, so leaving it unset drops packets the operator
# did not ask for. Never emit a netem argv without it.
DEFAULT_LIMIT = 10000


@dataclass(frozen=True)
class Knobs:
    """One fully resolved path configuration.

    The four middlebox fields are deliberately NOT set by any profile. Each
    models a specific broken path and must be switched on deliberately, or a
    stall it causes gets read as a congestion-control result — which is the one
    confusion this lab exists to prevent.
    `test_no_profile_carries_a_middlebox_knob` is what keeps that true as
    profiles are added.
    """

    delay_ms: float = 0.0
    jitter_ms: float = 0.0
    loss_pct: float = 0.0
    reorder_pct: float = 0.0
    rate: str | None = None          # a tc rate string, e.g. "20mbit"
    limit: int = DEFAULT_LIMIT       # queue depth in packets

    mtu: int | None = None
    pmtu_blackhole: bool = False
    conntrack_udp_timeout: int | None = None
    nat: bool = True


# delay_ms is ONE-WAY. Each direction is shaped separately, so a profile at
# 75ms yields a ~150ms round trip; the README says so beside this table.
PROFILES: dict[str, Knobs] = {
    "lan":         Knobs(delay_ms=1),
    "wan-jp":      Knobs(delay_ms=5),
    "wan-us":      Knobs(delay_ms=75),
    "wan-eu":      Knobs(delay_ms=115),
    "bufferbloat": Knobs(delay_ms=75, rate="20mbit", limit=2000),
    "thin":        Knobs(delay_ms=75, rate="2mbit", limit=100),
    "lossy":       Knobs(delay_ms=75, loss_pct=1.0),
}


def resolve_knobs(profile: str | None, **overrides) -> Knobs:
    """Profile values with any explicitly-given knob laid on top.

    `overrides` uses None for "not given on the command line". That cannot
    simply be splatted into `replace()`: `rate`, `mtu` and
    `conntrack_udp_timeout` are all legally None, so "absent" and "explicitly
    none" would collapse and a profile's rate would be erased by not passing
    `--rate`.
    """
    if profile is None:
        base = Knobs()
    else:
        try:
            base = PROFILES[profile]
        except KeyError:
            known = ", ".join(sorted(PROFILES))
            raise ValueError(
                f"unknown profile {profile!r}; known profiles: {known}"
            ) from None
    given = {k: v for k, v in overrides.items() if v is not None}
    return replace(base, **given)


def _num(v: float) -> str:
    """Render 75.0 as "75" and 0.5 as "0.5", so the emitted command line reads
    the way the operator typed it."""
    f = float(v)
    return str(int(f)) if f.is_integer() else str(f)


def netem_argv(k: Knobs) -> list[str]:
    """The `netem ...` portion of a tc command line.

    `limit` is emitted last and unconditionally; see DEFAULT_LIMIT.
    """
    argv = ["netem"]
    if k.delay_ms > 0:
        argv += ["delay", f"{_num(k.delay_ms)}ms"]
        if k.jitter_ms > 0:
            argv += [f"{_num(k.jitter_ms)}ms", "distribution", "normal"]
    if k.loss_pct > 0:
        argv += ["loss", f"{_num(k.loss_pct)}%"]
    if k.reorder_pct > 0:
        if k.delay_ms <= 0:
            raise ValueError(
                "--reorder needs a non-zero --delay: netem reorders packets "
                "against the delay queue, and there is no queue at 0ms"
            )
        argv += ["reorder", f"{_num(k.reorder_pct)}%"]
    argv += ["limit", str(k.limit)]
    return argv


def qdisc_commands(dev: str, k: Knobs) -> list[list[str]]:
    """The tc invocations that shape one device, in the order they must run.

    With a rate, the chain is `htb` root -> `netem` leaf, so the queue that
    overflows is the bottleneck's and the loss it produces was CAUSED by the
    sender. That is the whole point: netem's own `loss` knob is independent of
    the sending rate, and a congestion controller measured only against it is
    measured against the wrong thing.

    Without a rate there is no bottleneck and netem is the root.

    Every command is `replace`, not `add`, so `shape` on a live lab reuses this
    path instead of needing one of its own.
    """
    if k.rate is None:
        return [["tc", "qdisc", "replace", "dev", dev, "root", *netem_argv(k)]]
    return [
        ["tc", "qdisc", "replace", "dev", dev, "root", "handle", "1:",
         "htb", "default", "10"],
        ["tc", "class", "replace", "dev", dev, "parent", "1:", "classid", "1:10",
         "htb", "rate", k.rate],
        ["tc", "qdisc", "replace", "dev", dev, "parent", "1:10", "handle", "10:",
         *netem_argv(k)],
    ]
