"""Watch the parallel work and exit on the first alert, so a parent that runs it
in the background is woken by its exit. Prints the time and the alerts.

Alerts:
  all-idle    every child has been idle for at least --idle-min minutes
  long-idle   one child has been idle for at least --long-min minutes
  lock-queue  more than --queue processes waiting in heavy_lock.py on --lock
  memory      available memory below --mem-mib (skipped where it cannot be read)
  failed      a child's task has the status failed

A failed child keeps raising its alert: take it out of --children before you
start the watcher again.
"""
import argparse
import os
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import parallel_common as pc  # noqa: E402


def queue_waiters(lock_path):
    """Live waiters of heavy_lock.py on lock_path; removes files of dead ones."""
    qdir = os.path.abspath(lock_path) + ".queue"
    try:
        names = os.listdir(qdir)
    except OSError:
        return 0
    live = 0
    for n in names:
        p = os.path.join(qdir, n)
        probe = pc.FileLock(p)
        try:
            got = probe.try_acquire()
        except OSError:
            continue  # removed in between, or held open on Windows: not countable
        if got:
            probe.release()
            try:
                os.remove(p)
            except OSError:
                pass
        else:
            live += 1
    return live


def check(a):
    msgs = []
    rows = pc.tasks()
    detail = []
    all_idle = True
    for c in a.children:
        t = next((r for r in rows if str(r.get("id", "")).startswith(c)), None)
        st = (t or {}).get("status", "unknown")
        idle = -1
        if t and str(t.get("activity") or "").startswith("idle") and (t.get("last_output_at") or 0) > 0:
            idle = int((t.get("output_idle_ms") or 0) // 60000)
        if st == "failed":
            msgs.append("failed: %s" % c)
        detail.append("%s=%dm" % (c, idle))
        if idle < a.idle_min:
            all_idle = False
        if idle >= a.long_min:
            msgs.append("long-idle: %s idle %dm" % (c, idle))
    if all_idle and a.children:
        msgs.append("all-idle: " + " ".join(detail))
    if a.lock:
        n = queue_waiters(a.lock)
        if n > a.queue:
            msgs.append("lock-queue: %d waiting on %s" % (n, a.lock))
    mem = pc.mem_available_mib()
    if mem is not None and mem < a.mem_mib:
        msgs.append("memory: available %dMiB" % mem)
    return msgs


def main():
    ap = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--children", nargs="+", required=True, help="task id prefixes of the children")
    ap.add_argument("--lock", help="the heavy-check lock file children pass to heavy_lock.py")
    ap.add_argument("--idle-min", type=int, default=15)
    ap.add_argument("--long-min", type=int, default=45)
    ap.add_argument("--queue", type=int, default=2)
    ap.add_argument("--mem-mib", type=int, default=1024)
    ap.add_argument("--interval", type=float, default=60, help="seconds between checks")
    ap.add_argument("--max-hours", type=float, default=6)
    a = ap.parse_args()
    if pc.mem_available_mib() is None:
        print("note: available memory cannot be read here; the memory alert is off", file=sys.stderr)
    deadline = time.monotonic() + a.max_hours * 3600
    while time.monotonic() < deadline:
        msgs = check(a)
        if msgs:
            print(time.strftime("%Y-%m-%d %H:%M:%S"))
            print("\n".join(msgs))
            return 0
        time.sleep(a.interval)
    print(time.strftime("%Y-%m-%d %H:%M:%S"))
    print("no alert until the deadline")
    return 0


if __name__ == "__main__":
    pc.utf8_stdio()
    sys.exit(main())
