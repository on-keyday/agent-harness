package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/topics"
)

type fakeStream struct{ writes [][]byte }

func (f *fakeStream) AppendData(eof bool, data ...[]byte) error {
	for _, d := range data {
		f.writes = append(f.writes, append([]byte(nil), d...))
	}
	return nil
}

func TestReplayNotifications_OnlyNotificationsTopic(t *testing.T) {
	r := newNotifyRing(8)
	r.append(protocol.NotifyEvent{Ts: 1, Origin: protocol.NotifyOrigin_External, TextLen: 1, Text: []byte("a")})
	r.append(protocol.NotifyEvent{Ts: 2, Origin: protocol.NotifyOrigin_External, TextLen: 1, Text: []byte("b")})

	// wrong topic → no writes
	var other fakeStream
	replayNotifications(r, "tasks.status", &other)
	if len(other.writes) != 0 {
		t.Fatalf("replayed to wrong topic: %d writes", len(other.writes))
	}

	// notifications topic → one write per ring entry, decodable, in order
	var nf fakeStream
	replayNotifications(r, topics.Notifications(), &nf)
	if len(nf.writes) != 2 {
		t.Fatalf("got %d writes, want 2", len(nf.writes))
	}
	var ev protocol.NotifyEvent
	if _, err := ev.Decode(nf.writes[0]); err != nil || ev.Ts != 1 {
		t.Fatalf("first replayed event wrong: ts=%d err=%v", ev.Ts, err)
	}
	var ev2 protocol.NotifyEvent
	if _, err := ev2.Decode(nf.writes[1]); err != nil || ev2.Ts != 2 {
		t.Fatalf("second replayed event wrong: ts=%d err=%v", ev2.Ts, err)
	}
}
