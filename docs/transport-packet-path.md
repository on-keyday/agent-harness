# Every stage one packet passes through

A complete enumeration of the goroutines, queues and serialization points a
single packet crosses between the application's write call and the peer
application's read call, over the UDP underlay.

**Why this exists.** On 2026-08-31 five hypotheses were tried against the
transport's throughput and one was right. The four misses were all local
changes proposed from a profile — a loss detector, a relay chunk size, a send
buffer, a batched send loop — and each was killed by measurement after being
written. The common fault was reasoning one step ahead of the structure. This
document is the structure, so the next attempt argues from it instead of from
a hunch.

**And then the structure was tested too.** Every serialization point named in
§6 is real, and §9 records that three of them cost nothing measurable; the one
change that did win (§3, worth 26.6%) was an allocation, not a queue. So read
this as a map of what the code *is*, never as a ranking of what it costs. The
instrument that separates the two is §8's ladder, which is cheap enough
(`go test -bench Throughput`, seconds) that there is no longer any reason to
argue a throughput claim rather than measure it.

Everything below cites a file and line. Where a claim is measured rather than
read, it says so.

---

## 1. The goroutines

`grep -rn "go func()\|^\s*go " objtrsf/{trsf,objproto,transport}` over the
non-test, non-mock, non-websocket files returns exactly these on the UDP data
path:

| # | goroutine | started at | lifetime | one iteration handles |
|---|---|---|---|---|
| G1 | trsf run loop | `trsf/conn.go:791` (`go s.run(ctx)`) | per **connection** | ONE packet — received or sent, never both |
| G2 | `AutoSend` | `trsf/api.go:85`, started by the harness | per **connection** | ONE `SendAction` |
| G3 | `AutoReceive` | `trsf/api.go:167`, started by the harness | per **connection** | ONE message |
| G4 | UDP sender | `transport/udp.go:55` | per **endpoint** | ONE datagram |
| G5 | UDP reader | `transport/udp.go:76` | per **endpoint** | ONE datagram |

**G4 and G5 are per endpoint, not per connection.** `server/server.go:530`
calls `transport.UDPEndpoint` once for the listener, so every connection a
server holds — every client and every runner — shares one sending goroutine
and one receiving goroutine. A relay, which by construction has traffic on two
connections at once, funnels both through that single pair.

There is exactly **one** goroutine inside `trsf` (`conn.go:791`). Everything
else in that package is trigger-driven: `trigger` (`trsf/queue.go:6`) is a
one-slot channel that coalesces wake-ups rather than queueing them.

## 2. Send direction, one connection

| stage | code | what it does | handoff to the next stage |
|---|---|---|---|
| S1 | `sendStream.WriteContext` → `AppendDataContext`, `trsf/send_stream.go:113,163` | copies the caller's bytes (`make` + `copy`, per Write), appends to `inputBuffer`, blocks if `dataInBuffer-offset >= bufferLimit` (**1 MB**, `send_stream.go:80`) | `sendTrigger.Push(r)` — wakes G1 |
| S2 | G1, `trsf/conn.go:427` `run` | `stream := s.sendTrigger.Pop()` — **one stream**, then `triggerPacket(maxPayload)` — **one packet**; assigns a packet number, `sh.OnSent` | `s.send.Push(&SendAction{…})` — wakes G2 |
| S3 | G2, `trsf/api.go:85` | `p.Recv(ctx)` (blocks), then `action.Send` → objproto `sendApplicationFrame` (AES-GCM) | the endpoint's sender channel — wakes G4 |
| S4 | G4, `transport/udp.go:55` | `for pkt := range sendTo`, then `WriteToUDP` | `sendto(2)` |

Four goroutines, three channel handoffs, before a byte reaches the kernel.

## 3. Receive direction, one connection

| stage | code | what it does | handoff |
|---|---|---|---|
| R1 | G5, `transport/udp.go:76` | `ReadFromUDP` into a shared 64 KiB buffer, then `make`+`copy` per datagram, then `sess.Receive(...)` **synchronously** | none — the call runs on G5 |
| R2 | objproto, `objproto/objproto.go:1066` | decrypts, then `activeConn.msgs.SendMessage(...)` — which **spawns a goroutine per message** (`objproto/message.go:117`) whose whole body is one channel send | message channel — wakes G3 |
| R3 | G3, `trsf/api.go:167` | `ReceiveMessageContext` (blocks), `p.Send(data)` for stream-related kinds | trsf recv queue — wakes G1 |
| R4 | G1, `trsf/conn.go:427` | `recvedData := s.recv.Pop()`; if non-nil → `handlePacket` → `OrderingQueue.Push` → the receive stream's buffer; then `continue` | the reader's signal |
| R5 | app | `ReadDirect` / `Read` returns | — |

**R2 spawns a goroutine per inbound packet.** `messageChannel.SendMessage`
(`objproto/message.go:117`) is:

```go
go func() {
    defer c.senderWg.Done()
    select {
    case c.messageChan <- internalMessage{msg: msg, seqNum: seqNum}:
    case <-c.ctx.Done():
        return
    }
}()
```

A whole goroutine to push one value into a channel, once per received packet,
and the `go` statement is paid on G5's stack because `sess.Receive` is
synchronous (R1). The non-spawning `SendMessageBlocking` sits right beside it
at `objproto/message.go:139`, so the spawn is there to keep a full channel
from blocking the socket reader.

**It could not be replaced by a blocking handoff, and the reason is the
locking, not the channel.** `receiveApplication` holds
`s.endpointLock.RLock()` *and* `activeConn.mu.Lock()` across the call
(`objproto/objproto.go:993`), so a send that blocked there would stall every
other connection on the endpoint. The fix that did work keeps the spawn as the
fallback and simply tries the send first, so the goroutine is paid only when
the reader is actually behind (objtrsf `a21e7a3`).

**Quantified, and it was worth 26.6%** — measured on the point-to-point rung
of the ladder in §8, not on this relay path, where it does not show at all.

**R1 is the stage that made the receive buffer matter.** `sess.Receive` runs on
G5, so while it decrypts and demultiplexes, nothing is calling `ReadFromUDP`
and arrivals queue in the socket. That is why the kernel default of 208 KB
overflowed and the drops were invisible to `tc` — they happen past the qdisc.
Fixed in objtrsf `8dc15c3`.

## 4. G1 is one goroutine for both directions

From `trsf/conn.go:427`:

```go
for {
    …
    recvedData := s.recv.Pop()
    if recvedData != nil {
        s.handlePacket(recvedData)
        continue            // ← back to the top; the send half below is skipped
    }
    deadline := s.nextWakeDeadline()
    select { … }            // ← parks
    …
    stream := s.sendTrigger.Pop()   // ← one stream
    …
}
```

Three consequences follow from the shape alone:

1. **One packet per iteration.** Receive and send are alternatives within an
   iteration, never concurrent.
2. **Receive starves send while the recv queue is non-empty.** The `continue`
   after `handlePacket` skips the whole send half.
3. **Every idle iteration is a park.** When neither queue has work the loop
   blocks in `select`, and the next packet costs a wake-up.

**Measured, consistent with (1):** the SIGUSR1 dump's `loopIters` advanced
~6,400/s on a connection whose packet rate was of the same order.

## 5. The harness `file push`, end to end

`server/file_transfer.go:73` and `:128` splice the two connections with
`spliceBidiHalfClose` → `relayBytes` (`server/task_handler.go:1537`):

```go
data, eof, err := src.ReadDirect(64 * 1024)   // leg 1
…
dst.AppendData(eof, data)                     // leg 2 — nothing is read meanwhile
```

So one byte pushed from a client to a runner crosses:

```
cli    S1 → S2 → S3 → S4                    (4 stages, 1 connection)
         ↓ wire
server R1 → R2 → R3 → R4                    (4 stages, connection A)
       relayBytes read  →  relayBytes write (2 stages, strictly alternating)
       S1 → S2 → S3 → S4                    (4 stages, connection B)
         ↓ wire
runner R1 → R2 → R3 → R4 → R5               (5 stages, connection B)
```

**19 stages, 8 goroutine handoffs on the critical path**, and on the server
the two connections' S4/R1 are the *same* G4/G5.

## 6. Serialization points, ranked by how much they narrow the path

These are ordered by how much they *look* like they narrow the path. Three of
them have since been measured and do not — see §9. The list is kept because
the structure is real even where the cost is not, and because that gap is the
whole lesson.

1. **G4/G5 are per endpoint.** Every connection on a server shares one sending
   and one receiving goroutine. A relay saturates both with its own two legs.
   *Measured: no consistent effect (§9.2).*
2. **G1 is per connection and handles both directions.** Send and receive on
   one connection cannot overlap, and receive wins. *Measured: making them
   overlap is worse (§9.1).*
3. **`relayBytes` alternates.** 64 KB is read, then written, then read; the two
   legs never overlap (`server/task_handler.go:1537`). *Measured: decoupling
   them is worse (§9.3).*
4. **One packet per iteration at G1, G2, G3, G4, G5.** No stage batches.
5. **`bufferLimit` = 1 MB per send stream** (`trsf/send_stream.go:80`) — the
   only backpressure knob, and measurement has not shown it binding.

Per-packet *costs* that are not serialization points but scale with the packet
rate, so they grow as the path gets faster:

- ~~A goroutine spawned per inbound packet~~ (`objproto/message.go:117`) —
  **fixed**, objtrsf `a21e7a3`. Worth 26.6% point-to-point and nothing here;
  see §8.
- **Three copies of every payload**: `WriteContext` copies the caller's bytes
  (`send_stream.go:113`), G5 copies each datagram out of its shared buffer
  (`transport/udp.go:76`), and the relay copies again through `ReadDirect` /
  `AppendData`.
- One of these already bit: the bytes-in-flight audit was `O(n²)` in the send
  path and only became visible once the congestion window grew (objtrsf
  `cc75e99`). Expect the same shape from the others — a cost that is harmless
  at a small window and dominant at a large one.

## 7. What is measured, and what is not

**Measured** (2026-08-31, `scripts/netem-lab`, lan profile, 100 MB pushes):

- ~11,650 packets/s, ~16,000 context switches/s across the three processes,
  ~98 µs mean run-queue delay (`perf sched record -a`; `-p PID` reports
  "192% context switch bugs" because scheduling needs a system-wide view).
- No process saturated: 1.45 of 4 cores total, server ~0.38.
- No UDP loss since `8dc15c3`; mutex contention ~4%; raw single-hop TCP over
  the same shaped path 508 MB/s against the harness's ~10 MB/s.
- Throughput on this benchmark spans **1.9x across identical runs**; anything
  below ~25% is not resolvable. Use `netem-lab bench`, never a single push.

**NOT established.** That the stage count is what limits throughput. A batched
send loop cut nothing: it *raised* context switches 5,977 → 8,081/s and left
throughput unchanged — so a 35% swing in switch rate produced no measurable
throughput change, which is evidence against scheduler round-trips being the
binding constraint. The batch also never engaged, because the send channel was
usually empty when G4 woke: **G4 is not backed up, so the queue that is full is
upstream of it.**

## 8. What each layer costs

`netem-lab bench` measures the whole three-process path and resolves ~25%,
which is why the four misses above could be argued at all. objtrsf now carries
a benchmark that takes the same bulk transfer at three rungs, in one process,
so a number belongs to a layer instead of to the sum
(`trsf/throughput_test.go`, objtrsf `17acb0a`):

```
go test ./trsf -run '^$' -bench Throughput -benchtime 1x -count 8
```

| rung | what it adds | median | spread | step |
|---|---|---|---|---|
| `mock` | trsf alone — packets by channel send | 145.9 MB/s | 1.17x | — |
| `udp` | + objproto AES-GCM + a real loopback socket | 69.8 MB/s | 1.82x | **2.09x** |
| `relay` | + a middle endpoint splicing two connections | 27.3 MB/s | 1.26x | **2.56x** |
| harness | + three processes and the application | ~11.8 MB/s | 1.9x | **2.31x** |

32 MB per iteration, 8 runs, MTU 1200/1500 — the same the UDP path runs. The
first three rungs are one process over loopback; the harness row is the
`netem-lab` lan profile (2 ms RTT) and is there for scale, not as a controlled
fourth point.

**The 12.4x gap is three roughly equal multiplicative steps, not one
bottleneck.** That is the result the old instrument could not have produced,
and it retires the question the rest of this document was written to answer.

It does *not* explain the four misses — each of those layers is large enough
that removing one would have cleared even the old 25% floor. What the misses
have in common is narrower and less comfortable: none of them removed a
factor. Raising the relay chunk 64x changed the chunk, not the alternation.
The batched send loop never engaged. A layer stays worth what it is worth
until something actually takes it away.

The per-rung spread of 1.17–1.26x is the other half of the point. A 26.6%
change is now resolvable, and `mock` — which never reaches objproto — doubles
as a control for machine drift on any change below it.

## 9. The three predictions, and what happened to them

All three were tested against the ladder in §8. **None of them survived.**

**1. G1's one-packet-per-iteration — FALSIFIED.** The concern was that the
`continue` after `handlePacket` (§4) skips the whole send half, so a
connection whose receive queue is non-empty emits nothing at all — including
the ACK, since `GenerateACK` lives below that `continue`. A bulk receiver's
queue is essentially never empty, so this predicted starved ACKs. Bounding the
consecutive-receive streak, so the send half runs every N packets:

| streak | mock | udp |
|---|---|---|
| unbounded (as shipped) | 155.1 MB/s | 88.6 MB/s |
| 1 | 50.5 (**−67%**) | 51.6 (**−42%**) |
| 4 | 155.9 (+1%) | 79.2 (−11%) |
| 16 | 158.9 (+2%) | 89.7 (+1%) |
| 64 | 157.8 (+2%) | 86.4 (−3%) |

Sending sooner never helped and hurt badly when forced. **The `continue` is an
optimization, not an oversight** — the send half allocates and encodes an ACK
and pops three queues, which is too much to run per received packet. Read §4's
"receive starves send" as a description, not a defect.

**2. G4/G5 being per-endpoint — NOT ESTABLISHED.** The `relay-2ep` rung gives
the relay's outbound leg its own socket, and so its own sender and reader
goroutine, changing nothing else. Three run sets: +8.1%, −17.7%, −13.7%. The
sign does not hold, so this is a non-result rather than a finding — but
nothing in it supports a second endpoint helping.

**3. `relayBytes` alternating — FALSIFIED in direction.** The `relay-pipelined`
rung decouples the read from the write with a bounded queue. Four run sets:
−17.1%, −1.2%, −8.3%, −19.3%. It never went faster. The alternation is real
and the legs really never overlap, but that is not what a relay costs: it does
the work twice, and the overlap a queue buys is worth less than the handoff it
adds.

Both dead ends are kept as rungs in the benchmark, because the shape of the
code keeps suggesting them.

**What the disagreement between run sets means.** The relay rungs run three
connections and four transports in one process, and they are far more
load-sensitive than the others: while the box was busy they swung 20.6–29.1
MB/s and `mock` (144.7–155.1) and `udp` (87.1–90.6) did not move at all. Two
interleaved 20-run sets then disagreed by 26 points on the same comparison. So
the ±X% a single run's stdev implies understates the real error whenever
anything else is running — check the load before a relay-rung comparison, and
treat a difference under ~25% on a loaded box as no result. `mock` and `udp`
are forgiving; `mock` is also the control for any objproto-side change,
because it never reaches objproto.

**What is left.** Nothing that rearranges who waits for whom. Throughput is
packets per second times bytes per packet, and with the serialization
explanations gone those two terms are the whole remaining space. §10 measures
both.

## 10. Bytes per packet, and work per byte

**Bytes per packet is spent.** `TestUDPPacketOccupancy` (objtrsf `0b476be`)
counts the packet numbers a sender actually consumes: **8,388,608 payload
bytes in 5,784 packets = 1450 bytes/packet at MTU 1500, 97% full**, three runs
agreeing within two packets. An estimate from the application's packet rate
had implied ~860 bytes and therefore real headroom; it was wrong. There is
nothing to reclaim inside a packet, and no MTU to raise on a real path — 1500
is the wire. (The ws/wss path is the exception and already took this lever:
`StreamMTU = 16384` was worth ~3x, harness `ed9b004`.)

**So it is work per byte, and it is large.** Allocation counts are
deterministic, which makes them the one thing measurable on a box that is not
idle. Per 32 MB transferred — about 23,100 packets:

| rung | allocs | bytes allocated | per packet |
|---|---|---|---|
| `mock` | 743,181 | 251 MB | ~32 allocs |
| `udp` | 1,755,812 | 566 MB | **~76 allocs, 24.5 KB** |

24.5 KB allocated to move a 1450-byte packet, ~17x the packet.

An allocation profile named the largest single item: **44.37% of all
allocations were in `objproto.popFromReorderBuf`**, which used
`for i, msg := range` and returned `&msg.msg`, so the loop variable escaped
and a copy was heap-allocated **on every entry scanned** rather than on the
one that matched — ~94 per received packet, one of them delivered. Indexing
instead (objtrsf `8e60e7b`) took udp to 1,354,425 allocs/op, **−22.9%**, with
the `mock` control flat at −0.1%. The alloc-count spread collapsed with it
(1.57M–2.10M before, 1.348M–1.365M after): the variance *was* the reorder
buffer's depth following the machine's load.

That depth is itself a finding. A message that had to take `SendMessage`'s
goroutine path holds up every message behind it, and they queue in the reorder
buffer waiting — where both the scan and the removal are O(n), so a deep
buffer is O(n²). Only the allocation has been addressed.

**Whether any of this moves throughput is unmeasured.** Interleaved A/B over
two test binaries put udp at +3.4% and the control at −1.4%, both inside a
±16% resolution on a busy box. The change landed on its allocation count and
its identical semantics, not on a throughput claim. The next honest step is
the same profile on an idle machine — after `popFromReorderBuf`, the profile's
remaining weight was `sendStream.onACK` (8.7%), `sendStream.triggerPacket`
(7.1%), `Streams.run` (4.3%), and ~4% in `ConnectionID.String` and
`netip.AddrPort.String`, which are logging arguments evaluated on the hot path
even when the logger discards them.
