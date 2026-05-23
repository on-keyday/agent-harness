//go:build integration

// Reproduce the chained-relay-missing bug.
//
// Topology:
//
//	server (127.0.0.1:18650)
//	   ▲   ▲
//	   │   │  Phase A direct reverse-dial   ─── proxy_runner (dial mode)
//	   │   │                                    │
//	   │   │  Phase C relay setup via proxy_runner
//	   │   │  for target_runner (listen mode on 127.0.0.1:18651)
//	   │   │
//	   │   └─── (proxy_runner forwards server↔target packets at slot=50699-ish)
//	   │
//	   └── after Phase C, target_runner is registered. Its `Session.ServerCID`
//	       is its view of the "server peer", which is actually proxy_runner's
//	       addr (Phase C is transparent — target doesn't know it's relayed).
//
// Bug scenario: an agent process running on the target_runner host runs
// `harness-cli ls` (or any cli.Dial-using subcommand). The agent has
// HARNESS_PROXY_VIA_RUNNER=ws:127.0.0.1:18651-* (target's listen addr,
// loopback-rewritten). cli.DialPeerConn detects the env and calls
// cli.DialViaProxy(target, taskID).
//
// target_runner's runAgentProxyCeremony does:
//
//	allocate := (target.serverCID.Transport, target.serverCID.Addr, agentCID.ID)
//	ep.SetProxy(agentCID, allocate)
//
// target.serverCID.Addr is the proxy_runner addr (Phase C made target see proxy
// as its server). When the agent rehandshakes, target forwards the packet to
// proxy_runner at a NEW slot_id (= agentCID.ID, NOT the Phase C slot 50699).
//
// proxy_runner sees a Handshake at the new slot_id, no expectedRelays entry
// matches, watchIncomingActiveConns closes it. The agent's rehandshake never
// completes; cli.DialViaProxy returns "waiting for rehandshake: context
// deadline exceeded".
//
// Expected outcome of this test: cli.Dial fails / times out. This is a red
// test pinning the current limitation — multi-hop relay is not implemented.
// When chained relay is added (Phase D), this test should be promoted to a
// green test by removing the t.Expect-fail assertion.

package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/server"
)

func TestChainedRelayMissing(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}

	const (
		serverAddr   = "127.0.0.1:18650"
		targetListen = "127.0.0.1:18651"
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Start server.
	srv := server.New(server.Config{
		Addr:    serverAddr,
		DataDir: t.TempDir(),
	})
	srvDone := make(chan error, 1)
	go func() { srvDone <- srv.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)

	serverCID, err := objproto.ParseConnectionID("ws:"+serverAddr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse server cid: %v", err)
	}

	// 2. Start proxy_runner (dial mode) → registers with server directly via
	//    the legacy dial flow (no Phase A reverse-dial needed since the
	//    runner reaches the server outbound).
	proxyCfg := runner.Config{
		ServerCID:    serverCID,
		AllowedRoots: []string{t.TempDir()},
		MaxTasks:     1,
		Hostname:     "chained-proxy",
	}
	proxyHandle, err := runner.Connect(ctx, proxyCfg)
	if err != nil {
		t.Fatalf("runner.Connect (proxy_runner): %v", err)
	}
	proxyOnConnectDone := make(chan error, 1)
	go func() { proxyOnConnectDone <- runner.OnConnect(ctx, proxyHandle) }()

	// 3. Start target_runner in listen mode.
	targetDone := make(chan error, 1)
	go func() {
		targetDone <- runner.ListenAndServe(ctx, runner.ListenConfig{
			Config: runner.Config{
				AllowedRoots: []string{t.TempDir()},
				MaxTasks:     1,
				Hostname:     "chained-target",
			},
			WSListen: targetListen,
		})
	}()
	time.Sleep(300 * time.Millisecond)

	// 4. Wait for proxy_runner to register, capture its CID for --via.
	proxyEntry := waitForRunnerByHostname(t, srv, "chained-proxy", 5*time.Second)
	proxyRegisteredCID, err := objproto.ParseConnectionID(proxyEntry.ID, 0)
	if err != nil {
		t.Fatalf("parse proxy registered CID %q: %v", proxyEntry.ID, err)
	}

	// 5. Phase C: register target_runner through proxy_runner.
	targetDialCID, err := objproto.ParseConnectionID("ws:"+targetListen+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse target cid: %v", err)
	}
	dialCtx, dialCancel := context.WithTimeout(ctx, 15*time.Second)
	resp, err := cli.ServerDialRunner(dialCtx, serverCID, targetDialCID, proxyRegisteredCID)
	dialCancel()
	if err != nil {
		t.Fatalf("Phase C dial target via proxy: %v", err)
	}
	if resp.Status != protocol.DialRunnerStatus_Ok {
		t.Fatalf("Phase C status: got %v want Ok", resp.Status)
	}

	// 6. Wait for target_runner to appear in the registry.
	_ = waitForRunnerByHostname(t, srv, "chained-target", 5*time.Second)

	// 7. Register a fake task on the target_runner so the agent_proxy
	//    ceremony's HasTask check passes.
	var taskID protocol.TaskID
	if _, err := rand.Read(taskID.Id[:]); err != nil {
		t.Fatal(err)
	}
	if err := runner.AddFakeTaskForListenServer(ctx, taskID); err != nil {
		t.Fatalf("AddFakeTaskForListenServer: %v", err)
	}

	// 8. Simulate the agent process running on target_runner host doing a
	//    cli.Dial (which would happen for `harness-cli ls` etc. with the
	//    new cli.DialPeerConn env-detection path).
	//
	// Env that the runner-spawned agent would have:
	//   HARNESS_PROXY_VIA_RUNNER=ws:127.0.0.1:18651-*  (loopback-rewritten target)
	//   HARNESS_TASK_ID=<hex of taskID>
	//
	// cli.DialPeerConn will detect the env and route via
	// cli.DialViaProxy(target, taskID). The Phase B ceremony succeeds on
	// the target side, but target's SetProxy allocate points to
	// target.serverCID.Addr — which after Phase C is proxy_runner's addr.
	// When the test's rehandshake packet arrives at proxy_runner, no
	// expectedRelays entry matches the new slot_id → watcher closes.
	// cli.DialViaProxy waits for rehandshake → times out.
	t.Setenv("HARNESS_PROXY_VIA_RUNNER", "ws:"+targetListen+"-*")
	t.Setenv("HARNESS_TASK_ID", hex.EncodeToString(taskID.Id[:]))

	dialCtx2, dialCancel2 := context.WithTimeout(ctx, 8*time.Second)
	defer dialCancel2()

	// cli.Dial may either:
	//   (a) fail outright (rehandshake timeout, proxy_runner closing the conn
	//       before HandshakeAck reaches the agent, PSK timeout, ...)
	//   (b) return ostensibly successfully — proxy_runner's Mutual mode
	//       happens to ECDH-respond to the agent's rehandshake before our
	//       watcher closes the activeConn, so the agent gets a "live"
	//       end-to-end peer.Conn ... with proxy_runner pretending to be the
	//       server. PSK exchange might trivially "pass" when no PSK is
	//       configured. The conn is unusable for any actual server operation.
	//
	// Either way, the resulting *cli.Client cannot perform a real operation
	// like cli.List against the SERVER, because the conn doesn't end-to-end
	// with the server — it terminates at proxy_runner. So the proper red
	// assertion is: an actual operation (List) on the dialed client fails
	// or times out.
	client, dialErr := cli.Dial(dialCtx2, serverCID)
	if dialErr != nil {
		t.Logf("cli.Dial through chained relay failed early (expected): %v", dialErr)
		return
	}
	defer client.Close()
	t.Logf("cli.Dial returned a client; verifying it's actually unusable for server ops...")

	listCtx, listCancel := context.WithTimeout(ctx, 5*time.Second)
	defer listCancel()
	var buf bytes.Buffer
	listErr := cli.List(listCtx, serverCID, &buf)
	if listErr == nil {
		t.Fatalf("cli.List unexpectedly succeeded through chained relay — chained relay is now supported? buf:\n%s", buf.String())
	}
	t.Logf("cli.List through chained relay failed as expected: %v", listErr)

	// Cleanup.
	cancel()
	for _, ch := range []chan error{srvDone, proxyOnConnectDone, targetDone} {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
		}
	}
}
