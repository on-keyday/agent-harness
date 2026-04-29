package agent_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli/agent"
)

// TestAgentCLI_E2E_Inbox_PeekIsIdempotent_AfterCommit drives the user-facing
// scenario: a UserPromptSubmit-style hook calls `inbox --since-last --commit`
// to drain pending messages and advance the live cursor; afterwards a manual
// `inbox --since-last` (no --commit) must return the **same batch** the hook
// just delivered, repeatedly. Without the prev-cursor snapshot, peek would
// see "nothing new since cursor" and the agent would mistakenly conclude its
// inbox is empty when in fact those messages were just injected into its
// prompt context.
func TestAgentCLI_E2E_Inbox_PeekIsIdempotent_AfterCommit(t *testing.T) {
	addr := freePortE2E(t)
	board := startServerE2E(t, addr)

	const (
		ridStrA = "ws:1.2.3.4:9300-31"
		ridStrB = "ws:5.6.7.8:9301-32"
	)
	var ticketA, ticketB [16]byte
	ticketA[0] = 0xC1
	ticketB[0] = 0xC2
	tidA := mkTidE2E(0x31)
	tidB := mkTidE2E(0x32)
	ridA := mkRidE2E([4]byte{1, 2, 3, 4}, 9300, 31)
	ridB := mkRidE2E([4]byte{5, 6, 7, 8}, 9301, 32)
	board.Registry().Register(ridA, tidA, ticketA)
	board.Registry().Register(ridB, tidB, ticketB)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// A subscribes so B's sends get retained for A's inbox.
	restoreA := setAgentEnv(addr, ridStrA, tidA, ticketA)
	var subOut bytes.Buffer
	if err := agent.Subscribe(ctx, []string{"--topic", "topic/peek-e2e"}, &subOut); err != nil {
		restoreA()
		t.Fatalf("Subscribe: %v", err)
	}
	restoreA()

	// B sends two messages.
	restoreB := setAgentEnv(addr, ridStrB, tidB, ticketB)
	for _, payload := range []string{`{"n":1}`, `{"n":2}`} {
		var sendOut bytes.Buffer
		if err := agent.Send(ctx,
			[]string{"--topic", "topic/peek-e2e", "--data", payload},
			nil, &sendOut); err != nil {
			restoreB()
			t.Fatalf("Send %s: %v", payload, err)
		}
	}
	restoreB()

	// Now A consumes its inbox in three forms: hook-style commit, then two
	// manual peeks. They must all produce the same payloads.
	restoreA2 := setAgentEnv(addr, ridStrA, tidA, ticketA)
	defer restoreA2()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	var commitOut bytes.Buffer
	if err := agent.Inbox(ctx, []string{"--since-last", "--commit"}, &commitOut); err != nil {
		t.Fatalf("Inbox commit: %v", err)
	}
	commitText := commitOut.String()
	if !strings.Contains(commitText, `"n":1`) || !strings.Contains(commitText, `"n":2`) {
		t.Fatalf("commit output missing one of the payloads: %q", commitText)
	}

	// First peek (after commit): must show the same batch.
	var peek1 bytes.Buffer
	if err := agent.Inbox(ctx, []string{"--since-last"}, &peek1); err != nil {
		t.Fatalf("Inbox peek1: %v", err)
	}
	if !strings.Contains(peek1.String(), `"n":1`) || !strings.Contains(peek1.String(), `"n":2`) {
		t.Errorf("peek1 missing payloads delivered by previous commit; got %q", peek1.String())
	}

	// Second peek: idempotent — must equal first peek byte-for-byte.
	var peek2 bytes.Buffer
	if err := agent.Inbox(ctx, []string{"--since-last"}, &peek2); err != nil {
		t.Fatalf("Inbox peek2: %v", err)
	}
	if peek1.String() != peek2.String() {
		t.Errorf("peek not idempotent:\n  peek1=%q\n  peek2=%q", peek1.String(), peek2.String())
	}

	// A new send arrives. Peek should now include it (peek reads everything
	// after the prev-cursor, which is still 0 in this scenario, so all three
	// messages appear).
	restoreB2 := setAgentEnv(addr, ridStrB, tidB, ticketB)
	var sendOut bytes.Buffer
	if err := agent.Send(ctx,
		[]string{"--topic", "topic/peek-e2e", "--data", `{"n":3}`},
		nil, &sendOut); err != nil {
		restoreB2()
		t.Fatalf("Send n=3: %v", err)
	}
	restoreB2()

	var peek3 bytes.Buffer
	if err := agent.Inbox(ctx, []string{"--since-last"}, &peek3); err != nil {
		t.Fatalf("Inbox peek3: %v", err)
	}
	if !strings.Contains(peek3.String(), `"n":3`) {
		t.Errorf("peek3 missing newly arrived payload: %q", peek3.String())
	}

	// A second commit advances live past n=3. The new prev-cursor is the
	// previous live (max seq of n=1 + n=2). Peek now returns ONLY n=3
	// (msgs > prev), not n=1 / n=2 anymore.
	var commit2 bytes.Buffer
	if err := agent.Inbox(ctx, []string{"--since-last", "--commit"}, &commit2); err != nil {
		t.Fatalf("Inbox commit2: %v", err)
	}
	if !strings.Contains(commit2.String(), `"n":3`) {
		t.Errorf("commit2 missing n=3: %q", commit2.String())
	}
	if strings.Contains(commit2.String(), `"n":1`) {
		t.Errorf("commit2 should not have re-delivered n=1: %q", commit2.String())
	}

	var peek4 bytes.Buffer
	if err := agent.Inbox(ctx, []string{"--since-last"}, &peek4); err != nil {
		t.Fatalf("Inbox peek4: %v", err)
	}
	if !strings.Contains(peek4.String(), `"n":3`) {
		t.Errorf("peek4 missing n=3 (just-delivered batch): %q", peek4.String())
	}
	if strings.Contains(peek4.String(), `"n":1`) || strings.Contains(peek4.String(), `"n":2`) {
		t.Errorf("peek4 leaked older batch (should only show last batch): %q", peek4.String())
	}
}
