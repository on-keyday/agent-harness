package server

import (
	"context"
	"encoding/hex"
	"net"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// Exercise handleConnection itself: a helper-only check would miss a role gate
// that was implemented but never applied to real incoming traffic.
func TestConnectionRolesE2E(t *testing.T) {
	for _, mode := range []string{"agent report", "client hello", "runner task control", "runner hello"} {
		t.Run(mode, func(t *testing.T) {
			for _, key := range []string{"HARNESS_TASK_ID", "HARNESS_RUNNER_ID", "HARNESS_AUTH_TICKET", "HARNESS_PROXY_VIA_RUNNER", "HARNESS_PSK", "HARNESS_PSK_FILE", "HARNESS_OPERATOR_PSK", "HARNESS_OPERATOR_PSK_FILE", "HARNESS_SERVER_CID", "HARNESS_WS_PATH", cli.ServerPubKeyEnv, cli.ServerPubKeyFileEnv} {
				t.Setenv(key, "")
			}
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			addr := ln.Addr().String()
			_ = ln.Close()
			cid := objproto.MustParseConnectionID("ws:" + addr + "-1")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			runnerPriv, runnerKeys := enrolledTestRunner(t)
			s := New(Config{Addr: addr, OperatorPSK: []byte("role-test-operator"), RunnerKeys: runnerKeys})
			s.SetBoard(newTestBoard(t))
			done := make(chan error, 1)
			go func() { done <- s.Run(ctx) }()
			t.Cleanup(func() {
				cancel()
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Error("server did not stop")
				}
			})
			// Wait for the listener without creating any task or runner.
			for {
				probe, err := net.DialTimeout("tcp", addr, 20*time.Millisecond)
				if err == nil {
					_ = probe.Close()
					break
				}
				if ctx.Err() != nil {
					t.Fatal("server did not listen")
				}
				time.Sleep(10 * time.Millisecond)
			}
			victim := s.Tasks().Create("repo", "", protocol.TaskKind_Oneshot, protocol.ClientKind_Cli, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{}, "")
			var wire []byte
			var conn objproto.Connection
			var peerDone <-chan struct{}
			if mode == "agent report" || mode == "client hello" {
				if mode == "agent report" {
					identity := testRunnerID("role-agent")
					id := s.Tasks().Create("repo", "", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent, protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, authority.Grant{}, "")
					ticket := [16]byte{1}
					boardRegisterTask(s.Board, identity, id, ticket, "", agentboard.NewConversation)
					t.Setenv("HARNESS_RUNNER_ID", identity.Hex())
					t.Setenv("HARNESS_TASK_ID", id)
					t.Setenv("HARNESS_AUTH_TICKET", hex.EncodeToString(ticket[:]))
				} else {
					t.Setenv("HARNESS_OPERATOR_PSK", "role-test-operator")
				}
				c, err := cli.Dial(ctx, cid, protocol.ClientKind_Cli)
				if err != nil {
					t.Fatal(err)
				}
				defer c.Close()
				if _, err := cli.WhoAmIWith(ctx, c); err != nil {
					t.Fatalf("legitimate task control failed: %v", err)
				}
				conn = c.Conn()
				peerDone = c.Peer().Done()
				if mode == "agent report" {
					msg := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_TaskFinished}
					msg.SetTaskFinished(protocol.TaskFinished{TaskId: taskIDFromHex(victim)})
					wire = msg.MustAppend([]byte{byte(appwire.AppKind_RunnerControl)})
				} else {
					msg := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_ClientHello}
					msg.SetClientHello(protocol.ClientHello{Kind: protocol.ClientKind_Cli})
					wire = msg.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
				}
			} else {
				pc, err := cli.DialPeerConn(ctx, cid)
				if err != nil {
					t.Fatal(err)
				}
				defer pc.Close()
				authed := make(chan protocol.PskAuthStatus, 1)
				pc.SetOnControl(func(kind appwire.AppKind, payload []byte) {
					if kind == appwire.AppKind_PskAuth {
						var resp protocol.PskAuthResponse
						if err := resp.DecodeExact(payload); err == nil {
							authed <- resp.Status
						}
					}
				})
				pc.Start(ctx)
				conn = pc.Connection()
				peerDone = pc.Done()
				hello := protocol.RunnerHello{Version: 1, RunnerId: testRunnerID("role-runner"), MaxTasks: 1}
				req := buildRunnerAuthReq(runnerPriv, conn.GetTranscript(), hello)
				if _, _, err := conn.SendMessage(buildPskAuthData(t, req)); err != nil {
					t.Fatal(err)
				}
				select {
				case status := <-authed:
					if status != protocol.PskAuthStatus_Ok {
						t.Fatalf("runner handshake: %v", status)
					}
				case <-ctx.Done():
					t.Fatal("runner handshake timed out")
				}
				if mode == "runner task control" {
					msg := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_Cancel}
					msg.SetCancel(protocol.CancelTask{TaskId: taskIDFromHex(victim)})
					wire = msg.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
				} else {
					msg := &protocol.RunnerMessage{Kind: protocol.RunnerMessageType_Hello}
					msg.SetHello(hello)
					wire = msg.MustAppend([]byte{byte(appwire.AppKind_RunnerControl)})
				}
			}
			if _, _, err := conn.SendMessage(wire); err != nil {
				t.Fatal(err)
			}
			select {
			case <-peerDone:
			case <-time.After(3 * time.Second):
				t.Fatal("role violation did not close the connection")
			}
			if task, _ := s.Tasks().Get(victim); task.Status != protocol.TaskStatus_Queued {
				t.Fatalf("role violation changed unrelated task to %v", task.Status)
			}
		})
	}
}
