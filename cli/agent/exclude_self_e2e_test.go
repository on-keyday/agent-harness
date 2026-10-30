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

// A sender subscribed to the topic it publishes on gets its own message back —
// unless it asks not to. Driven through the argv parser, so the flag is proven
// to reach the wire, not only the board.
func TestAgentCLI_E2E_ExcludeSelf(t *testing.T) {
	addr := freePortE2E(t)
	board, _ := startServerE2E(t, addr)

	var ticket [16]byte
	ticket[0] = 0xC9
	tid := mkTidE2E(0x59)
	rid := mkRidE2E([4]byte{1, 2, 3, 4}, 9509, 59)
	e2eRegister(t, board, rid, tid, ticket)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	restore := setAgentEnv(addr, rid, tid, ticket)
	defer restore()

	topic := agent.SelfTopic(tid) + ".grp"
	var subOut, sendOut bytes.Buffer
	if err := agent.Subscribe(ctx, []string{"--topic", topic}, &subOut); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	if err := agent.Send(ctx, []string{"--topic", topic, "--exclude-self", "--data", "mine-excluded"}, nil, &sendOut); err != nil {
		t.Fatalf("Send: %v", err)
	}
	var res struct {
		Seq         uint64 `json:"seq"`
		DeliveredTo int    `json:"delivered_to"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(sendOut.Bytes()), &res); err != nil {
		t.Fatalf("send output %q: %v", sendOut.String(), err)
	}
	if res.DeliveredTo != 0 {
		t.Errorf("delivered_to = %d, want 0 (the sender is excluded and nobody else subscribes)", res.DeliveredTo)
	}

	var inboxOut bytes.Buffer
	if err := agent.Inbox(ctx, []string{"--json"}, &inboxOut); err != nil {
		t.Fatalf("Inbox: %v", err)
	}
	if strings.Contains(inboxOut.String(), "mine-excluded") {
		t.Errorf("inbox shows the sender's own excluded message: %s", inboxOut.String())
	}

	var readOut bytes.Buffer
	if err := agent.Read(ctx, []string{itoa(res.Seq)}, &readOut); err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(readOut.String(), "mine-excluded") {
		t.Errorf("agent read %d = %q, want the body", res.Seq, readOut.String())
	}
}
