package agent

import (
	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// hexTaskID is gone with the cursor file it named: it existed only to build
// $XDG_CACHE_HOME/harness/agent-cursor-<task>, and the delivery position now
// lives on the server keyed by the connection's authenticated identity.

// SelfTopicPrefix is the prefix the per-agent inbound topic convention uses.
// See SKILL.md "Naming inbound channels".
const SelfTopicPrefix = agentboard.SelfTopicPrefix

// SelfTopic returns the inbound topic this agent owns under the SKILL.md
// convention: chat.<first-8-hex-chars-of-task-id>. The returned string is
// what `harness-cli agent subscribe --self` subscribes to, and what peers
// should target as reply_topic.
func SelfTopic(t protocol.TaskID) string {
	return agentboard.SelfTopic(t)
}
