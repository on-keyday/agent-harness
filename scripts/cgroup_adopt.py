"""Put a freshly spawned daemon into its own systemd unit cgroup (Linux).

``setsid()`` — what ``daemon._spawn_detached`` uses to detach — creates a new
session and process group but does **not** change cgroup membership: the
spawned daemon inherits the cgroup of whoever called it. When
``restart.py`` / ``build_and_restart_all.py`` run from inside a claude
session hosted by an agent-runner, every slot they respawn therefore lands
in *that runner's* systemd unit cgroup instead of its own.

The runner units are ``KillMode=control-group``, so once the fleet has piled
into one unit's cgroup a single ``systemctl --user restart
harness-agent-runner`` SIGTERMs all of it — every runner plus every live
agent session underneath them. That is what happened on 2026-07-29 00:18,
taking out the bash / review / sandbox slots simultaneously; the surviving
slots looked healthy only because they had been respawned into a different
cgroup minutes earlier.

Adopting the child into its own unit's cgroup immediately after spawn
restores the intended one-slot-per-kill-domain boundary no matter who
invoked the spawn.

Pure stdlib on purpose (same rationale as ``agent_presets.py``): importable
and testable without ``scripts/.venv`` / psutil.
"""

from __future__ import annotations

import subprocess
import sys
from pathlib import Path

# Slot "agent-runner-blog" is owned by unit "harness-agent-runner-blog.service".
# Mirrors runner-autostart.py's _slot_name()/_systemd_unit_name() naming.
UNIT_PREFIX = "harness-"
CGROUP_ROOT = Path("/sys/fs/cgroup")


def systemd_unit_name(slot: str) -> str:
    """The systemd user unit that owns *slot*."""
    return f"{UNIT_PREFIX}{slot}.service"


def unit_cgroup(slot: str) -> Path | None:
    """Path to *slot*'s systemd user unit cgroup, or None when there isn't one.

    Returns None for a slot with no registered unit, an inactive unit, a
    non-Linux host, or a host without a running ``systemd --user``. systemd
    is the authority for the path — deriving it from /proc/self/cgroup would
    bake in the current slice layout.
    """
    if not sys.platform.startswith("linux"):
        return None
    try:
        out = subprocess.run(
            [
                "systemctl", "--user", "show", systemd_unit_name(slot),
                "-p", "ControlGroup", "--value",
            ],
            capture_output=True,
            text=True,
            timeout=10,
        )
    except (OSError, subprocess.SubprocessError):
        return None
    rel = out.stdout.strip()
    # An unknown or inactive unit reports an empty ControlGroup.
    if out.returncode != 0 or not rel or rel == "/":
        return None
    cg = CGROUP_ROOT / rel.lstrip("/")
    return cg if (cg / "cgroup.procs").is_file() else None


def current_cgroup(pid: int | str) -> str:
    """Basename of *pid*'s cgroup ("" when unavailable)."""
    try:
        line = Path(f"/proc/{pid}/cgroup").read_text().strip()
    except OSError:
        return ""
    return line.rsplit("/", 1)[-1] if line else ""


def adopt_into_unit_cgroup(pid: int, slot: str) -> None:
    """Move *pid* into *slot*'s systemd unit cgroup when one exists.

    Never fatal: when the slot has no registered unit, the unit is inactive,
    or the host isn't Linux, the daemon still runs — it just stays in the
    caller's cgroup. When that leaves it inside some *other* harness unit's
    kill domain we say so loudly, because the failure mode is otherwise
    silent right up until an unrelated ``systemctl restart`` takes the
    process down with it.

    Call immediately after spawn. A child forked in the intervening
    milliseconds would be left behind; daemons here do not fork that early,
    and every later fork inherits the corrected cgroup.
    """
    if not sys.platform.startswith("linux"):
        return
    cg = unit_cgroup(slot)
    if cg is None:
        inherited = current_cgroup(pid)
        if inherited.startswith(UNIT_PREFIX) and inherited != systemd_unit_name(slot):
            sys.stderr.write(
                f"[{slot}] warning: no active systemd unit for this slot; it "
                f"inherited {inherited}\n"
                f"        A restart/stop of that unit will also kill this "
                f"process (KillMode=control-group).\n"
                f"        Give the slot its own kill domain with: "
                f"scripts/runner-autostart.py register --as <tag> ...\n"
            )
        return
    if current_cgroup(pid) == cg.name:
        return  # already correct — e.g. systemd itself ran our ExecStart
    try:
        (cg / "cgroup.procs").write_text(f"{pid}\n")
    except OSError as e:
        sys.stderr.write(
            f"[{slot}] warning: could not adopt pid {pid} into {cg.name}: {e}\n"
        )
        return
    print(f"[{slot}] adopted into {cg.name}")
