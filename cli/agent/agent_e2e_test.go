package agent_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/cli/agent"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/server"
)

// startServerE2E starts an in-process server.Server with a Board on addr,
// returning (board, cancel). cancel stops the server and closes the board.
func startServerE2E(t *testing.T, addr string) *agentboard.Board {
	t.Helper()

	board := agentboard.New(agentboard.Config{
		RingN:      64,
		TopicTTL:   time.Hour,
		MaxTopics:  32,
		MaxPayload: 4096,
	})

	s := server.New(server.Config{Addr: addr})
	s.SetBoard(board)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Run(ctx) }()

	// Poll until the HTTP server is ready to accept connections.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			c.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Cleanup(func() {
		cancel()
		board.Close()
	})

	return board
}

// freePortE2E finds a free port on 127.0.0.1.
func freePortE2E(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePortE2E: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// mkRidE2E builds a synthetic protocol.RunnerID matching "ws:IP:PORT-UNIQUE".
func mkRidE2E(ip [4]byte, port uint16, unique uint16) protocol.RunnerID {
	var r protocol.RunnerID
	r.SetTransport([]byte("ws"))
	r.SetIpAddr(ip[:])
	r.Port = port
	r.UniqueNumber = unique
	return r
}

// mkTidE2E builds a synthetic protocol.TaskID with discriminator byte b.
func mkTidE2E(b byte) protocol.TaskID {
	var t protocol.TaskID
	t.Id[0] = b
	return t
}

// setAgentEnv overwrites the HARNESS_* env vars used by cliopts to identify
// this agent. It is the caller's responsibility to restore them (e.g., via
// t.Cleanup or a subsequent call). Returns a restore function.
func setAgentEnv(serverAddr, ridStr string, tid protocol.TaskID, ticket [16]byte) func() {
	prev := map[string]string{
		"HARNESS_SERVER_CID":  os.Getenv("HARNESS_SERVER_CID"),
		"HARNESS_RUNNER_ID":   os.Getenv("HARNESS_RUNNER_ID"),
		"HARNESS_TASK_ID":     os.Getenv("HARNESS_TASK_ID"),
		"HARNESS_AUTH_TICKET": os.Getenv("HARNESS_AUTH_TICKET"),
	}
	os.Setenv("HARNESS_SERVER_CID", "ws:"+serverAddr+"-*")
	os.Setenv("HARNESS_RUNNER_ID", ridStr)
	os.Setenv("HARNESS_TASK_ID", hex.EncodeToString(tid.Id[:]))
	os.Setenv("HARNESS_AUTH_TICKET", hex.EncodeToString(ticket[:]))
	return func() {
		for k, v := range prev {
			if v == "" {
				os.Unsetenv(k)
			} else {
				os.Setenv(k, v)
			}
		}
	}
}

// TestAgentCLI_E2E_SendThenWait spins up an in-process server, registers
// tickets for two synthetic agents, has agent A call Send(), then has agent B
// call Wait(), and asserts that B's wait output contains A's payload.
//
// The agentboard buffers messages in a ring; Wait does an implicit subscribe
// and immediately returns buffered messages with seq > since, so B does not
// need to subscribe before A sends.
func TestAgentCLI_E2E_SendThenWait(t *testing.T) {
	addr := freePortE2E(t)
	board := startServerE2E(t, addr)

	// Synthetic agent identities. The RunnerID string must be parseable by
	// cliopts.ResolveRunnerID: "ws:IP:PORT-UNIQUE" with a numeric ID.
	const (
		ridStrA = "ws:1.2.3.4:9000-1"
		ridStrB = "ws:5.6.7.8:9001-2"
	)

	var ticketA, ticketB [16]byte
	ticketA[0] = 0xAA
	ticketB[0] = 0xBB

	tidA := mkTidE2E(1)
	tidB := mkTidE2E(2)

	ridA := mkRidE2E([4]byte{1, 2, 3, 4}, 9000, 1)
	ridB := mkRidE2E([4]byte{5, 6, 7, 8}, 9001, 2)

	board.Registry().Register(ridA, tidA, ticketA)
	board.Registry().Register(ridB, tidB, ticketB)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// --- Agent A sends -------------------------------------------------------
	restoreA := setAgentEnv(addr, ridStrA, tidA, ticketA)
	var sendOut bytes.Buffer
	if err := agent.Send(ctx,
		[]string{"--topic", "topic/test-e2e", "--data", `{"msg":"hello-from-A"}`},
		nil,
		&sendOut,
	); err != nil {
		restoreA()
		t.Fatalf("agent.Send: %v", err)
	}
	restoreA()

	if !strings.Contains(sendOut.String(), "ok") {
		t.Errorf("Send output missing 'ok': %s", sendOut.String())
	}

	// --- Agent B waits -------------------------------------------------------
	restoreB := setAgentEnv(addr, ridStrB, tidB, ticketB)
	var waitOut bytes.Buffer
	if err := agent.Wait(ctx,
		[]string{"--topic", "topic/test-e2e", "--timeout", "2s"},
		&waitOut,
	); err != nil {
		restoreB()
		t.Fatalf("agent.Wait: %v", err)
	}
	restoreB()

	got := waitOut.String()
	if !strings.Contains(got, "hello-from-A") {
		t.Errorf("Wait output missing payload: %s", got)
	}
}

// TestAgentCLI_E2E_SubscribeThenSendAndWait verifies the Subscribe → Send →
// Wait flow over three sequential CLI-function calls (two agents).
// Agent B subscribes first, then A sends, then B waits.
func TestAgentCLI_E2E_SubscribeThenSendAndWait(t *testing.T) {
	addr := freePortE2E(t)
	board := startServerE2E(t, addr)

	const (
		ridStrA = "ws:1.2.3.4:9002-3"
		ridStrB = "ws:5.6.7.8:9003-4"
	)

	var ticketA, ticketB [16]byte
	ticketA[0] = 0xCA
	ticketB[0] = 0xCB

	tidA := mkTidE2E(3)
	tidB := mkTidE2E(4)

	ridA := mkRidE2E([4]byte{1, 2, 3, 4}, 9002, 3)
	ridB := mkRidE2E([4]byte{5, 6, 7, 8}, 9003, 4)

	board.Registry().Register(ridA, tidA, ticketA)
	board.Registry().Register(ridB, tidB, ticketB)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Agent B subscribes to "topic/sub-test".
	restoreB := setAgentEnv(addr, ridStrB, tidB, ticketB)
	var subOut bytes.Buffer
	if err := agent.Subscribe(ctx,
		[]string{"--topic", "topic/sub-test"},
		&subOut,
	); err != nil {
		restoreB()
		t.Fatalf("agent.Subscribe: %v", err)
	}
	restoreB()

	if !strings.Contains(subOut.String(), "ok") {
		t.Errorf("Subscribe output missing 'ok': %s", subOut.String())
	}

	// Agent A sends to "topic/sub-test".
	restoreA := setAgentEnv(addr, ridStrA, tidA, ticketA)
	var sendOut bytes.Buffer
	if err := agent.Send(ctx,
		[]string{"--topic", "topic/sub-test", "--data", `{"msg":"hello-sub"}`},
		nil,
		&sendOut,
	); err != nil {
		restoreA()
		t.Fatalf("agent.Send: %v", err)
	}
	restoreA()

	// Agent B waits (topic is in board ring, so returns immediately).
	restoreB2 := setAgentEnv(addr, ridStrB, tidB, ticketB)
	var waitOut bytes.Buffer
	if err := agent.Wait(ctx,
		[]string{"--topic", "topic/sub-test", "--timeout", "2s"},
		&waitOut,
	); err != nil {
		restoreB2()
		t.Fatalf("agent.Wait: %v", err)
	}
	restoreB2()

	if !strings.Contains(waitOut.String(), "hello-sub") {
		t.Errorf("Wait output missing payload: %s", waitOut.String())
	}
}

// TestAgentCLI_E2E_BadTicket verifies that ConnectAgent returns an error when
// the auth ticket does not match the registered one.
func TestAgentCLI_E2E_BadTicket(t *testing.T) {
	addr := freePortE2E(t)
	board := startServerE2E(t, addr)

	const ridStr = "ws:1.2.3.4:9004-5"

	var goodTicket [16]byte
	goodTicket[0] = 0xDE
	var badTicket [16]byte
	badTicket[0] = 0xAD

	tid := mkTidE2E(5)
	rid := mkRidE2E([4]byte{1, 2, 3, 4}, 9004, 5)

	board.Registry().Register(rid, tid, goodTicket)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	restore := setAgentEnv(addr, ridStr, tid, badTicket)
	var out bytes.Buffer
	err := agent.Send(ctx,
		[]string{"--topic", "topic/bad-ticket", "--data", "should-fail"},
		nil,
		&out,
	)
	restore()

	if err == nil {
		t.Fatal("expected error for bad ticket, got nil")
	}
	if !strings.Contains(err.Error(), "hello rejected") && !strings.Contains(err.Error(), "BadTicket") {
		t.Errorf("error message unexpected: %v", err)
	}
}
