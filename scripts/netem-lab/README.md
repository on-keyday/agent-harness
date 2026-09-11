# netem-lab — a shaped WAN path for the transport, on one machine

Puts a delayed, lossy, rate-limited, small-MTU, NATed network path between
`harness-server` and `agent-runner`, so the transport stack (`objproto` +
`trsf`) can be exercised against conditions it has never met. Every throughput
figure this project has was taken over loopback or a local link.

**No root.** Everything runs inside a user namespace created by `unshare --user
--map-root-user`, which carries `CAP_NET_ADMIN` over the network namespaces it
owns. **Linux only** — `unshare`, network namespaces, `tc` and `iptables` have
no equivalent elsewhere, and the preflight refuses rather than half-building.

Design and the measurements behind it:
[`docs/superpowers/specs/2026-08-31-netem-lab-design.md`](../../docs/superpowers/specs/2026-08-31-netem-lab-design.md)

---

## Three things to know before you read anything else

**1. `harness-cli` run from your shell cannot reach the lab.** The host network
namespace has no route to `10.90.0.2`, so a call from outside **times out
without saying anything**. Every command goes through `exec`:

```bash
scripts/netem-lab/netem-lab.py --name t1 exec cli -- \
  harness-cli --server-cid "$CID" ls
```

`exec` injects that instance's PSK itself, so this works whether or not you ran
`eval "$(netem-lab.py env)"` first.

**2. `--delay` is ONE-WAY.** Each direction is shaped separately, so
`--profile wan-us` (75 ms) is a **~150 ms round trip**. Reading it as the RTT
halves every result you take. The `up` output states both, measured and
configured, side by side.

**3. `--loss` is not congestion.** It models link corruption: netem's loss is
an independent process, unrelated to how fast you are sending. **Congestion**
loss comes from `--rate` plus `--limit` — a bottleneck whose queue you fill
yourself. A congestion controller measured only against `--loss` is measured
against the wrong thing, which is the single reason this tool is shaped the way
it is.

---

## Topology

```
      srv ns                    rtr ns                    cli ns
 ┌──────────────┐         ┌──────────────┐         ┌──────────────┐
 │  10.90.0.2   │◄─veth──►│  10.90.0.1   │         │              │
 │              │         │  MASQUERADE  │         │              │
 │harness-server│         │  cli -> srv  │         │ agent-runner │
 │              │         │  10.91.0.1   │◄─veth──►│  10.91.0.2   │
 └──────────────┘         └──────────────┘         └──────────────┘
                          netem + htb on BOTH
                          rtr-side veth devices
```

The runner sits behind the NAT because that is the direction the real
deployment has: the runner dials the server. You can see it working — the
runner registers from `10.90.0.1` although it lives at `10.91.0.2`.

Shaping is on the **router side** of each veth, egress direction. A packet
cli→srv is delayed once on `v-rtr-s`; its reply once on `v-rtr-c`. Hence
one-way delay, doubled round trip.

## Verbs

```
netem-lab.py [--name N] up    [--profile P] [knobs...] [-- <extra runner flags>]
netem-lab.py [--name N] env
netem-lab.py [--name N] exec  {srv|rtr|cli} -- <cmd...>
netem-lab.py [--name N] shape [--profile P] [knobs...]
netem-lab.py [--name N] path  {1|2} {up|down}
netem-lab.py [--name N] failover [--leg N] [--timeout S]
netem-lab.py [--name N] show
netem-lab.py [--name N] down
```

`--name` lets independent labs coexist. `up` also takes `--agent claude|fake`
(default `fake`), `--transport udp|ws` (default `udp`),
`--subnet-a` / `--subnet-b`, and `--paths 1|2` with `--subnet-c`
(see **Two paths** below; `--paths 1` is the default and leaves the topology
exactly as it was).

Words after `--` go to the **runner**. `--server-arg` is the server's
equivalent, and it is repeatable. Use the `=` form for anything starting with a
dash, or argparse reads the value as a flag of its own:

```bash
scripts/netem-lab/netem-lab.py --name p up --profile lan \
  --server-arg=--pprof-listen --server-arg=10.90.0.2:6060
# then, from inside the lab:
#   ... exec srv -- curl -o /tmp/cpu.pprof \
#         'http://10.90.0.2:6060/debug/pprof/profile?seconds=20'
#   go tool pprof -top bin/harness-server /tmp/cpu.pprof
```

`harness-server --pprof-listen` also turns the **block and mutex** profilers on.
Read the block profile with care: idle goroutines parked on a channel dominate
it by wall-clock, so a periodic sweeper that spends its life asleep outranks the
thing you are hunting. `kill -USR1 <server-pid>` is often more direct — it dumps
each connection's trsf state (cwnd, srtt, bytes in flight) to the server log.

**`--transport udp` is the default on purpose.** Over WebSocket the kernel's
TCP does the congestion control and the code under test is mostly bypassed;
the UDP leg is where `trsf`'s own controller runs.

## Profiles

`--delay` values are **one-way**. Double them for the round trip.

| profile | delay (one-way) | rate | loss | limit |
|---|---|---|---|---|
| `lan` | 1 ms | — | 0 | 10000 |
| `wan-jp` | 5 ms | — | 0 | 10000 |
| `wan-us` | 75 ms | — | 0 | 10000 |
| `wan-eu` | 115 ms | — | 0 | 10000 |
| `bufferbloat` | 75 ms | 20 mbit | 0 | 2000 |
| `thin` | 75 ms | 2 mbit | 0 | 100 |
| `lossy` | 75 ms | — | 1 % | 10000 |

Any knob given explicitly overrides the profile's value for that knob only:
`--profile bufferbloat --delay 10` keeps the 20 mbit rate and the 2000-packet
queue.

Knobs: `--delay`, `--jitter`, `--loss`, `--reorder`, `--rate`, `--limit`.

**`limit` is the queue depth in packets, and it is always emitted.** netem's
own default is 1000, which at 115 ms and 100 Mbit/s is below the in-flight
count — leaving it unset drops packets nobody asked for, and they read as loss
on the link.

## The middlebox knobs

No profile sets any of these. Each produces a stall that would otherwise read
as a congestion-control result, so it must be switched on deliberately.

| knob | what it models |
|---|---|
| `--mtu N` | a narrow link **beyond** the router (applied to the srv leg only, both ends). PMTU discovery still works: the router answers `frag needed, mtu = N`. |
| `--pmtu-blackhole` | that router also **drops the ICMP**. Oversized packets vanish and nothing reports why. |
| `--conntrack-udp-timeout SEC` | a NAT whose mapping expires after `SEC`. Set it below the keepalive interval to make a mapping die under a live connection. Per-namespace: the host's value is untouched. |
| `--no-nat` | the control case. If a symptom survives `--no-nat`, the NAT did not cause it. |

Verifying the black hole is real needs a packet size **between** the two MTUs,
or you only test cli's own link:

```bash
# --mtu 1400 without --pmtu-blackhole:
$ ... exec cli -- ping -c2 -M do -s 1440 10.90.0.2
From 10.91.0.1 icmp_seq=1 Frag needed and DF set (mtu = 1400)

# --mtu 1400 --pmtu-blackhole:
$ ... exec cli -- ping -c2 -M do -s 1440 10.90.0.2
--- 10.90.0.2 ping statistics ---
2 packets transmitted, 0 received, 100% packet loss
```

`-s 1500` tells you nothing: 1500 + 28 exceeds cli's own 1500-byte link, so the
kernel refuses locally and the router is never consulted.

## A measurement, start to finish

```bash
# 1. Bring it up. Confirm the measured RTT matches the configured one before
#    trusting anything downstream.
scripts/netem-lab/netem-lab.py --name t1 up --profile wan-us
#   netem-lab: configured RTT 150ms (2 x 75ms one-way)
#   netem-lab: measured   rtt min/avg/max/mdev = 150.113/150.142/150.166/... ms

# 2. Drive it.
eval "$(scripts/netem-lab/netem-lab.py --name t1 env)"
scripts/netem-lab/netem-lab.py --name t1 exec cli -- \
  harness-cli --server-cid "$CID" submit --repo "$REPO" --task "hello"

# 3. Change ONE parameter under the running connection.
scripts/netem-lab/netem-lab.py --name t1 shape --profile bufferbloat

# 4. Read the counters, then stop.
scripts/netem-lab/netem-lab.py --name t1 show
scripts/netem-lab/netem-lab.py --name t1 down
```

Step 1 is not optional. A lab whose measured RTT does not match its profile is
not a lab, and the cheapest moment to find out is before you have a number you
believe.

## `bench` — and why a single number is not a result

```bash
scripts/netem-lab/netem-lab.py --name t1 bench --runs 6 --size-mb 100
```

Pushes the same file N times against a throwaway sink task it creates and
cancels itself, discards a warm-up run, pins server / runner / client to
separate cores (`--no-pin` to skip), and reports the **spread** alongside the
middle:

```
  n=6  median=10.07  mean=10.85  stdev=2.67 (27%)  min=7.56  max=14.47  spread=1.91x
  RESOLUTION: this run can distinguish differences larger than about 31%.
```

**The resolution line is the point of the verb.** This workload measured
6.76–12.70 MB/s across six runs of one unchanged build. A lone number therefore
says nothing about any change smaller than roughly 2×, and this project has
twice mistaken one for a finding: an `O(n²)` allocation removed from the send
path and a relay chunk size raised 64× both produced "improvements" that were
entirely inside the noise. The one real result of that investigation — a UDP
receive buffer left at 208 KB — was a 5.5× change, which is why a single run
could see it.

So: **quote the median of a `bench` run, never a single push, and do not claim
a change smaller than the resolution figure did anything.**

Two things measured about the noise itself, so nobody re-derives them:

- **Pinning does not help.** stdev 27% pinned vs 23% unpinned. CPU contention
  between the three processes is not the source.
- **More runs help slowly.** n=6 → 31%, n=16 → 26%: the distribution has a fat
  tail, so a larger sample raises the observed stdev and partly cancels the
  `1/√n` gain. Reaching ~10% would need on the order of 60 runs. Reducing the
  underlying variance is the better lever.
- **~~The variance is a function of the delay.~~ SOLVED — it was a bug, not a
  property of the lab.** A retransmit returned from the top of
  `sendStream.triggerPacket` and skipped the re-queue at the bottom, so the
  stream fell off the send trigger with its buffer full and nothing sent again
  until the next ACK — one round trip. How many retransmits happened to land
  inside a given transfer set that transfer's rate, which is exactly what a
  4.44x spread looks like. objtrsf `5c3a630`:

  | `--delay` | before | after |
  | --- | --- | --- |
  | 1 ms | median 9.60 MB/s, stdev **70%**, spread 4.44x | median **44.86**, stdev 9%, spread 1.26x |
  | 25 ms | median 7.49 MB/s, stdev 16% | median **8.75**, stdev **3%**, spread 1.09x |

  Those two 25 ms figures come from labs built on different days, and read as
  "+17%". A **paired** A/B — the same 64 MB push six times per build, each on a
  freshly created lab, nothing differing but the binary — puts it at 8.62 vs
  **11.42 MB/s**, +32%, with four of six post-fix runs beating every pre-fix
  run. **Build a fresh lab per arm and interleave; do not compare a number to
  one you took yesterday.**

  So the numbers this file quotes above (6.76–12.70 MB/s across six runs, and
  the ±31–81% resolution) were all taken against that bug. **Re-measure before
  comparing anything to them.** The resolution figures a `bench` run prints are
  still the right way to read a result; they are simply much tighter now.

## Two paths, and `failover`

`agent-runner --server-cid` takes a comma-separated ordered list of server
addresses for a runner on a machine that moves. Whether it actually fails over
cannot be tested on loopback: it needs two paths that can be taken away
independently.

```bash
scripts/netem-lab/netem-lab.py --name fo up --paths 2 --transport ws
#   netem-lab: up  name=fo  srv=10.90.0.2  cli=10.91.0.2  srv2=10.92.0.2 (leg 2, UNSHAPED)

scripts/netem-lab/netem-lab.py --name fo failover
#   netem-lab: runner 8faf554e on conn ws:10.90.0.1:51712-31023
#   netem-lab: taking leg 1 down, waiting up to 150s for it to move
#   netem-lab: moved to ws:10.91.0.2:34110-2275 in 63s
#   netem-lab: RunnerID survived (8faf554e)
```

`--paths 2` gives `srv` a **second address on a second veth to the router**, so
it is one server reachable two ways, and the runner is started with both as
candidates. `path 1 down` takes a leg away; `failover` does that and measures
the runner arriving on the other one, then restores the leg. The runner's list
is `RUNNER_CID` in `env`; `CID` stays a single address, because `harness-cli`
takes one and only the runner takes candidates.

Two assertions, and the second is the one the feature was designed around: the
runner comes back, and the server keeps it as the **same RunnerID** —
`Registry.Add`'s takeover of an identity arriving on a new connection, rather
than a second row for one process.

**The wait is ~65 s, and it is not the ping interval.** Measured 63 s in the
lab and ~70 s for both failure shapes in a two-veth probe: the runner's own link
withdrawn (route gone) and the path blackholed with the route intact (`DROP`).
`--ping-interval 2s` does not move it. Neither failure makes a **send** fail —
an established TCP connection whose route disappears keeps accepting writes into
the socket buffer — so `objproto`'s `CannotSend` never fires and what finally
notices is `AutoGarbageCollect` deleting an inactive connection
(`connectionTimeout = 1 min` in `runner.Connect`). The same asymmetry is on
record for a dead UDP peer (~68 s). Budget a **minute of no dispatch** after a
laptop changes networks; a faster failover means shortening that GC window, not
the ping.

Two properties of leg 2 to keep in mind, both deliberate:

- **Unshaped.** `apply_shaping` only touches the leg-1 devices, so the second
  path carries no delay, loss or MTU narrowing. It tests reachability, not two
  shaped paths.
- **Un-NAT'd.** The `MASQUERADE` rule names leg 1's device, so a runner that has
  failed over appears at the **client's** own address (`10.91.0.2`) rather than
  the router's. That is why `failover` keys on the connection id changing rather
  than on which address it holds.

## Reading `show`

`show` prints `tc -s qdisc show` for both shaped devices. The counters are how
you tell the two kinds of loss apart:

```
qdisc netem 800d: root refcnt 5 limit 10000 delay 75ms
 Sent 17360 bytes 68 pkt (dropped 0, overlimits 0 requeues 0)
 backlog 0b 0p requeues 0
```

- **`dropped` rising with a `--rate` set** — the bottleneck queue overflowed.
  That loss was **caused by the sender**, which is the congestion signal worth
  measuring against.
- **`dropped` rising with no `--rate`** — netem's `--loss` did it. Independent
  of your sending rate; do not read a congestion-control conclusion from it.
- **`backlog` large and steady** — bufferbloat. The queue is full and staying
  full, so every packet is paying the full queueing delay.
- **`Sent` at zero** — the traffic is not crossing this device at all. Check
  that you ran the command through `exec`, not from your own shell. One
  exception: a `shape` that changes the qdisc KIND installs a new one, so a
  fresh zero right after such a reshape means nothing has crossed *since then* —
  not that nothing is crossing.

**A `shape` does NOT reliably reset the counters, and reading `show` as though
it did will invent a result.** The commands are `tc qdisc replace`, and
`replace` on a matching handle and kind updates in place and **keeps** the
statistics. Measured: `lan` → `lan` left the handle at `809c` and the counters
running (11698 → 11740 bytes across the reshape); `lan` → `bufferbloat` swapped
netem for htb+netem and zeroed them. So a reshape between two `--delay` values
carries every byte of the previous run forward, and treating the total as one
transfer's is how a 1.10x wire overhead was once read as 11x amplification.

**Take a delta, never a total.** `show` before and after the thing you are
measuring, and subtract:

```bash
scripts/netem-lab/netem-lab.py --name t1 show | grep Sent   # before
... the transfer ...
scripts/netem-lab/netem-lab.py --name t1 show | grep Sent   # after
```

`show` also prints the conntrack entry count and the NAT rule, which is where
to look when a UDP flow stops being translated.

## Troubleshooting

| symptom | cause |
|---|---|
| a `harness-cli` call hangs forever | run from the host namespace. Use `exec cli`. |
| `SETUP: user.max_user_namespaces is 0` | unprivileged user namespaces are disabled on this host. |
| `SETUP: a user namespace could not create a veth with netem attached` | this kernel does not autoload qdisc modules from a user namespace. `sudo modprobe sch_netem sch_htb` once, permanently. |
| `setgroups failed: Operation not permitted` | an `nsenter` built without `--preserve-credentials`. A `--map-root-user` namespace has `setgroups` permanently denied. |
| loss you did not configure, at high delay | a `netem` argv missing `limit`. Every one this tool emits carries it; a hand-typed `tc` command will not. |

## What this cannot reach

Four things stay out of reach of any local emulation: a cloud load balancer's
UDP flow hashing and idle timeout, an ISP policer and its treatment of UDP,
carrier-grade NAT, and route changes. One confirmation run against a real
remote host is still the last step — this lab exists so that step is a
confirmation rather than the investigation.

Also out of scope: competing cross-traffic with its own congestion control
(`exec cli -- iperf3 ...` if you want to hand-build one), and trace-driven
replay — [`mahimahi`](https://github.com/ravinet/mahimahi) records and replays
real link traces and is the right tool if these synthetic profiles turn out to
be too clean.

## Tests

```bash
python3 scripts/netem-lab/test_netem_lab.py
```

`unittest`, stdlib only, run directly. **No make target runs it** — `make test`
is `go test ./...`. It covers the parts whose failures are silent (profile
expansion, `limit` always present, `--preserve-credentials` always present, the
state round-trip, that no profile carries a middlebox knob, which devices each
server leg names, and the `ls` row shape `failover` measures against — that last
one so a changed row format breaks loudly instead of reporting "never moved").
Standing a lab up is a manual check by construction, and so is `failover`: it
needs namespaces and a live harness.
