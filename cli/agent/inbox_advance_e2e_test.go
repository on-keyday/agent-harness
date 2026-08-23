package agent_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli/agent"
)

// The two reads differ in exactly one way: the hook one moves the server-side
// delivery mark and the plain one does not. Under the old client cursor this
// distinction lived in --since-last/--commit, a flag pair the skill had to tell
// agents not to touch.
func TestAgentCLI_E2E_PlainInboxIsIdempotentAndDoesNotAdvance(t *testing.T) {
	addr := freePortE2E(t)
	board, _ := startServerE2E(t, addr)

	const (
		ridStrA = "ws:1.2.3.4:9301-31"
		ridStrB = "ws:5.6.7.8:9302-32"
	)
	var ticketA, ticketB [16]byte
	ticketA[0] = 0xE1
	ticketB[0] = 0xE2
	tidA := mkTidE2E(0x31)
	tidB := mkTidE2E(0x32)
	ridA := mkRidE2E([4]byte{1, 2, 3, 4}, 9301, 31)
	ridB := mkRidE2E([4]byte{5, 6, 7, 8}, 9302, 32)
	board.Registry().Register(ridA, tidA, ticketA)
	board.Registry().Register(ridB, tidB, ticketB)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	restoreB := setAgentEnv(addr, ridStrB, tidB, ticketB)
	var subOut bytes.Buffer
	if err := agent.Subscribe(ctx, []string{"--topic", "topic/advance-e2e"}, &subOut); err != nil {
		restoreB()
		t.Fatalf("Subscribe: %v", err)
	}
	restoreB()

	restoreA := setAgentEnv(addr, ridStrA, tidA, ticketA)
	var sendOut bytes.Buffer
	if err := agent.Send(ctx,
		[]string{"--topic", "topic/advance-e2e", "--data", `{"msg":"marked-once"}`},
		nil, &sendOut); err != nil {
		restoreA()
		t.Fatalf("Send: %v", err)
	}
	restoreA()

	restoreB2 := setAgentEnv(addr, ridStrB, tidB, ticketB)
	defer restoreB2()

	// Two plain reads: same content both times, and neither consumes.
	var first, second bytes.Buffer
	if err := agent.Inbox(ctx, []string{"--json"}, &first); err != nil {
		t.Fatalf("first plain Inbox: %v", err)
	}
	if err := agent.Inbox(ctx, []string{"--json"}, &second); err != nil {
		t.Fatalf("second plain Inbox: %v", err)
	}
	if !strings.Contains(first.String(), "marked-once") {
		t.Fatalf("plain inbox = %q, want the published message", first.String())
	}
	if first.String() != second.String() {
		t.Fatalf("plain inbox is not idempotent:\n first=%q\nsecond=%q", first.String(), second.String())
	}

	// The hook read still sees it -- the plain reads consumed nothing.
	var hook bytes.Buffer
	if err := agent.Inbox(ctx, []string{"--json", "--user-prompt-submit-hook"}, &hook); err != nil {
		t.Fatalf("hook Inbox: %v", err)
	}
	if !strings.Contains(hook.String(), "marked-once") {
		t.Fatalf("hook read = %q, want the message the plain reads did not consume", hook.String())
	}

	// ...and it advanced, so the next hook read is empty.
	var again bytes.Buffer
	if err := agent.Inbox(ctx, []string{"--json", "--user-prompt-submit-hook"}, &again); err != nil {
		t.Fatalf("second hook Inbox: %v", err)
	}
	if strings.Contains(again.String(), "marked-once") {
		t.Fatalf("second hook read = %q, want the message gone", again.String())
	}

	// A plain read still shows it: the ring is untouched, only the mark moved.
	var afterHook bytes.Buffer
	if err := agent.Inbox(ctx, []string{"--json"}, &afterHook); err != nil {
		t.Fatalf("plain Inbox after hook: %v", err)
	}
	if !strings.Contains(afterHook.String(), "marked-once") {
		t.Fatalf("plain inbox after the hook = %q, want the ring still holding the message", afterHook.String())
	}
}

// The retired flags must not merely be ignored -- a hook line still carrying
// them has to fail loudly rather than silently reading with different
// semantics than the caller asked for.
func TestAgentCLI_E2E_RetiredInboxFlagsAreRejected(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	for _, args := range [][]string{
		{"--stop-hook"},
		{"--since-last"},
		{"--since-last", "--commit"},
	} {
		var out bytes.Buffer
		if err := agent.Inbox(ctx, args, &out); err == nil {
			t.Errorf("agent inbox %v: expected a flag error, got nil", args)
		}
	}
}
