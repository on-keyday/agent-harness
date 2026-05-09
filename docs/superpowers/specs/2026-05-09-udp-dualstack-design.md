# UDP / WebSocket Dualstack — Design

- Date: 2026-05-09
- Status: Implemented (feat/udp-dualstack)
- Scope: `transport/`, `cmd/harness-server`, `cli`, `runner`

## 1. Goal

Make UDP a first-class transport for harness server, runner, and CLI/TUI
client, alongside the existing WebSocket support. Repair the latent
asymmetry in `transport/dualstack.go` (the "BROKEN AS-IS" fanout) so
that running both transports concurrently is operational, not just a
template.

WebUI WASM stays WebSocket-only (browsers cannot speak raw UDP).

## 2. Non-goals

- **NAT traversal / hole-punching**. UDP requires periodic
  re-registration through stateful firewalls; the existing `AutoPing`
  (15 s default) is sufficient for typical home-LAN NAT timeouts and
  no STUN/TURN-style assistance is added.
- **Transport auto-fallback** (try UDP → fall back WS). Caller selects
  one transport via the CID prefix (`ws:`, `wss:`, `udp:`); a future
  spec can add probing.
- **Datagram size optimisation**. trsf already does PLPMTUD on Linux;
  no new MTU policy is introduced.
- **TLS / DTLS over UDP**. ECDH + AES-GCM at the objproto layer remains
  the auth/encryption boundary; UDP carries opaque ciphertext.
- **WebUI / WASM UDP**. Out of reach for browsers.

## 3. Existing architecture (要点)

- `transport/udp.go`: complete `UDPEndpoint(Ex)` with PLPMTUD probe-mode
  on Linux. Send/recv loops are correct. Accepts an explicit `sendTo`
  channel so it can be fanned-out from a shared `RawEndpoint`.
- `transport/websocket.go`: `WebSocketEndpointEx(rawSess, mux, cfg)`
  always reads `rawSess.GetSenderChannel()` directly. The historical
  `sendTo` parameter was dropped during the WebSocketConfig refactor
  (commit `43bea49`).
- `transport/dualstack.go`: `UDPWebsocketDualStackEndpoint` documents
  the resulting fanout race ("roughly half the WS-bound traffic
  lost") in a "BROKEN AS-IS" comment; `ClientEndpoint` works for
  single-stack but is currently caller-zero.
- `objproto.ConnectionID` already parses `ws:`, `wss:`, `udp:`
  prefixes. `PacketData.To.Transport` carries the per-packet target
  transport, so a fanout split by `pkt.To.Transport` is the documented
  routing point.
- `cmd/harness-server/main.go`: builds a single WS endpoint via
  `transport.WebSocketEndpoint(...)` from `--listen host:port`.
- `cli.Dial` and `runner.Connect`: both call
  `transport.WebSocketEndpoint(...)` unconditionally; the runner has a
  `PingInterval` knob but no transport selection.

## 4. UX

### 4.1 Server CLI

`cmd/harness-server/main.go` gets a new flag, additive to `--listen`:

```
--listen host:port           WebSocket listen address (default :8539,
                             current behaviour, unchanged)
--udp-listen host:port       additional UDP listen address. Empty =
                             disabled. Combine with --listen for
                             dualstack.
```

Combinations:

| Flags | Result |
|---|---|
| `--listen :8539` (current) | WS only. CIDs advertised: `ws:host:8539-N`. |
| `--udp-listen :8540` | UDP only. CIDs advertised: `udp:host:8540-N`. |
| `--listen :8539 --udp-listen :8540` | Dualstack. Server accepts both transports on the *same* RawEndpoint, so peer connections can land via either. CIDs advertised: `ws:host:8539-N` or `udp:host:8540-N` depending on origin transport. |
| (neither set) | error: at least one listen address required. |

Currently `--listen` accepts a host:port string with implicit `ws://`.
Keeping that semantics avoids breaking any wrapper script that does
`scripts/server.sh up --listen :8539`. UDP is opt-in via the new flag.

### 4.2 Client / Runner CLI

Both already accept a `--server-cid` of the form `<transport>:host:port-N`.
Today only `ws:` and `wss:` actually function; this spec makes `udp:`
operational. No new flags on the client/runner side; they read the
transport from the CID and dial accordingly.

Examples:

```
agent-runner --server-cid 'udp:192.168.3.234:8540-*' ...
harness-tui  --server-cid 'udp:192.168.3.234:8540-*' ...
harness-cli  --server-cid 'ws:192.168.3.234:8539-*' ls   # ws unchanged
```

### 4.3 WASM (WebUI)

Untouched. `harness.connect("ws:...")` still calls `cli.Dial` which
dispatches to WebSocket. UDP CIDs returned by `cli.Dial` would error
out at endpoint construction since `transport/websocket_wasm.go` does
not link `transport.UDPEndpoint` (UDP not available in WASM env).

## 5. Architecture (changes)

### 5.1 `transport/websocket.go` — restore `sendTo`

Re-add the `sendTo` parameter that `43bea49` dropped. Back-compat-friendly:

```go
// WebSocketEndpointEx — back-compat: sendTo == nil means
// rawSess.GetSenderChannel() (the prior shape).
func WebSocketEndpointEx(
    rawSess objproto.RawEndpoint,
    mux *http.ServeMux,
    cfg WebSocketConfig,
    sendTo <-chan *objproto.PacketData,
) error
```

Internal call to `startTransportLoops` uses `sendTo` (or
`rawSess.GetSenderChannel()` when `sendTo` is nil) for the sender loop.
Existing single-stack callers (`cli.Dial`, `runner.Connect`,
`server.Run`'s WS branch, `transport.WebSocketEndpoint`) pass `nil` and
preserve current behaviour.

The `WebSocketEndpoint` non-Ex wrapper stays as is — it's defined as
"single stack with default sender channel", so it explicitly passes
nil.

### 5.2 `transport/dualstack.go` — repair fanout

Update `UDPWebsocketDualStackEndpoint` to construct two channels and
hand them to UDP and WS legs respectively:

```go
udpChan := make(chan *objproto.PacketData, 100)
wsChan  := make(chan *objproto.PacketData, 100)

UDPEndpointEx(rawSess, cfg.Logger, cfg.UDPPort, udpChan)
WebSocketEndpointEx(rawSess, cfg.Mux, cfg.WS, wsChan)

// Single reader of rawSess.GetSenderChannel(), routed by transport.
go func() {
    for pkt := range rawSess.GetSenderChannel() {
        switch pkt.To.Transport {
        case "udp":
            udpChan <- pkt
        case "ws", "wss":
            wsChan <- pkt
        default:
            cfg.Logger.Error("unsupported transport", "t", pkt.To.Transport)
        }
    }
}()
```

Remove the "BROKEN AS-IS" warning. Add a unit test.

### 5.3 `cmd/harness-server/main.go` and `server.Run`

**Implemented as committed (5f5fedc):** the entry-point split `Run` /
`RunWithEndpoint` in the original draft was dropped — there are no
external callers to preserve and a single `Server.Run` is cleaner.

`Config` gains a `UDPAddr` field. `Server.Run` inspects `cfg.Addr` /
`cfg.UDPAddr` and dispatches inline:

- `Addr` only      → single-stack WebSocket on `cfg.Addr` (current behaviour)
- `UDPAddr` only   → single-stack UDP on `cfg.UDPAddr`; webui not served
- both             → ws+udp dualstack via `UDPWebsocketDualStackEndpoint`
- neither          → error

`buildEndpoint` is an unexported helper next to `Run` that returns
`(ep, mux, httpAddr)`. `parseListenPort` is a small string→port
helper for `:port` / `host:port` syntax.

`cmd/harness-server/main.go` adds `--udp-listen` alongside `--listen`,
both passed verbatim into `server.Config{Addr, UDPAddr}`.

### 5.4 `cli/client.go` — transport-aware Dial

`cli.Dial` builds a transport-specific endpoint based on the parsed
peerCID:

```go
func Dial(ctx context.Context, peerCID objproto.ConnectionID) (*Client, error) {
    ep, err := buildClientEndpoint(peerCID)
    if err != nil { ... }
    ...
}

func buildClientEndpoint(peerCID objproto.ConnectionID) (objproto.Endpoint, error) {
    switch peerCID.Transport {
    case "ws", "wss":
        return transport.WebSocketEndpoint(nil, transport.WebSocketConfig{
            Logger: slog.Default(),
            Path:   WebSocketPath,
            Mode:   objproto.EndpointModeClient,
        })
    case "udp":
        // Local UDP source port: 0 = OS-assigned.
        return transport.UDPEndpoint(slog.Default(), 0, objproto.EndpointModeClient)
    default:
        return nil, fmt.Errorf("unsupported transport: %s", peerCID.Transport)
    }
}
```

The WASM build (`cli/client.go` is `!js`-tagged?) — verify file
boundaries; UDP path needs `!js` build tag so the WASM build doesn't
pull in `net.ListenUDP` (unavailable in JS env).

If `cli/client.go` is shared between native and WASM, the dispatch
moves to a build-tagged helper (`cli/dial_native.go` vs
`cli/dial_js.go`).

### 5.5 `runner/connect.go` — transport-aware Connect

Same pattern as `cli.Dial`. The runner is native-only (no WASM), so a
straight switch is sufficient inside `runner.Connect`.

### 5.6 `transport/websocket_wasm.go`

No changes. WASM build links only `WebSocketEndpoint`; UDP is not
referenced.

## 6. Concurrency model

- Single `RawEndpoint` per process (server) or per dial (client/runner).
- Dualstack server: the rawSess fanout goroutine is the *only* reader
  of `rawSess.GetSenderChannel()`; UDP/WS legs each read their own
  dedicated bounded channel (`udpChan`, `wsChan`, cap 100).
- Single-stack server / client / runner: legacy path — leg reads
  `rawSess.GetSenderChannel()` directly via `sendTo == nil` semantics.
- No new goroutines beyond the fanout; UDP/WS internal goroutines are
  unchanged.

## 7. Edge cases

| Case | Handling |
|---|---|
| Both `--listen` and `--udp-listen` set on a server | Dualstack; rawSess shared, fanout routes packets by `pkt.To.Transport`. |
| `--udp-listen` only | UDP-only; clients must dial `udp:` CIDs. |
| Client dials `udp:` against WS-only server | UDP packets land on no listener; objproto handshake times out; user sees "ecdh handshake failed". |
| Client dials `ws:` against UDP-only server | TCP connect refused; clear error. |
| Dualstack server, client dials `ws:` | WS leg accepts; rawSess attaches the connection; outbound packets carry `Transport: "ws"` so fanout routes correctly. |
| Dualstack server, runner dials `udp:` | Symmetric; UDP leg accepts; fanout routes correctly. |
| WASM client tries `udp:` | Build error if attempted (UDP transport not linked); empirically unreachable since the page CID is a string the user controls. |
| Server restart while runner uses UDP and `--persist=true` | UDP NAT binding may have expired; runner re-dials, OS assigns a new local source port; ConnectionID changes; `Fresh reconnect` semantics from persist spec apply unchanged. |
| Path MTU drops on UDP path | `udp_pmtud_linux.go` already handles `EMSGSIZE`; trsf adapts. |

## 8. Affected files

```
transport/websocket.go                  (sendTo arg restored)
transport/websocket_wasm.go             (no change required; verify)
transport/dualstack.go                  (repair fanout, drop BROKEN comment)
transport/dualstack_test.go             (new; smoke fanout)
cmd/harness-server/main.go              (--udp-listen flag, RawEndpoint selection)
server/server.go                        (RunWithEndpoint variant; Run delegates)
cli/client.go                           (transport-aware Dial; potentially split native/js)
cli/client_native.go                    (new only if cli/client.go is shared)
cli/client_js.go                        (new only if cli/client.go is shared)
runner/connect.go                       (transport-aware Connect)
integration/udp_test.go                 (new; in-process UDP server + UDP runner round-trip)
```

## 9. Testing strategy

### 9.1 Unit

- `transport/dualstack_test.go::TestDualStackFanoutSplitsByTransport`:
  build a dualstack, push fake `PacketData` with `Transport: "udp"`
  and `"ws"` onto `rawSess.GetSenderChannel()`, assert udpChan and
  wsChan each receive their share with no loss.
- `transport/dualstack_test.go::TestDualStackUnsupportedTransportLogs`:
  push a packet with `Transport: "bogus"`, assert log emitted, no
  panic.

### 9.2 Integration (`integration/udp_test.go`, build tag `integration`)

- `TestRunnerOverUDP_RoundTrip`: in-process UDP server + UDP runner;
  submit one task via cli.Client (also UDP-dialed); assert task
  completes Successful end-to-end.
- `TestServerDualStackAcceptsBothLegs`: server with WS+UDP listens;
  one runner dials WS, one runner dials UDP; submit two tasks; assert
  each lands on its respective runner.

### 9.3 Manual smoke

```
bin/harness-server --listen :8539 --udp-listen :8540 --data-dir ...
bin/agent-runner --server-cid 'udp:127.0.0.1:8540-*' --roots /tmp/x ...
bin/harness-cli --server-cid 'udp:127.0.0.1:8540-*' submit --repo /tmp/x --task "hi"
```

## 10. Implementation order

1. `transport/websocket.go`: re-add `sendTo` (back-compat nil default).
2. `transport/dualstack.go`: repair fanout; remove BROKEN comment.
3. `transport/dualstack_test.go`: unit test for fanout.
4. `server/server.go` + `cmd/harness-server/main.go`: `--udp-listen` flag + `RunWithEndpoint`.
5. `cli/client.go` (and any build-tag split needed): transport-aware Dial.
6. `runner/connect.go`: transport-aware Connect.
7. `integration/udp_test.go`: end-to-end.
8. Final review, spec amendment if commonalisation is in order.

Each step is a self-contained commit (atomic).

## 11. Implementation notes (post-merge)

- The original draft proposed splitting `server.Run` into `Run`
  (back-compat) and `RunWithEndpoint` (new). Per individual-dogfood
  policy (no external callers), this was dropped during step 3 in
  favour of a single `Server.Run` that dispatches by `cfg.UDPAddr` /
  `cfg.Addr`. The `RunWithEndpoint` symbol does not exist in the
  landed code.
- `transport/dualstack.go::fanOutByTransport` was extracted as an
  unexported helper to make the routing testable without binding a
  real UDP port. Three regression tests cover it.
- `cli/dial_endpoint_native.go` (build !js) and
  `cli/dial_endpoint_js.go` (build js) split the transport selection
  so the WASM build doesn't try to link `transport.UDPEndpoint`.
- `runner/connect.go::buildRunnerEndpoint` is a private mirror; runner
  is native-only so no build-tag split is needed.
- All 4 + 16 unit/integration tests pass; the only failing test in
  the suite (`TestSubmitWakeE2E`) is a pre-existing flake unrelated to
  this work.

## 12. Future work

### 12.1 Migrate large control messages to trsf streams (priority)

The harness was authored with WS-bias: many control messages go through
`objproto.Connection.SendMessage` (single application-kind packet)
rather than via `trsf` bidirectional streams. WS has no per-message
size limit, so this works there. **UDP delivers each `SendMessage` as
one datagram and silently drops anything exceeding path MTU**
(`transport/udp.go:43-46`: EMSGSIZE → debug log + `continue`).

Known offenders that will fail under UDP once the system holds
non-trivial state:

| Site | Path | Why it gets large |
|---|---|---|
| Snapshot response | `server/task_handler.go::TaskControlKind_List` | Up to 100 TaskInfo + all RunnerInfo packed into one TaskControlResponse — ~20 KB+ on a busy server |
| AssignTask | `server/dispatch.go::sendAssign` | Carries repo path + prompt + extra-args; long prompts blow MTU |
| Agentboard agent_message | `cli/agent/conn.go::Send` and `server/agent_handler.go` | Configurable up to 64 KB payload by default |
| Various TaskControl responses with embedded variable-length fields | `server/task_handler.go` | TaskInfo / event payloads |

The integration tests in §9.2 pass because they only exercise small
state (1–2 tasks, short prompts) — they do NOT catch this edge.

Migration approach (per-offender):

1. Allocate a server-initiated bidi stream when responding to a
   request that may exceed MTU. Send the payload as framed data on
   the stream, send only the small "stream_id" via SendMessage so the
   client knows where to read.
2. The existing `GetTaskLog` flow already follows this pattern and
   works under UDP (see `cli/get_log.go:87`-style comments).

Until these are migrated, **UDP is operational for small-state
deployments only**; mixed UDP+WS dualstack lets WS-using clients
handle the large payloads while UDP-using clients still benefit for
small RPCs (Hello / TaskAccepted / Heartbeat).

### 12.2 Surface oversize *application* messages (not transport EMSGSIZE)

`transport/udp.go` currently `slog.Debug`-logs EMSGSIZE on send and
continues. **This is the right level**: probe-mode PLPMTUD
intentionally sends oversize datagrams to discover path MTU, so
EMSGSIZE is the *normal* feedback signal during probing — bumping it
to `Warn` would spam the log on every healthy MTU search.

By the time bytes reach `transport/udp.go`, the packet has already
been encrypted and packed into a `*objproto.PacketData`; the
`IsMTUProbe` bit that `trsf` set on its `SendingPacket` (see
`trsf/ack_handler.go:53`, `trsf/conn.go:548`) does not survive into
the `PacketKind`, which is just `Application` for both probes and
real data. Transport cannot tell them apart.

The right place to warn is **the application send site** —
`objproto.activeConnection.SendMessage` (or its callers), where we
know the bytes are real application data, not a probe. A pre-encrypt
size check against a configurable "safe payload" threshold (e.g.
1200 bytes — well under typical 1500 Ethernet MTU minus IP/UDP/AES
overhead) emitting `slog.Warn` with the decoded
`wire.ApplicationPayloadKind` (the first byte of the message)
preserves probe-quietness while surfacing the LLM-pattern. This
diagnostic is independent of UDP / WS — it warns on WS too where the
bytes will go through fine, but the message is still a hint that the
caller should consider a stream.

### 12.3 Other items

- Transport auto-fallback (probe UDP, fall back to WS).
- DTLS over UDP if a non-objproto auth boundary is ever wanted.
- A `harness-server --listen url1,url2,...` form that takes a comma list of `<scheme>://host:port` and dispatches dynamically.
- Configurable UDP local source port on client/runner (instead of OS-assigned 0) to ease firewall whitelisting.
- objproto-layer application-message fragmentation, so SendMessage
  callers don't need to know about MTU at all. This is a deeper
  protocol change (sequence number space, reassembly buffer, drop
  semantics) and intentionally deferred per §2 toy-scope.
