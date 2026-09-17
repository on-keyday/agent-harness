# A trsf datagram frame, and UDP port forwarding on top of it — Design

Date: 2026-09-17

Two repositories change together. `objtrsf` gains an unreliable-but-accounted
datagram frame; the harness gains a `udp` protocol axis and a `route` axis on
port forwarding, and becomes the frame's first consumer. The schema for both
lives here, in one place, because a reader who finds only half of it cannot tell
what the wire looks like.

Line numbers into `objtrsf` are against
`v0.0.0-20260907210744-7db84f609950`, the version pinned in `go.mod` today.

---

## 1. Problem

`harness-cli forward` moves TCP only. One accepted connection becomes one trsf
bidirectional stream and the server splices the pair
(`docs/superpowers/specs/2026-06-02-port-forward-design.md`). A UDP service on
the runner host — a DNS resolver, a measurement endpoint, a QUIC dev server, a
homegrown datagram protocol — has no route to the client at all.

Carrying UDP on the existing machinery is not a small variation on it. A
reliable, in-order stream retransmits, so a lost datagram arrives late instead
of not arriving; for every protocol that chose UDP, late is worse than absent.
Per-flow streams would also impose a stream open on the first packet of each
5-tuple.

What the transport does NOT have is an unreliable frame. The public API in
`trsf/api.go` is `SendStream` / `ReceiveStream` / `BidirectionalStream` /
`Multiplexer`; a grep for `Datagram` over the whole module finds only
`maxDatagramSize` in `trsf/congestion/pacer.go`.

There is an unreliable path at the layer below — `objproto.Connection.SendMessage`
seals one application packet and hands it to the socket
(`objproto/objproto.go:1429`), which is how every `appwire.AppKind` control
message travels. It is the wrong place to put a tunnel, for a reason that is
mechanical rather than stylistic:

- **It is never congestion controlled.** `sendApplicationFrame` goes straight to
  `sendPacket`. It does not touch `trsf.Streams`, so the NewReno controller and
  the pacer (`trsf/conn.go:1015`) never see it. The `CanSend()` gate at
  `trsf/conn.go:579` bounds stream sends and nothing else.
- **It is never acknowledged.** `AutoReceive` (`trsf/api.go:184`) hands any
  non-stream kind to `onEvent` without passing it to `Streams`, so its packet
  number never reaches `PacketNumTracker`. Nothing can be said afterwards about
  whether it arrived.

Every bulk path in this system already avoids that. `peer/publish.go` uses
`SendMessage` for the JOIN handshake and carries the payload on a stream. A UDP
tunnel on `SendMessage` would be the first bulk consumer of an unmetered path,
and it would share a connection with trsf streams that do respond to loss — so
under contention the streams would yield and the tunnel would not.

## 2. Non-goals

`Decided-by` in §3 says who chose. This section says what a later reader must
not read into the absence of something.

- **L3 / tun-device forwarding is not in this spec.** It is wanted later. The
  wire here must not preclude it, and §6 says how it does not. Nothing here
  claims L3 was evaluated and rejected.
- **Fragmentation and reassembly are not built, anywhere.** A payload that does
  not fit one datagram is dropped and counted. This is a decision (§3), not an
  oversight, and it has a consequence the operator must be able to see (§7).
- **The existing `appwire.AppKind` control messages are NOT migrated onto the
  new frame.** They keep their current behaviour: unreliable, unacknowledged,
  outside congestion control. Migrating them would give them loss visibility
  they lack today and is worth doing; it is a separate change with a blast
  radius covering every RPC path, and folding it in here would make this spec's
  rollout unreviewable.
- **TCP forwarding over the non-splice routes is schema-only.** §6 gives
  `route` to both protocols so the product has no holes, but the first landing
  need not implement all six cells. The repo already does exactly this with
  `PortForwardDirection` (`runner/protocol/message.bgn:2445`), where `local`
  shipped implemented and `remote` shipped as a reserved value.
- **No claim is made that the end-to-end route is faster.** §6 explains why the
  measured numbers do not decide this question, and the answer is not "we
  measured and it wins".

## 3. Decisions taken

`Decided-by` is provenance, not emphasis. Only rows marked `operator` were
chosen by the operator; the rest are this document's author's and a later reader
may overturn them on evidence.

| # | Decision | Decided-by |
|---|---|---|
| 1 | Build the datagram frame in `trsf` rather than on `appwire`, so it can be congestion controlled and acknowledged | operator |
| 2 | The frame offers BOTH a congestion-controlled and a congestion-uncontrolled mode | operator |
| 3 | The tunnel does NOT fragment. Oversized payloads are dropped and counted | operator |
| 4 | Both carriage routes are built — server-spliced and end-to-end data plane — not one | operator |
| 5 | Scope is the frame plus L4 UDP forwarding, in one spec. L3 comes later | operator |
| 6 | The frame's payload is opaque to `trsf`; flow multiplexing is the consumer's framing | author |
| 7 | `FileTransferRoute` is renamed `DataPlaneRoute` and shared, rather than a second identical enum | author |
| 8 | `protocol` is spelled as a `/udp` suffix on the existing spec grammar; `route` is a command-level flag | author |
| 9 | Drops are counted in three separate causes, never collapsed into one | author |
| 10 | `IsMTUProbe` is decomposed into three fields rather than generalised into one | author |
| 11 | A congestion-controlled datagram that meets a closed window is dropped, not parked | author |

## 4. Wire changes — all of them, in one place

### 4a. `objtrsf` — `trsf/wire/stream.bgn`

The transport-owned kind range is `0x00..0x3F`; `USER_DEFINED_START ::= 0x40`
(line 98) begins the consumer range that `appwire.AppKind` occupies. The
datagram frame is transport-owned, because the transport is what must
acknowledge it.

```
enum ApplicationPayloadKind:          # line 102; existing members are 0..6
    ping
    pong
    close
    stream_data
    stream_cancel
    stream_ack
    stream_window_update
    datagram                          # appended = 7

format DatagramPacket:
    data :[..]u8

format StreamAppPacket:               # line 122
    config.go.union = "noheap"
    header :PacketHeader
    match header.kind:
        ...
        ApplicationPayloadKind.datagram => datagram :DatagramPacket
        .. => error("Unexpected packet")
```

`DatagramPacket` carries no length field. `objproto` does not split an
application message — `server/trsf_state.go:132` records the consequence of
forgetting that — so one datagram is one packet by construction and
rest-of-packet is exact.

**Congestion mode is deliberately absent from the wire.** It is a property of
how the sender treats its own window. The receiver acknowledges both modes
identically, so a wire bit would offer a receiver a choice it does not have.

`fn isStreamRelated` (line 116) must gain `datagram`, and that makes its name
describe its membership rather than its job. Its only caller is the routing
branch at `trsf/api.go:184`, so what it actually answers is "is this decoded by
`StreamAppPacket` and handled by the run loop" — a datagram qualifies for that
reason, not because it is stream-related. Renaming it is optional; leaving the
name is a choice a later reader should be able to see was made.

Its membership and `StreamAppPacket`'s arms are the same set written twice in
one file, and `is_defined` cannot derive one from the other — that answers enum
membership, not union-arm membership. §10 says what guards the duplication
instead.

### 4b. Harness — `appwire/app.bgn`

```
enum AppKind:
    :u8
    ...
    dial_greeting      = 0x47
    forward_datagram   = 0x48      # appended
```

### 4c. Harness — `runner/protocol/message.bgn`

```
enum ForwardProtocol:
    :u8
    tcp = "tcp"
    udp = "udp"

# Renamed from FileTransferRoute. Members, ordinals and string tags unchanged;
# the ~100-line comment at :2104 describing where the plaintext sits on each
# route moves with it, because that comment is about the data plane and not
# about files.
enum DataPlaneRoute:
    :u8
    splice    = "splice"
    forwarded = "forwarded"
    direct    = "direct"

format ForwardDatagram:
    forward_id :u64
    flow_id    :u32
    payload    :[..]u8
```

Appended to `RegisterPortForwardRequest` (:2673) and `PortForwardInfo` (:2699):

```
    protocol :ForwardProtocol
    route    :DataPlaneRoute
```

Appended to `PortForwardInfo` only, meaningful when `protocol == udp`:

```
    max_datagram_size  :u16   # what fits RIGHT NOW; moves with PLPMTUD
    dropped_oversize   :u64
    dropped_congestion :u64
    dropped_queue      :u64
```

`OpenPortForwardRequest` is unchanged. It exists for the per-connection open
that TCP needs; UDP has none, because a flow is created by the arrival of its
first datagram.

There is no disk axis. `server/wal.go` persists wire bytes for exactly one type,
`RunnerSelector`; port forwards live only in the in-memory
`portForwardRegistry`, so none of the above can meet an older decoder on disk.

## 5. objtrsf — the datagram frame

### 5a. Sending

```go
SendDatagram(b []byte) error              // congestion controlled
SendDatagramUncontrolled(b []byte) error  // outside cwnd and the pacer
MaxDatagramSize() int
```

`MaxDatagramSize()` belongs to `trsf` so that no consumer restates
`CurrentMTU − objproto header − AEAD tag − frame header`. It moves with PLPMTUD
and callers must read it as a live value, not a constant.

All sending passes through the run loop, as stream data does
(`AppendData` → `sendTrigger` → loop), so a handoff queue is structural. It is
kept **shallow**, and neither end of it parks:

- the queue is full → the caller gets an error and the drop is counted;
- the run loop reaches a congestion-controlled datagram while `CanSend()` is
  false → **drop and count; do not park it**. Parking trades a visible drop for
  invisible latency and unbounded buffering, and for a tunnel the drop is the
  honest outcome — it is what the path would have done.

An uncontrolled datagram is sent regardless of the window.

### 5b. Receiving

`AutoReceive` currently decides two things with one predicate
(`trsf/api.go:184`): whether a packet is acknowledged, and whether it reaches
the stream machinery. A datagram needs the first and not the second.

The routing predicate widens to admit `datagram` — and **only** `datagram`. It
must not widen to "transport-owned kinds", because that set contains `ping`,
`pong` and `close`, which `AutoReceive` handles itself; routing those into
`Streams` would put them through `StreamAppPacket.DecodeExact` and into its
`error("Unexpected packet")` arm.

Acknowledgement is then decided where it already is — per union arm inside
`handlePacket`. `stream_data` (`trsf/conn.go:443`), `window_update` (:487) and
`stream_cancel` (:498) each call `s.pt.InsertUnacked`; the ACK arm deliberately
does not, because an ACK is not ack-eliciting. The datagram arm calls it, then
places the payload on a **bounded receive queue** drained by
`Transport.ReceiveDatagram(ctx)`. Overflow drops the newest and counts it,
matching what a socket receive buffer does.

### 5c. Loss, acknowledgement, and the two modes

Not retransmitting is not a structural change. Retransmission is expressed as
the per-packet `OnLost` callback — `trsf/conn.go:764` re-pushes a stream cancel
from inside it — so a datagram passes an `OnLost` that counts and does not
re-push.

The two modes are where the existing code does not fit, and the reason is that
`SentPacket.IsMTUProbe` (`trsf/ack_handler.go:127`) is **three concepts fused
into one bool**:

| | in-flight / `RecordSend` | `RecordACK` | `RecordLoss` | `loss.Packets` / spurious | retransmit | probe bookkeeping |
|---|---|---|---|---|---|---|
| stream data | yes | yes | yes | yes | yes | — |
| MTU probe | no | **yes** | no | no | no | yes |
| datagram, controlled | yes | yes | yes | yes | no | — |
| datagram, uncontrolled | no | no | no | **yes** | no | — |

The load-bearing cell is the last row's `loss.Packets`. An MTU probe's loss is
excluded from the loss accounting on purpose — `detectLost`'s own comment says a
probe is "expected to be lost — that is how the probe reports a too-large MTU —
so they are not evidence about the path". **A datagram's loss IS evidence about
the path**, and is the only thing that can distinguish a path drop from one of
our own. So "exempt from congestion control" and "not evidence about the path"
must stop being the same flag.

Decomposition:

- `CongestionExempt` — participation in `bytesInFlight`, `RecordSend`,
  `RecordACK`, `RecordLoss`.
- `PathEvidence` — participation in `loss.Packets`, `loss.Events`,
  `rememberDeclaredLost`.
- `IsMTUProbe` — retains only `mtuProbesOutstanding`.

An MTU probe sets exempt and the probe flag, and clears path-evidence, which
preserves today's behaviour everywhere except `RecordACK` (§5e).

`detectLost` currently gates two things with one condition —
`if lostCount > mtuProbe { loss.Events++; cong.RecordLoss(...) }` — and they
must be split, because an uncontrolled datagram's loss belongs in the first and
not the second.

### 5d. Every site that reads the fused flag

`IsMTUProbe` and `mtuProbesOutstanding` are read at **nine** places in
`trsf/ack_handler.go`, plus the construction site in `trsf/conn.go:908`. A new
packet class needs an answer at each. Four were read while writing this spec;
the rest are to be opened during implementation, not assumed.

| site | what it decides | read? |
|---|---|---|
| `:143` `GetInternal` | copies the flag into `InternalSentPacket` (observability) | yes |
| `:198` `auditBytesInFlight` | recomputes expected in-flight, excluding probes | yes |
| `:214` `OnSent` | in-flight and `RecordSend` participation | yes |
| `:279` `detectAck` | `probeSize` excluded from `removeBytesInFlight` | yes |
| `:337` `detectLost` | `probeSize` excluded from `removeBytesInFlight` and `RecordLoss` | yes |
| `:378` `setLossDetectionTimer` | **see below** | yes |
| `:405/:415/:428` `declareLostMTUProbes` | probe-specific loss declaration | no |
| `:476` `OnTimeout` | PTO handling | no |

**`:378` is the trap.**

```go
if ah.bytesInFlight == 0 && ah.mtuProbesOutstanding == 0 {
```

This disarms the loss-detection timer. An uncontrolled datagram is in neither
term — it is not in flight and it is not a probe — so a naive `CongestionExempt`
field lets the timer disarm while uncontrolled datagrams are still outstanding.
Their loss is then never detected, `OnLost` never fires, and the drop counter
never increments. The one property the table above marks as wanted for that row
dies silently. The condition must account for outstanding packets whose loss we
still intend to observe.

`auditBytesInFlight` needs the same treatment for the opposite reason: it
already excludes probes when recomputing the expected total, and will report a
false discrepancy on exempt datagrams until it excludes those too.

### 5e. An asymmetry in `RecordACK`, and when it becomes a defect

`detectAck` ends:

```go
ah.removeBytesInFlight(sentSize - probeSize)   // probes excluded
ah.cong.RecordACK(sentSize, rcvTime)           // probes NOT excluded
```

The send side does not put probe bytes into `RecordSend`, and `detectLost`
excludes them from `RecordLoss`. Only the ACK side counts them, which grows the
window using bytes that never consumed it.

This is not worth calling a defect today. An MTU probe is one packet per 30s
reprobe period and the effect is unmeasurable. **It becomes a defect when this
spec's consumer arrives**: uncontrolled datagrams flowing at rate would have
every acknowledgement inflate the window of the controlled streams sharing the
connection — a flow that ignores congestion would also be enlarging everyone
else's share.

`RecordACK` takes `sentSize - exemptSize`. This changes MTU-probe behaviour
slightly and is called out here rather than folded in silently.

### 5f. What the MTU tracker does not expose

`mtu.MTUTracker` has `OnMTUUpdate(fn func(int))` (`trsf/mtu/plpmtud.go:53`), but
`Streams` constructs the tracker internally and does not surface it. A consumer
that must follow the MTU — the L3 tun device of a later spec is exactly that —
can only poll `GetInternalState().CurrentMTU`, and that call walks every stream
and allocates a `SentPackets` slice, so it is not a per-packet query.

Exposing the callback is not required by this spec and is the natural companion
to it.

## 6. Harness — routes and flow multiplexing

### 6a. Both routes, named by the caller

The three paths already exist and are already named by the caller for file
transfer. The comment at `runner/protocol/message.bgn:2104` documents them and
the refusal rule: a route that cannot be taken is refused with
`route_unavailable` and **never silently replaced**, because a caller who asked
for `forwarded` asked for the server not to read these bytes and quietly
splicing hands over exactly what was withheld. Port forwarding inherits that
rule unchanged.

What differs for datagrams is the implementation cost, and it inverts the
intuition:

- **`forwarded` / `direct`**: the server needs no new data path. `SetProxy`
  (`server/dataplane.go:182`) already forwards packets without decrypting them,
  so client and runner hold one objproto connection end to end and the tunnel's
  congestion control measures the whole path. What is new is a grant kind for a
  forward and its revocation.
- **`splice`**: the server must decode each datagram and re-emit it on the other
  connection. Two congestion domains, a junction that must decide what to do
  when the far leg cannot take a packet (drop — that is what a router does), and
  a counter saying who dropped it.

**Speed does not decide this.** The measured fleet ordering is
`splice 5.0 / direct 4.1 / forwarded 2.3 MB/s`, but the same comment establishes
that the gap is not the route: `openDataPlaneStream` dials a fresh connection
per transfer, so `forwarded` and `direct` pay a cold congestion window every
time while `splice` rides the server's long-lived connection. Handing a fresh
connection a large initial window took `forwarded` from 8.5 to 24.5 MB/s against
splice's 34.

A port forward is not a file transfer. **A registration is long-lived**: one
data-plane connection per registration pays the cold start once and every
subsequent flow rides a warmed window. The artifact that produced 2.3 MB/s does
not arise. That is a prediction, not a measurement — §10 says how to falsify it.

`direct` additionally costs availability rather than speed: the punch has a
lifetime, cloud NAT gateways use destination-dependent mappings so the
single-sided punch cannot work at all behind one, and there is no fallback by
design. A browser client cannot take it either (`dataPlaneDirectOK` requires
both ends on udp).

### 6b. `route` applies to both protocols

`protocol × route` is 2 × 3 and **all six cells mean something** — a TCP forward
over a data-plane connection is a TCP forward the server cannot read, and the
data plane already carries streams, which is what file transfer does. Leaving
`route` meaningful only for UDP would create a hole in the product that then
needs a predicate to defend, which is the shape the `ClientEndpointKind` comment
already warns about in this same file.

### 6c. Flow multiplexing

`ForwardDatagram` (§4c) names the registration and the flow. `forward_id` is
redundant on a data-plane route, where the carrier belongs to one registration,
and is carried anyway so that **one decoder serves every route**. Twelve bytes
are cheaper than two framings.

- **`-L`**: the client's UDP socket receives from a source address, allocates or
  finds a `flow_id` for it, and sends. The runner, on an unseen `flow_id`,
  creates a socket toward the target and remembers the mapping; replies return
  on the same `flow_id` and the client writes them back to the source address.
- **`-R`**: the runner binds, and a packet from a new source becomes a new
  `flow_id`. **No notification frame is needed.** TCP's `-R` needs
  `RemoteForwardConn` because a stream must be created and named before bytes
  can move; a UDP flow is created by the arrival of its first datagram, so
  `PortForwardEventKind.conn_notify` stays TCP-only.

Flows have no close, so both ends keep a table with an idle timeout of **60
seconds** — between conntrack's 30s unreplied and 180s assured — and a size cap.
On overflow the oldest idle flow is evicted and counted; without a cap, a peer
that varies its source port can make the runner open sockets without bound.

`PortForwardInfo.conns_total` / `conns_open` are reused as flow counts on a UDP
row. A flow is the datagram analogue of a connection and the display says
"flows" for those rows.

### 6d. Room left for L3

`trsf` sees an opaque payload; the harness owns the framing. An L3 mode is then
"`flow_id` names a tun device, `payload` is an IP packet" and needs no change
below the harness. Fragmentation, if it is ever wanted, is added to
`ForwardDatagram` rather than to the transport frame.

The size budget is what L3 must plan around. The anchor in this repo is
`server/trsf_state.go:132`, which records ten `TrsfConnState` rows measuring
1229 bytes against udp's 1200 path MTU and being dropped for it — so the usable
application payload at the initial MTU is a little under 1200, and this spec's
framing takes 13 bytes more (kind byte plus `forward_id` and `flow_id`). Call it
~1157 B at `DefaultInitialMTU`, growing toward ~1400 as PLPMTUD converges on
`DefaultMaxMTU = 1500 − 48` (`trsf/conn.go:977`). `MaxDatagramSize()` is the
authority; these figures are for sizing an L3 tun MTU (~1100 fits with room),
not for a consumer to compute with.

The same arithmetic is why **a 1200-byte QUIC Initial does not pass until the
outer path has grown**, which §7 makes visible rather than mysterious.

## 7. Operator surface

### 7a. Spelling

```
-L 5353:127.0.0.1:5353/udp
-L 5353/udp
-R 5353:127.0.0.1:5353/udp
```

Absent suffix means tcp, so no existing spelling changes. The precedent is
docker's `-p 5353:5353/udp`: the same colon-separated shape with the same
suffix. There is no ambiguity — a hostname cannot contain `/` and IPv6 literals
are already unsupported by `ParseForwardSpec`.

The workspace file needs no grammar work: `cli/workspace/config.go:6` states
that the package "owns the FILE, not the value grammars inside it" and hands
values to `cli.ParseForwardSpec`. One place restates the grammar and must be
updated — the error string at `cli/workspace/validate.go:33` — and that
restatement is a pre-existing split in the grammar's single source of truth.

`--route` is a command-level flag, mirroring the `--route` that file transfer
already has (`cli/verb/route.go`). It applies to every spec in the invocation;
mixing routes means running two commands. The schema carries `route` per
registration, so the granularity exists and only the CLI declines to expose it.

Four call sites create forwards without going through the spec parser —
`cli/sshgw/tcpip.go`, `cli/x11.go`, `cli/raw_forward_wasm.go`,
`cli/preview_forward_wasm.go`. All are TCP by construction; whether they pass
`tcp` explicitly or rely on the enum's zero value is decided during
implementation, not left to whichever is easier at each site.

### 7b. Display

The convention already exists and is documented in
`cli/forward_snapshot_row.go`: one renderer per display concept lives in `cli`,
all three surfaces consume it, and counters are emitted raw **and** rendered so
the browser does not re-derive a format in JS. New axes go into those renderers,
not into three formatting sites.

- **`protocol` → `PortForwardSpecString`** (`cli/port_forward_list.go:114`). It
  changes what the row means, so it belongs on the identity line, and it
  round-trips with what the operator typed:
  `127.0.0.1:5353 -> 127.0.0.1:5353/udp`.
- **`route` → its own column**, beside `origin`. It describes how the bytes are
  carried, not what is forwarded, so it does not belong inside the spec string.
- **drops and `max_datagram_size` → `PortForwardTrafficLine`** (:162), on UDP
  rows.

### 7c. Three drop causes, never one

Collapsing them erases the operator's next action.

| counter | meaning | what it asks of the operator |
|---|---|---|
| `dropped_oversize` | the payload did not fit one datagram | nothing on our side; the application must send smaller, or the path must grow |
| `dropped_congestion` | the window was closed | congestion control did its job; reduce offered load |
| `dropped_queue` | the run loop fell behind | host load, not the network |

And a fourth thing that is none of ours: loss on the path, in `LossStats`, which
already rides the SIGUSR1 dump and the TUI `trsf` view. **If "we dropped it" and
"the network dropped it" are not distinguishable, every diagnosis starts at the
wrong layer.** That is the whole argument.

`dropped_oversize` is the only line that explains a QUIC connection failing to
establish, and it means nothing alone, so it is rendered next to the size that
caused it: `mtu=1157 oversize=3`. That number moves with PLPMTUD and the display
is a snapshot.

### 7d. Zero versus absent

`portForwardJSON` (`cli/port_forward_list.go:176`) already carries the rule:
"Always emitted, zeros included — the JSON form carries everything, with no
elision." The new fields follow it, with one boundary:

- **omitted because the value is zero — forbidden.** `oversize=0` on a UDP row
  states that nothing has overflowed yet.
- **omitted because it cannot exist — correct.** A TCP row has no
  `max_datagram_size`.

The gate is `protocol == udp`, an existence condition, never the value. Getting
this backwards deletes the one clue available to an operator asking why QUIC
will not connect.

## 8. Surface matrix

Both axes reach every input and display surface, so
`.claude/skills/surface-parity-checklist/SKILL.md` items 1–39 are walked item by
item with a verdict per number before implementation — not summarised.

| surface | file | what changes |
|---|---|---|
| CLI input | `cmd/harness-cli/dispatch.go` | `/udp` suffix, `--route` |
| workspace file | `cli/workspace/validate.go` | error string only (grammar is delegated) |
| TUI input | `tui/portforward.go` | protocol and route in the form |
| WebUI input | `cmd/harness-webui-wasm/main.go` | same two fields |
| spec parse | `cli/port_forward.go` | `ParseForwardSpec`, `ParseRemoteForwardSpec` |
| non-parser creators | `cli/sshgw/tcpip.go`, `cli/x11.go`, two `*_wasm.go` | state `tcp` |
| CLI display | `cli/port_forward_list.go` | spec string, traffic line, JSON struct |
| WebUI display | `cli/forward_snapshot_row.go` | raw + rendered fields |
| TUI display | `tui/portforward.go` | new columns |
| server | `server/port_forward.go`, `server/port_forward_registry.go` | route dispatch, splice-side datagram relay, counters |
| runner | `runner/port_forward.go` | UDP flow table beside the TCP dial/listen paths |

## 9. Rollout

Two repositories, in order:

1. **objtrsf**: frame, send/receive API, the field decomposition of §5c–5d, the
   `RecordACK` correction of §5e. Landed on trunk by fast-forward per the
   recorded policy, then a `go.mod` bump in the harness.
2. **harness**: schema, routes, flow tables, surfaces.

Step 1 is a change to the transport kind range, which is below the handshake
layer — an old peer cannot decode the packet at all rather than rejecting it at
the PSK layer. `scripts/wire-skew-check.sh` documents that outcome and is run
unconditionally. The harness step touches `message.bgn`, so it is run again
there.

The `FileTransferRoute` → `DataPlaneRoute` rename is source churn across 20
files (cli, tui, wasm, server, verb, integration) with no wire change: members,
ordinals and string tags are unchanged, and `--route` keeps its name. Regen
churn in the generated `.go` is expected and is not investigated.

## 10. What could go wrong

- **The loss-detection timer disarms early** (§5d) and uncontrolled datagram
  drops stop being counted. Silent. A test must send uncontrolled datagrams,
  drop them all, and assert the count rises.
- **`bytesInFlight` underflows.** Exempt packets never entered it, so any
  retirement path that subtracts unconditionally goes negative.
  `auditBytesInFlight` exists and is the detector.
- **The predicate and the union drift apart** (§4a). Adding an arm without the
  predicate means packets never reach `handlePacket` and vanish with no error
  anywhere. Detection: a kind below `wire.USER_DEFINED_START` reaching
  `AutoReceive`'s `onEvent` is by construction a routing bug, and is countable.
  Today the condition cannot fire.
- **The end-to-end route is slower anyway** and §6a's reasoning about cold
  windows does not survive a long-lived connection. Both routes exist, so the
  answer is a flag change rather than a redesign.
- **The uncontrolled mode reaches a bulk consumer.** Two separate methods make
  it greppable, but nothing prevents it. The counters in `InternalState` make
  misuse observable, which is the chosen defence — visibility, not prohibition.

## 11. Testing

- **trsf, in-process**: a datagram arrives; a lost datagram is not retransmitted;
  a lost datagram's `OnLost` fires and is counted; a controlled datagram meeting
  a closed window is dropped rather than parked; an uncontrolled datagram is sent
  with the window closed; acknowledged exempt packets leave `bytesInFlight` at
  zero without underflow; `MaxDatagramSize()` tracks a changed MTU.
- **`mock` rung is a control.** `trsf/throughput_test.go` never reaches objproto,
  so it must not move for any of this. Relay-rung comparisons need an unloaded
  box and interleaved A/B; a difference under ~25% there is not a result.
- **Harness, unit**: `/udp` parses in every spec form including the bare one and
  through the workspace file; a TCP row renders no `max_datagram_size`; a UDP row
  renders `oversize=0`; an unavailable route is refused rather than substituted.
- **End to end, on a dummy harness**: a UDP echo behind `-L` and behind `-R`; the
  same forward on `splice` and on `forwarded`; an oversized payload increments
  `dropped_oversize` and nothing else; a flow idles out and `conns_open` falls.
- **The falsifier for §6a**: run one long-lived forward on each route and compare
  after the first flow, not during it. If `forwarded` still trails `splice` once
  the connection is warm, the cold-window explanation is wrong and §6a's
  prediction fails.

---

## Amendment — 2026-09-17, written from the implementation

Three things this spec got wrong or left unsaid, recorded here so a later
reader verifying against the shipped behaviour is not misled by the text above.

### §5c: retransmission is NOT fully expressed by the `OnLost` callback

The claim was that "not retransmitting is expressed as the per-packet `OnLost`
callback", so passing one that does not re-push is the whole of it. It is not.
`OnTimeout`'s PTO path also has to KNOW which packets are retransmittable, for
two reasons it could not get from a callback:

- with only an exempt packet outstanding it returned
  `errors.New("BUG: no packets in flight")` — a legitimate state reported as a
  defect, permanently, in the log of every connection carrying uncontrolled
  datagrams;
- its retransmit loop picked any non-probe, so it would have chosen a datagram,
  called an `OnLost` that re-sends nothing, reported `true` for a retransmission
  that never happened, and left the packet in `sentRanges` for the next expiry
  to count as a second loss.

So `SentPacket` has FOUR new-or-narrowed properties, not three:
`CongestionExempt`, `PathEvidence`, `Retransmittable`, and `IsMTUProbe` reduced
to probe bookkeeping. `declareLostMTUProbes` generalised to
`declareLostUnretransmittable`, whose predecessor's own comment had already
argued the case without noticing it was not probe-specific.

### §6: the runner needs the target BEFORE the first datagram

The spec described the flow lifecycle but not how the runner learns where to
dial. tcp carries the target on a per-connection `RunnerOpenPortForwardRequest`;
a udp flow has no open, so there is nothing to carry it on. Shipped:
`RunnerOpenPortForwardRequest` gains `protocol`, and a udp `-L` sends ONE such
request at registration with `stream_id = 0`. That field is also what tells the
runner which of the two things it has been handed.

### §7: a second serving path existed and the spec did not name it

§8's matrix lists "TUI input" as `tui/portforward.go`, which is true for `-L`
and was incomplete for `-R`: `DoStartRemoteForward` called
`ServeRemoteForwardControl` directly, which is only the tcp half. A udp `-R`
from the TUI bound a listener on the runner and carried nothing. Fixed by
making `(*Client).ServeRemoteForward` the whole obligation, with a grep guard;
the matrix row should be read as "every path that SERVES a forward", not only
every path that parses one.

### Surface matrix, checked row by row against the code

| row | verdict |
|---|---|
| CLI input (`/udp` suffix, `--route`) | `/udp` done; **`--route` omitted** — splice is the only implemented route, so the flag's every non-default value would error. The axis is on the wire and in the API; only the flag is deferred |
| workspace file | done — the grammar is delegated, and the validator's restated error string was updated |
| TUI input | done for `-L` (spec string) and for `-R` after the fix above |
| WebUI input | **n/a** — starting a forward is CLI+TUI only (`forward`'s `CmdlineSurfaces: CLI` plus two TUI modals); the WebUI's forward surface is the listing |
| spec parse | done |
| non-parser creators | done — all four state `tcp` explicitly rather than inheriting a zero value |
| CLI display | done — spec string, traffic line, JSON struct |
| WebUI display | done — `ForwardSnapshotRow` carries both axes raw AND the rendered `traffic`/`spec` from the Go renderers |
| TUI display | done — the spec cell carries `/udp`, and an always-present `udp` column carries the datagram numbers (empty on tcp rows, which is an existence gate) |
| server | done — route dispatch, the splice-side datagram relay, the counters |
| runner | done — both directions |

`udp × forwarded` and `udp × direct` remain `route_unavailable`, as §2 said they
would. §6a's prediction — that a long-lived registration does not pay the cold
window those measurements captured — is therefore still untested, and its
falsifier in §11 is still open.

---

## Amendment — 2026-09-17, the frame lost a byte

§4a's `ApplicationPayloadKind.datagram` and `DatagramPacket` no longer exist.
objtrsf `ab0ecf8`, harness `d764baca`.

The shape this spec shipped put **two** kind bytes on the wire: the
transport-owned `datagram` kind in the header, and inside its opaque payload the
consumer's own `appwire.AppKind`. §4a justified the outer one on
acknowledgement — only transport kinds reach `PacketNumTracker`, so a consumer
kind could not be ACKed — and that was true of how `AutoReceive` routed, not of
the layering.

The inner byte was never a discriminator. `ForwardDatagram` was the only thing
sent through `SendDatagram`, and `ReceiveDatagram` is a separate queue from the
control seam, so the consumer already knew what it was getting. `peer/conn.go`
recorded the real reason it was there: so a payload is spelled the same whether
it goes to `SendMessage` or `SendDatagram`.

Now the routing asks the CONSUMER. `SetDatagramKinds(func(kind uint8) bool)` is
registered by the owner, `AutoReceive` consults it for kinds at or above
`USER_DEFINED_START`, and the byte in the header position is the consumer's own
— acknowledged like stream data, with the core still never learning what it
means. The range boundary already said who owns the byte; it did not need saying
twice.

Consequences for what this spec claims:

- **`datagramFrameOverhead` is gone** — it was literally the removed byte, so
  `MaxDatagramSize()` grows by one and §6d's ~1157 B becomes ~1158.
- **§4a's `isStreamRelated` note stands but shrinks**: the predicate and the
  union are still two hand-kept lists, with one fewer member in each.
- **`sendDatagram` refuses a payload whose leading byte is below
  `USER_DEFINED_START`.** That check is what makes one byte sufficient for both
  layers: such a payload would be decoded by the peer's core as one of its own
  kinds.
- **The wire break is udp forwards only.** An old sender's kind 7 raises
  `unrouted_transport_kind` on a new peer — the counter §4a's sibling says must
  read zero — so one direction announces the skew; a new sender's 0x48 lands on
  an old peer's control seam and is dropped.

The §2 non-goal that migrating `appwire.AppKind` control messages onto the new
frame is a separate change is **unaffected**: those still travel by
`SendMessage`, unreliable and unacknowledged. What changed is only that a
datagram no longer wraps its consumer kind in a transport one.
