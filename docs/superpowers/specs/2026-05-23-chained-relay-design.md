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
5. The proxy_runner sees a Handshake at a slot it has no `proxySettings` or `expectedRelays` entry for → either dropped (Client-mode endpoint) or accepted-as-local-conn-then-closed (Mutual-mode endpoint, where `watchIncomingActiveConns` correctly rejects unauthorized handshakes)

The agent's rehandshake completes only because the proxy_runner's Mutual mode happens to send `HandshakeAck` before `watchIncomingActiveConns` runs Close, but the conn that the agent gets is end-to-end with the **proxy_runner**, not the real server. Any subsequent application traffic (PSK, RunnerHello, TaskControl, ...) is dropped at the proxy_runner ("WARN no active connection for application data") and times out.

The red test `integration/chained_relay_e2e_test.go::TestChainedRelayMissing` reproduces this end to end.

## Non-goals

- **Dynamic re-routing**: if a proxy_runner mid-chain dies, the chain is broken and the registered runner must re-register. No automatic failover.
- **Multiple parallel proxy paths**: a registered runner has exactly one upstream conn at a time.
- **Optimizing the steady-state cost**: every Phase B ceremony pays a "tell upstream about this slot" round-trip. Caching strategies are out of scope for the MVP.
- **Browser / WASM clients**: the WASM transport is Client-mode only and cannot participate in the chain. The chain applies to native processes (CLI, agent-spawned harness-cli, TUI) only.
- **Server initiating the chain detection**: the runner asks upstream; upstream does not pre-announce its proxy status. Less wire traffic in the common case where no chain exists.

## In scope

- Registered runner that is itself reached via one or more Phase C relay hops can host agent processes that use `HARNESS_PROXY_VIA_RUNNER` (= the registered runner) for `harness-cli` calls, and have those calls reach the real server end-to-end.
- 2-hop chain (one proxy_runner between registered runner and server) — primary use case driven by the user's current deployment.
- N-hop chain — falls out naturally from the design (each intermediate runner forwards the relay-extension request to its own upstream), as long as N is finite. Reference target: N ≤ 4.
- Both `EndpointModeMutual` proxy_runners (Phase C-registered listen-mode runners) and dial-mode proxy_runners (originally dial-mode runners that have been chained via further --via in another invocation).

## Topology

### Current (broken, pinned by red test)

```
agent ──Phase B──> local_runner ──forward(NEW slot)──> proxy_runner ──???───> server
                                                       │
                                                       └── no expectedRelays for NEW slot
                                                           → closed; agent times out
```

### Desired

```
agent ──Phase B──> local_runner ──extend-relay-request──> proxy_runner ──extend-relay-request──> server
                                                                                                  │
                                                                                                  │ "I'm the real
                                                                                                  │  server, no
                                                                                                  │  chain needed"
                                                                                                  │ reply
                                                                                                  ▼
                                                                                              [Ok response
                                                                                               propagates
                                                                                               back through
                                                                                               the chain]
                                                       │
                                                       │ Each intermediate has now
                                                       │ registered expectedRelays
                                                       │ for the agent's slot_id.
                                                       │
agent ──Handshake(rehandshake)──> local ──fwd(slot)──> proxy ──fwd(slot)──> server
                                                                              ▲
                                                                              │ ECDH end-to-end
                                                                              │ with agent
                                                                              ▼
                                                                          peer.Conn ready
```

## Design

### Wire schema additions

`runner/protocol/message.bgn`:

```bgn
# Bidirectional control message that requests an intermediate hop to set up a
# forwarding entry for a given slot_id. Sent runner → "upstream peer" before
# the runner's local SetProxy completes (Phase B agent proxy ceremony).
#
# - When the upstream peer is a real server: server replies NotAProxy. The
#   runner interprets this as "no chain needed, my SetProxy already points
#   to you, the rehandshake will work directly". Status is informational, no
#   server-side state changes.
# - When the upstream peer is a proxy_runner (= itself chained or just
#   acting as Phase C relay): proxy_runner records expectedRelays[slot_id]
#   = its own upstream-side allocate CID, recurses by sending its OWN
#   ExtendRelay to its upstream, and replies based on the recursion result.
format ExtendRelayRequest:
    slot_id :u16
    # target is the FINAL destination (server.Addr from the original
    # caller's view). Each intermediate substitutes its own upstream-side
    # ConnectionID for its local SetProxy allocate, but the target field
    # propagates upstream verbatim so the deepest hop can verify reachability.
    target :RunnerID

enum ExtendRelayStatus:
    :u8
    ok              = "ok"            # relay chain established up to the responder
    not_a_proxy     = "not_a_proxy"   # responder is a real server; caller may proceed direct
    slot_collision  = "slot_collision"
    upstream_failed = "upstream_failed"  # responder is a proxy but its own upstream rejected
    invalid_target  = "invalid_target"

format ExtendRelayResponse:
    status :ExtendRelayStatus
```

Wire kind tagging: `ExtendRelayRequest` and `ExtendRelayResponse` are added as new variants in BOTH `RunnerMessage` and `RunnerRequest` (the two directions of the registered conn) — see the `Wire bidirectionality` section below.

### Wire bidirectionality

The existing schema treats `RunnerRequest` as strictly server → runner and `RunnerMessage` as strictly runner → server. This spec needs `ExtendRelay` traveling runner → upstream (where upstream may be a server OR another proxy_runner) — i.e. always in the runner-message direction. Add `ExtendRelayRequest` to `RunnerMessage` (sent by the lower runner). Add `ExtendRelayResponse` to `RunnerRequest` (sent by the upstream, since from its perspective it's the runner-control-message side).

This is consistent with the existing `RunnerHello` / `RunnerHelloResponse` pair (runner sends Hello, server replies HelloResponse via RunnerRequest).

### Behavior on each role

#### Local runner (the one running agent_proxy ceremony)

In `runAgentProxyCeremony` (`runner/agent_proxy.go`), insert a step BEFORE the local `SetProxy`:

1. Compute the `slot_id` (= `agentCID.ID`)
2. Build `ExtendRelayRequest{slot_id, target=server.Addr-as-from-original-caller-perspective}`
   - For the local runner: target is `Session.ServerCID` (its view of the final destination)
3. Send over the existing registered conn (Sender)
4. Wait for `ExtendRelayResponse` (timeout 10s, parallel to current `EstablishRelay` response handling)
5. Branch on status:
   - `Ok` or `NotAProxy` → continue to local `SetProxy` (current code)
   - any error → reject the agent's ProxyRequest with `ProxyEstablishStatus_InternalError` (existing path)

#### Upstream peer: server case

Server adds a handler for `RunnerMessage{ExtendRelayRequest}`:

- Look at `req.target`. The server checks whether `req.target` matches its own listen address.
- If yes: reply `RunnerRequest{ExtendRelayResponse{NotAProxy}}` over the same registered conn
- If no: the runner is asking the server to act as a proxy for a different target. Currently out of scope (server-as-proxy is a different feature). Reply `InvalidTarget`.

#### Upstream peer: proxy_runner case (recursive)

When a proxy_runner receives `RunnerMessage{ExtendRelayRequest}` from a registered runner:

1. Slot collision check vs its own server-conn slot_id (same logic as `relayHandlerState.validate` for Phase C)
2. Store `expectedRelays[req.slot_id]` = `(local.serverCID.Transport, local.serverCID.Addr, req.slot_id)` — same shape as `runAgentProxyCeremony`'s `allocateCID` but built from this proxy_runner's view of ITS server (which is either the real server OR yet another proxy_runner upstream)
3. **Recurse**: send its own `ExtendRelayRequest{slot_id, target=req.target}` to ITS upstream peer over its own registered conn
4. Wait for upstream response (timeout 10s, propagated)
5. On upstream Ok / NotAProxy → reply Ok to the original caller
6. On upstream error → reply with the appropriate error (e.g. UpstreamFailed)
7. When the actual Handshake packet arrives at slot_id later (from the lower runner via SetProxy forward at the lower runner's local SetProxy), the existing `watchIncomingActiveConns` (or accept-loop equivalent) picks it up via the `expectedRelays.Take(slot_id)` hit and runs `completeRelaySetup`

The same `expectedRelays` map and `completeRelaySetup` function (`runner/relay_handler.go`) are reused — no fork of the codepath.

### Ceremony (full 2-hop case)

```
admin laptop                  agent on runner_L      runner_L           proxy_runner_P   server
                              (HARNESS_TASK_ID set)  (listen mode,      (dial mode,
                              (HARNESS_PROXY_VIA=    registered via     directly
                               local L addr)          Phase C through P) registered)

[Phase A + Phase C registration setup happens earlier]
agent invokes `harness-cli ls`:
  agent.cli.DialPeerConn → DialViaProxy(L, taskID)
  ───Dial L─────────────────────────►
  ───ProxyRequest{taskID}────────────►
                                       L.runAgentProxyCeremony:
                                         validate → Ok
                                         (NEW STEP) send ExtendRelay
                                         ──ExtendRelayRequest{slot=agentCID.ID, target=server.Addr}─►
                                                                           P.handleExtendRelay:
                                                                             validate slot collision → Ok
                                                                             Put expectedRelays[slot]
                                                                               = (P.serverCID.Addr, slot)
                                                                             (NEW STEP, recurse)
                                                                             ──ExtendRelayRequest{slot, target}─►
                                                                                                              server.handle:
                                                                                                                target == self → reply NotAProxy
                                                                                                              ◄──Response{NotAProxy}─
                                                                           Ok → reply Ok upward
                                         ◄─Response{Ok}─────────────────
                                         (CONTINUE existing path)
                                         SetProxy(agentCID, allocate=(L.serverCID.Addr, slot))
                                                                                 (= P.Addr, slot)
                                         reply ProxyEstablishResponse{Ok}
  ◄─Response{Ok}──────────────────────
  ───RehandshakeForProxy───►
                                       L's SetProxy hits → forward
                                       ────────────────────────────────►
                                                                           P's expectedRelays hit
                                                                           → completeRelaySetup
                                                                           → SetProxy(L.Addr-slot,
                                                                              alloc=(server.Addr, slot))
                                                                           ───────────────────────────►
                                                                                                              server.receive
                                                                                                              → ECDH with agent
                                                                                                              ←─HandshakeAck (back through
                                                                                                                proxy SetProxy chain)
  ◄─end-to-end peer.Conn ready (agent ↔ server, transparent through L and P)

agent.cli.Dial returns *cli.Client backed by this end-to-end conn.
PSK + RunnerHello + actual List request etc. flow normally.
```

### N-hop case

Same flow recursing N times. Each intermediate sees ExtendRelayRequest, sets up expectedRelays, forwards to its own upstream, replies based on the chain's result. The original caller (agent's local runner) sees a single Ok/error.

Limits:
- Practical N: 2-4. Beyond that, latency stacks up (10s × N timeout potential).
- No explicit max-hop count in the protocol. Each hop's 10s timeout bounds total latency.
- Loop detection: out of scope. If a proxy_runner accidentally forms a cycle (P → Q → P), the recursion sees the same slot_id collide and replies `SlotCollision` → caller fails fast.

## Implementation outline

### Files touched

| File | Change |
|---|---|
| `runner/protocol/message.bgn` | Add ExtendRelayRequest / ExtendRelayResponse / ExtendRelayStatus. Variants on RunnerMessage + RunnerRequest. |
| `runner/protocol/message.go` | Regenerated |
| `runner/relay_handler.go` | New function `handleExtendRelay` (mirrors `handleEstablishRelay` shape). Sends its own request upstream via Session.Sender. |
| `runner/agent_proxy.go` | `runAgentProxyCeremony` adds an ExtendRelay request step BEFORE local SetProxy. Uses Session.Sender + a response-channel pattern. |
| `runner/connect.go` | Add `dispatchRunnerRequest` arm for `ExtendRelayResponse` (incoming reply from upstream). |
| `runner/session.go` | Add per-slot response channel map (mirror of server's `relayRespCh`). |
| `server/runner_handler.go` | Add handler for `RunnerMessage{ExtendRelayRequest}`. Compute self-match-or-not, reply via RunnerRequest. |
| `server/server.go` | Wire the new dispatcher hook into RunnerHandler. |
| `integration/chained_relay_e2e_test.go` | Flip from "expect failure" to "expect success" — invert the cli.List assertion. |

### Test plan

- New unit tests on `handleExtendRelay` (slot collision / valid target / invalid target).
- New unit tests on the recursive upstream-call timeout path.
- Existing `TestChainedRelayMissing` red test becomes green (single assertion flip).
- New positive E2E: 3-hop case (agent → runner_A → proxy_P → proxy_Q → server) to exercise N>2 path.

### Order of implementation (per pitfall #1 lesson — keep scope contiguous)

1. Schema additions (commit 1)
2. Server-side `ExtendRelayRequest` handler (replies NotAProxy for self-target) (commit 2)
3. proxy_runner-side `handleExtendRelay` + recursive upstream send (commit 3)
4. agent_proxy ceremony's new pre-SetProxy ExtendRelay step (commit 4)
5. Flip the red E2E test + add 3-hop test (commit 5)

Each commit must build + pass its targeted tests independently. The red test should remain red until commit 4 lands; flipping it earlier hides progress.

## Trust model

The chain extends the existing Phase C trust model recursively:
- Each hop's outbound ExtendRelay travels over its PSK-validated registered conn to upstream (same as Phase C's `EstablishRelay`).
- No new wire-level auth introduced.
- End-to-end ECDH between agent and server survives the chain (objproto.SetProxy is opaque packet relay at each hop — proxy_runner cannot decrypt agent ↔ server payloads even if compromised).
- PSK exchange happens agent ↔ server end-to-end after the rehandshake; chain hops cannot validate or forge.

## Open questions

These are intentionally left unresolved at spec time; implementation may pick a default and document the choice:

1. **Server validation of target field**: when the server receives ExtendRelayRequest with target == self, should it require the target's exact addr+port match its bind addr, or just the port match? With bind-addr `0.0.0.0`, exact-match would fail unless the runner sends loopback-normalized addr. Spec assumes loopback-normalization (already done for `HARNESS_PROXY_VIA_RUNNER` env in `runner/agentenv.go`), but the matching policy needs explicit code.

2. **Connection lifetime of expectedRelays entries**: currently Phase C `expectedRelays.Take` is one-shot (deletes on consume). Chained relay extends this same map. If a hop sets up but the Handshake never arrives (lower-level dial failed), the entry leaks. Add a TTL sweep? Defer to implementation; document if leak is bounded.

3. **Server-as-proxy**: out of this spec. If the user later wants the server to relay to a sibling server (multi-server federation), that's a separate design.

4. **What happens when the local runner's serverCID is itself wrong** (e.g. listen-mode runner that was never reached by a server dial): runner's Session.ServerCID is zero. ExtendRelayRequest has nowhere to go. Implementation should detect and skip the new step gracefully (= treat as no chain, attempt local SetProxy directly — which would still fail, but at the local layer with a clearer error than at the chain layer).
