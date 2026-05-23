//go:build integration

// End-to-end relay test for the server→proxy_runner→target_runner path.
//
// Topology:
//
//	                   ┌─────────────────────────────────────────────────┐
//	                   │  server (Mutual mode, in-process, port 18630)   │
//	                   └────────┬────────────────────────────────────────┘
//	                            │
//	     Phase A (direct)       │  Phase B (relay)
//	     ─────────────────      │  ───────────────────────────────────────
//	  cli.ServerDialRunner      │  cli.ServerDialRunner(... via=proxyRID)
//	    → runnerCID=18631       │    → server sends EstablishRelayRequest
//	                            │    → proxy_runner SetProxy(target.CID)
//	                            │    → server RehandshakeForProxy → target
//	   proxy_runner (port 18631)│  target_runner (port 18632)
//
// Flow:
//  1. Start server.Run on port 18630.
//  2. Start proxy_runner in Listen mode on 18631.
//  3. Start target_runner in Listen mode on 18632.
//  4. Phase A: cli.ServerDialRunner(serverCID, proxyCID, {}) → proxy registers.
//  5. Wait for proxy_runner to appear in srv.RegisteredRunners().
//  6. Phase B: cli.ServerDialRunner(serverCID, targetCID, proxyRegisteredCID)
//     → target registers via relay.
//  7. Verify target_runner appears in srv.RegisteredRunners() with distinct ID.

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

func TestRelayE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}

	// Ports distinct from all other integration tests (see grep for 18NNN usage).
	const (
		serverAddr   = "127.0.0.1:18630"
		proxyListen  = "127.0.0.1:18631"
		targetListen = "127.0.0.1:18632"
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Start the server.
	srv := server.New(server.Config{
		Addr:    serverAddr,
		DataDir: t.TempDir(),
	})
	srvDone := make(chan error, 1)
	go func() { srvDone <- srv.Run(ctx) }()
	time.Sleep(300 * time.Millisecond) // bind grace

	// 2. Start proxy_runner in Listen mode.
	proxyDone := make(chan error, 1)
	go func() {
		proxyDone <- runner.ListenAndServe(ctx, runner.ListenConfig{
			Config: runner.Config{
				AllowedRoots: []string{t.TempDir()},
				MaxTasks:     1,
				Hostname:     "relay-proxy-runner",
			},
			WSListen: proxyListen,
		})
	}()
	time.Sleep(300 * time.Millisecond) // bind grace

	// 3. Start target_runner in Listen mode.
	targetDone := make(chan error, 1)
	go func() {
		targetDone <- runner.ListenAndServe(ctx, runner.ListenConfig{
			Config: runner.Config{
				AllowedRoots: []string{t.TempDir()},
				MaxTasks:     1,
				Hostname:     "relay-target-runner",
			},
			WSListen: targetListen,
		})
	}()
	time.Sleep(300 * time.Millisecond) // bind grace

	// Parse CIDs.
	serverCID, err := objproto.ParseConnectionID("ws:"+serverAddr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse server cid: %v", err)
	}
	proxyCID, err := objproto.ParseConnectionID("ws:"+proxyListen+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse proxy runner cid: %v", err)
	}
	targetCID, err := objproto.ParseConnectionID("ws:"+targetListen+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse target runner cid: %v", err)
	}

	// 4. Phase A: direct reverse-dial server → proxy_runner.
	dialCtx, dialCancel := context.WithTimeout(ctx, 10*time.Second)
	resp, err := cli.ServerDialRunner(dialCtx, serverCID, proxyCID, objproto.ConnectionID{})
	dialCancel()
	if err != nil {
		t.Fatalf("Phase A ServerDialRunner (proxy): %v", err)
	}
	if resp.Status != protocol.DialRunnerStatus_Ok {
		t.Fatalf("Phase A dial-runner status: got %v want Ok", resp.Status)
	}

	// 5. Wait for proxy_runner to appear in the registry; capture its registered CID.
	proxyEntry := waitForRunnerByHostname(t, srv, "relay-proxy-runner", 5*time.Second)
	t.Logf("proxy_runner registered: ID=%s", proxyEntry.ID)

	// Build the via CID from the registered entry ID (the objproto CID string
	// the server assigned to this connection).
	proxyRegisteredCID, err := objproto.ParseConnectionID(proxyEntry.ID, 0)
	if err != nil {
		t.Fatalf("parse proxy registered CID %q: %v", proxyEntry.ID, err)
	}

	// 6. Phase B: relay — server dials target_runner via proxy_runner.
	dialCtx2, dialCancel2 := context.WithTimeout(ctx, 15*time.Second)
	resp2, err := cli.ServerDialRunner(dialCtx2, serverCID, targetCID, proxyRegisteredCID)
	dialCancel2()
	if err != nil {
		t.Fatalf("Phase B ServerDialRunner (target via proxy): %v", err)
	}
	if resp2.Status != protocol.DialRunnerStatus_Ok {
		t.Fatalf("Phase B dial-runner status: got %v want Ok", resp2.Status)
	}

	// 7. Verify target_runner appears in the registry with a distinct connection ID.
	targetEntry := waitForRunnerByHostname(t, srv, "relay-target-runner", 5*time.Second)
	t.Logf("target_runner registered via relay: ID=%s", targetEntry.ID)

	// Sanity: proxy and target must have distinct registered IDs.
	if targetEntry.ID == proxyEntry.ID {
		t.Errorf("proxy and target share the same registered ID %q — relay did not establish a distinct conn", targetEntry.ID)
	}

	// Cleanup: cancel and drain (best-effort; timeout is non-fatal to mirror the
	// other E2E tests' teardown pattern).
	cancel()
	for _, ch := range []chan error{srvDone, proxyDone, targetDone} {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
		}
	}
}

// waitForRunnerByHostname polls srv.RegisteredRunners until a runner with the
// given hostname appears, then returns its RunnerEntry.
func waitForRunnerByHostname(t *testing.T, srv *server.Server, hostname string, timeout time.Duration) server.RunnerEntry {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, e := range srv.RegisteredRunners() {
			if e.Hostname == hostname {
				return e
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("runner %q did not appear in registry within %v", hostname, timeout)
	return server.RunnerEntry{}
}
