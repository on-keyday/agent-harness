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
agent ──Phase B──> local_runner ──RequestChainedRelay (over L↔server e2e)──> server
                                                                              │
                                                                              │ walk L's chain
                                                                              │ in registry
                                                                              ▼
                                                                            for each hop:
                                                                              EstablishRelay(slot)
                                                                              (existing Phase C wire,
                                                                               sent over server's
                                                                               direct conn to that hop)
                                                                              │
                                                                              ▼
                                                                            each hop sets up
                                                                            proxySettings for slot
                                                                              │
                                                                              ▼
                                                                            server replies Ready to L
local_runner ◄──Ready─────────────────────────────────────────────────────
                                       │
                                       SetProxy(agentCID, (proxy.Addr, slot))
                                       reply Ok to agent

agent ──Handshake (rehandshake)──> local ──fwd──> proxy ──fwd──> server
                                                                  ▲
                                                                  ECDH end-to-end
                                                                  with agent
                                                                  ▼
                                                              peer.Conn ready
```

## Why server-orchestrated (and why not P2P)

A naïve "each hop forwards the setup request to its own upstream" design fails because **Phase C is transparent at the runner level**. After Phase C registration, the registered runner L has an end-to-end ECDH peer.Conn with the SERVER — the intermediate proxy_runner P is a packet forwarder that L cannot address, decrypt for, or send application messages to.

If L tries to send a control message (e.g. "set up forwarding for slot X") over its registered conn, the message reaches the SERVER (encrypted end-to-end), not P. P forwards opaque ciphertext. P has no way to participate in L-initiated setup ceremony.

Therefore the chained-relay setup MUST go through the server:
- Server has its own direct registered conn to P (independent of L's chain).
- Server can send `EstablishRelay` to P over that direct conn.
- Server has authoritative visibility into L's chain (via the registry — L's `RunnerEntry.ID` carries P's addr).

This also subsumes N-hop naturally: server walks the chain by chasing the addr fields in registered runner entries.

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

### Prerequisite: record the via relationship in `RunnerEntry`

Currently `server.RunnerEntry` does NOT track which proxy_runner (if any) a given runner was registered through. Phase C registration completes, the resulting `RunnerEntry.ID` holds a CID whose addr happens to be the proxy_runner's addr, but the relationship is not stored explicitly. Chain reconstruction by addr-matching the registry would be fragile (two unrelated runners could coincidentally share addrs in edge cases).

Spec change: add `Via *RunnerEntry` (or `ViaID string`) to `RunnerEntry`:

```go
// server/registry.go
type RunnerEntry struct {
    // ... existing fields ...

    // Via, when non-nil, is the proxy_runner that this runner was registered
    // through via Phase C (--via). nil for directly-registered runners.
    // Walking Via.Via.Via... terminates at a directly-registered entry
    // whose Via is nil (= that entry is reachable from the server without
    // any proxy hop).
    Via *RunnerEntry
}
```

Populated by `server/dial_runner_handler.go`'s `HandleWithVia` at registration time: when the via path succeeds and the target runner sends its `RunnerHello`, the newly-created RunnerEntry's `Via` field is set to the resolved proxy_runner's entry.

The `Via` field also serves diagnostic / UX purposes (`harness-cli ls` can show "via X" annotation), independent of chained relay.

### Server-side handler

On `RunnerMessage{RequestChainedRelay{slot_id}}` from runner L:

1. Look up L's `RunnerEntry` in the registry (keyed by L's conn CID).
2. Walk `L.Via` (the new field) upward, collecting each intermediate proxy_runner. Stop when an entry's `Via` is nil — that hop is directly registered with the server; the walk ends.
   - If `L.Via == nil` → L is direct, reply `ChainedRelayResponse{Direct}`. No setup needed.
3. Collect the chain: walk `L.Via.Via....` until hitting a nil terminator. Each non-nil entry visited is an intermediate hop that needs an `EstablishRelay` setup. (If L.Via is nil, L is directly registered — no setup needed, reply `Direct`.)

4. For each hop H in the chain, compute `target := H_downstream` — that is, the entry one step closer to L. Concretely: traversing the walk from `L.Via` upward, the immediate downstream of `L.Via` is L itself; the downstream of `L.Via.Via` is `L.Via`; etc.

5. Issue all hops' `EstablishRelayRequest{slot_id, target=H_downstream}` over the server's direct registered conn to each H, **concurrently**. Each hop's SetProxy is independent (synthetic owned + allocate, no precondition between hops), so parallel dispatch is safe and minimizes setup latency.

6. Collect all responses (10s timeout each, applied in parallel).
   - All Ok → reply `ChainedRelayResponse{Ok}` to L over its e2e conn.
   - Any hop returns non-Ok or times out → reply `HopSetupFailed`. See open question 4 for rollback of already-Ok'd hops.

Concrete 2-hop example (chain = L → P → server):
- Walk: L.Via = P, P.Via = nil. Chain = [P].
- Issue: `EstablishRelay{slot, target=L}` to P. P sets up SetProxy(server.Addr↔L.Addr at slot).
- One hop, one EstablishRelay.

Concrete 3-hop example (chain = L → P → Q → server):
- Walk: L.Via = P, P.Via = Q, Q.Via = nil. Chain = [Q, P] (server-to-L order).
- Issue 1: `EstablishRelay{slot, target=P}` to Q. Q sets up SetProxy(owned=(server.Addr, slot), allocate=(P.Addr, slot)).
- Issue 2: `EstablishRelay{slot, target=L}` to P. P sets up SetProxy(owned=(Session.serverCID.Addr, slot), allocate=(L.Addr, slot)).
  - The owned side at P uses `P.Session.serverCID` directly — that's P's own view of its upstream, populated by `driveAfterConn` from the `pc.Connection().ConnectionID()` when P was registered. After Phase C through Q, this resolves to `(Q.Addr-as-seen-from-P, slot)`. The Phase C handler `handleEstablishRelay` already computes owned this way; no chain-specific code needed in the handler.
- Two hops, two EstablishRelays.

The hops can be issued in parallel — each hop's SetProxy uses synthetic owned + allocate and has no dependency on other hops being ready first. Server issues all EstablishRelays concurrently, collects responses, and replies to L once all succeed. Total round-trip from L's view: one `RequestChainedRelay` → server → max(per-hop RT) → response → done.

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

### local_runner-side change (agent_proxy ceremony)

`runner/agent_proxy.go`'s `runAgentProxyCeremony` adds a step BEFORE local SetProxy:

1. Compute `slot_id` = `agentCID.ID`
2. Send `RunnerMessage{RequestChainedRelay{slot_id}}` over `Session.Sender`
3. Wait for `ChainedRelayResponse` (timeout 10s; correlate via per-slot response channel on Session)
4. Branch on status:
   - `Ok` or `Direct` → continue to local SetProxy (current code)
   - any error → reject the agent's ProxyRequest with `ProxyEstablishStatus_InternalError`

### Server `RequestChainedRelay` correlation

Server needs to correlate `RunnerMessage{RequestChainedRelay}` with the runner that sent it. Use the conn's `ConnectionID()` directly — every registered runner has one. No request_id field needed in the wire because there is at-most-one outstanding chain setup per runner conn at a time (per the non-goals constraints).

If a runner sends a second `RequestChainedRelay` before the first completes, server rejects the second with a new status `ChainedRelayStatus_AnotherInFlight`. Reason: the wire schema has no `request_id` field; queueing would require correlation infrastructure. The "agent_proxy ceremony" code path on the runner side is synchronous (one ProxyRequest per ceremony), so concurrent in-flight requests from the same runner are an error condition, not a normal-load case.

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
| `server/registry.go` | Add `Via *RunnerEntry` field. |
| `server/registry_test.go` | Tests covering Via population + walk. |
| `server/dial_runner_handler.go` | Set `Via = resolvedEntry` on the new RunnerEntry constructed for the registered target. Drop `RehandshakeForProxy` step (no longer needed with eager SetProxy on proxy side). |
| `runner/protocol/message.bgn` | Add `RequestChainedRelay` / `ChainedRelayResponse` / `ChainedRelayStatus`. Variants on `RunnerMessage` and `RunnerRequest`. |
| `runner/protocol/message.go` | Regenerated. |
| `runner/relay_handler.go` | Convert `handleEstablishRelay` from lazy (expectedRelays + completeRelaySetup) to eager `SetProxy` (synthetic-owned). Remove `expectedRelays`, `completeRelaySetup`, and the `Session.ExpectedRelays` field entirely — dead code in the new design. |
| `runner/listen.go` | Remove the expectedRelays shortcut in `handleAcceptedConn` (it's now dead; eager SetProxy means the rehandshake packet hits proxySettings directly via objproto.receive). |
| `runner/connect.go` | Add `dispatchRunnerRequest` arm for `ChainedRelayResponse`. |
| `runner/agent_proxy.go` | Add `RequestChainedRelay` send + response wait BEFORE local `SetProxy`. |
| `runner/session.go` | Per-slot response channel for chained-relay correlation. |
| `server/runner_handler.go` | Handle `RunnerMessage{RequestChainedRelay}` — walk the Via chain, send `EstablishRelay` per hop in server-to-target order, reply. |
| `server/server.go` | Wire the new handler. |
| `integration/chained_relay_e2e_test.go` | Flip from "expect failure" to "expect success" — invert the cli.List assertion. |

## Test plan

- objproto unit test: synthetic-owned SetProxy + forward via proxySettings.
- `handleEstablishRelay` revised behavior unit tests (eager SetProxy, no expectedRelays state).
- Server `RequestChainedRelay` handler unit tests:
  - Direct runner (no chain) → replies `Direct`
  - 2-hop chain → walks correctly, sends one EstablishRelay
  - Broken chain (addr doesn't match any runner) → `ChainUnwalkable`
- Phase C existing tests must remain green after eager-SetProxy migration.
- `TestChainedRelayMissing` flips to expect success.
- New positive E2E: 3-hop chain (agent → L → P → Q → server).

## Order of implementation

1. `objproto.SetProxy` synthetic-owned relaxation + unit test (commit 1).
2. `RunnerEntry.Via` field + population in `dial_runner_handler.go` (commit 2). Existing Phase A direct tests + Phase C tests must stay green; new test asserts Via is populated correctly post-Phase-C registration and remains nil for direct registration.
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

## Decisions taken (formerly open questions)

1. **Phase C `RehandshakeForProxy` removal**: drop it as commit 7 of this spec. With eager-SetProxy on the proxy side, server's first `SendHandshake` at slot_id is forwarded raw to target; target ECDHs with server directly. `RehandshakeForProxy` was an artifact of the lazy SetProxy + initial-ECDH dance that no longer applies. If a regression is found in Phase C E2E during the migration, it indicates the eager-SetProxy refactor itself has a bug that must be fixed, not preserved by re-adding RehandshakeForProxy.

2. **Concurrent `RequestChainedRelay`**: server rejects with `ChainedRelayStatus_AnotherInFlight` (decided above in "Server `RequestChainedRelay` correlation"). No request_id field on the wire; in-flight is keyed by runner conn CID.

3. ~~Chain walk authoritativeness~~ — resolved by `RunnerEntry.Via`.

4. **Cleanup on broken chain mid-setup**: leave dangling SetProxy entries to `AutoGarbageCollect`'s idle TTL sweep (default 5min). Reason: explicit per-hop rollback adds complexity (server tracks which hops succeeded, issues `DeleteProxy` to each on failure) for a rare failure case. AutoGC already handles abandoned proxySettings entries safely. If chain setup is reliable in steady-state (target use case), this code path rarely runs; the 5min stale entry is benign because the slot_id was randomly chosen and won't collide in practice.

5. **Target self-match policy on server's `RequestChainedRelay` handler**: this used to be open question 1 in the prior revision; collapsed into the walk-via-Via design — server does NOT do target self-match. The walk terminates at `Via == nil`, which is the source of truth for "directly registered" status, independent of any addr comparison.
