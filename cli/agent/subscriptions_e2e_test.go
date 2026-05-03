package agent_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli/agent"
)

func TestAgentCLI_E2E_Subscriptions(t *testing.T) {
	addr := freePortE2E(t)
	board := startServerE2E(t, addr)

	const ridStr = "ws:1.2.3.4:9400-41"
	var ticket [16]byte
	ticket[0] = 0xB1
	tid := mkTidE2E(0x41)
	rid := mkRidE2E([4]byte{1, 2, 3, 4}, 9400, 41)
	board.Registry().Register(rid, tid, ticket)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	restore := setAgentEnv(addr, ridStr, tid, ticket)
	defer restore()

	for _, topic := range []string{"alpha/x", "beta/y", "gamma/z"} {
		if err := agent.Subscribe(ctx, []string{"--topic", topic}, &bytes.Buffer{}); err != nil {
			t.Fatalf("Subscribe %s: %v", topic, err)
		}
	}

	var out bytes.Buffer
	if err := agent.Subscriptions(ctx, nil, &out); err != nil {
		t.Fatalf("Subscriptions: %v", err)
	}
	got := out.String()
	for _, topic := range []string{"alpha/x", "beta/y", "gamma/z"} {
		if !strings.Contains(got, `"pattern":"`+topic+`"`) {
			t.Errorf("output missing pattern %s: %s", topic, got)
		}
	}
}

// TestAgentCLI_E2E_SubscribeSelf verifies that `subscribe --self` derives
// the inbound topic from HARNESS_TASK_ID via SelfTopic and successfully
// subscribes — i.e. the runner's auto-injected SessionStart hook works
// without out-of-band shell expansion.
func TestAgentCLI_E2E_SubscribeSelf(t *testing.T) {
	addr := freePortE2E(t)
	board := startServerE2E(t, addr)

	const ridStr = "ws:1.2.3.4:9500-51"
	var ticket [16]byte
	ticket[0] = 0x5E
	tid := mkTidE2E(0x5F)
	tid.Id[1] = 0xA1
	tid.Id[2] = 0xB2
	tid.Id[3] = 0xC3
	rid := mkRidE2E([4]byte{1, 2, 3, 4}, 9500, 51)
	board.Registry().Register(rid, tid, ticket)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	restore := setAgentEnv(addr, ridStr, tid, ticket)
	defer restore()

	if err := agent.Subscribe(ctx, []string{"--self"}, &bytes.Buffer{}); err != nil {
		t.Fatalf("Subscribe --self: %v", err)
	}

	var out bytes.Buffer
	if err := agent.Subscriptions(ctx, nil, &out); err != nil {
		t.Fatalf("Subscriptions: %v", err)
	}
	want := agent.SelfTopic(tid)
	if !strings.Contains(out.String(), `"pattern":"`+want+`"`) {
		t.Errorf("Subscriptions did not list self-topic %q: %s", want, out.String())
	}
}

// TestAgentCLI_SubscribeSelfRejectsTopicConflict guards the API: --self and
// --topic together is a usage error, not a silently-overridden flag — peers
// reading the SKILL.md should not be able to combine them by accident.
func TestAgentCLI_SubscribeSelfRejectsTopicConflict(t *testing.T) {
	ctx := context.Background()
	err := agent.Subscribe(ctx, []string{"--self", "--topic", "x"}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutually-exclusive error, got %v", err)
	}
}
