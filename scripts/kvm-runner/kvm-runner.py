#!/usr/bin/env python3
"""An agent-runner slot inside a KVM guest, so a task can break the kernel.

The guest is an ordinary runner host — the same thing the Windows slot is,
only reached over a local port forward instead of the LAN. It joins the
server by dialling out, so no bridge, no inbound rule and no root: this
host's /dev/kvm is world-rw and libvirt runs in session mode
(``qemu:///session``), with passt providing user-mode networking.

Why a VM and not the podman sandbox (scripts/sandbox/): loading a BPF
program needs CAP_BPF against a *shared* kernel, so a container can only
either refuse the capability or hand over the host's kernel. Neither is a
place to develop eBPF. Here the kernel the agent can wedge is the guest's.

What this does NOT give you, and cannot:

- **A fresh kernel per task.** The guest kernel outlives every task on the
  slot. Pinned programs in bpffs, attached tc/XDP/kprobe hooks and modified
  sysctls carry into the next one. ``snapshot`` / ``revert`` is the cleanup.
- **A blast radius of one task.** When the guest panics, the runner's
  connection drops and the server marks EVERY task active on that runner
  Failed (server.failAndRevokeTasksOf), which a human then resumes. That is
  why --max-tasks defaults to 1 here and not to the 4-8 a host slot runs.
- **Isolation inside the guest.** BPF needs root there, so the agent gets
  passwordless sudo. Put nothing in the guest you cannot re-create: the
  agent can destroy its checkout and its credentials.

Landing work out of the guest is a push to the remote and a fast-forward on
the trunk-authoritative checkout, exactly as it is from the Windows slot.

Usage:
  kvm-runner.py [--name N] up          [--memory MB] [--vcpus N] [--disk GB]
  kvm-runner.py [--name N] provision   [--agent-bin PATH] [--token-file PATH]
  kvm-runner.py [--name N] runner up   [--server-cid CID] [--max-tasks N]
                                       [--slot S] [--no-worktree]
  kvm-runner.py [--name N] runner down [--slot S]
  kvm-runner.py [--name N] status
  kvm-runner.py [--name N] ssh         [-- <cmd...>]
  kvm-runner.py [--name N] snapshot    [--tag TAG]
  kvm-runner.py [--name N] revert      [--tag TAG]
  kvm-runner.py [--name N] down | destroy

First bring-up is three commands:

  scripts/kvm-runner/kvm-runner.py up
  scripts/kvm-runner/kvm-runner.py provision
  scripts/kvm-runner/kvm-runner.py runner up

then `harness-cli ls --json` lists the slot under "runners", and a task
reaches it with `submit --host <name> --repo <guest lab path>`.

A second slot in the same guest — a bash one that runs commands in the lab
directory itself rather than in a per-task worktree — is:

  scripts/kvm-runner/kvm-runner.py --agent bash runner up --no-worktree

Both slots report the SAME hostname, and that is fine: they are told apart
by profile, not by host. A runner's default profile is named after its
--agent-bin basename (cmd/agent-runner/main.go), so these advertise
"claude" and "bash", and `submit --agent bash` narrows the candidates to one
(server/task_handler.go filterByProfile; a name no candidate has answers
ProfileUnavailable, not ambiguous). Interactive opens list one row per
(runner, profile) combo, so the picker separates them too. Only a bare
`submit` naming neither profile nor a distinct repo can still come back
ambiguous.
"""

from __future__ import annotations

import argparse
import json
import os
import shlex
import shutil
import subprocess
import sys
import time
from pathlib import Path

_HERE = Path(__file__).resolve().parent
_SCRIPTS = _HERE.parent
_ROOT = _SCRIPTS.parent
sys.path.insert(0, str(_SCRIPTS))

import agent_presets  # noqa: E402  (path set above; stdlib-only, no venv needed)

# Where the pristine download and the per-guest disk live. Outside the repo:
# these are multi-hundred-MB artifacts, not source.
VM_DIR = Path.home() / "vm"

# Default image, overridable with --image-url. Note what `latest` means for the
# cache: the file it downloads to is named from the URL, which carries no
# version, so once ~/vm/<basename> exists nothing ever re-fetches it. The guest
# is then pinned to whenever `up` first ran, silently, while the URL keeps
# claiming to be current. `up` prints the cached file's date for that reason,
# and --refresh-image re-fetches.
DEFAULT_IMAGE_URL = "https://geo.mirror.pkgbuild.com/images/latest/Arch-Linux-x86_64-cloudimg.qcow2"
DEFAULT_OSINFO = "archlinux"

# Guest-side layout. GUEST_USER matches the host user so that a shared
# filesystem, if one is ever added, needs no uid mapping.
GUEST_USER = os.environ.get("USER") or "agent"
GUEST_HOME = f"/home/{GUEST_USER}"
GUEST_BIN = f"{GUEST_HOME}/.local/bin"
GUEST_RUN = f"{GUEST_HOME}/.run"
GUEST_TOKEN = f"{GUEST_HOME}/.config/harness/agent-token"

# Packages cloud-init installs. The eBPF set is the point of the guest; the
# rest is what an agent needs to be useful in a checkout. These are PACMAN
# names, so they travel with DEFAULT_IMAGE_URL — a non-Arch --image-url needs
# --packages and a matching --osinfo, or cloud-init installs nothing and the
# guest comes up without a toolchain.
GUEST_PACKAGES = [
    "git", "base-devel", "python", "ripgrep",
    "clang", "llvm", "linux-headers",
    "bpf", "libbpf", "bpftrace",
    "strace", "tcpdump", "iproute2",
]

# Agents whose binary is bridged in from the host, because the guest image has
# no install of them. `bash` is deliberately absent: the image's own
# /usr/bin/bash IS the agent, and rewriting --agent-bin for it would point the
# slot at a path that does not exist in the guest.
BRIDGED_AGENTS = {"claude", "codex", "agy", "opencode"}

# Only claude has a revocable-token mode (scripts/sandbox/README.md). A slot
# whose agent has no entry here never sees the token in its environment — a
# bash slot does not need the credential and should not carry it.
#
# It is NOT the default here, and the podman kit's reasoning does not carry
# over. That kit trades resume away for token auth because the container's HOME
# is ephemeral; a guest's home is a real disk, so `~/.claude` keeps both the
# credentials and the session store across tasks and resume works either way.
# And measured 2026-09-08 in this guest: after one run under
# CLAUDE_CODE_OAUTH_TOKEN, claude had written a full `claudeAiOauth` blob
# *including a refreshToken* plus a trustedDeviceToken into
# ~/.claude/.credentials.json — so token auth does not keep a refresh token out
# of the guest either. Logging in inside the guest is the honest default.
TOKEN_ENV_BY_AGENT = {"claude": "CLAUDE_CODE_OAUTH_TOKEN"}

CONNECT = "qemu:///session"


def die(msg: str) -> "NoReturn":  # type: ignore[valid-type]
    print(f"kvm-runner: {msg}", file=sys.stderr)
    raise SystemExit(1)


def run(argv: list[str], *, check: bool = True, capture: bool = False,
        stdin: str | None = None, env: dict[str, str] | None = None) -> subprocess.CompletedProcess:
    return subprocess.run(
        argv, check=check, text=True, input=stdin, env=env,
        stdout=subprocess.PIPE if capture else None,
        stderr=subprocess.STDOUT if capture else None,
    )


def virsh(*args: str, capture: bool = False, check: bool = True) -> subprocess.CompletedProcess:
    # LC_ALL=C because domstate is translated: on a Japanese host it answers
    # 実行中, and every `== "running"` test in here would read it as off.
    return run(["virsh", "-c", CONNECT, *args], capture=capture, check=check,
               env={**os.environ, "LC_ALL": "C"})


def domain_state(name: str) -> str:
    """"running" / "shut off" / "" when the domain is not defined."""
    p = virsh("domstate", name, capture=True, check=False)
    return p.stdout.strip() if p.returncode == 0 else ""


# ---------------------------------------------------------------- ssh plumbing

def ssh_key_path(args) -> Path:
    return Path(args.ssh_key).expanduser()


def ensure_ssh_key(args) -> Path:
    """The guest is reached by key only; make one on first use.

    Kept out of the repo tree deliberately — a key committed next to the
    script that provisions it into every guest is a key with no revocation.
    """
    key = ssh_key_path(args)
    if not key.exists():
        key.parent.mkdir(parents=True, exist_ok=True)
        run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C",
             f"kvm-runner:{args.name}", "-f", str(key)])
        print(f"generated {key}")
    return key


def ssh_argv(args, *, tty: bool = False) -> list[str]:
    key = ssh_key_path(args)
    argv = ["ssh", "-i", str(key),
            "-o", "StrictHostKeyChecking=no",
            "-o", f"UserKnownHostsFile={VM_DIR / args.name / 'known_hosts'}",
            "-o", "ConnectTimeout=5",
            "-p", str(args.port)]
    if tty:
        argv.append("-t")
    argv.append(f"{GUEST_USER}@127.0.0.1")
    return argv


def guest_sh(args, script: str, *, capture: bool = False,
             check: bool = True) -> subprocess.CompletedProcess:
    """Run a shell script in the guest, passed on stdin rather than as argv.

    stdin keeps the caller free of the double quoting that an inline
    `ssh host "..."` demands, which is where these scripts go wrong.
    """
    # The guest's output goes straight to our inherited stdout, so anything we
    # printed and have not flushed would appear AFTER it.
    sys.stdout.flush()
    return run([*ssh_argv(args), "bash", "-s"], stdin=script,
               capture=capture, check=check)


def scp_to(args, sources: list[Path], dst: str, *, preserve: bool = False) -> None:
    key = ssh_key_path(args)
    argv = ["scp", "-q", "-i", str(key),
            "-o", "StrictHostKeyChecking=no",
            "-o", f"UserKnownHostsFile={VM_DIR / args.name / 'known_hosts'}",
            "-P", str(args.port)]
    if preserve:
        argv.append("-p")
    argv += [str(s) for s in sources]
    argv.append(f"{GUEST_USER}@127.0.0.1:{dst}")
    run(argv)


def wait_for_ssh(args, timeout: int = 600) -> None:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        p = run([*ssh_argv(args), "true"], check=False, capture=True)
        if p.returncode == 0:
            return
        time.sleep(5)
    die(f"guest did not accept ssh on 127.0.0.1:{args.port} within {timeout}s "
        f"(try: virsh -c {CONNECT} console {args.name})")


# ------------------------------------------------------------------------- up

def cloud_init_user_data(args, pubkey: str) -> str:
    names = args.packages.split(",") if args.packages else GUEST_PACKAGES
    packages = "\n".join(f"  - {p.strip()}" for p in names if p.strip())
    return f"""#cloud-config
hostname: {args.name}
fqdn: {args.name}
users:
  - name: {GUEST_USER}
    uid: 1000
    groups: [wheel]
    sudo: ["ALL=(ALL) NOPASSWD:ALL"]
    shell: /bin/bash
    ssh_authorized_keys:
      - {pubkey}
ssh_pwauth: false
disable_root: true
package_update: true
packages:
{packages}
growpart:
  mode: auto
  devices: ['/']
resize_rootfs: true
runcmd:
  - [ bash, -lc, 'install -d -o {GUEST_USER} -g {GUEST_USER} {GUEST_HOME}/workspace' ]
"""


def cmd_up(args) -> int:
    for tool in ("virt-install", "virsh", "qemu-img", "curl", "passt"):
        if not shutil.which(tool):
            die(f"{tool} not found on PATH")
    if not os.access("/dev/kvm", os.R_OK | os.W_OK):
        die("/dev/kvm is not readable+writable by this user "
            "(join the kvm group, or check its mode)")

    state = domain_state(args.name)
    if state:
        if state != "running":
            virsh("start", args.name)
        print(f"domain {args.name} already defined ({state}); waiting for ssh")
        wait_for_ssh(args)
        return cmd_status(args)

    vmd = VM_DIR / args.name
    vmd.mkdir(parents=True, exist_ok=True)
    base = VM_DIR / Path(args.image_url).name
    if args.refresh_image or not base.exists():
        print(f"fetching {args.image_url}")
        tmp = base.with_suffix(base.suffix + ".part")
        run(["curl", "-fsSL", "-o", str(tmp), args.image_url])
        tmp.replace(base)
    st = base.stat()
    print(f"base image: {base} ({st.st_size // (1 << 20)} MiB, fetched "
          f"{time.strftime('%Y-%m-%d', time.localtime(st.st_mtime))}"
          f"{' — --refresh-image to re-fetch' if not args.refresh_image else ''})")

    disk = vmd / f"{args.name}.qcow2"
    if not disk.exists():
        run(["cp", "--reflink=auto", str(base), str(disk)])
        run(["qemu-img", "resize", str(disk), f"{args.disk}G"])

    key = ensure_ssh_key(args)
    user_data = vmd / "user-data"
    user_data.write_text(cloud_init_user_data(
        args, key.with_suffix(".pub").read_text().strip()))

    # portForward needs the passt backend: libvirt does not implement it for
    # the SLIRP user backend. Outbound to the LAN — which is where the harness
    # server is — works either way, so the forward is only for our own ssh.
    net = ("user,model=virtio,backend.type=passt,"
           "portForward0.proto=tcp,portForward0.address=127.0.0.1,"
           f"portForward0.range0.start={args.port},portForward0.range0.to=22")
    run(["virt-install", "--connect", CONNECT,
         "--name", args.name,
         "--memory", str(args.memory),
         # freePageReporting hands unused guest pages back to a host that has
         # no swap; without it the whole --memory is gone for the VM's life.
         "--memballoon", "model=virtio,freePageReporting=on",
         "--vcpus", str(args.vcpus),
         "--cpu", "host-passthrough",
         "--import",
         "--disk", f"path={disk},format=qcow2,bus=virtio",
         "--osinfo", args.osinfo,
         "--network", net,
         "--graphics", "none",
         "--console", "pty,target_type=serial",
         "--cloud-init", f"user-data={user_data}",
         "--noautoconsole"])

    print("waiting for ssh (cloud-init installs the toolchain on first boot)")
    wait_for_ssh(args)
    guest_sh(args, "sudo cloud-init status --wait >/dev/null 2>&1 || true")
    return cmd_status(args)


# ------------------------------------------------------------------ provision

def default_bin_dir() -> Path:
    """The MAIN checkout's bin/, even when this script runs from a worktree.

    Not `_ROOT/bin`: a harness task worktree has no bin/ of its own, and one
    built there is a private artifact that nothing refreshes — not the
    post-landing `make build`, not build_and_restart_all.py — so the guest
    would drift into running a build no other slot runs, which is the wire
    skew this fleet is careful about. The main checkout's bin/ is the trunk
    build every other slot runs. Pass --bin-dir to deliberately ship an
    unlanded build into the guest.
    """
    p = run(["git", "-C", str(_ROOT), "worktree", "list", "--porcelain"],
            capture=True, check=False)
    for line in p.stdout.splitlines() if p.returncode == 0 else []:
        if line.startswith("worktree "):
            return Path(line[len("worktree "):].strip()) / "bin"
    return _ROOT / "bin"


def host_binaries(args) -> list[Path]:
    """The three harness binaries the guest slot needs.

    Deliberately built binaries and not a guest-side `go build`: the slot must
    be wire-compatible with the running server, so it ships the same bytes the
    rest of the fleet runs.
    """
    bindir = Path(args.bin_dir).expanduser() if args.bin_dir else default_bin_dir()
    missing, found = [], []
    for name in ("agent-runner", "harness-cli", "harness-stream-adapter"):
        b = bindir / name
        (found if b.exists() else missing).append(b)
    if missing:
        die("missing " + ", ".join(str(m) for m in missing) +
            f" — run `make build` in {bindir.parent}")
    print(f"binaries from {bindir}")
    return found


def cmd_provision(args) -> int:
    if domain_state(args.name) != "running":
        die(f"domain {args.name} is not running (try `up`)")

    stage = f"{GUEST_BIN}/.staged"
    guest_sh(args, f"mkdir -p {stage} {GUEST_RUN} "
                   f"{shlex.quote(str(Path(GUEST_TOKEN).parent))} {GUEST_HOME}/workspace")
    # Everything lands in a staging dir and is then renamed into place. Writing
    # over a binary a live slot is executing fails with ETXTBSY; a rename does
    # not, because the running process keeps the old inode.
    scp_to(args, host_binaries(args), f"{stage}/")

    if args.agent in BRIDGED_AGENTS:
        agent_src = args.agent_bin or shutil.which(args.agent)
        if not agent_src:
            die(f"no {args.agent} binary on PATH; pass --agent-bin")
        # The agent binary is usually a symlink into a versioned directory, and
        # scp would copy the link, not the payload.
        scp_to(args, [Path(agent_src).resolve()], f"{stage}/{args.agent}", preserve=True)
    else:
        # Copying the host's /usr/bin/bash into ~/.local/bin would shadow the
        # guest's own on PATH with a binary built against a different libc.
        print(f"{args.agent}: using the guest image's own binary, nothing bridged")

    token = Path(args.token_file).expanduser() if args.token_file else None
    if args.agent not in TOKEN_ENV_BY_AGENT or args.auth == "none":
        # The guest logs itself in, and its home is persistent, so that login
        # and its session store survive every task and every slot restart.
        # Removing any token file left by an earlier `--auth token` keeps the
        # answer to "how does this slot authenticate" a single one: the launch
        # script exports the token only when that file exists.
        guest_sh(args, f"rm -f {GUEST_TOKEN}")
        if args.agent in TOKEN_ENV_BY_AGENT:
            print(f"auth=none: log in inside the guest once —\n"
                  f"  scripts/kvm-runner/kvm-runner.py --name {args.name} ssh -- {args.agent}\n"
                  f"  (or `{args.agent} setup-token`); it persists in the guest's ~/.claude")
    elif token and token.exists():
        # Passed straight to scp: the bytes never enter this process.
        scp_to(args, [token], GUEST_TOKEN)
        print(f"auth=token: installed from {token}")
    else:
        die(f"--auth token but no token file at {token} (pass --token-file, or --auth none)")

    email = run(["git", "config", "--get", "user.email"], capture=True, check=False).stdout.strip()
    who = run(["git", "config", "--get", "user.name"], capture=True, check=False).stdout.strip()
    lab = args.roots or f"{GUEST_HOME}/workspace/ebpf-lab"
    guest_sh(args, f"""set -e
for f in {stage}/*; do mv -f "$f" {GUEST_BIN}/"$(basename "$f")"; done
rmdir {stage}
chmod 700 {GUEST_BIN}/*
if [ -f {GUEST_TOKEN} ]; then chmod 600 {GUEST_TOKEN}; fi
git config --global init.defaultBranch main
{f'git config --global user.email {shlex.quote(email)}' if email else ':'}
{f'git config --global user.name {shlex.quote(who)}' if who else ':'}
# --roots wants a repo, and `git worktree add` wants a commit to branch from.
if [ ! -d {shlex.quote(lab)}/.git ]; then
  mkdir -p {shlex.quote(lab)} && cd {shlex.quote(lab)} && git init -q
  printf '# %s\\n\\nBPF experiments run in the {args.name} guest.\\nA bad program wedges THIS kernel, not the host.\\n' "$(basename {shlex.quote(lab)})" > README.md
  git add README.md && git commit -q -m 'chore: seed the runner worktree root'
fi
echo "--- guest ---"; uname -n; uname -r
ls -l /sys/kernel/btf/vmlinux || echo "NO BTF: CO-RE will not work"
{f'{GUEST_BIN}/{args.agent}' if args.agent in BRIDGED_AGENTS else args.agent} --version 2>&1 | head -1
{GUEST_BIN}/harness-cli version 2>&1 | head -1
""")
    return 0


# --------------------------------------------------------------------- runner

def slot_base(args) -> str:
    """Per-slot file prefix in the guest. One guest can carry several slots."""
    return f"{GUEST_RUN}/agent-runner-{args.slot or args.agent}"


def runner_flags(args) -> list[str]:
    """The host's own preset for this agent, retargeted at guest paths.

    Read out of scripts/agent_presets.py rather than restated here: a slot
    whose argv templates drift from the host's presets fails in the way that
    is hardest to see — the agent runs, and only its streamed progress is
    wrong (one final blob instead of events).

    --agent-bin is rewritten only for a bridged agent. Leaving a non-bridged
    one as the preset's bare name is load-bearing twice over: the guest's own
    PATH then resolves it, and the runner names its default profile after the
    basename, which is what `submit --agent <name>` matches on.
    """
    flags = agent_presets.expand_agents_preset(args.agent, [])
    out: list[str] = []
    it = iter(flags)
    for flag in it:
        val = next(it)
        if flag == "--agent-bin" and args.agent in BRIDGED_AGENTS:
            val = f"{GUEST_BIN}/{args.agent}"
        elif flag == "--agent-stream-adapter" and val:
            val = f"{GUEST_BIN}/harness-stream-adapter"
        out += [flag, val]
    return out


def cmd_runner_up(args) -> int:
    if domain_state(args.name) != "running":
        die(f"domain {args.name} is not running (try `up`)")
    cid = args.server_cid or os.environ.get("HARNESS_SERVER_CID", "")
    if not cid:
        die("no --server-cid and no HARNESS_SERVER_CID in the environment")

    lab = args.roots or f"{GUEST_HOME}/workspace/ebpf-lab"
    base = slot_base(args)
    argv = [f"{GUEST_BIN}/agent-runner",
            "--shutdown-file", f"{base}.shutdown",
            *runner_flags(args),
            "--server-cid", cid,
            "--hostname", args.name,
            "--roots", lab,
            # 1, not the 4-8 a host slot carries: a guest panic fails every
            # task on the slot at once, and each one is resumed by hand.
            "--max-tasks", str(args.max_tasks)]
    if args.no_worktree:
        # Tasks then run in the bound repo path itself. Concurrent tasks would
        # share one working tree, which is the other reason --max-tasks is 1.
        argv.append("--no-worktree")

    token_env = TOKEN_ENV_BY_AGENT.get(args.agent)
    # `-s`, and only then: exporting the variable EMPTY (which
    # `$(cat missing-file)` does) is worse than not exporting it, because the
    # agent sees its token env set and can take the token path with nothing in
    # it instead of reading the credentials the guest holds. Measured: after
    # `--auth none` removed the file, the slot still carried an empty
    # CLAUDE_CODE_OAUTH_TOKEN.
    export = (f"if [ -s {GUEST_TOKEN} ]; then export {token_env}=\"$(cat {GUEST_TOKEN})\"; fi\n"
              if token_env else "")
    # TERM, explicitly, because nothing downstream supplies it: the runner does
    # not set one on the PTY it opens for the agent, so the agent inherits the
    # runner's, and a daemon started over a TTY-less ssh has none at all. The
    # agent then renders monochrome. A host slot started from a terminal picks
    # up xterm-256color by accident; here it has to be said.
    export += f"export TERM={shlex.quote(args.term)}\n"
    # Written into the guest as a file rather than interpolated into an ssh
    # command line: the argv templates contain spaces and braces, and this
    # leaves the exact launch inspectable next to its log.
    launch = ("#!/bin/bash\nset -e\n" + export +
              f"rm -f {base}.shutdown\n" +
              "exec " + " ".join(shlex.quote(a) for a in argv) + "\n")
    guest_sh(args, f"""set -e
if [ -f {base}.pid ] && kill -0 "$(cat {base}.pid)" 2>/dev/null; then
  echo "slot already running (pid $(cat {base}.pid))"; exit 0
fi
cat > {base}.launch <<'LAUNCH'
{launch}LAUNCH
chmod 700 {base}.launch
setsid nohup {base}.launch >> {base}.log 2>&1 < /dev/null &
echo $! > {base}.pid
sleep 3
kill -0 "$(cat {base}.pid)" 2>/dev/null || {{ echo "slot died on start:"; tail -20 {base}.log; exit 1; }}
echo "slot {args.slot or args.agent}: pid $(cat {base}.pid)"
tail -6 {base}.log
""")
    print(f"\nhostname={args.name}, profile={args.agent}"
          f"{' (no-worktree)' if args.no_worktree else ''}; reach it with:\n"
          f"  harness-cli submit --host {args.name} --agent {args.agent} "
          f"--repo {lab} --task '...'")
    return 0


def cmd_runner_down(args) -> int:
    if domain_state(args.name) != "running":
        print(f"domain {args.name} is not running; nothing to stop")
        return 0
    base = slot_base(args)
    guest_sh(args, f"""
[ -f {base}.pid ] || {{ echo "no pid file for slot {args.slot or args.agent}"; exit 0; }}
pid=$(cat {base}.pid)
touch {base}.shutdown
for _ in $(seq 1 20); do
  kill -0 "$pid" 2>/dev/null || {{ echo "slot $pid exited via shutdown-file"; rm -f {base}.pid; exit 0; }}
  sleep 1
done
echo "shutdown-file did not take after 20s; sending TERM to $pid"
kill -TERM "$pid" 2>/dev/null || true
""", check=False)
    return 0


# --------------------------------------------------------- snapshots / status

def cmd_snapshot(args) -> int:
    tag = args.tag or time.strftime("clean-%Y%m%d-%H%M%S")
    # Internal qcow2 snapshot of a live domain: this is the only cheap answer
    # to kernel state (pinned programs, attached hooks) surviving a task.
    virsh("snapshot-create-as", "--domain", args.name, "--name", tag,
          "--description", "kvm-runner clean point", "--atomic")
    virsh("snapshot-list", args.name)
    return 0


def stop_all_slots(args) -> None:
    """Stop every slot in the guest, not just --slot's.

    Whole-guest operations (shutdown, snapshot revert) take the kernel out
    from under all of them, so leaving one running means the server marks its
    tasks Failed on a drop we chose to cause.
    """
    if domain_state(args.name) != "running":
        return
    guest_sh(args, f"""
shopt -s nullglob
for pidf in {GUEST_RUN}/agent-runner-*.pid; do
  slot=$(basename "$pidf" .pid); slot=${{slot#agent-runner-}}
  pid=$(cat "$pidf")
  touch "{GUEST_RUN}/agent-runner-$slot.shutdown"
  for _ in $(seq 1 20); do
    kill -0 "$pid" 2>/dev/null || break
    sleep 1
  done
  kill -0 "$pid" 2>/dev/null && kill -TERM "$pid" 2>/dev/null || true
  rm -f "$pidf"
  echo "stopped slot $slot"
done
""", check=False)


def cmd_revert(args) -> int:
    tag = args.tag
    if not tag:
        p = virsh("snapshot-list", args.name, "--name", capture=True)
        names = [n for n in p.stdout.split() if n]
        if not names:
            die("no snapshots to revert to")
        tag = names[-1]
    print(f"reverting {args.name} to {tag} — every task on this guest dies with it")
    stop_all_slots(args)
    virsh("snapshot-revert", "--domain", args.name, "--snapshotname", tag, "--running")
    wait_for_ssh(args)
    return 0


def cmd_status(args) -> int:
    state = domain_state(args.name) or "(not defined)"
    print(f"domain    {args.name}: {state}")
    fwd = run(["ss", "-tln"], capture=True, check=False).stdout
    listening = any(f":{args.port} " in line for line in fwd.splitlines())
    print(f"ssh fwd   127.0.0.1:{args.port}: {'listening' if listening else 'absent'}")
    if state != "running":
        return 0
    guest_sh(args, f"""
echo "guest     $(uname -n)  kernel $(uname -r)"
echo -n "btf       "; [ -r /sys/kernel/btf/vmlinux ] && echo present || echo "MISSING (no CO-RE)"
echo -n "mem       "; free -m | awk 'NR==2{{printf "%s MiB total, %s available\\n", $2, $7}}'
shopt -s nullglob
found=0
for pidf in {GUEST_RUN}/agent-runner-*.pid; do
  found=1
  slot=$(basename "$pidf" .pid); slot=${{slot#agent-runner-}}
  pid=$(cat "$pidf")
  if kill -0 "$pid" 2>/dev/null; then
    echo "slot      $slot: pid $pid alive, $(grep -c 'persist: connected' "{GUEST_RUN}/agent-runner-$slot.log" 2>/dev/null || echo 0) connect(s)"
    tail -2 "{GUEST_RUN}/agent-runner-$slot.log" 2>/dev/null | sed "s/^/  $slot  /"
  else
    echo "slot      $slot: pid $pid DEAD (stale pid file)"
  fi
done
[ "$found" = 1 ] || echo "slot      none running"
""", check=False)
    return 0


def cmd_ssh(args, extra: list[str]) -> int:
    argv = ssh_argv(args, tty=True)
    if extra:
        argv += extra
    return subprocess.run(argv).returncode


def cmd_down(args) -> int:
    if domain_state(args.name) != "running":
        print("already off")
        return 0
    stop_all_slots(args)
    virsh("shutdown", args.name)
    for _ in range(60):
        if domain_state(args.name) != "running":
            print("guest off")
            return 0
        time.sleep(1)
    print("still running after 60s; destroying")
    virsh("destroy", args.name, check=False)
    return 0


def cmd_destroy(args) -> int:
    if not args.force:
        die("destroy removes the domain AND its disk; pass --force")
    # Ask libvirt where the disk actually is instead of assuming this script's
    # own layout: a domain defined by hand, or before a layout change, points
    # somewhere else, and the assumed path would unlink nothing while
    # reporting success.
    disks = []
    p = virsh("domblklist", args.name, "--details", capture=True, check=False)
    if p.returncode == 0:
        for line in p.stdout.splitlines():
            f = line.split()
            if len(f) == 4 and f[0] == "file" and f[3].startswith("/"):
                disks.append(Path(f[3]))
    virsh("destroy", args.name, check=False)
    virsh("undefine", args.name, "--nvram", "--snapshots-metadata", check=False)
    for disk in disks:
        if disk.parent == VM_DIR:
            # Per-guest disks live in VM_DIR/<name>/; anything directly in
            # VM_DIR is a base image other guests share.
            print(f"kept {disk} (a shared base image)")
            continue
        if disk.exists():
            disk.unlink()
            print(f"removed {disk}")
    # The per-guest directory otherwise keeps a cloud-init seed for a guest
    # that no longer exists, which a later `up` of the same name would look
    # like it reused (it regenerates it).
    vmd = VM_DIR / args.name
    if vmd.is_dir() and all(f.name in ("user-data", "known_hosts") for f in vmd.iterdir()):
        for f in vmd.iterdir():
            f.unlink()
        vmd.rmdir()
        print(f"removed {vmd}")
    return 0


# ----------------------------------------------------------------------- main

def main() -> int:
    ap = argparse.ArgumentParser(prog="kvm-runner.py", add_help=True)
    ap.add_argument("--name", default="kvm-ebpf", help="guest name; also the runner's --hostname")
    ap.add_argument("--port", type=int, default=2222, help="host-side ssh forward port")
    ap.add_argument("--ssh-key", default="~/.ssh/id_ed25519_kvm_guest",
                    help="key for the guest; generated if absent")
    ap.add_argument("--agent", default="claude", help="agent preset name (scripts/agent_presets.py)")
    ap.add_argument("--slot", default="",
                    help="slot label for this guest's pid/log files (default: the agent name). "
                         "Slots share the guest's hostname and are told apart by profile.")
    ap.add_argument("--roots", default="", help="guest-side repo root the slot serves")
    ap.add_argument("--bin-dir", default="",
                    help="harness bin/ to ship into the guest "
                         "(default: the MAIN checkout's, i.e. the build the rest of the fleet runs)")
    sub = ap.add_subparsers(dest="cmd", required=True)

    up = sub.add_parser("up", help="create (or start) the guest and wait for ssh")
    up.add_argument("--memory", type=int, default=4096)
    up.add_argument("--vcpus", type=int, default=2)
    up.add_argument("--disk", type=int, default=40, help="disk size in GiB")
    up.add_argument("--image-url", default=DEFAULT_IMAGE_URL,
                    help="cloud image to import; cached in ~/vm/ under its basename")
    up.add_argument("--refresh-image", action="store_true",
                    help="re-fetch the image even when the cached copy exists "
                         "(a 'latest' URL is otherwise frozen at first use)")
    up.add_argument("--osinfo", default=DEFAULT_OSINFO,
                    help="virt-install --osinfo; must match --image-url")
    up.add_argument("--packages", default="",
                    help="comma-separated packages for cloud-init to install "
                         "(default: the Arch/pacman eBPF set; change it with a non-Arch image)")

    pv = sub.add_parser("provision", help="push binaries + token, seed the lab repo")
    pv.add_argument("--agent-bin", default="", help="host path to the agent binary (default: from PATH)")
    pv.add_argument("--auth", choices=("none", "token"), default="none",
                    help="none (default): the guest holds its own login, done once "
                         "inside it and persistent. token: install --token-file and "
                         "export it to the slot instead.")
    pv.add_argument("--token-file", default="~/.config/harness/sandbox-claude-token",
                    help="the token --auth token installs")

    rn = sub.add_parser("runner", help="start/stop the agent-runner in the guest")
    rnsub = rn.add_subparsers(dest="rcmd", required=True)
    rup = rnsub.add_parser("up")
    rup.add_argument("--server-cid", default="", help="default: $HARNESS_SERVER_CID")
    rup.add_argument("--max-tasks", type=int, default=1)
    rup.add_argument("--no-worktree", action="store_true",
                     help="run tasks in the bound repo path instead of a per-task worktree")
    rup.add_argument("--term", default="xterm-256color",
                     help="TERM for the slot, inherited by every agent it spawns "
                          "(without one the agent renders monochrome)")
    rnsub.add_parser("down")

    sub.add_parser("status")
    sh = sub.add_parser("ssh")
    sh.add_argument("rest", nargs=argparse.REMAINDER)
    sn = sub.add_parser("snapshot")
    sn.add_argument("--tag", default="")
    rv = sub.add_parser("revert")
    rv.add_argument("--tag", default="")
    sub.add_parser("down")
    ds = sub.add_parser("destroy")
    ds.add_argument("--force", action="store_true")

    args = ap.parse_args()
    if args.cmd == "up":
        return cmd_up(args)
    if args.cmd == "provision":
        return cmd_provision(args)
    if args.cmd == "runner":
        return cmd_runner_up(args) if args.rcmd == "up" else cmd_runner_down(args)
    if args.cmd == "status":
        return cmd_status(args)
    if args.cmd == "ssh":
        rest = args.rest[1:] if args.rest[:1] == ["--"] else args.rest
        return cmd_ssh(args, rest)
    if args.cmd == "snapshot":
        return cmd_snapshot(args)
    if args.cmd == "revert":
        return cmd_revert(args)
    if args.cmd == "down":
        return cmd_down(args)
    if args.cmd == "destroy":
        return cmd_destroy(args)
    die(f"unhandled subcommand {args.cmd}")


if __name__ == "__main__":
    raise SystemExit(main())
