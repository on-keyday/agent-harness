# trsf datagram frame — Implementation Plan (objtrsf half)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give `trsf` an unreliable frame that is acknowledged and — in one of its two modes — congestion controlled, so a tunnel can ride it without either retransmitting stale datagrams or ignoring the window.

**Architecture:** A new transport-owned `ApplicationPayloadKind` decoded by `StreamAppPacket` and handled in the run loop, so its packet number reaches the receiver's `PacketNumTracker` and it gets acknowledged like stream data. Not retransmitting is expressed by the packet's own `OnLost` callback rather than by new machinery. The two congestion modes come from decomposing `SentPacket.IsMTUProbe`, which today fuses three independent properties.

**Tech Stack:** Go 1.25, `objtrsf` (`trsf`, `trsf/wire`, `trsf/congestion`, `trsf/mtu`), `.bgn` schemas regenerated with ebm2go via the harness's `scripts/protoregen.sh`.

**Spec:** `docs/superpowers/specs/2026-09-17-trsf-datagram-udp-forward-design.md` (in the harness repo) — §4a, §5a–§5f, §10, §11.

**Scope note:** This plan covers the **objtrsf half only**. The harness half (schema, routes, flow tables, operator surfaces) is a second plan, written after this one lands, because its task bodies must quote signatures that do not exist until Tasks 4 and 5 are done.

## Global Constraints

- **Repository:** all work is in `/home/kforfk/workspace/objtrsf`, NOT in the harness worktree. That checkout is on `main` and clean at `7db84f6`; cut a branch before the first commit.
- **objtrsf has no Makefile and no regen script.** Verification is `go build ./... && go vet ./... && go test ./...` run from the objtrsf root. Regeneration uses the harness's `scripts/protoregen.sh <abs path to .bgn>`, which writes `<name>.go` beside the input.
- **Regen churn is a constant of ebm2go.** Any added `fn`, `format`, enum arm, or field renumbers global temp counters, so the generated `.go` rewrites in bulk. Do not investigate it and do not stage any ritual around it. What is checked is the ordinary thing: build / vet / test green, plus the new symbols exist with the expected values.
- **Do not change `trsf/throughput_test.go`.** Its `mock` rung never reaches objproto and is the free control: it must not move for anything in this plan.
- **Naming:** `CongestionExempt`, `PathEvidence`, `IsMTUProbe`, `SendDatagram`, `SendDatagramUncontrolled`, `MaxDatagramSize`, `ReceiveDatagram`, `ApplicationPayloadKind.datagram`. Later tasks depend on these exact spellings.
- **No cross-version compatibility shims.** Both ends of this transport are rebuilt together; a version skew is expected to fail, and the requirement is only that it fails recoverably.

---

## File Structure

| File | Responsibility | Task |
|---|---|---|
| `trsf/ack_handler.go` (modify) | `SentPacket` fields; every accounting decision that reads them | 1, 2 |
| `trsf/conn.go` (modify) | the MTU-probe construction site; the run loop; `InternalState`; send/receive of datagrams | 1, 4, 5 |
| `trsf/datagram_accounting_test.go` (create) | the accounting contract of an exempt packet, independent of the wire | 1, 2 |
| `trsf/wire/stream.bgn` (modify) | the `datagram` kind, `DatagramPacket`, the union arm, the routing predicate | 3 |
| `trsf/wire/stream.go` (regenerate) | generated decoder | 3 |
| `trsf/wire/datagram_wire_test.go` (create) | encode/decode round trip for the new arm | 3 |
| `trsf/api.go` (modify) | `AutoReceive` routing; the `Transport` interface | 4, 5 |
| `trsf/datagram_test.go` (create) | end-to-end datagram behaviour over a `Streams` pair | 4, 5 |

---

## Task 1: Decompose the fused `IsMTUProbe` flag

`SentPacket.IsMTUProbe` currently decides three unrelated things. An uncontrolled datagram needs "exempt from congestion control" **without** "not evidence about the path" — a combination the single bool cannot express. This task adds the fields and fixes the two accounting sites that would otherwise break, with no new packet kind and no behaviour change for MTU probes.

Note `trsf/mtu_probe_deadlock_test.go` and its comment: the `mtuProbesOutstanding == 0` term in `setLossDetectionTimer` exists **because** a lone probe otherwise armed no timer and its loss could never be detected. An exempt datagram falls into that same hole, which is what Step 1's test pins.

**Files:**
- Modify: `/home/kforfk/workspace/objtrsf/trsf/ack_handler.go`
- Modify: `/home/kforfk/workspace/objtrsf/trsf/conn.go:908` (the probe construction site)
- Test: `/home/kforfk/workspace/objtrsf/trsf/datagram_accounting_test.go` (create)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `SentPacket.CongestionExempt bool`, `SentPacket.PathEvidence bool`. `IsMTUProbe bool` survives with its meaning narrowed to probe bookkeeping only. Task 5 sets `CongestionExempt` and `PathEvidence` when it builds a datagram's `SentPacket`.

- [ ] **Step 1: Cut a branch**

```bash
cd /home/kforfk/workspace/objtrsf
git checkout -b feat/datagram-frame
git log --oneline -1   # expect 7db84f6
```

- [ ] **Step 2: Write the failing tests**

Create `/home/kforfk/workspace/objtrsf/trsf/datagram_accounting_test.go`:

```go
package trsf

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/on-keyday/objtrsf/trsf/congestion"
	"github.com/on-keyday/objtrsf/trsf/mtu"
	"github.com/on-keyday/objtrsf/trsf/wire"
)

// newExemptOnlyHandler puts one congestion-exempt packet in flight and nothing
// else. Kind is StreamData deliberately: the accounting under test is driven by
// the flags, not by the wire kind, so this test does not wait on Task 3.
func newExemptOnlyHandler(t *testing.T) (*SentPacketHandler, *int) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	rtt := congestion.NewRTTStats(333 * time.Millisecond)
	tracker := mtu.NewMTUTracker(1200, 1500, 30*time.Second)
	cong := congestion.NewNewReno(tracker, rtt, logger)
	sh := NewSentPacketHandler(logger, rtt, cong)

	lost := new(int)
	sh.OnSent(&SentPacket{
		PacketNumber:     0,
		PacketSize:       1000,
		SentTime:         time.Now(),
		CongestionExempt: true,
		PathEvidence:     true,
		Kind:             wire.ApplicationPayloadKind_StreamData,
		OnLost:           func(now time.Time) { *lost++ },
	})
	return sh, lost
}

// An exempt packet is in neither bytesInFlight nor mtuProbesOutstanding, so the
// pre-existing disarm condition in setLossDetectionTimer would park the loop
// with no deadline and this packet's loss would never be detected. That is the
// same hole 7db84f6 closed for a lone MTU probe.
func TestLoneExemptPacketArmsLossTimer(t *testing.T) {
	sh, _ := newExemptOnlyHandler(t)
	if lt := sh.LossDetectionTimeout(); lt.IsZero() {
		t.Fatal("a lone congestion-exempt packet armed no loss timer: its OnLost can never fire, so nothing can count the drop")
	}
}

// Exempt means "does not feed congestion control". It does NOT mean "invisible":
// a datagram's loss is the only evidence distinguishing a path drop from one of
// our own, so it must reach loss.Packets while leaving the window alone.
func TestExemptPacketLossIsEvidenceButNotCongestion(t *testing.T) {
	sh, lost := newExemptOnlyHandler(t)
	_, _, cwndBefore, _, _ := sh.GetInternal()

	now := time.Now().Add(10 * time.Second)
	if lt := sh.LossDetectionTimeout(); !lt.IsZero() && now.After(lt) {
		if _, err := sh.OnTimeout(now); err != nil {
			t.Fatalf("OnTimeout: %v", err)
		}
	}

	if *lost != 1 {
		t.Fatalf("OnLost fired %d times, want 1", *lost)
	}
	_, inFlight, cwndAfter, _, ls := sh.GetInternal()
	if ls.Packets != 1 {
		t.Errorf("loss.Packets = %d, want 1 (a datagram's loss IS path evidence)", ls.Packets)
	}
	if ls.Events != 0 {
		t.Errorf("loss.Events = %d, want 0 (an exempt packet must not trigger a congestion response)", ls.Events)
	}
	if cwndAfter != cwndBefore {
		t.Errorf("cwnd moved %d -> %d on an exempt packet's loss", cwndBefore, cwndAfter)
	}
	if inFlight != 0 {
		t.Errorf("bytesInFlight = %d, want 0", inFlight)
	}
}

// The mirror of the above on the ACK path. An exempt packet never entered
// bytesInFlight, so retiring it must not subtract from it.
func TestExemptPacketAckDoesNotUnderflowBytesInFlight(t *testing.T) {
	sh, _ := newExemptOnlyHandler(t)
	if err := sh.ReceiveACK(time.Now(), []Range{{Begin: 0, End: 1}}, 0); err != nil {
		t.Fatalf("ReceiveACK: %v", err)
	}
	if _, inFlight, _, _, _ := sh.GetInternal(); inFlight != 0 {
		t.Fatalf("bytesInFlight = %d after acking an exempt packet, want 0", inFlight)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

```bash
cd /home/kforfk/workspace/objtrsf
go test ./trsf -run 'TestLoneExemptPacketArmsLossTimer|TestExemptPacket' -v
```

Expected: compile failure — `unknown field CongestionExempt in struct literal`.

- [ ] **Step 4: Add the fields**

In `trsf/ack_handler.go`, in `type SentPacket struct` (around line 120), replace the lone `IsMTUProbe bool` with:

```go
	// CongestionExempt keeps this packet out of the congestion controller
	// entirely: bytesInFlight, RecordSend, RecordACK and RecordLoss. It is what
	// an uncontrolled datagram sets, and what an MTU probe has always meant by
	// IsMTUProbe.
	CongestionExempt bool
	// PathEvidence says this packet's fate tells us something about the path,
	// so its loss belongs in loss.Packets and in the spurious accounting. An
	// MTU probe clears it -- a probe's loss is the probe's ANSWER, not a signal
	// about the path -- while a datagram sets it, because a datagram's loss is
	// the only thing that distinguishes a path drop from one of our own.
	PathEvidence bool
	// IsMTUProbe now means only "this is an MTU probe", for mtuProbesOutstanding
	// and declareLostMTUProbes. It no longer decides congestion participation;
	// a probe sets CongestionExempt as well.
	IsMTUProbe bool
```

Add alongside `mtuProbesOutstanding` in `type SentPacketHandler struct`:

```go
	// exemptOutstanding counts congestion-exempt packets sitting in sentRanges
	// whose loss we still intend to detect. It exists for the same reason
	// mtuProbesOutstanding does: those packets are absent from bytesInFlight,
	// so without a term of their own setLossDetectionTimer disarms while they
	// are still in flight and their OnLost never fires.
	exemptOutstanding int
```

- [ ] **Step 5: Split the accounting in `OnSent`**

`OnSent` currently fuses the two concerns with an `else`. Replace its body's middle with two independent conditions:

```go
	ah.sentRanges = append(ah.sentRanges, s)
	if !s.CongestionExempt {
		ah.addBytesInFlight(s.PacketSize)
		ah.cong.RecordSend(s.PacketSize, s.SentTime)
	} else {
		ah.exemptOutstanding++
	}
	if s.IsMTUProbe {
		ah.mtuProbesOutstanding++
	}
```

- [ ] **Step 6: Key the retirement paths on the right field**

In `detectAck`, the per-packet loop keys `probeSize` and the outstanding counters on the new fields:

```go
	for _, p := range ackedPackets {
		sentSize += p.PacketSize
		if p.OnACK != nil {
			p.OnACK(rcvTime)
		}
		if p.CongestionExempt {
			exemptSize += p.PacketSize
			ah.exemptOutstanding--
		}
		if p.IsMTUProbe {
			ah.mtuProbesOutstanding--
		}
	}
	if len(ackedPackets) > 0 {
		ah.removeBytesInFlight(sentSize - exemptSize)
		ah.cong.RecordACK(sentSize, rcvTime)
	}
```

(declare `exemptSize` where `probeSize` was declared, and delete `probeSize`. `RecordACK`'s argument is Task 2's subject — leave it as `sentSize` here.)

In `detectLost`, split the loop's bookkeeping and then the two things the trailing `if` currently gates together:

```go
		if packetLost {
			somePacketLost = true
			lostSize += p.PacketSize
			if p.OnLost != nil {
				p.OnLost(now) // maybe queueing
			}
			lostCount++
			if p.CongestionExempt {
				exemptSize += p.PacketSize
				ah.exemptOutstanding--
			} else {
				congestionLostSize += p.PacketSize
				congestionLostCount++
			}
			if p.IsMTUProbe {
				ah.mtuProbesOutstanding--
			}
			if p.PathEvidence {
				evidenceCount++
				ah.rememberDeclaredLost(p.PacketNumber)
			}
		}
```

and the tail:

```go
	if somePacketLost {
		ah.removeBytesInFlight(lostSize - exemptSize)
		ah.loss.Packets += evidenceCount
		if congestionLostCount > 0 {
			ah.loss.Events++
			ah.cong.RecordLoss(congestionLostSize, now)
		}
	}
```

Declare `exemptSize`, `congestionLostSize`, `congestionLostCount` and `evidenceCount` where `probeSize` and `mtuProbe` were, and delete those two.

Update `LossStats.Packets`'s comment — it says "non-probe packets declared lost" and now means "packets declared lost that were path evidence".

- [ ] **Step 7: Fix the disarm condition**

In `setLossDetectionTimer`, add the third term:

```go
	if ah.bytesInFlight == 0 && ah.mtuProbesOutstanding == 0 && ah.exemptOutstanding == 0 {
```

- [ ] **Step 8: Teach the in-flight audit about exempt packets**

In `auditBytesInFlight`, the recomputation currently skips `IsMTUProbe`. It must skip `CongestionExempt` instead, or it will report a false discrepancy on every exempt packet:

```go
		if ah.sentRanges[i].CongestionExempt {
			continue
		}
```

- [ ] **Step 9: Carry the new fields into the observability snapshot**

In `GetInternal`, add them to the `InternalSentPacket` it builds, and to the `InternalSentPacket` struct definition:

```go
			IsMTUProbe:       p.IsMTUProbe,
			CongestionExempt: p.CongestionExempt,
			PathEvidence:     p.PathEvidence,
```

- [ ] **Step 10: Update the MTU probe's construction site**

At `trsf/conn.go:908`, the probe now states all three:

```go
			s.sh.OnSent(&SentPacket{
				// ... existing fields ...
				IsMTUProbe:       true,
				CongestionExempt: true,
				PathEvidence:     false,
			})
```

- [ ] **Step 11: Run the new tests**

```bash
cd /home/kforfk/workspace/objtrsf
go test ./trsf -run 'TestLoneExemptPacketArmsLossTimer|TestExemptPacket' -v
```

Expected: all three PASS.

- [ ] **Step 12: Run the whole suite — the probe tests are the regression gate**

```bash
cd /home/kforfk/workspace/objtrsf
go build ./... && go vet ./... && go test ./...
```

Expected: PASS, and specifically `TestLoneMTUProbeArmsLossTimer` and `TestLoneLostMTUProbeDoesNotWedgeDiscovery` still pass — this task must not change probe behaviour.

- [ ] **Step 13: Commit**

```bash
cd /home/kforfk/workspace/objtrsf
git add trsf/ack_handler.go trsf/conn.go trsf/datagram_accounting_test.go
git commit -m "refactor(trsf): IsMTUProbe was three concepts, and one of them has a second user

Congestion exemption, not-evidence-about-the-path, and probe bookkeeping
rode one bool. An uncontrolled datagram needs the first without the
second, which the bool cannot express.

setLossDetectionTimer disarms on bytesInFlight==0 &&
mtuProbesOutstanding==0 -- the condition 7db84f6 added so a lone probe's
loss could be detected at all. An exempt packet is in neither term and
reopens exactly that hole, so exemptOutstanding joins them.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

## Task 2: `RecordACK` stops counting bytes that never consumed the window

The send side excludes exempt bytes from `RecordSend` and `detectLost` excludes them from `RecordLoss`, but `detectAck` passes the full `sentSize` to `RecordACK`. The window therefore grows on bytes that never occupied it.

At one MTU probe per 30s reprobe period this is unmeasurable, which is why it has stood. It stops being harmless the moment Task 5's consumer exists: uncontrolled datagrams at rate would have every acknowledgement inflate the window of the controlled streams sharing the connection — a flow that ignores congestion would also be enlarging everyone else's share. This is a real behaviour change for MTU probes, which is why it is its own task and its own commit.

**Files:**
- Modify: `/home/kforfk/workspace/objtrsf/trsf/ack_handler.go` (`detectAck`)
- Test: `/home/kforfk/workspace/objtrsf/trsf/datagram_accounting_test.go` (append)

**Interfaces:**
- Consumes: `SentPacket.CongestionExempt` and the `exemptSize` accumulator from Task 1.
- Produces: no new symbols.

- [ ] **Step 1: Write the failing test**

Append to `trsf/datagram_accounting_test.go`:

```go
// An exempt packet's bytes never entered the window, so acknowledging them must
// not grow it. Without this, a high-rate uncontrolled sender inflates the window
// of the controlled streams sharing its connection.
func TestExemptAckDoesNotGrowCongestionWindow(t *testing.T) {
	sh, _ := newExemptOnlyHandler(t)
	_, _, cwndBefore, _, _ := sh.GetInternal()

	if err := sh.ReceiveACK(time.Now(), []Range{{Begin: 0, End: 1}}, 0); err != nil {
		t.Fatalf("ReceiveACK: %v", err)
	}

	if _, _, cwndAfter, _, _ := sh.GetInternal(); cwndAfter != cwndBefore {
		t.Fatalf("cwnd %d -> %d on acking a congestion-exempt packet: it grew on bytes that never occupied it", cwndBefore, cwndAfter)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

```bash
cd /home/kforfk/workspace/objtrsf
go test ./trsf -run TestExemptAckDoesNotGrowCongestionWindow -v
```

Expected: FAIL — cwnd grew.

- [ ] **Step 3: Exclude exempt bytes**

In `detectAck`'s tail:

```go
	if len(ackedPackets) > 0 {
		ah.removeBytesInFlight(sentSize - exemptSize)
		ah.cong.RecordACK(sentSize-exemptSize, rcvTime)
	}
```

Add a comment saying the send and loss sides already exclude these, and that ACK was the odd one out.

- [ ] **Step 4: Run the test and the whole suite**

```bash
cd /home/kforfk/workspace/objtrsf
go test ./trsf -run TestExemptAckDoesNotGrowCongestionWindow -v
go build ./... && go vet ./... && go test ./...
```

Expected: PASS, everything green. If an MTU-probe test now fails, do not weaken it — the probe's behaviour genuinely changed and the test tells you how.

- [ ] **Step 5: Commit**

```bash
cd /home/kforfk/workspace/objtrsf
git add trsf/ack_handler.go trsf/datagram_accounting_test.go
git commit -m "fix(trsf): RecordACK counted probe bytes that RecordSend never did

Send excludes exempt bytes, loss excludes them, ACK did not -- so the
window grew on bytes that never occupied it. Harmless at one probe per
30s reprobe period; not harmless once a datagram sender is exempt at
rate, where it would let a flow that ignores congestion enlarge the
window of the streams sharing its connection.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

## Task 3: The wire — a transport-owned `datagram` kind

**Files:**
- Modify: `/home/kforfk/workspace/objtrsf/trsf/wire/stream.bgn`
- Regenerate: `/home/kforfk/workspace/objtrsf/trsf/wire/stream.go`
- Test: `/home/kforfk/workspace/objtrsf/trsf/wire/datagram_wire_test.go` (create)

**Interfaces:**
- Consumes: nothing.
- Produces: `wire.ApplicationPayloadKind_Datagram` (value 7), `wire.DatagramPacket` with a `Data []byte` field, `StreamAppPacket.Datagram()` / `SetDatagram()`, and `wire.IsStreamRelated` returning true for the new kind. Tasks 4 and 5 use all of these.

- [ ] **Step 1: Edit the schema**

In `trsf/wire/stream.bgn`, append to `enum ApplicationPayloadKind` (line 102), after `stream_window_update`:

```
    datagram
```

Add, next to the other formats:

```
# DatagramPacket carries one unreliable application payload. There is no length
# field: objproto does not split an application message, so one datagram is
# exactly one packet and rest-of-packet is exact.
#
# Congestion mode is deliberately NOT on the wire. It is a property of how the
# sender treats its own window; the receiver acknowledges both modes
# identically, so a wire bit would offer the receiver a choice it does not have.
format DatagramPacket:
    data :[..]u8
```

Add the arm to `format StreamAppPacket` (line 122), before the `..` error arm:

```
        ApplicationPayloadKind.datagram => datagram :DatagramPacket
```

Add the kind to `fn isStreamRelated` (line 116). Note what this does to the name: the predicate's only caller is the routing branch at `trsf/api.go:184`, so what it actually answers is "is this decoded by `StreamAppPacket` and handled by the run loop". A datagram qualifies for that reason, not because it is stream-related. Leave the name and record the fact in a comment above the `fn`:

```
# NOTE this answers "does the run loop handle this kind", which is what its one
# caller (AutoReceive) asks. Its membership must equal StreamAppPacket's arms;
# is_defined cannot derive that, since it answers enum membership and this is a
# subset. A kind added to the union but not here reaches onEvent and vanishes
# with no error -- see the guard in AutoReceive.
```

- [ ] **Step 2: Regenerate**

```bash
cd /home/kforfk/workspace/remote-agent-harness
./scripts/protoregen.sh /home/kforfk/workspace/objtrsf/trsf/wire/stream.bgn
```

Expected: `trsf/wire/stream.go` rewritten. The diff will be large and mechanical — do not investigate it.

- [ ] **Step 3: Check the new symbols exist with the expected values**

```bash
cd /home/kforfk/workspace/objtrsf
grep -n 'ApplicationPayloadKind_Datagram\|func (t \*StreamAppPacket) Datagram\|type DatagramPacket' trsf/wire/stream.go
go build ./...
```

Expected: `ApplicationPayloadKind_Datagram` present with value 7; `DatagramPacket` and the union accessor present; build green.

- [ ] **Step 4: Write the round-trip test**

Create `/home/kforfk/workspace/objtrsf/trsf/wire/datagram_wire_test.go`:

```go
package wire

import (
	"bytes"
	"testing"
)

func TestDatagramPacketRoundTrip(t *testing.T) {
	payload := []byte("the quick brown fox")

	var out StreamAppPacket
	out.Header.Kind = ApplicationPayloadKind_Datagram
	out.SetDatagram(DatagramPacket{Data: payload})

	encoded, err := out.EncodeCopy(nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	var in StreamAppPacket
	if err := in.DecodeExact(encoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := in.Datagram()
	if got == nil {
		t.Fatal("decoded packet has no datagram arm")
	}
	if !bytes.Equal(got.Data, payload) {
		t.Fatalf("payload = %q, want %q", got.Data, payload)
	}
}

// The predicate's membership and the union's arms are the same set written
// twice in this file. A kind in the union but not the predicate never reaches
// the run loop and vanishes silently, which is the expensive direction.
func TestDatagramIsRoutedToTheRunLoop(t *testing.T) {
	if !IsStreamRelated(ApplicationPayloadKind_Datagram) {
		t.Fatal("datagram is decoded by StreamAppPacket but the routing predicate rejects it: AutoReceive would hand it to onEvent and it would disappear")
	}
}
```

- [ ] **Step 5: Run the tests**

```bash
cd /home/kforfk/workspace/objtrsf
go test ./trsf/wire -run 'TestDatagram' -v
go build ./... && go vet ./... && go test ./...
```

Expected: both new tests PASS; whole suite green.

- [ ] **Step 6: Commit**

```bash
cd /home/kforfk/workspace/objtrsf
git add trsf/wire/stream.bgn trsf/wire/stream.go trsf/wire/datagram_wire_test.go
git commit -m "feat(trsf)!: a transport-owned datagram kind

Transport-owned rather than in the >=0x40 consumer range, because the
transport is what must acknowledge it -- AutoReceive hands consumer
kinds straight to onEvent, so a payload there can never be ACKed.

No length field (objproto does not split an application message, so one
datagram is one packet) and no congestion-mode bit (that is the sender's
property; the receiver ACKs both modes identically).

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

## Task 4: Receiving a datagram

**Files:**
- Modify: `/home/kforfk/workspace/objtrsf/trsf/conn.go` (`handlePacket`, `Streams` struct, `InternalState`)
- Modify: `/home/kforfk/workspace/objtrsf/trsf/api.go` (`AutoReceive` guard, `Transport` interface)
- Test: `/home/kforfk/workspace/objtrsf/trsf/datagram_test.go` (create)

**Interfaces:**
- Consumes: `wire.ApplicationPayloadKind_Datagram`, `wire.DatagramPacket`, `StreamAppPacket.Datagram()` from Task 3.
- Produces: `Transport.ReceiveDatagram(ctx context.Context) ([]byte, error)` and `InternalState.DatagramsReceived` / `DatagramsDroppedReceiveQueue` (`uint64`). Task 5 adds the send-side counters beside them.

- [ ] **Step 1: Write the failing test**

Create `/home/kforfk/workspace/objtrsf/trsf/datagram_test.go`:

```go
package trsf

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/trsf/wire"
)

func newTestStreams(t *testing.T, isServer bool) *Streams {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return NewStreams(ctx, isServer, DefaultInitialMTU, DefaultMaxMTU, &stubPNIssuer{}, logger).(*Streams)
}

// A received datagram is acknowledged like stream data -- its packet number
// must reach the tracker -- and its payload is handed to the application
// through a queue rather than through the stream machinery.
func TestReceivedDatagramIsQueuedAndAckEliciting(t *testing.T) {
	s := newTestStreams(t, true)
	payload := []byte("hello datagram")

	var pkt wire.StreamAppPacket
	pkt.Header.Kind = wire.ApplicationPayloadKind_Datagram
	pkt.SetDatagram(wire.DatagramPacket{Data: payload})
	encoded, err := pkt.EncodeCopy(nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	s.handlePacket(&objproto.Message{Data: encoded, PacketNumber: 42})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, err := s.ReceiveDatagram(ctx)
	if err != nil {
		t.Fatalf("ReceiveDatagram: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatalf("payload = %q, want %q", got, payload)
	}

	ranges, _ := s.pt.GenerateACK()
	found := false
	for _, r := range ranges {
		if 42 >= r.Begin && 42 < r.End {
			found = true
		}
	}
	if !found {
		t.Fatal("packet number 42 was not queued for acknowledgement: the sender can never learn this datagram arrived")
	}
}
```

`stubPNIssuer` already exists in `trsf/accept_queue_test.go` in this package and is used as a pointer. `PacketNumTracker.GenerateACK() ([]Range, time.Duration)` is the existing drain — do not add an accessor just for this test.

- [ ] **Step 2: Run it to verify it fails**

```bash
cd /home/kforfk/workspace/objtrsf
go test ./trsf -run TestReceivedDatagramIsQueuedAndAckEliciting -v
```

Expected: compile failure — `s.ReceiveDatagram undefined`.

- [ ] **Step 3: Add the receive queue to `Streams`**

In `type Streams struct` in `trsf/conn.go`, beside `newRecvStreamQueue`:

```go
	// datagramRecv is bounded and drops the NEWEST on overflow, which is what a
	// socket receive buffer does. A datagram has no retransmission, so blocking
	// here would convert a drop into unbounded latency for every other packet
	// the run loop still has to process.
	datagramRecv        chan []byte
	datagramsReceived   atomic.Uint64
	datagramsDroppedRcv atomic.Uint64
```

In `NewStreams`, initialise it:

```go
		datagramRecv: make(chan []byte, 256),
```

- [ ] **Step 4: Handle the arm in `handlePacket`**

In `handlePacket`'s chain in `trsf/conn.go`, after the `StreamCancel` branch, add:

```go
	} else if dg := pkt.Datagram(); dg != nil {
		// Ack-eliciting like stream data: without this the sender has no way to
		// learn the datagram arrived, and nothing can separate a path drop from
		// one of our own.
		s.pt.InsertUnacked(uint64(recvData.PacketNumber))
		payload := make([]byte, len(dg.Data))
		copy(payload, dg.Data)
		select {
		case s.datagramRecv <- payload:
			s.datagramsReceived.Add(1)
		default:
			s.datagramsDroppedRcv.Add(1)
		}
	}
```

- [ ] **Step 5: Add `ReceiveDatagram` and put it on the interface**

In `trsf/conn.go`:

```go
// ReceiveDatagram returns the next datagram, blocking until one arrives or ctx
// ends. Datagrams are not ordered against stream data and carry no stream id;
// what they mean is entirely the consumer's framing.
func (s *Streams) ReceiveDatagram(ctx context.Context) ([]byte, error) {
	select {
	case b := <-s.datagramRecv:
		return b, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
```

In `trsf/api.go`, add to the `Transport` interface:

```go
	ReceiveDatagram(ctx context.Context) ([]byte, error)
```

- [ ] **Step 6: Surface the counters**

Add to `InternalState` in `trsf/conn.go` and populate them in `GetInternalState`:

```go
	DatagramsReceived            uint64
	DatagramsDroppedReceiveQueue uint64
```

```go
		DatagramsReceived:            s.datagramsReceived.Load(),
		DatagramsDroppedReceiveQueue: s.datagramsDroppedRcv.Load(),
```

- [ ] **Step 7: Add the routing-drift guard**

In `trsf/api.go`'s `AutoReceive`, immediately before the final `onEvent` call:

```go
		if uint8(kind) < wire.USER_DEFINED_START {
			// A transport-owned kind reaching the consumer seam is a routing
			// bug, not a payload: ping/pong/close are handled above and
			// everything else in this range belongs to the run loop. It means
			// StreamAppPacket gained an arm that IsStreamRelated did not, and
			// without this line the packet would disappear with no error.
			p.CountUnroutedTransportKind()
		}
```

with, on `Streams`:

```go
func (s *Streams) CountUnroutedTransportKind() {
	s.unroutedTransportKind.Add(1)
	s.logger.Error("transport-owned payload kind reached the application seam; IsStreamRelated and StreamAppPacket have drifted apart")
}
```

an `unroutedTransportKind atomic.Uint64` field, a matching `UnroutedTransportKind uint64` in `InternalState`, and the method on the `Transport` interface.

- [ ] **Step 8: Run the tests**

```bash
cd /home/kforfk/workspace/objtrsf
go test ./trsf -run TestReceivedDatagramIsQueuedAndAckEliciting -v
go build ./... && go vet ./... && go test ./...
```

Expected: new test PASS; whole suite green. Any other implementer of `Transport` in tests needs the two new methods — add them there rather than removing them from the interface.

- [ ] **Step 9: Commit**

```bash
cd /home/kforfk/workspace/objtrsf
git add trsf/conn.go trsf/api.go trsf/datagram_test.go
git commit -m "feat(trsf): receive datagrams, acknowledged, on a bounded queue

AutoReceive decided two things with one predicate: whether a packet is
ACKed and whether it reaches the stream machinery. A datagram needs the
first and not the second, so it routes into the run loop and handlePacket
inserts its packet number before queueing the payload.

The queue drops the newest on overflow, as a socket receive buffer does;
blocking would turn a drop into latency for everything behind it.

Adds the drift guard: a transport-range kind arriving at the consumer
seam means the union gained an arm the predicate did not, which is the
direction that otherwise fails silently.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

## Task 5: Sending a datagram, in both congestion modes

**Files:**
- Modify: `/home/kforfk/workspace/objtrsf/trsf/conn.go` (send queue, run-loop drain, `MaxDatagramSize`, counters)
- Modify: `/home/kforfk/workspace/objtrsf/trsf/api.go` (`Transport` interface)
- Test: `/home/kforfk/workspace/objtrsf/trsf/datagram_test.go` (append)

**Interfaces:**
- Consumes: `SentPacket.CongestionExempt` / `PathEvidence` (Task 1), the wire arm (Task 3), the counter pattern (Task 4).
- Produces: `Transport.SendDatagram(b []byte) error`, `Transport.SendDatagramUncontrolled(b []byte) error`, `Transport.MaxDatagramSize() int`, the sentinel errors `ErrDatagramTooLarge`, `ErrCongestionBlocked`, `ErrDatagramQueueFull`, and `InternalState.DatagramsSent` / `DatagramsSentUncontrolled` / `DatagramsDroppedOversize` / `DatagramsDroppedCongestion` / `DatagramsDroppedSendQueue`. **The harness plan quotes all of these.**

- [ ] **Step 1: Write the failing tests**

Append to `trsf/datagram_test.go`:

```go
// The size ceiling is trsf's to state, so no consumer restates
// CurrentMTU - fixedOverhead - frame header. It moves with PLPMTUD.
func TestMaxDatagramSizeIsBelowTheCurrentMTU(t *testing.T) {
	s := newTestStreams(t, false)
	max := s.MaxDatagramSize()
	if max <= 0 || max >= DefaultInitialMTU {
		t.Fatalf("MaxDatagramSize() = %d, want 0 < n < %d", max, DefaultInitialMTU)
	}
	if err := s.SendDatagram(make([]byte, max+1)); err != ErrDatagramTooLarge {
		t.Fatalf("oversized send returned %v, want ErrDatagramTooLarge", err)
	}
	if got := s.GetInternalState().DatagramsDroppedOversize; got != 1 {
		t.Fatalf("DatagramsDroppedOversize = %d, want 1: an oversized datagram was refused but not counted, so nothing would explain why a tunnel drops large packets", got)
	}
}

// A controlled datagram that meets a closed window is DROPPED, not parked.
// Parking trades a visible drop for invisible latency and unbounded buffering,
// and for a tunnel the drop is what the path would have done anyway.
func TestControlledDatagramIsDroppedWhenTheWindowIsClosed(t *testing.T) {
	s := newTestStreams(t, false)
	fillCongestionWindow(t, s)

	err := s.SendDatagram([]byte("payload"))
	if err != ErrCongestionBlocked {
		t.Fatalf("send with a closed window returned %v, want ErrCongestionBlocked", err)
	}
	if s.GetInternalState().DatagramsDroppedCongestion != 1 {
		t.Fatal("a congestion drop was not counted")
	}
}

// The uncontrolled mode exists precisely so a bounded-rate sender is not made
// to wait on a window it is not part of.
func TestUncontrolledDatagramIgnoresTheWindow(t *testing.T) {
	s := newTestStreams(t, false)
	fillCongestionWindow(t, s)

	if err := s.SendDatagramUncontrolled([]byte("payload")); err != nil {
		t.Fatalf("uncontrolled send with a closed window: %v", err)
	}
	if s.GetInternalState().DatagramsSentUncontrolled != 1 {
		t.Fatal("an uncontrolled send was not counted")
	}
}

// fillCongestionWindow puts enough in flight that CanSend() is false.
func fillCongestionWindow(t *testing.T, s *Streams) {
	t.Helper()
	for i := 0; s.sh.CanSend() && i < 1000; i++ {
		s.sh.OnSent(&SentPacket{
			PacketNumber: uint64(i),
			PacketSize:   DefaultInitialMTU,
			SentTime:     time.Now(),
			PathEvidence: true,
			Kind:         wire.ApplicationPayloadKind_StreamData,
		})
	}
	if s.sh.CanSend() {
		t.Fatal("setup: could not close the congestion window")
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

```bash
cd /home/kforfk/workspace/objtrsf
go test ./trsf -run 'TestMaxDatagramSize|TestControlledDatagram|TestUncontrolledDatagram' -v
```

Expected: compile failure — `s.MaxDatagramSize undefined`.

- [ ] **Step 3: Add the sentinel errors and the size ceiling**

In `trsf/conn.go`:

```go
var (
	// ErrDatagramTooLarge: the payload does not fit one packet. There is no
	// fragmentation by design, so this is final for this payload -- the caller
	// sends smaller or waits for PLPMTUD to raise MaxDatagramSize.
	ErrDatagramTooLarge  = errors.New("trsf: datagram exceeds MaxDatagramSize")
	ErrCongestionBlocked = errors.New("trsf: congestion window closed; datagram dropped")
	ErrDatagramQueueFull = errors.New("trsf: datagram send queue full; datagram dropped")
)

// datagramFrameOverhead is the kind byte StreamAppPacket puts in front of the
// payload. It lives here so no consumer restates the arithmetic.
const datagramFrameOverhead = 1

// MaxDatagramSize is the largest payload that fits one packet right now. It
// MOVES with PLPMTUD, so callers must read it rather than cache it.
func (s *Streams) MaxDatagramSize() int {
	return s.mtu.CurrentMTU() - fixedOverhead - datagramFrameOverhead
}
```

- [ ] **Step 4: Add the shallow send queue and the two entry points**

```go
// datagramSend is deliberately SHALLOW. Every send goes through the run loop,
// so a handoff queue is structural, but depth is latency a tunnel cannot see
// and cannot afford. Neither end of it parks: a full queue is an error to the
// caller, and a closed window at drain time is a counted drop.
type pendingDatagram struct {
	payload []byte
	exempt  bool
}

func (s *Streams) SendDatagram(b []byte) error { return s.sendDatagram(b, false) }

func (s *Streams) SendDatagramUncontrolled(b []byte) error { return s.sendDatagram(b, true) }

func (s *Streams) sendDatagram(b []byte, exempt bool) error {
	if len(b) > s.MaxDatagramSize() {
		s.datagramsDroppedOversize.Add(1)
		return ErrDatagramTooLarge
	}
	if !exempt && !s.sh.CanSend() {
		s.datagramsDroppedCongestion.Add(1)
		return ErrCongestionBlocked
	}
	payload := make([]byte, len(b))
	copy(payload, b)
	select {
	case s.datagramSend <- pendingDatagram{payload: payload, exempt: exempt}:
	default:
		s.datagramsDroppedSendQueue.Add(1)
		return ErrDatagramQueueFull
	}
	s.datagramTrigger.Notify()
	return nil
}
```

Add to `Streams`: `datagramSend chan pendingDatagram` sized 32, `datagramTrigger *trigger` (built with `newTrigger()` in `NewStreams`, the same primitive `withTriggerQueue` embeds), and the five `atomic.Uint64` counters. Initialise the channel and the trigger in `NewStreams`.

The loop must be woken by it. In both select blocks at `trsf/conn.go:636` and `:656`, alongside the existing `case <-s.sendTrigger.Notification():` lines, add:

```go
			case <-s.datagramTrigger.Notification(): // when a datagram is queued
```

A dedicated trigger rather than reusing `sendTrigger`: that queue holds `sendStream` values and its push reasons feed `SendPushApp`/`SendPushCwnd`/… in `InternalState`, so pushing datagram wakes through it would make those counters answer a question they no longer describe.

- [ ] **Step 5: Drain it in the run loop**

In the run loop's send-action selection in `trsf/conn.go`, before the stream-data case, drain one pending datagram per iteration:

```go
		select {
		case dg := <-s.datagramSend:
			if !dg.exempt && !s.sh.CanSend() {
				// Dropped, not parked: see sendDatagram's comment.
				s.datagramsDroppedCongestion.Add(1)
				break
			}
			var pkt wire.StreamAppPacket
			pkt.Header.Kind = wire.ApplicationPayloadKind_Datagram
			pkt.SetDatagram(wire.DatagramPacket{Data: dg.payload})
			encoded, err := pkt.EncodeCopy(nil)
			if err != nil {
				s.logger.Error("encode datagram", "error", err)
				break
			}
			pn := s.pnIssuer.ConsumePacketNumber()
			s.sh.OnSent(&SentPacket{
				Kind:             wire.ApplicationPayloadKind_Datagram,
				PacketNumber:     pn,
				PacketSize:       fixedOverhead + len(encoded),
				SentTime:         time.Now(),
				CongestionExempt: dg.exempt,
				// A datagram's loss IS evidence about the path -- it is the only
				// thing that separates a path drop from one of our own.
				PathEvidence: true,
				// No retransmit: not re-pushing here is the whole of it.
				OnLost: func(now time.Time) {},
			})
			if dg.exempt {
				s.datagramsSentUncontrolled.Add(1)
			} else {
				s.datagramsSent.Add(1)
			}
			return &SendAction{ /* same shape the stream-data case builds, carrying `encoded` and `pn` */ }
		default:
		}
```

Match the surrounding code's actual `SendAction` construction rather than inventing one; the existing stream-data case at `trsf/conn.go:850`-ish is the template.

- [ ] **Step 6: Put the three methods on the interface and surface the counters**

In `trsf/api.go`, add to `Transport`:

```go
	SendDatagram(b []byte) error
	SendDatagramUncontrolled(b []byte) error
	MaxDatagramSize() int
```

Add the five counters to `InternalState` and populate them in `GetInternalState`:

```go
	DatagramsSent              uint64
	DatagramsSentUncontrolled  uint64
	DatagramsDroppedOversize   uint64
	DatagramsDroppedCongestion uint64
	DatagramsDroppedSendQueue  uint64
```

- [ ] **Step 7: Run the tests**

```bash
cd /home/kforfk/workspace/objtrsf
go test ./trsf -run 'TestMaxDatagramSize|TestControlledDatagram|TestUncontrolledDatagram|TestReceivedDatagram' -v
```

Expected: all PASS.

- [ ] **Step 8: Run the whole suite and confirm the control did not move**

```bash
cd /home/kforfk/workspace/objtrsf
go build ./... && go vet ./... && go test ./...
go test ./trsf -run '^$' -bench Throughput -benchtime 1x -count 4
```

Expected: suite green. In the benchmark, the `mock` rung must be unchanged — it never reaches objproto, so nothing in this plan can legitimately move it. Ignore the `udp` and `relay` rungs here; they need interleaved A/B on an unloaded box to say anything.

- [ ] **Step 9: Commit**

```bash
cd /home/kforfk/workspace/objtrsf
git add trsf/conn.go trsf/api.go trsf/datagram_test.go
git commit -m "feat(trsf): send datagrams, congestion controlled or not

Two entry points rather than a bool parameter, so a consumer that opts
out of congestion control is greppable. Not retransmitting is the OnLost
callback declining to re-push -- no new machinery.

A controlled datagram meeting a closed window is dropped and counted,
never parked: parking trades a visible drop for invisible latency and
unbounded buffering, and for a tunnel the drop is what the path would
have done.

MaxDatagramSize lives here so no consumer restates
CurrentMTU - fixedOverhead - frame header, and it moves with PLPMTUD.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

## After the last task

- [ ] **Run the wire-skew check.** This changes the transport kind range, which sits below the handshake layer: an old peer cannot decode the packet at all rather than rejecting it at the PSK layer. From the harness repo:

```bash
cd /home/kforfk/workspace/remote-agent-harness
./scripts/wire-skew-check.sh
```

The requirement is not that skew works — it is that the failure is recoverable: a new runner against an old server retries and never exits, and self-heals once the server is upgraded.

- [ ] **Do not push or bump `go.mod` without asking.** Landing objtrsf is a fast-forward to its trunk followed by a harness `go.mod` bump, and that is the point at which the harness plan becomes writable.

## Self-review notes

Spec coverage, section by section: §4a → Task 3. §5a (send API, no parking, shallow queue) → Task 5. §5b (routing, per-arm ACK, bounded receive queue) → Task 4. §5c (the decomposition table) → Task 1. §5d (the nine sites, `:378`, `auditBytesInFlight`) → Task 1 Steps 5–9. §5e (`RecordACK`) → Task 2. §10 (drift guard) → Task 4 Step 7. §11 (trsf tests, `mock` as control) → Tasks 1–5 plus the final check.

**§5f is deliberately not implemented here.** The spec says exposing `MTUTracker.OnMTUUpdate` is "not required by this spec and is the natural companion to it"; its consumer is the later L3 work, so building it now would be a symbol with no caller.

`declareLostMTUProbes` (`:405`/`:415`/`:428`) and `OnTimeout` (`:476`) were listed in the spec as unread. Task 1 Step 6 changes neither, which is correct only if both read `IsMTUProbe` for probe-specific purposes. **Open them during Task 1 and confirm that before Step 11**; if either uses `IsMTUProbe` to mean "exempt from congestion", it needs `CongestionExempt` instead and that is a Task 1 change, not a follow-up.
