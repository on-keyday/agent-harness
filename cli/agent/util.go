package agent

import (
	"encoding/hex"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func hexTaskID(t protocol.TaskID) string {
	return hex.EncodeToString(t.Id[:])
}

// SelfTopicPrefix is the prefix the per-agent inbound topic convention uses.
// See SKILL.md "Naming inbound channels".
const SelfTopicPrefix = "chat."

// selfTopicShortLen is the number of leading hex chars from the task id used
// as the inbound topic suffix. Matches the SKILL.md convention
// "chat.<first-8-chars-of-task-id>". Centralised so subscribe --self and any
// future producer of the same string stay in sync.
const selfTopicShortLen = 8

// SelfTopic returns the inbound topic this agent owns under the SKILL.md
// convention: chat.<first-8-hex-chars-of-task-id>. The returned string is
// what `harness-cli agent subscribe --self` subscribes to, and what peers
// should target as reply_topic.
func SelfTopic(t protocol.TaskID) string {
	h := hexTaskID(t)
	if len(h) < selfTopicShortLen {
		return SelfTopicPrefix + h
	}
	return SelfTopicPrefix + h[:selfTopicShortLen]
}
