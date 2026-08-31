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
netem-lab.py [--name N] show
netem-lab.py [--name N] down
```

`--name` lets independent labs coexist. `up` also takes `--agent claude|fake`
(default `fake`), `--transport udp|ws` (default `udp`), and
`--subnet-a` / `--subnet-b`.

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
  exception: `shape` **replaces** the qdisc, which resets every counter, so a
  fresh zero right after a reshape means nothing has crossed *since then* — not
  that nothing is crossing.

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
state round-trip, and that no profile carries a middlebox knob). Standing a lab
up is a manual check by construction.
