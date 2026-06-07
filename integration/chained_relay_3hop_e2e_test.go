//go:build integration

// 3-hop chained relay end-to-end test.
//
// Topology:
//
//	server (127.0.0.1:18750)
//	   ▲
//	   │  Phase A direct reverse-dial (dial mode)
//	   │  Q ("chained-Q") — registered outbound via runner.Connect
//	   │
//	   │  Phase C relay: Q relays server↔P
//	   │  P ("chained-P") — listen mode at 127.0.0.1:18751
//	   │                    registered via cli.ServerDialRunner with via=Q
//	   │
//	   │  Phase C relay: P relays server↔L (through Q)
//	   │  L ("chained-L") — listen mode at 127.0.0.1:18752
//	   │                    registered via cli.ServerDialRunner with via=P
//	   │
//	   └── agent process on L's host:
//	       HARNESS_PROXY_VIA_RUNNER=ws:127.0.0.1:18752-*
//	       cli.List succeeds through the 3-hop chain.
//
// Via chain: L.Via=P, P.Via=Q, Q.Via=nil.
//
// With chained relay (Phase D):
//   - L emits RequestChainedRelay → server walks L's Via chain (L → P → Q).
//   - Server dispatches EstablishRelay in parallel to both P and Q.
//   - P installs eager SetProxy for the agent slot targeting L's addr.
//   - Q installs eager SetProxy for the agent slot targeting P's addr.
//   - L installs its own SetProxy(agentCID, (P.Addr, slot)).
//   - Agent rehandshake flies: L → P → Q → server.
//   - cli.List succeeds; response shows Q, P, L all registered.

package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/agent-harness/runner"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/server"
)

func TestChainedRelay_3Hop_E2E(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}

	const (
		serverAddr = "127.0.0.1:18750"
		pListen    = "127.0.0.1:18751"
		lListen    = "127.0.0.1:18752"
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

	// 2. Start Q via runner.Connect (dial mode, reverse-dial).
	//    Q registers with the server outbound; acts as the top-level relay proxy.
	qCfg := runner.Config{
		ServerCID:    serverCID,
		AllowedRoots: []string{t.TempDir()},
		MaxTasks:     1,
		Hostname:     "chained-Q",
	}
	qHandle, err := runner.Connect(ctx, qCfg)
	if err != nil {
		t.Fatalf("runner.Connect (Q): %v", err)
	}
	qOnConnectDone := make(chan error, 1)
	go func() { qOnConnectDone <- runner.OnConnect(ctx, qHandle) }()

	// 3. Wait for Q to register (dial-mode runners self-register after ECDH+Hello).
	qEntry := waitForRunnerByHostname(t, srv, "chained-Q", 5*time.Second)
	t.Logf("Q registered: ID=%s", qEntry.ID)

	qRegisteredCID, err := objproto.ParseConnectionID(qEntry.ID, 0)
	if err != nil {
		t.Fatalf("parse Q registered CID %q: %v", qEntry.ID, err)
	}

	// 4. Start P via runner.ListenAndServe (listen mode at pListen).
	pDone := make(chan error, 1)
	go func() {
		pDone <- runner.ListenAndServe(ctx, runner.ListenConfig{
			Config: runner.Config{
				AllowedRoots: []string{t.TempDir()},
				MaxTasks:     1,
				Hostname:     "chained-P",
			},
			WSListen: pListen,
		})
	}()
	time.Sleep(300 * time.Millisecond)

	// 5. Register P via Q using cli.ServerDialRunner(target=P_listen_cid, via=Q).
	pListenCID, err := objproto.ParseConnectionID("ws:"+pListen+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse P listen cid: %v", err)
	}
	dialCtxP, dialCancelP := context.WithTimeout(ctx, 15*time.Second)
	respP, err := cli.ServerDialRunner(dialCtxP, serverCID, pListenCID, qRegisteredCID)
	dialCancelP()
	if err != nil {
		t.Fatalf("ServerDialRunner (P via Q): %v", err)
	}
	if respP.Status != protocol.DialRunnerStatus_Ok {
		t.Fatalf("ServerDialRunner (P via Q) status: got %v want Ok", respP.Status)
	}

	// 6. Wait for P to appear in registry.
	pEntry := waitForRunnerByHostname(t, srv, "chained-P", 5*time.Second)
	t.Logf("P registered via Q: ID=%s", pEntry.ID)

	pRegisteredCID, err := objproto.ParseConnectionID(pEntry.ID, 0)
	if err != nil {
		t.Fatalf("parse P registered CID %q: %v", pEntry.ID, err)
	}

	// 7. Start L via runner.ListenAndServe (listen mode at lListen).
	//    L is the leaf runner — the agent dials into L.
	lDone := make(chan error, 1)
	go func() {
		lDone <- runner.ListenAndServe(ctx, runner.ListenConfig{
			Config: runner.Config{
				AllowedRoots: []string{t.TempDir()},
				MaxTasks:     1,
				Hostname:     "chained-L",
			},
			WSListen: lListen,
		})
	}()
	time.Sleep(300 * time.Millisecond)

	// 8. Register L via P using cli.ServerDialRunner(target=L_listen_cid, via=P).
	lListenCID, err := objproto.ParseConnectionID("ws:"+lListen+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse L listen cid: %v", err)
	}
	dialCtxL, dialCancelL := context.WithTimeout(ctx, 15*time.Second)
	respL, err := cli.ServerDialRunner(dialCtxL, serverCID, lListenCID, pRegisteredCID)
	dialCancelL()
	if err != nil {
		t.Fatalf("ServerDialRunner (L via P): %v", err)
	}
	if respL.Status != protocol.DialRunnerStatus_Ok {
		t.Fatalf("ServerDialRunner (L via P) status: got %v want Ok", respL.Status)
	}

	// 9. Wait for L to appear in registry.
	_ = waitForRunnerByHostname(t, srv, "chained-L", 5*time.Second)
	t.Logf("L registered via P (via Q)")

	// 10. Inject a fake task on L (the leaf runner, which is the last
	//     ListenAndServe started — lastListenSession now holds L's session).
	var taskID protocol.TaskID
	if _, err := rand.Read(taskID.Id[:]); err != nil {
		t.Fatal(err)
	}
	// Poll briefly in case Phase C registration on L is racing with the
	// lastListenSession update (driveAfterConn stores the session only after
	// the server-conn completes).
	{
		deadline := time.Now().Add(3 * time.Second)
		var lastErr error
		for time.Now().Before(deadline) {
			if err := runner.AddFakeTaskForListenServer(ctx, taskID); err == nil {
				lastErr = nil
				break
			} else {
				lastErr = err
			}
			time.Sleep(50 * time.Millisecond)
		}
		if lastErr != nil {
			t.Fatalf("AddFakeTaskForListenServer on L (after 3s): %v", lastErr)
		}
	}

	// 11. Simulate the agent process on L's host:
	//     HARNESS_PROXY_VIA_RUNNER=ws:127.0.0.1:18752-*  (L's listen addr)
	//     HARNESS_TASK_ID=<hex of taskID>
	//
	// cli.DialPeerConn detects env, routes through cli.DialViaProxy(L, taskID).
	// With 3-hop chained relay:
	//   - L emits RequestChainedRelay → server walks L.Via=P, P.Via=Q, Q.Via=nil.
	//   - Server dispatches EstablishRelay in parallel to P (target=L.Addr) and
	//     Q (target=P.Addr).
	//   - P installs eager SetProxy for the agent slot, allocating toward L.
	//   - Q installs eager SetProxy for the agent slot, allocating toward P.
	//   - L installs its own SetProxy(agentCID, (P.Addr, slot)).
	//   - Agent rehandshake: L → P → Q → server (end-to-end AEAD).
	t.Setenv("HARNESS_PROXY_VIA_RUNNER", "ws:"+lListen+"-*")
	t.Setenv("HARNESS_TASK_ID", hex.EncodeToString(taskID.Id[:]))

	dialCtx2, dialCancel2 := context.WithTimeout(ctx, 30*time.Second)
	defer dialCancel2()

	client, dialErr := cli.Dial(dialCtx2, serverCID)
	if dialErr != nil {
		t.Fatalf("cli.Dial through 3-hop chained relay failed: %v", dialErr)
	}
	defer client.Close()
	t.Logf("cli.Dial through 3-hop chained relay succeeded")

	listCtx, listCancel := context.WithTimeout(ctx, 10*time.Second)
	defer listCancel()
	var buf bytes.Buffer
	listErr := cli.List(listCtx, serverCID, &buf)
	if listErr != nil {
		t.Fatalf("cli.List should succeed through 3-hop chained relay: %v", listErr)
	}
	t.Logf("cli.List through 3-hop chained relay succeeded; output:\n%s", buf.String())

	// 12. Verify that Q, P, and L all appear in the list output.
	output := buf.String()
	for _, hostname := range []string{"chained-Q", "chained-P", "chained-L"} {
		if !strings.Contains(output, hostname) {
			t.Errorf("cli.List output does not contain hostname %q; got:\n%s", hostname, output)
		}
	}

	// Cleanup.
	cancel()
	for _, ch := range []chan error{srvDone, qOnConnectDone, pDone, lDone} {
		select {
		case <-ch:
		case <-time.After(2 * time.Second):
		}
	}
}
