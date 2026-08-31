# `netem-lab` — a shaped WAN path for the transport, on one machine — design

## Problem

Every throughput figure this project has recorded for the transport stack
(`objproto` + `trsf`, in the companion `objtrsf` module) was taken over
loopback or a local link: roughly 20 MB/s native, 2.8 MB/s from wasm, 3.3 GB/s
over loopback. Those paths have a round-trip time near zero, no loss, a fixed
MTU, no NAT and no bottleneck queue. The congestion control, the
retransmission timers, the path-MTU handling and the keepalive interval are
therefore unmeasured against the conditions they exist for.

- **P1.** Nothing in this repo produces a delayed, lossy, rate-limited or
  small-MTU path. A search of `*.sh`, `*.py`, `*.go` and `*.md` for `netem`,
  `tc qdisc`, `netns` and `ip link add` returns no test scaffolding — only
  incidental matches in the session-debugging skill and the exec specs. There
  is nothing to point the transport at.

- **P2.** The obvious tool does not, by itself, answer the question.
  `tc netem loss X%` drops packets from a process that is **independent of the
  sender's rate**. Real congestion loss is *caused* by the sender: it appears
  when a queue the sender is filling overflows. A controller that behaves
  correctly against rate-independent loss can behave arbitrarily badly against
  self-induced loss, so a lab built only on `netem loss` would report a green
  result for the property least likely to hold.

- **P3.** Two failure classes have no local reproduction at all today:
  a NAT mapping that expires before the keepalive interval reaches it, and a
  path-MTU black hole (a router that drops packets over the MTU *and* drops the
  ICMP `fragmentation-needed` that would report it). Both are ordinary on real
  paths and both present as an unexplained stall.

- **P4.** Reproducing any of the above against a real remote host costs money
  per month and, more importantly, **is not bisectable**: an internet path
  cannot be re-run with exactly one parameter changed. A finding obtained that
  way cannot be narrowed except by guessing.

- **P5.** `scripts/dummy-harness.py` cannot be reused for this. It starts the
  server and the runner **from one process, both on loopback**, so the two land
  in the same network namespace with nothing between them. Shaping loopback
  instead would shape every other loopback user on the host — the operator's
  own terminal sessions included.

## Decisions taken

Each decision below is `DECIDED (operator, 2026-08-31)` unless marked
otherwise. D1, D2, D4 and the P3 knobs rest on measurements taken on this
machine before the spec was written; the commands and results are in
**Evidence**. D8 rests on a platform fact, not a measurement: no measurement
was taken on Windows or macOS, and none is needed to know that `unshare` and
network namespaces are Linux interfaces.

- **D1.** The lab runs **without real root**, inside a user namespace created
  by `unshare --user --map-root-user`. No `sudo`, no setuid helper, no
  capability grant on any binary.

- **D2.** The topology is **three** network namespaces — `srv`, `rtr`, `cli` —
  not two. A NAT and an ICMP-dropping middlebox both require a device in the
  middle of the path; a bare veth pair has no middle.

- **D3.** The NAT masquerades traffic **from `cli` toward `srv`**, so the
  *runner* sits behind it. That is the direction the real deployment has: the
  runner dials the server (`cmd/agent-runner/main.go` defaults its
  `--server-cid` to a dial target), and the server is the reachable side.

- **D4.** Namespaces are held open by **detached placeholder processes** and
  re-entered from later invocations with
  `nsenter --preserve-credentials --user=/proc/PID/ns/user --net=/proc/PID/ns/net`.
  The `--preserve-credentials` flag is not optional; see Evidence.

- **D5.** The bottleneck is `htb` for rate with a **`netem` leaf whose `limit`
  is the queue depth**, so that loss is produced by *queue overflow* rather
  than by `netem`'s loss knob. This is the direct answer to P2. A `--loss` knob
  still exists, but the README and `--help` both state that it models link
  corruption, not congestion, and its default is 0.

- **D6.** Everything lives under **`scripts/netem-lab/`**. Nothing is added at
  `scripts/` top level. This follows `scripts/sandbox/`, which is the existing
  precedent for a self-contained kit with its own `README.md`.

- **D7.** `scripts/dummy-harness.py` is **not modified**. Its pure helpers
  (`tmp_root`, `pick_port`, `listening`, `kill_pid`, `build`,
  `go_env_for_make`, `bash_profile`, `FAKE_AGENT`, `scrub_own_env`,
  `survive_undisplayable_output`) are borrowed by loading it
  with `importlib.util`, exactly as `scripts/test_dummy_harness.py` already
  does. No shared module is extracted and no existing tool is rewritten.
  Observable failure this predicts: none — `python3
  scripts/test_dummy_harness.py` stays green and `scripts/dummy-harness.sh up`
  behaves identically, because no byte of `dummy-harness.py` changes and
  importing it runs only `ensure_venv()` and `import daemon`, both of which the
  lab wants anyway.

  The launch orchestration is **not** borrowed. It is welded into `cmd_up` and
  it genuinely differs here: three namespaces, an `nsenter` prefix on every
  spawn, a UDP transport option and a different readiness probe. If a second
  consumer of that orchestration ever appears, extract it then.

- **D8.** **Linux only.** `unshare`, network namespaces, `tc` and `iptables`
  have no equivalent on Windows or macOS. The preflight refuses on a non-Linux
  platform and names the reason. Observable failure this predicts: on
  Windows, `up` exits 2 with a platform message rather than failing somewhere
  inside `ip link`.

- **D9.** `shape` is a **separate verb** from `up`, and re-shapes a running
  lab. Path conditions that change mid-transfer cannot be tested by a tool
  that only sets conditions at startup.

- **D10.** The server always listens on **both** WebSocket and UDP; a
  `--transport {udp,ws}` flag selects which one the *runner* dials. The
  default is `udp`, because the UDP leg is where `trsf`'s own congestion
  control runs — over WebSocket the kernel's TCP does that work and the code
  under test is mostly bypassed.

- **D11.** Tests are `unittest`, stdlib-only, run directly as
  `python3 scripts/netem-lab/test_netem_lab.py`, matching
  `scripts/test_dummy_harness.py`. **No make target runs them**: `make test`
  is `go test ./...` and `scripts/requirements.txt` contains only
  `psutil>=5.9`, so pytest is not even installed. This is written down because
  a reader who assumes `make test` covers Python is wrong today.

- **D12.** The lab's subnets are `10.90.0.0/24` (srv↔rtr) and `10.91.0.0/24`
  (rtr↔cli), overridable with `--subnet-a` / `--subnet-b`. They cannot collide
  with anything the host can reach: the namespaces have no route to the host's
  network namespace, so an address that also exists on a real LAN is simply a
  different address in a different namespace. Observable failure this
  predicts: none — a host on `10.90.0.0/24` stays reachable from the host
  namespace while the lab runs.

## Problem coverage

Every bullet in **Problem** and where it is answered. A bullet with no row
here would be a scope contraction between the two sections, which is the
failure this table exists to make visible.

| | answered by |
|---|---|
| **P1** — no shaped path exists | **Shape → Topology** and **Profiles**: three namespaces with `netem` + `htb` on both `rtr`-side devices, and named profiles spanning 1 ms to 115 ms one-way with and without a bottleneck. |
| **P2** — `netem loss` is rate-independent | **D5** and **Shape → Profiles**: when `rate` is set the chain is `htb` root → `netem` leaf and `limit` is the queue depth, so loss comes from overflow the sender caused. `--loss` stays available and is documented as link corruption. **Calibration** names the `tc -s qdisc` counters that tell the two apart. |
| **P3** — NAT expiry and PMTU black holes | **Shape → Middlebox behaviour**: `--conntrack-udp-timeout`, `--mtu`, `--pmtu-blackhole`, with `--no-nat` as the control case. Rootless feasibility measured in **Evidence**. |
| **P4** — a real path is not bisectable | The whole tool is local and deterministic, and **D9** makes `shape` a separate verb so exactly one parameter can be changed — on a running lab, without restarting the processes under test. |
| **P5** — `dummy-harness.py` cannot be reused | **D6** and **D7**: a separate tool under `scripts/netem-lab/` that borrows only the pure helpers and writes its own three-namespace orchestration. `dummy-harness.py` is unchanged. |

## Evidence

Measured 2026-08-31 on Linux 7.1.3, before the design was fixed. These are the
load-bearing mechanisms; each was run rather than assumed.

**User namespaces are available and grant `CAP_NET_ADMIN` inside (D1).**

```
$ sysctl user.max_user_namespaces
user.max_user_namespaces = 62803

$ unshare --user --map-root-user --net --mount bash -c '
    ip link add v0 type veth peer name v1
    ip addr add 10.99.0.1/24 dev v0 && ip link set v0 up
    tc qdisc add dev v0 root netem delay 50ms limit 2000
    tc qdisc show dev v0; id -u'
qdisc netem 8001: root refcnt 5 limit 2000 delay 50ms seed ...
0
```

`htb` was created the same way in the same namespace.

**A veth end can be moved into a second namespace held by a PID (D2).**

```
$ unshare --user --map-root-user --net --mount bash -c '
    unshare --net sleep 20 & P=$!; sleep 0.4
    ip link add v0 type veth peer name v1
    ip link set v1 netns $P
    nsenter --net=/proc/$P/ns/net ip addr add 10.99.0.2/24 dev v1
    nsenter --net=/proc/$P/ns/net tc qdisc add dev v1 root netem delay 25ms limit 3000'
```

Both the address and the qdisc were accepted in the second namespace.

**qdisc modules autoload from inside the user namespace on this kernel.**
`sch_cake` was absent from `lsmod`, was created from inside a user namespace,
and appeared in `lsmod` afterwards. `CONFIG_NET_SCH_NETEM` and
`CONFIG_NET_SCH_HTB` are both `=m` here, so this was a real autoload and not a
built-in. This is **not** assumed to hold elsewhere — a kernel that refuses the
autoload is exactly what the preflight's netem probe is for.

**NAT, the ICMP drop, the conntrack timeout and the MTU are all settable
rootlessly (P3).** Run inside one `unshare --user --map-root-user --net
--mount`:

```
iptables -t nat -A POSTROUTING -o n0 -j MASQUERADE          -> accepted
iptables -A FORWARD -p icmp --icmp-type fragmentation-needed -j DROP
                                                             -> accepted
sysctl -w net.netfilter.nf_conntrack_udp_timeout=30          -> 30
sysctl -w net.netfilter.nf_conntrack_udp_timeout_stream=45   -> 45
ip link set n0 mtu 1400                                      -> accepted
```

The two conntrack timeouts are per-network-namespace, so writing them here
leaves the host's values untouched. Observable failure this predicts: with
`--conntrack-udp-timeout 30`, a UDP flow idle for 30 s stops being translated
and the peer's next datagram is dropped by the router rather than delivered.

**Re-entry from a fresh process needs `--preserve-credentials` (D4).**

```
$ nsenter --user=/proc/$HP/ns/user --net=/proc/$HP/ns/net id
nsenter: setgroups failed: Operation not permitted

$ nsenter --preserve-credentials --user=/proc/$HP/ns/user --net=/proc/$HP/ns/net id
uid=0(root) gid=0(root) groups=0(root),65534(nobody)
```

A user namespace created with `--map-root-user` has `/proc/PID/setgroups` set
to `deny` — that is what makes an unprivileged `gid_map` write legal in the
first place — so `nsenter`'s default post-entry `setgroups()` can never
succeed against it. Observable failure this predicts: omitting the flag makes
every `exec`, `shape` and `show` fail with `setgroups failed`, and no
namespace operation runs at all.

## Shape

### Topology

```
      srv ns                    rtr ns                    cli ns
 ┌──────────────┐         ┌──────────────┐         ┌──────────────┐
 │              │         │              │         │              │
 │  10.90.0.2   │◄─veth──►│  10.90.0.1   │         │              │
 │              │         │              │         │              │
 │harness-server│         │  MASQUERADE  │         │ agent-runner │
 │   (listens   │         │  cli -> srv  │         │   (dials)    │
 │  ws AND udp) │         │              │         │              │
 │              │         │  10.91.0.1   │◄─veth──►│  10.91.0.2   │
 └──────────────┘         └──────────────┘         └──────────────┘
                          netem + htb on
                          BOTH rtr-side
                          veth devices
```

All three network namespaces are owned by **one** user namespace, created by
`up`. Each is held by a detached placeholder process whose PID is recorded in
the state file; that PID is how every later invocation addresses the namespace.

Shaping is applied on the **`rtr` side of each veth**, in the egress direction
toward each endpoint, so both directions of the path are shaped and the
configured delay is a one-way delay on each leg. A `--delay 75ms` profile
therefore yields a ~150 ms round trip, and the README says so next to the
table, because a reader who assumes `delay` is the RTT will halve every result.

### Verbs

```
netem-lab.py up    [--name N] [--profile P]
                   [--delay D] [--jitter J] [--loss PCT] [--rate R]
                   [--limit PKTS] [--mtu N] [--reorder PCT]
                   [--agent claude|fake] [--transport udp|ws] [--no-nat]
                   [--conntrack-udp-timeout SEC] [--pmtu-blackhole]
                   [--subnet-a CIDR] [--subnet-b CIDR]
                   [-- <extra agent-runner flags>]
netem-lab.py env   [--name N]
netem-lab.py exec  [--name N] {srv|rtr|cli} -- <cmd...>
netem-lab.py shape [--name N] [--profile P] [same knobs as up]
netem-lab.py show  [--name N]
netem-lab.py down  [--name N]
```

`--name` lets independent labs coexist, for the same reason
`dummy-harness.py` has one.

`env` prints `export` lines — `HARNESS_PSK`, `CID`, `REPO`, plus
`NETEM_LAB_NAME` — and the same `unset` line `dummy-harness.py` emits, because
a shell inside a harness task still carries `HARNESS_AUTH_TICKET` for the live
server and `harness-cli` prefers a ticket over the PSK.

`harness-cli` runs from the **host** namespace and cannot reach the lab's
addresses; it must be run through `exec cli` (or `exec srv`). The README leads
with that, since the failure is a timeout rather than a message.

### Profiles

A profile is a named set of knob values and nothing else; any knob given
explicitly overrides the profile's value for that knob.

| profile | delay (one-way) | rate | loss | limit |
|---|---|---|---|---|
| `lan` | 1ms | — | 0 | 10000 |
| `wan-jp` | 5ms | — | 0 | 10000 |
| `wan-us` | 75ms | — | 0 | 10000 |
| `wan-eu` | 115ms | — | 0 | 10000 |
| `bufferbloat` | 75ms | 20mbit | 0 | 2000 |
| `thin` | 75ms | 2mbit | 0 | 100 |
| `lossy` | 75ms | — | 1% | 10000 |

When `rate` is set, the qdisc chain is `htb` root → `netem` leaf, and `limit`
is that leaf's queue depth in packets. When `rate` is unset there is no
bottleneck and `limit` only guards against `netem`'s own default.

**`netem`'s default `limit` is 1000 packets.** At 115 ms and 100 Mbit/s the
in-flight count exceeds that, so the default silently drops packets that the
operator did not ask for and that read as link loss. Every profile therefore
sets `limit` explicitly, and the default when no profile is named is 10000.

### Middlebox behaviour (P3)

The three knobs that answer P3 all act in `rtr`, and none of them is part of a
profile — each models a specific broken path and is switched on deliberately.

- **`--conntrack-udp-timeout SEC`** writes
  `net.netfilter.nf_conntrack_udp_timeout` (and `..._udp_timeout_stream`) in
  `rtr`'s network namespace. Both are per-namespace, so this changes nothing
  on the host. Setting it *below* the transport's keepalive interval is how a
  NAT mapping is made to expire under a live connection — the failure that on
  a real path takes minutes of idling to provoke and here takes seconds.

- **`--mtu N`** sets the MTU on both `rtr`-side veth devices. On its own this
  is a well-behaved small-MTU path: the router still emits ICMP
  `fragmentation-needed`, so path-MTU discovery works and the sender learns
  the limit.

- **`--pmtu-blackhole`** adds, in `rtr`,
  `iptables -A FORWARD -p icmp --icmp-type fragmentation-needed -j DROP`.
  Combined with `--mtu`, this is the black hole: oversized packets vanish and
  nothing reports why. A transport that relies on receiving the ICMP will
  stall rather than adapt, and that stall is the observable the flag exists to
  produce.

`--no-nat` removes the masquerade entirely, which is the control case: if a
symptom survives `--no-nat`, the NAT is not what caused it.

### State

`$TMPDIR/harness-netem-$USER/<name>.json`, holding the user-namespace holder
PID, the three namespace PIDs, both subnets, the resolved knob values, the
PSK, the CID, the temp dir, the repo path, and the server and runner PIDs.

`up` creates a throwaway git repo in the temp dir — `git init` plus one empty
commit — and passes it as the runner's `--roots`, the same way
`dummy-harness.py` does. The runner refuses a `--roots` entry that is not a
repo, and the lab is about the path between the two processes, not about any
particular working tree.

The state directory is resolved with `dummy-harness.py`'s own `tmp_root()`,
which reads `TMPDIR` on POSIX and `TEMP` on Windows — deliberately **not**
`tempfile.gettempdir()`. That function consults `TMP` first, and
`dummy-harness.py`'s `env` exports `TMP` set to the *instance's* directory
(`cmd_env`, still today). One `eval "$(dummy-harness.py env)"` therefore made
every later call in that shell resolve its state directory inside the
instance; `down` reported nothing to stop and left a server and a runner
running with their state file orphaned. `tmp_root()` is the fix, and reusing
it rather than reaching for `gettempdir()` is why this tool inherits it
instead of writing its own.

This tool's `env` additionally does not export `TMP` at all, so the two tools'
`env` output can be sourced into the same shell without either one steering
the other's state lookup.

`down` kills **by recorded PID only, never by name**. The lab runs the same
binary names as the real fleet, and a name-matched kill on a developer machine
has already taken production runners with it once (recorded in
`scripts/dummy-harness.py`'s `kill_pid`).

`down` kills **every** recorded PID — server, runner, the three namespace
holders, then the user-namespace holder. Killing the user-namespace holder
does **not** cascade: the namespace holders are separate processes, not its
children in any sense the kernel acts on, and a namespace survives as long as
any process remains in it. What makes the teardown complete is that the lab
never creates a bind mount for a namespace — it uses PID-held namespaces
rather than `ip netns add`, which would leave an entry under `/run/netns` that
outlives every process. Observable failure this predicts: after `down`,
`lsns -t net` lists no namespace belonging to the lab, and re-running `up`
with the same `--name` succeeds rather than reporting the addresses in use.

### Preflight

Each check that fails exits **2** (`setup_err`), not 1, matching
`dummy-harness.py`'s split between "the environment is not ready" and "the
thing under test failed":

1. `platform.system() != "Linux"` → refuse, naming the platform (D8).
2. `user.max_user_namespaces` is 0 or unreadable → refuse, naming the sysctl.
3. `ip`, `tc`, `iptables`, `unshare`, `nsenter` missing from `PATH` → refuse,
   naming which.
4. A throwaway `unshare --user --map-root-user --net` that creates a veth and
   attaches `netem` to it → refuse if it fails, printing the underlying error
   and suggesting `sudo modprobe sch_netem sch_htb` for the kernel where the
   autoload does not work.

Check 4 exists because check 2 passing does not imply check 4 passing, and the
difference between them is a kernel policy this design measured on exactly one
machine.

### Calibration

`up` finishes by running `ping -c3` from `cli` to the `srv` address inside the
lab and printing the measured RTT next to the configured one. A lab whose
measured RTT does not match its profile is not a lab, and the cheapest moment
to notice is before any measurement is taken.

`show` prints `tc -s qdisc show` for both shaped devices, the conntrack entry
count in `rtr`, and the addresses. The `tc` counters carry `sent`, `dropped`,
`overlimits` and `backlog` per qdisc, which is what separates "the bottleneck
queue overflowed" from "`netem` dropped it" — the distinction P2 is about.

## Testing

`scripts/netem-lab/test_netem_lab.py`, `unittest`, stdlib only, run directly
(D11). It covers the parts whose failures are silent and which need no
namespaces:

- **Profile expansion.** Each profile name expands to the expected `tc` argv,
  and an explicit knob overrides the profile's value for that knob only.
- **`limit` is always explicit.** A test asserts no generated `netem` argv
  omits `limit`, because the failure mode of omitting it is invisible loss
  rather than an error.
- **State round-trip.** The state file written by `up` is read back by `down`
  with every PID intact, and a missing state file makes `down` a no-op rather
  than an error.
- **`nsenter` argv always carries `--preserve-credentials`.** Asserted on the
  generated argv, because its absence fails at runtime with an error that
  names `setgroups` and not the missing flag.
- **The middlebox knobs are absent from every profile.** Asserted over the
  profile table: none of them sets `conntrack_udp_timeout` or
  `pmtu_blackhole`. A profile that quietly carried one would make a stall
  produced by a broken middlebox look like a congestion-control result, which
  is the one confusion this lab exists to prevent.

Standing a lab up is a manual check by construction — that is what the tool is
for — and the README documents the sequence: `up`, confirm the ping RTT
matches, `exec cli -- harness-cli ... file push` a large file, read `show`.

## Surfaces

This adds no operator-visible harness feature: no CLI verb, no TUI binding, no
WebUI control, no wire change, no capability. The `surface-parity-checklist`
walk is therefore not applicable, and this paragraph is the explicit statement
of that rather than an omission.

The documentation surfaces that *do* change:

- `scripts/netem-lab/README.md` — the body of the documentation, in the role
  `scripts/sandbox/README.md` plays for that kit.
- The root `README.md` directory listing, which already has a line for
  `scripts/sandbox/`, gains one for `scripts/netem-lab/`.

## Non-goals

- **Windows and macOS.** D8. Refused at preflight with the platform named.
- **Touching the host's network namespace.** The lab has no route to it and no
  code path that creates one. The real fleet's traffic cannot be shaped by
  this tool even by mistake.
- **Replacing a real remote host.** Four things stay out of reach of any local
  emulation: a cloud load balancer's UDP flow hashing and idle timeout, an
  ISP's policer and its treatment of UDP, carrier-grade NAT, and route changes.
  A single confirmation run against a real remote host remains the last step;
  this lab exists so that step is a confirmation rather than the investigation.
- **Competing cross-traffic with its own congestion control.** Fairness
  between two controllers is a separate question and needs a second flow the
  lab does not generate. `exec cli -- iperf3 ...` is available for anyone who
  wants to hand-build one.
- **Trace-driven replay.** `mahimahi` records and replays real link traces and
  is the right tool if synthetic profiles turn out to be too clean. This lab
  does not attempt it.
