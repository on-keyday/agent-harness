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

// There is one hook mode now, so there is nothing to be mutually exclusive
// with: --stop-hook was deleted along with the Stop hook itself, which
// runner/settings.go retired and pruneStaleHarnessHooks removes from
// worktrees. TestAgentCLI_E2E_RetiredInboxFlagsAreRejected covers that it
// fails rather than being ignored.

// TestAgentCLI_E2E_Inbox_ProseArrivesReadable runs a non-JSON body through the
// whole wire — send, the server's retained ring, the payload stream, emit — and
// pins the rendering at both exits. A prose instruction is what a relayed human
// request looks like, and it used to arrive with payload_b64 as its ONLY body:
// unreadable to the hook path (claude) and to the polled plain read (codex,
// gemini, bash) alike. Multi-byte text is the case that matters, since a
// mangled decode is what a reader would have to notice.
func TestAgentCLI_E2E_Inbox_ProseArrivesReadable(t *testing.T) {
	addr := freePortE2E(t)
	board, _ := startServerE2E(t, addr)

	const (
		ridStrA = "ws:1.2.3.4:9203-23"
		ridStrB = "ws:5.6.7.8:9204-24"
		prose   = "指示: X を実装して\nY は触らないこと"
		topic   = "topic/prose-e2e"
	)
	var ticketA, ticketB [16]byte
	ticketA[0] = 0xF3
	ticketB[0] = 0xF4
	tidA := mkTidE2E(0x23)
	tidB := mkTidE2E(0x24)
	ridA := mkRidE2E([4]byte{1, 2, 3, 4}, 9203, 23)
	ridB := mkRidE2E([4]byte{5, 6, 7, 8}, 9204, 24)
	board.Registry().Register(ridA, tidA, ticketA)
	board.Registry().Register(ridB, tidB, ticketB)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	restoreB := setAgentEnv(addr, ridStrB, tidB, ticketB)
	var subOut bytes.Buffer
	if err := agent.Subscribe(ctx, []string{"--topic", topic}, &subOut); err != nil {
		restoreB()
		t.Fatalf("Subscribe: %v", err)
	}
	restoreB()

	restoreA := setAgentEnv(addr, ridStrA, tidA, ticketA)
	var sendOut bytes.Buffer
	if err := agent.Send(ctx, []string{"--topic", topic, "--data", prose}, nil, &sendOut); err != nil {
		restoreA()
		t.Fatalf("Send: %v", err)
	}
	restoreA()

	restoreB2 := setAgentEnv(addr, ridStrB, tidB, ticketB)
	defer restoreB2()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	// The plain read first: it moves no delivery position, so the hook read
	// after it still gets the same message.
	var plain bytes.Buffer
	if err := agent.Inbox(ctx, nil, &plain); err != nil {
		t.Fatalf("Inbox (plain): %v", err)
	}
	var plainRec map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(plain.String())), &plainRec); err != nil {
		t.Fatalf("plain record: %v; raw=%q", err, plain.String())
	}
	if got, _ := plainRec["payload_text"].(string); got != prose {
		t.Errorf("plain payload_text = %q, want %q", got, prose)
	}
	if _, ok := plainRec["payload_b64"]; !ok {
		t.Error("plain payload_b64 absent: the exact bytes must stay recoverable here")
	}

	var hookOut bytes.Buffer
	if err := agent.Inbox(ctx, []string{"--user-prompt-submit-hook"}, &hookOut); err != nil {
		t.Fatalf("Inbox (hook): %v", err)
	}
	var env struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(hookOut.String())), &env); err != nil {
		t.Fatalf("hook envelope: %v; raw=%q", err, hookOut.String())
	}
	var hookRec map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(env.HookSpecificOutput.AdditionalContext)), &hookRec); err != nil {
		t.Fatalf("hook record: %v; raw=%q", err, env.HookSpecificOutput.AdditionalContext)
	}
	if got, _ := hookRec["payload_text"].(string); got != prose {
		t.Errorf("hook payload_text = %q, want %q", got, prose)
	}
	if _, ok := hookRec["payload_b64"]; ok {
		t.Error("hook payload_b64 present: a readable body must not also be inlined as base64")
	}
}
