# objproto Key Phase + Header Masking Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn objproto's dead `PacketHeader.version` byte into a working AEAD key-phase bit plus a mask seed, add a KDF-ratchet key update, and make the declared-but-unused `ProtectedHeader` real as an internal control-frame carrier.

**Architecture:** The first header byte becomes `key_phase:u1 + mask_seed:u7`. `kind` and `connection_id` are XOR-masked on the wire with a fixed public 256x3 table indexed by that whole byte — unkeyed, self-contained, reversible by anyone. Traffic keys are derived per phase from a per-direction secret that ratchets forward through HKDF; each direction advances independently and the peer follows the phase bit. The header-protection key stays outside the ratchet.

**Tech Stack:** Go 1.25.7, `crypto/cipher` AES-GCM / ChaCha20-Poly1305, `golang.org/x/crypto/hkdf`, brgen `.bgn` schemas regenerated via this repo's `scripts/protoregen.sh` (lang=go3).

**Spec:** `docs/superpowers/specs/2026-08-18-objproto-key-phase-design.md`

## Global Constraints

- `OBJTRSF` below means the local `objtrsf` checkout (a sibling working copy of this repo, module `github.com/on-keyday/objtrsf`). Tasks 1-7 and 9 run there; task 8 runs in this repo.
- **Never reset the packet number or the replay tracker on key update.** `PacketNumber` is objproto's send counter (`objproto.go:358`) surfaced into trsf's loss detection (`trsf/ack_handler.go:131, 207, 317`). `addActiveConnection` does reset both (`objproto.go:831-832`); that is the proxy-rehandshake reuse path and must not be imitated.
- **Never unmask in place in a received buffer, and never mutate a decoded header struct.** Handshake and ack transcripts are raw wire bytes (`objproto.go:913, 963`) consumed as the harness PSK binder input (`server/psk.go:149`, `runner/connect.go:283`, `cli/client.go:100`, `cli/agent/conn.go:198`). Both ends must retain byte-identical transcripts. Recover logical values into locals via helpers.
- **AAD is the 6 wire header bytes verbatim** — the masked form, exactly as transmitted.
- Read the protected 8-byte prefix through `ProtectedHeader.NonceCounter()` (bits 0..62), never a raw `binary.BigEndian.Uint64`.
- Generated field names, confirmed by generating the proposed schema on 2026-08-18: `MaskedKind uint8`, `MaskedConnectionId uint16`, `Len uint16` are plain fields; `KeyPhase() bool` / `SetKeyPhase(bool) bool`, `MaskSeed() uint8` / `SetMaskSeed(uint8) bool`, `Control() bool` / `SetControl(bool) bool`, `NonceCounter() uint64` / `SetNonceCounter(uint64) bool` are accessors over a private backing field.
- Constants: `keyUpdatePackets = 1 << 22`, `keyUpdateInterval = 10 * time.Minute`, `prevKeyRetention = 3 * time.Second`, `minPacketsBetweenUpdates = 1024`, `minTimeBetweenUpdates = 1 * time.Second`.
- HKDF labels: `ksdk-protocol-key`, `ksdk-protocol-nonce`, `ksdk-protocol-ku`. Unchanged: `ksdk-protocol-connection`, `ksdk-protocol-master-hs`, `ksdk-protocol-master-ack`, `ksdk-protocol-header-protect-hs`, `ksdk-protocol-header-protect-ack`. Removed: `ksdk-protocol-nonce-hs`, `ksdk-protocol-nonce-ack`.
- This is a flag day. Do not add compatibility shims for the old wire format.

## File Structure

| File | Responsibility |
|---|---|
| `OBJTRSF/objproto/packet/packet.bgn` | wire schema (masked header form) |
| `OBJTRSF/objproto/packet/packet.go` | generated; never hand-edited |
| `OBJTRSF/objproto/mask.go` | **new** — mask table, mask/unmask helpers, header build/parse helpers |
| `OBJTRSF/objproto/mask_test.go` | **new** — mask round-trip and wire-layout tests |
| `OBJTRSF/objproto/crypto.go` | key derivation; `aeadKeyLen`, per-phase derivation, ratchet |
| `OBJTRSF/objproto/crypto_test.go` | **new** — key-length and ratchet tests |
| `OBJTRSF/objproto/objproto.go` | `activeConnection` phase state, send/receive paths, triggers |
| `OBJTRSF/objproto/keyupdate_test.go` | **new** — phase follow, reordering, hostile, monotonicity |
| `OBJTRSF/objproto/control.go` | **new** — control-frame encode/decode and handling |
| `OBJTRSF/objproto/session.go` | `Connection.UpdateKey`, `AutoKeyUpdate` |

---

### Task 1: Regenerate packet.go with the current toolchain

No schema change. Isolates ~978 lines of generator drift so the wire-format diff in Task 3 is reviewable.

**Files:**
- Modify: `OBJTRSF/objproto/packet/packet.go` (regenerated wholesale)

**Interfaces:**
- Consumes: nothing
- Produces: a `packet.go` stamped `ebm2go at https://github.com/on-keyday/brgen` with unchanged public API

- [ ] **Step 1: Confirm the working tree is clean**

```bash
cd $OBJTRSF && git status --porcelain
```

Expected: no output. If dirty, stop and resolve before regenerating.

- [ ] **Step 2: Regenerate**

From this repo's root (the script lives here, the target does not):

```bash
./scripts/protoregen.sh $OBJTRSF/objproto/packet/packet.bgn
```

- [ ] **Step 3: Confirm the diff is cosmetic only**

```bash
cd $OBJTRSF && git diff --stat objproto/packet/packet.go
git diff objproto/packet/packet.go | grep -E '^[+-]' | grep -vE 'tmp[0-9]+|io_temp_[0-9]+|Variant[0-9]+|int\(|Code generated' | grep -vE '^[+-][+-][+-]'
```

Expected: the stat shows a similar count of insertions and deletions with the file staying 1651 lines, and the second command prints nothing. If it prints anything, the generator changed behaviour — stop and report before continuing.

- [ ] **Step 4: Build and test**

```bash
cd $OBJTRSF && go build ./... && go test ./objproto/... ./trsf/...
```

Expected: build clean, all packages `ok` or `[no test files]`. `trsf` takes ~40 s.

- [ ] **Step 5: Commit**

```bash
cd $OBJTRSF && git add objproto/packet/packet.go
git commit -m "chore(objproto): regenerate packet.go with the current ebm2go

No schema change. The committed file predated the current generator
(rebrgen vs brgen stamp); this isolates the identifier-renumbering churn
from the wire-format change that follows."
```

---

### Task 2: Mask table and helpers

Pure Go over plain values. No schema dependency, so it lands and tests green before anything else moves.

**Files:**
- Create: `OBJTRSF/objproto/mask.go`
- Create: `OBJTRSF/objproto/mask_test.go`

**Interfaces:**
- Consumes: `packet.PacketKind` (existing enum)
- Produces:
  - `func maskFor(b0 byte) [3]byte`
  - `func maskKind(kind packet.PacketKind, b0 byte) uint8`
  - `func unmaskKind(masked uint8, b0 byte) packet.PacketKind`
  - `func maskConnID(id uint16, b0 byte) uint16`
  - `func unmaskConnID(masked uint16, b0 byte) uint16`
  - `func newMaskSeed(phase uint64, application bool) byte`

- [ ] **Step 1: Write the failing test**

Create `OBJTRSF/objproto/mask_test.go`:

```go
package objproto

import (
	"testing"

	"github.com/on-keyday/objtrsf/objproto/packet"
)

func TestMaskRoundTrip(t *testing.T) {
	kinds := []packet.PacketKind{
		packet.PacketKind_Handshake,
		packet.PacketKind_HandshakeAck,
		packet.PacketKind_Application,
		packet.PacketKind_Probe,
	}
	ids := []uint16{0, 1, 0xEBBC, 0xFFFF}
	for b0 := 0; b0 < 256; b0++ {
		for _, k := range kinds {
			if got := unmaskKind(maskKind(k, byte(b0)), byte(b0)); got != k {
				t.Fatalf("kind b0=%#x: got %v want %v", b0, got, k)
			}
		}
		for _, id := range ids {
			if got := unmaskConnID(maskConnID(id, byte(b0)), byte(b0)); got != id {
				t.Fatalf("connid b0=%#x: got %#x want %#x", b0, got, id)
			}
		}
	}
}

func TestMaskIsNotIdentity(t *testing.T) {
	// A mask that leaves the value untouched for every seed would silently
	// defeat the whole point, so require that some seed changes each field.
	changedKind, changedID := false, false
	for b0 := 0; b0 < 256; b0++ {
		if maskKind(packet.PacketKind_Application, byte(b0)) != uint8(packet.PacketKind_Application) {
			changedKind = true
		}
		if maskConnID(0xEBBC, byte(b0)) != 0xEBBC {
			changedID = true
		}
	}
	if !changedKind || !changedID {
		t.Fatalf("mask never alters a field: kind=%v id=%v", changedKind, changedID)
	}
}

func TestMaskColumnsArePermutations(t *testing.T) {
	for i := 0; i < 3; i++ {
		var seen [256]bool
		for b0 := 0; b0 < 256; b0++ {
			v := maskFor(byte(b0))[i]
			if seen[v] {
				t.Fatalf("column %d is not a permutation: %#x repeats", i, v)
			}
			seen[v] = true
		}
	}
}

func TestNewMaskSeedCarriesPhase(t *testing.T) {
	for phase := uint64(0); phase < 4; phase++ {
		b0 := newMaskSeed(phase, true)
		if want := byte(phase&1) << 7; b0&0x80 != want {
			t.Fatalf("phase %d: bit7 = %#x want %#x", phase, b0&0x80, want)
		}
	}
	// Non-application packets have no phase, so all 8 bits must be free to
	// vary; a constant top bit would be the fixed byte we are removing.
	varied := false
	for i := 0; i < 512; i++ {
		if newMaskSeed(0, false)&0x80 != 0 {
			varied = true
			break
		}
	}
	if !varied {
		t.Fatal("non-application seed never sets bit 7")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

```bash
cd $OBJTRSF && go test ./objproto/ -run TestMask -v
```

Expected: FAIL to build, `undefined: maskFor` and friends.

- [ ] **Step 3: Write the implementation**

Create `OBJTRSF/objproto/mask.go`:

```go
package objproto

import (
	"math/rand/v2"

	"github.com/on-keyday/objtrsf/objproto/packet"
)

// The first header byte is key_phase:u1 || mask_seed:u7. It travels in
// cleartext and is the index into maskTable, which XOR-masks `kind` and
// `connection_id` on the wire.
//
// This is obfuscation, not confidentiality: the seed is right there in the
// packet, so anyone who knows the scheme reverses it. What it buys is that no
// header byte holds a constant value at a constant offset. See the Non-goals
// section of the design doc before claiming anything more for it.
var maskTable [256][3]byte

func init() {
	// Odd multipliers, so each column is a permutation of 0..255.
	c := [3]byte{0x1d, 0x8b, 0x37}
	d := [3]byte{0x5a, 0xa5, 0x3c}
	for b := 0; b < 256; b++ {
		for i := 0; i < 3; i++ {
			maskTable[b][i] = byte(b)*c[i] ^ d[i]
		}
	}
}

func maskFor(b0 byte) [3]byte { return maskTable[b0] }

func maskKind(kind packet.PacketKind, b0 byte) uint8 {
	return uint8(kind) ^ maskTable[b0][0]
}

func unmaskKind(masked uint8, b0 byte) packet.PacketKind {
	return packet.PacketKind(masked ^ maskTable[b0][0])
}

func maskConnID(id uint16, b0 byte) uint16 {
	m := maskTable[b0]
	return id ^ (uint16(m[1])<<8 | uint16(m[2]))
}

func unmaskConnID(masked uint16, b0 byte) uint16 {
	return maskConnID(masked, b0) // XOR is its own inverse
}

// newMaskSeed builds the first header byte. For application packets bit 7 is
// the key phase and the low 7 bits are random. For every other kind there is
// no phase, so all 8 bits are random -- leaving bit 7 at zero would keep a
// constant bit at a constant offset.
func newMaskSeed(phase uint64, application bool) byte {
	b := byte(rand.UintN(256))
	if application {
		b &^= 0x80
		b |= byte(phase&1) << 7
	}
	return b
}
```

- [ ] **Step 4: Run the tests to verify they pass**

```bash
cd $OBJTRSF && go test ./objproto/ -run TestMask -v && go test ./objproto/ -run TestNewMaskSeed -v
```

Expected: PASS for all four tests.

- [ ] **Step 5: Commit**

```bash
cd $OBJTRSF && git add objproto/mask.go objproto/mask_test.go
git commit -m "feat(objproto): add the header mask table and helpers

Unkeyed, self-contained XOR masking indexed by the first header byte.
Not yet wired into the wire format."
```

---

### Task 3: Schema change and wire the masking in

**Files:**
- Modify: `OBJTRSF/objproto/packet/packet.bgn`
- Modify: `OBJTRSF/objproto/packet/packet.go` (regenerated)
- Modify: `OBJTRSF/objproto/packet/protocol_test.go`
- Modify: `OBJTRSF/objproto/mask.go` (add header build/parse helpers)
- Modify: `OBJTRSF/objproto/objproto.go:588, 897, 1114, 1176` (header literals), `:985` and `:1134` (AAD), `:1051` (cid), `:1064-1065` (proxy), `:1068` (dispatch)
- Test: `OBJTRSF/objproto/mask_test.go`

**Interfaces:**
- Consumes: Task 2's `maskKind` / `unmaskKind` / `maskConnID` / `unmaskConnID` / `newMaskSeed`
- Produces:
  - `func buildHeader(kind packet.PacketKind, connID uint16, length uint16, phase uint64) packet.PacketHeader`
  - `func headerByte0(h *packet.PacketHeader) byte`
  - `func headerKind(h *packet.PacketHeader) packet.PacketKind`
  - `func headerConnID(h *packet.PacketHeader) uint16`

- [ ] **Step 1: Write the failing wire-layout test**

Append to `OBJTRSF/objproto/mask_test.go`:

```go
func TestHeaderWireLayout(t *testing.T) {
	h := buildHeader(packet.PacketKind_Application, 0xEBBC, 0x0102, 1)
	wire, err := h.Append(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(wire) != 6 {
		t.Fatalf("header is %d bytes, want 6", len(wire))
	}
	b0 := wire[0]
	if b0&0x80 == 0 {
		t.Fatalf("phase 1 must set bit 7, got %#x", b0)
	}
	if wire[1] != maskKind(packet.PacketKind_Application, b0) {
		t.Fatalf("kind byte not masked: %#x", wire[1])
	}
	wantID := maskConnID(0xEBBC, b0)
	if got := uint16(wire[2])<<8 | uint16(wire[3]); got != wantID {
		t.Fatalf("connid not masked: got %#x want %#x", got, wantID)
	}
	if got := uint16(wire[4])<<8 | uint16(wire[5]); got != 0x0102 {
		t.Fatalf("len must stay cleartext, got %#x", got)
	}

	var back packet.PacketHeader
	off := 0
	if err := back.DecodeSlice(wire, &off); err != nil {
		t.Fatal(err)
	}
	if headerKind(&back) != packet.PacketKind_Application {
		t.Fatalf("kind round-trip failed: %v", headerKind(&back))
	}
	if headerConnID(&back) != 0xEBBC {
		t.Fatalf("connid round-trip failed: %#x", headerConnID(&back))
	}
	if headerByte0(&back) != b0 {
		t.Fatalf("byte0 reassembly failed: %#x want %#x", headerByte0(&back), b0)
	}
}

func TestHeaderKindVariesOnTheWire(t *testing.T) {
	// The whole point: the kind byte must not be a constant at a fixed offset.
	seen := map[byte]bool{}
	for i := 0; i < 4096; i++ {
		h := buildHeader(packet.PacketKind_Application, 0xEBBC, 16, 0)
		wire, err := h.Append(nil)
		if err != nil {
			t.Fatal(err)
		}
		seen[wire[1]] = true
	}
	if len(seen) < 32 {
		t.Fatalf("kind byte took only %d distinct values over 4096 packets", len(seen))
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

```bash
cd $OBJTRSF && go test ./objproto/ -run TestHeader -v
```

Expected: FAIL to build, `undefined: buildHeader`.

- [ ] **Step 3: Change the schema**

In `OBJTRSF/objproto/packet/packet.bgn`, replace the `PacketHeader` format:

```
format PacketHeader:
    key_phase :u1
    mask_seed :u7
    masked_kind :u8              # PacketKind ^ MASK[byte0][0]
    masked_connection_id :u16    # connection_id ^ MASK[byte0][1..2]
    len :u16
```

and replace `ProtectedHeader`:

```
format ProtectedHeader:
    control :u1          # 0 = application data, 1 = objproto-internal control frame
    nonce_counter :u63
```

Add a `ControlKind` enum next to the other enums:

```
enum ControlKind:
    :u8
    ping = 0x4b
```

Leave `enum PacketKind` in place — it stays the documented value space for the unmasked kind.

- [ ] **Step 4: Regenerate and confirm the packing**

From this repo's root:

```bash
./scripts/protoregen.sh $OBJTRSF/objproto/packet/packet.bgn
cd $OBJTRSF && grep -n "type PacketHeader" -A 6 objproto/packet/packet.go
```

Expected struct: a private backing field, then `MaskedKind uint8`, `MaskedConnectionId uint16`, `Len uint16`. Accessors `KeyPhase() bool`, `SetKeyPhase(bool) bool`, `MaskSeed() uint8`, `SetMaskSeed(uint8) bool` exist. If the shape differs, stop and report — the code below depends on it.

- [ ] **Step 5: Add the header helpers**

Append to `OBJTRSF/objproto/mask.go`:

```go
func headerByte0(h *packet.PacketHeader) byte {
	var b byte
	if h.KeyPhase() {
		b = 0x80
	}
	return b | h.MaskSeed()
}

func headerKind(h *packet.PacketHeader) packet.PacketKind {
	return unmaskKind(h.MaskedKind, headerByte0(h))
}

func headerConnID(h *packet.PacketHeader) uint16 {
	return unmaskConnID(h.MaskedConnectionId, headerByte0(h))
}

// buildHeader produces a header already in wire (masked) form. phase is
// ignored for non-application kinds, which carry no key phase.
func buildHeader(kind packet.PacketKind, connID uint16, length uint16, phase uint64) packet.PacketHeader {
	b0 := newMaskSeed(phase, kind == packet.PacketKind_Application)
	var h packet.PacketHeader
	h.SetKeyPhase(b0&0x80 != 0)
	h.SetMaskSeed(b0 & 0x7f)
	h.MaskedKind = maskKind(kind, b0)
	h.MaskedConnectionId = maskConnID(connID, b0)
	h.Len = length
	return h
}
```

- [ ] **Step 6: Update the four header literals**

In `OBJTRSF/objproto/objproto.go`, each of the four `packet.PacketHeader{...}` literals (`sendHandshake` ~588, `receiveHandshake` ~897, `sendApplication` ~1114, `makeProbe` ~1176) currently sets `Version`, `Kind`, `ConnectionId` and later `Len`. Replace each with a `buildHeader` call once the length is known. For example in `sendHandshake`:

```go
	data, err := hs.Append(nil)
	if err != nil {
		return nil, fmt.Errorf("failed to encode handshake: %w", err)
	}
	pkt := &packet.Packet{
		Header: buildHeader(packet.PacketKind_Handshake, cid.ID, uint16(len(data)), 0),
	}
	if !pkt.SetData(data) {
		return nil, fmt.Errorf("dictionary too large")
	}
```

`receiveHandshake` uses `packet.PacketKind_HandshakeAck` with `cid.ID`, `makeProbe` uses `packet.PacketKind_Probe` with `probeID`. `sendApplication` is handled in Step 8.

- [ ] **Step 7: Update the receive path**

In `receive()` (`objproto.go:1046`), derive the cid from the unmasked value and dispatch on the unmasked kind. Replace the cid line and every `pkt.Header.Kind` use in that function:

```go
	kind := headerKind(&pkt.Header)
	cid := NewConnectionID(transport, from, headerConnID(&pkt.Header))
```

then use `kind` at the proxy relay (`s.sendPacket(peer, kind, data)` and the log), at the `switch`, and in the two mode guards and the ack branch. Do not write back into `pkt.Header` — the raw buffer and the decoded struct both stay in wire form so transcripts match.

- [ ] **Step 8: Move the AAD to the wire bytes**

In `sendApplication` (`objproto.go:1112`), build the header with `buildHeader` before sealing and use its serialisation as AAD:

```go
	pktLen := 8 + len(data) + activeConn.connectionSecret.Overhead()
	if pktLen > 0xffff {
		return 0, 0, fmt.Errorf("data too large to send")
	}
	pkt := &packet.Packet{
		Header: buildHeader(packet.PacketKind_Application, cid.ID, uint16(pktLen), activeConn.sendPhase),
	}
	hdrData := pkt.Header.MustAppend(nil) // the 6 wire bytes, masked form
```

`activeConn.sendPhase` does not exist yet — for this task pass the literal `0` and change it to `activeConn.sendPhase` in Task 5. In `receiveApplication` (`objproto.go:985`) `hdrData := hdr.MustAppend(nil)` already reproduces the wire bytes because the struct is never mutated; leave the line where it is.

- [ ] **Step 9: Fix the generated-schema fuzz test**

`OBJTRSF/objproto/packet/protocol_test.go` builds `packet.PacketHeader{Kind: ...}` literals that no longer compile. Replace both literals with the masked field and unmask in the fuzz body:

```go
	validPacket := &packet.Packet{
		Header: packet.PacketHeader{MaskedKind: uint8(packet.PacketKind_Handshake), Len: 30},
		Data:   bytes.Repeat([]byte{0x32}, 30),
	}
	corpus := validPacket.MustAppend(nil)
	f.Add(corpus)
	invalidPacket := &packet.PacketHeader{MaskedKind: uint8(packet.PacketKind_Application), Len: 0xffff}
```

and in the fuzz body drop the `pkt.Header.Kind == packet.PacketKind_Handshake` guard, decoding the handshake unconditionally:

```go
	f.Fuzz(func(t *testing.T, data []byte) {
		pkt := &packet.Packet{}
		if err := pkt.DecodeExact(data); err != nil {
			return
		}
		hs := &packet.Handshake{}
		hs.DecodeExact(pkt.Data)
	})
```

The mask helpers live in package `objproto`, not `packet`, so this test cannot unmask; decoding unconditionally keeps the same crash surface.

- [ ] **Step 10: Add the transcript stability test**

Append to `OBJTRSF/objproto/mask_test.go`:

```go
func TestDecodeDoesNotMutateTheBuffer(t *testing.T) {
	// Transcripts are raw wire bytes and feed the harness PSK binder. If
	// decoding or unmasking ever writes back, the two ends diverge and
	// authentication fails.
	h := buildHeader(packet.PacketKind_Handshake, 0x1234, 4, 0)
	pkt := &packet.Packet{Header: h, Data: []byte{1, 2, 3, 4}}
	wire := pkt.MustAppend(nil)
	before := append([]byte(nil), wire...)

	var got packet.Packet
	if err := got.DecodeExact(wire); err != nil {
		t.Fatal(err)
	}
	_ = headerKind(&got.Header)
	_ = headerConnID(&got.Header)

	if !bytes.Equal(before, wire) {
		t.Fatalf("decode mutated the buffer:\nbefore % x\nafter  % x", before, wire)
	}
	if got.Header.MustAppend(nil)[0] != wire[0] {
		t.Fatal("re-serialising the decoded header does not reproduce the wire bytes")
	}
}
```

Add `"bytes"` to that file's imports.

- [ ] **Step 11: Build and run everything**

```bash
cd $OBJTRSF && go build ./... && go test ./objproto/... ./trsf/...
```

Expected: build clean, all tests pass, including the pre-existing `trsf` suite which drives objproto end to end.

- [ ] **Step 12: Commit**

```bash
cd $OBJTRSF && git add objproto/packet/packet.bgn objproto/packet/packet.go objproto/packet/protocol_test.go objproto/mask.go objproto/mask_test.go objproto/objproto.go
git commit -m "feat(objproto): replace the dead version byte with key_phase + mask seed

kind and connection_id are now XOR-masked on the wire with a public table
indexed by the first header byte; len stays cleartext. AAD is the six wire
header bytes. No key phase is acted on yet -- the bit is always zero."
```

---

### Task 4: Per-phase key derivation and the AES key-length fix

**Files:**
- Modify: `OBJTRSF/objproto/crypto.go:96-135` (`NewAEADFromCommonKeyKind`), `:88` area (`DeriveKey` unchanged)
- Modify: `OBJTRSF/objproto/objproto.go:740-783` (`KeyInfo`, `keySchedule`), `:786-860` (`addActiveConnection`), `:913`, `:963` (call sites)
- Create: `OBJTRSF/objproto/crypto_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks
- Produces:
  - `func aeadKeyLen(kind packet.CommonKeyKind) (int, error)`
  - `type phaseKeys struct { secret []byte; aead cipher.AEAD; iv []byte }`
  - `func derivePhaseKeys(secret []byte, kind packet.CommonKeyKind) (phaseKeys, error)`
  - `func ratchetSecret(secret []byte) ([]byte, error)`
  - `KeyInfo` fields renamed to `HSSecret`, `AckSecret`, `HsHeaderProtect`, `AckHeaderProtect` (the `HSIV` / `AckIV` fields are removed)

- [ ] **Step 1: Write the failing tests**

Create `OBJTRSF/objproto/crypto_test.go`:

```go
package objproto

import (
	"bytes"
	"testing"

	"github.com/on-keyday/objtrsf/objproto/packet"
)

func TestAEADKeyLen(t *testing.T) {
	cases := []struct {
		kind packet.CommonKeyKind
		want int
	}{
		{packet.CommonKeyKind_Aes128Gcm, 16},
		{packet.CommonKeyKind_Aes192Gcm, 24},
		{packet.CommonKeyKind_Aes256Gcm, 32},
		{packet.CommonKeyKind_Chacha20Poly1305, 32},
	}
	for _, c := range cases {
		got, err := aeadKeyLen(c.kind)
		if err != nil {
			t.Fatalf("%v: %v", c.kind, err)
		}
		if got != c.want {
			t.Fatalf("%v: key length %d, want %d", c.kind, got, c.want)
		}
	}
	if _, err := aeadKeyLen(packet.CommonKeyKind(0)); err == nil {
		t.Fatal("unknown key kind must be an error")
	}
}

func TestDerivePhaseKeysUsesTheNegotiatedKeyLength(t *testing.T) {
	// Regression: the old NewAEADFromCommonKeyKind handed a 32-byte slice to
	// aes.NewCipher for every AES kind, so a negotiated aes128_gcm silently
	// ran AES-256. A 12-byte GCM nonce and a 16-byte overhead hold for all of
	// them, so assert on the derived key length instead.
	secret := bytes.Repeat([]byte{0x11}, 32)
	for _, kind := range []packet.CommonKeyKind{
		packet.CommonKeyKind_Aes128Gcm,
		packet.CommonKeyKind_Aes192Gcm,
		packet.CommonKeyKind_Aes256Gcm,
		packet.CommonKeyKind_Chacha20Poly1305,
	} {
		pk, err := derivePhaseKeys(secret, kind)
		if err != nil {
			t.Fatalf("%v: %v", kind, err)
		}
		if len(pk.iv) != 12 {
			t.Fatalf("%v: iv is %d bytes, want 12", kind, len(pk.iv))
		}
		if pk.aead.NonceSize() != 12 {
			t.Fatalf("%v: nonce size %d, want 12", kind, pk.aead.NonceSize())
		}
	}
	if _, err := NewAEADFromCommonKeyKind(packet.CommonKeyKind_Aes128Gcm, bytes.Repeat([]byte{1}, 32)); err == nil {
		t.Fatal("aes128_gcm must reject a 32-byte key instead of silently running AES-256")
	}
}

func TestRatchetSecretMovesForward(t *testing.T) {
	s0 := bytes.Repeat([]byte{0x22}, 32)
	s1, err := ratchetSecret(s0)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := ratchetSecret(s1)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(s0, s1) || bytes.Equal(s1, s2) || bytes.Equal(s0, s2) {
		t.Fatal("ratchet produced a repeated secret")
	}
	if len(s1) != 32 || len(s2) != 32 {
		t.Fatalf("ratchet changed the secret length: %d %d", len(s1), len(s2))
	}
	again, err := ratchetSecret(s0)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(s1, again) {
		t.Fatal("ratchet is not deterministic")
	}
}
```

- [ ] **Step 2: Run them to verify they fail**

```bash
cd $OBJTRSF && go test ./objproto/ -run 'TestAEADKeyLen|TestDerivePhaseKeys|TestRatchetSecret' -v
```

Expected: FAIL to build, `undefined: aeadKeyLen`.

- [ ] **Step 3: Rewrite the AEAD constructor**

In `OBJTRSF/objproto/crypto.go`, add `aeadKeyLen` and make `NewAEADFromCommonKeyKind` require an exact length:

```go
func aeadKeyLen(kind packet.CommonKeyKind) (int, error) {
	switch kind {
	case packet.CommonKeyKind_Aes128Gcm:
		return 16, nil
	case packet.CommonKeyKind_Aes192Gcm:
		return 24, nil
	case packet.CommonKeyKind_Aes256Gcm, packet.CommonKeyKind_Chacha20Poly1305:
		return 32, nil
	default:
		return 0, fmt.Errorf("unsupported common key kind: %v", kind)
	}
}

func NewAEADFromCommonKeyKind(kind packet.CommonKeyKind, key []byte) (cipher.AEAD, error) {
	want, err := aeadKeyLen(kind)
	if err != nil {
		return nil, err
	}
	if len(key) != want {
		return nil, fmt.Errorf("invalid key length for %v: got %d, want %d", kind, len(key), want)
	}
	if kind == packet.CommonKeyKind_Chacha20Poly1305 {
		return chacha20poly1305.New(key)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("failed to create %v cipher: %w", kind, err)
	}
	return cipher.NewGCM(block)
}

type phaseKeys struct {
	secret []byte
	aead   cipher.AEAD
	iv     []byte
}

func derivePhaseKeys(secret []byte, kind packet.CommonKeyKind) (phaseKeys, error) {
	keyLen, err := aeadKeyLen(kind)
	if err != nil {
		return phaseKeys{}, err
	}
	key, err := DeriveKey(secret, "ksdk-protocol-key", keyLen)
	if err != nil {
		return phaseKeys{}, fmt.Errorf("failed to derive phase key: %w", err)
	}
	iv, err := DeriveKey(secret, "ksdk-protocol-nonce", 12)
	if err != nil {
		return phaseKeys{}, fmt.Errorf("failed to derive phase iv: %w", err)
	}
	aead, err := NewAEADFromCommonKeyKind(kind, key)
	if err != nil {
		return phaseKeys{}, err
	}
	clear(key)
	return phaseKeys{secret: secret, aead: aead, iv: iv}, nil
}

func ratchetSecret(secret []byte) ([]byte, error) {
	return DeriveKey(secret, "ksdk-protocol-ku", 32)
}
```

- [ ] **Step 4: Rework the key schedule**

In `OBJTRSF/objproto/objproto.go`, `KeyInfo` drops the IV fields and renames the masters to secrets:

```go
type KeyInfo struct {
	HSSecret         []byte
	AckSecret        []byte
	HsHeaderProtect  []byte
	AckHeaderProtect []byte
}
```

In `keySchedule`, delete the two `ksdk-protocol-nonce-*` derivations and return the renamed fields. The `ksdk-protocol-master-hs` / `-ack` derivations stay exactly as they are — only what the caller does with the result changes.

- [ ] **Step 5: Derive the current phase inside addActiveConnection**

Change `addActiveConnection` to take secrets rather than AEADs:

```go
func (s *endpoint) addActiveConnection(cid ConnectionID, selfSecret []byte, peerSecret []byte,
	commonKeyKind packet.CommonKeyKind,
	selfHeaderProtect []byte, peerHeaderProtect []byte,
	transcript []byte, hsDone chan Connection, proxyConn *activeConnection) error {
```

At the top of the body, derive both directions and fail before mutating any state:

```go
	sendKeys, err := derivePhaseKeys(selfSecret, commonKeyKind)
	if err != nil {
		return fmt.Errorf("failed to derive send keys: %w", err)
	}
	recvKeys, err := derivePhaseKeys(peerSecret, commonKeyKind)
	if err != nil {
		return fmt.Errorf("failed to derive receive keys: %w", err)
	}
```

Then set `connectionSecret = sendKeys.aead`, `selfIV = sendKeys.iv`, `peerSecret = recvKeys.aead`, `peerIV = recvKeys.iv` in both the proxy-reuse and fresh-construction branches, exactly where the old parameters were assigned. Also store `commonKeyKind` on the connection — add the field to `activeConnection`; Task 5 needs it to re-derive.

Leave the existing `selfHeaderProtect[:16] / [:24] / [:32]` slicing alone. The header-protection key is deliberately outside the ratchet and its sizing is already correct.

- [ ] **Step 6: Update the two call sites**

`receiveHandshake` (`objproto.go:913`) and `receiveHandshakeAck` (`objproto.go:963`) currently build `selfAEAD` / `peerAEAD` with `NewAEADFromCommonKeyKind` and pass IVs. Delete those four `NewAEADFromCommonKeyKind` calls and pass the secrets straight through, keeping the same direction mapping:

- `receiveHandshake` (server side): `addActiveConnection(cid, keys.AckSecret, keys.HSSecret, commonKeyKind, keys.AckHeaderProtect, keys.HsHeaderProtect, ...)`
- `receiveHandshakeAck` (client side): `addActiveConnection(cid, keys.HSSecret, keys.AckSecret, commonKeyKind, keys.HsHeaderProtect, keys.AckHeaderProtect, ...)`

- [ ] **Step 7: Build and test**

```bash
cd $OBJTRSF && go build ./... && go test ./objproto/... ./trsf/...
```

Expected: all pass. The `trsf` suite establishing real connections is the proof that phase-0 keys still agree across both ends.

- [ ] **Step 8: Commit**

```bash
cd $OBJTRSF && git add objproto/crypto.go objproto/crypto_test.go objproto/objproto.go
git commit -m "feat(objproto): derive traffic keys per phase from a ratchetable secret

hs/ack master secrets stop being AEAD keys directly; key and IV are now
derived from them per phase. Fixes NewAEADFromCommonKeyKind handing a
32-byte key to aes.NewCipher for every AES kind, which made a negotiated
aes128_gcm run AES-256."
```

---

### Task 5: Key update state machine

**Files:**
- Modify: `OBJTRSF/objproto/objproto.go:252-272` (`activeConnection` fields), `:786-860` (initialise phase state), `:971-1011` (`receiveApplication`), `:1112-1150` (`sendApplication`)
- Create: `OBJTRSF/objproto/keyupdate_test.go`

**Interfaces:**
- Consumes: Task 4's `phaseKeys`, `derivePhaseKeys`, `ratchetSecret`; Task 3's `buildHeader`
- Produces:
  - `func (a *activeConnection) ratchetSendLocked() error`
  - `func (a *activeConnection) openWithPhase(bit byte, pn PacketNumber, nonce, ciphertext, aad []byte) ([]byte, error)`

- [ ] **Step 1: Add the phase state fields**

In `activeConnection` (`objproto.go:252`), keep the existing `connectionSecret` / `peerSecret` / `selfIV` / `peerIV` as the *current* phase and add:

```go
	commonKeyKind  packet.CommonKeyKind
	sendSecret     []byte
	sendPhase      uint64
	sendPhaseFrom  PacketNumber
	sendPhaseAt    time.Time
	recvSecret     []byte
	recvPhase      uint64
	recvPhaseFrom  PacketNumber
	prevPeerSecret cipher.AEAD
	prevPeerIV     []byte
	prevExpiry     time.Time
	nextPeerSecret cipher.AEAD
	nextPeerIV     []byte
	nextRecvSecret []byte
```

In `addActiveConnection`, set `sendSecret = sendKeys.secret`, `recvSecret = recvKeys.secret`, both phases and both `*PhaseFrom` to `0`, `sendPhaseAt = now`, and precompute the next receive phase:

```go
	nextSecret, err := ratchetSecret(recvKeys.secret)
	if err != nil {
		return fmt.Errorf("failed to precompute next receive secret: %w", err)
	}
	nextKeys, err := derivePhaseKeys(nextSecret, commonKeyKind)
	if err != nil {
		return fmt.Errorf("failed to precompute next receive keys: %w", err)
	}
```

assigning `nextPeerSecret = nextKeys.aead`, `nextPeerIV = nextKeys.iv`, `nextRecvSecret = nextSecret`. In the proxy-reuse branch clear `prevPeerSecret`, `prevPeerIV` and `prevExpiry` so a reused connection does not inherit a stale previous key.

- [ ] **Step 2: Write the test harness**

Create `OBJTRSF/objproto/keyupdate_test.go` with the two-endpoint harness. It
lives in package `objproto` because the tests read private phase state, so it
cannot reuse `transport/mock.go` (that package imports objproto). Delivery is
synchronous and manual so a test can capture a packet without delivering it.

```go
package objproto

import (
	"crypto/ecdh"
	"io"
	"log/slog"
	"net/netip"
	"testing"
	"time"

	"github.com/on-keyday/objtrsf/objproto/packet"
)

type testPair struct {
	client, server *activeConnection
	// pump delivers everything queued on both endpoints until both are quiet.
	pump func()
	// toServer and toClient inject one raw datagram from the correct source
	// address, for tests that hand-modify bytes.
	toServer func([]byte) error
	toClient func([]byte) error
}

func newConnectedPair(t *testing.T) *testPair {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cliEP := NewEndpoint(logger, EndpointModeClient).(*endpoint)
	srvEP := NewEndpoint(logger, EndpointModeServer).(*endpoint)

	srvAddr := netip.MustParseAddrPort("127.0.0.1:9001")
	cliAddr := netip.MustParseAddrPort("127.0.0.1:9002")
	srvCID := NewConnectionID("mock", srvAddr, 0x1111)

	// A packet the client queued arrives at the server FROM the client's
	// address, and vice versa. Getting this backwards silently creates two
	// unrelated connection ids and the handshake never completes.
	toServer := func(b []byte) error { return srvEP.Receive("mock", cliAddr, b) }
	toClient := func(b []byte) error { return cliEP.Receive("mock", srvAddr, b) }

	pump := func() {
		t.Helper()
		for {
			select {
			case pkt := <-cliEP.GetSenderChannel():
				if err := toServer(pkt.Data); err != nil {
					t.Logf("server dropped a packet: %v", err)
				}
			case pkt := <-srvEP.GetSenderChannel():
				if err := toClient(pkt.Data); err != nil {
					t.Logf("client dropped a packet: %v", err)
				}
			default:
				return
			}
		}
	}

	priv, hs, err := NewECDHHandshake(ecdh.X25519(), packet.CommonKeyKind_Aes128Gcm)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := cliEP.SendHandshake(srvCID, priv, hs)
	if err != nil {
		t.Fatal(err)
	}
	pump() // handshake to the server, ack back to the client

	clientConn, err := ch.WaitWithTimeout(t.Context(), time.Second)
	if err != nil {
		t.Fatalf("client handshake did not complete: %v", err)
	}
	serverConn, err := srvEP.WaitNewActiveConnection(time.Second)
	if err != nil {
		t.Fatalf("server handshake did not complete: %v", err)
	}
	return &testPair{
		client:   clientConn.(*activeConnection),
		server:   serverConn.(*activeConnection),
		pump:     pump,
		toServer: toServer,
		toClient: toClient,
	}
}

// captureNextPacket sends a packet and returns its raw bytes WITHOUT
// delivering it, so a test can reorder or corrupt it.
func captureNextPacket(t *testing.T, a *activeConnection, payload []byte) []byte {
	t.Helper()
	if _, _, err := a.endpoint.sendApplication(a.cid, payload, a, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case pkt := <-a.endpoint.GetSenderChannel():
		return append([]byte(nil), pkt.Data...)
	case <-time.After(time.Second):
		t.Fatal("no packet was queued")
		return nil
	}
}

// ratchet advances the send side the way the triggers do, for tests that need
// a phase change at an exact point.
func ratchet(t *testing.T, a *activeConnection) {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.ratchetSendLocked(); err != nil {
		t.Fatal(err)
	}
}
```

- [ ] **Step 2b: Verify the harness itself works**

Add this first and run it alone. If the handshake wiring is wrong, every later
test fails for the same uninformative reason.

```go
func TestHarnessRoundTrip(t *testing.T) {
	p := newConnectedPair(t)
	if _, _, err := p.client.endpoint.sendApplication(p.client.cid, []byte("hi"), p.client, nil); err != nil {
		t.Fatal(err)
	}
	p.pump()
	msg, err := p.server.ReceiveMessageTimeout(t.Context(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if string(msg.Data) != "hi" {
		t.Fatalf("got %q", msg.Data)
	}
}
```

```bash
cd $OBJTRSF && go test ./objproto/ -run TestHarnessRoundTrip -v
```

Expected: PASS. This works with Task 3 and 4 already landed and no phase logic yet.

- [ ] **Step 2c: Write the failing key-update tests**

Append to `OBJTRSF/objproto/keyupdate_test.go`:

```go
func TestPeerFollowsPhaseAdvance(t *testing.T) {
	p := newConnectedPair(t)
	if _, _, err := p.client.endpoint.sendApplication(p.client.cid, []byte("phase0"), p.client, nil); err != nil {
		t.Fatal(err)
	}
	p.pump()
	ratchet(t, p.client)
	if _, _, err := p.client.endpoint.sendApplication(p.client.cid, []byte("phase1"), p.client, nil); err != nil {
		t.Fatal(err)
	}
	p.pump()

	for _, want := range []string{"phase0", "phase1"} {
		msg, err := p.server.ReceiveMessageTimeout(t.Context(), time.Second)
		if err != nil {
			t.Fatalf("%s: %v", want, err)
		}
		if string(msg.Data) != want {
			t.Fatalf("got %q want %q", msg.Data, want)
		}
	}
	p.server.mu.Lock()
	defer p.server.mu.Unlock()
	if p.server.recvPhase != 1 {
		t.Fatalf("receiver stayed on phase %d", p.server.recvPhase)
	}
	if p.server.prevPeerSecret == nil {
		t.Fatal("receiver dropped the previous key immediately")
	}
}

func TestPacketNumberStaysMonotonicAcrossUpdate(t *testing.T) {
	p := newConnectedPair(t)
	var last PacketNumber
	for i := 0; i < 4; i++ {
		if i == 2 {
			ratchet(t, p.client)
		}
		_, pn, err := p.client.endpoint.sendApplication(p.client.cid, []byte("x"), p.client, nil)
		if err != nil {
			t.Fatal(err)
		}
		if pn <= last {
			t.Fatalf("packet number went backwards across an update: %d after %d", pn, last)
		}
		last = pn
		p.pump()
	}
}

func TestFlippedPhaseBitIsDroppedWithoutStateChange(t *testing.T) {
	p := newConnectedPair(t)
	raw := captureNextPacket(t, p.client, []byte("hello"))
	raw[0] ^= 0x80 // the phase bit is inside the AAD

	p.server.mu.Lock()
	before := p.server.recvPhase
	p.server.mu.Unlock()

	if err := p.toServer(raw); err == nil {
		t.Fatal("a packet with a flipped phase bit must not decrypt")
	}
	p.server.mu.Lock()
	defer p.server.mu.Unlock()
	if p.server.recvPhase != before {
		t.Fatalf("receive phase advanced on a failed packet: %d -> %d", before, p.server.recvPhase)
	}
	if !p.server.IsActive() {
		t.Fatal("connection was torn down by one bad packet")
	}
}

func TestPreviousPhasePacketDecryptsInsideRetention(t *testing.T) {
	p := newConnectedPair(t)
	// Both phase-0 packets must be captured BEFORE the ratchet, because
	// captureNextPacket always seals under the client's current phase.
	old := captureNextPacket(t, p.client, []byte("late"))
	old2 := captureNextPacket(t, p.client, []byte("too late"))

	ratchet(t, p.client)
	if _, _, err := p.client.endpoint.sendApplication(p.client.cid, []byte("new"), p.client, nil); err != nil {
		t.Fatal(err)
	}
	p.pump()
	if _, err := p.server.ReceiveMessageTimeout(t.Context(), time.Second); err != nil {
		t.Fatal(err)
	}

	// The phase-0 packet that got overtaken must still open.
	if err := p.toServer(old); err != nil {
		t.Fatalf("in-retention previous-phase packet was rejected: %v", err)
	}

	// Expire the previous key, then replay the second phase-0 packet.
	p.server.mu.Lock()
	p.server.prevExpiry = time.Now().Add(-time.Second)
	p.server.mu.Unlock()
	if err := p.toServer(old2); err == nil {
		t.Fatal("previous-phase packet must be dropped after the retention window")
	}
}
```

- [ ] **Step 3: Run them to verify they fail**

```bash
cd $OBJTRSF && go test ./objproto/ -run 'TestPeerFollows|TestPacketNumberStays|TestFlippedPhase|TestPreviousPhase' -v
```

Expected: FAIL — `newConnectedPair` has no body and `ratchetSendLocked` is undefined. Write the harness helpers first, confirm they compile and the tests fail on the missing behaviour, then continue.

- [ ] **Step 4: Implement the send-side ratchet**

Add to `OBJTRSF/objproto/objproto.go`. The caller must hold `a.mu`.

```go
func (a *activeConnection) ratchetSendLocked() error {
	next, err := ratchetSecret(a.sendSecret)
	if err != nil {
		return fmt.Errorf("failed to ratchet send secret: %w", err)
	}
	keys, err := derivePhaseKeys(next, a.commonKeyKind)
	if err != nil {
		return err
	}
	clear(a.sendSecret)
	a.sendSecret = next
	a.connectionSecret = keys.aead
	a.selfIV = keys.iv
	a.sendPhase++
	a.sendPhaseFrom = PacketNumber(a.sentCounter.Load())
	a.sendPhaseAt = time.Now()
	return nil
}
```

- [ ] **Step 5: Implement the receive-side follow**

Replace the single `Open` call in `receiveApplication` with phase selection. `bit` comes from the cleartext first header byte, `pn` from `ProtectedHeader.NonceCounter()`.

```go
	bit := byte(0)
	if hdr.KeyPhase() {
		bit = 1
	}

	var plaintext []byte
	switch {
	case bit == byte(activeConn.recvPhase&1):
		plaintext, err = open(activeConn.peerSecret, activeConn.peerIV, nonce, ciphertext, hdrData)
	case pn < activeConn.recvPhaseFrom && activeConn.prevPeerSecret != nil && time.Now().Before(activeConn.prevExpiry):
		plaintext, err = open(activeConn.prevPeerSecret, activeConn.prevPeerIV, nonce, ciphertext, hdrData)
	default:
		plaintext, err = open(activeConn.nextPeerSecret, activeConn.nextPeerIV, nonce, ciphertext, hdrData)
		if err == nil {
			if cerr := activeConn.commitRecvPhaseLocked(pn); cerr != nil {
				return cerr
			}
		}
	}
	if err != nil {
		s.logger.Warn("failed to decrypt application data", "cid", cid.String(), "error", err)
		return fmt.Errorf("failed to decrypt data: %w", err)
	}
```

with the two helpers:

```go
// open builds the per-packet nonce from the phase IV and decrypts. nonce must
// already carry the 8-byte counter in its last 8 bytes.
func open(aead cipher.AEAD, iv, nonce, ciphertext, aad []byte) ([]byte, error) {
	n := make([]byte, len(nonce))
	subtle.XORBytes(n, iv, nonce)
	return aead.Open(ciphertext[:0], n, ciphertext, aad)
}

// commitRecvPhaseLocked promotes the precomputed next phase. Only ever called
// after a successful decryption, so a forged phase bit costs one AEAD open and
// changes nothing.
func (a *activeConnection) commitRecvPhaseLocked(pn PacketNumber) error {
	a.prevPeerSecret = a.peerSecret
	a.prevPeerIV = a.peerIV
	a.prevExpiry = time.Now().Add(prevKeyRetention)
	a.peerSecret = a.nextPeerSecret
	a.peerIV = a.nextPeerIV
	clear(a.recvSecret)
	a.recvSecret = a.nextRecvSecret
	a.recvPhase++
	a.recvPhaseFrom = pn

	next, err := ratchetSecret(a.recvSecret)
	if err != nil {
		return fmt.Errorf("failed to precompute next receive secret: %w", err)
	}
	keys, err := derivePhaseKeys(next, a.commonKeyKind)
	if err != nil {
		return err
	}
	a.nextRecvSecret = next
	a.nextPeerSecret = keys.aead
	a.nextPeerIV = keys.iv
	return nil
}
```

The existing code XORs the IV into the nonce in place before `Open`; move that into `open` so each branch uses its own phase IV. Keep the replay-tracker dry-run check ahead of decryption exactly where it is, and keep the committing `InsertNonce` after a successful decrypt.

Declare `prevKeyRetention = 3 * time.Second` as a package constant in this file.

- [ ] **Step 6: Use the send phase in the header**

In `sendApplication`, change the `buildHeader` call's last argument from the literal `0` left by Task 3 to `activeConn.sendPhase`.

- [ ] **Step 7: Run the tests**

```bash
cd $OBJTRSF && go test ./objproto/ -run 'TestPeerFollows|TestPacketNumberStays|TestFlippedPhase|TestPreviousPhase' -v
cd $OBJTRSF && go test ./objproto/... ./trsf/...
```

Expected: all pass.

- [ ] **Step 8: Commit**

```bash
cd $OBJTRSF && git add objproto/objproto.go objproto/keyupdate_test.go
git commit -m "feat(objproto): follow the peer's key phase with prev/current/next keys

Each direction ratchets independently and the peer follows the phase bit.
Phase commit happens only after a successful decryption, so a forged bit
costs one AEAD open. Packet numbers and the replay tracker are untouched."
```

---

### Task 6: Packet-count trigger and the UpdateKey API

**Files:**
- Modify: `OBJTRSF/objproto/objproto.go:1112` (`sendApplication`), `:349` area (`activeConnection` methods)
- Modify: `OBJTRSF/objproto/session.go:97-119` (`Connection` interface)
- Modify: `OBJTRSF/objproto/keyupdate_test.go`

**Interfaces:**
- Consumes: Task 5's `ratchetSendLocked`
- Produces: `func (a *activeConnection) UpdateKey() error`, added to the `Connection` interface

- [ ] **Step 1: Write the failing tests**

Append to `OBJTRSF/objproto/keyupdate_test.go`:

```go
func TestPacketCountTriggerAdvancesThePhase(t *testing.T) {
	orig := keyUpdatePackets
	keyUpdatePackets = 8
	t.Cleanup(func() { keyUpdatePackets = orig })

	p := newConnectedPair(t)
	for i := uint64(0); i < keyUpdatePackets+2; i++ {
		if _, _, err := p.client.endpoint.sendApplication(p.client.cid, []byte("x"), p.client, nil); err != nil {
			t.Fatal(err)
		}
		p.pump()
	}
	p.client.mu.Lock()
	defer p.client.mu.Unlock()
	if p.client.sendPhase == 0 {
		t.Fatal("send phase never advanced past the packet threshold")
	}
}

func TestUpdateKeyRespectsTheFloor(t *testing.T) {
	p := newConnectedPair(t)
	if err := p.client.UpdateKey(); err != nil {
		t.Fatalf("first update rejected: %v", err)
	}
	if err := p.client.UpdateKey(); err == nil {
		t.Fatal("a second update inside the floor must be refused")
	}
}
```

The real threshold is 2^22, far too many packets for a test, so
`keyUpdatePackets` and `minPacketsBetweenUpdates` are package-level `var`s
rather than `const`s specifically so tests can lower them. The floor test
relies on `minPacketsBetweenUpdates` staying at its default 1024: the second
`UpdateKey` is refused because almost no packets have been sent since the
first.

- [ ] **Step 2: Run to verify failure**

```bash
cd $OBJTRSF && go test ./objproto/ -run 'TestPacketCountTrigger|TestUpdateKeyRespects' -v
```

Expected: FAIL, `c.UpdateKey undefined`.

- [ ] **Step 3: Implement the trigger and the API**

Add to `OBJTRSF/objproto/objproto.go`:

```go
var (
	keyUpdatePackets         uint64 = 1 << 22
	minPacketsBetweenUpdates uint64 = 1024
	minTimeBetweenUpdates           = 1 * time.Second
)

// canUpdateLocked reports whether another send-side update is allowed. The
// receiver holds only prev/current/next, so it can follow one advance at a
// time; updating twice inside the peer's reordering window would strand it.
func (a *activeConnection) canUpdateLocked() bool {
	sent := a.sentCounter.Load()
	if sent < uint64(a.sendPhaseFrom)+minPacketsBetweenUpdates {
		return false
	}
	return time.Since(a.sendPhaseAt) >= minTimeBetweenUpdates
}

func (a *activeConnection) UpdateKey() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sendPhase > 0 && !a.canUpdateLocked() {
		return fmt.Errorf("key update refused: previous update was too recent")
	}
	return a.ratchetSendLocked()
}
```

In `sendApplication`, immediately after `count` is chosen and before the header is built:

```go
	if count-uint64(activeConn.sendPhaseFrom) >= keyUpdatePackets && activeConn.canUpdateLocked() {
		if err := activeConn.ratchetSendLocked(); err != nil {
			return 0, 0, err
		}
	}
```

Add `UpdateKey() error` to the `Connection` interface in `OBJTRSF/objproto/session.go`, beside `Close() error`.

- [ ] **Step 4: Run the tests**

```bash
cd $OBJTRSF && go test ./objproto/... ./trsf/...
```

Expected: all pass. `trsf/mock` and the trsf test fakes implement `UnderlayingSendTransport`, not `Connection`, so they need no change — confirm by the build succeeding.

- [ ] **Step 5: Commit**

```bash
cd $OBJTRSF && git add objproto/objproto.go objproto/session.go objproto/keyupdate_test.go
git commit -m "feat(objproto): trigger key update on packet count and expose UpdateKey

The floor keeps the sender from advancing twice inside the peer's
reordering window, which the prev/current/next receiver cannot follow."
```

---

### Task 7: Control frame and the time trigger

**Files:**
- Create: `OBJTRSF/objproto/control.go`
- Modify: `OBJTRSF/objproto/objproto.go:971-1011` (`receiveApplication`), `:1112-1150` (`sendApplication`)
- Modify: `OBJTRSF/objproto/session.go:60-68` area (`AutoKeyUpdate`)
- Modify: `OBJTRSF/objproto/keyupdate_test.go`

**Interfaces:**
- Consumes: Task 6's `UpdateKey`; the `ProtectedHeader` and `ControlKind` from Task 3's schema
- Produces:
  - `func (s *endpoint) sendControl(cid ConnectionID, a *activeConnection, kind packet.ControlKind) error`
  - `func AutoKeyUpdate(s Endpoint, interval, maxKeyAge time.Duration)`

- [ ] **Step 1: Write the failing tests**

Append to `OBJTRSF/objproto/keyupdate_test.go`:

```go
func TestPingIsNotDeliveredToTheApplication(t *testing.T) {
	p := newConnectedPair(t)
	if err := p.client.endpoint.sendControl(p.client.cid, p.client, packet.ControlKind_Ping); err != nil {
		t.Fatal(err)
	}
	p.pump()
	if _, _, err := p.client.endpoint.sendApplication(p.client.cid, []byte("real"), p.client, nil); err != nil {
		t.Fatal(err)
	}
	p.pump()
	msg, err := p.server.ReceiveMessageTimeout(t.Context(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if string(msg.Data) != "real" {
		t.Fatalf("control frame leaked to the application: %q", msg.Data)
	}
}

func TestUpdateKeyEmitsAPingThatMovesThePeer(t *testing.T) {
	p := newConnectedPair(t)
	if err := p.client.UpdateKey(); err != nil {
		t.Fatal(err)
	}
	p.pump()
	p.server.mu.Lock()
	defer p.server.mu.Unlock()
	if p.server.recvPhase != 1 {
		t.Fatalf("idle peer did not follow the update: phase %d", p.server.recvPhase)
	}
}

func TestControlBitDoesNotCorruptTheReplayTracker(t *testing.T) {
	p := newConnectedPair(t)
	if err := p.client.endpoint.sendControl(p.client.cid, p.client, packet.ControlKind_Ping); err != nil {
		t.Fatal(err)
	}
	p.pump()
	p.server.mu.Lock()
	defer p.server.mu.Unlock()
	if p.server.recvTracker.largestNonce > 1<<32 {
		t.Fatalf("control bit leaked into the replay counter: %d", p.server.recvTracker.largestNonce)
	}
}
```

The `packet` import is already present from the harness in Task 5.

- [ ] **Step 2: Run to verify failure**

```bash
cd $OBJTRSF && go test ./objproto/ -run 'TestPing|TestUpdateKeyEmits|TestControlBit' -v
```

Expected: FAIL, `sendControl` undefined.

- [ ] **Step 3: Route the protected prefix through the accessors**

In `sendApplication`, replace the raw 8-byte counter write with the generated struct so the control bit has a defined home:

```go
	var prot packet.ProtectedHeader
	prot.SetControl(control)
	prot.SetNonceCounter(count)
	protBytes := prot.MustAppend(nil) // 8 bytes
	copy(finalData[:8], protBytes)
	copy(nonce[4:], protBytes)
```

In `receiveApplication`, replace `nonceCounter := binary.BigEndian.Uint64(data[:8])` with a decode, so bit 63 never reaches the tracker:

```go
	var prot packet.ProtectedHeader
	off := 0
	if err := prot.DecodeSlice(data[:8], &off); err != nil {
		return fmt.Errorf("failed to decode protected header: %w", err)
	}
	nonceCounter := prot.NonceCounter() // bits 0..62; the control bit is excluded
```

The nonce material stays the full 8 bytes (`copy(nonce[4:], data[:8])`), so the control bit is authenticated by the AEAD.

- [ ] **Step 4: Add the control frame**

Create `OBJTRSF/objproto/control.go`:

```go
package objproto

import (
	"fmt"

	"github.com/on-keyday/objtrsf/objproto/packet"
)

// sendControl emits an objproto-internal frame. It is an ordinary application
// packet with the protected header's control bit set, so it is encrypted,
// carries the current key phase, and consumes a packet number like any other
// packet. It is never surfaced to the application.
//
// Its one caller today is the time-based key update: an idle connection has no
// data packet to carry a new phase bit.
func (s *endpoint) sendControl(cid ConnectionID, a *activeConnection, kind packet.ControlKind) error {
	_, _, err := s.sendApplicationFrame(cid, []byte{byte(kind)}, a, nil, true)
	return err
}

func (s *endpoint) handleControl(a *activeConnection, plaintext []byte) error {
	if len(plaintext) < 1 {
		return fmt.Errorf("empty control frame")
	}
	switch packet.ControlKind(plaintext[0]) {
	case packet.ControlKind_Ping:
		// The packet itself was the payload: receiving it already moved the
		// peer's phase in receiveApplication. Nothing further to do.
		return nil
	default:
		// Unknown control kinds are ignored rather than fatal, so a future
		// kind does not tear down a connection with an older peer.
		s.logger.Debug("ignoring unknown control frame", "cid", a.cid.String(), "kind", plaintext[0])
		return nil
	}
}
```

Rename the existing `sendApplication` body to `sendApplicationFrame(cid, data, a, pn, control bool)` and make `sendApplication` a thin wrapper passing `control=false`, so both paths share the sealing code rather than duplicating it.

In `receiveApplication`, after a successful decrypt, branch before delivery:

```go
	activeConn.recvTracker.InsertNonce(nonceCounter, time.Now(), false)
	activeConn.lastTime = time.Now()
	if prot.Control() {
		return s.handleControl(activeConn, plaintext)
	}
	activeConn.msgs.SendMessage(Message{...})
```

- [ ] **Step 5: Make UpdateKey emit the ping**

`UpdateKey` currently only ratchets. Send the ping after releasing the lock, because `sendApplicationFrame` takes `a.mu` itself:

```go
func (a *activeConnection) UpdateKey() error {
	a.mu.Lock()
	if a.sendPhase > 0 && !a.canUpdateLocked() {
		a.mu.Unlock()
		return fmt.Errorf("key update refused: previous update was too recent")
	}
	if err := a.ratchetSendLocked(); err != nil {
		a.mu.Unlock()
		return err
	}
	a.mu.Unlock()
	return a.endpoint.sendControl(a.cid, a, packet.ControlKind_Ping)
}
```

The packet-count trigger inside `sendApplicationFrame` does **not** send a ping: the data packet being sent already carries the new phase.

- [ ] **Step 6: Add AutoKeyUpdate**

In `OBJTRSF/objproto/session.go`, beside `AutoGarbageCollect` and `AutoRespondProbes`:

```go
// AutoKeyUpdate rekeys connections whose current phase has been in use longer
// than maxKeyAge. The packet-count trigger inside the send path covers busy
// connections; this covers idle ones, which would otherwise hold one key for
// the life of the connection.
func AutoKeyUpdate(s Endpoint, interval, maxKeyAge time.Duration) {
	ticker := time.NewTicker(interval)
	for range ticker.C {
		for _, conn := range s.ListActiveConnections() {
			if time.Since(conn.KeyPhaseAt()) < maxKeyAge {
				continue
			}
			if err := conn.UpdateKey(); err != nil {
				continue // refused by the floor; the next tick retries
			}
		}
	}
}
```

Add `KeyPhaseAt() time.Time` to the `Connection` interface and implement it on `activeConnection` returning `sendPhaseAt` under the lock. Declare `keyUpdateInterval = 10 * time.Minute` as the documented default for `maxKeyAge` in the same file.

- [ ] **Step 7: Verify the trsf packet-number gap assumption**

Control frames consume packet numbers that trsf never issued, so trsf sees gaps in its own numbering.

```bash
cd $OBJTRSF && grep -n "largestSent\|largestAcked\|PacketNumber" trsf/ack_handler.go | head -30
```

Confirm `ack_handler` works from its own record of sent packets (`sentRanges`) and never assumes contiguous numbering. If it does assume contiguity, stop and report — the fix belongs in a separate task, and the time trigger must not ship until it is resolved.

- [ ] **Step 8: Run everything**

```bash
cd $OBJTRSF && go build ./... && go test ./objproto/... ./trsf/...
```

Expected: all pass.

- [ ] **Step 9: Commit**

```bash
cd $OBJTRSF && git add objproto/control.go objproto/objproto.go objproto/session.go objproto/keyupdate_test.go
git commit -m "feat(objproto): add the internal control frame and the time-based rekey

Makes the previously unused ProtectedHeader real: control=1 marks an
objproto-internal frame that never reaches the application. Reading the
counter through NonceCounter() keeps the control bit out of the replay
tracker. UpdateKey emits a ping so an idle peer learns the new phase."
```

---

### Task 8: Harness adoption

**Files:**
- Modify: `go.mod`, `go.sum` (this repo)
- Modify: `server/conn_list_test.go:67` area (`fakeRawConn`)
- Modify: wherever `AutoGarbageCollect` is called

**Interfaces:**
- Consumes: the published objtrsf with `Connection.UpdateKey() error` and `Connection.KeyPhaseAt() time.Time`
- Produces: a harness that builds and tests against the new objproto

- [ ] **Step 1: Land and publish objtrsf**

Follow the `landing-to-main` skill for objtrsf: rebase the work onto current trunk and fast-forward push. Never cherry-pick to the remote.

- [ ] **Step 2: Bump the dependency**

```bash
go get github.com/on-keyday/objtrsf@latest && go mod tidy
```

- [ ] **Step 3: Find every break**

```bash
make check 2>&1 | head -40
go vet ./... 2>&1 | head -40
```

Expected breaks: `server/conn_list_test.go` `fakeRawConn` lacks `UpdateKey` and `KeyPhaseAt`. Use `make check`, not `go build ./...` — the latter hides pattern breaks.

- [ ] **Step 4: Implement the fake's new methods**

In `server/conn_list_test.go`, beside the existing one-line stubs:

```go
func (f *fakeRawConn) UpdateKey() error          { return nil }
func (f *fakeRawConn) KeyPhaseAt() time.Time     { return time.Time{} }
```

Match the surrounding stub style exactly — the neighbouring methods are single-line bodies.

- [ ] **Step 5: Wire AutoKeyUpdate**

```bash
grep -rn "AutoGarbageCollect" --include='*.go' .
```

At each call site, start `AutoKeyUpdate` the same way — as a goroutine alongside it, with `interval` matching the existing GC interval and `maxKeyAge` of `10 * time.Minute`. Follow whatever launch pattern the neighbouring `AutoGarbageCollect` call already uses at that site rather than inventing a new one.

- [ ] **Step 6: Verify**

```bash
make check && make wasm-check && make vet && make test
```

Expected: all green. Use the make targets, not ad-hoc `go build ./...`.

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum server/conn_list_test.go
git add -u
git commit -m "feat(objproto): adopt key phase and header masking

Bumps objtrsf for the new wire format, implements the two new Connection
methods on the server test fake, and starts AutoKeyUpdate alongside the
existing AutoGarbageCollect loops."
```

---

### Task 9: End-to-end verification

**Files:**
- Create: `OBJTRSF/objproto/keyupdate_integration_test.go`

**Interfaces:**
- Consumes: everything above
- Produces: evidence the design survives real traffic and a real harness

- [ ] **Step 1: Write the integration test**

Create `OBJTRSF/objproto/keyupdate_integration_test.go`:

```go
package objproto

import (
	"bytes"
	"crypto/rand"
	"testing"
)

// Drives enough traffic through a real pair to cross the update threshold many
// times, and checks every byte survives. Lowering the threshold is what makes
// this cheap enough to run in the normal suite.
func TestBulkTransferAcrossManyKeyUpdates(t *testing.T) {
	origPackets, origFloor := keyUpdatePackets, minPacketsBetweenUpdates
	keyUpdatePackets, minPacketsBetweenUpdates = 64, 8
	t.Cleanup(func() { keyUpdatePackets, minPacketsBetweenUpdates = origPackets, origFloor })

	p := newConnectedPair(t)
	const chunks = 2000
	payload := make([]byte, 512)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	var sent, got bytes.Buffer
	for i := 0; i < chunks; i++ {
		if _, _, err := p.client.endpoint.sendApplication(p.client.cid, payload, p.client, nil); err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
		sent.Write(payload)
		p.pump()
		msg, err := p.server.ReceiveMessage()
		if err != nil {
			t.Fatalf("chunk %d: %v", i, err)
		}
		got.Write(msg.Data)
	}
	if !bytes.Equal(sent.Bytes(), got.Bytes()) {
		t.Fatal("payload corrupted across key updates")
	}
	p.server.mu.Lock()
	defer p.server.mu.Unlock()
	if p.server.recvPhase < 10 {
		t.Fatalf("only %d key updates over %d packets", p.server.recvPhase, chunks)
	}
}
```

- [ ] **Step 2: Run it**

```bash
cd $OBJTRSF && go test ./objproto/ -run TestBulkTransfer -v
```

Expected: PASS, with `recvPhase` well above 10.

- [ ] **Step 3: Commit**

```bash
cd $OBJTRSF && git add objproto/keyupdate_integration_test.go
git commit -m "test(objproto): bulk transfer across many key updates"
```

- [ ] **Step 4: Run the harness E2E**

In this repo, after Task 8 has landed:

```bash
scripts/dummy-harness.sh up --detach --agent fake --name KEYPHASE
```

Evaluate the printed environment, then exercise a real command over the wire (`harness-cli ls`, then an interactive session) and confirm both work. Consult the `dummy-harness` skill for the environment traps that make a dummy instance fail misleadingly.

```bash
scripts/dummy-harness.sh down
```

Expected: commands succeed. A handshake failure here means the wire change did not land consistently on both sides.

- [ ] **Step 5: Restart the real fleet in the right order**

This is a wire change, so the order is not optional:

```bash
make build
```

Then restart the **server first**, and only then the runners. An old server against a new runner fails at the handshake. Use `scripts/build_and_restart_all.py` only after the server is already on the new build.

- [ ] **Step 6: Confirm on the live deployment**

Run `harness-cli ls` and open a session against the live server. Both must work. If the WebUI is running, confirm a terminal attaches — it exercises the same objproto path through wasm.
