"""Shared helpers for the running-parallel-workers scripts.

Standard library only, and the same behaviour on Linux, macOS and Windows:
file locking (fcntl or msvcrt), available memory, calling harness-cli, and the
records directory. The scripts import it from their own directory.

Run directly, inside the repository the records are for:
  parallel_common.py suggest      print the recommended records directory
  parallel_common.py init PATH    set up PATH (the place the conventions name) as
                                  this repository's records; refuses a PATH that is
                                  in use for something else
  parallel_common.py records-dir  print the records directory the scripts use
                                  ($PARALLEL_DIR, else the recommended place)
"""
import json
import os
import shutil
import subprocess
import sys
import time

if os.name == "nt":
    import msvcrt

    def _try_lock(fd):
        os.lseek(fd, 0, os.SEEK_SET)
        try:
            msvcrt.locking(fd, msvcrt.LK_NBLCK, 1)
            return True
        except OSError:
            return False

    def _unlock(fd):
        os.lseek(fd, 0, os.SEEK_SET)
        msvcrt.locking(fd, msvcrt.LK_UNLCK, 1)
else:
    import fcntl

    def _try_lock(fd):
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            return True
        except OSError:
            return False

    def _unlock(fd):
        fcntl.flock(fd, fcntl.LOCK_UN)


class FileLock:
    """An exclusive lock on a file, held for the life of the with-block.

    timeout=None waits forever; otherwise TimeoutError after that many seconds.
    The lock belongs to the open file, so it is released when the process dies.
    """

    def __init__(self, path, timeout=None):
        self.path = path
        self.timeout = timeout
        self.fd = None

    def try_acquire(self):
        fd = os.open(self.path, os.O_RDWR | os.O_CREAT, 0o644)
        if _try_lock(fd):
            self.fd = fd
            return True
        os.close(fd)
        return False

    def __enter__(self):
        deadline = None if self.timeout is None else time.monotonic() + self.timeout
        while not self.try_acquire():
            if deadline is not None and time.monotonic() >= deadline:
                raise TimeoutError(self.path)
            time.sleep(0.2)
        return self

    def release(self):
        if self.fd is not None:
            try:
                _unlock(self.fd)
            finally:
                os.close(self.fd)
                self.fd = None

    def __exit__(self, *exc):
        self.release()


def mem_available_mib():
    """Memory available to new processes in MiB, or None when it cannot be read."""
    try:
        if sys.platform.startswith("linux"):
            with open("/proc/meminfo", encoding="ascii") as f:
                for line in f:
                    if line.startswith("MemAvailable:"):
                        return int(line.split()[1]) // 1024
        elif os.name == "nt":
            import ctypes

            class MemoryStatusEx(ctypes.Structure):
                _fields_ = [
                    ("dwLength", ctypes.c_ulong),
                    ("dwMemoryLoad", ctypes.c_ulong),
                    ("ullTotalPhys", ctypes.c_ulonglong),
                    ("ullAvailPhys", ctypes.c_ulonglong),
                    ("ullTotalPageFile", ctypes.c_ulonglong),
                    ("ullAvailPageFile", ctypes.c_ulonglong),
                    ("ullTotalVirtual", ctypes.c_ulonglong),
                    ("ullAvailVirtual", ctypes.c_ulonglong),
                    ("ullAvailExtendedVirtual", ctypes.c_ulonglong),
                ]

            st = MemoryStatusEx()
            st.dwLength = ctypes.sizeof(MemoryStatusEx)
            if ctypes.windll.kernel32.GlobalMemoryStatusEx(ctypes.byref(st)):
                return st.ullAvailPhys // (1024 * 1024)
        elif sys.platform == "darwin":
            out = subprocess.run(["vm_stat"], capture_output=True, text=True).stdout
            page = 4096
            pages = 0
            for line in out.splitlines():
                if "page size of" in line:
                    page = int(line.split("page size of")[1].split()[0])
                elif line.startswith(("Pages free:", "Pages inactive:", "Pages speculative:")):
                    pages += int(line.split(":")[1].strip().rstrip("."))
            return pages * page // (1024 * 1024)
    except (OSError, ValueError, AttributeError):
        pass
    return None


def harness_cli():
    exe = shutil.which("harness-cli")
    if exe is None:
        sys.exit("harness-cli is not on PATH")
    return exe


def harness(*args, check=False):
    """Run harness-cli; returns the CompletedProcess with text output."""
    return subprocess.run(
        [harness_cli(), *args],
        capture_output=True, text=True, encoding="utf-8", errors="replace", check=check,
    )


def json_lines(text):
    """Parse JSON Lines, skipping lines that are not JSON objects."""
    for line in text.splitlines():
        line = line.strip()
        if not line:
            continue
        try:
            obj = json.loads(line)
        except ValueError:
            continue
        if isinstance(obj, dict):
            yield obj


def tasks():
    """The task rows of `harness-cli ls --json`, or [] when it fails."""
    r = harness("ls", "--json")
    if r.returncode != 0:
        return []
    try:
        return json.loads(r.stdout).get("tasks", [])
    except ValueError:
        return []


# --- The records directory ---------------------------------------------------
#
# Where the records go is a repository convention: the owner picks it while the
# conventions document is written, and the document names it (relative to ~).
# The recommended place is <state root>/running-parallel-workers/<repo>, the state
# root being $XDG_STATE_HOME (else ~/.local/state) on Linux and macOS and
# %LOCALAPPDATA% on Windows. <records>/.repo names the main checkout the records
# belong to, so records of another repository are refused rather than mixed in.

APP = "running-parallel-workers"
REPO_MARK = ".repo"
SUBDIRS = ("instructions", "log", "questions", "readback")


class RecordsError(Exception):
    pass


def _same_path(a, b):
    return os.path.normcase(os.path.realpath(a)) == os.path.normcase(os.path.realpath(b))


def main_checkout(cwd=None):
    """Path of the repository's main checkout, from any of its worktrees; None outside one."""
    try:
        r = subprocess.run(["git", "rev-parse", "--git-common-dir"], cwd=cwd,
                           capture_output=True, text=True)
    except OSError:
        return None
    if r.returncode != 0 or not r.stdout.strip():
        return None
    common = os.path.abspath(os.path.join(cwd or os.getcwd(), r.stdout.strip()))
    if os.path.basename(common) == ".git":
        common = os.path.dirname(common)
    return os.path.realpath(common)


def suggested_records_dir(cwd=None):
    """The recommended place for this repository's records; None outside a repository."""
    checkout = main_checkout(cwd)
    if checkout is None:
        return None
    if os.name == "nt":
        base = os.environ.get("LOCALAPPDATA") or os.path.join(os.path.expanduser("~"), "AppData", "Local")
    else:
        base = os.environ.get("XDG_STATE_HOME") or os.path.join(os.path.expanduser("~"), ".local", "state")
    return os.path.join(base, APP, os.path.basename(checkout))


def _check(path, checkout):
    """None if PATH is this repository's records; else why not."""
    mark = os.path.join(path, REPO_MARK)
    if os.path.isfile(mark):
        with open(mark, encoding="utf-8") as f:
            theirs = f.readline().strip()
        if checkout is None or _same_path(theirs, checkout):
            return None
        return "%s holds the records of another repository (%s)" % (path, theirs)
    if os.path.isdir(path) and os.listdir(path):
        return "%s exists and is not a records directory (no %s): it is in use for something else" % (path, REPO_MARK)
    return "%s is not set up: run `parallel_common.py init %s`" % (path, path)


def records_dir(explicit=None, cwd=None):
    """The records directory: EXPLICIT (a --dir), else $PARALLEL_DIR, else the
    recommended place. Raises RecordsError unless it is this repository's records
    (outside a repository, an explicit path only needs to be a records directory)."""
    path = explicit or os.environ.get("PARALLEL_DIR")
    checkout = main_checkout(cwd)
    if not path:
        path = suggested_records_dir(cwd)
        if path is None:
            raise RecordsError("not inside a git repository: give --dir (the records "
                               "directory the conventions document names)")
    path = os.path.abspath(os.path.expanduser(path))
    why = _check(path, checkout)
    if why:
        if not (explicit or os.environ.get("PARALLEL_DIR")):
            why += ". If the conventions name another place, give it with --dir or PARALLEL_DIR"
        raise RecordsError(why)
    return path


def init_records(path, cwd=None):
    """Set up PATH as this repository's records. Refuses a PATH in use for something else."""
    checkout = main_checkout(cwd)
    if checkout is None:
        raise RecordsError("run init from inside the repository the records are for")
    path = os.path.abspath(os.path.expanduser(path))
    mark = os.path.join(path, REPO_MARK)
    if os.path.isfile(mark) or (os.path.isdir(path) and os.listdir(path)):
        why = _check(path, checkout)
        if why:
            raise RecordsError(why + ". Ask the owner for another place.")
    for d in SUBDIRS:
        os.makedirs(os.path.join(path, d), exist_ok=True)
    if not os.path.isfile(mark):
        with open(mark, "w", encoding="utf-8") as f:
            f.write(checkout + "\n")
    if not os.path.isdir(os.path.join(path, ".git")):
        subprocess.run(["git", "init", "-q", path], check=True)
    return path


def resolve_records_dir(explicit=None):
    """records_dir(), or exit with its reason: for the scripts' --dir."""
    try:
        return records_dir(explicit)
    except RecordsError as e:
        sys.exit(str(e))


def utf8_stdio():
    """Never die on a console that cannot encode a character."""
    for s in (sys.stdout, sys.stderr):
        try:
            s.reconfigure(errors="replace")
        except AttributeError:
            pass


if __name__ == "__main__":
    args = sys.argv[1:]
    try:
        if args == ["suggest"]:
            s = suggested_records_dir()
            if s is None:
                sys.exit("not inside a git repository")
            print(s)
        elif len(args) == 2 and args[0] == "init":
            print(init_records(args[1]))
        elif args == ["records-dir"]:
            print(records_dir())
        else:
            sys.exit("usage: parallel_common.py suggest | init PATH | records-dir")
    except RecordsError as e:
        sys.exit(str(e))
