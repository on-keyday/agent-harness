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

// freeUDPPortE2E returns a localhost UDP "ip:port" string the test can bind
// the in-process server to. Mirrors freePortE2E (TCP) but for UDP.
func freeUDPPortE2E(t *testing.T) string {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	if err != nil {
		t.Fatalf("freeUDPPortE2E: %v", err)
	}
	addr := c.LocalAddr().String()
	c.Close()
	return addr
}

// startUDPServerE2E starts an in-process server.Server listening on UDP only
// (no WS leg) with a Board attached. Mirrors startServerE2E. Returns the
// board for ticket registration; cleanup is wired via t.Cleanup.
func startUDPServerE2E(t *testing.T, udpAddr string) *agentboard.Board {
	t.Helper()
	board := agentboard.New(agentboard.Config{
		RingN:      64,
		TopicTTL:   time.Hour,
		MaxTopics:  32,
		MaxPayload: 4096,
	})

	s := server.New(server.Config{UDPAddr: udpAddr})
	s.SetBoard(board)

	ctx, cancel := context.WithCancel(context.Background())
	go func() { _ = s.Run(ctx) }()

	// UDP has no listen-accept handshake we can probe; mirror integration/
	// udp_test.go's startUDPServer and give the listener a moment to bind.
	time.Sleep(300 * time.Millisecond)

	t.Cleanup(func() {
		cancel()
		board.Close()
	})

	return board
}

// setAgentEnvUDP mirrors setAgentEnv but emits a "udp:" CID for
// HARNESS_SERVER_CID so cliopts.ResolveServerCID returns Transport="udp".
// This is the path BuildClientEndpoint dispatches to UDPEndpoint.
func setAgentEnvUDP(serverAddr, ridStr string, tid protocol.TaskID, ticket [16]byte) func() {
	prev := map[string]string{
		"HARNESS_SERVER_CID":  os.Getenv("HARNESS_SERVER_CID"),
		"HARNESS_RUNNER_ID":   os.Getenv("HARNESS_RUNNER_ID"),
		"HARNESS_TASK_ID":     os.Getenv("HARNESS_TASK_ID"),
		"HARNESS_AUTH_TICKET": os.Getenv("HARNESS_AUTH_TICKET"),
	}
	os.Setenv("HARNESS_SERVER_CID", "udp:"+serverAddr+"-*")
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

// mkRidUDP_E2E is mkRidE2E's sibling that tags Transport="udp", matching the
// runner identity a UDP-dialed runner would register with.
func mkRidUDP_E2E(ip [4]byte, port uint16, unique uint16) protocol.RunnerID {
	var r protocol.RunnerID
	r.SetTransport([]byte("udp"))
	r.SetIpAddr(ip[:])
	r.Port = port
	r.UniqueNumber = unique
	return r
}

// TestAgentCLI_E2E_SendThenWait_UDP is the regression for
// fix/agent-cli-cid-transport (commit 0d3576d): ConnectAgent used to
// hardcode a WebSocket endpoint and silently warn "no sent probe for
// handshake ack" on every UserPromptSubmit hook when the server CID was
// UDP. End-to-end Send→Wait via cli/agent over a UDP-only server proves
// BuildClientEndpoint now honors peerCID.Transport.
func TestAgentCLI_E2E_SendThenWait_UDP(t *testing.T) {
	addr := freeUDPPortE2E(t)
	board := startUDPServerE2E(t, addr)

	const (
		ridStrA = "udp:1.2.3.4:9600-91"
		ridStrB = "udp:5.6.7.8:9601-92"
	)

	var ticketA, ticketB [16]byte
	ticketA[0] = 0xA9
	ticketB[0] = 0xB9

	tidA := mkTidE2E(0x91)
	tidB := mkTidE2E(0x92)

	ridA := mkRidUDP_E2E([4]byte{1, 2, 3, 4}, 9600, 91)
	ridB := mkRidUDP_E2E([4]byte{5, 6, 7, 8}, 9601, 92)

	board.Registry().Register(ridA, tidA, ticketA)
	board.Registry().Register(ridB, tidB, ticketB)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Agent A sends.
	restoreA := setAgentEnvUDP(addr, ridStrA, tidA, ticketA)
	var sendOut bytes.Buffer
	if err := agent.Send(ctx,
		[]string{"--topic", "topic/udp-e2e", "--data", `{"msg":"udp-hello"}`},
		nil,
		&sendOut,
	); err != nil {
		restoreA()
		t.Fatalf("agent.Send (udp): %v", err)
	}
	restoreA()

	if !strings.Contains(sendOut.String(), "ok") {
		t.Errorf("Send output missing 'ok': %s", sendOut.String())
	}

	// Agent B waits.
	restoreB := setAgentEnvUDP(addr, ridStrB, tidB, ticketB)
	var waitOut bytes.Buffer
	if err := agent.Wait(ctx,
		[]string{"--topic", "topic/udp-e2e", "--timeout", "2s"},
		&waitOut,
	); err != nil {
		restoreB()
		t.Fatalf("agent.Wait (udp): %v", err)
	}
	restoreB()

	if !strings.Contains(waitOut.String(), "udp-hello") {
		t.Errorf("Wait output missing payload: %s", waitOut.String())
	}
}
