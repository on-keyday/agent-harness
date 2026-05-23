# Chained relay: agent → runner → proxy_runner → server

Date: 2026-05-23
Status: Design
Red test: `integration/chained_relay_e2e_test.go` (commit `6d37d64`)
Prior art: `docs/superpowers/specs/2026-05-22-server-mode-runner-reverse-dial-design.md` (Phase A + B), `docs/superpowers/specs/2026-05-23-server-to-runner-via-relay-design.md` (Phase C `--via`)

## Problem statement

After Phase C `--via` registration, a runner can be registered through one (or more) intermediate proxy_runners. From the registered runner's perspective, Phase C is transparent — its `Session.ServerCID` points to the proxy_runner's network address, and it has no way to know it isn't directly connected to a server.

This becomes a bug the moment that registered runner needs to serve an agent process via Phase B (the standard `harness-cli` proxy path documented in the 2026-05-22 spec). When an agent on the registered runner host runs any `harness-cli` subcommand:

1. The agent dials the local listen-mode runner (`HARNESS_PROXY_VIA_RUNNER` = its addr)
2. The local runner's `runAgentProxyCeremony` (`runner/agent_proxy.go`) calls `SetProxy(agentCID, allocate=(local.serverCID.Transport, local.serverCID.Addr, agentCID.ID))`
3. `local.serverCID.Addr` points to the proxy_runner (because Phase C is transparent)
4. When the agent rehandshakes, the local runner forwards the packet to the proxy_runner at a NEW slot_id (= `agentCID.ID`, not the Phase C registration slot)
5. The proxy_runner has no proxySettings or expectedRelays entry for the new slot — Mutual-mode endpoint accepts the Handshake locally and replies HandshakeAck, then `watchIncomingActiveConns` closes the orphan. The agent gets an end-to-end peer.Conn with the proxy_runner (NOT the real server) and any subsequent application traffic times out at the proxy_runner.

The red test `integration/chained_relay_e2e_test.go::TestChainedRelayMissing` reproduces this end to end.

## Non-goals

- **Dynamic re-routing**: if a proxy_runner mid-chain dies, the chain is broken and the registered runner must re-register. No automatic failover.
- **Multiple parallel proxy paths**: a registered runner has exactly one upstream conn at a time.
- **Optimizing the steady-state cost**: every Phase B ceremony pays a setup round-trip through the server. Caching strategies are out of scope for the MVP.
- **Browser / WASM clients**: the WASM transport is Client-mode only and cannot participate in the chain. The chain applies to native processes (CLI, agent-spawned harness-cli, TUI) only.
- **Server-as-proxy**: the server only orchestrates; it does not itself relay between sibling servers.

## In scope

- Registered runner that is itself reached via one or more Phase C relay hops can host agent processes that use `HARNESS_PROXY_VIA_RUNNER` (= the registered runner) for `harness-cli` calls, and have those calls reach the real server end-to-end.
- 2-hop chain (one proxy_runner between registered runner and server) — primary use case driven by the user's current deployment.
- N-hop chain — server walks its registry to identify each intermediate proxy and sets up Phase C `EstablishRelay` on each. No protocol-level cap; latency stacks per hop.
- Both `EndpointModeMutual` proxy_runners (Phase C-registered listen-mode runners) and dial-mode proxy_runners (originally dial-mode runners that have been chained via further --via in another invocation).

## Topology

### Current (broken, pinned by red test)

```
agent ──Phase B──> local_runner ──forward(NEW slot)──> proxy_runner ──???───> server
                                                       │
                                                       └── no expectedRelays for NEW slot
                                                           → closed; agent times out
```

### Desired (server-orchestrated setup)

```
agent ──Phase B──> L (= local_runner, the listen-mode runner agent dialed)
                   │
                   │ RequestChainedRelay{slot} (over L↔server e2e conn)
                   ▼
                  server
                   │
                   │ walk L.Via.Via... in registry
                   │
                   │ for each intermediate hop, in parallel:
                   │   EstablishRelay (existing Phase C wire,
                   │   sent over server's DIRECT conn to that hop)
                   ▼
                  each hop sets up proxySettings for slot
                  (synthetic owned + allocate, no per-hop ECDH)
                   │
                   ▼
                  server replies ChainedRelayResponse{Ok} to L
L ◄────────────────
   │
   SetProxy(agentCID, allocate=(L.serverCID.Addr, slot))
   reply ProxyEstablishResponse{Ok} to agent
   │
agent ──Handshake (rehandshake)──> L ──fwd──> P (proxy_runner) ──fwd──> server
                                                                          ▲
                                                                          ECDH end-to-end
                                                                          with agent
                                                                          ▼
                                                                      peer.Conn ready
```

## Why server-orchestrated (and why not P2P)

A naïve "each hop forwards the setup request to its own upstream" design fails because **Phase C is transparent at the runner level**. After Phase C registration, the registered runner L has an end-to-end ECDH peer.Conn with the SERVER. The intermediate proxy_runner P is a packet forwarder that lacks the e2e keys; L has no way to address an application message to P specifically.

If L tries to send a control message (e.g. "set up forwarding for slot X") over its registered conn, the message reaches the SERVER (encrypted end-to-end), not P. P just forwards opaque ciphertext. P has no way to participate in L-initiated setup ceremony.

Therefore the chained-relay setup MUST go through the server:
- Server has its own direct registered conn to P (independent of L's chain).
- Server can send `EstablishRelay` to P over that direct conn.
- Server has authoritative visibility into L's chain via the explicit `RunnerEntry.Via` field (added in this spec — see "Prerequisite: record the via relationship" below).

This also subsumes N-hop naturally: server walks `L.Via.Via....` until reaching a directly-registered hop.

## Design

### Prerequisite: `objproto.SetProxy` accepts synthetic `owned`

Current `objproto.SetProxy(owned, allocate)` (`objproto/objproto.go:429`) requires `owned` to exist in `activeConnections`. This is a leftover from the ksdk pattern where the proxy first ECDH's with the initiator, then `SetProxy`s, then closes the activeConn. After Close, the proxySettings entry persists and forwarding still works (verified in Phase C's `completeRelaySetup`).

For chained relay, we need to set up proxySettings BEFORE any handshake arrives at a hop. The bootstrap initial-ECDH at each hop is unnecessary friction. Spec change: remove the `owned must exist` check.

#### Audit: where `activeConnections[owned]` is referenced post-`SetProxy`

Direct verification by `grep -n "activeConnections\[\|proxySettings\[\|getPeer\|peer1\|peer2" objproto/objproto.go` (re-run when re-validating this claim — line numbers may drift):

| Call site | What it does | Affected by synthetic `owned`? |
|---|---|---|
| `SetProxy` precondition `activeConnections[owned]` (line 432) | Rejects if owned is not an existing activeConn. | **YES — this is the check we are removing.** |
| `SetProxy` precondition `activeConnections[allocate]` (line 435) | Rejects if allocate IS an existing activeConn. | No — keep this check, prevents ambiguous routing when an inbound activeConn would compete with the synthetic SetProxy entry. |
| `receive()` proxy forward (lines 1018-1039) | Looks up `proxySettings[cid]` FIRST. On hit, calls `sendPacket(peer, kind, data)` and `return nil` — never reaches activeConn lookup. | No — proxied packets short-circuit before any activeConn reference. |
| `sendPacket` (line 533) | Enqueues a `PacketData` on `pktQueue`. No activeConn use. | No. |
| `mayCloseProxy` (lines 486-496) → `closeCannotSend` (lines 498-) | Tries `proxySettings[pkt.To]` first, only falls through to `activeConnections[pkt.To]` cleanup if no proxy entry. | No — for proxied paths, the proxy branch handles cleanup before activeConn cleanup runs. |
| `receiveApplication` (lines 943-) | Looks up `activeConnections[cid]` to decrypt application data. | No — only runs when `receive()` falls through to non-proxy processing (i.e. cid is NOT in proxySettings). For SetProxy'd cids this is unreachable. |
| `receiveHandshake` and `receiveHandshakeAck` (collision checks at lines 645, 868) | `activeConnections[cid]` existing-entry check before creating a new activeConn. | No — same as receiveApplication; only runs on non-proxy paths. For proxied cids, receive() short-circuits to forwarding before reaching these. |
| `closeConnection(a *activeConnection)` (lines 475-484) | Closes an activeConn by reference. | No — it operates on an existing `*activeConnection` value; if no activeConn was ever created at owned (synthetic case), no one holds a reference to close. |
| `GetConnection(cid)` (line 563) | Public lookup. | No — callers don't call this on synthetic owned cids (they have no `*Connection` to reach for). |
| `sendRehandshakeForProxy(a, ...)` (lines 627-636) | Requires `activeConnections[a.cid]` to exist and match `a`. | No — this is called by the INITIATOR (the dialer doing RehandshakeForProxy), operating on its OWN endpoint's activeConn. The proxy_runner's owned does not enter this code path. Phase C's existing `server.dial_runner_handler.go` RehandshakeForProxy step runs against the server's own activeConn, unrelated to the proxy's owned. |
| `deleteActiveConnection` / `AutoGarbageCollect` (proxy TTL at line 695) | Iterates `proxySettings`, not `activeConnections`, for proxy expiry. | No — proxy lifecycle is independent of owned activeConn lifecycle. |

Conclusion: every `activeConnections[owned]` reference post-`SetProxy` is in a code path that is either (a) the precondition check we're removing, (b) reserved for non-proxy traffic and unreachable for SetProxy'd cids, or (c) on the dialer's endpoint (not the proxy's).

#### Safety of removing the check

- The activeConn at `owned` is referenced only by the precondition check itself. After Phase C's `completeRelaySetup` Closes it, the activeConn is gone — yet forwarding via `proxySettings` continues to work. This is empirically proven by Phase C E2E tests (`TestRelayE2E`, `TestRelayE2E_DialModeProxy`) green on main.
- `allocate must NOT exist` check is preserved — prevents ambiguous routing.
- No new attack surface: `SetProxy` is called only by runner code (no public client API).

Phase C's existing handler keeps working with the relaxed contract: it happens to call `SetProxy` after the server's initial ECDH has already created owned, satisfying the precondition as a special case. The relaxation makes the strict precondition optional, not required.

### Prerequisite: record the via relationship + via-dial addr in `RunnerEntry`

Currently `server.RunnerEntry` does NOT track:
1. Which proxy_runner (if any) a given runner was registered through.
2. The address server passed to the upstream proxy as `EstablishRelay.target` (the addr the proxy actually dialed) when registering this runner via Phase C.

Both pieces of information are erased after registration completes. Chained relay setup requires both:

**(1)** is needed to walk the chain (`L.Via.Via.Via...` to find the intermediate proxy hops).

**(2)** is needed because the addr server passes as `EstablishRelay.target` to each hop must be **the addr that hop knows the downstream by** — not server's-view-ephemeral. Due to TCP's directional asymmetry and the Phase C ceremony, the server-visible CID encodes the upstream proxy's ephemeral, not the downstream's listen addr:
- Server view of L (= `L.ID`): the CID server registered L under = `(proxy.Addr, slotID)`, because Phase C reuses the slot CID across both initial ECDH and `RehandshakeForProxy` (see `objproto.sendRehandshakeForProxy` reusing `a.cid`). L's LISTEN_ADDR does not appear in `L.ID`.
- P's view of L: `(L.LISTEN_ADDR, slot)` — the addr P originally dialed when forwarding server's rehandshake to L during L's Phase C registration.

`P` was given L's LISTEN_ADDR via the original `EstablishRelay.target` when L was registered, but server then forgot the addr after registration completed. For chained relay setup, server must replay this addr.

Note: **Phase A direct registration does NOT have this gap** — `objproto.DoECDHHandshake(cid=target_CID)` returns a conn whose `ConnectionID()` equals `target_CID`, so `entry.ID` already encodes the dialed addr. The new field is required only for Phase C registrations where the conn's CID is keyed by the upstream proxy's ephemeral.

Spec change: add two fields to `RunnerEntry`:

```go
// server/registry.go
type RunnerEntry struct {
    // ... existing fields ...

    // Via, when non-nil, is the proxy_runner that this runner was registered
    // through via Phase C (--via). nil for directly-registered runners and
    // for reverse-dial runners (those that called runner.Connect themselves).
    // Walking Via.Via.Via... terminates at an entry whose Via is nil (= a
    // hop reachable from server without any proxy).
    Via *RunnerEntry

    // ViaDialAddr is the addr server passed to Via as EstablishRelay.target
    // when registering this runner via Phase C — equivalently, the addr Via
    // actually dialed to forward server's rehandshake to this runner during
    // Phase C registration. For chained relay, this is the addr each upstream
    // hop uses for its SetProxy.allocate when forwarding traffic to this
    // runner.
    //
    // Populated together with Via (both non-zero iff this runner was
    // registered via Phase C). Zero-valued for:
    //   - Phase A direct registrations (server dialed directly; the dialed
    //     addr is already encoded in entry.ID).
    //   - Reverse-dial registrations (runner.Connect path; server did not
    //     dial — concept does not apply).
    //
    // Such zero-ViaDialAddr runners cannot serve as a middle hop on a
    // chained-relay path because no upstream needs them as a forwarding
    // target; they can only sit at the chain's terminus (= directly reachable
    // from server / their own upstream).
    //
    // Stored as objproto.ConnectionID — the result of
    // protocol.RunnerIDToConnID(target). Only Transport + Addr are
    // load-bearing; the ID portion happens to carry the admin-supplied
    // UniqueNumber but is not used by any consumer (chained-relay dispatch
    // reads only Transport + Addr to construct EstablishRelayRequest.Target,
    // then fills the request's separate SlotId field from the agent-chosen
    // chained-relay slot).
    ViaDialAddr objproto.ConnectionID
}
```

Populated by `server/dial_runner_handler.go`'s `HandleWithVia` (Phase C path only): `ViaDialAddr = protocol.RunnerIDToConnID(target)` — stored as-is, no further normalization. The value is plumbed to runner_handler.go (Hello handler, where the RunnerEntry is built) via the `OnDialed` callback signature — see "Plumbing from HandleWithVia to RunnerEntry" below. `Handle` (Phase A direct) does not need to plumb anything: `entry.ID` already encodes the dialed addr.

The `Via` field also serves diagnostic / UX purposes (`harness-cli ls` can show "via X" annotation), independent of chained relay.

### Plumbing from `HandleWithVia` to `RunnerEntry`

`RunnerEntry` is constructed in `runner_handler.go` when the Hello message arrives, NOT in `dial_runner_handler.go`. By Hello-receive time, `HandleWithVia`'s stack frame is gone and `target` is out of scope. To bridge the gap:

1. Extend `DialRunnerHandler.OnDialed` signature from `func(ctx, conn)` to `func(ctx, conn, viaInfo *ViaRegistrationInfo)`, where `ViaRegistrationInfo` carries `{Via *RunnerEntry, ViaDialAddr objproto.ConnectionID}`. `Handle` (Phase A) passes nil; `HandleWithVia` (Phase C) passes the populated struct with `ViaDialAddr = protocol.RunnerIDToConnID(target)`.
2. `server.handleConnection` accepts the extra parameter and stashes it on a per-conn map (keyed by `conn.ConnectionID()`) before dispatching to the RunnerHandler.
3. `RunnerHandler.Handle` (Hello case) consults the map for the conn's CID; if present, populates `entry.Via` + `entry.ViaDialAddr`; if absent, leaves both zero. The map entry is removed after Hello processing (or on conn close, whichever first).

The map is the simplest plumbing — no API ripple beyond the two files. Alternative (no map, fully synchronous) requires threading viaInfo through `handleConnection → Hello dispatch`, which crosses an existing interface boundary and is messier.

### Wire schema additions

`runner/protocol/message.bgn`:

```bgn
# runner → server: "I need an end-to-end sub-conn for slot_id reaching you.
# Walk my chain and set up Phase C EstablishRelay on each intermediate hop,
# then tell me you're done."
#
# Sent by `runAgentProxyCeremony` BEFORE the local `SetProxy`, over the
# existing registered (e2e) conn. Server responds via RunnerRequest.
format RequestChainedRelay:
    slot_id :u16

enum ChainedRelayStatus:
    :u8
    ok                  = "ok"                    # all hops set up, runner may proceed
    direct              = "direct"                # runner is registered direct (no chain), no setup needed
    slot_collision      = "slot_collision"        # collision on some hop's slot_id
    hop_setup_failed    = "hop_setup_failed"      # an intermediate EstablishRelay was rejected
    chain_unwalkable    = "chain_unwalkable"      # server couldn't trace the chain (bug condition)
    another_in_flight   = "another_in_flight"     # a previous RequestChainedRelay from this runner is still being processed

format ChainedRelayResponse:
    status :ChainedRelayStatus
```

Wire kind tagging: `RequestChainedRelay` is added as a new variant in `RunnerMessage` (runner → server direction); `ChainedRelayResponse` is added in `RunnerRequest` (server → runner direction).

### Server-side handler

On `RunnerMessage{RequestChainedRelay{slot_id}}` from runner L:

1. Look up L's `RunnerEntry` in the registry (keyed by L's conn CID).
2. If `L.Via == nil` → L is directly registered; reply `ChainedRelayResponse{Direct}` and stop. No chain setup needed; L's local SetProxy already points at server's actual addr.
3. Otherwise walk `L.Via.Via....` until hitting a `Via == nil` terminator. Each non-nil entry on the walk is an intermediate proxy_runner that needs an `EstablishRelay`. (Loop detection: if the walk visits the same entry twice — bug condition; abort with `ChainUnwalkable`.)
4. For each hop `H` on the walk, compute `target := H_downstream` — the entry one step closer to L. Concretely: `L.Via`'s downstream is L itself; `L.Via.Via`'s downstream is `L.Via`; etc.
5. Issue every hop's `EstablishRelayRequest{SlotId: slot_id, Target: protocol.ConnIDToRunnerID(H_downstream.ViaDialAddr)}` to its respective H. **The `target` addr is the downstream's `ViaDialAddr`, NOT its `entry.ID`** — because `H` knows downstream by the addr it (or its upstream) originally dialed, not by server-side CID ephemeral. Note `target.UniqueNumber` portion is unused by `handleEstablishRelay` (it reads `request.SlotId` + `target.Transport` + `target.Addr` only); ships through for wire-format completeness. Dispatch to all hops **concurrently** over server's existing registered conns; SetProxy at each hop is independent (synthetic owned + allocate, no precondition between hops), so parallel dispatch is safe and minimizes setup latency.

   For the top hop (the one whose `Via == nil`, directly registered), server sends EstablishRelay over its direct conn to that hop. For deeper hops (registered via further-up proxies), server sends over the existing relayed conn (the e2e conn established during that hop's Phase C registration, forwarded by its own upstream proxy at the registration slot — opaque ciphertext through the proxy, decrypted at the destination hop).
6. Collect all responses (per-hop 10s timeout, all in flight in parallel).
   - All Ok → reply `ChainedRelayResponse{Ok}` to L over L's e2e conn.
   - Any hop returns non-Ok or times out → reply `ChainedRelayResponse{HopSetupFailed}`. Already-Ok'd hops' SetProxy entries are NOT actively rolled back (see Decision 4).

Concrete 2-hop example (chain = L → P → server):
- Walk: L.Via = P, P.Via = nil. Chain = [P].
- Issue: `EstablishRelay{slot, target=L.ViaDialAddr}` to P (over server↔P direct conn). P sets up SetProxy(owned=(P.Session.serverCID.Addr, slot), allocate=(L.ViaDialAddr, slot)).
  - L.ViaDialAddr is the address server passed when registering L through P — = L's LISTEN_ADDR. P first dialed this addr to forward server's rehandshake during L's registration, so P.transport.connMap already has an outbound WS conn to L.ViaDialAddr (= the existing P↔L conn).
- One hop, one EstablishRelay.

Concrete 3-hop example (chain = L → P → Q → server):
- Walk: L.Via = P, P.Via = Q, Q.Via = nil. Two intermediate hops: P and Q.
- `EstablishRelay{slot, target=P.ViaDialAddr}` to Q (over server↔Q direct conn, dispatched in parallel with the other below). Q sets up SetProxy(owned=(server.LISTEN_ADDR-from-Q, slot), allocate=(P.ViaDialAddr, slot)).
- `EstablishRelay{slot, target=L.ViaDialAddr}` to P (over server↔P virtual conn = forwarded by Q at P's registration slot). P sets up SetProxy(owned=(P.Session.serverCID.Addr, slot), allocate=(L.ViaDialAddr, slot)).
  - The owned side at P uses `P.Session.serverCID` directly — that's P's own view of its upstream, populated by `driveAfterConn` from `pc.Connection().ConnectionID()` when P was registered. After Phase C through Q, this resolves to `(Q.LISTEN_ADDR-from-P-view, slot)`. The Phase C handler `handleEstablishRelay` already computes owned this way; no chain-specific code needed in the handler.

Total round-trip from L's view: one `RequestChainedRelay` → server → max(per-hop RT for parallel EstablishRelays) → response → done.

### Phase C handler revision (proxy_runner side)

`runner/relay_handler.go`'s `handleEstablishRelay` currently does lazy SetProxy (only sets `expectedRelays`; the actual `SetProxy` runs from `completeRelaySetup` when the matching activeConn arrives). For the chained-relay use case the matching activeConn never arrives (the agent's rehandshake arrives directly at the relay-target slot without a separate initial ECDH).

With the synthetic-owned `SetProxy` change (Prerequisite above), the handler can be simplified to do **eager** SetProxy:

```go
// New behavior of handleEstablishRelay (replacing the expectedRelays-based path):
//
// 1. Validate slot_id (collision check) → return non-Ok if invalid.
// 2. allocCID = NewConnectionID(target.Transport, target.Addr, slot_id)   // synthetic
// 3. ownedCID = NewConnectionID(s.serverCID.Transport, s.serverCID.Addr, slot_id)  // synthetic
// 4. ep.SetProxy(ownedCID, allocCID)
// 5. Reply EstablishRelayResponse{Ok}
```

`expectedRelays` and `completeRelaySetup` become unused after the refactor. **Remove them entirely** in commit 4 — dead code rots, and the new path doesn't need either. The `listen.go` shortcut that consults `expectedRelays` (added for Phase C) is also removed; with eager SetProxy, the rehandshake packet hits `proxySettings` in `objproto.receive` and is forwarded without ever creating a local activeConn.

The existing direct Phase C flow (server-initiated dial-runner --via) still works with eager SetProxy because:
- Server's `SendHandshake` at slot would now hit the proxySettings entry created eagerly at EstablishRelay time
- Packet forwarded raw to target (= same downstream effect)
- Server's `RehandshakeForProxy` no longer needed because there is no initial-ECDH to rehandshake — the first handshake from server is already forwarded

This is a behavior change for Phase C: server-side `HandleWithVia` in `server/dial_runner_handler.go` can drop the `RehandshakeForProxy` step. Server's first `SendHandshake` at slot_id IS the agent-side ECDH-with-target. Simpler, fewer round trips.

### Registration-time chain walk in `HandleWithVia`

For an N-hop registration topology (admin runs `dial-runner L --via P` where `P` itself was registered via `dial-runner P --via Q`), `HandleWithVia`'s single `EstablishRelay` to the direct via runner is insufficient. The registration `SendHandshake` to `(P.Addr, slotID)` is forwarded by `Q`'s proxySettings entry only if `Q` already has an entry for `slotID` → `(P.Addr, slotID)`; without it, `Q` accepts the handshake locally and `L` never registers. The 3-hop e2e test (`TestChainedRelay_3Hop_E2E`) exposes this.

`HandleWithVia` therefore walks `via_entry.Via.Via...` (loop detection applies) and dispatches `EstablishRelay{slotID, target=H_downstream.ViaDialAddr}` to every upstream hop in parallel BEFORE issuing the registration `SendHandshake`. This is structurally the same walk that `ChainedRelayHandler.Handle` performs at runtime for agent-initiated relay requests — both should call a shared helper (see "Files touched"). Concretely for 3-hop registration of L via P-via-Q at slot S:

1. EstablishRelay{S, target=L.LISTEN_ADDR} → P (over server↔P virtual conn via Q)
2. EstablishRelay{S, target=P.ViaDialAddr} → Q (over server↔Q direct conn) — added by Step 3b walk
3. SendHandshake to (P.Addr, S): packet → Q's eager SetProxy for slot S → P's eager SetProxy for slot S → L. ECDH e2e with L.

This is the REGISTRATION path; the RUNTIME path (agent on L initiating a relay request) is handled separately by `ChainedRelayHandler`. The two paths use the same chain-walk algorithm but are triggered by different events (admin CLI vs runner-initiated `RequestChainedRelay`).

### local_runner-side change (agent_proxy ceremony)

`runner/agent_proxy.go`'s `runAgentProxyCeremony` adds a step BEFORE local SetProxy:

1. Compute `slot_id` = `agentCID.ID`
2. Send `RunnerMessage{RequestChainedRelay{slot_id}}` over `Session.Sender`
3. Wait for `ChainedRelayResponse` (timeout 10s; correlate via per-slot response channel on Session)
4. Branch on status:
   - `Ok` or `Direct` → continue to local SetProxy (current code)
   - any error → reject the agent's ProxyRequest with `ProxyEstablishStatus_InternalError`

### Server `RequestChainedRelay` correlation and concurrency

Server correlates `RunnerMessage{RequestChainedRelay}` to the sending runner via the conn's `ConnectionID()` (every registered runner has a unique one). No `request_id` field on the wire — the agent_proxy ceremony on the runner side is synchronous (one ProxyRequest per ceremony, one chained-relay setup per ProxyRequest), so at-most-one in-flight `RequestChainedRelay` per runner conn is the natural property of the calling pattern.

Defensive guard: if a runner does somehow send a second `RequestChainedRelay` while the first is still being processed (bug / race), server rejects the second with `ChainedRelayStatus_AnotherInFlight` and leaves the first unaffected. Server tracks "in flight per conn" with a simple map keyed by conn CID, similar to the existing `relayRespCh` map for Phase C EstablishRelay correlation.

### Ceremony (full 2-hop case)

```
agent on runner_L      runner_L            proxy_runner_P       server
(HARNESS_TASK_ID set)  (listen mode,       (dial mode,
(HARNESS_PROXY_VIA=     registered via      directly
 local L addr)          Phase C through P)  registered)

[Phase A direct dial registration of P, then Phase C registration of L
 through P, both completed earlier. L↔server is e2e via P forwarding;
 server↔P is direct.]

agent invokes harness-cli ls:
  agent.cli.DialPeerConn → DialViaProxy(L, taskID)
  ──Dial L (initial ECDH)─────►
  ──ProxyRequest{taskID}──────►
                                L.runAgentProxyCeremony:
                                  validate task_id → Ok
                                  (NEW) emit chained-relay request
                                  ──RequestChainedRelay{slot=agentCID.ID}──────────────────────────────►
                                                                                                          server:
                                                                                                            walk L's chain
                                                                                                            from registry
                                                                                                            → L is via P
                                                                                                            (P is direct)
                                                                                                            for each intermediate (just P):
                                                                                                            ──EstablishRelay{slot,target=L}──►
                                                                                                                                                P.handleEstablishRelay:
                                                                                                                                                  validate slot → Ok
                                                                                                                                                  SetProxy(
                                                                                                                                                    owned=(server.Addr, slot)  ← synthetic
                                                                                                                                                    allocate=(L.Addr, slot)    ← synthetic
                                                                                                                                                  )
                                                                                                                                                ◄─Response{Ok}─
                                                                                                            all hops set
                                  ◄─ChainedRelayResponse{Ok}────────────────────────────────────────────
                                  SetProxy(
                                    owned=(agent.Addr, slot)
                                    allocate=(P.Addr, slot)  ← synthetic (= L's view of "server" = P)
                                  )
                                  ProxyEstablishResponse{Ok}
  ◄─Response{Ok}──────────────
  ──RehandshakeForProxy──►
                                L's proxySettings hit
                                → forward raw to P
                                ─────────────────────────────►
                                                                P's proxySettings hit
                                                                → forward raw to server
                                                                ─────────────────────────────────────►
                                                                                                       server.receive:
                                                                                                         Handshake at slot
                                                                                                         → ECDH with agent
                                                                                                       ◄─HandshakeAck (back through
                                                                                                         P's then L's SetProxy)
  ◄─end-to-end peer.Conn ready (agent ↔ server, opaque through L and P)

PSK + RunnerHello flow normally between agent and server over the relayed conn.
```

### N-hop case

Server's chain walk handles N hops by tracing addresses through the registry. Each intermediate hop receives a `EstablishRelay` and sets up SetProxy. The agent's rehandshake flows through all hops sequentially.

Loop detection: server-side. If the walk visits the same hop twice, abort with `ChainUnwalkable`. (Bug-condition guard; should not happen with sane registration.)

## Files touched

| File | Change |
|---|---|
| `objproto/objproto.go` | Remove `owned must exist in activeConnections` precondition in `SetProxy`. |
| `objproto/objproto_test.go` | Unit test: synthetic-owned SetProxy + receive() forwards via proxySettings without prior ECDH. |
| `server/registry.go` | Add `Via *RunnerEntry` and `ViaDialAddr objproto.ConnectionID` fields. |
| `server/registry_test.go` | Tests covering Via population + walk, ViaDialAddr population for Phase C registrations. Phase A direct + reverse-dial: both leave Via and ViaDialAddr zero. |
| `server/dial_runner_handler.go` | (a) Source `slotID` from `target.UniqueNumber` instead of `rand.Uint32()` (precursor cleanup — kills the orphan target.UniqueNumber field; admin's original `*` wildcard still random-generates client-side via ParseConnectionID). (b) Extend `OnDialed` signature to carry `ViaRegistrationInfo`. (c) `HandleWithVia` (Phase C) constructs and passes `ViaRegistrationInfo{Via: resolvedEntry, ViaDialAddr: protocol.RunnerIDToConnID(target)}`. `Handle` (Phase A direct) passes nil. (d) Drop `RehandshakeForProxy` step (no longer needed with eager SetProxy on proxy side). |
| `server/server.go` | (a) `handleConnection` accepts an extra `ViaRegistrationInfo` parameter, stashes it in a per-conn-CID map before dispatching to RunnerHandler (entry removed after Hello processing or on conn close). (b) Wire the `RequestChainedRelay` handler. |
| `server/runner_handler.go` | (a) Hello case consults the per-conn-CID map; populates `entry.Via` + `entry.ViaDialAddr` from the stashed info if present. (b) Handle `RunnerMessage{RequestChainedRelay}` — walk the Via chain, send `EstablishRelay` per hop in server-to-target order, reply. |
| `runner/protocol/message.bgn` | Add `RequestChainedRelay` / `ChainedRelayResponse` / `ChainedRelayStatus`. Variants on `RunnerMessage` and `RunnerRequest`. |
| `runner/protocol/message.go` | Regenerated. |
| `runner/relay_handler.go` | Convert `handleEstablishRelay` from lazy (expectedRelays + completeRelaySetup) to eager `SetProxy` (synthetic-owned). Remove `expectedRelays`, `completeRelaySetup`, and the `Session.ExpectedRelays` field entirely — dead code in the new design. |
| `runner/listen.go` | Remove the `sess.ExpectedRelays.Take(slotID)` short-circuit in `handleAcceptedConn` (the field is gone after the relay_handler refactor; rehandshake packets now hit `proxySettings` directly inside `objproto.receive` before any listen-side dispatch runs). |
| `runner/connect.go` | Add `dispatchRunnerRequest` arm for `ChainedRelayResponse`. |
| `runner/agent_proxy.go` | Add `RequestChainedRelay` send + response wait BEFORE local `SetProxy`. |
| `runner/session.go` | Per-slot response channel for chained-relay correlation. |
| `integration/chained_relay_e2e_test.go` | Flip from "expect failure" to "expect success" — invert the cli.List assertion. |

## Test plan

- objproto unit test: synthetic-owned `SetProxy` + `receive()` forwards via proxySettings without prior ECDH.
- `handleEstablishRelay` revised behavior unit tests (eager SetProxy, no `expectedRelays` state).
- Server `RequestChainedRelay` handler unit tests:
  - Direct runner (no chain) → replies `Direct`
  - 2-hop chain → walks correctly, sends one EstablishRelay
  - Broken `Via` loop → `ChainUnwalkable`
- Phase C E2E (`TestRelayE2E`, `TestRelayE2E_DialModeProxy`):
  - Commits 1-6: must remain green AS-IS (no test changes; the eager-SetProxy refactor preserves the existing flow because server's initial ECDH + RehandshakeForProxy still does the right thing on top of eager-SetProxy).
  - Commit 7: tests UPDATED to expect the simpler flow (no RehandshakeForProxy step on server side). Existing assertions about end-to-end e2e conn establishment still hold; only the intermediate sequence changes.
- `TestChainedRelayMissing` (commit 8): inverted to expect success (cli.List actually returns).
- New positive E2E (commit 8): 3-hop chain (agent → L → P → Q → server), verifies `cli.List` succeeds + Via chain visible in server registry.

## Order of implementation

0. **Precursor cleanup (commit 0)**: `server/dial_runner_handler.go` `HandleWithVia` sources `slotID` from `target.UniqueNumber` instead of `rand.Uint32()`. Existing Phase C E2E tests stay green (they admin-supply `*` wildcard, ParseConnectionID still generates a random UniqueNumber client-side, so functional behavior is identical). The change just removes a second randomness source on the server side and stops orphaning `target.UniqueNumber`. This commit is independent of chained relay and could ship standalone.
1. `objproto.SetProxy` synthetic-owned relaxation + unit test (commit 1).
2. `RunnerEntry.Via` and `RunnerEntry.ViaDialAddr` fields + Phase C plumbing (commit 2). Adds:
   - `ViaRegistrationInfo` struct and the `OnDialed` signature extension.
   - `HandleWithVia` (Phase C) constructs `ViaRegistrationInfo{Via: resolvedEntry, ViaDialAddr: protocol.RunnerIDToConnID(target)}` and passes through `OnDialed`.
   - `server.handleConnection` stashes the info on a per-conn-CID map.
   - `runner_handler.go` Hello case consults the map and populates `entry.Via` + `entry.ViaDialAddr`.

   `Handle` (Phase A direct) passes nil — leaving `Via` and `ViaDialAddr` zero. Reverse-dial path (`runner.Connect`) is unaffected — server's inbound accept path never went through `OnDialed`, so no plumbing changes there. Existing Phase A direct + Phase C tests must stay green; new tests assert Via + ViaDialAddr are populated correctly post-Phase-C registration and both remain zero for direct + reverse-dial registrations.
3. Schema additions for `RequestChainedRelay` / response (commit 3).
4. Phase C handler refactor: `handleEstablishRelay` eager SetProxy (commit 4). Existing Phase C E2E must stay green.
5. Server `RequestChainedRelay` handler + chain walk over `Via` (commit 5).
6. `runAgentProxyCeremony` request step (commit 6).
7. Drop `RehandshakeForProxy` from `server/dial_runner_handler.go` (commit 7). Phase C tests adjusted to expect the simpler flow.
8. Flip red E2E + add 3-hop test (commit 8).

The red test (`TestChainedRelayMissing`) stays red until commit 6 lands. Commit 7 is part of this spec's scope — Phase C's `RehandshakeForProxy` is redundant once proxy-side SetProxy is eager (server's first `SendHandshake` IS the end-to-end ECDH, forwarded raw to target). No deferral; ship together.

## Trust model

- Chained-relay setup requests travel runner → server over the PSK-validated, end-to-end-ECDH-encrypted registered conn. Server is the only entity that orchestrates relay setup; intermediate hops only receive Phase C `EstablishRelay` from server over their direct registered conns (same trust path as existing Phase C).
- End-to-end ECDH between agent and server survives the chain — `objproto.SetProxy` at each hop is opaque packet relay (no decrypt).
- PSK exchange happens agent ↔ server end-to-end after the rehandshake; hops cannot validate or forge.

## Decisions taken

1. **Phase C `RehandshakeForProxy` removal**: drop it as commit 7 of this spec. With eager-SetProxy on the proxy side, server's first `SendHandshake` at slot_id is forwarded raw to target; target ECDHs with server directly. `RehandshakeForProxy` was an artifact of the lazy SetProxy + initial-ECDH dance that no longer applies. If a regression is found in Phase C E2E during the migration, it indicates a bug in the eager-SetProxy refactor itself that must be fixed, not papered over by re-adding RehandshakeForProxy.

2. **Concurrent `RequestChainedRelay`**: server rejects with `ChainedRelayStatus_AnotherInFlight` (see "Server `RequestChainedRelay` correlation and concurrency" above). No `request_id` field on the wire; in-flight is keyed by runner conn CID.

3. **Cleanup on broken chain mid-setup**: leave dangling SetProxy entries to `AutoGarbageCollect`'s idle TTL sweep (default 5min). Reason: explicit per-hop rollback adds complexity (server tracks which hops succeeded, issues `DeleteProxy` to each on failure) for a rare failure case. AutoGC already handles abandoned `proxySettings` entries safely. The dangling entry is benign because the slot_id was randomly chosen and won't collide with future setups in practice.

4. **Chain-end detection**: server identifies a directly-registered hop by `RunnerEntry.Via == nil`. No addr-comparison against server's listen addr or anything similar — the explicit `Via` field is the source of truth.
