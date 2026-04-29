package agent_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli/agent"
)

func TestAgentCLI_E2E_Topics(t *testing.T) {
	addr := freePortE2E(t)
	board := startServerE2E(t, addr)

	const (
		ridStrA = "ws:1.2.3.4:9300-31"
		ridStrB = "ws:5.6.7.8:9301-32"
	)
	var ticketA, ticketB [16]byte
	ticketA[0] = 0xA1
	ticketB[0] = 0xA2
	tidA := mkTidE2E(0x31)
	tidB := mkTidE2E(0x32)
	ridA := mkRidE2E([4]byte{1, 2, 3, 4}, 9300, 31)
	ridB := mkRidE2E([4]byte{5, 6, 7, 8}, 9301, 32)
	board.Registry().Register(ridA, tidA, ticketA)
	board.Registry().Register(ridB, tidB, ticketB)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// A publishes 2 messages on alpha/x and 1 on beta/y
	restoreA := setAgentEnv(addr, ridStrA, tidA, ticketA)
	for _, args := range [][]string{
		{"--topic", "alpha/x", "--data", `{"i":1}`},
		{"--topic", "alpha/x", "--data", `{"i":2}`},
		{"--topic", "beta/y", "--data", `{"i":3}`},
	} {
		if err := agent.Send(ctx, args, nil, &bytes.Buffer{}); err != nil {
			restoreA()
			t.Fatalf("Send %v: %v", args, err)
		}
	}
	restoreA()

	// B lists topics
	restoreB := setAgentEnv(addr, ridStrB, tidB, ticketB)
	defer restoreB()
	var out bytes.Buffer
	if err := agent.Topics(ctx, nil, &out); err != nil {
		t.Fatalf("Topics: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, `"name":"alpha/x"`) {
		t.Errorf("output missing alpha/x: %s", got)
	}
	if !strings.Contains(got, `"name":"beta/y"`) {
		t.Errorf("output missing beta/y: %s", got)
	}
	if !strings.Contains(got, `"msg_count":2`) {
		t.Errorf("output missing msg_count:2 for alpha/x: %s", got)
	}
}
