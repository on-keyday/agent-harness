package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli/agent"
)

// TestAgentCLI_E2E_Inbox_StopHook_NoMessages verifies that --stop-hook with
// an empty inbox emits nothing (so the Stop hook lets the agent stop normally).
func TestAgentCLI_E2E_Inbox_StopHook_NoMessages(t *testing.T) {
	addr := freePortE2E(t)
	board := startServerE2E(t, addr)

	const ridStr = "ws:1.2.3.4:9100-10"
	var ticket [16]byte
	ticket[0] = 0xE0
	tid := mkTidE2E(0x10)
	rid := mkRidE2E([4]byte{1, 2, 3, 4}, 9100, 10)
	board.Registry().Register(rid, tid, ticket)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	restore := setAgentEnv(addr, ridStr, tid, ticket)
	defer restore()

	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	var out bytes.Buffer
	if err := agent.Inbox(ctx, []string{"--stop-hook"}, &out); err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if got := out.String(); got != "" {
		t.Errorf("expected empty output for empty inbox with --stop-hook, got %q", got)
	}
}

// TestAgentCLI_E2E_Inbox_StopHook_WithMessages verifies that --stop-hook with
// a non-empty inbox emits a single {"decision":"block","reason":...} JSON line
// whose reason embeds the JSON-Lines records of the buffered messages.
func TestAgentCLI_E2E_Inbox_StopHook_WithMessages(t *testing.T) {
	addr := freePortE2E(t)
	board := startServerE2E(t, addr)

	const (
		ridStrA = "ws:1.2.3.4:9101-11"
		ridStrB = "ws:5.6.7.8:9102-12"
	)
	var ticketA, ticketB [16]byte
	ticketA[0] = 0xE1
	ticketB[0] = 0xE2
	tidA := mkTidE2E(0x11)
	tidB := mkTidE2E(0x12)
	ridA := mkRidE2E([4]byte{1, 2, 3, 4}, 9101, 11)
	ridB := mkRidE2E([4]byte{5, 6, 7, 8}, 9102, 12)
	board.Registry().Register(ridA, tidA, ticketA)
	board.Registry().Register(ridB, tidB, ticketB)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// B subscribes so A's send is buffered for it.
	restoreB := setAgentEnv(addr, ridStrB, tidB, ticketB)
	var subOut bytes.Buffer
	if err := agent.Subscribe(ctx, []string{"--topic", "topic/stop-hook-e2e"}, &subOut); err != nil {
		restoreB()
		t.Fatalf("Subscribe: %v", err)
	}
	restoreB()

	// A sends.
	restoreA := setAgentEnv(addr, ridStrA, tidA, ticketA)
	var sendOut bytes.Buffer
	if err := agent.Send(ctx,
		[]string{"--topic", "topic/stop-hook-e2e", "--data", `{"msg":"wake-up"}`},
		nil, &sendOut); err != nil {
		restoreA()
		t.Fatalf("Send: %v", err)
	}
	restoreA()

	// B reads inbox in --stop-hook mode.
	restoreB2 := setAgentEnv(addr, ridStrB, tidB, ticketB)
	defer restoreB2()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	var out bytes.Buffer
	if err := agent.Inbox(ctx, []string{"--stop-hook"}, &out); err != nil {
		t.Fatalf("Inbox: %v", err)
	}

	line := strings.TrimSpace(out.String())
	if line == "" {
		t.Fatal("expected stop-hook output, got empty")
	}
	var rec map[string]string
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("output not valid JSON: %v; raw=%q", err, line)
	}
	if rec["decision"] != "block" {
		t.Errorf("decision = %q, want %q", rec["decision"], "block")
	}
	if !strings.Contains(rec["reason"], "wake-up") {
		t.Errorf("reason missing payload: %q", rec["reason"])
	}
	if !strings.Contains(rec["reason"], `"topic":"topic/stop-hook-e2e"`) {
		t.Errorf("reason missing topic: %q", rec["reason"])
	}
}
