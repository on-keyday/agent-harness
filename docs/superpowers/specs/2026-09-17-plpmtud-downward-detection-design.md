# PLPMTUD that can go down, and a retransmit that can be re-split — Design

Date: 2026-09-17

Two repositories change together. `objtrsf` gains a downward transition in its
MTU state machine, a wake deadline that makes the machine run at all when the
connection is idle, and a retransmit path that can re-split a chunk that no
longer fits. The harness gains one counter so the new transition is visible.
Both halves are described here, in one place, because a reader who finds only
the harness half cannot tell what the transport now does.

Line numbers into `objtrsf` are against `v0.0.0-20260916160519-cc8d0928a19e`,
the version pinned in `go.mod` today. Harness line numbers are against
`83341ecb`.

---

## 1. Problem

`MTUTracker.mtu` is assigned in exactly one place:

```go
// trsf/mtu/plpmtud.go:102, inside OnACK
if t.lastProbe > t.mtu {
    t.mtu = t.lastProbe
```

`OnLost` lowers `t.high`, the search ceiling, and never touches `t.mtu`
(:114-127). The re-probe after convergence reopens the range upward only,
`t.low = t.mtu + 1` (:71). **The estimate is monotonically non-decreasing and
there is no code path that lowers it.**

Measured on `scripts/netem-lab`, one connection, `--profile lan`:

```bash
scripts/netem-lab/netem-lab.py --name mtu up --profile lan --mtu 1300
scripts/netem-lab/netem-lab.py --name mtu exec cli -- \
  harness-cli --server-cid "$CID" conns --trsf --json
scripts/netem-lab/netem-lab.py --name mtu shape --profile lan --mtu 1100
```

| step | `mtu` on the runner connection |
|---|---|
| converged on a 1300-byte link | **1270** |
| a freshly dialled connection, same lab | 1200 (`DefaultInitialMTU`) |
| 10 s … 60 s after the link narrowed to 1100 | **1270, unchanged** |

The upward search works. The downward case does not exist.

**What that costs is a partial wedge, which is worse than a clean failure.**
With the estimate stale-high, every packet the sender fills to `CurrentMTU`
exceeds the path and is dropped; packets that happen to be smaller still pass.
So the connection keeps answering and keeps failing, depending on the size of
the answer. Measured in the same lab, at a 1100-byte link with a connection
converged at 1270:

| call | result |
|---|---|
| `harness-cli ls` | returns normally |
| `harness-cli conns` | returns normally |
| `harness-cli conns --trsf` | **never returns** |
| the same call after `shape --mtu 1500` | returns immediately |

`conns --trsf` differs from the other two only in how many bytes its answer is.
An operator reading this sees a live server, a live runner, and one subcommand
that hangs.

### 1a. Why the deadline that should notice this never fires

`Probe()` checks the re-probe deadline *inside itself* (:65). Nothing arms a
timer for it. `nextWakeDeadline()` (`trsf/conn.go:653-676`) returns the loss
detection timeout and, conditionally, the pacing timeout — the MTU deadline is
not a candidate. `setLossDetectionTimer` disarms when nothing is in flight, and
the pacer is skipped when `sendTrigger` is empty, so an idle connection parks
with no deadline and `Probe()` is simply never called.

So the existing upward re-probe is already best-effort: it runs when the loop
happens to wake for another reason. On a busy connection that is continuous; on
an idle one it does not happen. Any detector placed in `Probe()` inherits this,
which is fatal for one whose whole job is the idle case.

### 1b. Why lowering the estimate is not sufficient on its own

`trsf/send_stream.go:268`, the retransmit branch:

```go
if popped := r.retransmitQueue.Pop(); popped != nil {
    headerSize := StreamPacketHeaderSize(popped.ID, popped.Offset, len(popped.Data))
    if maxPayload <= headerSize {
        r.retransmitQueue.Push(popped)
    } else {
        return popped
    }
}
```

The guard asks whether the **header** fits, not whether `headerSize +
len(popped.Data)` does. `headerSize` is a handful of bytes, so a chunk captured
at a 1270-byte budget is returned whole into a 1200-byte one.

`maxPayload` is recomputed per call from `CurrentMTU()` (`trsf/conn.go:957`), so
lowering the estimate does immediately tighten the budget for *new* chunks. Data
already in flight would keep being retransmitted at its original size forever.

**This guard is correct today.** Under an estimate that never decreases,
`len(Data)` cannot exceed a later budget. This design is what makes it wrong, so
it changes in the same commit range.

Datagrams need none of this: nothing retransmits them, and `MaxDatagramSize()`
reads `CurrentMTU()` on every call (`trsf/conn.go:1091`).

## 2. Non-goals

`Decided-by` in §3 says who chose. This section says what a later reader must
not read into the absence of something.

- **Making a path below BASE work is not in scope.** RFC 9000 §14 is explicit
  that this is not a transport's job: "QUIC MUST NOT be used if the network path
  cannot support a maximum datagram size of at least 1200 bytes." §7 says what
  we do instead, which is report it.
- **Endpoint fragmentation is not built.** RFC 8899 permits a PL to fall back to
  IP fragmentation below MIN_PLPMTU. Nothing here does.

  This is not what §5e does, and the two must not be read as the same thing. A
  fragment is a piece of a datagram that means nothing until its siblings
  arrive and be reassembled. §5e re-splits *stream bytes*, which carry their own
  offset and are already delivered and ordered by offset — the receiver cannot
  tell a re-split retransmission from data that was sent in those sizes to begin
  with. The datagram path, which genuinely has no framing to re-split, stays
  exactly as the datagram spec left it: an oversized payload is dropped and
  counted.
- **Speeding up the climb after a path WIDENS is not in scope.** A path that
  grows without ever black-holing produces no signal, so the only way up stays
  the existing re-probe timer, whose backoff reaches `30s * 2^6` ≈ 32 min. This
  is untouched. It does NOT describe the recovery after a fallback, which §5c
  shows is immediate.
- **Connection migration is untouched.** A client that changes address is a
  different problem; this design only covers a path whose MTU changes under a
  connection that stays put.
- **Stream transports are excluded by construction, not by a predicate.** §5a
  says how.

## 3. Decisions taken

`Decided-by` is provenance, not emphasis. Only rows marked `operator` were
chosen by the operator; the rest are this document's author's and a later reader
may overturn them on evidence.

| # | Decision | Decided-by |
|---|---|---|
| 1 | Lift the "path MTU does not vary during a connection" simplification the transport was built under | operator |
| 2 | The bar is "does not wedge silently", not a complete RFC 8899 state machine | operator |
| 3 | Data-packet loss during a transfer is a trigger, not only a probe | operator |
| 4 | A validation probe is ALSO kept, because it is the only detector that works when nothing is being sent | operator |
| 5 | The verdict lives in `MTUTracker`; both detectors feed one entry | operator |
| 6 | On a verdict, fall back to BASE and let the existing upward search climb | operator |
| 7 | A path below BASE is out of scope to FIX and in scope to REPORT | operator |
| 8 | The state machine must stay correct for any configured `(min, max)`, including `min` below 1200 — that number is a caller's default, not an algorithmic constant | operator |
| 9 | "Large" means `size > min`: exactly the set a shrunk path can drop and BASE cannot | author |
| 10 | The data-loss verdict is time-based, not a consecutive-loss count | author |
| 10a | That time derives from the RTT estimate rather than being a constant | operator |
| 11 | The MTU deadline joins `nextWakeDeadline`, gated on `probeSent` and on `min < max` | author |
| 12 | A too-large retransmit is SPLIT, not discarded and not stalled | author |

## 4. Wire changes — all of them, in one place

### 4a. `objtrsf`

None. No new packet kind, no new field. A validation probe is an ordinary MTU
probe; a re-split retransmit is ordinary stream data carrying an offset the
receiver already handles. `wire/stream.bgn` is untouched.

### 4b. Harness — `runner/protocol/message.bgn`

Appended to `TrsfCounterKey` (:1907 is where the datagram block was appended the
same way; existing ordinals unchanged, so an older runner omits these rather
than reporting zeros):

```
    # The MTU estimate fell. Non-zero means this connection met a path that
    # stopped carrying a size it had already proven, which is the one thing
    # the search alone cannot discover. A fleet-wide rise means the detector
    # is firing on congestion; zero everywhere means it is not firing at all,
    # and those two are indistinguishable without this counter.
    mtu_fallbacks = "mtu_fallbacks"

    # How many times the search collapsed with BASE itself still being lost.
    # Nothing below BASE is attempted (RFC 9000 s14 -- "QUIC MUST NOT be used
    # if the network path cannot support a maximum datagram size of at least
    # 1200 bytes"), so this is the row that explains a connection answering
    # small calls and hanging on large ones.
    #
    # A COUNT, like every other member here, so it reads "this happened N
    # times on this connection" and never "this is true now". A path that
    # recovers leaves the count standing; pair it with mtu on the same row to
    # tell a connection that is currently stuck from one that was.
    mtu_base_unusable = "mtu_base_unusable"
```

There is no disk axis. `server/wal.go` persists wire bytes for `RunnerSelector`
only; trsf counters are sampled live.

## 5. objtrsf — the changes

### 5a. The deadline, and what may set it

`MTUTracker` gains `NextDeadline() (time.Time, bool)`, and
`Streams.nextWakeDeadline()` takes it as a third candidate beside the loss timer
and the pacer.

Two gates, and both exist to avoid a known failure of this loop rather than for
tidiness:

- **No deadline while `probeSent` is true.** `Probe()` returns -1 for the whole
  time a probe is outstanding, so a deadline in that window is a past timestamp
  the loop wakes on, cannot act on, and immediately sees again — the 0-delay
  spin documented at `trsf/conn.go:655-668`, which on the single-threaded wasm
  runtime starves the JS event loop. The outstanding probe is already covered by
  the loss detection timer.
- **No deadline when `min == max`.** `peer.MTUForTransport` (`peer/conn.go:170`)
  returns `(StreamMTU, StreamMTU)` for `ws`/`wss`, so this is exactly the set of
  stream transports. Their "MTU" is a framing choice, not a path property:
  nothing to discover and nothing that can shrink. This keeps the blast radius
  of "idle connections now wake periodically" on UDP connections only. It is the
  construction §2 refers to — no predicate names WebSocket anywhere.

`Probe()`'s early return at `:68-70`, `if t.mtu >= t.max { return -1 }`, changes
meaning from "stop probing" to "stop searching UP". A connection converged at
the ceiling must still validate, or the ceiling is the one place a shrink can
never be noticed. The `min < max` gate runs first, so `ws` never reaches it.

### 5b. The verdict

Two detectors, one entry. `MTUTracker` gains:

```go
OnLargePacketACKed(size int, now time.Time)
OnLargePacketLost(size int, now time.Time)
```

called from the existing per-packet `OnACK`/`OnLost` callbacks for non-probe
packets — the same plumbing MTU probes already use, so `ack_handler.go` gains no
new concept.

**Large means `size > min`.** That is the set a shrunk path can drop while BASE
still passes, so the boundary of the signal is the boundary of the remedy.

**The data-loss verdict is time-based.** A consecutive-loss count cannot work:
congestion drops whole bursts, so three in a row is an ordinary Tuesday. What
distinguishes a black hole is that *nothing* large gets through while the
connection is otherwise alive:

```
converged
  AND at least one large packet has been declared lost
  AND no large packet has been ACKed for T
  AND some ACK has arrived within T
```

The second clause is required because a period during which nothing large was
*offered* would otherwise fire. The fourth is required because a dead connection
would otherwise fire; falling back there is harmless but the verdict would stop
meaning what it is named.

**T is derived from the RTT estimate, not fixed.** What T has to outlast is a
congestion episode, and an episode is measured in round trips — a constant is
either far too long on a LAN or far too short on a satellite path.

```
T = clamp(k * srtt, floor, cap)
```

srtt is well defined at exactly the moment this verdict is evaluated, which is
not obvious and is why this works: the fourth clause above already requires that
ACKs are arriving, so the RTT estimate is live and being updated by the small
packets that are still getting through. A detector that fired on a silent
connection could not rely on that.

**PTO would be the wrong unit** even though it already folds in RTT variance.
It backs off exponentially under sustained loss, which is precisely the state
this verdict is trying to recognise, so `N * PTO` stretches T as the black hole
persists and pushes detection away exactly when it is needed.

`mtu` does not import `congestion`; the tracker takes a `func() time.Duration`
supplied by `Streams`, which already holds the RTT stats it passes to NewReno
(`trsf/conn.go:1289`).

Three numbers, all author picks and all calibratable by §10's lossy and
bufferbloat arms rather than by argument: **k = 10**, **floor = 1 s**,
**cap = 30 s**. The floor exists because `k * srtt` on loopback is microseconds;
the cap bounds worst-case detection latency on a very slow path.

The validation probe reuses the existing `OnLost`. **Its loss counter must be
separate from the search counter.** Sharing `lossCount` means two lost search
probes followed by one lost validation trips the three-strike rule, and that
mixed sequence is ordinary.

### 5c. What the verdict does

One named method, and the only place in the module where the estimate decreases:

```
mtu = min;  low = min + 1;  high = max
lastProbe = mtu;  probeSent = false          # see below
reset the loss counters and the black-hole timestamps
onMTUUpdate(mtu)
mtu_fallbacks++
```

**The `lastProbe` line is load-bearing and its absence is a silent
regression.** `OnACK` raises the estimate on `t.lastProbe > t.mtu`
(`plpmtud.go:102`) and reads `lastProbe` unconditionally. A probe issued at,
say, 1327 before the fallback can be acknowledged *after* it — the ACK is in
flight while the verdict fires — and would then set `mtu = 1327`, restoring
precisely the estimate the fallback just ruled out, with `onMTUUpdate`
announcing it as an increase. Re-pointing `lastProbe` at the new `mtu` makes
that late ACK a no-op, and clearing `probeSent` lets the next `Probe()` issue a
fresh one rather than waiting on a probe whose verdict no longer applies.

The same reasoning covers a late `OnLost` for that stale probe: it must not
lower `high` below the range the fallback just opened. Deriving both from the
post-fallback `lastProbe` handles it without a separate epoch counter.

Because it sets `low <= high`, the machine is back in the searching state, and
`Probe()`'s backoff gate at `:64` is not consulted at all — **the climb back up
starts on the next loop pass.** Roughly `log2(max-min)` search steps, each
costing three lost probes when it fails, so tens of seconds; `mtu` sits at BASE
for that time. `OnACK` resets `reprobeBackoffCount` on the first increase, so
the post-recovery state is the same as a fresh convergence.

Keeping the decrement in one named method is the point. The monotonicity of
`t.mtu` is currently what makes this file readable, and a grep that finds one
assignment is what replaces it.

**The fallback does nothing about data already queued for retransmission, and
does not need to.** Those chunks were captured at the old budget and are the
reason §5e exists: each is re-split against the current budget when it is
popped. A fallback that also tried to rewrite the queue would be doing the
splitter's job in a second place, at a moment when `maxPayload` is about to
change again as the search climbs.

### 5d. Below BASE: report, do not fix

If the search collapses — `high` walks down to `min` and `mtu == min` while BASE
itself is still being lost — the path cannot carry what RFC 9000 §14.2 calls the
smallest allowed maximum datagram size. That section's answer is to stop, not to
try harder:

> If a QUIC endpoint determines that the PMTU between any pair of local and
> remote IP addresses cannot support the smallest allowed maximum datagram size
> of 1200 bytes, it MUST immediately cease sending QUIC packets, except for
> those in PMTU probes or those containing CONNECTION_CLOSE frames, on the
> affected path.

This design implements the **observable** half only: set `mtu_base_unusable` and
surface it. Ceasing transmission and terminating the connection are a separate
decision with a blast radius over every stream on the connection, and are not
taken here.

The value of the observable half is precise: it converts the §1 symptom from
"one subcommand hangs and nothing says why" into a row that names the cause.

`DefaultInitialMTU = 1200` and `DefaultMaxMTU = 1500 - 48` (`trsf/conn.go:1228`)
are QUIC's numbers arrived at the same way — 1280 is the IPv6 minimum link MTU,
less 40 + 8 gives 1232, and the 32 bytes down to 1200 are RFC 9000 §14's stated
reservation for IPv6 extension headers. **They are the caller's defaults, not
constants of the algorithm** (decision 8); `NewStreams` takes both, and the
comment at `:1228` already invites an IPv4-only deployment to pass 1472.

### 5e. Re-splitting a retransmit

`triggerPacket`'s retransmit branch splits a popped range that no longer fits
rather than returning it whole:

- `n = maxPayload - headerSize`
- head: `Data[:n]` at `Offset`, `Eof: false`
- tail: `Data[n:]` at `Offset + n`, `Eof: popped.Eof`, pushed back on the queue
- return head

Three obligations, each of which is a silent defect if missed:

1. **`Eof` rides the tail only.** On the head it ends the stream at the head's
   last offset and the remainder is never delivered.
2. **`headerSize` is recomputed for each fragment.** The length field is a
   varint, so a shorter fragment can need a narrower one. The comment directly
   below this branch records what a fixed reservation cost the last time a chunk
   crossed a varint boundary.
3. **`sentRanges` is matched by pointer identity** (`send_stream.go:379`,
   `:387`). The original `popped` is still in `sentRanges` — it is removed on
   ACK, not on loss — so splitting must replace that one entry with the two new
   ranges, each carrying its own `OnACK`/`OnLost`. Leaving the original behind
   retires data that was never acknowledged; appending without removing degrades
   the O(1) head path into the O(n) filter that `df7d04b` exists to avoid.

## 6. Harness — the changes

`go.mod` bump, and the two `TrsfCounterKey` members of §4b carried through
`runner/protocol/trsf_row.go` the way `CurrentMTU` already is (`:27`).

Nothing else. `MaxDatagramSize()` is read live from `CurrentMTU()`, so the udp
port forward's `max_datagram_size` follows a fallback with no change on the
harness side — including the `forward ls` row and its `mtu=` rendering.

## 7. Operator surface

`conns --trsf --json` carries every counter through `ObserveJSON`, so both new
members are reachable the day the runner reports them. The text table's column
choice is deliberate curation and is not changed — the same call that was made
when the nine datagram counters landed.

Two readings this is for, and neither is possible today:

| reading | what it means |
|---|---|
| `mtu_fallbacks` rising across many connections | the detector is firing on congestion; T or the "large" boundary is wrong |
| `mtu_base_unusable` non-zero **and** `mtu` still at BASE | this path cannot carry BASE right now. Nothing below is attempted, by design |
| `mtu_base_unusable` non-zero **and** `mtu` above BASE | it happened and the path recovered. The count does not clear |

**Read both endpoints.** The previous PLPMTUD defect showed only on the
receive-dominated side and was hidden by dumping the server alone. An
acceptance check reads `conns --trsf --runner <cid> --json` as well.

## 8. Rollout

1. **objtrsf**: the tracker's verdict and fallback, `NextDeadline` and its two
   gates, the `nextWakeDeadline` wiring, the retransmit split. Landed on trunk
   by fast-forward per the recorded policy.
2. **harness**: `go.mod` bump, the two counter members, `trsf_row.go`.

The harness step touches `message.bgn`, so `scripts/wire-skew-check.sh` runs
unconditionally. The change is an append to a keyed list with existing ordinals
unchanged, which is the shape that lets an older runner omit the members instead
of reporting zeros. `objtrsf` introduces no wire change at all (§4a), so the
first step has no skew to check.

## 9. What could go wrong

- **The detector fires on congestion.** The cost is a fallback to BASE and tens
  of seconds of re-search. It is worse here than on most stacks: the binding
  constraint measured on this transport is the packet rate, not the byte rate,
  so a smaller MTU is a near-proportional throughput cut for that window.
  `mtu_fallbacks` is what makes a fleet-wide false-positive rate visible; the
  unit tests in §10 pin the negative cases, and its `lossy` and `bufferbloat`
  arms are what set `k` rather than leaving it a guess.
- **Idle connections now wake periodically.** Bounded to UDP connections by the
  `min == max` gate. The cost is one timer and one probe packet per interval per
  UDP connection.
- **The new deadline busy-spins.** The `probeSent` gate is what prevents it, and
  it is the same failure the pacer's own comment documents. A unit test asserts
  `NextDeadline` reports nothing while a probe is outstanding.
- **A split retransmit corrupts a stream.** The three obligations in §5e are
  each testable without a network, and each fails silently in production.
- **The `min == max` gate is written as a predicate on transport name instead.**
  That would be the shape the repo's own `ClientEndpointKind` comment warns
  about. The gate is on the values because the values are what make the question
  meaningless.

## 10. Testing

**The tracker, as a pure state machine** (`trsf/mtu/plpmtud_test.go`). It takes
`now` as a parameter and performs no I/O, so everything about the verdict
belongs here and runs in milliseconds:

- a healthy path **never** falls back, over a long simulated run — the
  false-positive guard, and the most important test in the file;
- large lost, no large ACK for T, some ACK within T → fallback to `min`, search
  reopened;
- large lost but a large ACK inside T → no fallback;
- no ACK at all → no fallback;
- search-probe losses and validation-probe losses do not share a counter;
- `NextDeadline` reports nothing while `probeSent`, and nothing when
  `min == max`;
- `mtu >= max` still validates and no longer searches up;
- **a table over `(min, max)` pairs, including `min` below 1200, `min == max`,
  and `min + 1 == max`.** Per decision 8 the algorithm's correctness must not
  depend on the caller's defaults: the estimate stays within `[min, max]`, the
  search terminates, a fallback lands on `min`, and no configuration produces a
  past-timestamp deadline;
- **a probe issued before a fallback, acknowledged after it, does not raise the
  estimate** — the §5c race. Its `OnLost` twin must likewise not lower `high`
  below the reopened range;
- **two fallbacks in a row** — a verdict reached while already at `min` leaves a
  consistent state rather than an inverted range.

**The split, as a unit test** (`send_stream`). Also no network:

- a range queued at budget B1 popped at B2 < B1 yields two ranges that each fit
  B2, with contiguous offsets and `Eof` on the tail alone;
- `sentRanges` holds the two new pointers and not the original, and ACKing both
  retires them through the O(1) head path;
- a range whose length crosses a varint boundary gets a recomputed header;
- **the tail splits again** when the budget drops a second time before it is
  sent. Splitting is not a one-shot;
- **a budget too small for `header + 1` byte pushes back instead of splitting.**
  That is the original guard's job and it has to survive the rewrite: `n <= 0`
  must never produce an empty range, which would consume a queue slot and
  advance nothing;
- **a range carrying only `Eof` with no data is not split.** A head with no
  bytes and no EOF advances nothing and the tail is the same range again —
  the shape that turns the retransmit queue into a spin;
- **splitting does not double-count flow control.** The original send already
  consumed the window; two ranges where there was one must not consume it twice.

**The harness surface needs no new test, and that is worth stating rather than
assuming.** `TestTrsfRowFromCarriesEveryNumberInInternalState`
(`runner/protocol/trsf_row_test.go:57`) already fails, naming each one, for any
`InternalState` number a row does not carry — it is how the nine datagram
counters were caught in `9f4472dc`. The two members of §4b are covered by it the
moment `objtrsf` exposes them. What does have to be run is
`scripts/wire-skew-check.sh`, unconditionally, because `message.bgn` changes.

**Regression on the rungs.** `mock` is **not** a free control here: unlike the
datagram frame, this change is inside `trsf` itself, including the run loop's
wake computation, so both rungs can legitimately move. Build two test binaries
and alternate them, **ABBA and BAAB in turn so each arm takes every position
equally**. Plain ABBA is not sufficient — measured 2026-09-17 on this box,
position medians were U-shaped and ABBA hands both middle slots to one arm,
producing a −5.8% that reversed under the balanced order.

```bash
go test -c ./trsf -o /tmp/trsf-BEFORE.test   # at the base commit
go test -c ./trsf -o /tmp/trsf-AFTER.test
./trsf-{ARM}.test -test.run '^$' -test.bench 'Throughput/(mock|udp)$' \
  -test.benchtime 1x -test.count 1
```

The idle-wake cost does not show in throughput. Measure it as CPU on a lab with
established connections and no traffic.

**End to end, on `scripts/netem-lab`.** Three arms, because they exercise
different halves:

| arm | setup | expected |
|---|---|---|
| shrink, above BASE | `up --profile lan --mtu 1300` then `shape --mtu 1260` | `mtu` falls to 1200 and re-converges near 1230; `conns --trsf` answers throughout |
| shrink, idle | the same, with no traffic offered at all | the same — this is the only arm that exercises §5a, and the only one that would still pass if the wake deadline were forgotten |
| shrink, below BASE | `up --profile lan --mtu 1300` then `shape --mtu 1100` | `mtu_base_unusable` non-zero. The connection is NOT repaired: 1100 cannot carry BASE, and §2 says so |
| **no shrink, lossy** | `up --profile lossy` (1 % independent loss), bulk traffic, MTU never touched | `mtu_fallbacks` stays **0** |
| **no shrink, bufferbloat** | `up --profile bufferbloat` (20 mbit, 2000-packet queue), bulk traffic, MTU never touched | `mtu_fallbacks` stays **0** |
| **idle ws** | `up --transport ws`, connections established, no traffic | the loop's iteration count does not rise — the `min == max` gate, checked in place rather than only in a unit test |

Add `--pmtu-blackhole` to each shrink arm. With ICMP delivered the local kernel
learns the new size first and can mask the effect; the black-hole variant is the
one that tests PLPMTUD rather than the kernel's PMTU cache.

**The two no-shrink arms are what calibrate `k`, `floor` and `cap`**, and they
are the reason those numbers are not settled by argument in §5b. `lossy` and
`bufferbloat` are the two shapes that can make a healthy path lose large packets
in bursts, which is the whole false-positive hazard; `lossy` models link
corruption and `bufferbloat` models a queue the sender fills itself, and
netem-lab's README is explicit that a congestion controller measured only
against the first is measured against the wrong thing. If either arm produces a
fallback, `k` is too small.

The below-BASE arm is the reproduction that opened this investigation. Its
expected outcome changes from "hangs" to "says why", and that is the whole of
what §5d claims.

---

## Amendment — 2026-09-17, written from the implementation and the lab

### The numbers this shipped with

Measured on `scripts/netem-lab`, harness `5409a64f`, objtrsf `82f3e6c`. The
false-positive arms never touch the MTU; they exist to set `k`.

| arm | what the path does | `mtu_fallbacks` |
|---|---|---|
| `--profile lossy` (1 %, RTT 150 ms) | link corruption | **0** |
| `--profile bufferbloat` (20 mbit, 2000-packet queue) | queueing, `tc dropped 0` | **0** |
| `--profile thin` (2 mbit, 100-packet queue) | **self-inflicted overflow, `tc dropped 0 → 54`** | **0** |
| `--profile lan --mtu 1300` then `--mtu 1260`, with traffic | an MTU black hole | **1** |

`k = 10`, `floor = 1 s`, `cap = 30 s` stand. The shrink arm detected, fell to
the base, re-searched and settled at **1230** inside the first five-second
sample — 1230 is the arithmetic answer for a 1260-byte link (less IPv4 20 and
UDP 8) — and the transfer that ran across it measured 22.8 MB/s against 21.2
before the shrink, i.e. unchanged. Before this change the same lab held 1270
for sixty seconds and `conns --trsf` never returned.

**`bufferbloat` is a weaker arm than it looks and `thin` is the real one.**
`tc -s qdisc` read `dropped 0` through the whole bufferbloat run: a 2000-packet
queue at 20 mbit delays rather than overflows, so that arm exercises congestion
DELAY and not congestion LOSS. `thin`'s 100-packet queue is what actually drops.
Reshaping into `thin` from another htb profile fails with "Change operation not
supported by specified qdisc" — the limit cannot be changed in place, so that
arm needs its own lab.

### §5d is wrong: the counter cannot be read in the state it describes

`mtu_base_unusable` was to make a below-BASE path visible instead of mysterious.
Measured at `--mtu 1100`, it does not:

| call | result |
|---|---|
| `harness-cli ls` | returns |
| `harness-cli conns` | returns, runner still registered, age 17m |
| `harness-cli conns --trsf` | **never returns** |

Which is the symptom §1 opened with, unchanged — as §2 said the behaviour would
be. What does not hold is §5d's claim that we would at least report it. The
counter rides `conns --trsf`, whose answer is a stream packetised at
`CurrentMTU`, and at a path below BASE that is exactly what cannot cross. **The
one row that explains the failure is only readable on a connection that is not
failing.**

A fix has to put the signal somewhere that crosses, or somewhere that needs no
network at all. The cheapest is the second: log it where it is detected, on
whichever side detects it, so the server's own log carries it. That is not built
here and §5d should be read as describing an intent the measurement refuted.

### Three things the implementation changed, all found by running the tests

- **`OnLost` ignores a probe at or below the current estimate.** §5c said
  re-pointing `lastProbe` covered the late-ACK and late-loss races together. It
  covers the ACK; the loss needed this guard as well, or a probe outstanding
  across a fallback sets `high = min-1` and inverts the range the fallback just
  reopened.
- **The liveness signal is any packet RECEIVED, not an ACK of ours.** §5b's
  fourth clause was written as "some ACK has arrived", and on a bulk transfer in
  a black hole nothing of ours is acknowledged at all — the clause would be
  unsatisfiable on exactly the connection the verdict is for. `OnPeerActivity`
  is called from `handlePacket`.
- **`NextDeadline` stays silent while SEARCHING, not only while a probe is
  outstanding.** §5a named one gate; returning "now" for a pending search probe
  handed the run loop a past deadline every iteration and broke
  `TestNextWakeDeadlineNoSpinNothingToSend`, which is the guard that exists for
  it. A search probe is issued by the next pass through the send half anyway;
  the timer is for the converged connection, which has no other reason to wake.

`srtt` arrives through `OnSRTT` rather than a fourth constructor parameter.
`NewMTUTracker` has twelve call sites, eleven of them tests that do not care,
and `blackHoleTimeout` has to handle the unset case regardless.
