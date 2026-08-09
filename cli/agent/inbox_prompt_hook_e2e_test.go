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

// TestAgentCLI_E2E_Inbox_PromptHook_NoMessages verifies that an empty inbox
// emits nothing, so the hook contributes no context on a quiet turn.
func TestAgentCLI_E2E_Inbox_PromptHook_NoMessages(t *testing.T) {
	addr := freePortE2E(t)
	board, _ := startServerE2E(t, addr)

	const ridStr = "ws:1.2.3.4:9200-20"
	var ticket [16]byte
	ticket[0] = 0xF0
	tid := mkTidE2E(0x20)
	rid := mkRidE2E([4]byte{1, 2, 3, 4}, 9200, 20)
	board.Registry().Register(rid, tid, ticket)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	restore := setAgentEnv(addr, ridStr, tid, ticket)
	defer restore()

	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	var out bytes.Buffer
	if err := agent.Inbox(ctx, []string{"--user-prompt-submit-hook"}, &out); err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if got := out.String(); got != "" {
		t.Errorf("expected empty output for empty inbox, got %q", got)
	}
}

// TestAgentCLI_E2E_Inbox_PromptHook_SingleMessage is the regression case.
// One delivered message used to be emitted as one bare JSON-Lines record,
// which Claude Code parsed as a hook envelope and discarded whole. The
// envelope must be the outer object and the record must survive inside
// additionalContext.
func TestAgentCLI_E2E_Inbox_PromptHook_SingleMessage(t *testing.T) {
	addr := freePortE2E(t)
	board, _ := startServerE2E(t, addr)

	const (
		ridStrA = "ws:1.2.3.4:9201-21"
		ridStrB = "ws:5.6.7.8:9202-22"
	)
	var ticketA, ticketB [16]byte
	ticketA[0] = 0xF1
	ticketB[0] = 0xF2
	tidA := mkTidE2E(0x21)
	tidB := mkTidE2E(0x22)
	ridA := mkRidE2E([4]byte{1, 2, 3, 4}, 9201, 21)
	ridB := mkRidE2E([4]byte{5, 6, 7, 8}, 9202, 22)
	board.Registry().Register(ridA, tidA, ticketA)
	board.Registry().Register(ridB, tidB, ticketB)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	restoreB := setAgentEnv(addr, ridStrB, tidB, ticketB)
	var subOut bytes.Buffer
	if err := agent.Subscribe(ctx, []string{"--topic", "topic/prompt-hook-e2e"}, &subOut); err != nil {
		restoreB()
		t.Fatalf("Subscribe: %v", err)
	}
	restoreB()

	restoreA := setAgentEnv(addr, ridStrA, tidA, ticketA)
	var sendOut bytes.Buffer
	if err := agent.Send(ctx,
		[]string{"--topic", "topic/prompt-hook-e2e", "--data", `{"msg":"only-one"}`},
		nil, &sendOut); err != nil {
		restoreA()
		t.Fatalf("Send: %v", err)
	}
	restoreA()

	restoreB2 := setAgentEnv(addr, ridStrB, tidB, ticketB)
	defer restoreB2()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	var out bytes.Buffer
	if err := agent.Inbox(ctx, []string{"--user-prompt-submit-hook"}, &out); err != nil {
		t.Fatalf("Inbox: %v", err)
	}

	line := strings.TrimSpace(out.String())
	if line == "" {
		t.Fatal("expected hook output, got empty")
	}
	var rec struct {
		HookSpecificOutput struct {
			HookEventName     string `json:"hookEventName"`
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(line), &rec); err != nil {
		t.Fatalf("output not a single valid JSON object: %v; raw=%q", err, line)
	}
	if rec.HookSpecificOutput.HookEventName != "UserPromptSubmit" {
		t.Errorf("hookEventName = %q, want %q", rec.HookSpecificOutput.HookEventName, "UserPromptSubmit")
	}
	body := rec.HookSpecificOutput.AdditionalContext
	if !strings.Contains(body, "only-one") {
		t.Errorf("additionalContext missing payload: %q", body)
	}
	if !strings.Contains(body, `"topic":"topic/prompt-hook-e2e"`) {
		t.Errorf("additionalContext missing topic: %q", body)
	}
}

// The two hook envelopes are different shapes; asking for both is a
// configuration error and must fail before any board round-trip.
func TestAgentCLI_E2E_Inbox_HookModes_MutuallyExclusive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var out bytes.Buffer
	err := agent.Inbox(ctx, []string{"--stop-hook", "--user-prompt-submit-hook"}, &out)
	if err == nil {
		t.Fatal("expected an error when both hook modes are set")
	}
	if !strings.Contains(err.Error(), "mutually exclusive") {
		t.Errorf("error = %v, want it to mention mutual exclusion", err)
	}
	if out.Len() != 0 {
		t.Errorf("expected no output, got %q", out.String())
	}
}
