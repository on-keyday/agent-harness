package server

import (
	"github.com/on-keyday/agent-harness/topics"
)

// notifyStreamWriter is the subset of trsf.BidirectionalStream that replay needs.
// trsf.BidirectionalStream satisfies it (AppendData on the embedded send stream).
type notifyStreamWriter interface {
	AppendData(eof bool, data ...[]byte) error
}

// replayNotifications writes the ring backlog (oldest first) to a newly-joined
// subscriber of the notifications topic, so a client that connects later still
// sees recent notifications. No-op for any other topic. Send-only: it never
// reads the stream. Each event is encoded as its own message (the consumer
// decodes a stream of concatenated NotifyEvents).
func replayNotifications(ring *notifyRing, topic string, stream notifyStreamWriter) {
	if ring == nil || topic != topics.Notifications() {
		return
	}
	for _, ev := range ring.snapshot() {
		ev := ev
		_ = stream.AppendData(false, ev.MustAppend(nil))
	}
}
