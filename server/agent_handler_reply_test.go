package server

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func replyTestBoard(t *testing.T) *agentboard.Board {
	t.Helper()
	b := agentboard.New(agentboard.Config{RingN: 8, TopicTTL: time.Hour, MaxTopics: 8, MaxPayload: 1024})
	t.Cleanup(b.Close)
	return b
}

func replyTestRunnerID() protocol.RunnerID {
	var rid protocol.RunnerID
	rid.SetTransport([]byte("ws"))
	rid.SetIpAddr([]byte{1, 2, 3, 4})
	return rid
}

func TestResolveReplyTarget_DerivesParentSenderTopic(t *testing.T) {
	b := replyTestBoard(t)
	var parentTid protocol.TaskID
	for i := range parentTid.Id {
		parentTid.Id[i] = byte(i + 1)
	}

	parent, _, err := b.Send("chat.deadbeef", []byte("q"), replyTestRunnerID(), parentTid, "h", "", 0)
	if err != nil {
		t.Fatal(err)
	}

	topic, ok := resolveReplyTarget(b, "", parent)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if want := agentboard.SelfTopic(parentTid); topic != want {
		t.Errorf("topic = %q, want %q", topic, want)
	}
}

func TestResolveReplyTarget_ExplicitTopicWins(t *testing.T) {
	b := replyTestBoard(t)
	var parentTid protocol.TaskID
	parentTid.Id[0] = 9

	parent, _, err := b.Send("chat.deadbeef", []byte("q"), replyTestRunnerID(), parentTid, "h", "", 0)
	if err != nil {
		t.Fatal(err)
	}

	topic, ok := resolveReplyTarget(b, "rr.dec-019", parent)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if topic != "rr.dec-019" {
		t.Errorf("topic = %q, want %q", topic, "rr.dec-019")
	}
}

func TestResolveReplyTarget_UnknownParent(t *testing.T) {
	b := replyTestBoard(t)
	if _, ok := resolveReplyTarget(b, "chat.aaaa", 424242); ok {
		t.Error("ok = true for an unpublished parent, want false")
	}
}

func TestResolveReplyTarget_NotAReply(t *testing.T) {
	b := replyTestBoard(t)
	topic, ok := resolveReplyTarget(b, "plain", 0)
	if !ok {
		t.Fatal("ok = false for a non-reply, want true")
	}
	if topic != "plain" {
		t.Errorf("topic = %q, want %q", topic, "plain")
	}
}
