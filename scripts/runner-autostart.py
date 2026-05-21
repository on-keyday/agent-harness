#!/usr/bin/env python3
"""runner-autostart.py — register agent-runner to auto-start on login/boot.

Cross-platform wrapper that arranges for ``scripts/runner.py up --as
<tag> [args...]`` to be triggered on user login:

  - Windows: Task Scheduler ``AtLogOn`` task (per-user, no admin).
  - Linux:   systemd user service (per-user, runs via the user manager).

The actual daemon process is still managed by ``scripts/daemon.py``
(state under ``bin/.run/<slot>.{pid,log}``), so ``runner.py up/down``
keeps working unchanged. This script only adds the boot/login trigger.

Why not let the OS supervisor own the process directly (Task Scheduler
relaunch-on-failure, systemd ``Restart=on-failure``)?
  - agent-runner already has ``--persist`` (auto-reconnect on
    transport disconnect), so per-process restart-on-death matters
    only in the rare case where the binary itself crashes.
  - Letting daemon.py keep the pid/log invariants means
    ``runner.py down`` and ``harness-cli ls`` see the same state
    they always did. The auto-start trigger is purely additive.

Usage::

    scripts/runner-autostart.py register [--tag TAG] [runner.py flags...]
    scripts/runner-autostart.py unregister [--tag TAG]
    scripts/runner-autostart.py status [--tag TAG]

``register`` also brings the entry up immediately (Linux:
``systemctl --user enable --now``; Windows: ``Start-ScheduledTask``),
so you don't have to sign out/in or reboot just to test the new
autostart entry. Pass ``--no-start`` to register the trigger only.

``unregister`` symmetrically stops the running runner before
removing the trigger (Linux: ``systemctl --user disable --now``
fires ExecStop = ``runner.py down``; Windows: explicit
``runner.py down`` then ``Unregister-ScheduledTask``). Pass
``--no-stop`` to leave the running runner alone.

``status`` without ``--tag`` lists every registered entry with its
current state (summary form); with ``--tag`` it prints detail for the
single entry.

Examples::

    scripts/runner-autostart.py register --tag pdf2md \\
        --roots /home/user/workspace/pdf2md --max-tasks 4
    scripts/runner-autostart.py register \\
        --hostname gmkhost-bash --no-worktree --claude-bin bash \\
        --roots /home/user/workspace

Server CID:
    ``--server-cid <cid>`` is forwarded verbatim if present in the
    flags. Otherwise this script reads ``$HARNESS_SERVER_CID`` at
    register time and rewrites the trailing ``-<digits>`` instance
    suffix to ``-*`` (so the runner reconnects across server
    restarts). If neither is set, register aborts.
"""

from __future__ import annotations

import argparse
import os
import re
import shlex
import subprocess
import sys
from pathlib import Path

_HERE = Path(__file__).resolve().parent
_ROOT = _HERE.parent
_PREFIX = "harness-agent-runner"


def _slot_name(tag: str) -> str:
    """Stable identifier shared by Linux unit name / Windows task name."""
    return _PREFIX if not tag else f"{_PREFIX}-{tag}"


def _runner_tag_flags(tag: str) -> list[str]:
    return ["--as", tag] if tag else []


def _resolve_server_cid(args: list[str]) -> str:
    """Find --server-cid in args, or derive from $HARNESS_SERVER_CID.

    The env value is a specific instance id like ``ws:host:port-12982``;
    its trailing ``-<digits>`` is rewritten to ``-*`` so the runner
    reconnects across server restarts. Raise when neither source has
    a value.
    """
    for i, a in enumerate(args):
        if a == "--server-cid" and i + 1 < len(args):
            return args[i + 1]
        if a.startswith("--server-cid="):
            return a.split("=", 1)[1]
    env = os.environ.get("HARNESS_SERVER_CID", "")
    if not env:
        raise SystemExit(
            "error: --server-cid not in args and $HARNESS_SERVER_CID is unset.\n"
            "       Pass --server-cid explicitly or export HARNESS_SERVER_CID."
        )
    return re.sub(r"-\d+$", "-*", env)


def _ensure_server_cid_in_args(args: list[str], cid: str) -> list[str]:
    for a in args:
        if a == "--server-cid" or a.startswith("--server-cid="):
            return args
    return ["--server-cid", cid, *args]


def _runner_py() -> Path:
    return _HERE / "runner.py"


# --------------------------------------------------------------- Windows

def _ps_quote(s: str) -> str:
    """Quote a token for a PowerShell single-quoted string literal."""
    return "'" + s.replace("'", "''") + "'"


def _powershell(script: str) -> int:
    """Run a PowerShell script block; stream stdout/stderr to ours."""
    r = subprocess.run(
        ["powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass",
         "-Command", script],
        capture_output=True, text=True,
    )
    sys.stdout.write(r.stdout)
    sys.stderr.write(r.stderr)
    return r.returncode


def _register_windows(tag: str, args: list[str], start_now: bool) -> int:
    task = _slot_name(tag)
    python_exe = sys.executable or "python.exe"
    runner_args = ["up", *_runner_tag_flags(tag), *args]
    # Windows-style cmdline for the -Argument property (single string
    # that python.exe will reparse via CommandLineToArgvW).
    cmdline = subprocess.list2cmdline([str(_runner_py()), *runner_args])
    start_block = (
        f"Start-ScheduledTask -TaskName {_ps_quote(task)}\n"
        if start_now
        else ""
    )
    final_msg = (
        "registered + started: " if start_now else "registered (not started): "
    )

    ps = f"""
$action = New-ScheduledTaskAction `
    -Execute {_ps_quote(python_exe)} `
    -Argument {_ps_quote(cmdline)} `
    -WorkingDirectory {_ps_quote(str(_ROOT))}

$trigger = New-ScheduledTaskTrigger -AtLogOn -User $env:USERNAME

$settings = New-ScheduledTaskSettingsSet `
    -MultipleInstances IgnoreNew `
    -AllowStartIfOnBatteries `
    -DontStopIfGoingOnBatteries `
    -StartWhenAvailable `
    -RestartInterval (New-TimeSpan -Minutes 5) `
    -RestartCount 3

Register-ScheduledTask `
    -TaskName {_ps_quote(task)} `
    -Action $action -Trigger $trigger -Settings $settings `
    -Force | Out-Null

{start_block}Write-Output ({_ps_quote(final_msg)} + {_ps_quote(task)})
"""
    return _powershell(ps)


def _unregister_windows(tag: str, stop_first: bool) -> int:
    """Remove the scheduled task; optionally `runner.py down` the daemon first.

    Task Scheduler doesn't manage the spawned agent-runner directly
    (the action is "fire-and-forget" — Task Scheduler considers itself
    done once runner.py up returns), so to actually stop the daemon
    we have to invoke runner.py down ourselves. Without --no-stop,
    that's the default symmetry with Linux's ``systemctl --user
    disable --now`` (which fires ExecStop = runner.py down).
    """
    task = _slot_name(tag)
    if stop_first:
        subprocess.run(
            [sys.executable, str(_runner_py()), "down",
             *_runner_tag_flags(tag)]
        )
    ps = (
        f"Unregister-ScheduledTask -TaskName {_ps_quote(task)} "
        f"-Confirm:$false; Write-Output ('unregistered: ' + {_ps_quote(task)})"
    )
    return _powershell(ps)


def _status_windows(tag: str | None) -> int:
    if tag is None:
        ps = (
            f"Get-ScheduledTask | Where-Object {{ $_.TaskName -like "
            f"'{_PREFIX}*' }} | Format-Table TaskName,State -AutoSize"
        )
    else:
        ps = (
            f"Get-ScheduledTask -TaskName {_ps_quote(_slot_name(tag))} "
            f"| Get-ScheduledTaskInfo | Format-List"
        )
    return _powershell(ps)


# --------------------------------------------------------------- Linux

def _systemd_unit_dir() -> Path:
    base = Path(os.environ.get("XDG_CONFIG_HOME", Path.home() / ".config"))
    d = base / "systemd" / "user"
    d.mkdir(parents=True, exist_ok=True)
    return d


def _systemd_unit_name(tag: str) -> str:
    return f"{_slot_name(tag)}.service"


def _register_linux(tag: str, args: list[str], start_now: bool) -> int:
    """Write a systemd user service and enable it.

    Type=oneshot + RemainAfterExit=yes is intentional: runner.py up
    spawns its detached child and exits 0, but the daemon keeps
    running under daemon.py's pid file. Treating the unit as
    persistent after the oneshot lets `systemctl --user stop` route
    through ExecStop to runner.py down for a clean shutdown.

    Note for laptops / headless hosts without a persistent graphical
    session: the user manager only runs while you're logged in
    unless `loginctl enable-linger $USER` is set. We print the
    command rather than running it (root + system state change).
    """
    unit_name = _systemd_unit_name(tag)
    unit_path = _systemd_unit_dir() / unit_name
    up_args = ["up", *_runner_tag_flags(tag), *args]
    down_args = ["down", *_runner_tag_flags(tag)]
    exec_start = shlex.join([str(_runner_py()), *up_args])
    exec_stop = shlex.join([str(_runner_py()), *down_args])

    body = (
        f"[Unit]\n"
        f"Description=harness agent-runner ({_slot_name(tag)})\n"
        f"After=network-online.target\n"
        f"Wants=network-online.target\n"
        f"\n"
        f"[Service]\n"
        f"Type=oneshot\n"
        f"RemainAfterExit=yes\n"
        f"WorkingDirectory={_ROOT}\n"
        f"ExecStart={exec_start}\n"
        f"ExecStop={exec_stop}\n"
        f"\n"
        f"[Install]\n"
        f"WantedBy=default.target\n"
    )
    unit_path.write_text(body)
    print(f"wrote {unit_path}")

    rc = subprocess.run(["systemctl", "--user", "daemon-reload"]).returncode
    if rc != 0:
        return rc
    enable_cmd = ["systemctl", "--user", "enable"]
    if start_now:
        enable_cmd.append("--now")
    enable_cmd.append(unit_name)
    rc = subprocess.run(enable_cmd).returncode
    if rc != 0:
        return rc

    state = "registered + started" if start_now else "registered (not started)"
    print(f"{state}: {unit_name}")
    print(
        "note: if this host has no persistent login session "
        "(headless / laptop closed at boot), run\n"
        "      sudo loginctl enable-linger $USER\n"
        "      so the user manager starts at boot, not at first login."
    )
    return 0


def _unregister_linux(tag: str, stop_first: bool) -> int:
    """Disable + remove the systemd user unit.

    Default (stop_first=True): ``systemctl --user disable --now``
    runs ExecStop = ``runner.py down`` so the agent-runner exits.
    With --no-stop the ``--now`` is dropped, so the unit is detached
    from auto-start triggers but the running runner is untouched
    (use ``runner.py down`` later if you want it gone).
    """
    unit_name = _systemd_unit_name(tag)
    disable_cmd = ["systemctl", "--user", "disable"]
    if stop_first:
        disable_cmd.append("--now")
    disable_cmd.append(unit_name)
    subprocess.run(disable_cmd, stderr=subprocess.DEVNULL)
    unit_path = _systemd_unit_dir() / unit_name
    try:
        unit_path.unlink()
        print(f"removed {unit_path}")
    except FileNotFoundError:
        pass
    subprocess.run(["systemctl", "--user", "daemon-reload"])
    state = "unregistered + stopped" if stop_first else "unregistered (left running)"
    print(f"{state}: {unit_name}")
    return 0


def _status_linux(tag: str | None) -> int:
    if tag is None:
        units = sorted(_systemd_unit_dir().glob(f"{_PREFIX}*.service"))
        if not units:
            print("(no harness-agent-runner units registered)")
            return 0
        for u in units:
            r = subprocess.run(
                ["systemctl", "--user", "is-active", u.name],
                capture_output=True, text=True,
            )
            print(f"{u.name:50s}  {r.stdout.strip()}")
        return 0
    return subprocess.run(
        ["systemctl", "--user", "status",
         _systemd_unit_name(tag), "--no-pager"]
    ).returncode


# --------------------------------------------------------------- dispatch

def main(argv: list[str]) -> int:
    ap = argparse.ArgumentParser(
        prog="runner-autostart.py",
        description="Register agent-runner to auto-start on login/boot.",
    )
    sub = ap.add_subparsers(dest="cmd", required=True)

    p_reg = sub.add_parser("register", help="register autostart entry")
    p_reg.add_argument("--tag", default="",
                       help="slot tag (default: primary slot, no tag)")
    p_reg.add_argument(
        "--no-start", action="store_true",
        help="register only; don't bring the runner up right now "
        "(wait for the next login / reboot to trigger it)",
    )

    p_unreg = sub.add_parser("unregister", help="remove autostart entry")
    p_unreg.add_argument("--tag", default="")
    p_unreg.add_argument(
        "--no-stop", action="store_true",
        help="remove the autostart trigger only; leave the running "
        "agent-runner alone (use `runner.py down` later if you want it "
        "gone)",
    )

    p_status = sub.add_parser(
        "status",
        help="show autostart entry state (no --tag: summary of all entries)",
    )
    p_status.add_argument("--tag", default=None)

    args, rest = ap.parse_known_args(argv)

    if args.cmd == "register":
        cid = _resolve_server_cid(rest)
        rest_with_cid = _ensure_server_cid_in_args(rest, cid)
        start_now = not args.no_start
        if os.name == "nt":
            return _register_windows(args.tag, rest_with_cid, start_now)
        return _register_linux(args.tag, rest_with_cid, start_now)
    if args.cmd == "unregister":
        stop_first = not args.no_stop
        if os.name == "nt":
            return _unregister_windows(args.tag, stop_first)
        return _unregister_linux(args.tag, stop_first)
    if args.cmd == "status":
        if os.name == "nt":
            return _status_windows(args.tag)
        return _status_linux(args.tag)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
