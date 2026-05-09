#!/usr/bin/env python3
"""restart.py — detached restart of a daemon, preserving flags and CWD.

Cross-platform port of ``scripts/restart.sh``. Reads the running pid's
argv, cwd and exe path via psutil so it works without ``/proc``, and
re-launches via a detached subprocess so the restart sequence survives
even when invoked from a child of the daemon being restarted (e.g. a
Claude Code agent restarting its own agent-runner parent).

Usage::

    python scripts/restart.py <slot>

``<slot>`` is the ``bin/.run/<slot>.pid`` name — i.e. the binary name for
primary instances (``agent-runner``, ``harness-server``) or
``<binary>-<tag>`` for additional instances. Flags, CWD, and the
underlying binary are read from the running process so the new instance
comes up identically.

Examples::

    python scripts/restart.py agent-runner
    python scripts/restart.py agent-runner-2
    python scripts/restart.py harness-server

Output and the restart sequence's stdout/stderr go to
``bin/.run/<slot>.restart.log``; tail it to confirm completion.
"""

from __future__ import annotations

import datetime as _dt
import os
import subprocess
import sys
import time
from pathlib import Path

from bootstrap import ensure_venv

ensure_venv()

import psutil

import daemon as _daemon

_DETACHED_FLAG = "--__detached"


def _spawn_detached_child(args: list[str], log_path: Path) -> int:
    log_path.parent.mkdir(parents=True, exist_ok=True)
    log_fh = open(log_path, "ab")
    try:
        popen_kwargs: dict = dict(
            stdin=subprocess.DEVNULL,
            stdout=log_fh,
            stderr=subprocess.STDOUT,
            close_fds=True,
        )
        if os.name == "nt":
            popen_kwargs["creationflags"] = (
                subprocess.DETACHED_PROCESS | subprocess.CREATE_NEW_PROCESS_GROUP
            )
        else:
            popen_kwargs["start_new_session"] = True
        p = subprocess.Popen(args, **popen_kwargs)
    finally:
        log_fh.close()
    return p.pid


def _read_proc_state(pid: int) -> tuple[str, list[str], str]:
    """Return ``(bin_basename, flags_after_argv0, cwd)`` for a running pid.

    ``bin_basename`` is normalised to drop a Windows ``.exe`` suffix and a
    Linux ``" (deleted)"`` marker so the value matches the expected
    ``bin_name`` argument that ``daemon_up`` / ``daemon_down`` take.
    """
    proc = psutil.Process(pid)
    try:
        exe = proc.exe()
    except (psutil.AccessDenied, FileNotFoundError, OSError):
        exe = ""
    bin_name = os.path.basename(exe) if exe else proc.name()
    if bin_name.endswith(" (deleted)"):
        bin_name = bin_name[: -len(" (deleted)")].rstrip()
    if os.name == "nt" and bin_name.lower().endswith(".exe"):
        bin_name = bin_name[: -len(".exe")]
    cmdline = proc.cmdline()
    flags = list(cmdline[1:]) if len(cmdline) > 1 else []
    cwd = proc.cwd()
    return bin_name, flags, cwd


def _ts() -> str:
    return _dt.datetime.now().astimezone().isoformat(timespec="seconds")


def _do_restart(slot: str, bin_name: str, flags: list[str], orig_cwd: str) -> None:
    """Run inside the detached child: down, sleep, up. All output → restart log."""
    log = _daemon.restart_log(slot)
    log.parent.mkdir(parents=True, exist_ok=True)
    with open(log, "a", encoding="utf-8") as fh:

        def emit(msg: str) -> None:
            fh.write(msg + "\n")
            fh.flush()

        emit(
            f"[{_ts()}] restart {slot} (bin={bin_name}) begin "
            f"(cwd={orig_cwd} flags={' '.join(flags)})"
        )
        try:
            os.chdir(orig_cwd)
        except OSError as e:
            emit(f"[{slot}] chdir({orig_cwd}) failed: {e}")

        # daemon_up/down print to stdout/stderr; redirect them into the log.
        old_stdout, old_stderr = sys.stdout, sys.stderr
        sys.stdout = fh
        sys.stderr = fh
        try:
            try:
                _daemon.daemon_down(slot, bin_name)
            except Exception as e:
                emit(f"[{slot}] daemon_down failed: {e}")
                return
            time.sleep(1.0)
            try:
                _daemon.daemon_up(slot, bin_name, *flags)
            except Exception as e:
                emit(f"[{slot}] daemon_up failed: {e}")
                return
        finally:
            sys.stdout = old_stdout
            sys.stderr = old_stderr
        emit(f"[{_ts()}] restart {slot} end")


def _usage_and_exit() -> None:
    sys.stderr.write(
        "usage: restart.py <slot>   "
        "(e.g. agent-runner, agent-runner-2, harness-server)\n"
    )
    sys.exit(2)


def main(argv: list[str]) -> int:
    # Detached child mode: argv = [_DETACHED_FLAG, slot, bin_name, cwd, *flags]
    if argv and argv[0] == _DETACHED_FLAG:
        if len(argv) < 4:
            return 2
        slot = argv[1]
        bin_name = argv[2]
        cwd = argv[3]
        flags = list(argv[4:])
        _do_restart(slot, bin_name, flags, cwd)
        return 0

    if len(argv) != 1:
        _usage_and_exit()
    slot = argv[0]

    pf = _daemon.pid_file(slot)
    if not pf.is_file():
        sys.stderr.write(
            f"[{slot}] no pid file at {pf} (daemon not currently managed); "
            "start it via the per-daemon helper first\n"
        )
        return 1
    try:
        pid = int(pf.read_text().strip())
    except ValueError:
        sys.stderr.write(f"[{slot}] pid file unreadable\n")
        return 1
    if not psutil.pid_exists(pid):
        sys.stderr.write(
            f"[{slot}] pid file present but pid={pid} not running; "
            f"clean up {pf} and start fresh\n"
        )
        return 1

    try:
        bin_name, flags, cwd = _read_proc_state(pid)
    except (psutil.NoSuchProcess, psutil.AccessDenied) as e:
        sys.stderr.write(f"[{slot}] cannot inspect pid={pid}: {e}\n")
        return 1

    log = _daemon.restart_log(slot)
    log.parent.mkdir(parents=True, exist_ok=True)

    child_argv = [
        sys.executable,
        os.path.abspath(__file__),
        _DETACHED_FLAG,
        slot,
        bin_name,
        cwd,
        *flags,
    ]
    child_pid = _spawn_detached_child(child_argv, log)
    print(
        f"[{slot}] detached restart kicked off "
        f"(subshell pid={child_pid}, bin={bin_name}, log={log})"
    )
    print(f"[{slot}] follow with: tail -f {log}")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
