//go:build integration

// Reverse-dial E2E for harness-cli server dial-runner.
//
// Topology:
//
//	server (Mutual mode, in-process) <-- dial --> runner (Listen mode, in-process)
//	                                  ^
//	                                  | cli.ServerDialRunner(serverCID, runnerCID)
//	                                  |
//	                              this test
//
// Flow:
//  1. Start server.Run with ws addr A.
//  2. Start runner.ListenAndServe with ws listen B.
//  3. Connect to server, send DialRunnerRequest pointing at B.
//  4. Wait for the server's Snapshot to include a Runner with the expected
//     hostname (proves the inbound-from-server connection completed
//     PSK + Hello + Registry insert on the runner side, and the runner
//     itself reported back to the server).

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

	// Distinct ports from the other e2e tests (18539 / 18540) to avoid
	// collision when the suite runs in parallel.
	const (
		serverAddr   = "127.0.0.1:18550"
		runnerListen = "127.0.0.1:18551"
		hostname     = "reverse-dial-runner"
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Start the server.
	s := server.New(server.Config{
		Addr:    serverAddr,
		DataDir: t.TempDir(),
	})
	srvDone := make(chan error, 1)
	go func() { srvDone <- s.Run(ctx) }()
	time.Sleep(300 * time.Millisecond) // bind grace

	// 2. Start the runner in Listen mode.
	listenDone := make(chan error, 1)
	go func() {
		listenDone <- runner.ListenAndServe(ctx, runner.ListenConfig{
			Config: runner.Config{
				AllowedRoots: []string{t.TempDir()},
				MaxTasks:     1,
				Hostname:     hostname,
			},
			WSListen: runnerListen,
		})
	}()
	time.Sleep(300 * time.Millisecond) // bind grace

	// 3. Have a cli client ask the server to reverse-dial the runner.
	serverCID, err := objproto.ParseConnectionID("ws:"+serverAddr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse server cid: %v", err)
	}
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

	// 4. Verify the runner is registered: poll Snapshot until the runner
	// with the chosen hostname shows up (or we time out). The dial returns
	// Ok the moment ECDH succeeds; PSK + Hello + Registry insert happen
	// asynchronously inside handleConnection, so we have to wait.
	c, err := cli.Dial(ctx, serverCID)
	if err != nil {
		t.Fatalf("cli.Dial (verify): %v", err)
	}
	defer c.Close()

	deadline := time.Now().Add(5 * time.Second)
	var lastSeen []string
	for time.Now().Before(deadline) {
		snap, err := c.Snapshot(ctx)
		if err == nil {
			lastSeen = lastSeen[:0]
			for _, r := range snap.Runners {
				lastSeen = append(lastSeen, string(r.Hostname))
				if string(r.Hostname) == hostname {
					goto registered
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("runner %q never appeared in Snapshot within 5s; last seen hostnames=%v", hostname, lastSeen)

registered:
	// Cleanup: cancel + drain.
	cancel()
	select {
	case <-srvDone:
	case <-time.After(2 * time.Second):
		t.Log("server did not exit within 2s of cancel — leaking goroutine")
	}
	select {
	case <-listenDone:
	case <-time.After(2 * time.Second):
		t.Log("runner listen did not exit within 2s of cancel — leaking goroutine")
	}
}
