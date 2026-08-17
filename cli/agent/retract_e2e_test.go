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

// TestAgentCLI_E2E_Retract_NoCapabilityNeeded is the whole point of the verb:
// a task holding Capability_None — which cannot purge anything — withdraws its
// own message, and a since=0 re-read (the path a context-reset agent takes)
// stops resurfacing it. The operator's view still has it.
func TestAgentCLI_E2E_Retract_NoCapabilityNeeded(t *testing.T) {
	addr := freePortE2E(t)
	board, srv := startServerE2E(t, addr)

	const ridStr = "ws:1.2.3.4:9600-71"
	var ticket [16]byte
	ticket[0] = 0x71
	sender := mkTidE2E(0x71)
	rid := mkRidE2E([4]byte{1, 2, 3, 4}, 9600, 71)
	board.Registry().Register(rid, sender, ticket)

	srv.Tasks().ReplayEvents([]server.WALEvent{
		{
			Type:         "task_created",
			TaskID:       hex.EncodeToString(sender.Id[:]),
			Capabilities: uint32(protocol.Capability_None),
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	restore := setAgentEnv(addr, ridStr, sender, ticket)
	defer restore()

	// Subscribe so the sender's own since=0 read can see the topic — this is
	// the re-read path a reset agent walks.
	if err := agent.Subscribe(ctx, []string{"--topic", "chat.instruct"}, &bytes.Buffer{}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	var sendOut bytes.Buffer
	if err := agent.Send(ctx, []string{"--topic", "chat.instruct", "--data", `{"do":"the thing"}`}, nil, &sendOut); err != nil {
		t.Fatalf("Send: %v", err)
	}
	seq := seqFromRetractTestOutput(t, sendOut.String())

	var before bytes.Buffer
	if err := agent.Inbox(ctx, []string{"--since", "0"}, &before); err != nil {
		t.Fatalf("Inbox before: %v", err)
	}
	if !strings.Contains(before.String(), "the thing") {
		t.Fatalf("inbox before retract = %s, want the message", before.String())
	}

	var out bytes.Buffer
	if err := agent.Retract(ctx, []string{seq}, &out); err != nil {
		t.Fatalf("Retract: %v", err)
	}
	if !strings.Contains(out.String(), `"status":"ok"`) {
		t.Fatalf("retract output = %s, want status ok (no capability is required)", out.String())
	}

	var after bytes.Buffer
	if err := agent.Inbox(ctx, []string{"--since", "0"}, &after); err != nil {
		t.Fatalf("Inbox after: %v", err)
	}
	if strings.Contains(after.String(), "the thing") {
		t.Fatalf("inbox after retract = %s, want the withdrawn message gone", after.String())
	}

	// Gone for agents, kept for the operator: that asymmetry is what lets an
	// agent retract in seconds without shrinking the audit window.
	withdrawn, found := board.ListRetracted("chat.instruct")
	if !found || len(withdrawn) != 1 {
		t.Fatalf("operator view = (%d withdrawn, found=%v), want (1, true)", len(withdrawn), found)
	}
	if !strings.Contains(string(withdrawn[0].Payload), "the thing") {
		t.Errorf("withdrawn payload = %q, want it preserved for audit", withdrawn[0].Payload)
	}

	// Retracting again is an idempotent not_found.
	var again bytes.Buffer
	if err := agent.Retract(ctx, []string{seq}, &again); err != nil {
		t.Fatalf("Retract again: %v", err)
	}
	if !strings.Contains(again.String(), `"status":"not_found"`) {
		t.Errorf("second retract = %s, want not_found", again.String())
	}
}

// TestAgentCLI_E2E_Retract_NotTheAuthor: with no capability in the way,
// authorship is the only thing protecting one task's messages from another's
// retract. A non-author gets the same not_found a missing seq gets — telling
// them apart would confirm the existence of any seq on any topic.
func TestAgentCLI_E2E_Retract_NotTheAuthor(t *testing.T) {
	addr := freePortE2E(t)
	board, srv := startServerE2E(t, addr)

	const ridStr = "ws:1.2.3.4:9600-72"
	rid := mkRidE2E([4]byte{1, 2, 3, 4}, 9600, 72)

	var authorTicket, otherTicket [16]byte
	authorTicket[0] = 0x72
	otherTicket[0] = 0x73
	author := mkTidE2E(0x72)
	other := mkTidE2E(0x73)
	board.Registry().Register(rid, author, authorTicket)
	board.Registry().Register(rid, other, otherTicket)

	srv.Tasks().ReplayEvents([]server.WALEvent{
		{Type: "task_created", TaskID: hex.EncodeToString(author.Id[:]), Capabilities: uint32(protocol.Capability_All)},
		{Type: "task_created", TaskID: hex.EncodeToString(other.Id[:]), Capabilities: uint32(protocol.Capability_All)},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	restoreAuthor := setAgentEnv(addr, ridStr, author, authorTicket)
	var sendOut bytes.Buffer
	if err := agent.Send(ctx, []string{"--topic", "chat.shared", "--data", `{"mine":true}`}, nil, &sendOut); err != nil {
		t.Fatalf("Send: %v", err)
	}
	seq := seqFromRetractTestOutput(t, sendOut.String())
	restoreAuthor()

	// A different task, holding EVERY capability, still cannot withdraw it.
	restoreOther := setAgentEnv(addr, ridStr, other, otherTicket)
	var out bytes.Buffer
	if err := agent.Retract(ctx, []string{seq}, &out); err != nil {
		t.Fatalf("Retract by non-author: %v", err)
	}
	restoreOther()
	if !strings.Contains(out.String(), `"status":"not_found"`) {
		t.Fatalf("non-author retract = %s, want not_found", out.String())
	}
	if msgs, _ := board.ListRetained("chat.shared"); len(msgs) != 1 {
		t.Fatalf("live ring = %d after a refused retract, want the message untouched", len(msgs))
	}

	// The author can.
	restoreAuthor2 := setAgentEnv(addr, ridStr, author, authorTicket)
	defer restoreAuthor2()
	var ok bytes.Buffer
	if err := agent.Retract(ctx, []string{seq}, &ok); err != nil {
		t.Fatalf("Retract by author: %v", err)
	}
	if !strings.Contains(ok.String(), `"status":"ok"`) {
		t.Fatalf("author retract = %s, want ok", ok.String())
	}
}

// seqFromRetractTestOutput pulls the published seq out of `agent send`'s JSON
// line. Kept as a string: board seq is UnixNano-seeded and the CLI takes it as
// text anyway, so parsing to a number here would only add a rounding hazard.
func seqFromRetractTestOutput(t *testing.T, out string) string {
	t.Helper()
	const key = `"seq":`
	i := strings.Index(out, key)
	if i < 0 {
		t.Fatalf("send output %q carries no seq", out)
	}
	rest := out[i+len(key):]
	end := strings.IndexAny(rest, ",}\n")
	if end < 0 {
		t.Fatalf("send output %q has an unterminated seq", out)
	}
	return strings.Trim(strings.TrimSpace(rest[:end]), `"`)
}
