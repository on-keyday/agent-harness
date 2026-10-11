package server

import (
	"github.com/on-keyday/agent-harness/topics"
)

// replayNotifications sends the ring backlog (oldest first) to a newly-joined
// subscriber of the notifications topic, so a client that connects later still
// sees recent notifications. No-op for any other topic. Each event is its own
// message.
func replayNotifications(ring *notifyRing, topic string, send func(msg []byte) error) {
	if ring == nil || topic != topics.Notifications() {
		return
	}
	for _, ev := range ring.snapshot() {
		// Best-effort: a closed/slow subscriber is not a replay failure, so the
		// error is intentionally ignored (mirrors pubsub.Publish).
		_ = send(ev.MustAppend(nil))
	}
}
