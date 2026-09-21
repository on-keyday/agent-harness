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
