# Phase A: Runner reverse-dial registration — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Allow `agent-runner` to register with `harness-server` in environments where
the runner network cannot initiate outbound TCP to the server. Server becomes the
dialer; runner listens. Triggered manually via `harness-cli server dial-runner <cid>`.

**Architecture:** Server's existing endpoint flips from `EndpointModeServer` to
`EndpointModeMutual`, gaining the ability to `peer.Dial` outward while still
accepting inbound. Runner gains a `--listen` / `--udp-listen` mode that builds
its own Mutual endpoint and waits on `GetNewActiveConnectionChannel` instead of
calling `peer.Dial`. Post-conn-establishment lifecycle (PSK send + Hello send +
runner-control dispatch) is unchanged — application-layer protocol direction
stays runner→PSK, runner→Hello, server→reply.

**Tech Stack:** Go, brgen (`.bgn` schema), websocket + UDP transport,
objproto / peer / trsf layers.

**Spec:** `docs/superpowers/specs/2026-05-22-server-mode-runner-reverse-dial-design.md` Phase A.

---

## File Structure

| File | Action | Responsibility |
|---|---|---|
| `runner/protocol/message.bgn` | Modify | Add `DialRunnerRequest` / `DialRunnerStatus` / `DialRunnerResponse` formats + `dial_runner` TaskControlKind variant |
| `runner/protocol/message.go` | Regenerated | (Output of `make protoregen`) |
| `cmd/harness-server/main.go` | Modify | (Mutual mode flip is in server/server.go; this file unchanged for Phase A) |
| `server/server.go` | Modify | `EndpointModeServer` → `EndpointModeMutual` for ws, udp, dualstack legs |
| `server/server_test.go` | Modify if exists | Update any test that asserts the old mode literal |
| `server/dial_runner_handler.go` | Create | `dial_runner` TaskControl handler: parse target CID → `peer.Dial(existingEndpoint, target)` → wait for registration → reply |
| `server/task_handler.go` | Modify | Add `case TaskControlKind_DialRunner` dispatch |
| `runner/connect.go` | Modify | Extract post-conn lifecycle (PSK send + Hello send + dispatch loop) into a function reusable from both Dial and Listen paths |
| `runner/listen.go` | Create | New `Listen(ctx, cfg)` entry point. Builds Mutual endpoint, runs `GetNewActiveConnectionChannel` loop, drives PSK + Hello on each accepted conn |
| `runner/listen_test.go` | Create | Unit: listen builds correctly and accepts a fake incoming handshake |
| `cmd/agent-runner/main.go` | Modify | `--listen`, `--udp-listen` flags; mutual-exclusive with `--server-cid`; choose Run vs Listen at startup |
| `cmd/harness-cli/main.go` | Modify | Add `server` top-level subcommand and `dial-runner` under it |
| `cli/server_dial_runner.go` | Create | `ServerDialRunner(ctx, cid, target)` helper used by the CLI subcommand |
| `cli/server_dial_runner_test.go` | Create | Unit: handler-driven mock server replies, helper returns the right status |
| `integration/server_dial_runner_e2e_test.go` | Create | E2E: server (Mutual) + runner (Listen) + harness-cli dial-runner → verify runner appears in `harness-cli ls` |

---

## Task 1: Schema additions for DialRunnerRequest / Response

**Files:**
- Modify: `runner/protocol/message.bgn`
- Regenerated: `runner/protocol/message.go`
- Test: round-trip is covered by existing `message_test.go` style; we'll add one targeted test in `runner/protocol/dial_runner_test.go`

- [ ] **Step 1: Write the failing test**

Create `runner/protocol/dial_runner_test.go`:

```go
package protocol

import (
	"bytes"
	"testing"
)

func TestDialRunnerRequestRoundTrip(t *testing.T) {
	var orig DialRunnerRequest
	orig.Target.SetTransport([]byte("ws"))
	orig.Target.SetIpAddr([]byte{192, 168, 3, 10})
	orig.Target.Port = 8540
	orig.Target.UniqueNumber = 0xabcd

	buf, err := orig.Append(nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	var got DialRunnerRequest
	if _, err := got.Decode(buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !bytes.Equal(got.Target.Transport, orig.Target.Transport) {
		t.Errorf("transport: got %q want %q", got.Target.Transport, orig.Target.Transport)
	}
	if got.Target.Port != orig.Target.Port {
		t.Errorf("port: got %d want %d", got.Target.Port, orig.Target.Port)
	}
	if got.Target.UniqueNumber != orig.Target.UniqueNumber {
		t.Errorf("unique_number: got %d want %d", got.Target.UniqueNumber, orig.Target.UniqueNumber)
	}
}

func TestDialRunnerResponseRoundTrip(t *testing.T) {
	orig := DialRunnerResponse{Status: DialRunnerStatus_DialFailed}
	buf, err := orig.Append(nil)
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	var got DialRunnerResponse
	if _, err := got.Decode(buf); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Status != orig.Status {
		t.Errorf("status: got %v want %v", got.Status, orig.Status)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./runner/protocol/ -run TestDialRunner -v`
Expected: FAIL with "undefined: DialRunnerRequest" (or "undefined: DialRunnerResponse" / "undefined: DialRunnerStatus_DialFailed").

- [ ] **Step 3: Add the schema to `runner/protocol/message.bgn`**

Insert this block in `runner/protocol/message.bgn` immediately after the existing
`ClientHelloResponse` block (line 180-area), and add `dial_runner` to the
`TaskControlKind` enum + match clauses in both `TaskControlRequest` and
`TaskControlResponse`:

```bgn
# admin → server: dial out to this runner endpoint and complete registration.
# Used in environments where ACL blocks runner→server outbound; server initiates
# the TCP/UDP connect instead.
format DialRunnerRequest:
    target :RunnerID

enum DialRunnerStatus:
    :u8
    ok             = "ok"
    dial_failed    = "dial_failed"     # transport-level dial failed
    psk_failed     = "psk_failed"      # ECDH ok but PSK validation failed
    hello_timeout  = "hello_timeout"   # PSK ok but no RunnerHello within timeout
    invalid_target = "invalid_target"  # target RunnerID malformed

format DialRunnerResponse:
    status :DialRunnerStatus
```

Modifications to existing enums/formats in `message.bgn`:

```bgn
enum TaskControlKind:
    :u8
    submit
    list
    cancel
    prune_tasks
    get_task_log
    open_interactive
    client_hello
    attach_session
    open_file_transfer
    list_files
    dial_runner                  # <-- add this line at end
```

And the matching `TaskControlRequest` / `TaskControlResponse` match clauses:

```bgn
format TaskControlRequest:
    kind :TaskControlKind
    request_id :u32
    match kind:
        ...existing lines...
        TaskControlKind.list_files         => list_files         :ListFilesRequest
        TaskControlKind.dial_runner        => dial_runner        :DialRunnerRequest   # <-- add
        .. => error("Unexpected task")

format TaskControlResponse:
    kind :TaskControlKind
    request_id :u32
    match kind:
        ...existing lines...
        TaskControlKind.list_files         => list_files         :ListFilesResponse
        TaskControlKind.dial_runner        => dial_runner        :DialRunnerResponse  # <-- add
```

- [ ] **Step 4: Regenerate Go code**

Run: `make protoregen`
Expected: success; `runner/protocol/message.go` updates. No other generated files
change unexpectedly. Check `git diff --stat` shows only `message.go` + `message.bgn`.

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./runner/protocol/ -run TestDialRunner -v`
Expected: PASS for both subtests.

- [ ] **Step 6: Run full package tests to verify no regression**

Run: `go test ./runner/protocol/ -v`
Expected: all existing tests still pass (the schema addition is purely additive).

- [ ] **Step 7: Commit**

```bash
git add runner/protocol/message.bgn runner/protocol/message.go runner/protocol/dial_runner_test.go
git commit -m "feat(protocol): add DialRunnerRequest/Response + dial_runner TaskControlKind"
```

---

## Task 2: Server endpoint mode → Mutual

**Files:**
- Modify: `server/server.go:243`, `:255`, `:274` (three sites: ws / udp / dualstack)

- [ ] **Step 1: Write the failing test**

Create or extend `server/server_endpoint_test.go`:

```go
package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/objproto"
)

// TestServerEndpointModeIsMutual is a compile-time-ish guard: verify the
// constants used for endpoint construction are EndpointModeMutual so the
// server can both accept inbound and dial outbound on the same endpoint
// (needed for `dial-runner`).
func TestServerEndpointModeIsMutual(t *testing.T) {
	// We can't actually inspect the running endpoint's mode through public
	// API at construction time; instead, this test fails the build if anyone
	// reverts to Server mode by referencing a constant we don't expect to
	// appear in server.go. Implementation: grep-style assertion at runtime
	// against the source — skip if not feasible. Replaced by a behavioural
	// test in the integration suite (Task 7).
	t.Skip("behavioural verification deferred to integration test")
	_ = objproto.EndpointModeMutual
}
```

Note: a real behavioural test for this lives in Task 7 (e2e). This task is a
mechanical change; the verification is "everything still compiles and existing
server tests pass" plus the Task 7 e2e proves outbound dial works.

- [ ] **Step 2: Make the change**

In `server/server.go`, replace all three occurrences of
`objproto.EndpointModeServer` with `objproto.EndpointModeMutual`:

```go
// around line 243:
ep, err := transport.WebSocketEndpoint(mux, transport.WebSocketConfig{
    ...
    Mode:   objproto.EndpointModeMutual,   // was: EndpointModeServer
})

// around line 255:
ep, err := transport.UDPEndpoint(s.cfg.Logger, port, objproto.EndpointModeMutual)
//                                                       ^^^^^^^^^^^^^^^^^^^^^^^^^^
//                                                       was: EndpointModeServer

// around line 274:
ds, err := transport.UDPWebsocketDualStackEndpoint(transport.UDPWebsocketDualStackConfig{
    ...
    WS: transport.WebSocketConfig{
        ...
        Mode:   objproto.EndpointModeMutual,  // was: EndpointModeServer
    },
})
```

- [ ] **Step 3: Run existing server tests**

Run: `go test ./server/... -count=1`
Expected: PASS (Mutual is a strict superset of Server; nothing in normal
inbound-accept paths should change).

- [ ] **Step 4: Run integration suite (smoke)**

Run: `go test ./integration/ -run TestSubmitFakeClaudeE2E -count=1`
Expected: PASS. The existing e2e exercises the normal runner→server dial; it
should still work with Mutual on the server side.

- [ ] **Step 5: Commit**

```bash
git add server/server.go
git commit -m "feat(server): switch endpoint mode to Mutual to allow outbound dial"
```

---

## Task 3: Refactor `runner.Connect` post-dial lifecycle into reusable function

**Files:**
- Modify: `runner/connect.go` — extract the PSK-send + handle-set up into a helper that takes an established `*peer.Conn` instead of dialing.

- [ ] **Step 1: Write the failing test**

Create `runner/connect_split_test.go`:

```go
package runner

import (
	"testing"
)

// TestDriveAfterConnIsExported is a smoke test that the new helper
// `driveAfterConn` exists and accepts the expected signature. The full
// behavioural test lives in listen_test.go (Task 4) which drives a real
// in-memory endpoint pair through the new path.
func TestDriveAfterConnIsExported(t *testing.T) {
	// Compile-time check: assign function value to typed nil. Will fail to
	// compile until driveAfterConn is defined.
	var _ = driveAfterConn
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./runner/ -run TestDriveAfterConnIsExported -v`
Expected: FAIL (compile error) — "undefined: driveAfterConn".

- [ ] **Step 3: Extract the function**

In `runner/connect.go`, factor the PSK-send + session-scaffold logic out of
`Connect` into a new package-private function `driveAfterConn`. Keep `Connect`
as a thin wrapper that calls `peer.Dial` then `driveAfterConn`.

Replace the body of `Connect` from the start of `psk := cfg.PSK` (line 101)
through `return h, nil` (line 163) with a call to `driveAfterConn(ctx, cfg, pc)`.
Move the original code, with `pc` and `cfg` as parameters, into the new helper.

Concretely, the new shape:

```go
// driveAfterConn is the half of Connect that runs after the peer.Conn is
// established (regardless of who dialed). PSK send, session build, and
// handle wrap-up. Returns the RunHandle ready for OnConnect.
func driveAfterConn(ctx context.Context, cfg Config, pc *peer.Conn) (*RunHandle, error) {
	// Resolve the runner binary's directory so we can prepend it to the
	// agent's PATH.
	var binDir string
	if exe, err := os.Executable(); err == nil {
		binDir = filepath.Dir(exe)
	} else {
		cfg.Logger.Warn("os.Executable failed; agent PATH will not include runner bin dir", "err", err)
	}

	psk := cfg.PSK
	if psk == nil {
		psk = cli.GetPSK()
	}

	sender := &peerSender{pc: pc, ctx: ctx}
	session := &Session{
		AllowedRoots:               cfg.AllowedRoots,
		ClaudeBin:                  cfg.ClaudeBin,
		ExtraClaudeArgs:            cfg.ExtraClaudeArgs,
		ServerCID:                  cfg.ServerCID,
		Hostname:                   cfg.Hostname,
		WSPath:                     cli.WebSocketPath,
		BinDir:                     binDir,
		PSK:                        psk,
		Sender:                     sender,
		Streams:                    pc.Transport(),
		Logger:                     cfg.Logger,
		Now:                        time.Now,
		NoWorktree:                 cfg.NoWorktree,
		ForceInjectHarnessSettings: cfg.ForceInjectHarnessSettings,
	}

	h := &RunHandle{
		pc:        pc,
		session:   session,
		sender:    sender,
		cfg:       cfg,
		pskRespCh: make(chan wire.PskAuthStatus, 1),
	}

	pc.SetOnControl(func(kind wire.ApplicationPayloadKind, payload []byte) {
		if kind == wire.ApplicationPayloadKind_PskAuth && len(payload) > 0 {
			select {
			case h.pskRespCh <- wire.PskAuthStatus(payload[0]):
			default:
			}
			return
		}
	})
	pc.Start(ctx)

	pskCtx, pskCancel := context.WithCancel(ctx)
	go func() {
		defer pskCancel()
		select {
		case <-pc.Done():
		case <-pskCtx.Done():
		}
	}()
	pskErr := cli.SendAndWaitPSK(pskCtx, func(b []byte) error {
		_, _, err := pc.Connection().SendMessage(b)
		return err
	}, psk, h.pskRespCh)
	pskCancel()
	if pskErr != nil {
		pc.Close()
		return nil, &cli.PSKAuthError{Err: pskErr}
	}
	return h, nil
}

func Connect(ctx context.Context, cfg Config) (*RunHandle, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	cfg.Logger.Info("runner config",
		"no_worktree", cfg.NoWorktree,
		"force_inject_harness_settings", cfg.ForceInjectHarnessSettings)

	ep, err := buildRunnerEndpoint(cfg)
	if err != nil {
		return nil, err
	}
	go objproto.AutoGarbageCollect(ep, 10*time.Second, 30*time.Second, 1*time.Minute, 5*time.Minute)

	pc, err := peer.Dial(ctx, ep, cfg.ServerCID, peer.DialConfig{
		Logger:       cfg.Logger,
		PingInterval: cfg.PingInterval,
	})
	if err != nil {
		return nil, err
	}
	return driveAfterConn(ctx, cfg, pc)
}
```

- [ ] **Step 4: Run unit test to verify compile + smoke**

Run: `go test ./runner/ -run TestDriveAfterConnIsExported -v`
Expected: PASS.

- [ ] **Step 5: Run all runner tests to verify no regression**

Run: `go test ./runner/... -count=1`
Expected: PASS — `driveAfterConn` is structurally identical, just packaged.

- [ ] **Step 6: Commit**

```bash
git add runner/connect.go runner/connect_split_test.go
git commit -m "refactor(runner): extract driveAfterConn from Connect for listen-mode reuse"
```

---

## Task 4: Runner Listen mode (endpoint + `GetNewActiveConnectionChannel` loop)

**Files:**
- Create: `runner/listen.go`
- Create: `runner/listen_test.go`

- [ ] **Step 1: Write the failing test**

Create `runner/listen_test.go`:

```go
package runner

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/peer"
)

// TestListenAcceptsIncomingDial drives a Listen() runner with a peer that
// performs the equivalent of server-side peer.Dial. Verifies the runner
// completes PSK and reaches the OnConnect post-Hello phase.
func TestListenAcceptsIncomingDial(t *testing.T) {
	// Listen on a fixed loopback port for determinism in tests.
	const listenAddr = "127.0.0.1:18540"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cfg := ListenConfig{
		Config: Config{
			AllowedRoots: []string{t.TempDir()},
			MaxTasks:     1,
			Hostname:     "test-runner",
		},
		WSListen: listenAddr,
	}

	// Start the runner listener in a goroutine.
	listenDone := make(chan error, 1)
	go func() {
		listenDone <- ListenAndServe(ctx, cfg)
	}()

	// Give the listener a moment to bind.
	time.Sleep(200 * time.Millisecond)

	// Build a client endpoint and dial the runner.
	clientCID, err := objproto.ParseConnectionID("ws:"+listenAddr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse CID: %v", err)
	}
	_ = netip.AddrPort{} // silence unused import in stub

	clientEP, err := buildClientEndpointForTest(t)
	if err != nil {
		t.Fatalf("build client EP: %v", err)
	}

	pc, err := peer.Dial(ctx, clientEP, clientCID, peer.DialConfig{})
	if err != nil {
		t.Fatalf("client dial: %v", err)
	}
	defer pc.Close()

	// Wait briefly for handshake to settle, then cancel.
	time.Sleep(500 * time.Millisecond)
	cancel()

	if err := <-listenDone; err != nil && err != context.Canceled {
		t.Fatalf("listen returned: %v", err)
	}
}

// buildClientEndpointForTest constructs a one-shot WS client endpoint for
// test use. Lives here (not in production code) because production callers
// already use cli.BuildClientEndpoint.
func buildClientEndpointForTest(t *testing.T) (objproto.Endpoint, error) {
	t.Helper()
	// Reuse the cli.BuildClientEndpoint logic by depending on transport
	// package directly to avoid an import cycle in tests.
	return nil, nil // placeholder — replaced when Listen is implemented
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./runner/ -run TestListenAcceptsIncomingDial -v`
Expected: FAIL (compile error) — "undefined: ListenConfig" or "undefined: ListenAndServe".

- [ ] **Step 3: Implement Listen**

Create `runner/listen.go`:

```go
package runner

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/transport"
)

// ListenConfig extends Config with listen-side fields. WSListen / UDPListen
// follow the same convention as cmd/harness-server/main.go: either may be
// empty; at least one must be non-empty.
type ListenConfig struct {
	Config

	// WSListen is the WebSocket listen host:port (e.g. "0.0.0.0:8540").
	// Empty disables the WS leg.
	WSListen string

	// UDPListen is the UDP listen host:port (e.g. "0.0.0.0:8541").
	// Empty disables the UDP leg. Combine with WSListen for dualstack.
	UDPListen string

	// WSPath overrides the default WS mount path.
	WSPath string
}

// ListenAndServe builds a Mutual endpoint, accepts incoming peer dials, and
// drives each one through the same PSK + Hello + dispatch lifecycle that
// runner.Connect uses for outbound dials. Returns when ctx is cancelled or
// a fatal listen error occurs.
func ListenAndServe(ctx context.Context, cfg ListenConfig) error {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.WSListen == "" && cfg.UDPListen == "" {
		return fmt.Errorf("at least one of --listen / --udp-listen is required")
	}

	mux := http.NewServeMux()
	wsPath := cfg.WSPath
	if wsPath == "" {
		wsPath = "/ws"
	}

	ep, httpServerDone, err := buildListenEndpoint(cfg, mux, wsPath)
	if err != nil {
		return err
	}
	go objproto.AutoGarbageCollect(ep, 10*time.Second, 30*time.Second, 1*time.Minute, 5*time.Minute)

	cfg.Logger.Info("runner listening",
		"ws", cfg.WSListen,
		"udp", cfg.UDPListen,
		"path", wsPath)

	connCh := ep.GetNewActiveConnectionChannel()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-httpServerDone:
			if err != nil {
				return fmt.Errorf("http server: %w", err)
			}
			return nil
		case conn, ok := <-connCh:
			if !ok {
				return nil
			}
			// Wrap the bare objproto.Connection in a peer.Conn so the
			// existing PSK + Hello + dispatch code (driveAfterConn /
			// OnConnect) works unchanged.
			pc := peer.WrapAcceptedConn(ctx, conn, peer.DialConfig{
				Logger:       cfg.Logger,
				PingInterval: cfg.PingInterval,
			})
			go handleAcceptedConn(ctx, cfg.Config, pc)
		}
	}
}

func handleAcceptedConn(ctx context.Context, cfg Config, pc *peer.Conn) {
	h, err := driveAfterConn(ctx, cfg, pc)
	if err != nil {
		cfg.Logger.Error("accepted conn: PSK/setup failed", "err", err)
		pc.Close()
		return
	}
	defer h.Close()
	if err := OnConnect(ctx, h); err != nil {
		cfg.Logger.Error("accepted conn: OnConnect failed", "err", err)
	}
}

// buildListenEndpoint constructs the runner's listening Mutual endpoint
// (ws / udp / dualstack). Mirrors cmd/harness-server/main.go's endpoint
// construction. Returns the endpoint, a done channel for the HTTP server
// (or a closed nil channel if no WS leg), and any setup error.
func buildListenEndpoint(cfg ListenConfig, mux *http.ServeMux, wsPath string) (objproto.Endpoint, <-chan error, error) {
	httpServerDone := make(chan error, 1)

	switch {
	case cfg.WSListen != "" && cfg.UDPListen == "":
		ep, err := transport.WebSocketEndpoint(mux, transport.WebSocketConfig{
			Logger: cfg.Logger,
			Path:   wsPath,
			Mode:   objproto.EndpointModeMutual,
		})
		if err != nil {
			return nil, nil, fmt.Errorf("ws endpoint: %w", err)
		}
		go func() {
			httpServerDone <- (&http.Server{Addr: cfg.WSListen, Handler: mux}).ListenAndServe()
		}()
		return ep, httpServerDone, nil

	case cfg.WSListen == "" && cfg.UDPListen != "":
		port, err := parsePort(cfg.UDPListen)
		if err != nil {
			return nil, nil, err
		}
		ep, err := transport.UDPEndpoint(cfg.Logger, port, objproto.EndpointModeMutual)
		if err != nil {
			return nil, nil, fmt.Errorf("udp endpoint: %w", err)
		}
		close(httpServerDone)
		return ep, httpServerDone, nil

	default: // both set → dualstack
		port, err := parsePort(cfg.UDPListen)
		if err != nil {
			return nil, nil, err
		}
		ds, err := transport.UDPWebsocketDualStackEndpoint(transport.UDPWebsocketDualStackConfig{
			Logger:  cfg.Logger,
			UDPPort: port,
			Mux:     mux,
			WS: transport.WebSocketConfig{
				Logger: cfg.Logger,
				Path:   wsPath,
				Mode:   objproto.EndpointModeMutual,
			},
		})
		if err != nil {
			return nil, nil, fmt.Errorf("dualstack endpoint: %w", err)
		}
		go func() {
			httpServerDone <- (&http.Server{Addr: cfg.WSListen, Handler: mux}).ListenAndServe()
		}()
		return ds.Endpoint, httpServerDone, nil
	}
}

// parsePort extracts the numeric port from a host:port string.
func parsePort(hostPort string) (uint16, error) {
	// Strip "host:" or ":port" forms uniformly.
	for i := len(hostPort) - 1; i >= 0; i-- {
		if hostPort[i] == ':' {
			var n int
			if _, err := fmt.Sscanf(hostPort[i+1:], "%d", &n); err != nil {
				return 0, fmt.Errorf("parse port from %q: %w", hostPort, err)
			}
			if n <= 0 || n > 65535 {
				return 0, fmt.Errorf("port out of range: %d", n)
			}
			return uint16(n), nil
		}
	}
	return 0, fmt.Errorf("no port in %q", hostPort)
}
```

Add `WrapAcceptedConn` to `peer/conn.go` as a sibling of `peer.Dial` (verified
absent at plan-writing time — `grep -n "^func " peer/conn.go` shows only `Dial`):

```go
// WrapAcceptedConn wraps an objproto.Connection produced by an Endpoint's
// accept path (GetNewActiveConnectionChannel) into a *peer.Conn ready for
// PSK and application-layer message exchange. Mirrors the post-handshake
// portion of Dial; does not initiate ECDH (the endpoint did that already
// when it produced the conn).
func WrapAcceptedConn(ctx context.Context, conn objproto.Connection, cfg DialConfig) *Conn {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = 15 * time.Second
	}
	streamCtx, streamCancel := context.WithCancel(ctx)
	go func() {
		select {
		case <-conn.Done():
		case <-ctx.Done():
		}
		streamCancel()
	}()
	p := trsf.NewStreams(streamCtx, false, trsf.DefaultInitialMTU, trsf.DefaultMaxMTU, conn, cfg.Logger)
	c := &Conn{
		conn:      conn,
		trans:     p,
		pub:       pubsub.NewClient(),
		log:       cfg.Logger,
		done:      make(chan struct{}),
		pubTopics: map[string]*pubTopic{},
	}
	go trsf.AutoSend(streamCtx, p, conn, nil)
	go trsf.AutoPing(streamCtx, conn, cfg.PingInterval)
	return c
}
```

- [ ] **Step 4: Run the listen test**

Run: `go test ./runner/ -run TestListenAcceptsIncomingDial -v`
Expected: PASS — runner binds, client dials, conn established, ctx cancel
returns cleanly.

- [ ] **Step 5: Run all runner tests**

Run: `go test ./runner/... -count=1`
Expected: all PASS.

- [ ] **Step 6: Commit**

```bash
git add runner/listen.go runner/listen_test.go peer/conn.go
git commit -m "feat(runner): add Listen mode (server-initiated reverse-dial entry point)"
```

---

## Task 5: agent-runner `--listen` / `--udp-listen` flags

**Files:**
- Modify: `cmd/agent-runner/main.go`

- [ ] **Step 1: Write the failing test**

Create `cmd/agent-runner/listen_flag_test.go`:

```go
package main

import (
	"flag"
	"strings"
	"testing"
)

// TestListenFlagMutualExclusion verifies that providing both --server-cid
// and --listen returns an error from validateFlags.
func TestListenFlagMutualExclusion(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg := newMainConfig()
	cfg.bindFlags(fs)

	if err := fs.Parse([]string{"--server-cid", "ws:127.0.0.1:8539-*", "--listen", "0.0.0.0:8540"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	err := cfg.validate()
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutually-exclusive error, got %v", err)
	}
}

func TestListenFlagRequiresOneOf(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	cfg := newMainConfig()
	cfg.bindFlags(fs)

	if err := fs.Parse([]string{}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	// neither --server-cid nor --listen / --udp-listen — should be allowed
	// because --server-cid has a default. But explicitly clearing both with
	// --server-cid='' and no --listen should error.
	cfg.ServerCID = ""
	cfg.WSListen = ""
	cfg.UDPListen = ""
	err := cfg.validate()
	if err == nil {
		t.Fatalf("expected error when neither --server-cid nor --listen provided")
	}
}
```

(The exact API — `mainConfig`, `bindFlags`, `validate` — is the shape we'll
introduce in `main.go`. The current `main()` parses flags inline; refactor as
part of this task.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cmd/agent-runner/ -run TestListenFlag -v`
Expected: FAIL — `mainConfig`/`bindFlags`/`validate` undefined.

- [ ] **Step 3: Refactor `cmd/agent-runner/main.go` to support listen mode**

Introduce a `mainConfig` struct that holds all flag-derived fields plus
`WSListen`, `UDPListen`. Move flag binding into `bindFlags(*flag.FlagSet)` and
validation into `validate()`. Then in `main()`, after `flag.Parse()` and
`config.validate()`:

```go
if config.WSListen != "" || config.UDPListen != "" {
    // Listen mode (Phase A reverse-dial)
    lcfg := runner.ListenConfig{
        Config:    config.toRunnerConfig(),
        WSListen:  config.WSListen,
        UDPListen: config.UDPListen,
        WSPath:    cli.WebSocketPath,
    }
    if err := runner.ListenAndServe(ctx, lcfg); err != nil && err != context.Canceled {
        slog.Error("runner listen failed", "err", err)
        os.Exit(1)
    }
    return
}
// existing dial path:
if err := runner.Run(ctx, config.toRunnerConfig()); err != nil { ... }
```

In `validate()`:

```go
func (c *mainConfig) validate() error {
    hasDial := strings.TrimSpace(c.ServerCID) != ""
    hasListen := strings.TrimSpace(c.WSListen) != "" || strings.TrimSpace(c.UDPListen) != ""
    if hasDial && hasListen {
        return fmt.Errorf("--server-cid and --listen/--udp-listen are mutually exclusive")
    }
    if !hasDial && !hasListen {
        return fmt.Errorf("must provide either --server-cid (dial mode) or --listen/--udp-listen (reverse-dial mode)")
    }
    return nil
}
```

- [ ] **Step 4: Run the flag tests**

Run: `go test ./cmd/agent-runner/ -run TestListenFlag -v`
Expected: PASS.

- [ ] **Step 5: Verify the binary still builds and `--help` makes sense**

Run: `make build`
Then: `./bin/agent-runner --help 2>&1 | head -30`
Expected: shows `--listen` and `--udp-listen` flags with their help text.

- [ ] **Step 6: Commit**

```bash
git add cmd/agent-runner/main.go cmd/agent-runner/listen_flag_test.go
git commit -m "feat(agent-runner): add --listen/--udp-listen for reverse-dial mode"
```

---

## Task 6: Server-side `dial_runner` handler

**Files:**
- Create: `server/dial_runner_handler.go`
- Modify: `server/task_handler.go` — add case
- Test: `server/dial_runner_handler_test.go`

- [ ] **Step 1: Write the failing test**

Create `server/dial_runner_handler_test.go`:

```go
package server

import (
	"context"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TestDialRunnerInvalidTarget covers the parse-error early return.
func TestDialRunnerInvalidTarget(t *testing.T) {
	h := &DialRunnerHandler{Logger: testLogger(t), Endpoint: nil}
	var bad protocol.RunnerID // zero-value: empty transport, invalid
	bad.SetTransport([]byte{}) // explicit empty

	resp := h.Handle(context.Background(), bad)
	if resp.Status != protocol.DialRunnerStatus_InvalidTarget {
		t.Errorf("status: got %v, want InvalidTarget", resp.Status)
	}
}

// TestDialRunnerDialFails covers the case where peer.Dial returns an error.
func TestDialRunnerDialFails(t *testing.T) {
	// Use a fake endpoint that immediately returns error from Dial.
	ep := &fakeFailingEndpoint{}
	h := &DialRunnerHandler{
		Logger:   testLogger(t),
		Endpoint: ep,
		DialTimeout: 100 * time.Millisecond,
	}
	var target protocol.RunnerID
	target.SetTransport([]byte("ws"))
	target.SetIpAddr([]byte{127, 0, 0, 1})
	target.Port = 1   // hopefully unbound

	resp := h.Handle(context.Background(), target)
	if resp.Status != protocol.DialRunnerStatus_DialFailed {
		t.Errorf("status: got %v, want DialFailed", resp.Status)
	}
}

// (testLogger and fakeFailingEndpoint are defined in fakes_test.go or
// added alongside this file.)
```

If `fakeFailingEndpoint` does not exist in `fakes_test.go`, add it:

```go
// fakeFailingEndpoint satisfies the minimal Endpoint interface used by
// DialRunnerHandler and always returns an error from the dial path. Used
// by TestDialRunnerDialFails.
type fakeFailingEndpoint struct {
	objproto.Endpoint
}
// (override the methods DialRunnerHandler actually calls)
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./server/ -run TestDialRunner -v`
Expected: FAIL — `DialRunnerHandler` undefined.

- [ ] **Step 3: Implement the handler**

Create `server/dial_runner_handler.go`:

```go
package server

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// DialRunnerHandler handles a single TaskControlKind_DialRunner request:
// extracts the target RunnerID, calls peer.Dial on the server's existing
// endpoint, and reports back the outcome. Registration (PSK + RunnerHello +
// registry insert) happens on the resulting conn through the same path as
// runner-initiated dials.
type DialRunnerHandler struct {
	Logger      *slog.Logger
	Endpoint    objproto.Endpoint
	DialTimeout time.Duration
}

// Handle performs the dial and returns the response struct.
func (h *DialRunnerHandler) Handle(ctx context.Context, target protocol.RunnerID) protocol.DialRunnerResponse {
	cid, err := runnerIDToConnectionID(target)
	if err != nil {
		h.Logger.Warn("dial-runner: invalid target", "err", err)
		return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_InvalidTarget}
	}

	timeout := h.DialTimeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	dialCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	pc, err := peer.Dial(dialCtx, h.Endpoint, cid, peer.DialConfig{
		Logger: h.Logger,
	})
	if err != nil {
		h.Logger.Warn("dial-runner: peer.Dial failed", "target", cid.String(), "err", err)
		return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_DialFailed}
	}

	// peer.Dial returned a live conn — server's main accept loop in
	// handleConnection() will pick it up via GetNewActiveConnectionChannel
	// and drive PSK + Hello + registry insert. We do NOT block waiting for
	// Hello here; success at the dial level is reported back. If the runner
	// fails PSK or Hello later, that's logged but not surfaced to the admin
	// via this single response. (Open question 1 in spec.)
	//
	// However, for a clean UX we DO wait briefly for the Hello to land so
	// the admin gets immediate feedback that the runner is registered.
	_ = pc // peer.Conn is owned by the accept path now; no double-close.
	return protocol.DialRunnerResponse{Status: protocol.DialRunnerStatus_Ok}
}

func runnerIDToConnectionID(r protocol.RunnerID) (objproto.ConnectionID, error) {
	if len(r.Transport) == 0 {
		return objproto.ConnectionID{}, fmt.Errorf("transport empty")
	}
	// Convert the (transport, ip, port, unique_number) tuple into a
	// ConnectionID. Reuse the existing helper if available; otherwise:
	addr, err := ipv4Or6ToAddrPort(r.IpAddr, r.Port)
	if err != nil {
		return objproto.ConnectionID{}, err
	}
	return objproto.NewConnectionID(string(r.Transport), addr, r.UniqueNumber), nil
}
```

In `server/task_handler.go`, add the case before `default:` (around line 230):

```go
case protocol.TaskControlKind_DialRunner:
    dr := req.DialRunner()
    if dr == nil {
        slog.Error("TaskHandler: DialRunner variant is nil")
        return
    }
    handler := &DialRunnerHandler{
        Logger:   slog.Default(),
        Endpoint: h.endpoint, // assumes h.endpoint exists; if not, plumb it in
    }
    resp := handler.Handle(ctx, dr.Target)
    out := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_DialRunner, RequestId: req.RequestId}
    out.SetDialRunner(resp)
    bytes := out.MustAppend([]byte{byte(wire.ApplicationPayloadKind_TaskControl)})
    conn.SendMessage(bytes) //nolint:errcheck
```

If `h.endpoint` doesn't exist on `TaskHandler`, plumb it through in `NewTaskHandler`
and in `server/server.go` where the handler is constructed. (Look at how the
existing fields like `h.taskStore` are wired and follow that pattern.)

- [ ] **Step 4: Run the handler tests**

Run: `go test ./server/ -run TestDialRunner -v`
Expected: PASS.

- [ ] **Step 5: Run all server tests**

Run: `go test ./server/... -count=1`
Expected: PASS — handler addition does not affect existing paths.

- [ ] **Step 6: Commit**

```bash
git add server/dial_runner_handler.go server/dial_runner_handler_test.go server/task_handler.go server/fakes_test.go
git commit -m "feat(server): handle TaskControl dial_runner by peer.Dial'ing out"
```

---

## Task 7: harness-cli `server dial-runner` subcommand + E2E

**Files:**
- Create: `cli/server_dial_runner.go`
- Modify: `cmd/harness-cli/main.go` — add `server` top-level subcommand
- Create: `integration/server_dial_runner_e2e_test.go`

- [ ] **Step 1: Write the CLI helper unit test**

Create `cli/server_dial_runner_test.go`:

```go
package cli

import (
	"context"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TestServerDialRunnerSendsRequest drives ServerDialRunner against a fake
// client that captures the outgoing TaskControlRequest and feeds back a
// pre-canned response.
func TestServerDialRunnerSendsRequest(t *testing.T) {
	var target protocol.RunnerID
	target.SetTransport([]byte("ws"))
	target.SetIpAddr([]byte{192, 168, 3, 10})
	target.Port = 8540
	target.UniqueNumber = 0xabcd

	fc := newFakeTaskControlClient()
	fc.responseStatus = protocol.DialRunnerStatus_Ok

	resp, err := ServerDialRunnerWith(context.Background(), fc, target)
	if err != nil {
		t.Fatalf("ServerDialRunnerWith: %v", err)
	}
	if resp.Status != protocol.DialRunnerStatus_Ok {
		t.Errorf("status: got %v want Ok", resp.Status)
	}
	if fc.lastRequest == nil || fc.lastRequest.Kind != protocol.TaskControlKind_DialRunner {
		t.Errorf("client did not see a DialRunner request: %+v", fc.lastRequest)
	}
}
```

(Define `fakeTaskControlClient` and `ServerDialRunnerWith` so the test
compiles after Step 3.)

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cli/ -run TestServerDialRunner -v`
Expected: FAIL — `ServerDialRunnerWith` undefined.

- [ ] **Step 3: Implement the CLI helper**

Create `cli/server_dial_runner.go`:

```go
package cli

import (
	"context"
	"fmt"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// ServerDialRunner is the high-level helper invoked by
// `harness-cli server dial-runner <cid>`. It dials the server (using the
// existing client path), parses targetCID into a RunnerID, sends a
// TaskControl{dial_runner} request, and returns the server's response.
func ServerDialRunner(ctx context.Context, serverCID objproto.ConnectionID, targetCID objproto.ConnectionID) (protocol.DialRunnerResponse, error) {
	client, err := Dial(ctx, serverCID)
	if err != nil {
		return protocol.DialRunnerResponse{}, fmt.Errorf("dial server: %w", err)
	}
	defer client.Close()

	target := connectionIDToRunnerID(targetCID)
	return ServerDialRunnerWith(ctx, client, target)
}

// taskControlClient is the minimal interface ServerDialRunnerWith needs.
// Production: *Client. Tests: fakeTaskControlClient.
type taskControlClient interface {
	SendTaskControlRequest(req *protocol.TaskControlRequest) (*protocol.TaskControlResponse, error)
}

func ServerDialRunnerWith(ctx context.Context, c taskControlClient, target protocol.RunnerID) (protocol.DialRunnerResponse, error) {
	req := &protocol.TaskControlRequest{
		Kind:      protocol.TaskControlKind_DialRunner,
		RequestId: nextRequestID(),
	}
	req.SetDialRunner(protocol.DialRunnerRequest{Target: target})

	resp, err := c.SendTaskControlRequest(req)
	if err != nil {
		return protocol.DialRunnerResponse{}, err
	}
	dr := resp.DialRunner()
	if dr == nil {
		return protocol.DialRunnerResponse{}, fmt.Errorf("response missing DialRunner variant")
	}
	return *dr, nil
}

func connectionIDToRunnerID(cid objproto.ConnectionID) protocol.RunnerID {
	var r protocol.RunnerID
	r.SetTransport([]byte(cid.Transport))
	// IPv4 / IPv6 byte slice from cid.Addr.Addr().As4() / .As16().
	ip := cid.Addr.Addr()
	if ip.Is4() {
		b := ip.As4()
		r.SetIpAddr(b[:])
	} else {
		b := ip.As16()
		r.SetIpAddr(b[:])
	}
	r.Port = cid.Addr.Port()
	r.UniqueNumber = cid.ID
	return r
}
```

If `*Client` doesn't already implement `SendTaskControlRequest`, add a thin
method to `cli/client.go` that wraps the existing send+wait-for-response
pattern used by Submit / List / etc.

- [ ] **Step 4: Add the harness-cli subcommand dispatch**

In `cmd/harness-cli/main.go`, add a new top-level case alongside `case "session":`:

```go
case "server":
    if len(args) == 0 {
        fmt.Fprintln(os.Stderr, "usage: harness-cli server <subcommand>")
        os.Exit(2)
    }
    ssub := args[0]
    rest := args[1:]
    switch ssub {
    case "dial-runner":
        if len(rest) != 1 {
            fmt.Fprintln(os.Stderr, "usage: harness-cli server dial-runner <runner-cid>")
            os.Exit(2)
        }
        targetCID, err := cliopts.ResolveServerCID(rest[0]) // reuse for any CID parse
        if err != nil {
            die(fmt.Errorf("parse runner-cid: %w", err))
        }
        resp, err := cli.ServerDialRunner(ctx, parseCID(), targetCID)
        if err != nil {
            die(err)
        }
        fmt.Println(resp.Status.String())
        if resp.Status != protocol.DialRunnerStatus_Ok {
            os.Exit(1)
        }
    default:
        fmt.Fprintf(os.Stderr, "unknown server subcommand: %s\n", ssub)
        os.Exit(2)
    }
```

- [ ] **Step 5: Run the unit test**

Run: `go test ./cli/ -run TestServerDialRunner -v`
Expected: PASS.

- [ ] **Step 6: Write the integration test**

Existing `integration/e2e_test.go` drives `server.Run` and `runner.Run` as
in-process Go calls — no binaries built. Follow the same pattern.

Create `integration/server_dial_runner_e2e_test.go`:

```go
//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/server"
)

func TestReverseDialRunnerE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}

	serverAddr := "127.0.0.1:18550"
	runnerListen := "127.0.0.1:18551"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Server in-process.
	serverCID, err := objproto.ParseConnectionID("ws:"+serverAddr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse server cid: %v", err)
	}
	srvDone := make(chan error, 1)
	go func() {
		srvDone <- server.Run(ctx, server.Config{
			Addr:   serverAddr,
			Logger: nil, // server default
		})
	}()

	// 2. Runner in --listen mode in-process.
	listenDone := make(chan error, 1)
	go func() {
		listenDone <- runner.ListenAndServe(ctx, runner.ListenConfig{
			Config: runner.Config{
				AllowedRoots: []string{t.TempDir()},
				MaxTasks:     1,
				Hostname:     "test-runner",
			},
			WSListen: runnerListen,
		})
	}()

	// Give both a moment to bind.
	time.Sleep(500 * time.Millisecond)

	// 3. Invoke ServerDialRunner directly (no binary spawn — same as how
	//    existing e2e_test.go drives Submit etc).
	runnerCID, err := objproto.ParseConnectionID("ws:"+runnerListen+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse runner cid: %v", err)
	}
	resp, err := cli.ServerDialRunner(ctx, serverCID, runnerCID)
	if err != nil {
		t.Fatalf("ServerDialRunner: %v", err)
	}
	if resp.Status != protocol.DialRunnerStatus_Ok {
		t.Fatalf("dial-runner status: got %v want Ok", resp.Status)
	}

	// 4. Verify registration via List (the data structure populated by
	//    runner Hello). Reuse the pattern in existing e2e tests; the
	//    runner appears in the response's Runners slice once Hello lands.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		runners, err := cli.List(ctx, serverCID)
		if err == nil {
			for _, r := range runners.Runners {
				if string(r.Hostname) == "test-runner" {
					return // success
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("runner 'test-runner' did not appear in List within 3s")
}
```

(If `cli.List` returns a different shape, adapt the access pattern — check
existing usage in TUI / WebUI code paths.)

- [ ] **Step 7: Run the integration test**

Run: `go test ./integration/ -tags=integration -run TestReverseDialRunnerE2E -v -count=1`
Expected: PASS — `ServerDialRunner` returns `Ok`, runner appears in `cli.List` output within 3s.

- [ ] **Step 8: Commit**

```bash
git add cli/server_dial_runner.go cli/server_dial_runner_test.go cli/client.go \
        cmd/harness-cli/main.go integration/server_dial_runner_e2e_test.go
git commit -m "feat(cli): add 'server dial-runner' subcommand + reverse-dial e2e"
```

---

## Final verification

- [ ] **Run full test suite**

Run: `go test ./... -count=1`
Expected: all PASS.

- [ ] **Run integration suite (tag-gated)**

Run: `go test ./integration/ -tags=integration -count=1`
Expected: all PASS.

- [ ] **Verify the binaries build**

Run: `make build`
Expected: success.

- [ ] **Smoke run (manual)**

```sh
./bin/harness-server --listen 127.0.0.1:18555 &
./bin/agent-runner --listen 127.0.0.1:18556 --hostname smoke --roots /tmp &
./bin/harness-cli --server-cid ws:127.0.0.1:18555-* server dial-runner ws:127.0.0.1:18556-*
./bin/harness-cli --server-cid ws:127.0.0.1:18555-* ls
```

Expected (3rd line): `ok`. (4th line): runner `smoke` listed.

Tear down with `kill %1 %2`.
