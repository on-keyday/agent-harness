#!/usr/bin/env python3
"""build_and_restart_all.py -- make build, then restart every alive
agent-runner slot, restarting the slot that owns the calling process LAST.

Why "self last": when this script is invoked from inside a claude-code agent
spawned by an agent-runner, restarting the runner first would SIGHUP the
caller mid-script. Restarting "self" last lets the script complete the
build + restart of every other slot before tearing itself down. The detached
restart in scripts/restart.py keeps the actual restart sequence alive past
the cascade.

Slot discovery: walks bin/.run/agent-runner*.pid; only restarts slots whose
PID is currently alive AND whose binary verifies as agent-runner. Stale
pid files (process gone) are skipped silently — bring them back up via
the relevant scripts/runner.* helper.

Self-detection: walks the calling process's parent chain via psutil; the
first ancestor whose PID matches an agent-runner pid file is "self". When
no such ancestor exists (e.g. a normal dev shell running this manually),
all slots are restarted in arbitrary order.

Debounce / double-restart guard: a successful restart stamps
bin/.run/last-restart-stamp with the wall-clock time. A subsequent run within
--debounce-seconds (default 300) no-ops with a message instead of restarting
again. This exists because a session that self-restarts loses the transcript
record of having run this script (its stdout is SIGHUP'd away mid-call); on
resume it can wrongly conclude "I never restarted" and blindly re-run. The
stamp makes the re-run a safe no-op regardless of how the resume is phrased.
Pass --force to restart anyway (e.g. you rebuilt and genuinely want another
cycle within the window).

--stop-first: stop every slot, build, then start every slot again, all in
one detached process (scripts/stop_build_start.py). For hosts where a
running binary cannot be overwritten -- on Windows `make build` fails on
bin/agent-runner.exe while a runner runs from it. The script returns as soon
as the detached process is spawned; the sequence then finishes even though
the caller dies with its own runner. Every slot is down for the length of the
build, and every task on them dies. Follow bin/.run/build-and-restart-all.log.

Usage:
    python scripts/build_and_restart_all.py
    python scripts/build_and_restart_all.py --skip-build
    python scripts/build_and_restart_all.py --dry-run
    python scripts/build_and_restart_all.py --force
    python scripts/build_and_restart_all.py --stop-first
"""

from __future__ import annotations

import argparse
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
import restart as _restart
import stop_build_start as _sbs

_HERE = Path(__file__).resolve().parent
_ROOT = _HERE.parent
_RESTART_PY = _HERE / "restart.py"
_AGENT_RUNNER_BIN = "agent-runner"
_STAMP = _daemon.RUN_DIR / "last-restart-stamp"
_DEFAULT_DEBOUNCE_SECONDS = 300.0
_DETACHED_FLAG = "--__stop-build-start"
_PLAN = _daemon.RUN_DIR / "build-and-restart-all.plan.json"
_LOG = _daemon.RUN_DIR / "build-and-restart-all.log"


def _read_stamp() -> float | None:
    """Return the wall-clock time of the last restart, or None if unknown."""
    try:
        return float(_STAMP.read_text().strip())
    except (OSError, ValueError):
        return None


def _write_stamp() -> None:
    """Record 'a restart is being performed now' so a resumed re-run debounces.
    Written before the restart loop so it survives this process's own
    self-restart at the tail of that loop."""
    try:
        _STAMP.parent.mkdir(parents=True, exist_ok=True)
        _STAMP.write_text(f"{time.time():.0f}\n")
    except OSError as e:
        print(f"(warning: could not write restart stamp: {e})")


def _alive_runner_slots() -> dict[int, str]:
    """Return {pid: slot} for every bin/.run/agent-runner*.pid whose process
    is alive and verifies as the agent-runner binary."""
    out: dict[int, str] = {}
    for pid_file in sorted(_daemon.RUN_DIR.glob("agent-runner*.pid")):
        slot = pid_file.stem
        try:
            pid = int(pid_file.read_text().strip())
        except (OSError, ValueError):
            continue
        if not psutil.pid_exists(pid):
            continue
        if not _daemon.verify_owner(pid, _AGENT_RUNNER_BIN):
            continue
        out[pid] = slot
    return out


def _find_self_slot(pid_to_slot: dict[int, str]) -> str | None:
    """Walk the calling process's parent chain. Return the slot whose PID
    appears in our ancestry, or None if no agent-runner ancestor exists."""
    try:
        me = psutil.Process(os.getpid())
    except psutil.Error:
        return None
    for ancestor in me.parents():
        if ancestor.pid in pid_to_slot:
            return pid_to_slot[ancestor.pid]
    return None


def _run_make_build() -> None:
    print("==> make build")
    subprocess.check_call(["make", "build"], cwd=str(_ROOT))


def _restart_slot(slot: str) -> None:
    subprocess.check_call([sys.executable, str(_RESTART_PY), slot], cwd=str(_ROOT))


def _capture_slots(pid_to_slot: dict[int, str], order: list[str]) -> list[_sbs.Slot]:
    """Each slot's binary, argv and cwd, read from its live process before
    anything is stopped (the same read restart.py does)."""
    slot_to_pid = {s: p for p, s in pid_to_slot.items()}
    out: list[_sbs.Slot] = []
    for slot in order:
        bin_name, flags, cwd = _restart._read_proc_state(slot_to_pid[slot])
        out.append(_sbs.Slot(slot=slot, bin_name=bin_name, flags=flags, cwd=cwd))
    return out


def _spawn_stop_build_start() -> int:
    """Start the detached process that runs the sequence; return its pid.

    Detached the way restart.py's child is (setsid / DETACHED_PROCESS +
    new process group), so stopping the caller's runner does not reach it.
    On Windows it also asks to leave the caller's job object: a task run
    through the runner's non-PTY exec sits in a KILL_ON_JOB_CLOSE job, which
    would take this process with it. A job that does not allow breakaway
    refuses the flag, and the spawn is retried without it.
    """
    args = [sys.executable, os.path.abspath(__file__), _DETACHED_FLAG, str(_PLAN)]
    _LOG.parent.mkdir(parents=True, exist_ok=True)
    with open(_LOG, "ab") as log_fh:
        kwargs: dict = dict(
            stdin=subprocess.DEVNULL,
            stdout=log_fh,
            stderr=subprocess.STDOUT,
            close_fds=True,
            cwd=str(_ROOT),
            env=_daemon._clean_child_env(),
        )
        if os.name != "nt":
            kwargs["start_new_session"] = True
            return subprocess.Popen(args, **kwargs).pid
        base = subprocess.DETACHED_PROCESS | subprocess.CREATE_NEW_PROCESS_GROUP
        try:
            return subprocess.Popen(
                args, creationflags=base | subprocess.CREATE_BREAKAWAY_FROM_JOB, **kwargs
            ).pid
        except OSError:
            return subprocess.Popen(args, creationflags=base, **kwargs).pid


def _run_stop_build_start(plan_path: Path) -> int:
    """Detached-process entry: the whole sequence, output to the log."""
    slots = _sbs.read_plan(plan_path)
    with open(_LOG, "a", encoding="utf-8") as fh:

        def emit(msg: str) -> None:
            fh.write(msg + "\n")
            fh.flush()

        def ts() -> str:
            return _dt.datetime.now().astimezone().isoformat(timespec="seconds")

        def down(s: _sbs.Slot) -> None:
            _daemon.daemon_down(s.slot, s.bin_name, timeout=_restart._down_timeout(s.bin_name))

        def build() -> bool:
            # A stopped process can hold its image file a moment longer.
            time.sleep(1.0)
            emit(f"[{ts()}] make -k build")
            fh.flush()
            # -k: one target that still cannot be written (a harness-tui.exe
            # someone has open) must not keep agent-runner from being built.
            rc = subprocess.call(
                ["make", "-k", "build"], cwd=str(_ROOT), stdout=fh, stderr=subprocess.STDOUT
            )
            emit(f"[{ts()}] make build exit={rc}")
            return rc == 0

        def up(s: _sbs.Slot) -> None:
            try:
                os.chdir(s.cwd)
            except OSError as e:
                emit(f"[{s.slot}] chdir({s.cwd}) failed: {e}")
            _daemon.daemon_up(s.slot, s.bin_name, *s.flags)

        emit(f"[{ts()}] stop-first begin: {[s.slot for s in slots]}")
        # daemon_up/down print; keep it in the log.
        old_stdout, old_stderr = sys.stdout, sys.stderr
        sys.stdout = sys.stderr = fh
        try:
            out = _sbs.run(slots, down=down, build=build, up=up, emit=emit)
        finally:
            sys.stdout, sys.stderr = old_stdout, old_stderr
        emit(
            f"[{ts()}] stop-first end: build={'ok' if out.build_ok else 'FAILED'} "
            f"started={out.started} start_failed={out.start_failed} "
            f"stop_failed={out.stop_failed}"
        )
    return 0 if out.build_ok and not out.start_failed else 1


def main(argv: list[str]) -> int:
    if argv and argv[0] == _DETACHED_FLAG:
        if len(argv) != 2:
            return 2
        return _run_stop_build_start(Path(argv[1]))

    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument(
        "--skip-build",
        action="store_true",
        help="skip 'make build' (use the binaries already in bin/)",
    )
    parser.add_argument(
        "--dry-run",
        action="store_true",
        help="print the plan; do not build or restart anything",
    )
    parser.add_argument(
        "--between-seconds",
        type=float,
        default=2.0,
        help="seconds to sleep between non-self restarts (default: 2)",
    )
    parser.add_argument(
        "--force",
        action="store_true",
        help="restart even if one completed within the debounce window",
    )
    parser.add_argument(
        "--debounce-seconds",
        type=float,
        default=_DEFAULT_DEBOUNCE_SECONDS,
        help=(
            "skip if a restart completed this many seconds ago "
            f"(default: {_DEFAULT_DEBOUNCE_SECONDS:.0f}); guards against a "
            "resumed session double-restarting"
        ),
    )
    parser.add_argument(
        "--stop-first",
        action="store_true",
        help=(
            "stop every slot, build, then start them again, in a detached "
            "process that outlives the caller (for Windows, where a running "
            "binary cannot be overwritten); every slot is down during the build"
        ),
    )
    args = parser.parse_args(argv)
    if args.stop_first and args.skip_build:
        parser.error("--stop-first stops the slots so the build can replace them; "
                     "with --skip-build use the ordinary restart")

    pid_to_slot = _alive_runner_slots()
    if not pid_to_slot:
        print("no alive agent-runner slots under bin/.run/")
        return 0

    self_slot = _find_self_slot(pid_to_slot)
    others = sorted(s for s in pid_to_slot.values() if s != self_slot)
    order = others + ([self_slot] if self_slot else [])

    print(f"==> alive slots:    {sorted(pid_to_slot.values())}")
    print(f"==> self slot:      {self_slot or '(none -- running outside a runner)'}")
    print(f"==> restart order:  {order}")
    if args.stop_first:
        print("==> mode:           stop all -> make build -> start all (detached)")

    if args.dry_run:
        print("(dry-run; exiting)")
        return 0

    # Debounce: if a restart completed very recently, this is almost certainly a
    # resumed session re-running after its own self-restart (whose stdout record
    # was lost), not a genuine second request. No-op instead of double-restarting.
    if not args.force:
        ts = _read_stamp()
        if ts is not None:
            age = max(0.0, time.time() - ts)
            if age < args.debounce_seconds:
                print(
                    f"==> a restart completed {age:.0f}s ago "
                    f"(< {args.debounce_seconds:.0f}s debounce); skipping."
                )
                print(
                    "    The fleet is already on freshly-built binaries. This guard "
                    "prevents a\n    double restart when a session resumes right after "
                    "its own self-restart\n    and re-runs. Verify with `harness-cli ls` "
                    "(fresh connection IDs).\n    Pass --force to restart anyway."
                )
                return 0

    if args.stop_first:
        _sbs.write_plan(_PLAN, _capture_slots(pid_to_slot, order))
        _write_stamp()
        pid = _spawn_stop_build_start()
        print(f"==> detached stop-build-start running (pid={pid})")
        print(f"    follow with: tail -f {_LOG}")
        return 0

    if not args.skip_build:
        _run_make_build()

    # Stamp before the restart loop so it survives this process's own self-restart
    # at the tail of the loop. A resumed re-run will see it and debounce (above).
    _write_stamp()

    for i, slot in enumerate(order):
        is_self = slot == self_slot
        print(f"==> restarting {slot}{' (self -- caller will likely die after this)' if is_self else ''}")
        _restart_slot(slot)
        # Brief pause between non-self restarts so the previous one has time
        # to deregister + re-register on the server. Skipped after self
        # because the script will be torn down.
        if i < len(order) - 1:
            time.sleep(args.between_seconds)

    print("==> done")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
