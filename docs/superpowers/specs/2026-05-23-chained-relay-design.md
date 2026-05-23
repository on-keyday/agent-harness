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

### Prerequisite: `objproto.SetProxy` accepts synthetic `owned`

Current `objproto.SetProxy(owned, allocate)` requires `owned` to exist in
`activeConnections`. This is a precondition leftover from the ksdk pattern
where the proxy first ECDH's with the initiator, then `SetProxy`s, then
closes the activeConn. After Close, the proxySettings entry persists and
forwarding still works (verified in `completeRelaySetup`: it Closes the
activeConn but forwarding continues for subsequent packets).

For chained relay, this precondition forces every hop to perform an
initial-ECDH "warm-up" before `SetProxy` can run (lower hop sending
Handshake to upper hop). That doubles the round-trips per hop and adds
ordering complexity.

**Spec change**: remove the `owned must exist` check in
`objproto.SetProxy`. After the change, `SetProxy` becomes pure
proxySettings registration — both `owned` and `allocate` are synthetic
ConnectionIDs (matching incoming packet headers); no activeConn
requirement on either side.

Safety:
- The activeConn at `owned` was never used post-`SetProxy` (Phase C's
  `completeRelaySetup` Closes it immediately).
- `allocate must NOT exist` check is preserved — prevents ambiguous
  routing where an inbound packet would match both proxySettings and an
  activeConn.
- No new attack surface: `SetProxy` is called internally by runner code;
  there is no public API for arbitrary peers to invoke it.

Phase C's existing usage continues to work: `completeRelaySetup` happens
to call `SetProxy` AFTER an activeConn exists (because the server's
initial ECDH already ran), so the existing-conn case is the trivial
subset of the relaxed contract.

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

When a proxy_runner receives `RunnerMessage{ExtendRelayRequest}` from a
registered lower-runner over its incoming registered conn:

1. Slot collision check vs its own server-conn slot_id (same as
   `relayHandlerState.validate` for Phase C).
2. **Recurse first**: send its own `ExtendRelayRequest{slot_id, target=req.target}`
   to its OWN upstream peer over its own outbound registered conn.
3. Wait for upstream response (timeout 10s).
4. On upstream Ok / NotAProxy:
   - Compute `owned` = synthetic `(lower_runner.Addr, slot_id)` where
     `lower_runner.Addr` is the addr of the registered conn the request
     arrived over (= the proxy's view of the runner that sent ExtendRelay).
   - Compute `allocate` = synthetic `(local.serverCID.Transport,
     local.serverCID.Addr, slot_id)` — this proxy_runner's view of its
     upstream peer.
   - Call `ep.SetProxy(owned, allocate)` — with the synthetic-owned change
     from "Prerequisite" above, no prior ECDH required.
   - Reply `Ok` downward via `RunnerRequest{ExtendRelayResponse{Ok}}`.
5. On upstream error: do NOT SetProxy, reply with appropriate error
   (`UpstreamFailed` etc.) downward.

`expectedRelays` map and `completeRelaySetup` from the Phase C handler are
NOT used in the chained path — SetProxy happens eagerly in step 4,
before any Handshake packet arrives. (Phase C's lazy SetProxy was driven
by the ksdk-style initial-ECDH-then-SetProxy pattern; with synthetic
owned that bootstrap is unnecessary.)

The Phase C original handler (`runner/relay_handler.go` `completeRelaySetup`)
may be left in place as-is or migrated to the eager pattern in a follow-up
cleanup. Either way, this spec only requires the eager pattern for the new
ExtendRelay path.

### Ceremony (full 2-hop case)

With synthetic-owned SetProxy (see Prerequisite above), every hop sets up
its forwarding rule eagerly in `handleExtendRelay`. The agent's rehandshake
then flows through all SetProxy entries without any hop processing the
handshake locally.

```
agent on runner_L      runner_L            proxy_runner_P       server
(HARNESS_TASK_ID set)  (listen mode,       (dial mode,
(HARNESS_PROXY_VIA=    registered via      directly
 local L addr)          Phase C through P)  registered)

[Phase A direct dial registration of P, then Phase C registration of L
 through P, both completed earlier.]

agent invokes harness-cli ls:
  agent.cli.DialPeerConn → DialViaProxy(L, taskID)
  ──Dial L (initial ECDH)─────►
  ──ProxyRequest{taskID}──────►
                                L.runAgentProxyCeremony:
                                  validate task_id → Ok
                                  (NEW) emit ExtendRelay upstream
                                  ──ExtendRelayRequest{slot=agentCID.ID,
                                                       target=server.Addr}──►
                                                                              P.handleExtendRelay:
                                                                                slot collision → Ok
                                                                                (NEW) recurse upstream
                                                                                ──ExtendRelayRequest{slot,
                                                                                                     target}──►
                                                                                                                 server.handle:
                                                                                                                   target == self
                                                                                                                 ◄─Response{NotAProxy}─
                                                                                upstream Ok →
                                                                                  SetProxy(
                                                                                    owned=(L.Addr, slot)  ← synthetic
                                                                                    allocate=(server.Addr, slot)  ← synthetic
                                                                                  )
                                  ◄─Response{Ok}─────────────────────────
                                  (continue existing path)
                                  SetProxy(
                                    owned=(local-view-of-agent, slot)  ← real activeConn from initial Dial
                                    allocate=(P.Addr, slot)            ← synthetic
                                  )
                                  ProxyEstablishResponse{Ok}
  ◄─Response{Ok}──────────────
  ──RehandshakeForProxy──►
                                L's proxySettings hit (owned side)
                                → forward raw to P
                                ─────────────────────────────►
                                                                P's proxySettings hit (owned side)
                                                                → forward raw to server
                                                                ─────────────────────────────────────►
                                                                                                       server.receive:
                                                                                                         Handshake at slot
                                                                                                         → ECDH with agent's
                                                                                                           pubkey
                                                                                                         end-to-end keys
                                                                                                       ◄─HandshakeAck (back through
                                                                                                         P's then L's SetProxy)
  ◄─end-to-end peer.Conn ready (agent ↔ server, opaque through L and P)

PSK + RunnerHello (or whatever the cli subcommand needs) flow normally
agent ↔ server over the relayed conn.
```

Key timing properties:
- Each hop's `SetProxy` is set up BEFORE the rehandshake packet arrives,
  thanks to synthetic-owned. No hop performs local ECDH on the rehandshake.
- The chain is established in one downward-pass (each hop synchronously
  recurses upstream then SetProxy then replies). Total round trips per
  Phase B with N hops above the local runner: `1 (Phase B local dial) +
  N (ExtendRelay req/resp per hop, sequentially through the chain) +
  1 (rehandshake one-way to server, response flows back through chain)`.
  For N=1 (single Phase C upstream): 3 RTs total before agent's peer.Conn
  is usable.

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
| `objproto/objproto.go` | Remove the `owned must exist in activeConnections` precondition from `SetProxy`. Existing call sites unaffected (their owned still exists at call time; the check is purely defensive). |
| `objproto/objproto_test.go` | Add a test verifying synthetic-owned SetProxy + forward via the proxySettings table works without any prior ECDH. |
| `runner/protocol/message.bgn` | Add ExtendRelayRequest / ExtendRelayResponse / ExtendRelayStatus. Variants on RunnerMessage + RunnerRequest. |
| `runner/protocol/message.go` | Regenerated |
| `runner/relay_handler.go` | New function `handleExtendRelay` — eager SetProxy with synthetic owned, plus recursive upstream send via Session.Sender. |
| `runner/agent_proxy.go` | `runAgentProxyCeremony` adds an ExtendRelay request step BEFORE local SetProxy. Uses Session.Sender + a response-channel pattern. |
| `runner/connect.go` | Add `dispatchRunnerRequest` arm for `ExtendRelayResponse` (incoming reply from upstream) + `ExtendRelayRequest` arm for forwarding-on-behalf-of-lower. |
| `runner/session.go` | Add per-slot response channel map for outgoing-ExtendRelay correlation (mirror of server's `relayRespCh`). |
| `server/runner_handler.go` | Add handler for `RunnerMessage{ExtendRelayRequest}`. Compute self-match-or-not, reply via RunnerRequest. |
| `server/server.go` | Wire the new dispatcher hook into RunnerHandler. |
| `integration/chained_relay_e2e_test.go` | Flip from "expect failure" to "expect success" — invert the cli.List assertion. |

### Test plan

- objproto unit test: synthetic-owned SetProxy then receive() forwards by proxySettings without an activeConn.
- `handleExtendRelay` unit tests (slot collision / valid target / invalid target).
- Recursive upstream-call timeout path test.
- Existing `TestChainedRelayMissing` red test becomes green (single assertion flip).
- New positive E2E: 3-hop case (agent → runner_A → proxy_P → proxy_Q → server) to exercise N>2 recursion.

### Order of implementation (per pitfalls catalog — keep scope contiguous, red test stays red until last commit)

1. `objproto.SetProxy` synthetic-owned relaxation + unit test (commit 1)
2. Schema additions (commit 2)
3. Server-side `ExtendRelayRequest` handler (replies NotAProxy for self-target) (commit 3)
4. proxy_runner-side `handleExtendRelay` + recursive upstream send + eager SetProxy (commit 4)
5. agent_proxy ceremony's new pre-SetProxy ExtendRelay step (commit 5)
6. Flip the red E2E test + add 3-hop test (commit 6)

Each commit must build + pass its targeted tests independently. The red test should remain red until commit 5 lands; flipping it earlier hides progress.

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
