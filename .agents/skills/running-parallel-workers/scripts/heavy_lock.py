"""Run a command under the machine-wide heavy-check lock, one at a time.

  heavy_lock.py LOCKFILE -- COMMAND [ARG...]

Waits for LOCKFILE, runs COMMAND, and exits with its exit code. Every worktree
on the machine must name the same LOCKFILE (an absolute path).

While waiting, the process holds a lock on its own file in LOCKFILE.queue/, so
watch.py can count the waiters on any OS by counting the queue files that are
locked; a file left by a killed waiter is unlocked and is ignored and removed.
"""
import os
import subprocess
import sys
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import parallel_common as pc  # noqa: E402


def main(argv):
    if len(argv) < 3 or argv[1] != "--":
        sys.exit("usage: heavy_lock.py LOCKFILE -- COMMAND [ARG...]")
    lock_path = os.path.abspath(argv[0])
    cmd = argv[2:]
    qdir = lock_path + ".queue"
    os.makedirs(qdir, exist_ok=True)
    me = os.path.join(qdir, "%d-%d" % (os.getpid(), time.time_ns()))
    waiting = pc.FileLock(me)
    waiting.try_acquire()
    try:
        with pc.FileLock(lock_path):
            waiting.release()
            try:
                os.remove(me)
            except OSError:
                pass
            return subprocess.call(cmd)
    finally:
        waiting.release()
        try:
            os.remove(me)
        except OSError:
            pass


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
