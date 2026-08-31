# Every stage one packet passes through

A complete enumeration of the goroutines, queues and serialization points a
single packet crosses between the application's write call and the peer
application's read call, over the UDP underlay.

**Why this exists.** On 2026-08-31 five hypotheses were tried against the
transport's throughput and one was right. The four misses were all local
changes proposed from a profile — a loss detector, a relay chunk size, a send
buffer, a batched send loop — and each was killed by measurement after being
written. The common fault was reasoning one step ahead of the structure. This
document is the structure, so the sixth attempt argues from it instead of from
a hunch.

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
synchronous (R1). The design intent is visible and reasonable — the
non-spawning `SendMessageBlocking` sits right beside it at
`objproto/message.go:139`, so the spawn is there to keep a full channel from
blocking the socket reader. It is an expensive way to buy that: a bounded
worker, or a buffered channel with an explicit backpressure policy, costs no
allocation per packet.

Unquantified. It is consistent with the 16% `mallocgc` and 10% GC seen on the
server's CPU profile, and with the scheduler weight (`park_m`, `findRunnable`,
`schedule` each ~14–16%), but nothing has isolated it.

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

1. **G4/G5 are per endpoint.** Every connection on a server shares one sending
   and one receiving goroutine. A relay saturates both with its own two legs.
2. **G1 is per connection and handles both directions.** Send and receive on
   one connection cannot overlap, and receive wins.
3. **`relayBytes` alternates.** 64 KB is read, then written, then read; the two
   legs never overlap (`server/task_handler.go:1537`).
4. **One packet per iteration at G1, G2, G3, G4, G5.** No stage batches.
5. **`bufferLimit` = 1 MB per send stream** (`trsf/send_stream.go:80`) — the
   only backpressure knob, and measurement has not shown it binding.

Per-packet *costs* that are not serialization points but scale with the packet
rate, so they grow as the path gets faster:

- **A goroutine spawned per inbound packet** (`objproto/message.go:117`).
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

## 8. What this predicts

Each is falsifiable with `netem-lab bench --runs 8` before and after, which
resolves ~25%:

- If **G1's one-packet-per-iteration** binds, letting it drain N received
  packets per iteration should scale throughput with N until another stage
  takes over. If it does not, G1 is not the constraint.
- If **G4/G5 being per-endpoint** binds, a second endpoint (or a per-connection
  socket) should help a relaying server and do nothing for a single connection.
  That asymmetry is the test.
- If **`relayBytes` alternating** binds, decoupling its read and write with a
  bounded queue should approach the sum of the two legs rather than their
  serialization. Raising its chunk size 64 KB → 4 MB did *not* help, which
  already weakens this one.

The cheapest of the three to falsify is the second, because it predicts a
*difference between two cases* rather than an improvement, and a difference
survives the noise floor better than a ratio.
