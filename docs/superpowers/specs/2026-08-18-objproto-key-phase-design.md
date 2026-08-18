# objproto: key phase (AEAD key update) and header field masking

Date: 2026-08-18
Status: design approved, not implemented
Repos: `github.com/on-keyday/objtrsf` (`objproto/`), then `github.com/on-keyday/agent-harness`

## Problem

Two separate problems, both rooted in the same byte.

**1. `PacketHeader.version` is a dead field.** It is written as `0` at
`objproto/objproto.go:589, 898, 1115, 1177` and never read anywhere. No code
outside `objproto` references `PacketHeader` at all — the harness never touches
it. One byte of every packet carries nothing.

**2. There is no AEAD key update.** A connection derives its traffic keys once
at handshake and uses them for the whole connection lifetime. `sentCounter` is
a monotonic `atomic.Uint64` and the AEAD key never changes. QUIC's
confidentiality limit for AES-GCM is 2^23 packets; at trsf's ~1200-byte MTU
(`trsf/mock/mock.go:48`) and the measured ~20 MB/s file-transfer throughput,
that limit is reached in roughly eight minutes of a single large transfer. It
is a line this deployment actually crosses, not a theoretical bound.

Additionally, every packet carries two fields that are constant in practice and
sit at fixed offsets: `kind` (four enum values) and `connection_id` (derived
from the dial address, therefore identical on every connection to a given
endpoint). An independent black-box observation of the live `/ws` transport
(agentboard task `b9821205`, transparent WS MITM, read-only) recovered the full
6-byte header structure and used the constant leading `0x00` — the dead
`version` byte — as its frame-boundary marker, and classified handshake vs.
application traffic from the `kind` byte alone.

## Goals

- Repurpose the dead byte into a working AEAD key-phase bit.
- Add a KDF-ratchet key update so traffic keys change before AEAD usage limits
  are approached, and old keys can be destroyed.
- Remove fixed-value bytes at fixed offsets from the header.

## Non-goals

**This design makes no traffic-analysis-resistance claim.** The masking below is
unkeyed and self-contained: the mask seed travels in cleartext in the same
header, so any observer who knows the scheme reverses it immediately. What it
buys is that no byte of the header holds a constant value at a constant offset.
It buys nothing against an observer who has read this document.

Specifically NOT addressed, and not to be described as addressed:

- `len` stays cleartext, and packet sizes are visible regardless.
- The handshake body (`Handshake`: `key_kind`, `common_key_kind`, `len`, raw
  ECDH public key) stays cleartext. The handshake is a fixed 145 bytes on the
  wire and is identifiable by size and position alone.
- Source/destination addresses, datagram sizes, packet ordering and timing are
  unchanged.
- `key_phase` is observable. An observer can see that a connection rekeyed.

Genuine indistinguishability would require a pre-shared endpoint secret, packet
padding, and timing shaping — an obfuscated-transport layer wrapping `Packet`.
That is a separate project and is explicitly out of scope here.

Post-compromise recovery is also out of scope. A KDF ratchet gives forward
secrecy for past traffic once old secrets are destroyed; it does not heal after
a key compromise. Fresh-ECDH rekey is deliberately not implemented. The
`control` frame defined below is the carrier it would use if it is ever added.

## Wire format

`objproto/packet/packet.bgn`. brgen cannot currently express a field derived by
transformation from another field, so the schema describes the **wire** form
directly and the logical values are recovered by Go accessors. This keeps the
schema honest about every wire byte.

```
format PacketHeader:
    key_phase :u1
    mask_seed :u7
    masked_kind :u8
    masked_connection_id :u16
    len :u16
```

Header size is unchanged at 6 bytes. `enum PacketKind` is retained as the
documented value space for the unmasked `kind`.

### Mask

Let `b0` be the whole first wire byte (`key_phase << 7 | mask_seed`).

```
MASK[b][i] = ((b * C[i]) ^ D[i]) & 0xff       i = 0..2
C = {0x1d, 0x8b, 0x37}      # odd, so each column is a permutation of 0..255
D = {0x5a, 0xa5, 0x3c}
```

```
masked_kind          = kind        ^ MASK[b0][0]
masked_connection_id = connection_id ^ (MASK[b0][1]<<8 | MASK[b0][2])
```

The table is public and fixed; any agreed cheap function would do. Indexing by
the **whole** first byte (phase bit included) means the phase bit cannot be
isolated by comparing two packets. A 256x3 table is precomputed at init.

`mask_seed` is filled from a non-cryptographic PRNG (`math/rand/v2`) — it is not
a secret, and at ~17k packets/s a `crypto/rand` draw per packet is needless
cost.

### Per-kind rules

- **application** — `key_phase` is the sender's current send phase, low bit.
- **handshake / handshake_ack / probe** — no keys exist, so there is no phase.
  All 8 bits of `b0` are random. Receivers ignore bit 7 for these kinds.
  Filling only 7 bits would leave bit 7 constant at a fixed offset, which is
  the thing being removed.

Order of operations on receive: unmask `kind` first (the whole `b0` is the mask
index, so this needs nothing else), and only if `kind == application` interpret
bit 7 as the key phase. There is no circularity.

### `len` is deliberately not masked

Two reasons, both inside this design's own criterion:

1. `len` already varies per packet — it is not a fixed byte.
2. The generated `Packet.DecodeExact` slices `data :[header.len]u8` using it, so
   masking it would require unmasking **before** decoding. That means either
   mutating the raw buffer — which desynchronises the handshake transcript and
   breaks the PSK binder (see Constraints) — or restructuring the decode path.
   Cost without benefit.

### AAD

**AAD is the 6 wire header bytes verbatim.** This mask does not depend on the
ciphertext (unlike the existing header protection over the 8-byte nonce prefix),
so masking before sealing is not circular.

- Send: set the masked field values, serialise the header once
  (`Header.MustAppend(nil)`, 6 bytes), use those bytes as AAD, then reuse the
  same header in `Packet.Append`.
- Receive: the decoded struct holds the masked values as they arrived, so
  `hdr.MustAppend(nil)` reproduces the wire bytes exactly.

Because `b0` is inside the AAD, the mapping from masked to logical values is
authenticated along with everything else.

The single-format choice is deliberate. A second "logical" format would
reintroduce the question of which form the AAD covers, and both ends making the
same mistake would still interoperate — hiding the bug. There is nothing to get
wrong when the AAD is the wire bytes.

## Key schedule

`objproto/objproto.go:747` `keySchedule`, `objproto/crypto.go`.

```
preMaster        = HKDF(shared, "ksdk-protocol-connection"+integrityInfo, 32)   # unchanged
hp_<dir>         = HKDF(preMaster, "ksdk-protocol-header-protect-<dir>", 32)    # unchanged, phase-independent
secret_<dir>_0   = HKDF(preMaster, "ksdk-protocol-master-<dir>", 32)            # label kept; role changes from key to secret

key_<dir>_n      = HKDF(secret_<dir>_n, "ksdk-protocol-key", keyLen(commonKeyKind))
iv_<dir>_n       = HKDF(secret_<dir>_n, "ksdk-protocol-nonce", 12)
secret_<dir>_n+1 = HKDF(secret_<dir>_n, "ksdk-protocol-ku", 32)
```

`<dir>` is `hs` / `ack` as today. The header-protection key is **not** rotated. It
protects only the 8-byte nonce prefix, exactly as it does today, and keeping it
outside the ratchet means the prefix stays readable across a phase change.

The existing `ksdk-protocol-nonce-hs` / `ksdk-protocol-nonce-ack` labels are
removed; the IV is now derived per phase from the phase secret.

Old secrets are `clear()`ed after ratcheting, matching the existing habit for
ECDH privates (`objproto.go:889, 957`).

This is a flag day: phase-0 keys differ from the current implementation. The
header format changes in the same release, so old and new cannot interoperate
regardless.

### Defect fixed in scope

`NewAEADFromCommonKeyKind` (`objproto/crypto.go:100-125`) passes the full
32-byte key slice to `aes.NewCipher` for every AES kind, so a negotiated
`aes128_gcm` or `aes192_gcm` actually instantiates AES-256. Both ends do the
same thing so it interoperates, but the negotiated field does not describe what
runs. Introducing `keyLen(commonKeyKind)` (16/24/32) fixes it, and these are the
same lines being rewritten.

## Key update state machine

Per direction, **independent**. No coordination, no acknowledgement: each
endpoint advances its own send phase and the peer follows.

New `activeConnection` state (`objproto/objproto.go:252`):

```
// send
sendSecret []byte; sendPhase uint64; sendPhaseFrom PacketNumber; sendPhaseAt time.Time
// receive
recvSecret []byte; recvPhase uint64; recvPhaseFrom PacketNumber
prevPeerAEAD cipher.AEAD; prevPeerIV []byte; prevExpiry time.Time
nextPeerAEAD cipher.AEAD; nextPeerIV []byte; nextSecret []byte
```

At connection establishment both phases are 0, `sendPhaseFrom` and
`recvPhaseFrom` are 0, `sendPhaseAt` is the connection time, and there is no
previous key.

**Send** (`sendApplication`, `objproto.go:1100`): before sealing, if
`count - sendPhaseFrom >= keyUpdatePackets`, ratchet the send side. Write
`key_phase = sendPhase & 1`.

**Receive** (`receiveApplication`, `objproto.go:971`): `bit := data[0] >> 7`
(cleartext, no key needed).

1. `bit == recvPhase & 1` — current key.
2. else if `pn < recvPhaseFrom` and the previous key is retained and unexpired —
   previous key.
3. else — trial with the precomputed `nextPeerAEAD`. **On success only**:
   `prev <- current`, `current <- next`, `recvPhase++`,
   `recvPhaseFrom = pn`, `prevExpiry = now + prevKeyRetention`, recompute
   `next`.
4. Any decryption failure — drop the packet, mutate no state.

The phase bit is inside the AAD, so flipping it fails decryption. An off-path
attacker can force at most one extra AEAD open per packet. The receiver holds
only prev/current/next and can follow at most one advance at a time, so the
sender must not advance twice inside the peer's reordering window — hence the
send-side floor below.

| Constant | Initial value | Basis |
|---|---|---|
| `keyUpdatePackets` | 2^22 | half of QUIC's 2^23 AES-GCM confidentiality limit |
| `keyUpdateInterval` | 10 min | time trigger, for idle long-lived connections |
| `prevKeyRetention` | 3 s | **a guess.** objproto has no PTO estimate to derive it from |
| `minPacketsBetweenUpdates` | 1024 | prevents double advance |
| `minTimeBetweenUpdates` | 1 s | prevents double advance |

The two triggers differ in what they emit:

- **packet count** — evaluated inline in `sendApplication`. The data packet
  being sent carries the new phase, so nothing extra is emitted.
- **time** — evaluated by `AutoKeyUpdate` against `sendPhaseAt`, which calls
  `UpdateKey()`. `UpdateKey()` ratchets the send side and immediately emits a
  `ping` control frame under the new phase, because an idle connection has no
  data packet to carry it.

`UpdateKey()` returns an error if called inside the floor. Reordering beyond
`prevKeyRetention` causes drops that trsf retransmits — degradation, not
corruption.

## Control frame

`ProtectedHeader` in `packet.bgn` is currently declared and never referenced
from Go; the code reads the same 8 bytes as a bare `uint64`. Make it real, with
the polarity that keeps the common case zero:

```
format ProtectedHeader:
    control :u1          # 0 = application data, 1 = objproto-internal control frame
    nonce_counter :u63
```

Renamed from `raw_payload`. With the original name and polarity, every data
packet would set bit 63, and `nonce_counter` is surfaced upward as
`PacketNumber` into trsf's ACK numbers (`objproto.go:1131` ->
`trsf/ack_handler.go:131`). The rename is free — the field has no users.

These 8 bytes are already under header protection (`objproto.go:990, 1142`), so
`control` is not observable, and they are the nonce input
(`objproto.go:996`), so the bit is implicitly authenticated.

**Required:** pass `recvTracker.InsertNonce` the value with bit 63 cleared. The
raw `uint64` would jump to 2^63 and destroy replay tracking.

```
enum ControlKind:
    :u8
    ping = 0x4b   # empty body
```

One kind, one caller. `control = 1` packets are handled inside objproto and
never reach `msgs.SendMessage`.

**Why it is needed now:** the time trigger requires it. On an idle connection
the initiating side must emit something for the peer to learn the new phase, and
that packet must not surface to the application. With only the packet-count
trigger the phase flips on the next data packet and no control frame would be
needed — if the time trigger is dropped, this whole section can be dropped with
it.

**To verify during implementation:** control packets consume a PN, so trsf sees
gaps in numbering it did not issue. `ack_handler` appears to work from its own
record of sent packets, but confirm it does not assume contiguity.

## API

- `Connection.UpdateKey() error` added to the interface
  (`objproto/session.go:97-119`). Breaking for implementors: after the go.mod
  bump, `remote-agent-harness/server/conn_list_test.go:67` `fakeRawConn` needs
  the method.
- `AutoKeyUpdate(s Endpoint, interval, maxKeyAge time.Duration)` in
  `objproto/session.go`, beside `AutoGarbageCollect` and `AutoRespondProbes`.
  Additive — `AutoGarbageCollect`'s signature is unchanged. The harness must
  call it wherever it calls `AutoGarbageCollect`.

## Constraints and traps

1. **The handshake transcript is the PSK binder input.** `addActiveConnection`
   receives raw wire bytes of handshake + ack (`objproto.go:913, 963`), exposed
   as `GetTranscript()` and consumed by the harness at `server/psk.go:149`,
   `runner/connect.go:283`, `cli/client.go:100`, `cli/agent/conn.go:198`. Never
   unmask in place in the received buffer, and never mutate the decoded header
   struct — both ends must retain byte-identical transcripts. Recover logical
   values through accessors into locals.
2. **Never reset the packet number or the replay tracker on key update.**
   `PacketNumber` is objproto's send counter (`objproto.go:358`) surfaced into
   trsf's loss detection (`trsf/ack_handler.go:131, 207, 317`); trsf issues a
   fresh PN for every transmission including retransmits (`trsf/conn.go:523,
   568, 619, 668`), so there is no nonce reuse today and monotonicity must
   survive. Note `addActiveConnection` does reset both
   (`objproto.go:831-832`) — that is the proxy-rehandshake reuse path and key
   update must not follow it.
3. **The proxy relay has no keys.** `receive()` relays raw bytes between peers
   (`objproto.go:1053-1067`). Masking is unkeyed so the relay is unaffected, but
   it reads `pkt.Header.Kind` at `1064-1065`; the unmasked accessor must be used
   there. The value flows to `PacketData.Kind` and then `closeCannotSend`, which
   short-circuits through `mayCloseProxy` for proxied packets
   (`objproto.go:494-505`), so only the log line depends on it.
4. **Verify brgen's bitfield packing** for a `u1` + `u7` leading byte matches
   `ProtectedHeader`'s `u1` + `u63` MSB-first layout. This can only be checked
   once the schema changes; do it first, before writing any Go against the new
   field names.

## Regenerating packet.go

`objtrsf` has no codegen tooling of its own. It borrows this repo's
`scripts/protoregen.sh` (`make protoregen`), which drives the brgen local api
server at `lang=go3` (= ebm2go). The per-target loop uses the path as given and
writes `${bgn%.bgn}.go` beside it, so an absolute out-of-tree path works:

```
./scripts/protoregen.sh <objtrsf>/objproto/packet/packet.bgn
```

`--all` is scoped to this repo (`find .`) and will not reach objtrsf; always
pass the path explicitly. The `~/.cache/brgen-kit` cache must already exist or
the first run does a ~20 MB download plus an npm install.

### Toolchain drift: regenerate as a separate commit first

The committed `objproto/packet/packet.go` is stamped
`ebm2go at https://github.com/on-keyday/rebrgen`, while the current toolchain
emits `.../brgen`. `objproto/packet` and `exec/frame` carry the older stamp;
`trsf/wire` and this repo's `runner/protocol` carry the current one.

Measured on 2026-08-18 by regenerating the **unchanged** schema into a scratch
directory and diffing: same 1651 lines, and after normalising generated
identifier numbering the entire difference is cosmetic — `tmpNNN` /
`io_temp_NNN` renumbering, `Variant161` -> `Variant164` (no references outside
the generated file), and redundant `int(...)` wrapping. Swapping the
regenerated file in gave `go build ./...` clean and
`go test ./objproto/... ./trsf/...` all passing.

So the drift is safe, but it is ~978 raw diff lines of noise. **Land it as a
preparatory commit that regenerates packet.go with no schema change**, verified
green, before the wire-format change. Otherwise the wire diff is unreviewable
and a bisect cannot separate the two.

## Tests

1. Ratchet determinism — both ends derive identical phase 0..3 material, all
   phases pairwise distinct.
2. `aes128_gcm` yields a 16-byte key (regression test for the `crypto.go`
   defect).
3. Phase follow — send past `keyUpdatePackets`, peer decrypts continuously and
   `recvPhase` advances.
4. Reordering — a phase-n packet delivered after the phase-n+1 commit decrypts
   inside `prevKeyRetention` and is dropped after it.
5. Hostile — flip the phase bit on a valid packet: dropped, `recvPhase`
   unchanged, connection survives.
6. PN monotonicity across an update.
7. Mask round-trip — for all 256 `b0` values, mask then unmask is the identity
   for `kind` and `connection_id`.
8. Transcript stability — with masking on, handshake+ack transcripts are
   byte-identical on both ends. Guards the PSK binder.
9. Integration — `keyUpdatePackets` lowered to 64, multi-MB transfer over trsf,
   payload integrity verified.
10. E2E — `dummy-harness` after the go.mod bump.

## Rollout

Wire change, so the order is fixed:

1. objtrsf: land on trunk (Mode A local-trunk FF push), publish.
2. harness: `go.mod` bump, fix `fakeRawConn`, wire `AutoKeyUpdate`.
3. `make build`.
4. **Restart the server first.** An old server against a new runner fails at
   the handshake.

## Open items

- `prevKeyRetention` is unvalidated. Revisit if test 4 or the integration test
  shows drops under normal reordering.
- Whether `AutoKeyUpdate` should be folded into `AutoGarbageCollect` at the next
  deliberate signature change rather than living as a second loop.
