package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/topics"
)

func TestReplayNotifications_OnlyNotificationsTopic(t *testing.T) {
	r := newNotifyRing(8)
	r.append(protocol.NotifyEvent{Ts: 1, Origin: protocol.NotifyOrigin_External, TextLen: 1, Text: []byte("a")})
	r.append(protocol.NotifyEvent{Ts: 2, Origin: protocol.NotifyOrigin_External, TextLen: 1, Text: []byte("b")})

	// wrong topic → no writes
	var other [][]byte
	replayNotifications(r, "tasks.status", func(m []byte) error { other = append(other, m); return nil })
	if len(other) != 0 {
		t.Fatalf("replayed to wrong topic: %d writes", len(other))
	}

	// notifications topic → one write per ring entry, decodable, in order
	var writes [][]byte
	replayNotifications(r, topics.Notifications(), func(m []byte) error { writes = append(writes, m); return nil })
	if len(writes) != 2 {
		t.Fatalf("got %d writes, want 2", len(writes))
	}
	var ev protocol.NotifyEvent
	if _, err := ev.Decode(writes[0]); err != nil || ev.Ts != 1 {
		t.Fatalf("first replayed event wrong: ts=%d err=%v", ev.Ts, err)
	}
	var ev2 protocol.NotifyEvent
	if _, err := ev2.Decode(writes[1]); err != nil || ev2.Ts != 2 {
		t.Fatalf("second replayed event wrong: ts=%d err=%v", ev2.Ts, err)
	}
}
