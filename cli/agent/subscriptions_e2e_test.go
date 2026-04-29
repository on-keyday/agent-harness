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
