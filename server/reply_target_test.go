package server

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// resolveReplyTarget has three arms and the ORDER is the contract: an explicit
// --topic on the reply wins over the asker's declaration, which wins over the
// historical fallback. Each arm is asserted separately, because a wrong order
// still satisfies any single one of them.
func TestResolveReplyTarget_Priority(t *testing.T) {
	b := agentboard.New(agentboard.Config{
		RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024,
	})
	defer b.Close()

	var rid protocol.RunnerID
	rid.SetTransport([]byte("ws"))
	rid.SetIpAddr([]byte{1, 2, 3, 4})
	var tid protocol.TaskID
	tid.Id[0] = 0x5A

	declared, _, err := b.Send("t.ask", []byte("q"), rid, tid, "h", "", 0,
		agentboard.WithReplyTo("rr.declared"))
	if err != nil {
		t.Fatal(err)
	}
	plain, _, err := b.Send("t.ask", []byte("q"), rid, tid, "h", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	self := agentboard.SelfTopic(tid)

	for _, tc := range []struct {
		name      string
		topic     string
		inReplyTo uint64
		want      string
		wantOK    bool
	}{
		{"not a reply passes the topic through", "t.somewhere", 0, "t.somewhere", true},
		{"explicit topic beats the declaration", "t.override", declared, "t.override", true},
		{"declaration beats the self fallback", "", declared, "rr.declared", true},
		{"no declaration falls back to self", "", plain, self, true},
		{"unknown parent is refused", "", 99999, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := resolveReplyTarget(b, tc.topic, tc.inReplyTo)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("resolveReplyTarget(%q, %d) = %q,%v want %q,%v",
					tc.topic, tc.inReplyTo, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
