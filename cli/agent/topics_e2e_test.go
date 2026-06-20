package agent_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli/agent"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/server"
)

func TestAgentCLI_E2E_Topics(t *testing.T) {
	addr := freePortE2E(t)
	board, srv := startServerE2E(t, addr)

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

	// Inject TaskStore entries so agentCallerCaps can resolve Capability_InfoGlobal.
	// In production, every agentboard agent has a TaskStore entry created at task
	// spawn time (sendAssign). The E2E test bypasses that path, so we replay a
	// synthetic task_created event here to make the gate work the same way.
	srv.Tasks().ReplayEvents([]server.WALEvent{
		{
			Type:         "task_created",
			TaskID:       hex.EncodeToString(tidA.Id[:]),
			Capabilities: uint32(protocol.Capability_InfoGlobal),
		},
		{
			Type:         "task_created",
			TaskID:       hex.EncodeToString(tidB.Id[:]),
			Capabilities: uint32(protocol.Capability_InfoGlobal),
		},
	})

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

	// B lists topics — requires Capability_InfoGlobal (now injected above).
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

// TestAgentCLI_E2E_Topics_NoInfoGlobal verifies that an agent whose TaskStore
// entry lacks Capability_InfoGlobal receives an empty topic list from the gate.
func TestAgentCLI_E2E_Topics_NoInfoGlobal(t *testing.T) {
	addr := freePortE2E(t)
	board, srv := startServerE2E(t, addr)

	const ridStrC = "ws:9.10.11.12:9302-33"
	var ticketC [16]byte
	ticketC[0] = 0xA3
	tidC := mkTidE2E(0x33)
	ridC := mkRidE2E([4]byte{9, 10, 11, 12}, 9302, 33)
	board.Registry().Register(ridC, tidC, ticketC)

	// Inject entry WITHOUT InfoGlobal (Capability_None = 0).
	srv.Tasks().ReplayEvents([]server.WALEvent{
		{
			Type:         "task_created",
			TaskID:       hex.EncodeToString(tidC.Id[:]),
			Capabilities: uint32(protocol.Capability_None),
		},
	})

	// Also publish a topic so there is something to list.
	// Use a second agent (no TaskStore entry needed just to send).
	const ridStrD = "ws:1.2.3.4:9303-34"
	var ticketD [16]byte
	ticketD[0] = 0xA4
	tidD := mkTidE2E(0x34)
	ridD := mkRidE2E([4]byte{1, 2, 3, 4}, 9303, 34)
	board.Registry().Register(ridD, tidD, ticketD)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	restoreD := setAgentEnv(addr, ridStrD, tidD, ticketD)
	if err := agent.Send(ctx, []string{"--topic", "secret/topic", "--data", `{"x":1}`}, nil, &bytes.Buffer{}); err != nil {
		restoreD()
		t.Fatalf("Send: %v", err)
	}
	restoreD()

	// C lists topics — has no InfoGlobal, should receive an empty list.
	restoreC := setAgentEnv(addr, ridStrC, tidC, ticketC)
	defer restoreC()
	var out bytes.Buffer
	if err := agent.Topics(ctx, nil, &out); err != nil {
		t.Fatalf("Topics: %v", err)
	}
	got := out.String()
	// The gate returns an empty list; the JSON output should not contain the published topic.
	if strings.Contains(got, "secret/topic") {
		t.Errorf("confined agent (no InfoGlobal) should not see topic list; got: %s", got)
	}
}
