# wss Transport Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make a `wss:` ConnectionID mean TLS WebSocket end to end: the server terminates TLS itself, native clients dial wss without verifying the certificate, and the WebUI works over HTTPS.

**Architecture:** objtrsf stops deciding the scheme per endpoint (`cfg.TLS != nil`) and decides it per connection. A dial uses the scheme named by the destination CID's transport, and a received packet carries the transport of the connection it arrived on. The harness then adds `--tls-cert` / `--tls-key` to `harness-server`, adds one client helper that returns a skip-verify `tls.Config`, and passes that helper to the two native dial sites.

**Tech Stack:** Go (`crypto/tls`, `crypto/x509`, `net/http/httptest`, `golang.org/x/net/websocket`), objtrsf (a separate module at `/home/kforfk/workspace/objtrsf`), Python 3 (`scripts/dummy-harness.py`), openssl (E2E certificate only), Playwright MCP (WebUI check).

**Spec:** `docs/superpowers/specs/2026-09-28-wss-transport-design.md`

## Global Constraints

- Two repositories. **objtrsf**: `/home/kforfk/workspace/objtrsf`, module `github.com/on-keyday/objtrsf`, currently on `main` at `370cf26`. **harness**: the task worktree `/home/kforfk/workspace/remote-agent-harness/.harness-worktrees/70fbad4a6eb6f1e992be8a669f1bcefd` on branch `harness/70fbad4a6eb6f1e992be8a669f1bcefd`. Use absolute paths under that worktree for every harness tool call. A bare `/home/kforfk/workspace/remote-agent-harness/<rel>` path writes to the PARENT checkout (implementation-pitfalls, Pitfall 8). Check `git rev-parse --abbrev-ref HEAD` before each commit.
- Read `.claude/skills/implementation-pitfalls/SKILL.md` in full before writing code.
- Native clients use `InsecureSkipVerify: true`. objproto's PSK handshake and AEAD provide peer authentication and confidentiality. The reason is written ONCE, in the doc comment of `cli.ClientTLSConfig`, and is not restated at call sites.
- A `wss:` dial never falls back to plaintext. With no TLS config it fails with a log line and `CannotSend`.
- Scope is exactly the spec's. No reverse-proxy mode, no hostname verification, no self-signed generation inside the server, no certificate reload, no TLS on runner listen mode.
- No wire (`.bgn`) change. `ConnID.transport` is already a string.
- Public repository: no LAN IPs, hostnames or private paths in committed files. Tests use `127.0.0.1`. Never commit a certificate or key.
- Harness verification uses make targets (`make check vet test wasm-check`, `make test-integration`), not an ad-hoc `go build ./...` alone. Build hygiene: never run a bare `go build ./cmd/<x>/` inside a worktree.
- Every commit message ends with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## File Structure

objtrsf:
- `transport/websocket.go`: the per-connection transport label, scheme selection on dial, and TLS-based labelling on accept.
- `transport/websocket_wasm.go`: the same for the browser build.
- `transport/websocket_common.go`: the doc comment that says what `TLS` now means.
- `transport/websocket_tls_test.go` (new): wss round trip, mixed ws and wss, no plaintext fallback.

harness:
- `cli/tls_client.go` (new, `!js`): `ClientTLSConfig()`.
- `cli/dial_endpoint_native.go`, `runner/connect.go`: pass `ClientTLSConfig()`.
- `server/server.go`: `Config.TLS`, and `ListenAndServeTLS` when it is set.
- `cmd/harness-server/main.go`, `cmd/harness-server/main_test.go`: `--tls-cert` / `--tls-key` and `loadListenTLS`.
- `integration/wss_e2e_test.go` (new): server, CLI, runner and the agent's CID over wss.
- `scripts/dummy-harness.py`: `up --tls`.
- `README.md`, plus any skill text that lists CID transports: document wss.

---

### Task 1: objtrsf — the scheme follows the CID transport

**Files:**
- Modify: `/home/kforfk/workspace/objtrsf/transport/websocket.go`
- Modify: `/home/kforfk/workspace/objtrsf/transport/websocket_wasm.go`
- Modify: `/home/kforfk/workspace/objtrsf/transport/websocket_common.go:19-20`
- Create: `/home/kforfk/workspace/objtrsf/transport/websocket_tls_test.go`

**Interfaces:**
- Consumes: nothing new.
- Produces:
  - `WebSocketConfig.TLS *tls.Config` keeps its type. It now means "the client config used when dialing a `wss:` peer".
  - A dial to a `ConnectionID{Transport: "wss"}` uses `wss://`. An accepted connection is labelled `"wss"` iff `r.TLS != nil`.
  - Unexported API changes: `newWebSocketConn(conn, remoteAddr, transport string, cancel)`, `startTransportLoops` loses its `transportName` parameter, `newAcceptHandler` loses its `tlsConf` parameter, and `transportName()` is deleted.

- [ ] **Step 1: Branch**

```bash
cd /home/kforfk/workspace/objtrsf && git status --short && git switch -c wss-scheme-per-connection
```
Expected: no status output, then `Switched to a new branch`.

- [ ] **Step 2: Write the failing tests**

Create `transport/websocket_tls_test.go`:

```go
package transport

import (
	"crypto/ecdh"
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/objproto/packet"
)

const wsTestPath = "/ws"

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// wsServer starts a Server-mode WS endpoint behind httptest, over TLS when
// tlsOn is set. upgrades counts requests that reached the mux, so a test can
// prove a dial never happened.
func wsServer(t *testing.T, tlsOn bool) (objproto.Endpoint, netip.AddrPort, *atomic.Int32) {
	t.Helper()
	mux := http.NewServeMux()
	ep, err := WebSocketEndpoint(mux, WebSocketConfig{Logger: quietLogger(), Path: wsTestPath, Mode: objproto.EndpointModeServer})
	if err != nil {
		t.Fatal(err)
	}
	var upgrades atomic.Int32
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrades.Add(1)
		mux.ServeHTTP(w, r)
	})
	var ts *httptest.Server
	if tlsOn {
		ts = httptest.NewTLSServer(h)
	} else {
		ts = httptest.NewServer(h)
	}
	t.Cleanup(ts.Close)
	return ep, netip.MustParseAddrPort(ts.Listener.Addr().String()), &upgrades
}

func wsClient(t *testing.T, tlsConf *tls.Config) objproto.Endpoint {
	t.Helper()
	ep, err := WebSocketEndpoint(nil, WebSocketConfig{Logger: quietLogger(), Path: wsTestPath, Mode: objproto.EndpointModeClient, TLS: tlsConf})
	if err != nil {
		t.Fatal(err)
	}
	return ep
}

func handshake(t *testing.T, cli objproto.Endpoint, cid objproto.ConnectionID, wait time.Duration) (objproto.Connection, error) {
	t.Helper()
	priv, hs, err := objproto.NewECDHHandshake(ecdh.X25519(), packet.CommonKeyKind_Aes128Gcm)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := cli.SendHandshake(cid, priv, hs)
	if err != nil {
		t.Fatal(err)
	}
	return ch.WaitWithTimeout(t.Context(), wait)
}

func serverSideTransports(ep objproto.Endpoint) []string {
	var out []string
	for _, c := range ep.(objproto.RawEndpoint).ListActiveConnections() {
		out = append(out, c.ConnectionID().Transport)
	}
	return out
}

func skipVerify() *tls.Config { return &tls.Config{InsecureSkipVerify: true} }

// A wss round trip, with both ends seeing a wss: CID. Before the change the
// server labelled every accepted connection "ws" because its own cfg.TLS was
// nil, so its CID disagreed with the client's.
func TestWSSRoundTripLabelsBothEnds(t *testing.T) {
	srv, addr, _ := wsServer(t, true)
	cli := wsClient(t, skipVerify())

	conn, err := handshake(t, cli, objproto.NewConnectionID("wss", addr, 0x1001), 3*time.Second)
	if err != nil {
		t.Fatalf("wss handshake did not complete: %v", err)
	}
	if got := conn.ConnectionID().Transport; got != "wss" {
		t.Errorf("client-side transport = %q, want wss", got)
	}
	if got := serverSideTransports(srv); len(got) != 1 || got[0] != "wss" {
		t.Errorf("server-side transports = %v, want [wss]", got)
	}
}

// One endpoint dials a ws peer and a wss peer. Before the change the scheme
// was fixed per endpoint, so one of the two was dialed with the wrong scheme.
func TestOneEndpointDialsWSAndWSS(t *testing.T) {
	plainSrv, plainAddr, _ := wsServer(t, false)
	tlsSrv, tlsAddr, _ := wsServer(t, true)
	cli := wsClient(t, skipVerify())

	if _, err := handshake(t, cli, objproto.NewConnectionID("ws", plainAddr, 0x2001), 3*time.Second); err != nil {
		t.Fatalf("ws handshake: %v", err)
	}
	if _, err := handshake(t, cli, objproto.NewConnectionID("wss", tlsAddr, 0x2002), 3*time.Second); err != nil {
		t.Fatalf("wss handshake: %v", err)
	}
	if got := serverSideTransports(plainSrv); len(got) != 1 || got[0] != "ws" {
		t.Errorf("plain server transports = %v, want [ws]", got)
	}
	if got := serverSideTransports(tlsSrv); len(got) != 1 || got[0] != "wss" {
		t.Errorf("tls server transports = %v, want [wss]", got)
	}
}

// A wss: dial with no TLS config must not reach the network in plaintext.
// Before the change it dialed ws:// silently (and then failed to match the
// HandshakeAck, which arrived labelled "ws").
func TestWSSWithoutTLSConfigNeverDialsPlaintext(t *testing.T) {
	_, addr, upgrades := wsServer(t, false)
	cli := wsClient(t, nil)

	if _, err := handshake(t, cli, objproto.NewConnectionID("wss", addr, 0x3001), 500*time.Millisecond); err == nil {
		t.Fatal("wss handshake with no TLS config completed")
	}
	if n := upgrades.Load(); n != 0 {
		t.Errorf("server saw %d request(s); a wss dial without TLS must not dial at all", n)
	}
}
```

- [ ] **Step 3: Run the tests and confirm they fail for the stated reasons**

Run: `cd /home/kforfk/workspace/objtrsf && go test ./transport/ -run 'WSS|WSAndWSS' -count=1 -v`
Expected:
- `TestWSSRoundTripLabelsBothEnds` FAILs with `server-side transports = [ws], want [wss]`.
- `TestOneEndpointDialsWSAndWSS` FAILs on the ws handshake (the endpoint's TLS config turns it into a wss dial against a plaintext server).
- `TestWSSWithoutTLSConfigNeverDialsPlaintext` FAILs with `server saw 1 request(s)`.

If one of them PASSES on the unchanged code, stop and report it. The spec's problem statement would then be wrong about that case.

- [ ] **Step 4: Implement in `transport/websocket.go`**

Replace the `WebSocketConn` struct and `newWebSocketConn` (lines 22-36) with:

```go
// WebSocketConn is a connection that uses a WebSocket for communication.
type WebSocketConn struct {
	conn       *websocket.Conn
	remoteAddr netip.AddrPort
	// transport is the objproto transport this connection's packets are
	// delivered under: "wss" over TLS, "ws" otherwise. objproto keys a
	// connection by its full ConnectionID, transport included, so it must
	// equal the transport of the CID the peer is addressed by.
	transport string
	cancel    context.CancelFunc
}

// newWebSocketConn creates a new WebSocketConn.
func newWebSocketConn(conn *websocket.Conn, remoteAddr netip.AddrPort, transport string, cancel context.CancelFunc) *WebSocketConn {
	return &WebSocketConn{
		conn:       conn,
		remoteAddr: remoteAddr,
		transport:  transport,
		cancel:     cancel,
	}
}
```

In `startTransportLoops`, remove the `transportName string` parameter (line 99), so that the signature becomes:

```go
func startTransportLoops(rawSess objproto.RawEndpoint,
	connChan chan *WebSocketConn, connMap *connectionMap,
	senderChannel <-chan *objproto.PacketData,
	tlsConf *tls.Config, dialPath string, logger *slog.Logger) {
```

and change the receive call (line 123) to:

```go
					rawSess.Receive(c.transport, c.remoteAddr, recv)
```

Replace the dial block's scheme selection and `newWebSocketConn` call (lines 135-160) with:

```go
						// The scheme is the destination's, not the endpoint's: one
						// endpoint may reach both ws and wss peers.
						wsScheme, httpScheme := "ws", "http"
						if pkt.To.Transport == "wss" {
							if tlsConf == nil {
								logger.Error("wss dial needs WebSocketConfig.TLS; not falling back to ws",
									slog.String("address", pkt.To.Addr.String()))
								rawSess.CannotSend(pkt)
								return
							}
							wsScheme, httpScheme = "wss", "https"
						}
						conf := &websocket.Config{
							Location: &url.URL{
								Scheme: wsScheme,
								Host:   pkt.To.Addr.String(),
								Path:   dialPath,
							},
							Origin: &url.URL{
								Scheme: httpScheme,
								Host:   pkt.To.Addr.String(),
							},
							TlsConfig: tlsConf,
							Version:   websocket.ProtocolVersionHybi13,
						}
						ws, err := websocket.DialConfig(conf)
						if err != nil {
							logger.Error("failed to dial websocket", slog.String("address", pkt.To.Addr.String()), slog.String("error", err.Error()))
							rawSess.CannotSend(pkt)
							return
						}
						conn := newWebSocketConn(ws, pkt.To.Addr, wsScheme, func() {})
```

Replace `newAcceptHandler` (lines 187-218) with:

```go
// newAcceptHandler builds the http.Handler that upgrades incoming WS
// connections, registers them in connMap, and feeds them into connChan
// for the recv loop to pick up. A connection is labelled "wss" when its
// request arrived over TLS (the caller's http.Server terminates it).
func newAcceptHandler(connChan chan<- *WebSocketConn, connMap *connectionMap, logger *slog.Logger) http.Handler {
	return &websocket.Server{
		Handshake: func(c *websocket.Config, r *http.Request) error {
			var err error
			c.Origin, err = websocket.Origin(c, r)
			if err == nil && c.Origin == nil {
				return fmt.Errorf("null origin")
			}
			return err
		},
		Handler: func(ws *websocket.Conn) {
			ctx, cancel := context.WithCancel(ws.Request().Context())
			remoteAddr, err := netip.ParseAddrPort(ws.Request().RemoteAddr)
			if err != nil {
				logger.Error("invalid remote address", slog.String("address", ws.Request().RemoteAddr))
				ws.Close()
				cancel()
				return
			}
			transport := "ws"
			if ws.Request().TLS != nil {
				transport = "wss"
			}
			conn := newWebSocketConn(ws, remoteAddr, transport, cancel)
			connMap.Set(remoteAddr, conn)
			connChan <- conn
			<-ctx.Done()
		},
	}
}
```

Delete `transportName` (lines 220-227). In `WebSocketEndpointEx`, change line 292 to

```go
		mux.Handle(cfg.Path, newAcceptHandler(connChan, connMap, cfg.Logger))
```

and lines 306-307 to

```go
	startTransportLoops(rawSess, connChan, connMap,
		sendTo, cfg.TLS, dialPath, cfg.Logger)
```

- [ ] **Step 5: Implement in `transport/websocket_wasm.go`**

Delete lines 44-47 (the `transportName` block). Change the dial call at line 77 to:

```go
				go dialAndSend(rawSess, &connsMu, conns, pkt, cfg, logger)
```

In `dialAndSend`, remove the `transportName string,` parameter (line 112) and replace the scheme block (lines 119-122) with:

```go
	// The scheme is the destination's; the browser performs the TLS, so
	// cfg.TLS is not consulted here.
	scheme := "ws"
	if pkt.To.Transport == "wss" {
		scheme = "wss"
	}
```

Change the receive call (line 211) to:

```go
				rawSess.Receive(scheme, conn.remoteAddr, data)
```

- [ ] **Step 6: Update the doc comment in `transport/websocket_common.go`**

Replace lines 19-20 with:

```go
// TLS is the client config used when dialing a wss: peer. The scheme of each
// dial follows the destination ConnectionID's transport, so one endpoint can
// reach ws and wss peers alike; a wss dial with TLS nil fails rather than
// falling back to ws. The listen-side TLS for Server / Mutual is owned by the
// caller's *http.Server, and an accepted connection is labelled wss when its
// request arrived over TLS. The wasm build ignores TLS: the browser performs
// the handshake.
```

- [ ] **Step 7: Run the tests and the whole module**

Run: `cd /home/kforfk/workspace/objtrsf && go test ./transport/ -run 'WSS|WSAndWSS' -count=1 -v && go test ./... -count=1 && go vet ./... && GOOS=js GOARCH=wasm go build ./...`
Expected: all three new tests PASS, then `ok` for every package, no vet output, and the wasm build exits 0.

- [ ] **Step 8: Commit**

```bash
cd /home/kforfk/workspace/objtrsf && git add transport/websocket.go transport/websocket_wasm.go transport/websocket_common.go transport/websocket_tls_test.go && git commit -m "transport: choose ws/wss per connection from the CID transport

The WS leg picked its scheme from cfg.TLS and tagged every received packet
with one endpoint-wide name. objproto keys connections by the full
ConnectionID, so a server without cfg.TLS labelled wss peers \"ws\", one
endpoint could not reach both ws and wss peers, and a wss: dial with no TLS
config went out in plaintext.

Now a dial uses the destination CID's scheme, an accepted connection is
labelled wss when its request arrived over TLS, and a wss: dial with
TLS nil fails instead of falling back. cfg.TLS means the client config
for wss dials. The wasm build follows the same rule.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 2: Land objtrsf and bump the harness

**Files:**
- Modify: harness `go.mod`, `go.sum`

**Interfaces:**
- Consumes: Task 1's commit on objtrsf `origin/main`.
- Produces: the harness builds against the new objtrsf. Tasks 3-6 depend on it.

- [ ] **Step 1: Check the other consumers are unaffected**

Run: `grep -rn -E 'TLS:|"wss"' /home/kforfk/workspace/kcdn_ansible/ksdk/cmd /home/kforfk/workspace/kcdn_ansible/kscale/client /home/kforfk/workspace/kcdn_ansible/kscale/cmd --include=*.go | grep -v _test`
Expected: only `kscale/client/endpoint.go`, which sets `TLS` exactly when `cid.Transport == "wss"`. That is compatible with the new meaning. Any other hit: stop and report it.

- [ ] **Step 2: Fast-forward objtrsf main and push**

```bash
cd /home/kforfk/workspace/objtrsf && git fetch -q origin && git switch main && git merge --ff-only wss-scheme-per-connection && git merge-base --is-ancestor origin/main HEAD && git push origin HEAD:main && git log --oneline -1
```
Expected: the push succeeds, and the last line shows Task 1's commit.

- [ ] **Step 3: Bump the harness**

```bash
cd /home/kforfk/workspace/remote-agent-harness/.harness-worktrees/70fbad4a6eb6f1e992be8a669f1bcefd && GOPROXY=direct go get github.com/on-keyday/objtrsf@main && go mod tidy && git diff --stat
```
Expected: only `go.mod` and `go.sum` change, and the objtrsf pseudo-version names Task 1's commit.

- [ ] **Step 4: Verify that nothing regresses before any harness change**

Run: `cd /home/kforfk/workspace/remote-agent-harness/.harness-worktrees/70fbad4a6eb6f1e992be8a669f1bcefd && make wasm-check check vet test`
Expected: exit 0. No harness caller sets `cfg.TLS`, so ws behaviour must not change.

- [ ] **Step 5: Commit**

```bash
cd /home/kforfk/workspace/remote-agent-harness/.harness-worktrees/70fbad4a6eb6f1e992be8a669f1bcefd && git rev-parse --abbrev-ref HEAD && git add go.mod go.sum && git commit -m "deps: objtrsf with per-connection ws/wss scheme

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: harness — server `Config.TLS`, client helper, native dial sites

**Files:**
- Create: `cli/tls_client.go`
- Modify: `cli/dial_endpoint_native.go:24-28`
- Modify: `runner/connect.go:929-948` (the `legs.ws && legs.udp` and `legs.ws` cases)
- Modify: `server/server.go:37-45` (`Config`), `server/server.go:955-964` (listen)
- Create: `integration/wss_e2e_test.go`

**Interfaces:**
- Consumes: Task 2's objtrsf.
- Produces:
  - `func cli.ClientTLSConfig() *tls.Config` (build tag `!js`).
  - `server.Config.TLS *tls.Config`. nil means plaintext; non-nil means `ListenAndServeTLS` with it.
  - An integration helper `loopbackTLSConfig(t) *tls.Config`, used again only inside `integration/`.

- [ ] **Step 1: Write the failing integration test**

Create `integration/wss_e2e_test.go`:

```go
//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/server"
	"github.com/on-keyday/objtrsf/objproto"
)

// loopbackTLSConfig is a throwaway self-signed certificate for 127.0.0.1.
// Clients skip verification (cli.ClientTLSConfig), so only the handshake
// matters; nothing is written to disk.
func loopbackTLSConfig(t *testing.T) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "harness-wss-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1)},
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
}

func startWSSServer(t *testing.T) objproto.ConnectionID {
	t.Helper()
	addr := freePort(t)
	cid, err := objproto.ParseConnectionID("wss:"+addr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse server cid: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := server.New(server.Config{Addr: addr, DataDir: t.TempDir(), TLS: loopbackTLSConfig(t)})
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
		}
	})
	time.Sleep(300 * time.Millisecond)
	return cid
}

// The whole wss path in one pass: the CLI and a runner reach a TLS server
// over wss:, a task runs, and the agent is handed a wss: HARNESS_SERVER_CID
// (read off the live connection, so it is wss: only if the per-connection
// label reached the runner).
func TestWSS_CLIRunnerAndAgentCID(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}
	clearAgentEnv(t)
	cid := startWSSServer(t)

	agent := filepath.Join(t.TempDir(), "print-server-cid.sh")
	if err := os.WriteFile(agent, []byte("#!/bin/bash\necho \"server_cid=$HARNESS_SERVER_CID\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	repo := tempRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	runnerDone := make(chan error, 1)
	go func() {
		runnerDone <- runner.Run(ctx, runner.Config{
			RunnerID:         runner.NewRunnerID(),
			ServerCandidates: runner.CandidatesOf(cid),
			AllowedRoots:     []string{repo},
			MaxTasks:         1,
			Hostname:         "wss-runner-host",
			Profiles:         singleAgentProfile(agent),
		})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runnerDone:
		case <-time.After(3 * time.Second):
		}
	})

	c, err := cli.Dial(context.Background(), cid, protocol.ClientKind_Cli)
	if err != nil {
		t.Fatalf("cli.Dial(wss): %v", err)
	}
	defer c.Close()
	if !waitForRegisteredRunner(t, c, repo, 10*time.Second) {
		t.Fatal("runner did not register over wss within 10s")
	}

	id := mustSubmit(t, c, repo, "print the server cid")
	waitTaskTerminal(t, c, id, 15*time.Second)
	if ti := getTask(t, c, id); ti.Status != protocol.TaskStatus_Succeeded {
		t.Fatalf("task status = %v, want Succeeded", ti.Status)
	}
	var logs bytes.Buffer
	if err := c.Logs(context.Background(), id, &logs, false); err != nil {
		t.Fatalf("logs: %v", err)
	}
	if !strings.Contains(logs.String(), "server_cid=wss:127.0.0.1:") {
		t.Errorf("agent's HARNESS_SERVER_CID is not wss:; logs:\n%s", logs.String())
	}
}
```

- [ ] **Step 2: Run it and confirm it fails to compile for the right reason**

Run: `cd /home/kforfk/workspace/remote-agent-harness/.harness-worktrees/70fbad4a6eb6f1e992be8a669f1bcefd && go vet -tags integration ./integration/`
Expected: FAIL with `unknown field TLS in struct literal of type server.Config`.

- [ ] **Step 3: Add `Config.TLS` and the TLS listen in `server/server.go`**

In `type Config struct`, directly after the `UDPAddr` line (line 39), add:

```go
	TLS           *tls.Config   // non-nil: the WS/WebUI listener serves wss:// and https:// with it; nil: plaintext
```

Add `"crypto/tls"` to the import block. Replace the listen goroutine (lines 959-965, inside `if mux != nil && httpAddr != ""`) with:

```go
		httpServer = &http.Server{Addr: httpAddr, Handler: mux, TLSConfig: s.cfg.TLS}
		serverDone = make(chan error, 1)
		go func() {
			var err error
			if s.cfg.TLS != nil {
				// Certificates come from TLSConfig, so no file arguments.
				err = httpServer.ListenAndServeTLS("", "")
			} else {
				err = httpServer.ListenAndServe()
			}
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				serverDone <- err
				return
			}
			serverDone <- nil
		}()
```

Do not touch `buildEndpoint` (lines 682-730). The server's own WS leg keeps `TLS` nil, because it dials listen-mode runners over `ws:` (spec §2).

- [ ] **Step 4: Create `cli/tls_client.go`**

```go
//go:build !js

package cli

import "crypto/tls"

// ClientTLSConfig is the TLS config for dialing a wss: peer. objtrsf consults
// it only for a wss: destination, so ws: dials are unaffected.
//
// It does not verify the server certificate, and that is safe here: TLS on
// this path exists for browser compatibility (an HTTPS WebUI), not security.
// Peer authentication is objproto's PSK handshake and confidentiality is its
// AEAD, so a man in the middle who terminates TLS still cannot complete the
// objproto handshake without the PSK. Verifying would also need a hostname,
// and a ConnectionID carries only an address.
func ClientTLSConfig() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}
}
```

- [ ] **Step 5: Pass it at the native dial sites**

`cli/dial_endpoint_native.go`, the `case "ws", "wss":` literal becomes:

```go
		ep, err := transport.WebSocketEndpoint(nil, transport.WebSocketConfig{
			Logger: slog.Default(),
			Path:   WebSocketPath,
			TLS:    ClientTLSConfig(),
			Mode:   objproto.EndpointModeClient,
		})
```

`runner/connect.go`, in both the `legs.ws && legs.udp` case (`WS: transport.WebSocketConfig{…}`) and the `legs.ws` case, add the same field:

```go
			TLS:    cli.ClientTLSConfig(),
```

`cli` is already imported there (`cli.WebSocketPath`). Leave `cli/dial_endpoint_js.go` and `runner/listen.go` unchanged (spec §3 table). Then confirm that the sites are exhaustive:

Run: `cd /home/kforfk/workspace/remote-agent-harness/.harness-worktrees/70fbad4a6eb6f1e992be8a669f1bcefd && git grep -n 'WebSocketConfig{' -- '*.go' ':!*_test.go'`
Expected: exactly the 8 sites in spec §3's table. `cli/dial_endpoint_native.go` and the 2 `runner/connect.go` sites now carry `TLS:`. Any site not in the table: stop and report it.

- [ ] **Step 6: Run the integration test and the suites**

Run: `cd /home/kforfk/workspace/remote-agent-harness/.harness-worktrees/70fbad4a6eb6f1e992be8a669f1bcefd && go test -tags integration ./integration/ -run 'TestWSS_|TestUDPRunner_|TestDualStackServer_|TestServerCandidate' -count=1 -v -timeout 300s`
Expected: `TestWSS_CLIRunnerAndAgentCID` PASSes, and so do the ws/udp neighbours (the regression check).

Then run: `make wasm-check check vet test`
Expected: exit 0.

- [ ] **Step 7: Commit**

```bash
cd /home/kforfk/workspace/remote-agent-harness/.harness-worktrees/70fbad4a6eb6f1e992be8a669f1bcefd && git rev-parse --abbrev-ref HEAD && git add cli/tls_client.go cli/dial_endpoint_native.go runner/connect.go server/server.go integration/wss_e2e_test.go && git commit -m "wss: server TLS listener and a skip-verify client config

server.Config.TLS serves the WS/WebUI listener over TLS. The CLI, TUI and
runner pass cli.ClientTLSConfig(), which objtrsf consults only for a wss:
destination. The integration test drives a CLI, a runner and a task over
wss and checks that the agent is handed a wss: HARNESS_SERVER_CID.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: `harness-server --tls-cert` / `--tls-key`

**Files:**
- Modify: `cmd/harness-server/main.go` (the flag block at lines 31-54, a new `loadListenTLS`, the `server.New` literal at line 230)
- Test: `cmd/harness-server/main_test.go`

**Interfaces:**
- Consumes: `server.Config.TLS` (Task 3).
- Produces: `func loadListenTLS(certFile, keyFile, wsListen string) (*tls.Config, error)`.

- [ ] **Step 1: Write the failing tests**

Append to `cmd/harness-server/main_test.go`. Add `crypto/ecdsa`, `crypto/elliptic`, `crypto/rand`, `crypto/x509`, `crypto/x509/pkix`, `encoding/pem`, `math/big`, `path/filepath`, `strings` and `time` to its imports if they are missing.

```go
// writeCertPair writes a throwaway self-signed PEM pair and returns the paths.
func writeCertPair(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "harness-server-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile
}

func TestLoadListenTLS_NeitherFlagMeansPlaintext(t *testing.T) {
	cfg, err := loadListenTLS("", "", "127.0.0.1:8539")
	if err != nil || cfg != nil {
		t.Fatalf("got (%v, %v), want (nil, nil)", cfg, err)
	}
}

func TestLoadListenTLS_OneWithoutTheOtherIsAnError(t *testing.T) {
	certFile, keyFile := writeCertPair(t)
	for _, c := range []struct{ cert, key string }{{certFile, ""}, {"", keyFile}} {
		if _, err := loadListenTLS(c.cert, c.key, "127.0.0.1:8539"); err == nil || !strings.Contains(err.Error(), "together") {
			t.Errorf("cert=%q key=%q: err = %v, want a 'together' error", c.cert, c.key, err)
		}
	}
}

func TestLoadListenTLS_NeedsTheWSListener(t *testing.T) {
	certFile, keyFile := writeCertPair(t)
	if _, err := loadListenTLS(certFile, keyFile, ""); err == nil || !strings.Contains(err.Error(), "--listen") {
		t.Fatalf("err = %v, want a --listen error", err)
	}
}

func TestLoadListenTLS_UnreadablePairIsAnError(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.pem")
	if _, err := loadListenTLS(missing, missing, "127.0.0.1:8539"); err == nil {
		t.Fatal("a missing certificate loaded")
	}
}

func TestLoadListenTLS_ValidPair(t *testing.T) {
	certFile, keyFile := writeCertPair(t)
	cfg, err := loadListenTLS(certFile, keyFile, "127.0.0.1:8539")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Certificates) != 1 {
		t.Fatalf("certificates = %d, want 1", len(cfg.Certificates))
	}
}
```

- [ ] **Step 2: Run them and confirm they fail**

Run: `cd /home/kforfk/workspace/remote-agent-harness/.harness-worktrees/70fbad4a6eb6f1e992be8a669f1bcefd && go test ./cmd/harness-server/ -run LoadListenTLS -count=1`
Expected: FAIL with `undefined: loadListenTLS`.

- [ ] **Step 3: Implement**

In the flag block of `cmd/harness-server/main.go`, directly after the `udpListen` line, add:

```go
	tlsCert              = flag.String("tls-cert", "", "PEM certificate for the --listen WebSocket/WebUI listener; with --tls-key the server serves wss:// and https:// (clients do not verify it: objproto authenticates the peer)")
	tlsKey               = flag.String("tls-key", "", "PEM private key for --tls-cert")
```

Add `"crypto/tls"` to the imports. Add the function next to `resolveOperatorPSK`:

```go
// loadListenTLS turns --tls-cert / --tls-key into the listener's TLS config,
// or nil for plaintext. It runs before the listener starts: the listen call
// runs in a goroutine, so a bad certificate left to ListenAndServeTLS would
// surface only after startup.
func loadListenTLS(certFile, keyFile, wsListen string) (*tls.Config, error) {
	if certFile == "" && keyFile == "" {
		return nil, nil
	}
	if certFile == "" || keyFile == "" {
		return nil, errors.New("--tls-cert and --tls-key must be given together")
	}
	if wsListen == "" {
		return nil, errors.New("--tls-cert/--tls-key need --listen: TLS applies only to the WebSocket listener")
	}
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load --tls-cert/--tls-key: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}, nil
}
```

In `main`, directly after the operator-PSK error check that follows line 198, add (this matches the `PSK setup failed` handling at lines 188-192):

```go
	listenTLS, err := loadListenTLS(strings.TrimSpace(*tlsCert), strings.TrimSpace(*tlsKey), strings.TrimSpace(*listen))
	if err != nil {
		slog.Error("TLS setup failed", "err", err)
		os.Exit(1)
	}
```

and in the `server.New(server.Config{…})` literal, after `UDPAddr:`, add:

```go
		TLS:                  listenTLS,
```

- [ ] **Step 4: Run the tests**

Run: `go test ./cmd/harness-server/ -count=1 && make check vet`
Expected: PASS, then exit 0.

- [ ] **Step 5: Commit**

```bash
cd /home/kforfk/workspace/remote-agent-harness/.harness-worktrees/70fbad4a6eb6f1e992be8a669f1bcefd && git rev-parse --abbrev-ref HEAD && git add cmd/harness-server/main.go cmd/harness-server/main_test.go && git commit -m "harness-server: --tls-cert / --tls-key

Both or neither; either one without --listen is an error; the pair is loaded
before the listener starts so a bad certificate stops startup.

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: `dummy-harness up --tls` and the docs

**Files:**
- Modify: `scripts/dummy-harness.py` (`cmd_up` at line 387, `main` at line 550)
- Modify: `README.md:131-137` (transport paragraph), `README.md:183-193` (server start example)
- Modify: every skill or doc line found by Step 4's grep that enumerates CID transports

**Interfaces:**
- Consumes: Task 4's flags.
- Produces: `scripts/dummy-harness.sh up --tls`, which starts the server with a throwaway loopback certificate. Its `env` CID and runner CID are `wss:`.

- [ ] **Step 1: Add `--tls` to `scripts/dummy-harness.py`**

First run `grep -n '^import\|^from' scripts/dummy-harness.py` and add `import shutil` if it is absent.

Add this function above `cmd_up`:

```python
def make_loopback_cert(tmp: Path) -> tuple[Path, Path]:
    """A throwaway self-signed pair for 127.0.0.1, for `up --tls`. Clients do
    not verify it (cli.ClientTLSConfig); a browser needs its warning clicked
    through once, or `thisisunsafe` typed on Chromium's interstitial."""
    openssl = shutil.which("openssl")
    if not openssl:
        setup_err("--tls needs openssl on PATH to mint a throwaway certificate")
    cert, key = tmp / "tls-cert.pem", tmp / "tls-key.pem"
    subprocess.run(
        [openssl, "req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:prime256v1",
         "-nodes", "-days", "1", "-subj", "/CN=harness-dummy",
         "-addext", "subjectAltName=IP:127.0.0.1",
         "-keyout", str(key), "-out", str(cert)],
        check=True, capture_output=True,
    )
    return cert, key
```

Change the signature of `cmd_up` to add `tls: bool` after `udp: bool`:

```python
def cmd_up(name: str, agent: str, model: str, detach: bool, udp: bool, tls: bool,
           extra: list[str], server_args: list[str]) -> int:
```

Replace `cid = f"ws:127.0.0.1:{port}-*"` (line 403) with:

```python
    cid = f"{'wss' if tls else 'ws'}:127.0.0.1:{port}-*"
```

Directly after `repo.mkdir(parents=True)` (the line after `data = tmp / "data"`), add:

```python
    if tls:
        cert, key = make_loopback_cert(tmp)
        server_args = server_args + ["--tls-cert", str(cert), "--tls-key", str(key)]
```

In the `print(f"dummy-harness: up  name=…")` line, add `tls={tls}` after `port={port}`.

In `main`, after the `--udp` argument, add:

```python
    p.add_argument("--tls", action="store_true",
                   help="serve wss:// + https:// with a throwaway self-signed cert (needs openssl)")
```

and change the `cmd_up(...)` call to:

```python
        return cmd_up(args.name, args.agent, args.model, args.detach, args.udp, args.tls, extra,
                      args.server_arg)
```

- [ ] **Step 2: Check that the existing script tests still pass**

Run: `python3 scripts/test_dummy_harness.py`
Expected: `OK`.

- [ ] **Step 3: Update README**

In the transport paragraph (`README.md:131-137`), after the sentence that ends `(WS+UDP dualstack) on a single server.`, insert:

```markdown
The WebSocket underlay can run over TLS: start the server with
`--tls-cert FILE --tls-key FILE` (PEM) and address it as `wss:HOST:PORT-*`.
The WebUI on the same listener is then served over `https://`. Clients do
not verify the certificate, because objproto's PSK handshake already
authenticates the peer. TLS here is for browsers, not security.
```

After the `# bin/harness-server --listen :8539 --udp-listen :8540 …` example line (`README.md:193`), add:

```bash
# Optional: serve wss:// and an https:// WebUI (runners then use --server-cid 'wss:HOST:8539-*').
# bin/harness-server --listen :8539 --tls-cert cert.pem --tls-key key.pem --data-dir ./harness-data
```

- [ ] **Step 4: Find other text that lists the transports**

Run: `cd /home/kforfk/workspace/remote-agent-harness/.harness-worktrees/70fbad4a6eb6f1e992be8a669f1bcefd && git grep -n -E '\bws(s)?:[A-Za-z0-9<$]|ws, wss|ws or udp|ws/udp' -- '*.md' 'runner/agentskills/**' '.claude/skills/**' ':!docs/superpowers/**'`
For each hit that ENUMERATES the accepted transports (as opposed to showing one example CID), add `wss` with a pointer to `--tls-cert`. Example CIDs like `ws:HOSTNAME:8539-*` stay as they are. Record each file you change in the commit message.

- [ ] **Step 5: Walk the surface-parity checklist**

Invoke the `surface-parity-checklist` skill and walk its numbered items against this change: two new server flags, `wss:` accepted by the CLI, TUI and runner `--server-cid`, and a wss CID built by the WebUI. Record a verdict for every item number (implemented / not applicable because … / omitted because …) in the commit message body. Fix any item that comes out "missing".

- [ ] **Step 6: Commit**

```bash
cd /home/kforfk/workspace/remote-agent-harness/.harness-worktrees/70fbad4a6eb6f1e992be8a669f1bcefd && git rev-parse --abbrev-ref HEAD && git add scripts/dummy-harness.py README.md && git add -u && git status --short && git commit -m "docs, dummy-harness: wss / --tls

<list the files changed in Step 4 and the checklist verdicts from Step 5>

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```
(Replace the angle-bracket line with the actual content before committing. `git status --short` must show only files this task changed.)

---

### Task 6: Live E2E, then land

**Files:** none changed unless a defect is found. A defect gets fixed in the task that owns it, with its own commit.

- [ ] **Step 1: Stand up a TLS dummy**

```bash
cd /home/kforfk/workspace/remote-agent-harness/.harness-worktrees/70fbad4a6eb6f1e992be8a669f1bcefd && make build && scripts/dummy-harness.sh up --detach --name wss --agent fake --tls
```
Expected: `dummy-harness: up  name=wss  agent=fake  port=<P>  tls=True …`. `up` only returns after `harness-cli ls` over `wss:` shows the runner, so this line alone proves CLI and runner over wss.

- [ ] **Step 2: Check the CLI and the agent-side harness-cli over wss**

```bash
eval "$(scripts/dummy-harness.sh env --name wss)" && echo "$CID" && harness-cli --server-cid "$CID" ls && \
harness-cli --server-cid "$CID" submit --repo "$REPO" --agent bash --task 'echo "cid=$HARNESS_SERVER_CID"; harness-cli ls && echo agent-ls-ok'
```
Then follow the task with `harness-cli --server-cid "$CID" logs <task-id>`.
Expected: `$CID` starts with `wss:127.0.0.1:`. The task log shows `cid=wss:127.0.0.1:…` and `agent-ls-ok`.

- [ ] **Step 3: WebUI over https in Playwright**

Navigate the Playwright MCP browser to `https://127.0.0.1:$SERVER_PORT/#psk=$HARNESS_PSK`. If auto mode refuses to build this URL, ask the operator to paste it (memory `project_playwright_webui_visual_check`). On Chromium's certificate interstitial, focus the page and type `thisisunsafe`. Then:
- `browser_snapshot`: the status shows connected, and the Tasks tab lists the Step 2 task.
- `browser_evaluate '() => [location.protocol, window.isSecureContext]'` → `["https:", true]`.
- `browser_take_screenshot` with `filename: "wss-webui.png"`. Report the path the tool prints and keep the file (UI screenshots are deliverables).

If the interstitial cannot be passed, stop and ask the operator. The alternative, starting the MCP browser with `--ignore-https-errors`, is a config change of theirs.

- [ ] **Step 4: Plaintext still works**

```bash
scripts/dummy-harness.sh up --detach --name plain --agent fake && eval "$(scripts/dummy-harness.sh env --name plain)" && harness-cli --server-cid "$CID" ls
```
Expected: `$CID` starts with `ws:` and `ls` shows the runner.

- [ ] **Step 5: Tear down**

```bash
scripts/dummy-harness.sh down --name wss; scripts/dummy-harness.sh down --name plain
```

- [ ] **Step 6: Full verification**

Run: `make wasm-check check vet test && make test-integration`
Expected: exit 0. The server-flake family in memory (`project_flaky_test_open_interactive_sessionmux`, ~1/6 package runs) can fail unrelated tests. Before calling a failure a flake, rerun only the failing test and read its output.

- [ ] **Step 7: Land (Mode A) and build**

Use the `landing-to-main` skill. The harness feature set is Tasks 2-5's commits plus the spec and plan commits. Rebase onto current `main` if it moved, fast-forward local `main` in `/home/kforfk/workspace/remote-agent-harness`, and `git push origin main` (never force). Then run `make build` in the main checkout.

- [ ] **Step 8: Report**

Tell the operator:
- what landed (commit range in both repos);
- the E2E results with the screenshot path;
- that a real server is unchanged until they pass `--tls-cert` / `--tls-key`. After they do, runners must use `wss:` in `--server-cid` (a `ws:` dial at a TLS server fails, which is the spec's accepted failure mode).
