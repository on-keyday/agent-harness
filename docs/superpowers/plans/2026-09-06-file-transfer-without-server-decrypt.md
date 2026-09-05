# File transfer the server does not decrypt — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Carry `file push` / `pull` / `ls` bytes on one connection whose AEAD is end-to-end between client and runner, with the server forwarding packets instead of decrypting and re-encrypting them.

**Architecture:** The server keeps its existing caps/scope decision and its existing routing role, then instead of splicing two trsf streams it (a) mints a short-lived grant naming the request, (b) pushes it to the runner over the registered control connection, (c) installs an `objproto.SetProxy` entry so the client's packets are forwarded raw, and (d) returns the grant to the client. The runner accepts that connection, matches the grant, and runs its existing file I/O. The runner also gains a punch handler that v1 never triggers, so a later direct client→runner path needs no runner change.

**Tech Stack:** Go 1.25, brgen `.bgn` schema (`make protoregen`), objtrsf `objproto`/`trsf`, existing `peer.Conn` wrapper.

**Spec:** `docs/superpowers/specs/2026-09-06-file-transfer-without-server-decrypt-design.md`
Companion measurement: `docs/superpowers/specs/2026-09-06-direct-client-runner-dial-probe.md`

## Global Constraints

- **Work in the parent repo** `/home/kforfk/workspace/remote-agent-harness/`, not in a `.harness-worktrees/<hash>/` directory. Absolute paths under the parent route to the parent checkout (Pitfall 8).
- **Schema lives in one task.** Every `.bgn` addition this feature needs is Task 1. No later task edits `message.bgn` (`feedback_no_split_schemas`).
- **Regenerate with `make protoregen`**, never by hand-editing `runner/protocol/message.go`.
- **Verify with make targets**: `make check`, `make vet`, `make test` — not ad-hoc `go build ./...` (`feedback_verify_with_make_targets_not_adhoc`).
- **Build hygiene**: compile-check with `go build ./...` or `go vet ./cmd/<x>`; never bare `go build ./cmd/<x>/`, which drops a binary in the worktree root.
- **`peer.Conn.Close()` ≠ `pc.Connection().Close()`** — the former sends a trsf Close that propagates through a SetProxy entry to peers you did not mean to notify (Pitfall 5).
- **Grant TTL is 5 minutes**, refreshed while the connection carrying it is open.
- **The runner never sees a `Capability` value or a scope expression** (spec D7).
- **v1's server always writes `punch_target` with `transport_len == 0`** (spec D10). The runner handles a set value; nothing sets it.

---

### Task 1: The whole wire change

**Files:**
- Modify: `runner/protocol/message.bgn`
- Regenerate: `runner/protocol/message.go` (via `make protoregen`)
- Test: `runner/protocol/dataplane_wire_test.go` (create)

**Interfaces:**
- Consumes: nothing.
- Produces: `protocol.DataPlaneGrant`, `protocol.AuthorizeDataPlaneRequest`, `protocol.AuthorizeDataPlaneResponse`, `protocol.AuthorizeDataPlaneStatus_*`, `protocol.RevokeDataPlaneRequest`, `protocol.RevokeDataPlaneResponse`, `protocol.DataPlaneInfo`, `protocol.ClientKind_DataPlane`, `protocol.ClientHelloStatus_Expired`, `protocol.ClientHelloStatus_NotPermitted`, `protocol.RunnerRequestType_AuthorizeDataPlane`, `protocol.RunnerRequestType_RevokeDataPlane`, and the new fields on `OpenFileTransferResponse` / `ListFilesResponse`.

- [ ] **Step 1: Write the failing round-trip test**

Create `runner/protocol/dataplane_wire_test.go`:

```go
package protocol

import (
	"bytes"
	"testing"
)

func TestDataPlaneGrantRoundTripFileTransfer(t *testing.T) {
	g := DataPlaneGrant{
		GrantId:       [16]uint8{1, 2, 3},
		TaskId:        TaskID{Id: [16]uint8{9, 9}},
		ExpiresUnixMs: 1_700_000_000_000,
		Kind:          TaskControlKind_OpenFileTransfer,
		Direction:     FileTransferDirection_Pull,
	}
	b, err := g.Append(nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var back DataPlaneGrant
	if _, err := back.Decode(b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back.Kind != TaskControlKind_OpenFileTransfer || back.Direction != FileTransferDirection_Pull {
		t.Fatalf("kind/direction lost: %v %v", back.Kind, back.Direction)
	}
	if back.ExpiresUnixMs != g.ExpiresUnixMs || back.GrantId != g.GrantId {
		t.Fatalf("fixed fields lost")
	}
}

// A kind with no arm must encode shorter, and the fields BEFORE the variant
// must sit at the same offsets either way — that is why the variant is last.
func TestDataPlaneGrantVariantIsTail(t *testing.T) {
	base := DataPlaneGrant{ExpiresUnixMs: 7, Kind: TaskControlKind_GitQuery}
	withArm := DataPlaneGrant{ExpiresUnixMs: 7, Kind: TaskControlKind_OpenFileTransfer}
	a, err := base.Append(nil)
	if err != nil {
		t.Fatalf("encode git_query: %v", err)
	}
	b, err := withArm.Append(nil)
	if err != nil {
		t.Fatalf("encode open_file_transfer: %v", err)
	}
	if len(a) >= len(b) {
		t.Fatalf("expected the armless kind to be shorter: %d vs %d", len(a), len(b))
	}
	// Everything up to and including `kind` is identical except the kind byte.
	if !bytes.Equal(a[:len(a)-1], b[:len(a)-1]) {
		t.Fatalf("fields before the variant moved")
	}
}

// transport_len == 0 is how "do not punch" is spelled; it must round-trip.
func TestAuthorizeDataPlaneAbsentPunchTarget(t *testing.T) {
	req := AuthorizeDataPlaneRequest{SlotId: 0x4242}
	req.Grant.Kind = TaskControlKind_ListFiles
	b, err := req.Append(nil)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var back AuthorizeDataPlaneRequest
	if _, err := back.Decode(b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back.PunchTarget.TransportLen != 0 {
		t.Fatalf("absent punch target did not round-trip: %d", back.PunchTarget.TransportLen)
	}
	if back.SlotId != 0x4242 {
		t.Fatalf("slot lost: %d", back.SlotId)
	}
}
```

- [ ] **Step 2: Run it to confirm it fails**

Run: `go test ./runner/protocol -run 'DataPlane|AuthorizeDataPlane' -v`
Expected: FAIL — `undefined: DataPlaneGrant`.

- [ ] **Step 3: Edit the schema**

In `runner/protocol/message.bgn`:

Add `data_plane` to `ClientKind` (append; ordinals of existing values must not move).

Add to `ClientHelloStatus` (after `runner_mismatch`):

```
    expired
    not_permitted
```

Add `authorize_data_plane` and `revoke_data_plane` to `RunnerRequestType` (append) and the matching arms to the `RunnerRequest` union.

Add the formats — place them after `FileTransferDirection` so both `TaskControlKind` and `FileTransferDirection` are already declared:

```
# A grant is one request on one task, for a bounded time. It is NOT the task's
# auth_ticket: that names the task's agent, this names a request.
#
# The variant tail is LAST so grant_id, task_id and expires_unix_ms sit at
# fixed offsets whatever the kind, and a future arm moves none of them.
format DataPlaneGrant:
    grant_id :[16]u8
    task_id :TaskID
    expires_unix_ms :u64
    kind :TaskControlKind
    if kind == TaskControlKind.open_file_transfer:
        direction :FileTransferDirection

# server -> runner, on the existing registered conn.
#
# punch_target is where the runner should send probes so a client can reach it
# directly. transport_len == 0 means "do not punch, the server is forwarding" —
# the encoding DialRunnerRequest.via uses for "not specified", and the only
# value v1's server writes.
#
# slot_id precedes grant because DataPlaneGrant ends in a variant, so anything
# embedding it must place it last.
format AuthorizeDataPlaneRequest:
    slot_id :u16
    punch_target :RunnerID
    grant :DataPlaneGrant

enum AuthorizeDataPlaneStatus:
    :u8
    ok = "ok"
    unknown_task
    slot_collision
    duplicate_grant

format AuthorizeDataPlaneResponse:
    status :AuthorizeDataPlaneStatus

# server -> runner. Idempotent: revoking an unknown grant is ok.
format RevokeDataPlaneRequest:
    grant_id :[16]u8

format RevokeDataPlaneResponse:
    closed :u32

# client -> runner, inside the existing PskAuthRequest.
format DataPlaneInfo:
    grant_id :[16]u8
    task_id :TaskID
```

Add the `ClientHello` arm:

```
        AuthRole is unchanged; ClientHello gains:
    if kind == ClientKind.data_plane:
        data_plane_info :DataPlaneInfo
```

Append to `OpenFileTransferResponse` and `ListFilesResponse`:

```
    grant_id :[16]u8
    slot_id :u16
    runner_cid :RunnerID
```

- [ ] **Step 4: Regenerate and run the test**

Run: `make protoregen && go test ./runner/protocol -run 'DataPlane|AuthorizeDataPlane' -v`
Expected: PASS.
If brgen rejects an arm name, rename the field rather than the enum value — `.bgn` arm names collide with codec method names (`project_bgn_union_arm_name_collision`).

- [ ] **Step 5: Confirm nothing else broke**

Run: `make vet && go build ./...`
Expected: clean. Appending enum values keeps existing ordinals, so no call site changes yet.

- [ ] **Step 6: Commit**

```bash
git add runner/protocol/message.bgn runner/protocol/message.go runner/protocol/dataplane_wire_test.go
git commit -m "feat(proto): a data-plane grant, its authorize/revoke requests, and the client hello arm"
```

---

### Task 2: Runner-side grant store

**Files:**
- Create: `runner/dataplane_grants.go`
- Test: `runner/dataplane_grants_test.go`

**Interfaces:**
- Consumes: `protocol.DataPlaneGrant` (Task 1).
- Produces:
  - `type grantStore struct{}` with `newGrantStore() *grantStore`
  - `(*grantStore).Insert(g protocol.DataPlaneGrant, slotID uint16) protocol.AuthorizeDataPlaneStatus`
  - `(*grantStore).Validate(grantID [16]byte, taskID protocol.TaskID, kind protocol.TaskControlKind, dir protocol.FileTransferDirection, now time.Time) protocol.ClientHelloStatus`
  - `(*grantStore).Revoke(grantID [16]byte) uint32`
  - `(*grantStore).Sweep(now time.Time) int`
  - `(*grantStore).OnClose(grantID [16]byte, closer func())`

- [ ] **Step 1: Write the failing test**

Create `runner/dataplane_grants_test.go`:

```go
package runner

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func grant(id byte, exp time.Time) protocol.DataPlaneGrant {
	g := protocol.DataPlaneGrant{
		GrantId:       [16]uint8{id},
		TaskId:        protocol.TaskID{Id: [16]uint8{7}},
		ExpiresUnixMs: uint64(exp.UnixMilli()),
		Kind:          protocol.TaskControlKind_OpenFileTransfer,
		Direction:     protocol.FileTransferDirection_Pull,
	}
	return g
}

func TestGrantStoreValidateHappyPath(t *testing.T) {
	now := time.Now()
	s := newGrantStore()
	if st := s.Insert(grant(1, now.Add(time.Minute)), 0x10); st != protocol.AuthorizeDataPlaneStatus_Ok {
		t.Fatalf("insert: %v", st)
	}
	got := s.Validate([16]byte{1}, protocol.TaskID{Id: [16]uint8{7}},
		protocol.TaskControlKind_OpenFileTransfer, protocol.FileTransferDirection_Pull, now)
	if got != protocol.ClientHelloStatus_Ok {
		t.Fatalf("want ok, got %v", got)
	}
}

func TestGrantStoreRefusesWrongDirection(t *testing.T) {
	now := time.Now()
	s := newGrantStore()
	s.Insert(grant(1, now.Add(time.Minute)), 0x10)
	got := s.Validate([16]byte{1}, protocol.TaskID{Id: [16]uint8{7}},
		protocol.TaskControlKind_OpenFileTransfer, protocol.FileTransferDirection_Push, now)
	if got != protocol.ClientHelloStatus_NotPermitted {
		t.Fatalf("want not_permitted, got %v", got)
	}
}

func TestGrantStoreRefusesExpired(t *testing.T) {
	now := time.Now()
	s := newGrantStore()
	s.Insert(grant(1, now.Add(-time.Second)), 0x10)
	got := s.Validate([16]byte{1}, protocol.TaskID{Id: [16]uint8{7}},
		protocol.TaskControlKind_OpenFileTransfer, protocol.FileTransferDirection_Pull, now)
	if got != protocol.ClientHelloStatus_Expired {
		t.Fatalf("want expired, got %v", got)
	}
}

func TestGrantStoreRefusesUnknownAndWrongTask(t *testing.T) {
	now := time.Now()
	s := newGrantStore()
	s.Insert(grant(1, now.Add(time.Minute)), 0x10)
	if got := s.Validate([16]byte{2}, protocol.TaskID{Id: [16]uint8{7}},
		protocol.TaskControlKind_OpenFileTransfer, protocol.FileTransferDirection_Pull, now); got != protocol.ClientHelloStatus_BadTicket {
		t.Fatalf("unknown grant: want bad_ticket, got %v", got)
	}
	if got := s.Validate([16]byte{1}, protocol.TaskID{Id: [16]uint8{8}},
		protocol.TaskControlKind_OpenFileTransfer, protocol.FileTransferDirection_Pull, now); got != protocol.ClientHelloStatus_UnknownTask {
		t.Fatalf("wrong task: want unknown_task, got %v", got)
	}
}

func TestGrantStoreDuplicateInsertRefused(t *testing.T) {
	now := time.Now()
	s := newGrantStore()
	s.Insert(grant(1, now.Add(time.Minute)), 0x10)
	if st := s.Insert(grant(1, now.Add(time.Minute)), 0x11); st != protocol.AuthorizeDataPlaneStatus_DuplicateGrant {
		t.Fatalf("want duplicate_grant, got %v", st)
	}
}

func TestGrantStoreRevokeIsIdempotentAndCloses(t *testing.T) {
	now := time.Now()
	s := newGrantStore()
	s.Insert(grant(1, now.Add(time.Minute)), 0x10)
	closed := 0
	s.OnClose([16]byte{1}, func() { closed++ })
	if n := s.Revoke([16]byte{1}); n != 1 {
		t.Fatalf("want 1 closed, got %d", n)
	}
	if closed != 1 {
		t.Fatalf("closer not called")
	}
	if n := s.Revoke([16]byte{1}); n != 0 {
		t.Fatalf("second revoke should be a no-op, got %d", n)
	}
}

func TestGrantStoreSweepDropsExpired(t *testing.T) {
	now := time.Now()
	s := newGrantStore()
	s.Insert(grant(1, now.Add(-time.Second)), 0x10)
	s.Insert(grant(2, now.Add(time.Minute)), 0x11)
	if n := s.Sweep(now); n != 1 {
		t.Fatalf("want 1 swept, got %d", n)
	}
	if got := s.Validate([16]byte{2}, protocol.TaskID{Id: [16]uint8{7}},
		protocol.TaskControlKind_OpenFileTransfer, protocol.FileTransferDirection_Pull, now); got != protocol.ClientHelloStatus_Ok {
		t.Fatalf("live grant was swept")
	}
}
```

- [ ] **Step 2: Run it to confirm it fails**

Run: `go test ./runner -run TestGrantStore -v`
Expected: FAIL — `undefined: newGrantStore`.

- [ ] **Step 3: Implement the store**

Create `runner/dataplane_grants.go`:

```go
package runner

import (
	"crypto/subtle"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// grantStore holds the data-plane grants the server has pushed to this runner.
//
// It is the mirror of agentboard/registry.go, which does the same job with the
// verifier on the other side: there the server holds the store and the agent
// presents the ticket; here the runner holds it and a client presents a grant.
// Deliberately NOT a capability set — the entry names a request the server
// already authorized, and nothing here evaluates policy (spec D7).
type grantStore struct {
	mu      sync.Mutex
	entries map[[16]byte]*grantEntry
}

type grantEntry struct {
	grant  protocol.DataPlaneGrant
	slotID uint16
	closer func()
}

func newGrantStore() *grantStore {
	return &grantStore{entries: make(map[[16]byte]*grantEntry)}
}

// Insert records a grant. A repeat grant_id is refused rather than overwritten:
// overwriting would invalidate a credential a live connection may be holding,
// which is the mistake agentboard/registry.go's Ticket() comment warns about.
func (s *grantStore) Insert(g protocol.DataPlaneGrant, slotID uint16) protocol.AuthorizeDataPlaneStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.entries[g.GrantId]; exists {
		return protocol.AuthorizeDataPlaneStatus_DuplicateGrant
	}
	s.entries[g.GrantId] = &grantEntry{grant: g, slotID: slotID}
	return protocol.AuthorizeDataPlaneStatus_Ok
}

// Validate answers with the ClientHelloStatus the runner should send back.
// The comparison on grant_id is constant-time; the rest are equality checks
// against a request that has already arrived, so they leak nothing a caller
// did not supply.
func (s *grantStore) Validate(
	grantID [16]byte,
	taskID protocol.TaskID,
	kind protocol.TaskControlKind,
	dir protocol.FileTransferDirection,
	now time.Time,
) protocol.ClientHelloStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[grantID]
	if !ok {
		return protocol.ClientHelloStatus_BadTicket
	}
	if subtle.ConstantTimeCompare(e.grant.GrantId[:], grantID[:]) != 1 {
		return protocol.ClientHelloStatus_BadTicket
	}
	if e.grant.TaskId.Id != taskID.Id {
		return protocol.ClientHelloStatus_UnknownTask
	}
	if uint64(now.UnixMilli()) > e.grant.ExpiresUnixMs {
		return protocol.ClientHelloStatus_Expired
	}
	if e.grant.Kind != kind {
		return protocol.ClientHelloStatus_NotPermitted
	}
	if kind == protocol.TaskControlKind_OpenFileTransfer && e.grant.Direction != dir {
		return protocol.ClientHelloStatus_NotPermitted
	}
	return protocol.ClientHelloStatus_Ok
}

// OnClose registers the teardown for the connection redeeming this grant, so a
// revoke can reach work already in flight (spec D6).
func (s *grantStore) OnClose(grantID [16]byte, closer func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[grantID]; ok {
		e.closer = closer
	}
}

// Revoke removes a grant and tears down whatever is redeeming it. Returns how
// many connections it closed. Revoking an unknown grant is a no-op.
func (s *grantStore) Revoke(grantID [16]byte) uint32 {
	s.mu.Lock()
	e, ok := s.entries[grantID]
	if ok {
		delete(s.entries, grantID)
	}
	s.mu.Unlock()
	if !ok || e.closer == nil {
		return 0
	}
	e.closer()
	return 1
}

// Sweep drops expired grants. Returns how many were dropped. A grant whose
// connection is live is still dropped from the store — the TTL bounds
// redeemability, not an open transfer (spec, Server behaviour).
func (s *grantStore) Sweep(now time.Time) int {
	cutoff := uint64(now.UnixMilli())
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, e := range s.entries {
		if cutoff > e.grant.ExpiresUnixMs {
			delete(s.entries, id)
			n++
		}
	}
	return n
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./runner -run TestGrantStore -v`
Expected: PASS, all seven.

- [ ] **Step 5: Commit**

```bash
git add runner/dataplane_grants.go runner/dataplane_grants_test.go
git commit -m "feat(runner): a grant store, the mirror of the agentboard ticket registry"
```

---

### Task 3: Runner-side punch handler

**Files:**
- Create: `runner/dataplane_punch.go`
- Test: `runner/dataplane_punch_test.go`

**Interfaces:**
- Consumes: `protocol.RunnerID` (existing), `objproto.Endpoint` (existing).
- Produces: `func punchToward(ctx context.Context, ep probeSender, target protocol.RunnerID, interval time.Duration) int` and `type probeSender interface { SendProbe(objproto.ConnectionID, [6]byte, netip.AddrPort) error }`.

This is the only code in the plan that nothing reaches in v1 (spec D10). It exists so a later direct path is a server-and-client change with no runner in it, and it is the loop probe 3 ran against a live Windows runner host — 240 probes, none failing.

- [ ] **Step 1: Write the failing test**

Create `runner/dataplane_punch_test.go`:

```go
package runner

import (
	"context"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

type fakeProbeSender struct{ n atomic.Int32 }

func (f *fakeProbeSender) SendProbe(objproto.ConnectionID, [6]byte, netip.AddrPort) error {
	f.n.Add(1)
	return nil
}

func udpTarget() protocol.RunnerID {
	return protocol.ConnIDToRunnerID(objproto.NewConnectionID("udp",
		netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), 45999), 0x7777))
}

func TestPunchTowardSendsNothingWhenTargetAbsent(t *testing.T) {
	f := &fakeProbeSender{}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if n := punchToward(ctx, f, protocol.RunnerID{}, 5*time.Millisecond); n != 0 {
		t.Fatalf("absent target should send nothing, sent %d", n)
	}
	if got := f.n.Load(); got != 0 {
		t.Fatalf("SendProbe called %d times for an absent target", got)
	}
}

func TestPunchTowardSendsUntilContextEnds(t *testing.T) {
	f := &fakeProbeSender{}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	n := punchToward(ctx, f, udpTarget(), 10*time.Millisecond)
	if n < 3 {
		t.Fatalf("expected several probes in 120ms at a 10ms interval, got %d", n)
	}
	if int32(n) != f.n.Load() {
		t.Fatalf("return %d disagrees with calls %d", n, f.n.Load())
	}
}
```

- [ ] **Step 2: Run it to confirm it fails**

Run: `go test ./runner -run TestPunchToward -v`
Expected: FAIL — `undefined: punchToward`.

- [ ] **Step 3: Implement the handler**

Create `runner/dataplane_punch.go`:

```go
package runner

import (
	"context"
	"net/netip"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// probeSender is the slice of objproto.Endpoint punchToward needs, so the loop
// is testable without a socket.
type probeSender interface {
	SendProbe(cid objproto.ConnectionID, macAddr [6]byte, ipAddr netip.AddrPort) error
}

// punchToward opens the return path for a peer that will dial us.
//
// A host firewall drops an unsolicited inbound datagram even on a LAN with no
// NAT — measured, three live runners refused a dial that succeeded in 33ms once
// this loop had run (see the probe doc, F3/F4). Sending from THIS socket toward
// the exact address:port the peer will dial from is what opens the mapping, so
// the target must name that socket, not merely that host (F5).
//
// v1's server always sends transport_len == 0, so this returns immediately
// without sending. It ships now so a direct client->runner path is a change to
// the server and the client, with no runner to redeploy (spec D10).
//
// Returns how many probes were sent.
func punchToward(ctx context.Context, ep probeSender, target protocol.RunnerID, interval time.Duration) int {
	if target.TransportLen == 0 {
		return 0
	}
	cid := protocol.RunnerIDToConnID(target)
	addr := cid.Addr
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	sent := 0
	for {
		if err := ep.SendProbe(cid, [6]byte{}, addr); err == nil {
			sent++
		}
		select {
		case <-ctx.Done():
			return sent
		case <-ticker.C:
		}
	}
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./runner -run TestPunchToward -v`
Expected: PASS, both.

- [ ] **Step 5: Commit**

```bash
git add runner/dataplane_punch.go runner/dataplane_punch_test.go
git commit -m "feat(runner): the punch loop, unreached in v1 so the direct path needs no runner"
```

---

### Task 4: Runner handles AuthorizeDataPlane and RevokeDataPlane

**Files:**
- Modify: `runner/session.go` (add the store to `Session`, add the two handlers)
- Modify: `runner/connect.go:458` `dispatchRunnerRequest` (two new cases)
- Test: `runner/dataplane_handler_test.go` (create)

**Interfaces:**
- Consumes: `newGrantStore` / `Insert` / `Revoke` (Task 2), `punchToward` (Task 3), `protocol.AuthorizeDataPlaneRequest` (Task 1).
- Produces: `(*Session).handleAuthorizeDataPlane(ctx, *protocol.AuthorizeDataPlaneRequest)`, `(*Session).handleRevokeDataPlane(*protocol.RevokeDataPlaneRequest)`, and `Session.Grants *grantStore`.

- [ ] **Step 1: Write the failing test**

Create `runner/dataplane_handler_test.go`:

```go
package runner

import (
	"context"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestHandleAuthorizeDataPlaneInsertsAndAnswersOk(t *testing.T) {
	s := &Session{Grants: newGrantStore()}
	var sent []protocol.AuthorizeDataPlaneResponse
	s.sendAuthorizeDataPlaneResponse = func(r protocol.AuthorizeDataPlaneResponse) { sent = append(sent, r) }

	req := &protocol.AuthorizeDataPlaneRequest{SlotId: 0x20}
	req.Grant = protocol.DataPlaneGrant{
		GrantId:       [16]uint8{5},
		TaskId:        protocol.TaskID{Id: [16]uint8{7}},
		ExpiresUnixMs: uint64(time.Now().Add(time.Minute).UnixMilli()),
		Kind:          protocol.TaskControlKind_ListFiles,
	}
	s.handleAuthorizeDataPlane(context.Background(), req)

	if len(sent) != 1 || sent[0].Status != protocol.AuthorizeDataPlaneStatus_Ok {
		t.Fatalf("want one ok response, got %+v", sent)
	}
	if got := s.Grants.Validate([16]byte{5}, protocol.TaskID{Id: [16]uint8{7}},
		protocol.TaskControlKind_ListFiles, 0, time.Now()); got != protocol.ClientHelloStatus_Ok {
		t.Fatalf("grant not stored: %v", got)
	}
}

func TestHandleAuthorizeDataPlaneRejectsSlotCollision(t *testing.T) {
	s := &Session{Grants: newGrantStore()}
	s.ServerCID = serverCIDWithID(0x30)
	var sent []protocol.AuthorizeDataPlaneResponse
	s.sendAuthorizeDataPlaneResponse = func(r protocol.AuthorizeDataPlaneResponse) { sent = append(sent, r) }

	req := &protocol.AuthorizeDataPlaneRequest{SlotId: 0x30}
	req.Grant.ExpiresUnixMs = uint64(time.Now().Add(time.Minute).UnixMilli())
	s.handleAuthorizeDataPlane(context.Background(), req)

	if len(sent) != 1 || sent[0].Status != protocol.AuthorizeDataPlaneStatus_SlotCollision {
		t.Fatalf("want slot_collision, got %+v", sent)
	}
}

func TestHandleRevokeDataPlaneClosesAndReportsCount(t *testing.T) {
	s := &Session{Grants: newGrantStore()}
	var sent []protocol.RevokeDataPlaneResponse
	s.sendRevokeDataPlaneResponse = func(r protocol.RevokeDataPlaneResponse) { sent = append(sent, r) }

	g := protocol.DataPlaneGrant{GrantId: [16]uint8{9},
		ExpiresUnixMs: uint64(time.Now().Add(time.Minute).UnixMilli())}
	s.Grants.Insert(g, 0x40)
	closed := false
	s.Grants.OnClose([16]byte{9}, func() { closed = true })

	s.handleRevokeDataPlane(&protocol.RevokeDataPlaneRequest{GrantId: [16]uint8{9}})
	if !closed {
		t.Fatalf("revoke did not close the live connection")
	}
	if len(sent) != 1 || sent[0].Closed != 1 {
		t.Fatalf("want closed=1, got %+v", sent)
	}
}
```

Add the helper the collision test needs to the same file:

```go
func serverCIDWithID(id uint16) objproto.ConnectionID {
	return objproto.NewConnectionID("udp",
		netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), 8539), id)
}
```

with imports `net/netip` and `github.com/on-keyday/objtrsf/objproto`.

- [ ] **Step 2: Run it to confirm it fails**

Run: `go test ./runner -run 'HandleAuthorizeDataPlane|HandleRevokeDataPlane' -v`
Expected: FAIL — `Session` has no field `Grants`.

- [ ] **Step 3: Implement**

In `runner/session.go`, add to the `Session` struct:

```go
	// Grants holds the data-plane grants the server has pushed. Nil in older
	// tests; handlers construct one lazily via ensureGrants.
	Grants *grantStore

	// Response senders, injected so the handlers are testable without a conn.
	// Wired in driveAfterConn to write on the server conn.
	sendAuthorizeDataPlaneResponse func(protocol.AuthorizeDataPlaneResponse)
	sendRevokeDataPlaneResponse    func(protocol.RevokeDataPlaneResponse)
```

Add to `runner/session.go`:

```go
func (s *Session) ensureGrants() *grantStore {
	if s.Grants == nil {
		s.Grants = newGrantStore()
	}
	return s.Grants
}

// handleAuthorizeDataPlane records a grant the server has authorized and, if a
// punch target is present, opens the return path toward it. v1's server always
// sends transport_len == 0, so the punch is a no-op there (spec D10).
func (s *Session) handleAuthorizeDataPlane(ctx context.Context, req *protocol.AuthorizeDataPlaneRequest) {
	respond := func(st protocol.AuthorizeDataPlaneStatus) {
		if s.sendAuthorizeDataPlaneResponse != nil {
			s.sendAuthorizeDataPlaneResponse(protocol.AuthorizeDataPlaneResponse{Status: st})
		}
	}
	// Same rule as EstablishRelay: a slot equal to the server conn's id would
	// resolve to that conn instead of producing a new one.
	if req.SlotId == s.ServerCID.ID {
		respond(protocol.AuthorizeDataPlaneStatus_SlotCollision)
		return
	}
	if st := s.ensureGrants().Insert(req.Grant, req.SlotId); st != protocol.AuthorizeDataPlaneStatus_Ok {
		respond(st)
		return
	}
	respond(protocol.AuthorizeDataPlaneStatus_Ok)

	if req.PunchTarget.TransportLen != 0 && s.Endpoint != nil {
		grantCtx, cancel := context.WithDeadline(ctx,
			time.UnixMilli(int64(req.Grant.ExpiresUnixMs)))
		go func() {
			defer cancel()
			punchToward(grantCtx, s.Endpoint, req.PunchTarget, 500*time.Millisecond)
		}()
	}
}

// handleRevokeDataPlane drops a grant and tears down whatever is redeeming it.
// Idempotent, per the spec's D6: a revoke is a message and messages are lost,
// so this must survive being delivered twice or never.
func (s *Session) handleRevokeDataPlane(req *protocol.RevokeDataPlaneRequest) {
	closed := s.ensureGrants().Revoke(req.GrantId)
	if s.sendRevokeDataPlaneResponse != nil {
		s.sendRevokeDataPlaneResponse(protocol.RevokeDataPlaneResponse{Closed: closed})
	}
}
```

In `runner/connect.go`, inside `dispatchRunnerRequest`'s switch (alongside the existing `case protocol.RunnerRequestType_OpenFileTransfer:` at line 523):

```go
	case protocol.RunnerRequestType_AuthorizeDataPlane:
		ad := req.AuthorizeDataPlane()
		if ad == nil {
			return
		}
		session.handleAuthorizeDataPlane(ctx, ad)
	case protocol.RunnerRequestType_RevokeDataPlane:
		rd := req.RevokeDataPlane()
		if rd == nil {
			return
		}
		session.handleRevokeDataPlane(rd)
```

Both are handled synchronously rather than in a goroutine: each is a map
operation plus one reply, and the server blocks on the reply.

In `driveAfterConn` (`runner/connect.go:202`), after the session is built, wire the two senders to write a `RunnerMessage` on the server conn, following how the existing relay response is sent.

- [ ] **Step 4: Run the tests**

Run: `go test ./runner -run 'HandleAuthorizeDataPlane|HandleRevokeDataPlane|TestGrantStore|TestPunchToward' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add runner/session.go runner/connect.go runner/dataplane_handler_test.go
git commit -m "feat(runner): accept and revoke data-plane grants over the control conn"
```

---

### Task 5: Runner accepts a data-plane connection

**Files:**
- Modify: `runner/listen.go` (third arm in `handleAcceptedConn`, new `handleDataPlaneConn`)
- Modify: `runner/connect.go` (`buildRunnerEndpoint` caller gains an accept loop in dial mode)
- Test: `runner/dataplane_accept_test.go` (create)

**Interfaces:**
- Consumes: `grantStore.Validate` (Task 2), `protocol.DataPlaneInfo` / `ClientKind_DataPlane` (Task 1).
- Produces: `func handleDataPlaneConn(ctx context.Context, cfg Config, sessionRef *atomic.Pointer[Session], pc *peer.Conn, first firstMsgT)`.

- [ ] **Step 1: Write the failing test**

Create `runner/dataplane_accept_test.go` with a test that a `PskAuthRequest`
carrying `ClientKind_DataPlane` and an unknown grant is answered
`ClientHelloStatus_BadTicket`, and one carrying a stored grant is answered
`Ok` — driving `validateDataPlaneHello`, the pure function the conn handler
calls:

```go
package runner

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestValidateDataPlaneHelloUnknownGrant(t *testing.T) {
	s := &Session{Grants: newGrantStore()}
	info := protocol.DataPlaneInfo{GrantId: [16]uint8{3}, TaskId: protocol.TaskID{Id: [16]uint8{7}}}
	got := validateDataPlaneHello(s, info, protocol.TaskControlKind_ListFiles, 0, time.Now())
	if got != protocol.ClientHelloStatus_BadTicket {
		t.Fatalf("want bad_ticket, got %v", got)
	}
}

func TestValidateDataPlaneHelloAcceptsStoredGrant(t *testing.T) {
	s := &Session{Grants: newGrantStore()}
	s.Grants.Insert(protocol.DataPlaneGrant{
		GrantId:       [16]uint8{3},
		TaskId:        protocol.TaskID{Id: [16]uint8{7}},
		ExpiresUnixMs: uint64(time.Now().Add(time.Minute).UnixMilli()),
		Kind:          protocol.TaskControlKind_ListFiles,
	}, 0x50)
	info := protocol.DataPlaneInfo{GrantId: [16]uint8{3}, TaskId: protocol.TaskID{Id: [16]uint8{7}}}
	got := validateDataPlaneHello(s, info, protocol.TaskControlKind_ListFiles, 0, time.Now())
	if got != protocol.ClientHelloStatus_Ok {
		t.Fatalf("want ok, got %v", got)
	}
}

func TestValidateDataPlaneHelloWithNoSession(t *testing.T) {
	got := validateDataPlaneHello(nil, protocol.DataPlaneInfo{}, protocol.TaskControlKind_ListFiles, 0, time.Now())
	if got != protocol.ClientHelloStatus_UnknownTask {
		t.Fatalf("no server conn should read as unknown_task, got %v", got)
	}
}
```

- [ ] **Step 2: Run it to confirm it fails**

Run: `go test ./runner -run TestValidateDataPlaneHello -v`
Expected: FAIL — `undefined: validateDataPlaneHello`.

- [ ] **Step 3: Implement**

In `runner/listen.go`, add:

```go
// validateDataPlaneHello is the whole authorization decision for a data-plane
// connection, as a pure function so it is testable without a socket. The
// runner holds no capability and no scope: it matches the grant the server
// pushed against the request that arrived (spec D7).
func validateDataPlaneHello(
	sess *Session,
	info protocol.DataPlaneInfo,
	kind protocol.TaskControlKind,
	dir protocol.FileTransferDirection,
	now time.Time,
) protocol.ClientHelloStatus {
	if sess == nil || sess.Grants == nil {
		// No server conn means no grant can have been pushed. Answering
		// unknown_task rather than bad_ticket keeps it indistinguishable from
		// a task this runner does not have.
		return protocol.ClientHelloStatus_UnknownTask
	}
	return sess.Grants.Validate(info.GrantId, info.TaskId, kind, dir, now)
}
```

Add the third arm to `handleAcceptedConn`'s switch:

```go
		case appwire.AppKind_PskAuth:
			handleDataPlaneConn(ctx, cfg, sessionRef, ep, pc, msg)
```

and `handleDataPlaneConn`, which decodes the `PskAuthRequest`, verifies the
binder with the existing PSK gate, decodes `ClientHello.data_plane_info`, calls
`validateDataPlaneHello`, answers a `ClientHelloResponse`, and on `Ok`
registers `s.Grants.OnClose(info.GrantId, func() { pc.Connection().Close() })`
— `pc.Connection().Close()`, never `pc.Close()`, because a trsf Close would
propagate through the server's SetProxy entry (Pitfall 5).

In `runner/connect.go`, after the server conn is established in dial mode,
start the same accept loop `ListenAndServe` runs:

```go
	// Dial-mode runners have a bound socket that accepts inbound handshakes
	// (EndpointModeMutual, buildRunnerEndpoint above), but until now nothing
	// read the accept channel, so a connection the server forwards to us would
	// be established at the objproto layer and then serviced by no one.
	go func() {
		connCh := ep.GetNewActiveConnectionChannel()
		for {
			select {
			case <-ctx.Done():
				return
			case conn, ok := <-connCh:
				if !ok {
					return
				}
				pc := peer.WrapAcceptedConn(ctx, conn, peer.DialConfig{
					Logger: cfg.Logger, PingInterval: cfg.PingInterval,
				})
				go handleAcceptedConn(ctx, cfg, sessionRef, ep, pc)
			}
		}
	}()
```

- [ ] **Step 4: Run the tests**

Run: `go test ./runner -run 'ValidateDataPlaneHello' -v && make vet`
Expected: PASS, vet clean.

- [ ] **Step 5: Commit**

```bash
git add runner/listen.go runner/connect.go runner/dataplane_accept_test.go
git commit -m "feat(runner): accept and authorize a data-plane conn, in dial mode too"
```

---

### Task 6: Server mints, authorizes and proxies

**Files:**
- Create: `server/dataplane.go`
- Modify: `server/file_transfer.go:20-80` (`handleOpenFileTransfer`) and `:84-133` (`handleListFiles`)
- Modify: `server/server.go` (a response-correlation map beside `relayRespCh`)
- Test: `server/dataplane_test.go` (create)

**Interfaces:**
- Consumes: `protocol.AuthorizeDataPlaneRequest` (Task 1), the runner side (Tasks 4–5).
- Produces: `(*Server).sendAuthorizeDataPlaneRequest(ctx, *RunnerEntry, protocol.AuthorizeDataPlaneRequest) (protocol.AuthorizeDataPlaneResponse, error)`, `(*Server).sendRevokeDataPlaneRequest(...)`, and `mintGrant(kind, dir, taskID, ttl) protocol.DataPlaneGrant`.

- [ ] **Step 1: Write the failing test**

Create `server/dataplane_test.go` covering `mintGrant`: a pull mints
`kind=open_file_transfer, direction=pull`, a `file ls` mints
`kind=list_files` with no direction, the grant id is random (two mints differ),
and the expiry is now+TTL.

```go
package server

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestMintGrantFileTransferCarriesDirection(t *testing.T) {
	tid := protocol.TaskID{Id: [16]uint8{4}}
	g := mintGrant(protocol.TaskControlKind_OpenFileTransfer, protocol.FileTransferDirection_Pull, tid, 5*time.Minute)
	if g.Kind != protocol.TaskControlKind_OpenFileTransfer || g.Direction != protocol.FileTransferDirection_Pull {
		t.Fatalf("kind/direction: %v %v", g.Kind, g.Direction)
	}
	if g.TaskId.Id != tid.Id {
		t.Fatalf("task id lost")
	}
	if g.ExpiresUnixMs <= uint64(time.Now().UnixMilli()) {
		t.Fatalf("expiry is not in the future")
	}
}

func TestMintGrantListFilesHasNoDirection(t *testing.T) {
	g := mintGrant(protocol.TaskControlKind_ListFiles, 0, protocol.TaskID{}, time.Minute)
	if g.Kind != protocol.TaskControlKind_ListFiles {
		t.Fatalf("kind: %v", g.Kind)
	}
}

func TestMintGrantIdsAreRandom(t *testing.T) {
	a := mintGrant(protocol.TaskControlKind_ListFiles, 0, protocol.TaskID{}, time.Minute)
	b := mintGrant(protocol.TaskControlKind_ListFiles, 0, protocol.TaskID{}, time.Minute)
	if a.GrantId == b.GrantId {
		t.Fatalf("two mints produced the same grant id")
	}
	if a.GrantId == ([16]uint8{}) {
		t.Fatalf("grant id is zero")
	}
}
```

- [ ] **Step 2: Run it to confirm it fails**

Run: `go test ./server -run TestMintGrant -v`
Expected: FAIL — `undefined: mintGrant`.

- [ ] **Step 3: Implement `mintGrant` and the request senders**

Create `server/dataplane.go`:

```go
package server

import (
	"context"
	"crypto/rand"
	"fmt"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// dataPlaneGrantTTL bounds how long an unredeemed grant stays usable. It does
// NOT bound a transfer: a push longer than this is normal and must not be cut.
const dataPlaneGrantTTL = 5 * time.Minute

// mintGrant produces the credential the runner will check. It names the
// request the caps check just passed, never a capability — the runner must not
// hold anything it could interpret as policy (spec D7).
func mintGrant(
	kind protocol.TaskControlKind,
	dir protocol.FileTransferDirection,
	taskID protocol.TaskID,
	ttl time.Duration,
) protocol.DataPlaneGrant {
	g := protocol.DataPlaneGrant{
		TaskId:        taskID,
		ExpiresUnixMs: uint64(time.Now().Add(ttl).UnixMilli()),
		Kind:          kind,
	}
	if _, err := rand.Read(g.GrantId[:]); err != nil {
		// crypto/rand failing is not a condition this process can continue
		// under: every grant it mints afterwards would be guessable.
		panic(fmt.Sprintf("data plane: crypto/rand: %v", err))
	}
	if kind == protocol.TaskControlKind_OpenFileTransfer {
		g.Direction = dir
	}
	return g
}

// sendAuthorizeDataPlaneRequest pushes a grant to a runner and waits for its
// answer, correlating on the runner's conn id exactly as
// sendEstablishRelayRequest does (server/server.go:1384).
func (s *Server) sendAuthorizeDataPlaneRequest(
	ctx context.Context, entry *RunnerEntry, req protocol.AuthorizeDataPlaneRequest,
) (protocol.AuthorizeDataPlaneResponse, error) {
	if entry == nil || entry.Conn == nil {
		return protocol.AuthorizeDataPlaneResponse{}, fmt.Errorf("nil entry / Conn")
	}
	connCID := entry.Conn.ConnectionID()
	respCh := make(chan protocol.AuthorizeDataPlaneResponse, 1)
	s.dpRespChMu.Lock()
	s.dpRespCh[connCID] = respCh
	s.dpRespChMu.Unlock()
	defer func() {
		s.dpRespChMu.Lock()
		if cur, ok := s.dpRespCh[connCID]; ok && cur == respCh {
			delete(s.dpRespCh, connCID)
		}
		s.dpRespChMu.Unlock()
	}()

	var rr protocol.RunnerRequest
	rr.Kind = protocol.RunnerRequestType_AuthorizeDataPlane
	rr.SetAuthorizeDataPlane(req)
	payload, err := rr.Append([]byte{byte(appwire.AppKind_RunnerControl)})
	if err != nil {
		return protocol.AuthorizeDataPlaneResponse{}, fmt.Errorf("encode AuthorizeDataPlane: %w", err)
	}
	if _, _, err := entry.Conn.SendMessage(payload); err != nil {
		return protocol.AuthorizeDataPlaneResponse{}, fmt.Errorf("send AuthorizeDataPlane: %w", err)
	}
	select {
	case <-ctx.Done():
		return protocol.AuthorizeDataPlaneResponse{}, ctx.Err()
	case resp := <-respCh:
		return resp, nil
	}
}

// sendRevokeDataPlaneRequest is fire-and-forget: a revoke is idempotent on the
// runner and the TTL is the backstop if it never lands (spec D6).
func (s *Server) sendRevokeDataPlaneRequest(entry *RunnerEntry, grantID [16]byte) {
	if entry == nil || entry.Conn == nil {
		return
	}
	var rr protocol.RunnerRequest
	rr.Kind = protocol.RunnerRequestType_RevokeDataPlane
	rr.SetRevokeDataPlane(protocol.RevokeDataPlaneRequest{GrantId: grantID})
	payload, err := rr.Append([]byte{byte(appwire.AppKind_RunnerControl)})
	if err != nil {
		return
	}
	entry.Conn.SendMessage(payload) //nolint:errcheck
}
```

Add to the `Server` struct in `server/server.go`, beside `relayRespCh`:

```go
	dpRespCh   map[objproto.ConnectionID]chan protocol.AuthorizeDataPlaneResponse
	dpRespChMu sync.Mutex
```

initialised in `New` alongside `relayRespCh`, and routed to from the runner
message handler where `EstablishRelayResponse` is routed today.

- [ ] **Step 4: Run the tests**

Run: `go test ./server -run TestMintGrant -v && make vet`
Expected: PASS.

- [ ] **Step 5: Rewire `handleOpenFileTransfer` and `handleListFiles`**

In `server/file_transfer.go`, replace the two-stream splice with: mint, push,
`SetProxy`, answer. Keep every existing status code and both existing checks —
task is `Running` or `Detached`, runner registered — unchanged and ahead of the
new work.

- [ ] **Step 6: Run the server suite**

Run: `go test ./server -count=1`
Expected: PASS. `TestOpenInteractive*` and `ForwardTap` are known flaky
(`project_flaky_test_open_interactive_sessionmux`); re-run those alone before
treating a failure there as yours.

- [ ] **Step 7: Commit**

```bash
git add server/dataplane.go server/dataplane_test.go server/file_transfer.go server/server.go
git commit -m "feat(server): mint a grant, push it, and forward packets instead of splicing"
```

---

### Task 7: Narrowing caps revokes the data plane

**Files:**
- Modify: `server/set_caps_handler.go:109-125`
- Test: `server/dataplane_revoke_test.go` (create)

**Interfaces:**
- Consumes: `sendRevokeDataPlaneRequest` (Task 6).
- Produces: `(*Server).revokeDataPlaneForTask(taskIDHex string) int`.

- [ ] **Step 1: Write the failing test**

A narrowing `set_caps` on a task with two live grants calls the revoke sender
twice and calls `DeleteProxy` for each proxied client id; `--keep-conns`
narrowing revokes nothing.

- [ ] **Step 2: Run it to confirm it fails**

Run: `go test ./server -run TestRevokeDataPlaneForTask -v`
Expected: FAIL — `undefined: revokeDataPlaneForTask`.

- [ ] **Step 3: Implement**

Track issued grants per task on the server (`map[string][]issuedGrant` with
the runner entry and the proxied client CID), and in `handleSetCaps`, inside
the existing `if narrowed && !req.KeepConns()` branch, call
`revokeDataPlaneForTask` before `DropConnsForPrincipal`. All three parts of
D6 fire here: `DeleteProxy` locally, the revoke to the runner, and the TTL
already on the grant.

- [ ] **Step 4: Run the tests**

Run: `go test ./server -run 'TestRevokeDataPlane|TestSetCaps' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add server/set_caps_handler.go server/dataplane_revoke_test.go server/dataplane.go
git commit -m "feat(server): narrowing caps revokes the grants it can no longer justify"
```

---

### Task 8: The client redeems the grant

**Files:**
- Modify: `cli/file_transfer.go` (`OpenFileTransfer`, `ListFiles`)
- Create: `cli/dataplane_dial.go`
- Modify: `cli/psk.go:90-100` (a `data_plane` hello alongside the agent one)
- Test: `cli/dataplane_dial_test.go` (create)

**Interfaces:**
- Consumes: the extended `OpenFileTransferResponse` (Task 1), the runner accept path (Task 5).
- Produces: `func (c *Client) dialDataPlane(ctx context.Context, resp dataPlaneTarget) (*peer.Conn, error)`.

- [ ] **Step 1: Write the failing test**

`cli/dataplane_dial_test.go`: building the `PskAuthRequest` for a data-plane
dial produces `ClientKind_DataPlane` and carries the grant id and task id
verbatim; a response of `bad_ticket` / `expired` / `not_permitted` maps to a
distinct, non-generic Go error each.

- [ ] **Step 2: Run it to confirm it fails**

Run: `go test ./cli -run TestDataPlaneDial -v`
Expected: FAIL — `undefined: dialDataPlane`.

- [ ] **Step 3: Implement**

Dial the slot CID on the client's existing endpoint, run the PSK handshake with
`ClientKind_DataPlane`, and return the `peer.Conn`. `OpenFileTransfer` and
`ListFiles` then create their stream on that conn instead of waiting for a
server-side stream id.

- [ ] **Step 4: Run the tests**

Run: `go test ./cli -run TestDataPlaneDial -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cli/dataplane_dial.go cli/file_transfer.go cli/psk.go cli/dataplane_dial_test.go
git commit -m "feat(cli): redeem the grant on a conn the server cannot read"
```

---

### Task 9: End to end, skew, and the argv path

**Files:**
- Create: `integration/dataplane_e2e_test.go`
- Test: the live paths

**Interfaces:** consumes everything above.

- [ ] **Step 1: Write the E2E test**

A push and a pull over the proxied path, asserting the bytes arrive AND that
the server's own endpoint never held a connection whose keys could decrypt
them — the second half is what distinguishes this from the existing transfer
test.

- [ ] **Step 2: Run it**

Run: `go test ./integration -run TestDataPlaneE2E -v -count=1`
Expected: PASS.

- [ ] **Step 3: Wire-skew check**

Run: `scripts/wire-skew-check.sh`
Expected: PASS — a `.bgn` changed, so this must prove a NEW runner against an
OLD server fails recoverably rather than fatally (Pitfall 10).

- [ ] **Step 4: The argv path, against a dummy harness**

```bash
scripts/dummy-harness.sh up --detach --agent fake --name dp
eval "$(scripts/dummy-harness.sh env --name dp)"
harness-cli file push  "$TASK" ./README.md up.md
harness-cli file pull  "$TASK" up.md /tmp/down.md
harness-cli file ls    "$TASK"
scripts/dummy-harness.sh down --name dp
```

Expected: all three succeed in the exact spelling the help text prints. A Go
test enters below argv parsing and the new response fields meet the parser only
here (Pitfall 13).

- [ ] **Step 5: Full verification**

Run: `make check && make vet && make test`
Expected: clean.

- [ ] **Step 6: Commit**

```bash
git add integration/dataplane_e2e_test.go
git commit -m "test: the bytes cross a conn the server cannot decrypt"
```

---

## Self-review

**Spec coverage.** P1 → Tasks 6 and 8 (the splice is replaced). P2 → Task 9
step 1 asserts it. D1 → Task 6. D2 → the plan touches only the file family.
D3 → Tasks 2 and 5. D4 → Task 1's `DataPlaneGrant`, distinct from
`auth_ticket`. D5 → Task 1's `ClientHello` arm plus Task 8. D6 → Tasks 2
(TTL/sweep), 4 (revoke) and 7 (narrowing). D7 → Task 2 holds no `Capability`.
D8 → Task 6 keeps the splice for `ws:`. D9 → Task 8 sends the grant as bytes.
D10 → Tasks 1, 3, 4.

**Surfaces.** No operator-visible option is added, so the input surfaces are
untouched. The one display change the spec names — a revoked transfer must say
so rather than surfacing a connection error — is Task 8 step 3's error mapping,
and it must reach CLI, TUI and WebUI renderings of a transfer error.

**Known gaps to close during execution.** Tasks 7, 8 and 9 give the shape and
the test names but not every line; write the failing test first in each and let
it dictate the signature, per the step order above.
