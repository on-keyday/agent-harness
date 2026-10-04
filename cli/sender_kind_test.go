package cli

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestSenderParty(t *testing.T) {
	zero := strings.Repeat("0", 32)
	for _, c := range []struct{ kind, hex, want string }{
		{"operator", zero, "operator"},
		{"server", zero, "server"},
		{"agent", "abcdef12" + strings.Repeat("0", 24), "abcdef12"},
		{"", "abcdef12" + strings.Repeat("0", 24), "abcdef12"}, // an older server sends no kind
	} {
		if got := SenderParty(c.kind, c.hex); got != c.want {
			t.Errorf("SenderParty(%q) = %q, want %q", c.kind, got, c.want)
		}
	}
}

func TestSenderKindNameIsLowercase(t *testing.T) {
	for k, want := range map[protocol.SenderKind]string{
		protocol.SenderKind_Agent: "agent", protocol.SenderKind_Operator: "operator", protocol.SenderKind_Server: "server",
	} {
		if got := SenderKindName(k); got != want {
			t.Errorf("SenderKindName(%v) = %q, want %q", k, got, want)
		}
	}
}

// An agent <-> operator exchange keys as <8-hex>+operator: the operator's
// zero from_task must not add a 00000000 party, and the chat.operator suffix
// names the same party the sender kind does.
func TestConversationKey_OperatorIsAPartyByName(t *testing.T) {
	const agent = "cd000000cd000000cd000000cd000000"
	zero := strings.Repeat("0", 32)
	msgs := []BoardMessage{
		{Seq: 1, FromTaskHex: zero, SenderKind: "operator", ReceivedAtMs: 1},
		{Seq: 2, InReplyTo: 1, FromTaskHex: agent, SenderKind: "agent", ReceivedAtMs: 2},
	}
	topicOf := map[uint64]string{1: "chat.cd000000", 2: "chat.operator"}
	rows, err := SelectThreads(BuildThreads(msgs, topicOf), topicOf, ThreadFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 || rows[0].Conversation != "cd000000+operator" {
		t.Fatalf("conversation = %q, want cd000000+operator", rows[0].Conversation)
	}
}
