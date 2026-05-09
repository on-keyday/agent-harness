# UDP / WebSocket Dualstack — Design

- Date: 2026-05-09
- Status: Draft
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

### 5.3 `cmd/harness-server/main.go`

```go
var (
    listenAddr    = flag.String("listen", "", "WebSocket listen address (host:port). Empty unless paired with --udp-listen.")
    udpListenAddr = flag.String("udp-listen", "", "UDP listen address (host:port).")
    ...
)

if *listenAddr == "" && *udpListenAddr == "" {
    fatal("at least one of --listen or --udp-listen is required")
}

var ep objproto.Endpoint
switch {
case *listenAddr != "" && *udpListenAddr != "":
    // Dualstack
    udpPort := mustParsePort(*udpListenAddr)
    mux := http.NewServeMux()
    ds, err := transport.UDPWebsocketDualStackEndpoint(transport.UDPWebsocketDualStackConfig{
        Logger:  slog.Default(),
        UDPPort: udpPort,
        Mux:     mux,
        WS: transport.WebSocketConfig{
            Logger: slog.Default(),
            Path:   cli.WebSocketPath,
            Mode:   objproto.EndpointModeServer,
        },
    })
    fatal(err)
    ep = ds.Endpoint
    go startHTTPServer(*listenAddr, mux)
case *listenAddr != "":
    // WS only (current path; refactored into a helper)
    ep, err = newWSServer(*listenAddr)
    fatal(err)
case *udpListenAddr != "":
    // UDP only
    udpPort := mustParsePort(*udpListenAddr)
    ep, err = transport.UDPEndpoint(slog.Default(), udpPort, objproto.EndpointModeServer)
    fatal(err)
}

s := server.New(server.Config{Addr: bestAddr(*listenAddr, *udpListenAddr), DataDir: ...})
s.RunWithEndpoint(ctx, ep)
```

`server.Run` currently constructs the endpoint internally via WS-only.
This refactor splits `Run` into:

- `Run(ctx)` — back-compat; constructs WS endpoint from `cfg.Addr`.
- `RunWithEndpoint(ctx, ep)` — accepts a caller-built endpoint.

The first delegates to the second. `cmd/harness-server/main.go` uses
`RunWithEndpoint` for the dualstack/UDP-only paths.

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

## 11. Open questions

(none; all addressed in §4.1, §5, §7.)

## 12. Future work

- Transport auto-fallback (probe UDP, fall back to WS).
- DTLS over UDP if a non-objproto auth boundary is ever wanted.
- A `harness-server --listen url1,url2,...` form that takes a comma list of `<scheme>://host:port` and dispatches dynamically.
- Configurable UDP local source port on client/runner (instead of OS-assigned 0) to ease firewall whitelisting.
