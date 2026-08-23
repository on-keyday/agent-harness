package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/cli/agent"
)

// itoa renders a board seq for a --in-reply-to argument.
func itoa(n uint64) string { return strconv.FormatUint(n, 10) }

// lastSeqOnTopic reads the highest retained seq on a topic straight from the
// in-process board, which is how a test learns the seq a peer must reply to
// without driving a second CLI identity (setAgentEnv is process-global, so two
// concurrent identities race).
func lastSeqOnTopic(t *testing.T, b *agentboard.Board, topic string) uint64 {
	t.Helper()
	msgs, found := b.ListRetained(topic)
	if !found || len(msgs) == 0 {
		t.Fatalf("topic %s holds nothing to reply to", topic)
	}
	var max uint64
	for _, m := range msgs {
		if m.Seq > max {
			max = m.Seq
		}
	}
	return max
}

// topicPayloads concatenates a topic's retained payloads, for asserting that
// something is or is NOT there.
func topicPayloads(t *testing.T, b *agentboard.Board, topic string) string {
	t.Helper()
	msgs, _ := b.ListRetained(topic)
	var sb strings.Builder
	for _, m := range msgs {
		sb.Write(m.Payload)
		sb.WriteByte('\n')
	}
	return sb.String()
}

// The property the whole change exists for: the asker declares a destination,
// the peer answers with --in-reply-to ALONE, and the answer lands there —
// never on the asker's own chat.<short-id>. The negative half is asserted too,
// because "it arrived at R" and "it did not also arrive in my inbox" are
// different claims and only the second one is the goal.
func TestAgentCLI_E2E_ReplyToRoutesAwayFromTheAskersInbox(t *testing.T) {
	addr := freePortE2E(t)
	board, _ := startServerE2E(t, addr)

	const (
		ridStrA = "ws:1.2.3.4:9501-51" // asker
		ridStrB = "ws:5.6.7.8:9502-52" // peer
	)
	var ticketA, ticketB [16]byte
	ticketA[0] = 0xC1
	ticketB[0] = 0xC2
	tidA := mkTidE2E(0x51)
	tidB := mkTidE2E(0x52)
	ridA := mkRidE2E([4]byte{1, 2, 3, 4}, 9501, 51)
	ridB := mkRidE2E([4]byte{5, 6, 7, 8}, 9502, 52)
	board.Registry().Register(ridA, tidA, ticketA)
	board.Registry().Register(ridB, tidB, ticketB)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	selfA := agent.SelfTopic(tidA)
	selfB := agent.SelfTopic(tidB)
	const replyTo = "rr.task-1"

	// The asker subscribes to its declared destination and publishes, naming it.
	restoreA := setAgentEnv(addr, ridStrA, tidA, ticketA)
	var subOut, sendOut bytes.Buffer
	if err := agent.Subscribe(ctx, []string{"--topic", replyTo}, &subOut); err != nil {
		restoreA()
		t.Fatalf("Subscribe: %v", err)
	}
	if err := agent.Send(ctx,
		[]string{"--topic", selfB, "--reply-to", replyTo, "--data", `{"q":"question"}`},
		nil, &sendOut); err != nil {
		restoreA()
		t.Fatalf("Send: %v", err)
	}
	restoreA()

	// The peer replies with --in-reply-to ONLY. It never names replyTo.
	parent := lastSeqOnTopic(t, board, selfB)
	restoreB := setAgentEnv(addr, ridStrB, tidB, ticketB)
	var replyOut bytes.Buffer
	if err := agent.Send(ctx,
		[]string{"--in-reply-to", itoa(parent), "--data", `{"a":"answer-here"}`},
		nil, &replyOut); err != nil {
		restoreB()
		t.Fatalf("peer reply: %v", err)
	}
	restoreB()

	if got := topicPayloads(t, board, replyTo); !strings.Contains(got, "answer-here") {
		t.Errorf("declared topic %s holds %q, want the answer", replyTo, got)
	}
	if got := topicPayloads(t, board, selfA); strings.Contains(got, "answer-here") {
		t.Errorf("the asker's own topic holds %q — the answer was supposed to go elsewhere", got)
	}
}

// Saying nothing keeps today's behaviour, which is the default arm and the one
// every existing caller relies on.
func TestAgentCLI_E2E_WithoutReplyToTheAnswerComesHome(t *testing.T) {
	addr := freePortE2E(t)
	board, _ := startServerE2E(t, addr)

	const (
		ridStrA = "ws:1.2.3.4:9503-53"
		ridStrB = "ws:5.6.7.8:9504-54"
	)
	var ticketA, ticketB [16]byte
	ticketA[0] = 0xC3
	ticketB[0] = 0xC4
	tidA := mkTidE2E(0x53)
	tidB := mkTidE2E(0x54)
	ridA := mkRidE2E([4]byte{1, 2, 3, 4}, 9503, 53)
	ridB := mkRidE2E([4]byte{5, 6, 7, 8}, 9504, 54)
	board.Registry().Register(ridA, tidA, ticketA)
	board.Registry().Register(ridB, tidB, ticketB)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	selfA := agent.SelfTopic(tidA)
	selfB := agent.SelfTopic(tidB)

	restoreA := setAgentEnv(addr, ridStrA, tidA, ticketA)
	var sendOut bytes.Buffer
	if err := agent.Send(ctx,
		[]string{"--topic", selfB, "--data", `{"q":"question"}`}, nil, &sendOut); err != nil {
		restoreA()
		t.Fatalf("Send: %v", err)
	}
	restoreA()

	parent := lastSeqOnTopic(t, board, selfB)
	restoreB := setAgentEnv(addr, ridStrB, tidB, ticketB)
	var replyOut bytes.Buffer
	if err := agent.Send(ctx,
		[]string{"--in-reply-to", itoa(parent), "--data", `{"a":"came-home"}`},
		nil, &replyOut); err != nil {
		restoreB()
		t.Fatalf("peer reply: %v", err)
	}
	restoreB()

	if got := topicPayloads(t, board, selfA); !strings.Contains(got, "came-home") {
		t.Errorf("asker's own topic holds %q, want the answer (the default arm)", got)
	}
}

// The declaration has to survive the delivery path, not just the board: the
// recipient is the one party that can silently override it (by adding its own
// --topic), and it cannot decline to do that without being able to see it.
func TestAgentCLI_E2E_DeliveredMessageCarriesReplyToTopic(t *testing.T) {
	addr := freePortE2E(t)
	board, _ := startServerE2E(t, addr)

	const (
		ridStrA = "ws:1.2.3.4:9505-55"
		ridStrB = "ws:5.6.7.8:9506-56"
	)
	var ticketA, ticketB [16]byte
	ticketA[0] = 0xC5
	ticketB[0] = 0xC6
	tidA := mkTidE2E(0x55)
	tidB := mkTidE2E(0x56)
	board.Registry().Register(mkRidE2E([4]byte{1, 2, 3, 4}, 9505, 55), tidA, ticketA)
	board.Registry().Register(mkRidE2E([4]byte{5, 6, 7, 8}, 9506, 56), tidB, ticketB)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	selfB := agent.SelfTopic(tidB)

	restoreA := setAgentEnv(addr, ridStrA, tidA, ticketA)
	var out bytes.Buffer
	if err := agent.Send(ctx,
		[]string{"--topic", selfB, "--reply-to", "rr.task-1", "--data", `{"q":"declared"}`}, nil, &out); err != nil {
		restoreA()
		t.Fatalf("declared Send: %v", err)
	}
	if err := agent.Send(ctx,
		[]string{"--topic", selfB, "--data", `{"q":"plain"}`}, nil, &out); err != nil {
		restoreA()
		t.Fatalf("plain Send: %v", err)
	}
	restoreA()

	// Register alone does not seed the chat.<short-id> subscription the live
	// server creates at task assignment, and inbox reads only subscribed
	// topics -- so without this the read comes back empty and the assertion
	// below would pass vacuously if it were written as "no bad field".
	restoreB := setAgentEnv(addr, ridStrB, tidB, ticketB)
	var subOut, inbox bytes.Buffer
	if err := agent.Subscribe(ctx, []string{"--self"}, &subOut); err != nil {
		restoreB()
		t.Fatalf("Subscribe --self: %v", err)
	}
	err := agent.Inbox(ctx, []string{"--json"}, &inbox)
	restoreB()
	if err != nil {
		t.Fatalf("Inbox: %v", err)
	}

	var declared, plain bool
	for _, line := range strings.Split(strings.TrimSpace(inbox.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if uerr := json.Unmarshal([]byte(line), &rec); uerr != nil {
			t.Fatalf("inbox line %q: %v", line, uerr)
		}
		body, _ := json.Marshal(rec["payload"])
		switch {
		case strings.Contains(string(body), "declared"):
			declared = true
			if rec["reply_to_topic"] != "rr.task-1" {
				t.Errorf("declared record reply_to_topic = %v, want rr.task-1", rec["reply_to_topic"])
			}
		case strings.Contains(string(body), "plain"):
			plain = true
			if _, ok := rec["reply_to_topic"]; ok {
				t.Errorf("undeclared record carries reply_to_topic: %s", line)
			}
		}
	}
	if !declared || !plain {
		t.Fatalf("inbox missed a message (declared=%v plain=%v):\n%s", declared, plain, inbox.String())
	}
}
