#!/usr/bin/env python3
"""Stand up a throwaway server + runner from this checkout's bin/, for manual
verification and debugging.

Why this exists: several checks can only be done against a live harness —
interactive wake, TUI reconnect, "does the operator actually see this" — and
standing one up by hand has two traps that cost real time each rediscovery:

  1. A task shell inherits HARNESS_* from the harness that spawned it.
     HARNESS_AUTH_TICKET is a ticket for the LIVE server; against a dummy one
     it fails as `psk auth: psk: server rejected: BadTicket`, which reads like
     a PSK mistake rather than a stale-ticket one.
  2. It also inherits CLAUDE_CODE_* / CLAUDECODE / AI_AGENT. A claude spawned
     with those set decides it is a child session and silently disables
     transcript saving, so `--continue` / `--resume` find nothing.

Both are scrubbed here. Do not "simplify" that away.

The live fleet is never touched: loopback only, an ephemeral port, a fresh
PSK, and a temp data dir that goes away on teardown.

Usage:
  scripts/dummy-harness.py up [--agent claude|fake] [--model NAME] [--detach] [--name N]
                              [-- <extra agent-runner flags>]
  scripts/dummy-harness.py env  [--name N]   # print `export` lines for an instance
  scripts/dummy-harness.py down [--name N]

Flags after `--` go to agent-runner verbatim, appended last. The runner here
defaults to --no-worktree, which switches skill/settings injection OFF; add
`-- --force-inject-harness-settings` when the check needs them injected.

--name lets independent instances coexist. That is not a nicety: checking
what a client does when its server restarts needs the window you are
watching through to live on a DIFFERENT server than the one you restart.
Run a `host` instance and a `target` instance, drive the client from a
session on `host`, and bounce `target`.

  # foreground (Ctrl-C to stop), real claude on the cheapest model:
  scripts/dummy-harness.py up

  # detached, then drive it:
  scripts/dummy-harness.py up --detach
  eval "$(scripts/dummy-harness.py env)"
  harness-cli --server-cid "$CID" ls
  scripts/dummy-harness.py down

Every instance gets a `bash` profile alongside the agent one. A oneshot task
submitted to it runs its prompt as a shell command with that task's
HARNESS_* in the environment — which is how you exercise agent-side commands
(`harness-cli agent send`, inbox, …) without hand-minting a ticket. On Windows
that bash is Git for Windows' own, resolved to an absolute path: bare `bash`
there is the WSL launcher, which every such task dies inside. If none is found
`up` says so and ships no bash profile.

This is the canonical implementation; dummy-harness.sh is a thin wrapper, the
same shape runner.sh and restart.sh already have. It is Python because the
shell version could not run on Windows at all — MSYS make dropped the Go
environment so `make build` failed, Git Bash's `-x` would not resolve `.exe`
so the binary check failed, and the rest (mktemp, /dev/urandom, kill -0, seq,
ss) was POSIX-shaped. Which mattered: the one check that has to happen on
Windows is the one this script exists to make repeatable.
"""

from __future__ import annotations

import argparse
import getpass
import json
import os
import secrets
import shutil
import socket
import subprocess
import sys
import tempfile
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
# Both are stdlib-only by design, so they are safe to import above the venv
# bootstrap. bash_bin comes from the preset table on purpose: a dummy instance
# that resolved its shell differently from a real runner would pass checks the
# thing it stands in for fails.
from agent_presets import bash_bin  # noqa: E402  (path set above)
from bootstrap import ensure_venv  # noqa: E402  (path set above)

ensure_venv()

# Only below here are we inside scripts/.venv. `daemon` imports psutil at MODULE
# level, so importing it above the bootstrap made this script fail to start at
# all — the deps live in the venv precisely so the system interpreter does not
# need them, and the only reason it ever ran was a machine that happened to have
# psutil installed system-wide. Same order as runner.py / server.py / restart.py.
import daemon  # noqa: E402  (venv active, so psutil resolves)

REPO_ROOT = Path(__file__).resolve().parent.parent
BIN = REPO_ROOT / "bin"

# Every text read/write below names its encoding, and that is load-bearing
# rather than defensive style. Python's default for read_text/write_text and for
# subprocess text mode is the LOCALE codec, which on a Japanese Windows install
# is cp932 — and this file's own text contains U+2014, so writing the fake agent
# died outright there. The same default silently covers the state file `down`
# reads its pids back out of, i.e. the one file whose loss leaves a server and a
# runner alive with nothing recorded to kill them by.

# Session markers and live-harness credentials this script must not carry into
# anything it starts. daemon._clean_child_env already drops the claude ones for
# its own spawns; HARNESS_* is this script's addition, and the reason is in the
# module docstring — a ticket for the LIVE server fails against a dummy one with
# an error that reads like a PSK mistake.
_SCRUB_PREFIXES = ("HARNESS_", "CLAUDE_CODE")
_SCRUB_EXACT = frozenset({"CLAUDECODE", "AI_AGENT", "CLAUDE_EFFORT"})

# The names cmd_env unsets in the CONSUMING shell. Kept explicit rather than
# derived from what happens to be set here: whoever evals this is a different
# process, and the point is to clear what a harness task carries regardless of
# what this one does.
_UNSET_IN_CONSUMER = (
    "HARNESS_AUTH_TICKET HARNESS_TASK_ID HARNESS_RUNNER_ID HARNESS_SERVER_CID "
    "HARNESS_WS_PATH HARNESS_REPO_PATH HARNESS_HOSTNAME"
)


def scrub_own_env() -> None:
    """Drop the markers from THIS process, so everything it starts inherits a
    clean environment without each spawn having to remember to filter."""
    for key in list(os.environ):
        if key in _SCRUB_EXACT or key.startswith(_SCRUB_PREFIXES):
            del os.environ[key]


def survive_undisplayable_output() -> None:
    """Same locale-codec default as the file I/O above, reaching the streams.

    `print(__doc__)` is the no-subcommand usage path and the docstring holds
    U+2014, so on a cp932 console a bare invocation answered with a traceback
    instead of the usage. Encoding is left alone — whoever reads these streams
    decodes with their own codec, and forcing UTF-8 would mangle a non-ASCII
    temp path in the `export` lines. Only the error policy changes, so an
    undisplayable character costs one glyph rather than the whole message.
    """
    for stream in (sys.stdout, sys.stderr):
        try:
            stream.reconfigure(errors="replace")  # type: ignore[union-attr]
        except (AttributeError, OSError, ValueError):
            pass


def die(msg: str) -> "NoReturn":  # type: ignore[valid-type]
    print(f"dummy-harness: {msg}", file=sys.stderr)
    raise SystemExit(1)


def setup_err(msg: str) -> "NoReturn":  # type: ignore[valid-type]
    print(f"dummy-harness: SETUP: {msg}", file=sys.stderr)
    raise SystemExit(2)


def tmp_root() -> Path:
    """The directory instances and state files live under.

    NOT tempfile.gettempdir(), and the reason is a bug this had: gettempdir()
    consults TMPDIR, then TEMP, then TMP — and `env` exports TMP as the
    INSTANCE's directory. So one `eval "$(dummy-harness.py env)"` in a shell
    made every later call in that shell resolve its state directory inside the
    instance, `down` reported "nothing to stop", and a server and runner were
    left running with their state file orphaned. The shell version was immune
    only because it read ${TMPDIR:-/tmp} and never TMP.

    Reading a variable this script also EXPORTS is the whole trap, so the root
    is resolved from one it does not.
    """
    if os.name == "nt":
        return Path(os.environ.get("TEMP") or tempfile.gettempdir())
    return Path(os.environ.get("TMPDIR") or "/tmp")


def state_dir() -> Path:
    # getpass.getuser() rather than the shell's `id -u`, which has no Windows
    # equivalent. The name only has to separate users sharing a temp dir.
    return tmp_root() / f"harness-dummy-{getpass.getuser()}"


def state_path(name: str) -> Path:
    return state_dir() / f"{name}.json"


def pick_port() -> int:
    s = socket.socket()
    try:
        s.bind(("127.0.0.1", 0))
        return int(s.getsockname()[1])
    finally:
        s.close()


def listening(port: int) -> bool:
    """Connect rather than parse `ss -ltn`, which does not exist on Windows and
    tells us less: a successful connect is the property actually wanted."""
    try:
        with socket.create_connection(("127.0.0.1", port), timeout=0.25):
            return True
    except OSError:
        return False


def go_env_for_make() -> dict[str, str]:
    """The environment to run `make build` under.

    On Windows the MSYS make that ships with Git for Windows does not pass the
    Go environment through to its recipes, so `make build` dies with "module
    cache not found: neither GOMODCACHE nor GOPATH is set" even when the parent
    shell has them. Resolving them from `go env` and setting them explicitly in
    the child's environment fixes it at the point where this script actually has
    control, which a wrapper shell does not.
    """
    env = daemon._clean_child_env()
    for key in ("GOPATH", "GOMODCACHE", "GOCACHE"):
        if env.get(key):
            continue
        try:
            out = subprocess.run(
                ["go", "env", key], capture_output=True, text=True,
                encoding="utf-8", errors="replace", timeout=30,
            )
        except (OSError, subprocess.SubprocessError):
            continue
        val = out.stdout.strip()
        if out.returncode == 0 and val:
            env[key] = val
    return env


def build() -> None:
    # make build, not go build: harness-server embeds webui/static/main.wasm and
    # refuses to start without it. A server that never starts makes every
    # subsequent check trivially "pass", which is worse than a loud failure.
    env = go_env_for_make()
    # The same values also go on the command line: make exports command-line
    # variables to its recipes, which is a second route to the recipe on a make
    # that ignores the inherited environment.
    overrides = [f"{k}={env[k]}" for k in ("GOPATH", "GOMODCACHE") if env.get(k)]
    proc = subprocess.run(
        ["make", "build", *overrides],
        cwd=str(REPO_ROOT),
        env=env,
        capture_output=True,
        text=True,
        encoding="utf-8",
        errors="replace",
    )
    if proc.returncode != 0:
        sys.stderr.write(proc.stdout[-2000:])
        sys.stderr.write(proc.stderr[-2000:])
        setup_err("make build failed")
    for name in ("harness-server", "agent-runner", "harness-cli"):
        path = daemon.bin_path(name)
        if not daemon._binary_is_executable(path):
            setup_err(f"{path} missing after make build")


FAKE_AGENT = '''\
"""Deterministic stand-in for claude: emits claude's stream-json shapes with
pauses, for checks that must not depend on a model or a network.

Python rather than the /bin/sh it used to be, so the `fake` agent works on every
platform this script does. It ignores its arguments on purpose — the point is a
fixed, inspectable transcript, not an agent."""
import sys, time

def emit(s):
    sys.stdout.write(s + "\\n")
    sys.stdout.flush()

emit('{"type":"system","subtype":"init","session_id":"dummy-session"}')
time.sleep(1)
emit('{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"echo hi"}}]}}')
time.sleep(1)
emit('{"type":"user","message":{"content":[{"type":"tool_result","content":"hi"}]}}')
emit('{"type":"assistant","message":{"content":[{"type":"text","text":"done"}]}}')
emit('{"type":"result","subtype":"success","duration_ms":2000,"total_cost_usd":0}')
'''


def bash_profile(bin_: str) -> str:
    return json.dumps(
        [
            {
                "name": "bash",
                "bin": bin_,
                "oneshotArgv": ["{args}", "-c", "{prompt}"],
                "resumeOneshotArgv": ["{args}", "-c", "{prompt}"],
                "resumeInteractiveArgv": ["{args}"],
                "logFormat": "",
            }
        ],
        separators=(",", ":"),
    )


def spawn(args: list[str], log: Path) -> subprocess.Popen:
    """Start a child with its output to log, in its own process group so a
    Ctrl-C in this terminal does not reach it before `down` can tear it down in
    the right order."""
    fh = log.open("wb")
    kwargs: dict = {"stdout": fh, "stderr": subprocess.STDOUT, "env": os.environ.copy()}
    if os.name == "nt":
        kwargs["creationflags"] = subprocess.CREATE_NEW_PROCESS_GROUP
    else:
        kwargs["start_new_session"] = True
    return subprocess.Popen([str(a) for a in args], **kwargs)


def kill_pid(pid: int) -> None:
    """Terminate one process by PID, then kill it.

    By PID and never by name. The dummy runs the SAME binary names as the real
    fleet, so a name-matched kill on a developer machine takes the production
    runners with it — which has happened, and cost an afternoon. Recording the
    pids and killing only those makes that mistake unavailable rather than
    merely discouraged.
    """
    import psutil

    try:
        proc = psutil.Process(pid)
    except psutil.NoSuchProcess:
        return
    try:
        proc.terminate()
        proc.wait(timeout=5)
    except psutil.NoSuchProcess:
        return
    except (psutil.TimeoutExpired, psutil.Error):
        try:
            proc.kill()
        except psutil.Error:
            pass


def cmd_env(name: str) -> int:
    path = state_path(name)
    if not path.is_file():
        die(f"no instance (state file {path} missing); run 'up --detach' first")
    st = json.loads(path.read_text(encoding="utf-8"))
    # The scrub at the top of this script only cleans the script's own
    # environment. Whoever evals this is a different shell — inside a harness
    # task it still carries HARNESS_AUTH_TICKET for the LIVE server, and
    # harness-cli prefers a ticket over the PSK, so every call would fail
    # BadTicket while looking like a PSK problem. Emit the unsets too.
    print(f"unset {_UNSET_IN_CONSUMER}")
    for key in ("HARNESS_PSK", "CID", "TMP", "BIN", "REPO", "SERVER_PID", "RUNNER_PID", "SERVER_PORT"):
        print(f"export {key}='{st[key]}'")
    return 0


def cmd_down(name: str) -> int:
    path = state_path(name)
    if not path.is_file():
        print("dummy-harness: nothing to stop")
        return 0
    st = json.loads(path.read_text(encoding="utf-8"))
    for key in ("RUNNER_PID", "SERVER_PID"):
        pid = st.get(key)
        if pid:
            kill_pid(int(pid))
    tmp = st.get("TMP")
    if tmp and Path(tmp).is_dir():
        shutil.rmtree(tmp, ignore_errors=True)
    path.unlink(missing_ok=True)
    print("dummy-harness: stopped")
    return 0


def cmd_up(name: str, agent: str, model: str, detach: bool, extra: list[str]) -> int:
    if agent not in ("claude", "fake"):
        die(f"unknown --agent: {agent} (want claude or fake)")

    build()

    path = state_path(name)
    if path.is_file():
        die(f"an instance is already recorded at {path}; run 'down' first")
    state_dir().mkdir(parents=True, exist_ok=True)

    tmp = Path(tempfile.mkdtemp(prefix="harness-dummy.", dir=str(tmp_root())))
    port = pick_port()
    psk = "dummy-" + secrets.token_urlsafe(12).replace("-", "").replace("_", "")
    cid = f"ws:127.0.0.1:{port}-*"

    repo = tmp / "repo"
    data = tmp / "data"
    repo.mkdir(parents=True)
    data.mkdir(parents=True)
    subprocess.run(["git", "-C", str(repo), "init", "-q"], check=True)
    subprocess.run(
        ["git", "-C", str(repo), "-c", "user.email=dummy@example.invalid",
         "-c", "user.name=dummy", "commit", "-q", "--allow-empty", "-m", "dummy repo"],
        check=True,
    )

    server = spawn(
        [daemon.bin_path("harness-server"), "--listen", f"127.0.0.1:{port}",
         "--psk", psk, "--operator-psk", psk, "--data-dir", str(data)],
        tmp / "server.log",
    )
    for _ in range(40):
        if listening(port):
            break
        time.sleep(0.25)
    if not listening(port):
        sys.stderr.write(
            (tmp / "server.log").read_text(encoding="utf-8", errors="replace")[:2000]
        )
        kill_pid(server.pid)
        setup_err(f"server never listened on {port}")

    # --max-tasks 4, not the default 1: an interactive session holds a slot for
    # its whole life, so with one slot every follow-up task sits Queued forever
    # and the symptom is silence, not an error.
    runner_args: list[str] = [
        str(daemon.bin_path("agent-runner")),
        "--server-cid", cid, "--psk", psk, "--roots", str(repo), "--no-worktree",
        "--max-tasks", "4",
    ]
    # No profile at all beats one that registers and then fails per task: an
    # absent `bash` agent is rejected at submit with a name you can act on,
    # where a WSL launcher answers with a namespace error about a path nobody
    # asked for. Said at `up` time because that is when it can still be fixed.
    bash = bash_bin()
    if bash:
        runner_args += ["--agent-profiles", bash_profile(bash)]
    else:
        sys.stderr.write(
            "dummy-harness: no POSIX bash found (Git for Windows not installed?). "
            "This instance has no 'bash' profile, so the agent-side hands "
            "documented in the dummy-harness skill are unavailable on it.\n"
        )
    if agent == "claude":
        runner_args += [
            "--agent-bin", "claude",
            "--claude-args", f"--model {model}",
            "--agent-oneshot-argv", "--output-format stream-json --verbose {args} -p {prompt}",
            "--agent-resume-oneshot-argv", "--output-format stream-json --verbose {args} --continue -p {prompt}",
            "--agent-resume-interactive-argv", "{args} --continue",
            "--agent-log-format", "claude-stream-json",
        ]
    else:
        fake = tmp / "fake-claude.py"
        fake.write_text(FAKE_AGENT, encoding="utf-8")
        # agent-bin is the interpreter and the script leads every argv template,
        # so the fake needs no shebang and no exec bit — which is what makes it
        # work identically on Windows.
        runner_args += [
            "--agent-bin", sys.executable,
            "--agent-oneshot-argv", f"{fake} --output-format stream-json --verbose {{args}} -p {{prompt}}",
            "--agent-resume-oneshot-argv", f"{fake} --output-format stream-json --verbose {{args}} --continue -p {{prompt}}",
            "--agent-resume-interactive-argv", f"{fake} {{args}} --continue",
            "--agent-log-format", "claude-stream-json",
        ]
    runner_args += extra

    runner = spawn(runner_args, tmp / "runner.log")

    os.environ["HARNESS_PSK"] = psk  # harness-cli's operator binder falls back to this
    registered = False
    for _ in range(40):
        if runner.poll() is not None:
            break
        out = subprocess.run(
            [str(daemon.bin_path("harness-cli")), "--server-cid", cid, "ls"],
            capture_output=True, text=True, encoding="utf-8", errors="replace",
        )
        if "agent=" in out.stdout:
            registered = True
            break
        time.sleep(0.25)
    if not registered:
        sys.stderr.write(
            (tmp / "runner.log").read_text(encoding="utf-8", errors="replace")[-2000:]
        )
        kill_pid(runner.pid)
        kill_pid(server.pid)
        setup_err("runner never registered")

    # JSON rather than shell `export` lines, because `down` reads it too and a
    # state file that only a POSIX shell can parse is how this script came to be
    # unusable on the platform it was most needed on. cmd_env renders the export
    # lines from it, so consumers are unchanged.
    path.write_text(json.dumps({
        "HARNESS_PSK": psk,
        "CID": cid,
        "TMP": str(tmp),
        "BIN": str(BIN),
        "REPO": str(repo),
        "SERVER_PID": server.pid,
        "RUNNER_PID": runner.pid,
        "SERVER_PORT": port,
    }, indent=2), encoding="utf-8")

    me = Path(__file__).name
    print(f"dummy-harness: up  name={name}  agent={agent}  port={port}  repo={repo}")
    print(f"dummy-harness: eval \"$(scripts/{me} env)\" to drive it; 'scripts/{me} down' to stop")

    if detach:
        return 0
    print("dummy-harness: foreground; Ctrl-C to stop")
    try:
        while server.poll() is None:
            time.sleep(2)
    except KeyboardInterrupt:
        pass
    return cmd_down(name)


def main(argv: list[str]) -> int:
    survive_undisplayable_output()
    scrub_own_env()

    p = argparse.ArgumentParser(add_help=False)
    p.add_argument("sub", nargs="?", default="")
    p.add_argument("--name", default="default")
    p.add_argument("--agent", default="claude")
    p.add_argument("--model", default="claude-haiku-4-5-20251001")
    p.add_argument("--detach", "-d", action="store_true")
    # Everything after a literal `--` goes to agent-runner verbatim, appended
    # last so it overrides the defaults built above. Needed for runner flags this
    # script has no opinion about — e.g. --agentskills-dir, or
    # --force-inject-harness-settings to re-enable skill injection, which the
    # --no-worktree default otherwise switches off.
    if "--" in argv:
        cut = argv.index("--")
        argv, extra = argv[:cut], argv[cut + 1:]
    else:
        extra = []
    args, unknown = p.parse_known_args(argv)
    if unknown:
        die(f"unknown flag: {unknown[0]}")

    if args.sub == "up":
        return cmd_up(args.name, args.agent, args.model, args.detach, extra)
    if args.sub == "env":
        return cmd_env(args.name)
    if args.sub == "down":
        return cmd_down(args.name)
    print(__doc__)
    return 1


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
