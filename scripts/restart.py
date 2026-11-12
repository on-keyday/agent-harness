#!/usr/bin/env python3
"""restart.py — detached restart of a daemon, preserving flags and CWD.

Cross-platform port of ``scripts/restart.sh``. Reads the running pid's
argv, cwd and exe path via psutil so it works without ``/proc``, and
re-launches via a detached subprocess so the restart sequence survives
even when invoked from a child of the daemon being restarted (e.g. a
Claude Code agent restarting its own agent-runner parent).

Usage::

    python scripts/restart.py <slot> [override flags...]

``<slot>`` is the ``bin/.run/<slot>.pid`` name — i.e. the binary name for
primary instances (``agent-runner``, ``harness-server``) or
``<binary>-<tag>`` for additional instances. Flags, CWD, and the
underlying binary are read from the running process so the new instance
comes up identically. When the daemon is no longer running (it crashed),
they are read from ``bin/.run/<slot>.argv.json`` instead, which
``daemon.daemon_up`` writes after every successful start — so the same
command brings a dead daemon back with its last working flags.

Anything after the slot is an *override*: each ``--flag`` named there is
stripped from the inherited argv and the overrides are appended, so the
new instance comes up with just those flags changed. The main use case is
secret rotation without retyping the full flag set::

    python scripts/restart.py harness-server --operator-psk NEWSECRET
    python scripts/restart.py harness-server --operator-psk-file /path/to/new-psk

``--operator-psk`` and ``--operator-psk-file`` are one setting: overriding
either strips both, so switching from the value form to the file form does
not leave the old value behind (the server prefers a value over a file).

``--unset NAME`` (repeatable; restart.py's own, never passed on) strips a
flag without adding anything back, so the daemon falls back to its default.
It strips NAME's group too: ``--unset operator-psk`` leaves the server with
neither form, and it then reads ``<data-dir>/operator-psk`` — write the secret
there FIRST, or the server generates a new one into it::

    python scripts/restart.py harness-server --unset operator-psk
(``--operator-psk`` values are redacted in the restart log,
but the value form is still visible in ``ps`` on the new process — use
the ``-file`` form when that matters.)

Examples::

    python scripts/restart.py agent-runner
    python scripts/restart.py agent-runner-2 --max-tasks 4
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

# Flag names whose values are secrets: redacted in the restart log's
# "flags=" line so a rotation doesn't archive the new secret in plaintext.
_SECRET_FLAGS = frozenset({"operator-psk"})

# The server asks its runners to keep their children alive before it goes down,
# collects their answers, drains each held session and writes the result — and
# daemon_down's default hard-kills five seconds after the graceful signal. A
# SIGKILL mid-sequence is the CRASH case, which by design recovers nothing, so
# whatever had not been written yet silently degrades to the old behaviour.
#
# 15s is the server's budget: room for the ack window (1.5s), the drains and
# the per-session captures, with slack for a fleet larger than this one.
# Runners keep the 5s default — they have no such sequence.
_SERVER_DOWN_TIMEOUT = 15.0


def _down_timeout(bin_name: str) -> float:
    """Graceful-shutdown budget for *bin_name* before the hard kill."""
    if bin_name == "harness-server":
        return _SERVER_DOWN_TIMEOUT
    return 5.0



def _override_names(overrides: list[str]) -> list[str]:
    """The ``--flag`` names present in *overrides* (``--name`` / ``--name=v``)."""
    names: list[str] = []
    for tok in overrides:
        if tok.startswith("--"):
            name = tok[2:].split("=", 1)[0]
            if name:
                names.append(name)
    return names


# Flags that set ONE thing two ways. Overriding any member strips every member:
# harness-server takes --operator-psk over --operator-psk-file, so overriding
# only the file would leave the inherited value in force (and in ps).
_FLAG_GROUPS = (frozenset({"operator-psk", "operator-psk-file"}),)


_UNSET = "unset"


def _split_unset(overrides: list[str]) -> tuple[list[str], list[str]]:
    """Separate restart.py's own ``--unset NAME`` / ``--unset=NAME`` from the
    overrides passed on to the daemon. Returns ``(overrides, unset_names)``."""
    rest: list[str] = []
    unset: list[str] = []
    i = 0
    while i < len(overrides):
        tok = overrides[i]
        if tok == f"--{_UNSET}":
            if i + 1 >= len(overrides) or overrides[i + 1].startswith("--"):
                raise ValueError("--unset needs a flag name (e.g. --unset operator-psk)")
            unset.append(overrides[i + 1].lstrip("-"))
            i += 2
            continue
        if tok.startswith(f"--{_UNSET}="):
            name = tok.split("=", 1)[1].lstrip("-")
            if not name:
                raise ValueError("--unset needs a flag name (e.g. --unset=operator-psk)")
            unset.append(name)
            i += 1
            continue
        rest.append(tok)
        i += 1
    return rest, unset


def _merge_flags(inherited: list[str], overrides: list[str], unset: list[str] = ()) -> list[str]:
    """Inherited argv with every override-named and *unset* flag (and its
    group, see ``_FLAG_GROUPS``) stripped, overrides appended. Appending alone
    would usually work (Go flag parsing is last-wins), but stripping keeps
    ``ps`` output honest — no stale ``--operator-psk`` value lingering in
    front of the new one."""
    names = set(_override_names(overrides)) | set(unset)
    for group in _FLAG_GROUPS:
        if names & group:
            names |= group
    merged = list(inherited)
    for name in sorted(names):
        merged = _daemon._strip_flag(merged, name)
    return merged + list(overrides)


def _redact(flags: list[str]) -> list[str]:
    """Copy of *flags* with secret flag values replaced by ``<redacted>``."""
    out: list[str] = []
    i = 0
    while i < len(flags):
        tok = flags[i]
        name = tok[2:].split("=", 1)[0] if tok.startswith("--") else ""
        if name in _SECRET_FLAGS:
            if "=" in tok:
                out.append(f"--{name}=<redacted>")
                i += 1
            else:
                out.append(tok)
                if i + 1 < len(flags) and not flags[i + 1].startswith("--"):
                    out.append("<redacted>")
                    i += 1
                i += 1
            continue
        out.append(tok)
        i += 1
    return out


def _spawn_detached_child(args: list[str], log_path: Path) -> int:
    log_path.parent.mkdir(parents=True, exist_ok=True)
    log_fh = open(log_path, "ab")
    try:
        # Scrub claude-code session markers (CLAUDE_CODE_CHILD_SESSION etc.) from
        # the env so a restart invoked from inside a claude session (/restart-all
        # run in a conversation) doesn't leak them into the runner and suppress
        # spawned agents' local transcripts. Shares daemon._clean_child_env so the
        # `up` and `restart` paths strip the same set. See daemon.py for rationale.
        popen_kwargs: dict = dict(
            stdin=subprocess.DEVNULL,
            stdout=log_fh,
            stderr=subprocess.STDOUT,
            close_fds=True,
            env=_daemon._clean_child_env(),
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
            f"(cwd={orig_cwd} flags={' '.join(_redact(flags))})"
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
                _daemon.daemon_down(slot, bin_name, timeout=_down_timeout(bin_name))
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


def _live_pid(slot: str, recorded_bin: str | None) -> int | None:
    """The slot's pid if it is running and (when a record names the binary)
    still that binary — a pid file left by a crash can point at a reused pid."""
    try:
        pid = int(_daemon.pid_file(slot).read_text().strip())
    except (FileNotFoundError, ValueError):
        return None
    if pid <= 0 or not psutil.pid_exists(pid):
        return None
    if recorded_bin is not None and not _daemon.verify_owner(pid, recorded_bin):
        return None
    return pid


def _resolve_state(slot: str) -> tuple[str, list[str], str] | None:
    """``(bin_name, flags, cwd)`` to restart *slot* with: from the live
    process when there is one, else from its last recorded start. Reports
    and returns None when neither exists."""
    rec = _daemon.recorded_argv(slot)
    pid = _live_pid(slot, rec[0] if rec else None)
    if pid is not None:
        try:
            return _read_proc_state(pid)
        except (psutil.NoSuchProcess, psutil.AccessDenied) as e:
            if rec is None:
                sys.stderr.write(f"[{slot}] cannot inspect pid={pid}: {e}\n")
                return None
    if rec is None:
        sys.stderr.write(
            f"[{slot}] not running and no recorded start at {_daemon.argv_file(slot)}; "
            "start it via the per-daemon helper first\n"
        )
        return None
    bin_name, cwd, flags = rec
    print(f"[{slot}] not running; restarting with the flags recorded at its last start "
          f"({_daemon.argv_file(slot)})")
    return bin_name, flags, cwd


def _usage_and_exit() -> None:
    sys.stderr.write(
        "usage: restart.py <slot> [override flags...] [--unset NAME ...]   "
        "(e.g. agent-runner, harness-server --operator-psk NEW)\n"
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

    if not argv:
        _usage_and_exit()
    slot = argv[0]
    overrides = list(argv[1:])
    if overrides and not overrides[0].startswith("--"):
        _usage_and_exit()  # a second positional is a typo, not an override
    try:
        overrides, unset = _split_unset(overrides)
    except ValueError as e:
        sys.stderr.write(f"[{slot}] {e}\n")
        return 2

    state = _resolve_state(slot)
    if state is None:
        return 1
    bin_name, flags, cwd = state
    if overrides or unset:
        flags = _merge_flags(flags, overrides, unset)

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
