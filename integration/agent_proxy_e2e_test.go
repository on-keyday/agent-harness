//go:build integration

// End-to-end exercise for the Phase B agent-proxy ceremony.
//
// Topology:
//
//	    server (Mutual, in-process) <-- DialGreeting+ServerHello path -- runner (Listen mode)
//	                ^                                                          ^
//	                |                                                          | ProxyRequest
//	                |                                                          | + Rehandshake
//	                |                                                          |
//	                +------ end-to-end peer.Conn (cli.DialViaProxy) -----------+
//
// Flow:
//  1. Start server (Mutual mode) wired to an agentboard.
//  2. Start runner in --listen mode.
//  3. Drive Phase A reverse-dial (cli.ServerDialRunner) so the runner has
//     a live server-conn Session (lets the agent-proxy handler validate
//     "ServerNotConnected").
//  4. Inject a fake task on the runner side via the test hook + register a
//     matching auth_ticket on the server-side board. This skips the real
//     Submit+Assign+spawn-claude pipeline.
//  5. cli.DialViaProxy → end-to-end *peer.Conn against the SERVER (handshake
//     forwarded packet-by-packet by the runner; SetProxy is now in effect).
//  6. Run the PSK + AgentBridgeHello dance on that conn and assert
//     HelloStatus_Ok comes back.
//
// What this validates beyond Phase A:
//   - DialGreeting vs AgentProxyControl discrimination on the runner's
//     accept loop (handleAcceptedConn dispatch).
//   - ProxyRequest validation (server-connected + task-exists + no
//     id-collision) and EstablishResponse delivery.
//   - SetProxy + objproto-level packet relay through the runner.
//   - RehandshakeForProxy: the new keys are derived end-to-end with the
//     real server, not with the runner.
//   - The board-registered AuthTicket actually round-trips end-to-end —
//     anything wrong with the keying or the relay would surface as a
//     BadTicket / no-response failure here.

package integration

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/server"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

func TestAgentProxyE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}

	// Ports distinct from server_dial_runner_e2e_test.go (18550 / 18551).
	const (
		serverAddr   = "127.0.0.1:18590"
		runnerListen = "127.0.0.1:18591"
		hostname     = "proxy-e2e-runner"
	)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 1. Start the server. Board is wired in so AgentBridgeHello validation
	//    has somewhere to look up tickets.
	board := agentboard.New(agentboard.Config{
		RingN:      32,
		TopicTTL:   time.Hour,
		MaxTopics:  16,
		MaxPayload: 4096,
	})
	defer board.Close()

	srv := server.New(server.Config{
		Addr:    serverAddr,
		DataDir: t.TempDir(),
	})
	srv.SetBoard(board)

	srvDone := make(chan error, 1)
	go func() { srvDone <- srv.Run(ctx) }()
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

	// 3. Phase A reverse-dial. Server connects to runner; on success the
	//    runner's accept loop publishes a Session into lastListenSession
	//    via handleServerConn.
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

	dialResp, err := cli.ServerDialRunner(ctx, serverCID, runnerCID)
	if err != nil {
		t.Fatalf("ServerDialRunner: %v", err)
	}
	if dialResp.Status != protocol.DialRunnerStatus_Ok {
		t.Fatalf("dial-runner status: got %v want Ok", dialResp.Status)
	}

	// Wait for the runner to appear in the server's registry. The dial
	// returns Ok the moment ECDH succeeds; PSK + RunnerHello + Registry.Add
	// happen asynchronously on the server's handleConnection goroutine.
	var registered []server.RunnerEntry
	{
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			registered = srv.RegisteredRunners()
			if len(registered) >= 1 {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	if len(registered) != 1 {
		t.Fatalf("expected 1 registered runner after Phase A, got %d", len(registered))
	}

	// 4. Inject a fake task on the runner so the proxy ceremony's HasTask
	//    check passes, and register a matching auth_ticket on the board so
	//    the server's AgentBridgeHello validation passes.
	var taskID protocol.TaskID
	if _, err := rand.Read(taskID.Id[:]); err != nil {
		t.Fatalf("gen taskID: %v", err)
	}
	var ticket [16]byte
	if _, err := rand.Read(ticket[:]); err != nil {
		t.Fatalf("gen ticket: %v", err)
	}

	// Poll briefly: handleServerConn calls sessionRef.Store AFTER
	// driveAfterConn returns, which races with Phase A's Ok return.
	{
		deadline := time.Now().Add(2 * time.Second)
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
			t.Fatalf("AddFakeTaskForListenServer (after 2s): %v", lastErr)
		}
	}

	// The server keys the runner by its ConnectionID string. Convert the
	// same string the Registry uses → protocol.RunnerID for board register.
	srvRunnerEntry := registered[0]
	srvRunnerCID, err := objproto.ParseConnectionID(srvRunnerEntry.ID, 0)
	if err != nil {
		t.Fatalf("parse registered runner ID %q: %v", srvRunnerEntry.ID, err)
	}
	protoRid := protocol.ConnIDToRunnerID(srvRunnerCID)

	board.Registry().Register(protoRid, taskID, ticket)

	// agentboard.RunnerID / TaskID (distinct Go types, same wire shape) for
	// the Hello envelope.
	var boardRid agentboard.RunnerID
	boardRid.SetTransport(protoRid.Transport)
	boardRid.SetIpAddr(protoRid.IpAddr)
	boardRid.Port = protoRid.Port
	boardRid.UniqueNumber = protoRid.UniqueNumber

	var boardTid agentboard.TaskID
	boardTid.Id = taskID.Id

	// 5. End-to-end conn through the runner proxy.
	proxyConn, err := cli.DialViaProxy(ctx, runnerCID, taskID)
	if err != nil {
		t.Fatalf("DialViaProxy: %v", err)
	}
	defer proxyConn.Close()

	// 6. PSK + AgentBridgeHello on the proxied conn, mirroring
	//    cli/agent/conn.go ConnectAgent's combined-handler pattern.
	pskRespCh := make(chan wire.PskAuthStatus, 1)
	helloRespCh := make(chan agentboard.HelloStatus, 1)

	proxyConn.SetOnControl(func(kind wire.ApplicationPayloadKind, payload []byte) {
		switch kind {
		case wire.ApplicationPayloadKind_PskAuth:
			if len(payload) > 0 {
				select {
				case pskRespCh <- wire.PskAuthStatus(payload[0]):
				default:
				}
			}
		case wire.ApplicationPayloadKind_AgentMessage:
			msg := &agentboard.AgentMessage{}
			if _, err := msg.Decode(payload); err != nil {
				return
			}
			if msg.Kind != agentboard.AgentMessageKind_HelloResponse {
				return
			}
			resp := msg.HelloResponse()
			if resp == nil {
				return
			}
			select {
			case helloRespCh <- resp.Status:
			default:
			}
		}
	})
	proxyConn.Start(ctx)

	// PSK is not configured on the server in this test (nil) — SendAndWaitPSK
	// short-circuits and returns nil immediately. Kept in the test for shape
	// parity with the production ConnectAgent path.
	pskCtx, pskCancel := context.WithTimeout(ctx, 5*time.Second)
	defer pskCancel()
	if err := cli.SendAndWaitPSK(pskCtx, func(b []byte) error {
		_, _, err := proxyConn.Connection().SendMessage(b)
		return err
	}, nil, pskRespCh); err != nil {
		t.Fatalf("SendAndWaitPSK: %v", err)
	}

	// AgentBridgeHello.
	hello := agentboard.AgentBridgeHello{
		RunnerId:   boardRid,
		TaskId:     boardTid,
		AuthTicket: ticket,
	}
	hello.SetHostname([]byte("proxy-e2e-agent"))
	msg := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_Hello}
	msg.SetHello(hello)
	data := msg.MustAppend([]byte{byte(wire.ApplicationPayloadKind_AgentMessage)})
	if _, _, err := proxyConn.Connection().SendMessage(data); err != nil {
		t.Fatalf("send AgentBridgeHello: %v", err)
	}

	select {
	case status := <-helloRespCh:
		if status != agentboard.HelloStatusOk {
			t.Fatalf("hello rejected: got %v want Ok", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for HelloResponse on proxied conn")
	}

	// Cleanup: cancel and drain. Server / runner shutdown is best-effort —
	// a leaking goroutine here surfaces as a timeout log, not a test
	// failure, mirroring Phase A's pattern.
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
