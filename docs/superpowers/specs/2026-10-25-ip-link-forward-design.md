# IP link forwarding — a tun device at each end of a forward — Design

Date: 2026-10-25

Stage 1 of a layer-3 forward: the client and the runner each hold a tun
device, and every IP packet one of them reads is carried, unchanged, to the
other. It rides the datagram machinery the udp forward built
(`docs/superpowers/specs/2026-09-17-trsf-datagram-udp-forward-design.md`, read
it first — this document assumes its §4c, §6c, §6d and its three amendments).

Two existing gaps are closed on the way because a link would inherit them:
`forward tap` shows nothing on a datagram forward (§10c); the runner keeps
datagram-forward state after the forward or its connection ends (§6e, §7b);
and a forward whose runner-side state is gone stays listed and silent (§6f).

---

## 1. Intent

There is no workload asking for this. The reason to build it is that it is a
satisfying thing to have built: an IP packet written into a tun device on one
side of the harness comes out of a tun device on the other, and `ping` answers
across it (operator, 2026-10-25).

That shapes how success is judged. The first milestone is **`ping` answering
between two tun devices inside `scripts/netem-lab`**, then `iperf3` across the
same link so the datagram path's throughput is visible at layer 3. A real
client host (the operator's client is Windows) and a real runner come later.

The udp forward spec left the door open on purpose: its §2 says L3 "is wanted
later" and "nothing here claims L3 was evaluated and rejected", and its §6d
says an L3 mode is "`flow_id` names a tun device, `payload` is an IP packet"
with no change below the harness.

## 2. Two modes, two stages

| | stage 1: transparent | stage 2: router |
|---|---|---|
| what the harness is | a link between two interfaces | a hop between them |
| TTL | untouched | decremented; Time Exceeded at zero |
| oversized packet | does not arise while every leg is at its initial MTU or above (§7c); counted if it does | Packet Too Big back into the sending tun |
| `traceroute` | the far end is one hop away | the harness answers as a hop |
| reads the IP header | never | yes |

Both are wanted (operator, 2026-10-25). Stage 1 is this document. Stage 2 is
named here so stage 1 does not close it off; its design is not here, and §11
lists only what stage 1 leaves for it.

## 3. Non-goals

- **Routing beyond the two interfaces.** Stage 1 is point-to-point: each end
  reaches the other end's tun address and nothing behind it. No NAT, no routes
  pushed to either host, no `ip_forward`. An operator who adds routes by hand
  gets whatever the kernels do with them; the harness neither helps nor checks.
- **Any OS but Linux.** The client and the runner both refuse an ip link on
  any other OS (§9). Windows (wintun) is a later stage, because it is where
  most of the cost is and stage 1 should be able to fail for reasons that are
  not Windows'.
- **IPv6 addresses on the link.** The schema carries 16-byte addresses (§5)
  so IPv6 needs no wire change; stage 1 refuses them (§9). The pump does not
  read packets, so whatever either kernel writes into its tun crosses, IPv6
  included.
- **A pre-created tun device.** The harness creates both devices itself, which
  needs `CAP_NET_ADMIN` (§8). Attaching to a persistent device created earlier
  by root — the route to running without the capability — is not built.
- **A changing MTU.** Fixed at 1100 (§7c).
- **Starting a link from the WebUI** (§10a).

## 4. Decisions taken

`Decided-by` is provenance. Rows marked `author` were proposed in a playback
the operator accepted without objection on 2026-10-25; that is weaker than a
choice the operator made, and a later reader may overturn them on evidence.

| # | Decision | Decided-by |
|---|---|---|
| 1 | Build L3 forwarding; the motivation is building it | operator |
| 2 | Both transparent and router modes, transparent first | operator |
| 3 | Stage 1 is done when it works in `scripts/netem-lab` | operator |
| 4 | Point-to-point only; nothing behind either tun is reachable through the harness | author |
| 5 | MTU fixed at 1100 | author |
| 6 | Addresses are given by the operator when the link is created | author |
| 7 | The CLI and the TUI create a link; the WebUI cannot (a browser opens no tun); every surface lists and kills one | operator |
| 8 | `ForwardProtocol.ip` plus a new `PortForwardDirection.link` | author |
| 9 | A link needs a new capability bit, `forward_link`, and only it; later stages reuse the same bit | operator |
| 10 | Registration waits for the runner's tun, as `-R` waits for its bind | author |
| 11 | tun devices are created with `golang.org/x/sys/unix` ioctls; no new module | author |
| 12 | IPv4 only in stage 1; the schema carries either family | author |
| 13 | Server teardown tells the runner whenever the runner holds state, not only for `remote` | author |
| 14 | Taps read datagram forwards — udp as well as ip — instead of being refused on them | operator |
| 15 | A link whose runner end dies after setup is torn down and the client told, not left up and silent | operator |

## 5. Wire changes — all of them

All in `runner/protocol/message.bgn`. No `appwire` change and no `objtrsf`
change: the datagram frame and `AppKind_ForwardDatagram` carry this unchanged.

```
enum ForwardProtocol:
    :u8
    tcp = "tcp"
    udp = "udp"
    ip  = "ip"        # appended: payload is one whole IP packet

enum PortForwardDirection:
    :u8
    local
    remote
    link              # appended: NEITHER end listens or dials; each holds an
                      # interface, and a packet read from one is written to the
                      # other. Legal only with protocol == ip, and ip is legal
                      # only with link (§6a).

# IpLinkConfig is everything both ends need to bring their interface up.
# One address family per link: addr_len is 4 or 16 and applies to both
# addresses, so a link cannot mix families by construction.
format IpLinkConfig:
    addr_len    :u8
    client_addr :[addr_len]u8
    runner_addr :[addr_len]u8
    prefix_len  :u8          # shared: both addresses are on one subnet
    mtu         :u16         # both tun devices are set to this
```

Appended, each behind the same condition:

```
format RegisterPortForwardRequest:      # after route
    ...
    if protocol == ForwardProtocol.ip:
        link :IpLinkConfig

format RunnerOpenPortForwardRequest:    # after protocol
    ...
    if protocol == ForwardProtocol.ip:
        link :IpLinkConfig

format PortForwardInfo:                 # after counters
    ...
    if protocol == ForwardProtocol.ip:
        link :IpLinkConfig
```

On an ip registration the existing address fields are empty and the ports are
0: `bind_addr`, `bind_port`, `target_host` and `target_port` describe a
listener and a dial target, and a link has neither. The schema comment on
`RegisterPortForwardRequest` (`message.bgn`, the `direction decides what the
address pair means` block) gains a `link` line saying so.

`ForwardDatagram` is unchanged. On an ip forward `flow_id` is always 0: one
registration is one pair of tun devices, so there is nothing to multiplex. A
receiver ignores `flow_id` on an ip forward rather than rejecting non-zero, so
stage 2 can give it a meaning without a skew.

The runner's setup result is renamed and given a reason, because it is no
longer only about binding a `-R` listener and because "it failed" is not
enough for an operator who cannot read the runner's log:

```
# Renamed from RemoteForwardBindResult; RunnerMessageType member renamed from
# remote_forward_bind_result to forward_setup_result, ordinal unchanged.
format ForwardSetupResult:
    forward_id :u64
    ok         :u1
    reserved   :u7
    detail_len :u16          # appended: why, when ok == 0 (the error text);
    detail     :[detail_len]u8   # empty when ok == 1

enum OpenPortForwardStatus:
    ...
    link_failed = "link_failed"   # appended: an end could not bring up its
                                  # interface; detail says which end and why

format RegisterPortForwardResponse:     # appended after stream_id
    ...
    detail_len :u16
    detail     :[detail_len]u8
```

`-R` fills `detail` too, with its `net.Listen` error — today a bind failure
reaches the client as a bare `bind_failed`.

A forward's runner end can now end on its own, and the client is told why
(§6f):

```
enum RunnerMessageType:
    ...
    forward_ended                 # appended

# ForwardEnded (runner -> server): the runner's end of a forward stopped
# working after setup succeeded -- a tun read or write failed. The server
# tears the forward down and tells the client.
format ForwardEnded:
    forward_id :u64
    detail_len :u16
    detail     :[detail_len]u8

enum PortForwardCloseReason:
    ...
    runner_gone      # appended: the runner connection that held this
                     # forward's runner-side state ended or was replaced
    endpoint_failed  # appended: the runner reported its end failed;
                     # detail says how

format PortForwardClosed:
    reason     :PortForwardCloseReason
    detail_len :u16                 # appended: empty except endpoint_failed
    detail     :[detail_len]u8
```

The new capability bit is in §6b.

Taps learn to read datagrams (§10c):

```
enum ForwardTapRecordKind:
    ...
    datagram        # appended: one whole datagram, as the relay saw it
    datagram_gap    # appended: the tap could not keep up with one flow

# What the relay did with the datagram after the tap saw it. The relay's
# per-forward drop counters say HOW MANY; this says WHICH ONES.
enum ForwardTapDatagramFate:
    :u8
    relayed
    dropped_oversize
    dropped_congestion
    dropped_queue

# Not ForwardTapData with a flag: that format's stream_offset says record
# boundaries "carry no application meaning", and for a datagram the boundary
# is the only meaning there is.
format ForwardTapDatagram:
    flow_id         :u32     # as the endpoints assigned it; 0 on an ip forward
    direction       :ForwardTapDirection
    seq             :u64     # per (flow_id, direction), counted by the server
                             # from the first datagram it relayed on that flow,
                             # 1-based. A jump is datagrams this tap missed.
    fate            :ForwardTapDatagramFate
    truncated_bytes :u32     # cut by max_record_bytes
    data_len        :u32
    data            :[data_len]u8

format ForwardTapDatagramGap:
    flow_id       :u32
    direction     :ForwardTapDirection
    dropped_bytes :u64       # the count is the jump in seq on the next record

format ForwardTapRecord:
    ...
    match kind:
        ...
        ForwardTapRecordKind.datagram     => datagram     :ForwardTapDatagram
        ForwardTapRecordKind.datagram_gap => datagram_gap :ForwardTapDatagramGap
```

There is no disk axis, for the reason the udp spec gives: forwards live only
in the in-memory `portForwardRegistry`, and `server/wal.go` persists none of
these types.

## 6. Server

### 6a. Shape check

`handleRegisterPortForward` refuses, with `internal_error`, any pairing other
than `ip × link` and `{tcp, udp} × {local, remote}`. This is one function
(`validForwardShape(protocol, direction)`), called once, so the pairing rule
has one home. An ip registration must also carry `route == splice`; the
existing route refusal already does that.

### 6b. Capability

A link needs `forward_link`, a new bit, and nothing else:

```
enum Capability:
    ...
    # forward_link authorizes putting a network INTERFACE at each end of a
    # forward: a tun device on the client's host and one on the runner's,
    # carrying whole IP packets between them. Not implied by forward_local or
    # forward_remote, and does not imply them. A port forward names one target;
    # a link names none, so every service bound to all addresses on either
    # host is reachable from the other, over any IP protocol. Different blast
    # radius, its own bit, default-off, appended — so no existing grant
    # changes meaning. Later stages of the same feature (a routing mode, an
    # L2 device) are the same power and stay on this bit.
    forward_link   = 0x400000, "forward_link"
    all            = 0x7fffff, "all"
```

Requiring `forward_local` and `forward_remote` together was considered and
rejected: every task already holding both would gain links the day this
lands, which is the "existing grant changes meaning" this enum's comments
rule out (operator, 2026-10-25).

`all` is a literal and the WAL persists the number, so a principal whose mask
was persisted as `0x3fffff` does not hold `forward_link` until re-granted —
the same consequence every appended bit has had (`docs/backlog.md`, the board
publishing entry). `cli/verb/caps.go` gains the name and puts it in
`TierStrong`, beside `forward_tap`.

The capability is picked from the direction in two places today — the
`RegisterPortForward` case and the kill gate (`server/task_handler.go:715`),
each with its own `if remote` branch. Both call one function,
`forwardCapFor(direction)`, which maps `link` to `forward_link`.

The bit count: `Capability` is `:u32` and this uses the 23rd bit. At the rate
bits were added in October (five in fifteen days) the type is full within
months whatever this feature does; widening it to `:u64` is a separate change
with a disk axis (the WAL persists masks) and is recorded in
`docs/backlog.md` by this work, not done by it.

### 6c. Registration

A link follows the `-R` path in `registerRemoteForward`, not the udp `-L`
path: create the control stream, add to the registry, `addPending`, send
`RunnerOpenPortForwardRequest{direction: link, protocol: ip, link: ...}`, and
wait up to `remoteForwardBindTimeout` for `ForwardSetupResult`. On failure or
timeout: remove, close the control stream, send `ClosePortForward` (the runner
may have succeeded late), and answer `link_failed` with the runner's `detail`
prefixed `runner: `, or `runner: no answer within <timeout>`.

The udp `-L` path is fire-and-forget, which is acceptable there because a
runner that cannot reach the target fails per flow, visibly. A runner that
cannot create its tun fails every packet forever, and the client's own tun
would be up and silent.

### 6d. Relay

`relayForwardDatagram` (`server/forward_datagram.go:70`) refuses anything whose
`protocolKind != udp`. That becomes a predicate on the enum itself,
`func (p ForwardProtocol) CarriesDatagrams() bool` (udp or ip), in a
hand-written file in `runner/protocol` so the server, the cli renderers and
the runner all reach the same one. Every udp-only gate on datagram behaviour
moves to it:

- the relay check above;
- `cli/port_forward_list.go`'s `PortForwardDatagramCell` (the TUI's datagram
  column), which gates on `Protocol == udp` and would be empty on link rows;
- the server's counter group on a row;
- `server/telemetry.go`'s `fillEndpointDrops`, which asks both endpoints
  whenever the row has `max_datagram_size`. It will ask about link rows, so
  the client's `forwardDrops` and the runner's `dropsFor` must also search
  the link registries, or the per-endpoint keys come back absent.

`protocolSuffix` stays udp-only: a link row's spec string is built separately
(§10b).

Everything else in the relay already holds for ip: it re-emits bytes without
reading them, checks the sender is an endpoint, and counts drops.

### 6e. Teardown — and a gap the udp `-L` already has

`teardownPortForward` (`server/port_forward_list.go:168`) sends
`ClosePortForward` only when `pf.direction == remote`. A link's runner-side tun
is runner state that only that message releases, so the condition becomes "the
runner holds state for this forward": `remote`, or `link`, or `udp`.

The `udp` term fixes something that is already wrong. A udp `-L` registers on
the runner (`sendUDPForwardInstruction`), and the runner's
`udpForwards.close` says it is "called when the server says the forward is
over" — but the server never says so for a `local` forward (the direction
check is `server/port_forward_list.go:184`). The flows' sockets still idle out
after 60 s; the registration entry and its drop counters stay for the life of
the runner PROCESS, because nothing on the runner's side releases them either
(§7b). Found by reading; not reproduced.

### 6f. When the runner's end goes away

Setup waits for the runner (§6c) so that a link never sits with the client's
tun up and nothing behind it. After setup the same state can arise two ways,
and today neither reaches the server:

- **The runner's connection ends or is replaced.** `RunHandle.Close` releases
  the runner-side state (§7b). The server keeps the row, and
  `relayForwardDatagram` resolves the runner by identity
  (`server/forward_datagram.go:78`), so after a reconnect it sends the client's
  packets to the new connection, whose session has no entry for that
  forward and drops them silently, uncounted (`runner/forward_datagram.go:178`).
  Server-side forward cleanup on a connection's end matches the CLIENT's
  connection only (`DropPortForwardsForConn`, matching `pf.clientCID`).
- **The runner's tun fails** while the connection lives.

Both now end the forward:

- `portForward` records `runnerCID`, the runner connection that answered
  setup (or received the udp `-L` instruction), for every forward the runner
  holds state for — the same predicate as §6e. One function,
  `DropRunnerStateForwards(cid)`, tears each such forward down with
  `notify = true` and reason `runner_gone`. It is called from two places:
  the connection-teardown block in `server/server.go` beside
  `DropPortForwardsForConn`, and wherever `Registry.Add` returns a non-empty
  `displaced` id — a takeover is the runner on a new connection with a fresh
  session, and its old connection's teardown arrives only after the server's
  ping timeout (`server/registry.go`'s `Remove` comment), up to a minute of
  silent drops.
- The runner sends `ForwardEnded` when its tun fails (§7c). The server tears
  that forward down with `endpoint_failed` and the runner's detail.

The client, on `closed`, closes its tun and exits non-zero, printing the
reason and detail.

This changes two existing forwards' behaviour, deliberately. A tcp `-R` loses
its runner listener on every runner reconnect today (`RunHandle.Close`), and a
udp forward loses its runner registration with this spec's §7b; both rows
stayed listed and carried nothing. They now end with `runner_gone`. A tcp
`-L` holds no runner state — each connection is opened afresh — so it is not
in the predicate and survives a runner reconnect as it does today.

## 7. Endpoints

### 7a. Client

`harness-cli forward --ip ...` (§10) runs in the foreground like `-L`, in this
order:

1. Parse and validate the addresses (§9). Refuse before touching the kernel.
2. Create and configure the tun device (§8). On failure, exit with the
   error — nothing is registered.
3. `RegisterPortForward` with `protocol: ip`, `direction: link`.
   On anything but `ok`, close the tun and exit with the status and `detail`.
4. Pump in both directions until the control stream reports `closed` or the
   process is told to stop, then close the tun. On `closed`, print the reason
   and its detail and exit non-zero (§6f).

Steps 2 then 3 mirror `-L`, which binds its listener before registering
(`RegisterPortForwardRequest`'s comment: "bind_* is the CLIENT's
already-bound listener").

The client's datagram seam (`cli/forward_datagram.go`'s `dispatchDatagram`)
gains a third registry beside the udp `-L` and `-R` ones, keyed by
`forward_id`, whose `deliver` writes the payload to the tun.

### 7b. Runner

`handleOpenPortForward` (`runner/port_forward.go`) gains a `link` branch ahead
of the existing ones: check the task, create and configure the tun (§8), send
`ForwardSetupResult` either way, and on success start the pump. Its datagram
seam (`handleForwardDatagram`) gains the same third registry.
`ClosePortForward` closes the tun.

The runner's connection ending must close it too, and there is no hook for
that on the datagram side to copy. `RunHandle.Close` (`runner/connect.go:186`)
— which `PersistLoop` calls on every disconnect — releases only the tcp `-R`
listeners (`rforwards.closeAll`). The link registry is closed there, beside
it. So are the udp registries, in the same change, because the reading that
found this found them leaking through the same gap:

- a udp `-R`'s listening socket is never closed on reconnect — a socket
  leak, one per `-R` per reconnect;
- udp `-L` entries and every drop counter outlive the session for the life of
  the process. The sweep that reaps flows runs on the context passed to
  `driveAfterConn`, which is the persist loop's (`cli/persist.go`'s `dial(ctx)`),
  not the connection's as the comment at `runner/connect.go:426` says.

Read, not reproduced. The comment is corrected in the same change.

### 7c. The pump, and why nothing is oversized

Each direction is: read one packet from the tun, wrap it in
`ForwardDatagram{forward_id, flow_id: 0, payload}`, and `SendDatagram` it — the
**congestion-controlled** mode. The uncontrolled mode is for senders "whose
rate is bounded by construction" (`trsf/conn.go`, `SendDatagramUncontrolled`),
and an IP link is the opposite. A packet dropped because the window was closed
is the outcome the inner TCP is built to recover from.

MTU 1100 against the budget: `MaxDatagramSize()` is `CurrentMTU −
fixedOverhead` = 1200 − 30 = 1170 at the initial MTU (objtrsf
`trsf/conn.go:1150`, at the pinned version), and the kind byte, `forward_id`
and `flow_id` take 13 of it, so the largest IP packet that fits is 1157 ≥ 1100,
with room. The tun refuses anything larger at the sending
host's own IP layer — `ping -s 1100 -M do` fails locally, as on any 1100-byte
link — so an oversize drop in the harness cannot arise **while every leg is at
least its initial MTU**. A leg that is smaller is a carrier this budget did not
measure; the existing `*_dropped_oversize` counters (udp spec, third
amendment) report it, per hop, and nothing else is done about it in stage 1.

Read and write errors on the tun end the link. The client logs, closes its
tun and its control stream, which the server already reads as the forward
ending. The runner logs, closes its tun, removes the registry entry, and sends
`ForwardEnded` with the error text (§6f); a later `ClosePortForward` for it is
a no-op. A full receive path is the existing `*_dropped_queue`.

## 8. The tun device on Linux

Created with `golang.org/x/sys/unix`, already in `go.sum` as an indirect
dependency, so no new module:

- open `/dev/net/tun`; `TUNSETIFF` with `IFF_TUN | IFF_NO_PI` and the name
  template `hlink%d`, letting the kernel pick the number;
- on an `AF_INET` socket: `SIOCSIFADDR`, `SIOCSIFNETMASK` from `prefix_len`,
  `SIOCSIFMTU`, then `SIOCSIFFLAGS` with `IFF_UP`, using `unix.NewIfreq` and its
  typed setters — no hand-packed struct bytes;
- non-persistent: the device disappears when the last descriptor closes, so a
  crashed process leaves nothing behind.

The kernel picks the name, and both ends log it (`ip link: hlink0 up,
10.200.0.1/30, mtu 1100`). It is not on the wire: the server has no use for it
and each operator reads their own end's log.

`CAP_NET_ADMIN` over the network namespace is required. Measured 2026-10-25
on the development host:

```
unshare --user --map-root-user --net sh -c \
  'ip tuntap add dev t0 mode tun && ip addr add 10.200.0.1/30 dev t0 &&
   ip link set t0 mtu 1100 up && ip -br addr show t0'
# t0  DOWN  10.200.0.1/30      (DOWN = no process holds it open yet)
```

So inside `scripts/netem-lab`, which runs everything under
`unshare --user --map-root-user`, neither end needs root. That the
`netem-lab.py exec` path keeps the capability is expected from
`--preserve-credentials` in its `nsenter`, and is checked by the first step
of §12, not assumed.

Outside a namespace the process needs the capability itself. The failure is
`EPERM` from `TUNSETIFF`, surfaced verbatim in the error or `detail`.

## 9. Validation

Both addresses parse as IPv4; they differ; `prefix_len` is 1–31; both are
inside that prefix; and below /31 neither is the network or broadcast address
(a /31 has neither, RFC 3021). /32 is refused: the two ends would share no
subnet, so each kernel would need an explicit route to the other, and §3 says
the harness adds no routes. The client checks before creating anything; the
server checks again before contacting the runner, since a hand-built request
can skip the client. A 16-byte address is refused with
`ipv6 link addresses are not implemented` at both checks.

On any OS but Linux, `--ip` exits before dialling with
`ip links are Linux-only`, and a runner asked for a link answers
`ForwardSetupResult{ok: 0, detail: "ip links are Linux-only (this runner is
<GOOS>)"}`. The cli package is also built for wasm; the tun code lives behind
`//go:build linux` with a stub elsewhere.

## 10. Operator surface

### 10a. Creating

```
harness-cli forward --ip 10.200.0.1/30 --ip-peer 10.200.0.2 <task-id>
```

`--ip` is the client's address with the shared prefix; `--ip-peer` is the
runner's. One link per invocation; `--ip` and `-L` / `-R` / `-W` in one
command are refused, because they have different lifetimes on failure (a
refused link must not leave forwards behind it running).

All of that is declared in `forward`'s entry in `cli/verb/table.go`, not
hand-checked: two flags; `Exclusive` rules pairing `ip` with each of `L`, `R`
and `W`; `ip` added to the `AtLeastOne` rule; `Requires` in both directions
between `ip` and `ip-peer`; a `Notes` line; and a `ModalSurfaces` row for the
TUI's link mode.

**TUI.** The TUI already holds `-L` / `-R` forwards in its own process
(`tui/portforward.go`, `DoStartPortForward` / `DoStartRemoteForward`), and a
link is held the same way: the TUI process owns the client tun, and the link
ends when the TUI exits.

- **Key.** `L` in the main task view opens the forward modal in a third mode,
  `ForwardLink`, beside `p` (`-L`) and `b` (`-R`) (`tui/keys.go`'s
  `mainKeys`, `tui/actions.go`'s `onForwardStart`). `L` is unused in
  `mainKeys`; the grid view binds `L` in its own dispatch (`tui/grid.go`),
  which does not see main-view keys. The key tests decide if that reading is
  wrong.
- **Modal.** The input is `<client-addr>/<prefix> <runner-addr>` — the same
  two values `--ip` and `--ip-peer` take, parsed by the same function the CLI
  uses, so the TUI and the CLI cannot accept different addresses.
- **Stopping.** Through the forwards modal (`f`, then `x`), which already
  kills any registration. No dedicated stop key: `P` / `B` exist because a
  task can carry several forwards of each kind, and the forwards modal
  already covers picking among them.
- **Not Linux.** The modal opens, and starting the link fails with the same
  `ip links are Linux-only` the CLI prints, in the TUI's result pane.
- **TUI command line.** No change. `forward`'s verb declaration gives
  starting a forward to the CLI only (`CmdlineSurfaces: CLI`, recorded in the
  udp spec's first amendment), so `-L` / `-R` cannot be started from the TUI
  command line either. A link follows its siblings; adding all three there
  is a separate change.

**WebUI.** A browser cannot open a tun device, so the WebUI does not start
links. It lists and kills them like any other row.

### 10b. Listing and killing

Every surface already lists and kills any registration; a link is one.

- **Spec string** (`PortForwardSpecString`, `cli/port_forward_list.go`):
  `ip 10.200.0.1/30 <-> 10.200.0.2`.
- **Traffic line**: `bytes_to_target` is client→runner and
  `bytes_from_target` runner→client. `conns_*` are omitted on link rows — a
  link has no connections or flows — gated on `direction == link`, an
  existence condition, never on the value 0.
- **Counters**: the datagram group, gated on `carriesDatagrams()`.
- **JSON**: the `link` object, present exactly when `protocol == ip`.
- **DIR column**: `PortForwardDirFlag` (`cli/port_forward_list.go:164`)
  prints `-L` for anything that is not `remote`. It gains `link`, rendered
  `--ip`, the flag that created it.
- **Workspace save**: `PortForwardConfigSpec` (`cli/port_forward_list.go:385`)
  writes every `os_socket` row back as a spec string, from two save paths
  (`cmd/harness-cli/workspace.go:98`, `tui/workspace.go:388` and `:441`).
  A link row would come out as `-L :0::0` and re-apply as a tcp forward. It
  returns `false` for `direction == link`, so a saved workspace omits links.
  A link's `client_endpoint` is `os_socket` — the tun is an OS device — so the
  refusal is on the direction, not the endpoint kind.
- **Kill**: the kill gate (`server/task_handler.go:715`) picks the capability
  from the direction with its own `if remote` branch, separately from the
  registration gate. Both call one function, `forwardCapFor(direction)`, so a
  link needs the same capability to kill as to create (§6b).
- **Tap**: works on link rows, and on udp rows, which it does not today
  (§10c).
- **Close reasons**: `runner_gone` and `endpoint_failed` render wherever
  `killed` / `task_gone` do today — the client's exit line and a tap's
  `forward_closed` line (`tapCloseReason`) — with the detail after it.

### 10c. Taps on datagram forwards

Today the only caller that feeds a forward tap is the tcp splice
(`server/forward_splice.go:67` → `pf.observe` → `forwardTap.offer`).
`relayForwardDatagram` counts bytes and never offers them, so a tap on a udp
row attaches, the row's `taps` count rises, and nothing is shown. Taps are
made to work on every datagram forward — udp and ip — rather than refused
(operator, 2026-10-25).

- **Where.** In `relayForwardDatagram`, after the send attempt, so the record
  carries its `fate`. A datagram refused before the send (the sender is not an
  endpoint, the forward is not a datagram forward) is not offered: it never
  belonged to this forward. A datagram dropped for having no far end
  (`runner.Conn == nil`, `to == nil`) IS offered, with `dropped_queue`, the
  cause the relay already counts it under.
- **Sequence numbers.** `portForward` keeps one counter per
  `(flow_id, direction)`, incremented for every datagram the relay received on
  it, whether or not any tap is attached, so a tap opened late starts at the
  flow's real count — the same rule `stream_offset` follows for tcp (exec tap
  spec D12). The map is pruned with the flow: an entry untouched for the udp
  flow idle timeout (60 s) is deleted by a sweep on the server, which has no
  other signal that a flow ended. On an ip forward there is one key per
  direction.
- **Gaps.** `tapStreamKey` gains a `datagram bool` so a stream's missed bytes
  and a flow's are never merged under one key, and `forwardGapRecord` emits
  `datagram_gap` for a datagram key. The record says bytes; the count of
  datagrams missed is the `seq` jump on the next record, which the renderer
  prints.
- **What is not emitted.** No `conn_open` / `conn_close`: the server does not
  see a flow begin or end, only datagrams. `forward_closed` is unchanged.
- **Filters and truncation.** `direction_filter` and `max_record_bytes` apply
  as they do to `data` records.
- **Rendering.** `RenderTapRecord` (`cli/forward_tap_render.go`) is the one
  renderer the CLI, the TUI (`tui/tap_pump.go`) and the WebUI
  (`cmd/harness-webui-wasm/main.go`) all call, so the new kinds are added
  there once. A datagram's header line names the flow, the direction, the
  seq and the fate — `flow 3 → #41 relayed 512B` — and its body is the
  existing hex / text / raw rendering of the payload. A seq jump is printed as
  its own line, `flow 3 → missed 7 datagrams`. JSON emits every field.
  Reading the payload as an IP header is not done; on a link the operator
  has `tcpdump -i hlinkN` at either end for that.

### 10d. Surface matrix

`.claude/skills/surface-parity-checklist/SKILL.md` items 1–39 are walked item
by item, with a verdict per number, before implementation. The expected
outcome, which the walk confirms or corrects:

| surface | change |
|---|---|
| CLI input (`cmd/harness-cli/dispatch.go`, `forward` help) | `--ip`, `--ip-peer` |
| workspace file | save omits link rows (§10b); no grammar change |
| TUI key + modal (`tui/keys.go`, `tui/actions.go`, `tui/portforward.go`) | `L` → `ForwardLink` mode (§10a) |
| TUI cmdline | none — no forward is started there today (§10a) |
| WebUI input / cmdline | impossible: a browser opens no tun (§10a) |
| CLI display | spec string, traffic line, JSON |
| WebUI display (`cli/forward_snapshot_row.go`) | raw + rendered fields |
| TUI display (`tui/portforward.go`) | spec cell; conns cell empty on link rows |
| tap rendering (CLI, TUI, WebUI) | `RenderTapRecord` gains the two kinds and two close reasons (§10b, §10c) |
| caps (`cli/verb/caps.go`, every caps parser and listing) | `forward_link` (§6b) |
| server | §6, §10c |
| runner | §7b, §8 |

## 11. What stage 1 leaves for stage 2

- `flow_id` is ignored, not checked, on ip forwards (§5).
- The router needs its own address on the link and a mode on
  `IpLinkConfig`. Both are appended fields when stage 2 is designed.
- The relay never reads a packet, so a router can live at either endpoint or
  at the server without undoing anything here.

## 12. Testing

**Unit:**

- `IpLinkConfig` round-trips inside all three formats; a tcp or udp
  registration encodes no `link`.
- `validForwardShape` accepts exactly the four legal pairs plus `ip × link`.
- A link is denied without `forward_link`, including to a caller holding both
  `forward_local` and `forward_remote`; `forward_link` alone suffices; kill
  needs the same bit as create.
- `DropRunnerStateForwards`: a runner connection ending tears down its link,
  udp and tcp `-R` rows with `runner_gone` and leaves its tcp `-L` rows; so
  does a `Registry.Add` that displaces it. `ForwardEnded` tears down that one
  forward with `endpoint_failed` and the detail.
- Validation (§9): the boundary prefixes, equal addresses, network and
  broadcast addresses, an address outside the prefix, IPv6.
- Teardown sends `ClosePortForward` for `link` and for udp `local`.
- Registration waits for `ForwardSetupResult` and maps `ok: 0` to
  `link_failed` carrying the runner's detail.
- The pump, against an in-memory packet device standing in for the tun, so
  the tun is the only part not covered by `go test`.
- Renderers: a link row has no conns segment and does have the datagram
  counters at zero.
- Tap: a udp datagram relayed with a tap attached yields one `datagram`
  record with `relayed`; an oversized one yields `dropped_oversize`; a tap
  opened after N datagrams on a flow sees `seq` N+1 first; a tap that falls
  behind gets `datagram_gap` and a seq jump, never a `gap` with a flow id in
  `conn_seq`; the renderer prints `missed N datagrams` from the jump.

**Acceptance, in `scripts/netem-lab` — the milestone:**

1. `up --profile lan`. `exec srv -- harness-cli forward --ip 10.200.0.1/30
   --ip-peer 10.200.0.2 <task>`. Both ends log their `hlinkN` line. This is
   also the check that `exec` keeps `CAP_NET_ADMIN`.
2. `exec srv -- ping -c5 10.200.0.2` answers; `exec cli -- ping -c5
   10.200.0.1` answers. The client sits in `srv` and the runner in `cli`, so
   the packets cross the shaped path rather than looping inside one
   namespace.
3. `shape --profile wan-us`: the ping RTT rises by ~150 ms.
4. `iperf3` across the link, if it is installed in the lab — installing it is
   not part of this work.
5. `forward tap <id>` while step 2's ping runs: one `datagram` record per
   ICMP packet each way, all `relayed`.
6. `forward kill <id>` from the CLI: both `hlinkN` devices are gone
   (`exec srv -- ip link`, `exec cli -- ip link`).
7. Kill the `harness-cli` process with SIGKILL: the client's device is gone,
   the listing drops the row, and the runner's device is gone (§6e).
8. The same link from the TUI: `exec srv -- harness-tui ...`, select the task,
   `L`, enter `10.200.0.1/30 10.200.0.2`; ping answers as in step 2; quitting
   the TUI removes both devices.
9. Bring a link up again and restart the runner (SIGTERM, then start it as
   `up` did): the client exits with `runner_gone`, both devices are gone, and
   the row is gone. Then the same with SIGKILL on the runner: the same
   outcome, bounded by the reconnect (`Registry.Add` displacing the old
   connection) rather than by the server's ping timeout.

The failure path is not in the lab, because inside it both ends hold the
capability and outside it the client fails before the runner is asked. It is
covered by two Go tests instead: the runner's link handler, given a device
factory that returns `EPERM`, answers `ForwardSetupResult{ok: 0}` with the
error text and the CLI renders `link_failed` with `runner: ` before it; and
the real Linux device open, run as a non-root user outside any namespace,
returns `EPERM` (skipped when the test runs as root or off Linux).

**Wire:** `scripts/wire-skew-check.sh`, unconditionally — this touches
`message.bgn`, including a response format every forward uses.

## 13. What could go wrong

- **The runner's tun outlives the forward.** Only `ClosePortForward` or the
  runner's connection ending releases it, and §6e is the change that sends the
  first. Acceptance step 7 is the detector.
- **The two namespaces in the lab are not the ones the packets cross.** If
  both ends ran in one namespace, `ping` would answer through the kernel's
  local delivery and prove nothing. Acceptance step 3 is the detector: an RTT
  that does not follow `shape` means the harness is not on the path.
- **A leg below the initial MTU.** §7c's no-oversize argument assumes every
  leg is at least 1200; the per-hop oversize counters are the only signal if
  one is not.
- **A link reaches more than the operator meant.** A packet arriving on the
  runner's tun is delivered to any service on the runner's host bound to all
  addresses, and the reverse is true on the client's host. That is §6b's
  reason for a bit of its own, and it is the difference from `-L`, where the
  operator names one target.
- **A runner restart ends forwards that used to linger.** §6f ends tcp `-R`
  and udp forwards on a runner reconnect where today they stay listed and
  dead. A workflow that relied on the row surviving — re-reading it, or
  killing it later — now finds it gone. That is the intended trade.
