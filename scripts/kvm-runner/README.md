# A runner slot inside a KVM guest

For work that breaks the kernel — eBPF programs, kernel modules, anything that
attaches to the live kernel and can wedge it. The guest is an **ordinary runner
host**, the same shape as a slot on another machine, so the harness needed no
code changes: only `kvm-runner.py` is new.

The podman kit next door (`scripts/sandbox/`) cannot host this work. Loading a
BPF program needs `CAP_BPF` against a **shared** kernel, so a container can only
refuse the capability or hand over the host's own kernel. Neither is a place to
develop eBPF. Here the kernel that can be wedged is the guest's, and a guest is
cheap to reboot or roll back.

## Prerequisites

Rootless, and on this host it needed **nothing installed** (measured 2026-09-08):

- `/dev/kvm` is mode 0666, so no `kvm` group membership is required;
- `qemu-system-x86_64` 11.0.2 with `virtio-9p`, `vhost-vsock`, `-netdev stream`
  and `-machine microvm` all present;
- `passt` — **load-bearing**: libvirt implements `<portForward>` only for the
  passt backend, not for SLIRP user networking;
- `virt-install` / `libvirt` in **session mode** (`qemu:///session`), no system
  daemon and no sudo.

The runner dials out, so outbound-to-LAN is all the network the guest needs: no
bridge, no inbound rule, no port forward for the harness itself. The one forward
(`127.0.0.1:2222 → 22`) exists for this script's own ssh.

## Bring-up

```sh
scripts/kvm-runner/kvm-runner.py up          # create the guest, wait for ssh
scripts/kvm-runner/kvm-runner.py provision   # binaries + a seeded lab repo
scripts/kvm-runner/kvm-runner.py runner up   # join the server as a slot
```

Then log the guest in once — it keeps that login, because its home is a real
disk:

```sh
scripts/kvm-runner/kvm-runner.py ssh -- claude     # /login, or `claude setup-token`
```

A second slot in the same guest, running commands in the workspace itself
instead of a per-task worktree:

```sh
scripts/kvm-runner/kvm-runner.py --agent bash runner up --no-worktree
```

`kvm-runner.py status` prints the whole picture: domain state, the ssh forward,
the guest's kernel and BTF, and every slot with its connect count and last log
lines. `down` / `destroy` and `snapshot` / `revert` do what they say.

## Two slots, one hostname

| slot | profile | worktree | root it serves |
|---|---|---|---|
| claude | `claude` | per task | `~/workspace/ebpf-lab` |
| bash | `bash` | **none** — tasks run in the root | `~/workspace` |

They report the **same hostname**, and that is fine: they are addressed by
profile, not by host. A runner names its default profile after its
`--agent-bin` basename, so these advertise `claude` and `bash`, and
`submit --agent bash` narrows the candidates before the ambiguity check.
Interactive opens list one row per *(runner, profile)* combo, so the picker
separates them too.

```sh
harness-cli submit --host <name> --agent bash   --repo ~/workspace          --task '...'
harness-cli submit --host <name> --agent claude --repo ~/workspace/ebpf-lab --task '...'
```

The roots are a deliberate super-set/sub-set pair: candidate matching keeps only
the runners tying for the **longest** matching root, so each `--repo` resolves to
exactly one slot. **The corollary is worth knowing before you meet it:**
`--repo ~/workspace/ebpf-lab --agent bash` answers `ProfileUnavailable`, not a
fallback to the broader slot — the lab path belongs to the worktree slot, which
does not advertise `bash`.

## What this does NOT give you

- **A fresh kernel per task.** The guest kernel outlives every task on the slot.
  Pinned programs in bpffs, attached tc/XDP/kprobe hooks and changed sysctls
  carry into the next one. `snapshot` / `revert` is the cleanup — and a revert
  takes every task on the guest with it.
- **A blast radius of one task.** When the guest panics, the runner's connection
  drops and the server marks **every** task active on that runner Failed, each of
  which a human then resumes. That is why `--max-tasks` defaults to 1 here
  instead of the 4–8 a busy slot carries. Keep unrelated work on another slot.
- **Any isolation inside the guest.** BPF needs root, so the agent has
  passwordless sudo: it can destroy its checkout and its credentials. Put
  nothing in the guest you cannot re-create. The VM is the wall; inside it,
  everything is trusted.

## Getting work out

Two shapes, and which one you want depends on where the code lives.

**Work created in the guest** — nothing to set up. The host reads the guest's
repo over the ssh forward that already exists, so no credential ever goes into
the guest:

```sh
# ~/.ssh/config: Host <name> / HostName 127.0.0.1 / Port 2222 / IdentityFile …
git remote add kvm <name>:workspace/<repo>
git fetch kvm 'refs/heads/harness/*:refs/remotes/kvm/harness/*'   # task branches too
git push origin <branch>
```

**A host repo you want the guest to work on** — share it, and the transfer step
disappears: the agent's commits land in the host's own `.git`, so all that is
left for you is `git push`.

```sh
scripts/kvm-runner/kvm-runner.py up --share ~/workspace/<repo>   # also adds it to an existing guest
scripts/kvm-runner/kvm-runner.py provision                       # mounts it, with an fstab entry
```

The share is mounted at the **same absolute path** in the guest, so a path means
the same thing on both sides.

Three things about it are worth knowing:

- **The guest needs an idmap, and libvirt's default is the wrong way round.**
  For a rootless virtiofsd libvirt maps guest *root* to the host user and guest
  1000 into the subuid range, so the agent cannot write the share at all —
  host files read as root-owned inside the guest, and git adds "dubious
  ownership" on top. `up --share` installs the keep-id equivalent instead (the
  agent's own uid maps through unchanged, everything else to subuids) and
  bounces the guest once, because a filesystem device and its shared memory
  backing are both boot-time.
- **An agent with root in the guest can write whatever you export.** Export one
  repo, not `$HOME` — the same trade the podman kit makes with its bind mounts.
- **Do not give a guest slot the same exact `--roots` string as a host slot.**
  That is the one configuration where the server cannot tell the two runners
  apart. A share alone does not cause it: root matching keeps only the longest
  match, so a host slot serving `~/workspace/<repo>` shadows a guest slot
  serving `~/workspace`, and the guest's no-worktree slot never creates
  worktrees anyway.

Landing to a trunk still works the way it does from any other separate runner
host: push, then fast-forward the trunk-authoritative checkout.

## Authentication

`--auth none` is the default: the guest holds its own login. `--auth token`
installs a revocable token file and exports it to the slot instead.

Both halves of the podman kit's argument for token auth were checked here and
**neither transfers to a VM** (measured 2026-09-08):

- *"token auth costs you resume"* is a property of the container's **ephemeral
  HOME**, not of token auth. A guest's home is a real disk, so `~/.claude` keeps
  the session store across tasks and slot restarts — a real transcript was found
  at `~/.claude/projects/<cwd-hash>/<uuid>.jsonl` after a run.
- *"token auth keeps the refresh token out of the sandbox"* is false here.
  After one run under `CLAUDE_CODE_OAUTH_TOKEN`, the guest's
  `~/.claude/.credentials.json` held a full `claudeAiOauth` blob **including a
  refreshToken**, plus a trustedDeviceToken. The token is exchanged for exactly
  what it was meant to avoid.

Copying the host's `~/.claude` in — the VM analogue of the kit's mount auth — is
**not offered**: that directory is hundreds of MB of host transcripts, while the
credential inside it is a single small file.

## The image

The default is the Arch cloud image, overridable with `--image-url`. Know what
the default URL's `/latest/` means for the cache: the file it downloads to is
named from the URL and carries **no version**, so once `~/vm/<basename>` exists
nothing re-fetches it and the guest is pinned to whenever `up` first ran,
silently, while the URL keeps claiming to be current. `up` prints the cached
image's fetch date for that reason; `--refresh-image` re-fetches.

Changing distro is more than the URL: `--osinfo` must match the image, and the
cloud-init package list is **pacman names**, so a non-Arch image needs
`--packages` too or it comes up with no toolchain.

What the default gave on 2026-09-08: kernel 7.2.2 (newer than the host's),
`/sys/kernel/btf/vmlinux` present, `bpftool` / `bpftrace` / `clang` /
`linux-headers` installed, `bpf() syscall is available`, kprobe and xdp program
types available.

## Checking the claims instead of trusting them

| claim | how to check |
|---|---|
| the guest can load BPF | `ssh -- sudo bpftool feature probe \| grep -E 'syscall is available\|BTF'` |
| BTF is there for CO-RE | `status` prints `btf present`; else `ls /sys/kernel/btf/vmlinux` |
| the slot is connected | `status` prints each slot's `connect(s)` count and log tail |
| the slot runs the build you shipped | `readlink /proc/<pid>/exe` — no `(deleted)`, and its inode equals the on-disk one |
| agents get colour | `tr '\0' '\n' < /proc/<slot-pid>/environ \| grep TERM` |
| no stale credential in the env | the same, grepping the token variable — it must be **absent**, not empty |

## Traps this cost, so you do not pay them again

- **The runner sets no `TERM` on the PTY it opens**, so the agent inherits the
  runner's, and a slot started from a TTY-less context (`ssh host cmd`, nohup
  from a script, a unit) has none at all — the agent then renders monochrome. A
  slot started from a terminal picks up `xterm-256color` by accident. This script
  exports it explicitly (`runner up --term`).
- **An empty token variable is worse than an absent one.** `$(cat missing-file)`
  exports it set-and-empty, and the agent can then take the token path with
  nothing in it instead of reading the credentials the guest holds.
- **Re-provisioning a live guest hits `ETXTBSY`**: you cannot write over a binary
  a running slot is executing. `provision` stages and renames instead — the
  running process keeps the old inode, so restart the slot to pick up the new
  one.
- **`bin/` in a task worktree is not the fleet's build.** `provision` ships the
  **main checkout's** `bin/` on purpose (`--bin-dir` to override): a worktree
  build is refreshed by nothing, so the guest would drift into the one slot
  running a private build.
- **The Arch cloud image has no `hostname` binary.** Under `set -e` that aborts a
  provisioning script with 127; `uname -n` is always there.
- **`virsh domstate` is translated.** On a non-English host it answers something
  other than `running`, and a naive comparison reads a live guest as off. This
  script runs virsh under `LC_ALL=C`.
