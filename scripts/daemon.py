"""Cross-platform daemon control: detached up/down/owner-verify.

Mirrors the Bash ``_daemon.sh`` primitives:

  - ``daemon_up(slot, bin_name, *args)``   spawn ``bin/<bin_name>`` detached,
                                           record state under <slot>
  - ``daemon_down(slot, bin_name)``        graceful terminate, escalate to
                                           hard kill after 5s
  - ``verify_owner(pid, bin_name)``        does the running pid still belong
                                           to <bin_name>?

State files live under ``<repo>/bin/.run/<slot>.{pid,log}``. The slot is the
*instance* identity (single-instance daemons use slot == bin_name; multi-
instance daemons use a tagged slot like "agent-runner-2"). ``bin/`` is
gitignored, so pid/log artefacts don't pollute the working tree.

This module is interchangeable with the Bash version: pid/log files have
the same paths and semantics, so a daemon started via ``runner.sh`` can be
stopped via ``runner.py`` and vice versa.
"""

from __future__ import annotations

import os
import signal
import subprocess
import sys
import time
from pathlib import Path

import psutil

_HERE = Path(__file__).resolve().parent
_ROOT = _HERE.parent

RUN_DIR = _ROOT / "bin" / ".run"
BIN_DIR = _ROOT / "bin"


def bin_basename(bin_name: str) -> str:
    """Append ``.exe`` on Windows when missing."""
    if os.name == "nt" and not bin_name.lower().endswith(".exe"):
        return f"{bin_name}.exe"
    return bin_name


def bin_path(bin_name: str) -> Path:
    return BIN_DIR / bin_basename(bin_name)


def pid_file(slot: str) -> Path:
    return RUN_DIR / f"{slot}.pid"


def log_file(slot: str) -> Path:
    return RUN_DIR / f"{slot}.log"


def restart_log(slot: str) -> Path:
    return RUN_DIR / f"{slot}.restart.log"


def _proc_basename(p: psutil.Process) -> str:
    """Best-effort image basename for a running process — mirrors
    ``readlink /proc/<pid>/exe -> basename`` on Linux. Falls back to
    ``p.name()`` when ``exe()`` is unavailable. Returns "" on failure."""
    try:
        exe = p.exe()
    except (psutil.AccessDenied, psutil.NoSuchProcess, FileNotFoundError, OSError):
        try:
            return p.name() or ""
        except (psutil.AccessDenied, psutil.NoSuchProcess):
            return ""
    base = os.path.basename(exe) if exe else ""
    if base.endswith(" (deleted)"):
        base = base[: -len(" (deleted)")].rstrip()
    return base


def verify_owner(pid: int, bin_name: str) -> bool:
    """Return True iff *pid* is alive and its image basename matches *bin_name*."""
    try:
        p = psutil.Process(pid)
    except (psutil.NoSuchProcess, ValueError):
        return False
    expected = bin_basename(bin_name)
    actual = _proc_basename(p)
    return actual == expected


def _binary_is_executable(path: Path) -> bool:
    if not path.is_file():
        return False
    if os.name == "nt":
        return True  # Windows treats .exe as inherently executable
    return os.access(str(path), os.X_OK)


def _spawn_detached(args: list[str], log_path: Path) -> int:
    """Start *args* as a detached background process; return its pid.

    - stdout / stderr appended to *log_path*; stdin /dev/null.
    - Detached from controlling terminal: setsid on Unix, DETACHED_PROCESS
      | CREATE_NEW_PROCESS_GROUP on Windows.
    """
    log_path.parent.mkdir(parents=True, exist_ok=True)
    log_fh = open(log_path, "ab")
    try:
        popen_kwargs: dict = dict(
            stdin=subprocess.DEVNULL,
            stdout=log_fh,
            stderr=subprocess.STDOUT,
            close_fds=True,
            cwd=str(_ROOT),
        )
        if os.name == "nt":
            popen_kwargs["creationflags"] = (
                subprocess.DETACHED_PROCESS | subprocess.CREATE_NEW_PROCESS_GROUP
            )
        else:
            popen_kwargs["start_new_session"] = True  # setsid()
        p = subprocess.Popen(args, **popen_kwargs)
    finally:
        log_fh.close()
    return p.pid


def daemon_up(slot: str, bin_name: str, *args: str) -> int:
    """Start *bin_name* detached, recording state under *slot*.

    Returns the pid (existing if already running, else newly started).
    Raises ``FileNotFoundError`` if the binary is missing / not executable,
    or ``RuntimeError`` if the process exits within 0.5s of start.
    """
    RUN_DIR.mkdir(parents=True, exist_ok=True)
    pf = pid_file(slot)
    lf = log_file(slot)

    if pf.exists():
        try:
            prev = int(pf.read_text().strip())
        except ValueError:
            prev = 0
        if prev > 0 and verify_owner(prev, bin_name):
            print(f"[{slot}] already running (pid={prev}, log={lf})")
            return prev
        try:
            pf.unlink()
        except FileNotFoundError:
            pass

    bp = bin_path(bin_name)
    if not _binary_is_executable(bp):
        sys.stderr.write(f"[{slot}] binary missing or not executable: {bp}\n")
        sys.stderr.write("        run 'make build' first\n")
        raise FileNotFoundError(str(bp))

    pid = _spawn_detached([str(bp), *args], lf)
    pf.write_text(str(pid))

    # Catch immediate crashes (bad flag, port already bound, ...).
    time.sleep(0.5)
    if not psutil.pid_exists(pid):
        sys.stderr.write(f"[{slot}] failed to start; tail of log:\n")
        try:
            data = lf.read_bytes()[-4000:]
            for line in data.decode("utf-8", "replace").splitlines()[-20:]:
                sys.stderr.write(line + "\n")
        except OSError:
            pass
        try:
            pf.unlink()
        except FileNotFoundError:
            pass
        raise RuntimeError(f"{slot} failed to start")

    print(f"[{slot}] started pid={pid} log={lf}")
    return pid


def _graceful_terminate(p: psutil.Process) -> None:
    """Send a "please exit" signal that the target can catch.

    - Unix: ``SIGTERM`` via ``psutil.Process.terminate()``.
    - Windows: ``CTRL_BREAK_EVENT`` (works because the daemon was started
      with ``CREATE_NEW_PROCESS_GROUP``). Falls back to ``TerminateProcess``
      if delivery fails.
    """
    try:
        if os.name == "nt":
            try:
                p.send_signal(signal.CTRL_BREAK_EVENT)
                return
            except (OSError, ValueError, AttributeError):
                pass
        p.terminate()
    except psutil.NoSuchProcess:
        return


def daemon_down(slot: str, bin_name: str, *, timeout: float = 5.0) -> None:
    """Send graceful terminate to *slot*; escalate to hard kill after *timeout*."""
    pf = pid_file(slot)
    if not pf.exists():
        print(f"[{slot}] not running (no pid file)")
        return
    try:
        pid = int(pf.read_text().strip())
    except ValueError:
        pid = 0
    if pid <= 0 or not psutil.pid_exists(pid):
        print(f"[{slot}] not running (stale pid file)")
        try:
            pf.unlink()
        except FileNotFoundError:
            pass
        return
    if not verify_owner(pid, bin_name):
        sys.stderr.write(
            f"[{slot}] pid {pid} no longer belongs to {bin_name}; refusing to signal\n"
        )
        sys.stderr.write(
            f"        remove {pf} manually if the binary was renamed\n"
        )
        raise RuntimeError(f"owner mismatch for {slot}")

    try:
        p = psutil.Process(pid)
    except psutil.NoSuchProcess:
        try:
            pf.unlink()
        except FileNotFoundError:
            pass
        print(f"[{slot}] not running (vanished before signal)")
        return

    _graceful_terminate(p)
    try:
        p.wait(timeout=timeout)
    except psutil.TimeoutExpired:
        sys.stderr.write(
            f"[{slot}] graceful terminate timeout after {timeout:.0f}s, hard-killing pid={pid}\n"
        )
        try:
            p.kill()
            p.wait(timeout=2.0)
        except (psutil.NoSuchProcess, psutil.TimeoutExpired):
            pass
    try:
        pf.unlink()
    except FileNotFoundError:
        pass
    print(f"[{slot}] stopped")
