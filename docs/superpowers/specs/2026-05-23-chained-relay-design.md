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

Current `objproto.SetProxy(owned, allocate)` requires `owned` to exist in `activeConnections`. This is a leftover from the ksdk pattern where the proxy first ECDH's with the initiator, then `SetProxy`s, then closes the activeConn. After Close, the proxySettings entry persists and forwarding still works (verified in Phase C's `completeRelaySetup`).

For chained relay, we need to set up proxySettings BEFORE any handshake arrives at a hop. The bootstrap initial-ECDH at each hop is unnecessary friction. Spec change: remove the `owned must exist` check.

Safety:
- The activeConn at `owned` was never used post-`SetProxy` (Phase C's `completeRelaySetup` Closes it).
- `allocate must NOT exist` check preserved — prevents ambiguous routing.
- No new attack surface: `SetProxy` is called only by runner code.

Phase C's existing handler keeps working: it happens to call `SetProxy` after the server's initial ECDH creates owned, so it satisfies the relaxed contract as a special case.

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
    ok                = "ok"                  # all hops set up, runner may proceed
    direct            = "direct"              # runner is registered direct (no chain), no setup needed
    slot_collision    = "slot_collision"      # collision on some hop's slot_id
    hop_setup_failed  = "hop_setup_failed"    # an intermediate EstablishRelay was rejected
    chain_unwalkable  = "chain_unwalkable"    # server couldn't trace the chain (bug condition)

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
3. For each intermediate hop H (= every entry along the walk INCLUDING L's immediate upstream, but EXCLUDING the directly-registered terminus which IS the server's direct conn):
   - Wait — clarification: every entry on `L.Via.Via....` chain that is non-nil up to and including the last non-nil. The hop just before the nil-terminator is the one with a direct conn to server; THAT hop is the one that needs Phase C EstablishRelay set up first (because it's the first non-trivial relay layer adjacent to server).
   - Walk order: server's "natural" issue order is server-adjacent hop FIRST, then working downward toward L. Each hop's EstablishRelay tells the hop "forward slot_id to your downstream" — downstream is the next entry in the walk going toward L.
4. For each hop H in server-to-L order:
   a. `target := H_downstream` (= the entry whose `Via == H`, or L itself for the bottom-most relay)
   b. Send `EstablishRelayRequest{slot_id, target=target.ID-as-RunnerID}` over server's direct registered conn to H (`server.sendEstablishRelayRequest` from Phase C, reused verbatim).
   c. Wait for response (10s).
5. If all hops reply Ok, reply `ChainedRelayResponse{Ok}` to L over its e2e conn.
6. On any hop error: reply with the appropriate status (`HopSetupFailed`). Note open question 4 about rollback of already-Ok'd hops.

Concrete 2-hop example (chain = L → P → server):
- Walk: L.Via = P, P.Via = nil. Chain = [P].
- Issue: `EstablishRelay{slot, target=L}` to P. P sets up SetProxy(server.Addr↔L.Addr at slot).
- One hop, one EstablishRelay.

Concrete 3-hop example (chain = L → P → Q → server):
- Walk: L.Via = P, P.Via = Q, Q.Via = nil. Chain = [Q, P] (server-to-L order).
- Issue 1: `EstablishRelay{slot, target=P}` to Q. Q sets up SetProxy(server.Addr↔P.Addr at slot).
- Issue 2: `EstablishRelay{slot, target=L}` to P. P sets up SetProxy(Q.Addr↔L.Addr at slot).
   - Wait — `Q.Addr` from P's view? P's view of Q is P.serverCID.Addr — which equals Q's addr in the server's-view-of-P registry. The `target` field in EstablishRelay names the DOWNSTREAM peer for SetProxy. For P, downstream is L, upstream is Q. P's SetProxy is (upstream side, downstream side) — owned=(Q.Addr-as-from-P, slot), allocate=(L.Addr-as-from-P, slot). The Phase C handler already handles this: it computes owned = (Session.serverCID.Transport, Session.serverCID.Addr, slot_id), i.e. P's own view of its upstream (= Q), which is correct.
- Two hops, two EstablishRelays, sent in server-to-L order so each upstream is "ready" before its downstream tries to send through.

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

`expectedRelays` and `completeRelaySetup` become unnecessary for this path and can be removed (or kept as dead code temporarily — implementer's choice for one-commit-per-step ordering).

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

If a runner sends a second `RequestChainedRelay` before the first completes, server replies to the second with the previous in-flight's response when it arrives (or, simpler MVP: server rejects the second with a status — implementer decision).

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
| `runner/relay_handler.go` | Convert `handleEstablishRelay` from lazy (expectedRelays + completeRelaySetup) to eager `SetProxy` (synthetic-owned). Remove or deprecate `expectedRelays` / `completeRelaySetup`. |
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

The red test (`TestChainedRelayMissing`) stays red until commit 6 lands. Commit 7 is a cleanup of Phase C that becomes possible once eager SetProxy is the rule; could be deferred to a separate change if scope creeps.

## Trust model

- Chained-relay setup requests travel runner → server over the PSK-validated, end-to-end-ECDH-encrypted registered conn. Server is the only entity that orchestrates relay setup; intermediate hops only receive Phase C `EstablishRelay` from server over their direct registered conns (same trust path as existing Phase C).
- End-to-end ECDH between agent and server survives the chain — `objproto.SetProxy` at each hop is opaque packet relay (no decrypt).
- PSK exchange happens agent ↔ server end-to-end after the rehandshake; hops cannot validate or forge.

## Open questions

1. **Phase C `dial-runner --via` `RehandshakeForProxy` removal**: with eager-SetProxy at the proxy, server's first `SendHandshake` at slot_id IS the end-to-end ECDH (forwarded raw to target, target ECDHs with server). The current `RehandshakeForProxy` in `server/dial_runner_handler.go` becomes redundant. Need to confirm this doesn't break the existing flow under all conditions (DialGreeting send order, target's `handleAcceptedConn` discrimination, etc.).
2. **Concurrent RequestChainedRelay**: spec says "at most one in flight per runner conn", but the wire has no request_id. Implementer must decide between (a) reject second-in-flight (simplest), (b) queue, or (c) add a request_id field (cleanest but wider wire change).
3. ~~**Chain walk authoritativeness**~~ — resolved by adding `RunnerEntry.Via` (see Prerequisite above). Server walks the explicit Via field instead of addr-matching.
4. **Cleanup on broken chain mid-setup**: if hop 2 of a 3-hop chain rejects, the SetProxy set up at hop 1 dangles. Add an opportunistic `DeleteProxy` rollback in the server handler? Or leave the entries to AutoGarbageCollect's idle sweep? (TTL approximately 5min default.)
