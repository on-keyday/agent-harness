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

// TestAgentCLI_E2E_Inbox_PromptHook_OmitsOversizeBody wires the emit-level
// guard to the command that actually feeds an agent's prompt. The guard living
// in a helper proves nothing on its own — the failure mode is a helper that
// exists and a call site that never picks it, which looks identical to working
// code until a large message arrives.
//
// The same message read WITHOUT a hook flag must still carry the body: that is
// the path the omitted record points at, so truncating both would strand it.
func TestAgentCLI_E2E_Inbox_PromptHook_OmitsOversizeBody(t *testing.T) {
	const body = 100 << 10 // past the hook's 64KiB inline limit

	addr := freePortE2E(t)
	board, _ := startServerE2EWithMaxPayload(t, addr, 256<<10)

	const (
		ridStrA = "ws:1.2.3.4:9210-31"
		ridStrB = "ws:5.6.7.8:9211-32"
	)
	ridA := mkRidE2E([4]byte{1, 2, 3, 4}, 9210, 31)
	ridB := mkRidE2E([4]byte{5, 6, 7, 8}, 9211, 32)
	tidA, tidB := mkTidE2E(0x31), mkTidE2E(0x32)
	var ticketA, ticketB [16]byte
	ticketA[0], ticketB[0] = 0xE1, 0xE2
	board.Registry().Register(ridA, tidA, ticketA)
	board.Registry().Register(ridB, tidB, ticketB)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	restoreB := setAgentEnv(addr, ridStrB, tidB, ticketB)
	var subOut bytes.Buffer
	if err := agent.Subscribe(ctx, []string{"--topic", "topic/guard"}, &subOut); err != nil {
		restoreB()
		t.Fatalf("Subscribe: %v", err)
	}
	restoreB()

	restoreA := setAgentEnv(addr, ridStrA, tidA, ticketA)
	var sendOut bytes.Buffer
	if err := agent.Send(ctx,
		[]string{"--topic", "topic/guard", "--data", strings.Repeat("x", body)},
		nil, &sendOut); err != nil {
		restoreA()
		t.Fatalf("Send: %v", err)
	}
	restoreA()

	restoreB2 := setAgentEnv(addr, ridStrB, tidB, ticketB)
	defer restoreB2()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	var hookOut bytes.Buffer
	if err := agent.Inbox(ctx, []string{"--user-prompt-submit-hook"}, &hookOut); err != nil {
		t.Fatalf("Inbox --user-prompt-submit-hook: %v", err)
	}
	var env struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(hookOut.String())), &env); err != nil {
		t.Fatalf("hook output not a single JSON object: %v", err)
	}
	injected := env.HookSpecificOutput.AdditionalContext
	if !strings.Contains(injected, "payload_omitted") {
		t.Errorf("injected context does not mark the body omitted: %.200q", injected)
	}
	if len(injected) >= body {
		t.Errorf("injected %d chars for a %d-byte body: it was spliced into the prompt anyway",
			len(injected), body)
	}
	if !strings.Contains(injected, "read_with") {
		t.Error("injected context omits the body without saying how to fetch it")
	}

	var plainOut bytes.Buffer
	if err := agent.Inbox(ctx, []string{"--json"}, &plainOut); err != nil {
		t.Fatalf("Inbox --json: %v", err)
	}
	if !strings.Contains(plainOut.String(), "payload_b64") {
		t.Fatal("plain inbox dropped the body; read_with points here")
	}
	if plainOut.Len() < body {
		t.Errorf("plain inbox emitted %d chars for a %d-byte body: it truncated too", plainOut.Len(), body)
	}
}
