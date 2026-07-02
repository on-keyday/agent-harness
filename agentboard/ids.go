package agentboard

import (
	"encoding/hex"
	"fmt"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func formatIP(b []byte) string {
	switch len(b) {
	case 4:
		return fmt.Sprintf("%d.%d.%d.%d", b[0], b[1], b[2], b[3])
	case 16:
		return fmt.Sprintf("%x", b)
	default:
		return ""
	}
}

func runnerIDStringProto(r protocol.RunnerID) string {
	return fmt.Sprintf("%s:%s:%d-%d", string(r.Transport), formatIP(r.IpAddr), r.Port, r.UniqueNumber)
}

func runnerIDStringBoard(r RunnerID) string {
	return fmt.Sprintf("%s:%s:%d-%d", string(r.Transport), formatIP(r.IpAddr), r.Port, r.UniqueNumber)
}

func hexTaskIDProto(t protocol.TaskID) string {
	return hex.EncodeToString(t.Id[:])
}

func hexTaskIDBoard(t TaskID) string {
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
