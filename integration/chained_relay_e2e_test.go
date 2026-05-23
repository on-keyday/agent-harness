//go:build integration

// 2-hop chained relay end-to-end test.
//
// Topology:
//
//	server (127.0.0.1:18650)
//	   ▲
//	   │  Phase A direct reverse-dial (dial mode)
//	   │  proxy_runner ("chained-proxy")
//	   │
//	   │  Phase C relay setup via proxy_runner
//	   │  for target_runner (listen mode on 127.0.0.1:18651)
//	   │  target_runner ("chained-target")
//	   │
//	   └── target is registered via proxy as a 2-hop chain.
//	       target.Session.ServerCID points at proxy_runner's addr
//	       (Phase C is transparent from target's perspective).
//
// Chained relay scenario: an agent process running on the target_runner host
// runs cli.Dial (simulating `harness-cli ls` or any subcommand).
// HARNESS_PROXY_VIA_RUNNER=ws:127.0.0.1:18651-* (target's listen addr,
// loopback-rewritten). cli.DialPeerConn detects the env and routes through
// cli.DialViaProxy(target, taskID).
//
// With chained relay (Phase D) implemented:
//   - target's runAgentProxyCeremony emits RequestChainedRelay to the server.
//   - Server walks target's Via chain (target → proxy → server).
//   - Server dispatches EstablishRelay to proxy for the agent slot.
//   - proxy installs eager SetProxy(owned=(proxy.serverCID.Addr, slot),
//     allocate=(target.Addr, slot)).
//   - target installs its own SetProxy(agentCID, (proxy.Addr, slot)).
//   - Agent's rehandshake flies: target → proxy → server.
//   - cli.List succeeds; response shows both runners registered.
//
// This test was previously a RED test pinning the missing-feature state.
// After Task 6 (RequestChainedRelay orchestration), the 2-hop scenario
// works end-to-end and cli.List succeeds.

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

func TestChainedRelay_2Hop_E2E(t *testing.T) {
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
	// cli.DialPeerConn detects the env and routes via cli.DialViaProxy(target, taskID).
	// With chained relay:
	//   - target emits RequestChainedRelay → server walks chain → proxy gets eager SetProxy.
	//   - target installs its own SetProxy.
	//   - Agent rehandshake flies target → proxy → server end-to-end.
	t.Setenv("HARNESS_PROXY_VIA_RUNNER", "ws:"+targetListen+"-*")
	t.Setenv("HARNESS_TASK_ID", hex.EncodeToString(taskID.Id[:]))

	dialCtx2, dialCancel2 := context.WithTimeout(ctx, 30*time.Second)
	defer dialCancel2()

	client, dialErr := cli.Dial(dialCtx2, serverCID)
	if dialErr != nil {
		t.Fatalf("cli.Dial through chained relay failed: %v", dialErr)
	}
	defer client.Close()
	t.Logf("cli.Dial through chained relay succeeded")

	listCtx, listCancel := context.WithTimeout(ctx, 10*time.Second)
	defer listCancel()
	var buf bytes.Buffer
	listErr := cli.List(listCtx, serverCID, &buf)
	if listErr != nil {
		t.Fatalf("cli.List should succeed through chained relay: %v", listErr)
	}
	t.Logf("cli.List through chained relay succeeded; output:\n%s", buf.String())

	// Cleanup.
	cancel()
	for _, ch := range []chan error{srvDone, proxyOnConnectDone, targetDone} {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
		}
	}
}
