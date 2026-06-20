package agent_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli/agent"
)

// TestAgentCLI_E2E_Sender_RoundTrip verifies that when agent A sends a message,
// agent B's wait/inbox output includes from_runner_id / from_task_id /
// from_hostname populated from server-side attestation.
func TestAgentCLI_E2E_Sender_RoundTrip(t *testing.T) {
	addr := freePortE2E(t)
	board, _ := startServerE2E(t, addr)

	const (
		ridStrA = "ws:1.2.3.4:9200-21"
		ridStrB = "ws:5.6.7.8:9201-22"
	)
	var ticketA, ticketB [16]byte
	ticketA[0] = 0xF1
	ticketB[0] = 0xF2
	tidA := mkTidE2E(0x21)
	tidB := mkTidE2E(0x22)
	ridA := mkRidE2E([4]byte{1, 2, 3, 4}, 9200, 21)
	ridB := mkRidE2E([4]byte{5, 6, 7, 8}, 9201, 22)
	board.Registry().Register(ridA, tidA, ticketA)
	board.Registry().Register(ridB, tidB, ticketB)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// A sends with hostname host-A
	restoreA := setAgentEnv(addr, ridStrA, tidA, ticketA)
	t.Setenv("HARNESS_HOSTNAME", "host-A")
	if err := agent.Send(ctx,
		[]string{"--topic", "topic/sender-test", "--data", `{"msg":"hello"}`},
		nil, &bytes.Buffer{}); err != nil {
		restoreA()
		t.Fatalf("Send: %v", err)
	}
	restoreA()

	// B reads via Wait
	restoreB := setAgentEnv(addr, ridStrB, tidB, ticketB)
	defer restoreB()
	var waitOut bytes.Buffer
	if err := agent.Wait(ctx,
		[]string{"--topic", "topic/sender-test", "--timeout", "2s"},
		&waitOut); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	got := waitOut.String()
	if !strings.Contains(got, `"hostname":"host-A"`) {
		t.Errorf("output missing hostname host-A in from block: %s", got)
	}
	// task IDs are emitted as hex; tidA[0]=0x21 so prefix "21"
	if !strings.Contains(got, `"task_id":"21`) {
		t.Errorf("output missing task_id beginning with 21: %s", got)
	}
}
