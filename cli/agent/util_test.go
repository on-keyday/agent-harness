package agent_test

import (
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli/agent"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// TestSelfTopic asserts the chat.<first-8-hex-of-task-id> convention used by
// server-seeded task subscriptions and `harness-cli agent subscribe --self`.
// SKILL.md ("Naming inbound channels") publishes this exact shape; if it ever
// drifts from SelfTopic, peers that hand-derive `reply_topic` will miss
// messages.
func TestSelfTopic(t *testing.T) {
	cases := []struct {
		name string
		raw  [16]byte
		want string
	}{
		{
			name: "full id, take first 8 hex",
			raw:  [16]byte{0xab, 0xcd, 0xef, 0x01, 0x23, 0x45, 0x67, 0x89, 1, 2, 3, 4, 5, 6, 7, 8},
			want: "chat.abcdef01",
		},
		{
			name: "leading zero bytes preserved",
			raw:  [16]byte{0, 0, 0, 0xff},
			want: "chat.000000ff",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var tid protocol.TaskID
			tid.Id = tc.raw
			got := agent.SelfTopic(tid)
			if got != tc.want {
				t.Errorf("SelfTopic = %q, want %q", got, tc.want)
			}
			if !strings.HasPrefix(got, agent.SelfTopicPrefix) {
				t.Errorf("SelfTopic %q missing prefix %q", got, agent.SelfTopicPrefix)
			}
			if len(got) != len(agent.SelfTopicPrefix)+8 {
				t.Errorf("SelfTopic %q wrong length: got %d, want %d", got, len(got), len(agent.SelfTopicPrefix)+8)
			}
		})
	}
}
