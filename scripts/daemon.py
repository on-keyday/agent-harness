"""Cross-platform daemon control: detached up/down/owner-verify.

Canonical implementation of the up/down/restart primitives used by
``runner.py`` / ``server.py`` / ``restart.py`` (and, transitively, by
the matching ``.sh`` thin wrappers):

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


def shutdown_file(slot: str) -> Path:
    """Sentinel file watched by agent-runner / harness-server's
    ``--shutdown-file`` poller. Touching it requests a graceful exit —
    used on platforms (Windows) where SIGTERM can't reach the spawned
    DETACHED_PROCESS child."""
    return RUN_DIR / f"{slot}.shutdown"


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


# Env vars claude-code injects into its child processes to mark "you are running
# under claude" (a nested/child session). If runner.py is launched from inside a
# claude session — e.g. /restart-all run in a conversation — these leak into the
# detached agent-runner and then into every agent it spawns. CLAUDE_CODE_CHILD_SESSION
# in particular makes a spawned interactive claude treat itself as a child session
# and write NO local transcript (~/.claude/projects/<slug>/<uuid>.jsonl), proven by
# a controlled toggle test. Scrub them so the runner and its descendants start from
# a clean session identity regardless of who launched runner.py. (CLAUDE_CONFIG_DIR
# is deliberately preserved — it is real config, not a session marker.)
_CLAUDE_MARKER_PREFIXES = ("CLAUDE_CODE",)
_CLAUDE_MARKER_EXACT = frozenset({"CLAUDECODE", "CLAUDE_EFFORT", "AI_AGENT"})


def _clean_child_env() -> dict[str, str]:
    """os.environ minus claude-code's leaked session markers."""
    return {
        k: v
        for k, v in os.environ.items()
        if k not in _CLAUDE_MARKER_EXACT
        and not any(k.startswith(p) for p in _CLAUDE_MARKER_PREFIXES)
    }


def _spawn_detached(args: list[str], log_path: Path) -> int:
    """Start *args* as a detached background process; return its pid.

    - stdout / stderr appended to *log_path*; stdin /dev/null.
    - Detached from controlling terminal: setsid on Unix, DETACHED_PROCESS
      | CREATE_NEW_PROCESS_GROUP on Windows.
    - Env scrubbed of claude-code session markers (see _clean_child_env).
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
            env=_clean_child_env(),
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


def _strip_flag(args: list[str], name: str) -> list[str]:
    """Remove all occurrences of ``--<name>``, ``--<name> <value>`` and
    ``--<name>=<value>`` from *args*. Used to dedup flags when re-spawning a
    process whose previous argv already contained them (daemon_up's
    ``--shutdown-file`` injection, restart.py's override flags).

    A token following ``--<name>`` is treated as its value ONLY when it does
    not itself start with ``--`` — Go's flag package accepts bare boolean
    flags (``--no-worktree``), and blindly skipping the next token would eat
    the flag that follows a boolean."""
    out: list[str] = []
    long = f"--{name}"
    long_eq = f"--{name}="
    i = 0
    while i < len(args):
        a = args[i]
        if a == long:
            i += 1
            if i < len(args) and not args[i].startswith("--"):
                i += 1  # skip the flag's value
            continue
        if a.startswith(long_eq):
            i += 1
            continue
        out.append(a)
        i += 1
    return out


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

    # Clean up any stale shutdown sentinel from a previous unclean exit
    # before injecting --shutdown-file. Otherwise the freshly spawned
    # binary would observe the leftover file on its first poll and
    # exit immediately.
    sf = shutdown_file(slot)
    try:
        sf.unlink()
    except FileNotFoundError:
        pass

    # Strip any existing --shutdown-file from args before injecting our
    # own. restart.py inherits the running process's full argv via
    # /proc/<pid>/cmdline and passes it back through daemon_up, which
    # would otherwise double-inject (two --shutdown-file flags pointing
    # at the same path — harmless under Go's last-write-wins flag
    # parsing, but ugly in ps output and confusing to debug).
    args = _strip_flag(list(args), "shutdown-file")
    spawn_args = ["--shutdown-file", str(sf), *args]

    pid = _spawn_detached([str(bp), *spawn_args], lf)
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
    - Windows: ``CTRL_BREAK_EVENT`` first (the daemon was started with
      ``CREATE_NEW_PROCESS_GROUP``), then ``TerminateProcess`` as a
      fallback. Note that a daemon spawned with ``DETACHED_PROCESS`` has
      no attached console, so ``GenerateConsoleCtrlEvent`` may return
      success while the event is never delivered to the child — the
      timeout-then-hard-kill path in ``daemon_down`` covers that case.
    """
    if os.name == "nt":
        try:
            p.send_signal(signal.CTRL_BREAK_EVENT)
            return
        except (OSError, ValueError, AttributeError, psutil.AccessDenied):
            # CTRL_BREAK couldn't be delivered (most common cause: the
            # child was started with DETACHED_PROCESS and has no
            # console). Fall through to TerminateProcess via terminate().
            pass
        except psutil.NoSuchProcess:
            return
    try:
        p.terminate()
    except (psutil.NoSuchProcess, psutil.AccessDenied):
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

    # Touch the shutdown sentinel before sending OS-level signals.
    # On Windows this is the only graceful path (CTRL_BREAK_EVENT
    # can't reach a DETACHED_PROCESS child); on Linux SIGTERM still
    # arrives first and the watcher just races with it, harmless
    # either way (cancel is idempotent on the runner side).
    try:
        shutdown_file(slot).touch()
    except OSError:
        pass

    # Windows: give the --shutdown-file watcher (250ms poll) a brief
    # window to detect the sentinel and exit cleanly *before* we fall
    # back to _graceful_terminate. Without this delay TerminateProcess
    # races the watcher and almost always wins, killing the runner
    # before it can send the WS Close frame — which is exactly the
    # behavior the sentinel was added to avoid. On Linux SIGTERM is
    # reliable and fast, so we skip the grace window there.
    if os.name == "nt":
        try:
            p.wait(timeout=2.0)
        except (psutil.TimeoutExpired, psutil.NoSuchProcess):
            pass

    if p.is_running():
        _graceful_terminate(p)
    try:
        p.wait(timeout=timeout)
    except psutil.TimeoutExpired:
        sys.stderr.write(
            f"[{slot}] graceful terminate timeout after {timeout:.0f}s, hard-killing pid={pid}\n"
        )
        try:
            p.kill()
        except (psutil.NoSuchProcess, psutil.AccessDenied):
            pass
        try:
            p.wait(timeout=2.0)
        except (psutil.NoSuchProcess, psutil.TimeoutExpired):
            pass
    except psutil.NoSuchProcess:
        # Process exited before we could observe it — that's success,
        # not a failure. Common on Windows where TerminateProcess is
        # synchronous enough that the handle is gone by the time
        # wait() runs.
        pass
    try:
        pf.unlink()
    except FileNotFoundError:
        pass
    print(f"[{slot}] stopped")
