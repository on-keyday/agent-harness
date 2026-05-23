# Chained relay — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

## Subagent dispatch protocol (READ BEFORE DELEGATING)

Every implementer / reviewer subagent prompt for this plan **MUST** include the following two clauses verbatim. They exist because this branch already burned 148 commits of divergence on a harness-worktree routing bug — see "Why this is non-negotiable" below.

### Clause 1: Working directory + branch

> **Work in the parent repo at `/home/kforfk/workspace/remote-agent-harness/`, NOT in any harness worktree under `.harness-worktrees/`.** Verify with `git -C /home/kforfk/workspace/remote-agent-harness rev-parse --abbrev-ref HEAD` — must report `feature/chained-relay-spec` (or whatever the active impl branch is per the controller). All file paths in your tool calls should be absolute under `/home/kforfk/workspace/remote-agent-harness/...`. Do not `cd` into a harness worktree.

### Clause 2: Required reading

> **Read `.claude/skills/implementation-pitfalls/SKILL.md` in full before writing any code.** That file is the project-local failure catalog — 7 pitfalls + a controller dispatch checklist + project principles ported from user-level memory. Each entry was triggered by a real prior incident; the rules apply to this task too. If a section feels irrelevant, read it anyway — irrelevance is your judgment, and prior agents on this project misjudged the same way (that's why the catalog exists).

### Why this is non-negotiable

The harness runner spawns each task inside `/home/.../remote-agent-harness/.harness-worktrees/<hash>/`. The Edit / Write tools, when called with absolute paths under `/home/.../remote-agent-harness/<rel>`, **resolve to the parent repo's main checkout**, not the worktree. The worktree's HEAD points at an auto-generated `harness/<hash>` branch that does NOT track `feature/chained-relay-spec`. Result: writes silently land on the parent's branch while the worktree stays stale. A subagent that uses relative paths from a worktree cwd will diverge from the controller's view; a subagent that uses absolute paths to the worktree will silently land on the parent.

The defensive posture: **be explicit about the parent repo path everywhere**, and reference the pitfalls catalog so the next confusion has a precedent it can recognize.

### Sample dispatch prompt skeleton

```
You are implementing Task <N> of docs/superpowers/plans/2026-05-23-chained-relay.md.

Before you start:
1. Verify cwd is /home/kforfk/workspace/remote-agent-harness/ (parent repo). Do NOT operate in any harness worktree.
2. Run: git rev-parse --abbrev-ref HEAD — expect feature/chained-relay-spec.
3. Read .claude/skills/implementation-pitfalls/SKILL.md in full. Note the entries about worktree routing, sibling-code grep, peer.Conn.Close semantics, and the controller dispatch checklist.

Task: <Task N text verbatim from the plan>

Context:
<context relevant to this task — files, tests already in place, etc.>

When you're done, report DONE / DONE_WITH_CONCERNS / NEEDS_CONTEXT / BLOCKED per the subagent-driven-development skill, with the commit SHA(s) you produced.
```

---

**Goal:** Allow an agent running on a runner that was itself registered via Phase C (chained-relay-pending bug) to successfully `cli.Dial` the server. Currently agent dials fail because target_runner's Phase B `SetProxy` allocate-side points at target's `serverCID.Addr` (= proxy_runner after Phase C), and proxy_runner has no SetProxy entry for the new agent-chosen slot, so the rehandshake packet is rejected. This plan implements server-orchestrated chained relay so each intermediate hop gets the right SetProxy entry before the rehandshake flies.

**Architecture:** runner L sends `RequestChainedRelay{slot_id}` to server before its local Phase B SetProxy. Server walks L's `Via` chain in the registry, dispatches an `EstablishRelayRequest{slot_id, target=H_downstream.ViaDialAddr}` to each intermediate hop in parallel. Each hop does eager synthetic-owned `SetProxy` on receipt. After all hops ack, server replies `ChainedRelayResponse{Ok}` and L proceeds with its own SetProxy. Agent's rehandshake then flies through every SetProxy entry transparently, reaches server, end-to-end AEAD validates.

**Tech Stack:** Go, brgen `.bgn` schema, objproto SetProxy + sendRehandshakeForProxy, peer.WrapAcceptedConn (Phase B prerequisite).

**Spec:** `docs/superpowers/specs/2026-05-23-chained-relay-design.md`.
**Prereq:** Phase A (reverse-dial) + Phase B (agent_proxy ceremony) + Phase C (`server-to-runner-via-relay`) all merged on main.

Already landed on this branch (so not in Task scope):
- Commit `f2b0f5c`: `objproto.SetProxy` synthetic-owned relaxation (Task 1 code change).
- Commit `d31b5c9`: `HandleWithVia` sources slot_id from `target.UniqueNumber` (spec's "commit 0").
- POC test (`integration/chained_relay_poc_test.go`): wire-level mechanics for 3-hop and 4-hop chains, role-boundary enforced. Green.
- RED e2e (`integration/chained_relay_e2e_test.go`): pins the missing-feature failure mode. RED — will be flipped in Task 8.

---

## File Structure

| File | Action | Responsibility |
|---|---|---|
| `objproto/objproto.go` | Already landed (`f2b0f5c`) | `owned` precondition dropped in `SetProxy`; POC test exercises end-to-end |
| `objproto/objproto_test.go` | Optional (Task 1) | Package-level unit test for synthetic-owned `SetProxy`; skip if POC coverage is judged sufficient |
| `server/registry.go` | Modify (Task 2) | Add `Via *RunnerEntry` + `ViaDialAddr objproto.ConnectionID` fields to `RunnerEntry` |
| `server/registry_test.go` | Modify (Task 2) | Tests populating Via + ViaDialAddr; walk Via chain; verify zero for Phase A direct + reverse-dial |
| `server/dial_runner_handler.go` | Modify (Tasks 2 + 7) | (T2) Extend `OnDialed` signature to carry `ViaRegistrationInfo`; `HandleWithVia` constructs and passes it. (T7) Drop `RehandshakeForProxy` ceremony — server's first `SendHandshake` IS the e2e ECDH with target |
| `server/server.go` | Modify (Tasks 2 + 5) | (T2) `handleConnection` accepts `ViaRegistrationInfo`, stashes in a per-conn-CID map. (T5) Wire `RequestChainedRelay` handler |
| `server/runner_handler.go` | Modify (Tasks 2 + 5) | (T2) Hello case consults per-conn-CID map for via info. (T5) Decode and dispatch `RequestChainedRelay` |
| `server/chained_relay_handler.go` | Create (Task 5) | Server-side: walk `Via` chain, dispatch `EstablishRelayRequest` in parallel, send `ChainedRelayResponse` |
| `server/chained_relay_handler_test.go` | Create (Task 5) | Unit tests: direct runner / 2-hop / broken Via loop / hop timeout |
| `runner/protocol/message.bgn` | Modify (Task 3) | Add `RequestChainedRelay`, `ChainedRelayStatus`, `ChainedRelayResponse`; variants on `RunnerMessage` + `RunnerRequest` |
| `runner/protocol/message.go` | Regenerated (Task 3) | `make protoregen` output |
| `runner/protocol/chained_relay_test.go` | Create (Task 3) | Round-trip tests for the new wire types |
| `runner/relay_handler.go` | Modify (Task 4) | `handleEstablishRelay` switches from lazy (`expectedRelays` + `completeRelaySetup`) to eager `SetProxy`; remove `expectedRelays`, `completeRelaySetup`, `Session.ExpectedRelays` |
| `runner/relay_handler_test.go` | Modify (Task 4) | Update expectations: eager SetProxy installed at handler time, no `completeRelaySetup` path |
| `runner/listen.go` | Modify (Task 4) | Remove `sess.ExpectedRelays.Take(slotID)` short-circuit in `handleAcceptedConn` |
| `runner/session.go` | Modify (Tasks 4 + 6) | (T4) Drop `ExpectedRelays` field. (T6) Add per-slot `chainedRelayRespCh` map for response correlation |
| `runner/connect.go` | Modify (Task 6) | `dispatchRunnerRequest`: add `ChainedRelayResponse` arm — route to the per-slot waiter |
| `runner/agent_proxy.go` | Modify (Task 6) | `runAgentProxyCeremony`: emit `RequestChainedRelay`, wait response, gate local SetProxy on Ok/Direct |
| `integration/chained_relay_poc_test.go` | No change | Already PASSING; documents wire-level mechanics |
| `integration/chained_relay_e2e_test.go` | Modify (Task 8) | Flip from RED (expect failure) to GREEN (expect `cli.List` success). Drop the "expected to fail" narrative |
| `integration/chained_relay_3hop_e2e_test.go` | Create (Task 8) | New: 3-hop chain (agent → L → P → Q → server) — `cli.List` succeeds + Via chain visible in registry |

---

## Task 1: `objproto.SetProxy` synthetic-owned relaxation — package-level unit test

**Files:**
- Modify: `objproto/objproto_test.go`

**Status:** The code relaxation is ALREADY LANDED on this branch as commit `f2b0f5c` — `objproto.SetProxy` no longer rejects synthetic `owned`. The POC test in `integration/chained_relay_poc_test.go` exercises the synthetic-owned path end-to-end and is green. This Task only adds an `objproto`-package-level unit test for closer coverage (so a future refactor that re-adds the precondition fails fast inside the package, not just at integration time).

If you decide the POC coverage is sufficient, this Task can be skipped — note that in the commit message and move on to Task 2.

**Why (if pursued):** Provides a minimal regression gate inside the `objproto` package. The integration POC requires the `integration` build tag and spins up multiple endpoints; a unit test that drives a single endpoint's `receive()` directly is faster to run and clearer to debug.

- [ ] **Step 1: Add the unit test**

Add to `objproto/objproto_test.go`:

```go
func TestSetProxy_SyntheticOwned_ForwardsViaProxySettings(t *testing.T) {
    // Two in-process WS endpoints A ↔ B; configure A's SetProxy with a
    // synthetic owned (no preceding handshake), send a packet at the
    // synthetic CID, observe that B sees the packet at the allocate CID.
    // Verifies the synthetic-owned SetProxy contract that Task 4's eager
    // handler depends on.
    ...
}
```

Reference: existing `objproto` tests show endpoint setup pattern. The POC test in `integration/chained_relay_poc_test.go` shows the synthetic-owned setup if you need a working template.

- [ ] **Step 2: Run the test + the full objproto suite**

```sh
go test ./objproto/ -run TestSetProxy_SyntheticOwned -count=1 -v
go test ./objproto/ -count=1
```

Must pass; existing tests must stay green.

**Green criteria:**
- New synthetic-owned unit test passes (if added).
- All existing `objproto` tests pass.
- `go build ./...` clean.

---

## Task 2: `RunnerEntry` Via + ViaDialAddr fields + Phase C plumbing

**Files:**
- Modify: `server/registry.go`
- Modify: `server/registry_test.go`
- Modify: `server/dial_runner_handler.go`
- Modify: `server/server.go`
- Modify: `server/runner_handler.go`

**Why:** Server needs `entry.Via` to walk the chain at chained-relay setup time. `entry.ViaDialAddr` carries the addr each upstream hop knows the downstream by (= L's LISTEN_ADDR for hop P's SetProxy.allocate), which is otherwise erased after Phase C registration. Spec lines 130-200.

- [ ] **Step 1: Define the fields in `RunnerEntry`**

```go
// server/registry.go
type RunnerEntry struct {
    // ... existing fields ...

    // Via, when non-nil, is the proxy_runner this runner was registered
    // through via Phase C (--via). nil for Phase A direct and reverse-dial
    // (runner.Connect) registrations.
    Via *RunnerEntry

    // ViaDialAddr = protocol.RunnerIDToConnID(target) captured at Phase C
    // HandleWithVia time. Only Transport + Addr are load-bearing — the ID
    // portion happens to carry admin's UniqueNumber but no consumer reads
    // it. Zero for Phase A direct + reverse-dial.
    ViaDialAddr objproto.ConnectionID
}
```

- [ ] **Step 2: Add registry unit tests**

```go
// server/registry_test.go additions
func TestRegistry_PhaseAEntry_NoVia(t *testing.T) {
    // Add an entry as if Phase A registered it; assert Via == nil + ViaDialAddr zero.
}

func TestRegistry_PhaseCEntry_ViaPopulated(t *testing.T) {
    // Build a parent entry P. Add a child L with Via=P + ViaDialAddr=cid.
    // Assert: get back L; walk Via reaches P; P.Via == nil.
}

func TestRegistry_ViaWalk_TerminatesAtNil(t *testing.T) {
    // Q (Via=nil) → P (Via=Q) → L (Via=P). Walk from L: hits L, P, Q, stops.
    // Negative: a cycle would loop; the walk caller is responsible for loop detection.
}
```

- [ ] **Step 3: Introduce `ViaRegistrationInfo` + extend `OnDialed`**

`server/dial_runner_handler.go`:

```go
type ViaRegistrationInfo struct {
    Via         *RunnerEntry
    ViaDialAddr objproto.ConnectionID
}

type DialRunnerHandler struct {
    // ... existing fields ...
    OnDialed func(ctx context.Context, conn objproto.Connection, viaInfo *ViaRegistrationInfo)
}
```

Update `Handle` (Phase A) to pass `nil`:
```go
h.OnDialed(ctx, conn, nil)
```

Update `HandleWithVia` (Phase C) to construct and pass info AFTER `OnDialed` is reached:
```go
viaInfo := &ViaRegistrationInfo{
    Via:         entry,                                  // the resolved Via runner entry
    ViaDialAddr: protocol.RunnerIDToConnID(target),      // admin's CLI target
}
h.OnDialed(ctx, endToEndConn, viaInfo)
```

- [ ] **Step 4: `server.go` per-conn-CID map**

In `Server`:
```go
pendingViaInfo   map[objproto.ConnectionID]*ViaRegistrationInfo
pendingViaInfoMu sync.Mutex
```

In `Server.New`, replace the existing `OnDialed` wiring:
```go
s.taskHandler.OnDialed = func(connCtx context.Context, conn objproto.Connection, viaInfo *ViaRegistrationInfo) {
    if viaInfo != nil {
        s.pendingViaInfoMu.Lock()
        s.pendingViaInfo[conn.ConnectionID()] = viaInfo
        s.pendingViaInfoMu.Unlock()
    }
    go s.handleConnection(connCtx, conn)
}
```

Add a helper:
```go
func (s *Server) takePendingViaInfo(cid objproto.ConnectionID) *ViaRegistrationInfo {
    s.pendingViaInfoMu.Lock()
    defer s.pendingViaInfoMu.Unlock()
    info := s.pendingViaInfo[cid]
    delete(s.pendingViaInfo, cid)
    return info
}
```

Cleanup on conn close: `handleConnection`'s defer should call `takePendingViaInfo` if it hasn't been consumed yet, so a conn that closes before Hello doesn't leak.

- [ ] **Step 5: `runner_handler.go` Hello case consults the map**

`Server.New` must pass the helper to `RunnerHandler`:
```go
s.runnerHandler.TakePendingViaInfo = s.takePendingViaInfo
```

In `RunnerHandler.Handle` Hello case:
```go
entry := &RunnerEntry{
    ID:           runnerID,
    Hostname:     string(hello.Hostname),
    // ... existing
}
entry.Conn = conn
if h.TakePendingViaInfo != nil {
    if info := h.TakePendingViaInfo(conn.ConnectionID()); info != nil {
        entry.Via = info.Via
        entry.ViaDialAddr = info.ViaDialAddr
    }
}
h.Registry.Add(entry)
```

- [ ] **Step 6: Update existing tests that build `DialRunnerHandler.OnDialed`**

Find all places (test and prod) where `OnDialed` is wired and update for the new 3rd argument. Search:

```sh
grep -rn "OnDialed" server/ cli/ integration/
```

For tests that don't care about via info, pass a wrapper that ignores `viaInfo`:
```go
h.OnDialed = func(ctx context.Context, conn objproto.Connection, _ *ViaRegistrationInfo) { ... }
```

- [ ] **Step 7: Run targeted tests**

```sh
go test ./server/ -count=1
go test -tags integration ./integration/ -run "TestRelayE2E|TestDialRunner" -count=1
```

All green.

**Green criteria:**
- New registry tests pass.
- All existing server tests pass.
- Phase C E2E (`TestRelayE2E`, `TestRelayE2E_DialModeProxy`) stays green.
- `harness-cli ls` style code that reads `RunnerEntry` keeps compiling (Via + ViaDialAddr are additive).

---

## Task 3: Wire schema additions (`RequestChainedRelay` / `ChainedRelayResponse`)

**Files:**
- Modify: `runner/protocol/message.bgn`
- Regenerate: `runner/protocol/message.go`
- Create: `runner/protocol/chained_relay_test.go`

**Why:** New message types for the chained relay handshake — runner → server request and server → runner response. Per [[feedback_no_split_schemas]], ALL wire additions for chained relay live in this Task; nothing else adds wire types.

- [ ] **Step 1: Edit `message.bgn`**

Add new formats (place near `EstablishRelay*`):

```bgn
# runner → server: "before I install my local SetProxy for an agent Phase B
# ceremony, walk my chain on your side and make sure every upstream hop
# has the right SetProxy entry for slot_id."
format RequestChainedRelay:
    slot_id :u16

enum ChainedRelayStatus:
    :u8
    ok                  = "ok"
    direct              = "direct"             # no chain — runner is registered direct
    slot_collision      = "slot_collision"
    hop_setup_failed    = "hop_setup_failed"
    chain_unwalkable    = "chain_unwalkable"
    another_in_flight   = "another_in_flight"

format ChainedRelayResponse:
    status :ChainedRelayStatus
```

Add variants:
- `RunnerMessageType.request_chained_relay` (runner → server)
- `RunnerRequestType.chained_relay_response` (server → runner)

Both with the standard `body` reference to the format above. Mirror the existing `EstablishRelay*` variant style in the same file.

- [ ] **Step 2: Regenerate**

```sh
make protoregen
```

Verify `runner/protocol/message.go` got updated.

- [ ] **Step 3: Write round-trip tests**

`runner/protocol/chained_relay_test.go`:

```go
package protocol

import (
    "bytes"
    "testing"
)

func TestRequestChainedRelayRoundTrip(t *testing.T) {
    orig := RequestChainedRelay{SlotId: 0xABCD}
    buf, err := orig.Append(nil)
    if err != nil { t.Fatal(err) }
    var got RequestChainedRelay
    if _, err := got.Decode(buf); err != nil { t.Fatal(err) }
    if got.SlotId != orig.SlotId { t.Errorf("SlotId mismatch") }
}

func TestChainedRelayResponseRoundTrip(t *testing.T) {
    for _, st := range []ChainedRelayStatus{
        ChainedRelayStatus_Ok,
        ChainedRelayStatus_Direct,
        ChainedRelayStatus_SlotCollision,
        ChainedRelayStatus_HopSetupFailed,
        ChainedRelayStatus_ChainUnwalkable,
        ChainedRelayStatus_AnotherInFlight,
    } {
        orig := ChainedRelayResponse{Status: st}
        buf, err := orig.Append(nil)
        if err != nil { t.Fatalf("status %v: %v", st, err) }
        var got ChainedRelayResponse
        if _, err := got.Decode(buf); err != nil { t.Fatalf("status %v: %v", st, err) }
        if got.Status != orig.Status { t.Errorf("status %v: roundtrip mismatch %v", st, got.Status) }
    }
}

func TestRunnerMessage_RequestChainedRelay_Variant(t *testing.T) {
    inner := RequestChainedRelay{SlotId: 42}
    msg := &RunnerMessage{Kind: RunnerMessageType_RequestChainedRelay}
    msg.SetRequestChainedRelay(inner)
    buf, err := msg.Append(nil)
    if err != nil { t.Fatal(err) }
    var got RunnerMessage
    if _, err := got.Decode(buf); err != nil { t.Fatal(err) }
    if got.Kind != RunnerMessageType_RequestChainedRelay { t.Fatal("kind") }
    rcr := got.RequestChainedRelay()
    if rcr == nil || rcr.SlotId != 42 { t.Fatal("inner") }
}

func TestRunnerRequest_ChainedRelayResponse_Variant(t *testing.T) {
    // mirror of above for RunnerRequest direction
    ...
}
```

- [ ] **Step 4: Run tests**

```sh
go test ./runner/protocol/ -run "ChainedRelay" -count=1 -v
```

All pass.

**Green criteria:**
- All round-trip tests pass.
- `make protoregen` produces clean diff (no other files mutated by accident).
- `go build ./...` succeeds.

---

## Task 4: Phase C handler refactor — eager `SetProxy`

**Files:**
- Modify: `runner/relay_handler.go`
- Modify: `runner/relay_handler_test.go`
- Modify: `runner/listen.go`
- Modify: `runner/session.go`

**Why:** Current `handleEstablishRelay` is lazy — it just records `expectedRelays[slot_id] = target`, and the actual `SetProxy` runs from `completeRelaySetup` when the matching activeConn arrives at the accept handler. For chained relay, no activeConn arrives at proxy_runner — the agent's rehandshake is forwarded raw to the next hop, never landing locally. Eager `SetProxy` (Task 1 prerequisite) installs the entry immediately so the rehandshake hits `proxySettings` and gets forwarded. Spec lines 232-256.

This also lets us delete `expectedRelays`, `completeRelaySetup`, and the listen.go short-circuit — dead code in the new design.

- [ ] **Step 1: Update tests to expect eager SetProxy**

In `runner/relay_handler_test.go`, change assertions:
- After `handleEstablishRelay` returns `Ok`, the endpoint MUST already have `proxySettings` populated at `(ownedCID, allocCID)`.
- `expectedRelays` is gone — remove any assertion that reads it.
- `completeRelaySetup` is gone — remove the test that drives it.

- [ ] **Step 2: Rewrite `handleEstablishRelay`**

```go
func handleEstablishRelay(
    ctx context.Context,
    logger *slog.Logger,
    st *relayHandlerState,
    ep objproto.Endpoint,
    req protocol.EstablishRelayRequest,
    sendResponse func(protocol.EstablishRelayResponse) error,
) {
    resp := st.validate(req)
    if resp.Status == protocol.EstablishRelayStatus_Ok {
        target := protocol.RunnerIDToConnID(req.Target)
        ownedCID := objproto.NewConnectionID(
            st.serverCID.Transport, st.serverCID.Addr, req.SlotId)
        allocCID := objproto.NewConnectionID(
            target.Transport, target.Addr, req.SlotId)
        if err := ep.SetProxy(ownedCID, allocCID); err != nil {
            logger.Warn("relay: eager SetProxy failed",
                "owned", ownedCID.String(), "allocate", allocCID.String(), "err", err)
            resp.Status = protocol.EstablishRelayStatus_InvalidTarget
        }
    }
    if err := sendResponse(resp); err != nil {
        logger.Warn("relay: send response failed", "err", err)
    }
}
```

Note: the `expected *expectedRelays` parameter is gone. `ep objproto.Endpoint` is new — caller (dispatchRunnerRequest) must thread it. Caller change is in `runner/connect.go` / wherever EstablishRelay is dispatched.

- [ ] **Step 3: Delete dead code**

- Remove `type expectedRelays`, `newExpectedRelays`, `Put`, `Take` from `relay_handler.go`.
- Remove `completeRelaySetup` from `relay_handler.go`.
- Remove the `sess.ExpectedRelays.Take(slotID)` short-circuit in `runner/listen.go`'s `handleAcceptedConn`. Look for the block that handles the relay slot specially — delete it; the rehandshake now hits `proxySettings` in `objproto.receive` before listen.go's accept loop ever sees the new conn.
- Remove `ExpectedRelays` field from `runner/session.go`'s `Session` struct; remove its initialization.

- [ ] **Step 4: Run runner tests**

```sh
go test ./runner/ -count=1
go test ./runner/protocol/ -count=1
```

All green.

- [ ] **Step 5: Run Phase C E2E**

```sh
go test -tags integration ./integration/ -run "TestRelayE2E" -count=1 -v
```

Must stay green — spec line 250-255 explains why eager SetProxy is backward-compatible with the existing Phase C ceremony (server's initial ECDH still creates the activeConn; eager SetProxy entry just gets ignored on owned side until the rehandshake; RehandshakeForProxy then triggers forward via proxySettings, same end state).

If a regression appears, it's a bug in the refactor, NOT a sign that we should keep `expectedRelays`. Debug eagerly.

**Green criteria:**
- `relay_handler_test.go` passes with eager assertions.
- `runner/listen_test.go` and any session-aware tests pass.
- `TestRelayE2E` + `TestRelayE2E_DialModeProxy` stay green.
- `expectedRelays` / `completeRelaySetup` / `Session.ExpectedRelays` are gone (grep returns no hits).

---

## Task 5: Server-side `RequestChainedRelay` handler + chain walk

**Files:**
- Create: `server/chained_relay_handler.go`
- Create: `server/chained_relay_handler_test.go`
- Modify: `server/runner_handler.go`
- Modify: `server/server.go`

**Why:** Receive `RunnerMessage{RequestChainedRelay{slot_id}}` from a runner; walk its `Via` chain to find each intermediate hop; dispatch `EstablishRelayRequest` to all of them in parallel; reply `ChainedRelayResponse{Ok}` once all hops ack. Spec lines 232-273.

- [ ] **Step 1: Define the handler in `chained_relay_handler.go`**

```go
package server

type ChainedRelayHandler struct {
    Logger                  *slog.Logger
    Registry                *Registry
    SendEstablishRelay      func(ctx context.Context, entry *RunnerEntry,
                                req protocol.EstablishRelayRequest) (
                                protocol.EstablishRelayResponse, error)
    HopTimeout              time.Duration  // 0 → 10s default

    inFlight   map[string]struct{}  // keyed by runner conn CID string
    inFlightMu sync.Mutex
}

func (h *ChainedRelayHandler) Handle(
    ctx context.Context,
    conn ConnHandle,
    req protocol.RequestChainedRelay,
) protocol.ChainedRelayResponse {
    runnerID := conn.ConnectionID().String()

    // In-flight guard
    h.inFlightMu.Lock()
    if _, exists := h.inFlight[runnerID]; exists {
        h.inFlightMu.Unlock()
        return protocol.ChainedRelayResponse{
            Status: protocol.ChainedRelayStatus_AnotherInFlight}
    }
    h.inFlight[runnerID] = struct{}{}
    h.inFlightMu.Unlock()
    defer func() {
        h.inFlightMu.Lock()
        delete(h.inFlight, runnerID)
        h.inFlightMu.Unlock()
    }()

    // 1. Look up requesting runner L
    entry, ok := h.Registry.Get(runnerID)
    if !ok {
        h.Logger.Warn("chained-relay: requester not in registry", "runner", runnerID)
        return protocol.ChainedRelayResponse{Status: protocol.ChainedRelayStatus_ChainUnwalkable}
    }

    // 2. If L.Via == nil, no chain needed.
    if entry.Via == nil {
        return protocol.ChainedRelayResponse{Status: protocol.ChainedRelayStatus_Direct}
    }

    // 3. Walk L.Via.Via... collecting (hop, downstream.ViaDialAddr) pairs.
    type hopSetup struct {
        hop      *RunnerEntry
        downViaDialAddr objproto.ConnectionID
    }
    var hops []hopSetup
    cur := &entry
    seen := map[string]struct{}{entry.ID: {}}
    for cur.Via != nil {
        if _, ok := seen[cur.Via.ID]; ok {
            return protocol.ChainedRelayResponse{Status: protocol.ChainedRelayStatus_ChainUnwalkable}
        }
        hops = append(hops, hopSetup{hop: cur.Via, downViaDialAddr: cur.ViaDialAddr})
        seen[cur.Via.ID] = struct{}{}
        cur = cur.Via
    }

    // 4. Dispatch EstablishRelay to all hops in parallel.
    timeout := h.HopTimeout
    if timeout == 0 { timeout = 10 * time.Second }
    hopCtx, hopCancel := context.WithTimeout(ctx, timeout)
    defer hopCancel()

    type result struct {
        ok  bool
        err error
    }
    results := make(chan result, len(hops))
    for _, hp := range hops {
        hp := hp
        go func() {
            req := protocol.EstablishRelayRequest{
                Target: protocol.ConnIDToRunnerID(hp.downViaDialAddr),
                SlotId: req.SlotId,
            }
            resp, err := h.SendEstablishRelay(hopCtx, hp.hop, req)
            results <- result{
                ok:  err == nil && resp.Status == protocol.EstablishRelayStatus_Ok,
                err: err,
            }
        }()
    }

    // 5. Collect all results.
    allOk := true
    for i := 0; i < len(hops); i++ {
        r := <-results
        if !r.ok {
            allOk = false
            h.Logger.Warn("chained-relay: hop failed", "err", r.err)
        }
    }

    if !allOk {
        return protocol.ChainedRelayResponse{Status: protocol.ChainedRelayStatus_HopSetupFailed}
    }
    return protocol.ChainedRelayResponse{Status: protocol.ChainedRelayStatus_Ok}
}
```

- [ ] **Step 2: Unit tests**

`chained_relay_handler_test.go`:

```go
func TestChainedRelay_Direct(t *testing.T) {
    // Registry with L (Via=nil). Handle → Status=Direct, no SendEstablishRelay call.
}

func TestChainedRelay_2Hop(t *testing.T) {
    // Registry: P (Via=nil), L (Via=P, ViaDialAddr=L_addr).
    // Stub SendEstablishRelay to return Ok.
    // Handle → Status=Ok; assert SendEstablishRelay called with (entry=P, Target=L_addr, SlotId=42).
}

func TestChainedRelay_3Hop_Parallel(t *testing.T) {
    // Q (Via=nil), P (Via=Q, ViaDialAddr=P_addr), L (Via=P, ViaDialAddr=L_addr).
    // Stub SendEstablishRelay records call order via a sync.WaitGroup with delays;
    // assert both dispatched concurrently (not strictly sequential).
}

func TestChainedRelay_HopFailure(t *testing.T) {
    // 2-hop with stub returning non-Ok. Expect HopSetupFailed.
}

func TestChainedRelay_AnotherInFlight(t *testing.T) {
    // First call blocks (stub takes 200ms). Second call from same runner returns AnotherInFlight immediately.
}

func TestChainedRelay_LoopDetection(t *testing.T) {
    // Manually construct A.Via=B, B.Via=A (cycle). Expect ChainUnwalkable.
}
```

- [ ] **Step 3: Wire dispatch from `RunnerHandler`**

In `RunnerHandler.Handle`, add case:

```go
case protocol.RunnerMessageType_RequestChainedRelay:
    rcr := msg.RequestChainedRelay()
    if rcr == nil {
        slog.Error("RunnerHandler: RequestChainedRelay variant nil", "runnerID", runnerID)
        return
    }
    if h.ChainedRelay == nil {
        slog.Warn("RunnerHandler: RequestChainedRelay arrived but no handler wired",
            "runnerID", runnerID)
        return
    }
    resp := h.ChainedRelay.Handle(context.Background(), conn, *rcr)
    rrResp := &protocol.RunnerRequest{Kind: protocol.RunnerRequestType_ChainedRelayResponse}
    rrResp.SetChainedRelayResponse(resp)
    if rrBytes, err := rrResp.Append([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)}); err != nil {
        slog.Error("RunnerHandler: encode ChainedRelayResponse failed", "err", err)
    } else if _, _, err := conn.SendMessage(rrBytes); err != nil {
        slog.Error("RunnerHandler: send ChainedRelayResponse failed", "err", err)
    }
    return  // suppress trailing OnChange — no Registry mutation
```

Add the field on `RunnerHandler`:
```go
ChainedRelay *ChainedRelayHandler
```

- [ ] **Step 4: Wire in `server.go`**

In `Server.New`:
```go
s.chainedRelay = &ChainedRelayHandler{
    Logger:             cfg.Logger,
    Registry:           s.registry,
    SendEstablishRelay: s.sendEstablishRelayRequest,
}
s.runnerHandler.ChainedRelay = s.chainedRelay
```

- [ ] **Step 5: Run tests**

```sh
go test ./server/ -run "TestChainedRelay" -count=1 -v
go test ./server/ -count=1
```

**Green criteria:**
- All 6 unit tests pass.
- `server` package builds; existing tests stay green.
- No new lints.

---

## Task 6: Runner-side `runAgentProxyCeremony` emits `RequestChainedRelay`

**Files:**
- Modify: `runner/session.go`
- Modify: `runner/connect.go`
- Modify: `runner/agent_proxy.go`

**Why:** Before local Phase B `SetProxy`, runner must ask server to set up the chain. If server replies `Direct`, runner proceeds as today (no chain). If `Ok`, proceed (all hops ready). Otherwise reject the agent ProxyRequest. Spec lines 285-301.

- [ ] **Step 1: Add per-slot response channel to `Session`**

```go
// runner/session.go
type Session struct {
    // ... existing
    chainedRelayWaitersMu sync.Mutex
    chainedRelayWaiters   map[uint16]chan protocol.ChainedRelayResponse
}
```

Init in the Session constructor; close all on session shutdown.

Add helpers:
```go
func (s *Session) RegisterChainedRelayWaiter(slotID uint16) chan protocol.ChainedRelayResponse {
    s.chainedRelayWaitersMu.Lock()
    defer s.chainedRelayWaitersMu.Unlock()
    ch := make(chan protocol.ChainedRelayResponse, 1)
    s.chainedRelayWaiters[slotID] = ch
    return ch
}

func (s *Session) DeliverChainedRelayResponse(slotID uint16, resp protocol.ChainedRelayResponse) bool {
    s.chainedRelayWaitersMu.Lock()
    defer s.chainedRelayWaitersMu.Unlock()
    ch, ok := s.chainedRelayWaiters[slotID]
    if !ok { return false }
    delete(s.chainedRelayWaiters, slotID)
    ch <- resp
    return true
}
```

Note: `slotID` isn't carried on the `ChainedRelayResponse` wire format. Since the design constrains ONE in-flight RequestChainedRelay per runner conn (spec Decision 2 / line 295), we can:
- Option A: Re-add slot_id to the wire format. Easier correlation.
- Option B: Track "currently-pending slot" on Session (single uint16 + bool). Matches the one-at-a-time invariant.

**Decision: Option B.** Spec already commits to one-at-a-time at the protocol level. Simpler wire.

Revise:
```go
type Session struct {
    chainedRelayPendingMu sync.Mutex
    chainedRelayPendingCh chan protocol.ChainedRelayResponse  // nil when none pending
}

func (s *Session) BeginChainedRelay() (chan protocol.ChainedRelayResponse, error) {
    s.chainedRelayPendingMu.Lock()
    defer s.chainedRelayPendingMu.Unlock()
    if s.chainedRelayPendingCh != nil {
        return nil, fmt.Errorf("chained relay already in flight")
    }
    s.chainedRelayPendingCh = make(chan protocol.ChainedRelayResponse, 1)
    return s.chainedRelayPendingCh, nil
}

func (s *Session) DeliverChainedRelayResponse(resp protocol.ChainedRelayResponse) bool {
    s.chainedRelayPendingMu.Lock()
    defer s.chainedRelayPendingMu.Unlock()
    if s.chainedRelayPendingCh == nil { return false }
    s.chainedRelayPendingCh <- resp
    s.chainedRelayPendingCh = nil
    return true
}
```

- [ ] **Step 2: Dispatch arm in `connect.go`**

In `dispatchRunnerRequest`:
```go
case protocol.RunnerRequestType_ChainedRelayResponse:
    rcr := req.ChainedRelayResponse()
    if rcr == nil {
        logger.Error("dispatch: ChainedRelayResponse nil")
        return
    }
    if !sess.DeliverChainedRelayResponse(*rcr) {
        logger.Warn("dispatch: ChainedRelayResponse without waiter", "status", rcr.Status)
    }
```

- [ ] **Step 3: Emit + wait in `runAgentProxyCeremony`**

Before the existing local SetProxy:
```go
slotID := agentCID.ID  // agent's chosen ID becomes the chained-relay slot

ch, err := sess.BeginChainedRelay()
if err != nil {
    logger.Warn("chained-relay: another in flight on this session", "err", err)
    // reject agent with InternalError
    return
}

req := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_RequestChainedRelay}
req.SetRequestChainedRelay(protocol.RequestChainedRelay{SlotId: slotID})
if reqBytes, err := req.Append([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)}); err != nil {
    // ... handle, return
} else if err := sess.Sender.Send(reqBytes); err != nil {
    // ... handle, return
}

select {
case <-ctx.Done():
    logger.Warn("chained-relay: timeout waiting response")
    // reject agent
    return
case resp := <-ch:
    switch resp.Status {
    case protocol.ChainedRelayStatus_Ok, protocol.ChainedRelayStatus_Direct:
        // proceed
    default:
        logger.Warn("chained-relay: server returned non-Ok", "status", resp.Status)
        // reject agent
        return
    }
}

// existing local SetProxy follows
```

The agent rejection path should emit `ProxyEstablishStatus_InternalError` so the agent's `cli.Dial` returns a clean error.

- [ ] **Step 4: Tests**

`runner/agent_proxy_test.go`: extend the existing unit test (or add new) to assert:
- A stub server that returns `Direct` → ceremony proceeds (local SetProxy installed).
- A stub server that returns `Ok` → ceremony proceeds.
- A stub server that returns `HopSetupFailed` → ceremony aborts before local SetProxy.

Build a fake server side via the existing test scaffolding.

- [ ] **Step 5: Run all runner + integration tests**

```sh
go test ./runner/ -count=1
go test -tags integration ./integration/ -run "TestRelayE2E|TestChainedRelayPOC" -count=1
```

Phase C E2E + POC stay green.

**Green criteria:**
- New agent_proxy tests pass.
- `TestRelayE2E` still green (a directly-registered proxy_runner gets `Direct` response, proceeds normally — but wait, does Phase C's existing test scenario actually exercise this code path? The proxy_runner in `TestRelayE2E` doesn't run any agent — agent paths aren't hit. Confirm by reading the test.)
- `TestChainedRelayMissing` (the RED e2e test) **still RED at end of Task 6** — agent_proxy now emits RequestChainedRelay, but server-side handler returns Ok before any actual chained relay wire effect because Task 7's RehandshakeForProxy removal hasn't landed yet… actually wait, let me think.

  Actually, after Task 6 alone, the flow becomes:
  1. Agent dials L (Phase B initial ECDH).
  2. L emits RequestChainedRelay.
  3. Server walks L's chain (L was registered via Phase C; L.Via = P).
  4. Server sends EstablishRelay{slot=agentSlot, target=L.ViaDialAddr} to P.
  5. P does eager SetProxy(owned=(P.serverCID.Addr, agentSlot), allocate=(L.Addr, agentSlot)).
  6. Server replies ChainedRelayResponse{Ok}.
  7. L proceeds with local SetProxy(owned=agentCID, allocate=(L.serverCID.Addr, agentSlot)) — its serverCID is P.Addr.
  8. Agent rehandshakes. Packet flies L → P → server.
  9. Server receives at slot=agentSlot; ECDH with agent's keys. Sends HandshakeAck.
  10. HandshakeAck flies server → P → L → agent.

  So after Task 6, **chained relay should actually work end-to-end for the 2-hop case** (L via P, P direct). The RED test SHOULD turn green at end of Task 6 — UNLESS commit 7 (RehandshakeForProxy removal) is functionally required for chained relay too.

  Re-read spec Decision 1: RehandshakeForProxy removal is about cleaning up Phase C's redundant double-ECDH, not about chained relay. So chained relay should work without dropping it.

  Hmm — let me note: the RED test might turn green at end of Task 6. If so, Task 7 is a pure cleanup and Task 8 just adds the 3-hop positive test. The RED-to-GREEN flip happens at Task 6 implicitly.

  Adjust: Task 6 green criteria includes "RED test now passes" if my reading is correct. Verify by running it.

---

## Task 7: Drop `RehandshakeForProxy` from `HandleWithVia`

**Files:**
- Modify: `server/dial_runner_handler.go`

**Why:** With Task 4's eager SetProxy on proxy_runner, server's first `SendHandshake` at slot_id IS the e2e ECDH with target — no need for an initial ECDH followed by a `RehandshakeForProxy` step. Pure simplification. Spec line 282-283 + Decision 1.

- [ ] **Step 1: Refactor `HandleWithVia` ceremony**

Replace Steps 4-5 (initial ECDH + RehandshakeForProxy) with a single `DoECDHHandshake` at slot_cid:

```go
// New: one ECDH, e2e to target through proxy's eager SetProxy.
slotCID := objproto.NewConnectionID(proxyTransport, proxyAddr, slotID)
endToEndConn, err := objproto.DoECDHHandshake(dialCtx, h.Endpoint, slotCID, ecdh.P521(), objproto.AES128GCM)
if err != nil {
    // ... handle
}
// Step 6 (DialGreeting) and Step 7 (OnDialed) unchanged.
```

- [ ] **Step 2: Update Phase C E2E expectations**

`TestRelayE2E` may have implicit assumptions about the rehandshake step. Check what it asserts — likely just "target registers and `cli.List` works". Behavior-level assertions stay; if the test peeks at internal state (rehandshake transcripts etc.), update.

- [ ] **Step 3: Run all relevant tests**

```sh
go test ./server/ -count=1
go test -tags integration ./integration/ -run "TestRelayE2E|TestRelayE2E_DialModeProxy|TestChainedRelay" -count=1
```

All green.

**Green criteria:**
- `TestRelayE2E` + `TestRelayE2E_DialModeProxy` stay green with the simpler ceremony.
- Server unit tests for the dial_runner_handler updated as needed.
- `RehandshakeForProxy` no longer called from `dial_runner_handler.go` (grep returns no hits in that file).

---

## Task 8: Flip RED E2E + add positive 3-hop E2E

**Files:**
- Modify: `integration/chained_relay_e2e_test.go`
- Create: `integration/chained_relay_3hop_e2e_test.go`

**Why:** The RED test (`TestChainedRelayMissing`) currently pins the missing-feature state by asserting `cli.List` FAILS. After Task 6 (or 7), chained relay actually works, so the test must flip to assert `cli.List` SUCCEEDS. Also add a 3-hop positive test that wasn't covered by the 2-hop RED variant.

- [ ] **Step 1: Flip `TestChainedRelayMissing`**

Rename in spirit (the "Missing" name is now wrong). Either:
- Rename file + test: `chained_relay_e2e_test.go` → keep filename, rename test to `TestChainedRelay_2Hop_E2E`.
- Or: keep filename, rename test, drop the "expected to fail" narrative in comments.

Change the assertion:
```go
// Was: t.Fatalf if cli.List succeeded
// Now:
if listErr != nil {
    t.Fatalf("cli.List should succeed through chained relay: %v", listErr)
}
// optionally verify buf has the expected runner list
```

Update file header doc-comment to describe the now-working scenario.

- [ ] **Step 2: Create 3-hop E2E**

`integration/chained_relay_3hop_e2e_test.go`:

```go
//go:build integration

package integration

func TestChainedRelay_3Hop_E2E(t *testing.T) {
    if testing.Short() { t.Skip("E2E test skipped in -short mode") }

    // Topology:
    //   server (127.0.0.1:18750)
    //   Q (proxy_runner direct mode, dials server outbound, hostname "chained-Q")
    //   P (proxy_runner listen mode at 127.0.0.1:18751, registered via Q, hostname "chained-P")
    //   L (target_runner listen mode at 127.0.0.1:18752, registered via P, hostname "chained-L")
    //   agent process on L host: HARNESS_PROXY_VIA_RUNNER=ws:18752-*, runs cli.List.
    //
    // Expected: cli.List succeeds (Via chain = L → P → Q → server, agent's
    // rehandshake flies through all 3 SetProxy entries).

    // 1. Start server.
    // 2. Start Q via runner.Connect (reverse-dial).
    // 3. Start P via runner.ListenAndServe.
    // 4. cli.ServerDialRunner(target=P_listen_cid, via=Q_cid) → wait P registers.
    // 5. Start L via runner.ListenAndServe.
    // 6. cli.ServerDialRunner(target=L_listen_cid, via=P_registered_cid) → wait L registers.
    // 7. Verify server registry: Q.Via=nil, P.Via=Q, L.Via=P.
    // 8. AddFakeTaskForListenServer(taskID) on L.
    // 9. HARNESS env + cli.Dial → cli.List → expect success.
    // 10. Verify list buf shows Q, P, L (or just that buf is non-empty).
}
```

Use `integration/chained_relay_e2e_test.go` (the 2-hop one) as a structural reference — same wait helpers (`waitForRunnerByHostname`), same `AddFakeTaskForListenServer`.

- [ ] **Step 3: Run all integration tests**

```sh
go test -tags integration ./integration/ -count=1 -v
```

All green.

- [ ] **Step 4: Smoke the full test suite**

```sh
go test ./... -count=1
go test -tags integration ./... -count=1
go vet ./...
go build ./...
```

Clean.

**Green criteria:**
- Flipped 2-hop E2E passes.
- New 3-hop E2E passes.
- All other tests green.
- `go vet` clean.

---

## Task 9: Final review + finishing

**Files:** None (verification + commit cleanup).

- [ ] **Step 1: Read the spec one more time**, checking each section against actual code:
  - Prerequisite (objproto.SetProxy) — Task 1 ✓
  - RunnerEntry fields + plumbing — Task 2 ✓
  - Wire schema — Task 3 ✓
  - Phase C handler eager — Task 4 ✓
  - Server handler — Task 5 ✓
  - Runner ceremony — Task 6 ✓
  - RehandshakeForProxy removal — Task 7 ✓
  - E2E flip + 3-hop — Task 8 ✓

- [ ] **Step 2: Dispatch superpowers:requesting-code-review** for the full branch diff.

- [ ] **Step 3: Address any review feedback**, then merge via PR.

- [ ] **Step 4: Update spec status** with a final note that implementation landed; close the loop with any spec deviations that emerged during implementation.

---

## Open questions (deferred to plan execution)

- **`runner_handler_test.go` test scaffolding for `ChainedRelay` field**: existing tests construct `RunnerHandler` literals; they'll need `ChainedRelay: nil` to compile (or a fake). Decide pattern when Task 5 lands.
- **Phase C E2E expectation update for Task 7**: confirm `TestRelayE2E` doesn't assert intermediate transcript bytes that would change after RehandshakeForProxy removal.
- **`Session.chainedRelayPendingCh` cleanup on session close**: implement so a panic during ceremony doesn't deadlock the session.
- **`OnDialed` signature change ripple in CLI tests**: `cli/server_dial_runner_test.go` may build `DialRunnerHandler` literals; update at Task 2.
