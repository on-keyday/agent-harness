package agent_test

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/cli/agent"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// operatorReplyFixture is one registered agent and an in-process board. The
// operator side publishes straight onto the board: what is under test here is
// the AGENT's reply to an operator message, not the operator's send path
// (cli/board_send_e2e_test.go covers that).
func operatorReplyFixture(t *testing.T) (board *agentboard.Board, addr string, rid protocol.RunnerID, tid protocol.TaskID, ticket [16]byte) {
	t.Helper()
	addr = freePortE2E(t)
	board, _ = startServerE2E(t, addr)
	ticket[0] = 0xD1
	tid = mkTidE2E(0x61)
	rid = mkRidE2E([4]byte{1, 2, 3, 4}, 9601, 61)
	board.Registry().Register(rid, tid, ticket)
	return board, addr, rid, tid, ticket
}

// The operator surfaces prefill --reply-to chat.operator when the operator
// replies; the agent then answers with --in-reply-to alone and the answer
// lands on chat.operator.
func TestAgentCLI_E2E_ReplyToOperatorLandsOnChatOperator(t *testing.T) {
	board, addr, rid, tid, ticket := operatorReplyFixture(t)
	parent, _, err := board.Send(agent.SelfTopic(tid), []byte("from the operator"), protocol.RunnerID{}, protocol.TaskID{}, "", "", 0,
		agentboard.WithSenderKind(protocol.SenderKind_Operator), agentboard.WithReplyTo(agentboard.OperatorTopic))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	restore := setAgentEnv(addr, rid, tid, ticket)
	defer restore()
	var out bytes.Buffer
	if err := agent.Send(ctx, []string{"--in-reply-to", itoa(parent), "--data", "answer-for-op"}, nil, &out); err != nil {
		t.Fatalf("reply: %v", err)
	}
	if got := topicPayloads(t, board, agentboard.OperatorTopic); !strings.Contains(got, "answer-for-op") {
		t.Errorf("chat.operator holds %q, want the answer", got)
	}
}

// Without a declared destination the reply is refused, with the error that
// says why, and nothing is published anywhere.
func TestAgentCLI_E2E_ReplyToOperatorWithoutRouteIsRefused(t *testing.T) {
	board, addr, rid, tid, ticket := operatorReplyFixture(t)
	self := agent.SelfTopic(tid)
	parent, _, err := board.Send(self, []byte("fyi"), protocol.RunnerID{}, protocol.TaskID{}, "", "", 0,
		agentboard.WithSenderKind(protocol.SenderKind_Operator))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	restore := setAgentEnv(addr, rid, tid, ticket)
	defer restore()
	var out bytes.Buffer
	err = agent.Send(ctx, []string{"--in-reply-to", itoa(parent), "--data", "lost"}, nil, &out)
	var nr *cli.NoReplyRouteError
	if !errors.As(err, &nr) {
		t.Fatalf("err = %v, want NoReplyRouteError", err)
	}
	if last := lastSeqOnTopic(t, board, self); last != parent {
		t.Errorf("a message was published after the refusal (last seq %d, parent %d)", last, parent)
	}
	if got := topicPayloads(t, board, "chat.00000000"); got != "" {
		t.Errorf("chat.00000000 holds %q", got)
	}
}

func TestAgentCLI_E2E_SubscribingToChatOperatorIsRefused(t *testing.T) {
	_, addr, rid, tid, ticket := operatorReplyFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	restore := setAgentEnv(addr, rid, tid, ticket)
	defer restore()
	var out bytes.Buffer
	if err := agent.Subscribe(ctx, []string{"--topic", agentboard.OperatorTopic}, &out); err == nil {
		t.Fatalf("subscribe to %s succeeded (out=%q), want a refusal", agentboard.OperatorTopic, out.String())
	}
}
