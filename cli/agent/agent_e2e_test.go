package agent_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
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
// returning (board, srv). The server is automatically stopped and the board
// closed via t.Cleanup. Callers that only need the board can write:
//
//	board, _ := startServerE2E(t, addr)
func startServerE2E(t *testing.T, addr string) (*agentboard.Board, *server.Server) {
	t.Helper()
	return startServerE2EWithMaxPayload(t, addr, 4096)
}

// startServerE2EWithMaxPayload is startServerE2E with the per-message limit
// under the caller's control, for tests that need a limit large enough to
// exercise the transport's own thresholds.
func startServerE2EWithMaxPayload(t *testing.T, addr string, maxPayload int) (*agentboard.Board, *server.Server) {
	t.Helper()

	board := agentboard.New(agentboard.Config{
		RingN:      64,
		TopicTTL:   time.Hour,
		MaxTopics:  32,
		MaxPayload: maxPayload,
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

	return board, s
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

// mkRidE2E builds a synthetic runner identity. The parameters are kept so the
// call sites still read as "a distinct runner", but they now seed 16 opaque
// bytes rather than describing an address.
func mkRidE2E(ip [4]byte, port uint16, unique uint16) protocol.RunnerID {
	var r protocol.RunnerID
	copy(r.Id[:4], ip[:])
	r.Id[4], r.Id[5] = byte(port), byte(port>>8)
	r.Id[6], r.Id[7] = byte(unique), byte(unique>>8)
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
func setAgentEnv(serverAddr string, rid protocol.RunnerID, tid protocol.TaskID, ticket [16]byte) func() {
	prev := map[string]string{
		"HARNESS_SERVER_CID":  os.Getenv("HARNESS_SERVER_CID"),
		"HARNESS_RUNNER_ID":   os.Getenv("HARNESS_RUNNER_ID"),
		"HARNESS_TASK_ID":     os.Getenv("HARNESS_TASK_ID"),
		"HARNESS_AUTH_TICKET": os.Getenv("HARNESS_AUTH_TICKET"),
	}
	os.Setenv("HARNESS_SERVER_CID", "ws:"+serverAddr+"-*")
	os.Setenv("HARNESS_RUNNER_ID", rid.Hex())
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
	board, _ := startServerE2E(t, addr)

	// Synthetic agent identities. HARNESS_RUNNER_ID carries the identity's hex,
	// derived from the same value the board registers, so the two cannot drift.

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
	restoreA := setAgentEnv(addr, ridA, tidA, ticketA)
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
	restoreB := setAgentEnv(addr, ridB, tidB, ticketB)
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

// TestAgentCLI_E2E_DeliveredMessageCarriesSenderProfile asserts that the server
// attests the SENDER's agent profile on delivery: agent B learns that agent A
// runs under "codex" without holding board_observe (board delivery is uncapped,
// `ls` is not), and without A having supplied the value.
//
// Unlike the other E2Es here, this one creates a real TaskStore entry — the
// profile is resolved from the task record, so a bare ticket registration has
// nothing to resolve.
func TestAgentCLI_E2E_DeliveredMessageCarriesSenderProfile(t *testing.T) {
	addr := freePortE2E(t)
	board, srv := startServerE2E(t, addr)

	ridA := mkRidE2E([4]byte{1, 2, 3, 4}, 9010, 1)
	ridB := mkRidE2E([4]byte{5, 6, 7, 8}, 9011, 2)

	taskHexA := srv.Tasks().Create("/repo", "p", protocol.TaskKind_Interactive,
		protocol.ClientKind_Cli, protocol.TaskID{}, ridA.Hex(), protocol.RunnerSelector{},
		nil, protocol.Capability_All, server.Scope{}, "codex")
	var tidA protocol.TaskID
	raw, err := hex.DecodeString(taskHexA)
	if err != nil || len(raw) != 16 {
		t.Fatalf("task id %q: decode err=%v len=%d", taskHexA, err, len(raw))
	}
	copy(tidA.Id[:], raw)
	tidB := mkTidE2E(0x7B)

	var ticketA, ticketB [16]byte
	ticketA[0] = 0xA1
	ticketB[0] = 0xB1
	board.Registry().Register(ridA, tidA, ticketA)
	board.Registry().Register(ridB, tidB, ticketB)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	restoreA := setAgentEnv(addr, ridA, tidA, ticketA)
	var sendOut bytes.Buffer
	// The payload deliberately lies about the sender: the attested field must
	// not come from anything the agent wrote.
	if err := agent.Send(ctx,
		[]string{"--topic", "topic/profile-e2e", "--data", `{"agent":"not-really-me"}`},
		nil, &sendOut,
	); err != nil {
		restoreA()
		t.Fatalf("agent.Send: %v", err)
	}
	restoreA()

	restoreB := setAgentEnv(addr, ridB, tidB, ticketB)
	var waitOut bytes.Buffer
	if err := agent.Wait(ctx,
		[]string{"--topic", "topic/profile-e2e", "--timeout", "2s"},
		&waitOut,
	); err != nil {
		restoreB()
		t.Fatalf("agent.Wait: %v", err)
	}
	restoreB()

	var rec struct {
		From struct {
			Agent    string `json:"agent"`
			Hostname string `json:"hostname"`
		} `json:"from"`
	}
	line := strings.TrimSpace(waitOut.String())
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("wait output is not JSON Lines: %v\n%s", err, waitOut.String())
	}
	if rec.From.Agent != "codex" {
		t.Errorf("from.agent = %q, want %q", rec.From.Agent, "codex")
	}
}

// TestAgentCLI_E2E_SubscribeThenSendAndWait verifies the Subscribe → Send →
// Wait flow over three sequential CLI-function calls (two agents).
// Agent B subscribes first, then A sends, then B waits.
func TestAgentCLI_E2E_SubscribeThenSendAndWait(t *testing.T) {
	addr := freePortE2E(t)
	board, _ := startServerE2E(t, addr)

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
	restoreB := setAgentEnv(addr, ridB, tidB, ticketB)
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
	restoreA := setAgentEnv(addr, ridA, tidA, ticketA)
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
	restoreB2 := setAgentEnv(addr, ridB, tidB, ticketB)
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
	board, _ := startServerE2E(t, addr)

	var goodTicket [16]byte
	goodTicket[0] = 0xDE
	var badTicket [16]byte
	badTicket[0] = 0xAD

	tid := mkTidE2E(5)
	rid := mkRidE2E([4]byte{1, 2, 3, 4}, 9004, 5)

	board.Registry().Register(rid, tid, goodTicket)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	restore := setAgentEnv(addr, rid, tid, badTicket)
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
