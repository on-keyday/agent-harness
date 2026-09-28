# wss: a TLS WebSocket transport

A `wss:` ConnectionID is accepted everywhere a `ws:` one is, and nothing in the
harness implements TLS. Every `WebSocketConfig` the harness builds leaves `TLS`
nil (`cli/dial_endpoint_native.go`, `cli/dial_endpoint_js.go`,
`runner/connect.go`, `server/server.go`), and the server only ever calls
`http.Server.ListenAndServe`. TLS was deferred when the dial path was designed
(`2026-04-26-peer-dial-endpoint-injection-design.md`, "TLS / wss 対応の CLI
フラグ追加 → 別タスク") and never picked up.

## Decisions taken

| Decision | Decided by |
|---|---|
| The server terminates TLS itself (`--tls-cert` / `--tls-key`); no reverse-proxy mode | operator, 2026-09-28 |
| Native clients do not verify the server certificate | operator, 2026-09-28 |
| The HTTPS WebUI is reached by IP (`https://<ip>:<port>`); hostname access is out of scope | operator, 2026-09-28 |
| The certificate comes from PEM files the operator supplies; no self-signed generation | operator, 2026-09-28 |
| Approach A: objtrsf picks the scheme per connection from the CID transport, not per endpoint from `cfg.TLS` | claude, approved by operator 2026-09-28 |

## Problem

1. **An HTTPS WebUI cannot connect.** `webui/static/main.js` builds a `wss:`
   CID when the page is served over HTTPS, but the wasm transport opens
   `ws://` whenever `cfg.TLS` is nil (objtrsf `transport/websocket_wasm.go`,
   `dialAndSend`). A browser blocks `ws://` from an HTTPS page as mixed
   content. (Read from the code; not reproduced in a browser.)
2. **`wss:` from a native client dials plaintext and very likely never
   completes a handshake.** The native WS leg chooses its scheme from
   `cfg.TLS` alone (objtrsf `transport/websocket.go`, `startTransportLoops`)
   and tags every received packet with `transportName(cfg.TLS)`, i.e. `"ws"`.
   `objproto` keys a connection by the full ConnectionID, transport included
   (`objproto/objproto.go`, `receive`: `NewConnectionID(transport, from, …)`),
   so the HandshakeAck arrives as `ws:…` and does not match the pending
   `wss:…`. The first test below exists to confirm this.
3. **The scheme is fixed per endpoint.** One WS leg both dials and accepts.
   The server dials listen-mode runners (`runner/listen.go`) over the same leg,
   so giving that leg a TLS config would turn those dials into `wss` as well.

### What is NOT the problem

- **Confidentiality or peer authentication.** objproto authenticates both ends
  with the PSK handshake and encrypts with an AEAD (objtrsf
  `objproto/crypto.go`: X25519, ChaCha20-Poly1305 / AES-GCM). TLS here is for
  compatibility with browsers, not for security. That is why native clients can
  skip certificate verification: a man in the middle who terminates TLS still
  cannot complete the objproto handshake without the PSK.
- **The wire format.** `ConnID.transport` is a length-prefixed string
  (`runner/protocol/message.bgn`, `format ConnID`), so `"wss"` already travels
  unchanged. There is no schema change and no restart-order concern.

## Design

### 1. objtrsf: the scheme follows the CID transport

`transport/websocket.go` (native):

- `WebSocketConn` carries a `transport` string, and the receive loop calls
  `rawSess.Receive(c.transport, …)`. The label becomes a property of the
  connection, not of the endpoint.
- Dial: pick the scheme from `pkt.To.Transport`.
  - `"wss"`: `wss://` with an `https` Origin and `TlsConfig: cfg.TLS`. If
    `cfg.TLS` is nil, log and `CannotSend`; never fall back to plaintext.
  - `"ws"`: plaintext, as today.
  - The dialed connection is labelled with the scheme it used.
- Accept: label `"wss"` when `ws.Request().TLS != nil`, otherwise `"ws"`.
  Listen-side TLS stays with the caller's `http.Server`.
- Delete `transportName(cfg.TLS)`.
- `cfg.TLS` now means "the client config used when dialing a `wss:` peer".
  Update the doc comment in `websocket_common.go` to say so.

`transport/websocket_wasm.go`: choose the scheme from `pkt.To.Transport` and
label per connection the same way. The browser does the TLS, so `cfg.TLS` is
not read.

`fanOutByTransport` (`dualstack.go`) already routes `"ws"` and `"wss"` to the WS
leg and needs no change. No harness caller sets `cfg.TLS` today.

### 2. Server: flags and startup validation

- `cmd/harness-server/main.go` gains `--tls-cert <path>` and `--tls-key <path>`
  (PEM). They are valid only together; one without the other is a startup
  error. Either flag with an empty `--listen` (UDP only) is also a startup
  error, because TLS applies only to the WS leg.
- `main` loads the pair with `tls.LoadX509KeyPair` **before** listening and
  exits on failure. `ListenAndServe` runs in a goroutine whose error reaches
  `serverDone` asynchronously (`server/server.go`), so letting
  `ListenAndServeTLS(certFile, keyFile)` read the files would surface a bad
  certificate only after startup.
- `server.Config` gains `TLS *tls.Config`. When it is set, the server builds
  `http.Server{TLSConfig: cfg.TLS}` and calls `ListenAndServeTLS("", "")`; when
  it is nil, `ListenAndServe` as today.
- The WebUI shares that `http.Server`, so it becomes HTTPS with no further
  change; `main.js` already builds a `wss:` CID from `location.protocol`.
- The server's own WS leg keeps `cfg.TLS = nil`. It dials listen-mode runners
  over `ws:`, and by section 1 that is unaffected.

### 3. Clients

`WebSocketConfig` is built at these non-test sites:

| Site | Role | Change |
|---|---|---|
| `cli/dial_endpoint_native.go` | CLI, and the TUI through `cli.Client` | `TLS: ClientTLSConfig()` |
| `runner/connect.go` (dualstack and ws-only) | runner dial mode | `TLS: ClientTLSConfig()` |
| `cli/dial_endpoint_js.go` | WebUI (wasm) | none; the browser does TLS |
| `runner/listen.go` | runner listen mode | none (out of scope) |
| `server/server.go` | server | section 2 |

A new `cli/tls_client.go` (`//go:build !js`) holds the one helper:

```go
func ClientTLSConfig() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}
}
```

Its doc comment is the only place that explains why skipping verification is
safe (see "What is NOT the problem"). Call sites do not restate it. For a `ws:`
dial objtrsf never reads this config, so passing it unconditionally does not
change ws behaviour.

The `HARNESS_SERVER_CID` a runner gives its agents is
`pc.Connection().ConnectionID()` (`runner/connect.go`). With per-connection
labels it reads `wss:…` for a runner connected over wss, and the agent's
harness-cli dials wss with `ClientTLSConfig()`. The podman wrapper treats every
transport other than `udp` as tcp (`scripts/sandbox/agent-in-podman.sh`), so its
firewall carve-out is unchanged.

Dialing `wss:` at a plaintext server (or `ws:` at a TLS one) shows up as
objtrsf's `failed to dial websocket` log line and a handshake that never
completes. That is the same failure a ws connection shows today, and improving
it is out of scope.

## Testing

objtrsf:

1. First, a failing test: dial a `wss:` CID against `httptest.NewTLSServer` with
   the current code and show that the handshake does not complete (problem 2).
2. After the change:
   - a wss round trip where both ends see a `wss:` CID;
   - one endpoint dials both a ws and a wss peer;
   - a `wss:` dial with `cfg.TLS == nil` ends in `CannotSend`, not a plaintext
     dial;
   - the accept side labels by `r.TLS`.
3. `go test ./...` and `go vet`; `GOOS=js GOARCH=wasm go build ./...` at least
   builds.

harness:

1. `integration/wss_e2e_test.go`. The test generates its certificate with
   `crypto/x509` (IP SAN 127.0.0.1); no certificate is committed. It starts the
   server with `TLS` and checks that:
   - a `cli.Client` and a runner connect over `wss:`;
   - a task's agent receives a `HARNESS_SERVER_CID` starting with `wss:`.
   That an agent-side harness-cli connects with that CID is checked in the
   dummy-harness E2E below, through the `bash` profile: a Go test's fake agent
   has no auth ticket to run harness-cli with.
   The flag validation in `main` (cert without key; TLS flags with an empty
   `--listen`) gets its own test.
2. Verify with the make targets, not an ad-hoc `go build ./...`.
3. Dummy-harness E2E (`scripts/dummy-harness.sh`):
   - create an IP-SAN self-signed pair with `openssl` in a scratch directory;
   - start the server with `--tls-cert` / `--tls-key` and connect a runner and
     the CLI over `wss:`;
   - submit a `bash`-profile task that runs `harness-cli ls`, and check that it
     succeeds with the `wss:` `HARNESS_SERVER_CID` it was given;
   - open `https://127.0.0.1:<port>` in Playwright (`ignoreHTTPSErrors`) and
     check that the WebUI connects over wss and lists tasks; keep the
     screenshot;
   - take the dummy harness down afterwards.

## Landing

Both repositories land in Mode A (local-trunk fast-forward, then push).

1. objtrsf: fast-forward the change to local main and push.
2. harness: bump objtrsf in `go.mod` and land it with the harness changes as
   one feature set.
3. Run `make build` in the main checkout.

Running a real server with TLS is the operator's choice. Without the new flags
the server behaves exactly as today.

The README (server flags) and any skill text that describes CID transports are
updated in the same feature set; find them by grepping for `wss` and `ws:`
during implementation.

## Out of scope

- Reverse-proxy TLS termination, and verifying the certificate by hostname
  (this would need the hostname carried through the ConnectionID).
- Reaching the WebUI by hostname. It already fails today, because the wasm build
  cannot resolve DNS (`cli/selector_addr_js.go`).
- Self-signed certificate generation, and reloading a certificate without a
  restart.
- TLS on the runner's listen mode.
- A clearer error when the two ends disagree on ws versus wss.
