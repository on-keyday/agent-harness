package agentboard

import (
	"encoding/hex"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func runnerIDStringProto(r protocol.RunnerID) string {
	return r.Hex()
}

func hexTaskIDProto(t protocol.TaskID) string {
	return hex.EncodeToString(t.Id[:])
}

// SelfTopicPrefix is the prefix for each task's id-directed inbound topic.
const SelfTopicPrefix = "chat."

// OperatorTopic is where replies to the operator go. The TUI and WebUI prefill
// it as --reply-to when the operator replies, so an agent answering with
// --in-reply-to alone lands here. It is reserved: no agent may subscribe to it
// (Board.Subscribe), so a publish here wakes nobody and reaches no inbox. It
// cannot collide with a task's chat.<8-hex>, because "operator" is not hex.
const OperatorTopic = SelfTopicPrefix + "operator"

const selfTopicShortLen = 8

// SelfTopic returns the conventional inbound topic for tid:
// chat.<first-8-hex-chars-of-task-id>.
func SelfTopic(t protocol.TaskID) string {
	h := hexTaskIDProto(t)
	if len(h) < selfTopicShortLen {
		return SelfTopicPrefix + h
	}
	return SelfTopicPrefix + h[:selfTopicShortLen]
}
