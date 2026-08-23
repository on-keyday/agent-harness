package agentboard

import (
	"testing"
	"time"
)

func newReplyToBoard(t *testing.T) *Board {
	t.Helper()
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	t.Cleanup(b.Close)
	return b
}

// The destination is frozen onto the message, not looked up later: the ring
// outlives the connection that published, and a reply can arrive long after.
func TestBoard_SendRecordsReplyToTopic(t *testing.T) {
	b := newReplyToBoard(t)
	seq, _, err := b.Send("t.ask", []byte("q"), testRid, testTid, "h", "", 0, WithReplyTo("rr.task-1"))
	if err != nil {
		t.Fatal(err)
	}
	m, ok := b.Retained(seq)
	if !ok {
		t.Fatalf("seq %d not retained", seq)
	}
	if m.ReplyToTopic != "rr.task-1" {
		t.Errorf("ReplyToTopic = %q, want rr.task-1", m.ReplyToTopic)
	}
}

// Saying nothing must stay the default, and must be distinguishable from
// saying "": both are empty, which is why the ABSENT case is asserted rather
// than assumed.
func TestBoard_SendWithoutReplyToLeavesItEmpty(t *testing.T) {
	b := newReplyToBoard(t)
	seq, _, err := b.Send("t.ask", []byte("q"), testRid, testTid, "h", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := b.Retained(seq)
	if m.ReplyToTopic != "" {
		t.Errorf("ReplyToTopic = %q, want empty", m.ReplyToTopic)
	}
}

// One message's destination must not leak onto the next: append writes it per
// entry, and a shared sendConfig would be a silent cross-message bug.
func TestBoard_ReplyToIsPerMessage(t *testing.T) {
	b := newReplyToBoard(t)
	withSeq, _, _ := b.Send("t.ask", []byte("a"), testRid, testTid, "h", "", 0, WithReplyTo("rr.one"))
	plainSeq, _, _ := b.Send("t.ask", []byte("b"), testRid, testTid, "h", "", 0)
	other, _, _ := b.Send("t.ask", []byte("c"), testRid, testTid, "h", "", 0, WithReplyTo("rr.two"))

	for _, tc := range []struct {
		seq  uint64
		want string
	}{{withSeq, "rr.one"}, {plainSeq, ""}, {other, "rr.two"}} {
		m, ok := b.Retained(tc.seq)
		if !ok {
			t.Fatalf("seq %d not retained", tc.seq)
		}
		if m.ReplyToTopic != tc.want {
			t.Errorf("seq %d: ReplyToTopic = %q, want %q", tc.seq, m.ReplyToTopic, tc.want)
		}
	}
}

// WithReplyTo composes with the other option rather than replacing it — they
// set different fields of the same sendConfig.
func TestBoard_ReplyToComposesWithNoRetireOnReply(t *testing.T) {
	b := newReplyToBoard(t)
	seq, _, _ := b.Send("t.ask", []byte("q"), testRid, testTid, "h", "", 0,
		WithReplyTo("rr.task-1"), NoRetireOnReply())
	m, _ := b.Retained(seq)
	if m.ReplyToTopic != "rr.task-1" {
		t.Errorf("ReplyToTopic = %q, want rr.task-1", m.ReplyToTopic)
	}
	if !m.NoRetireOnReply {
		t.Error("NoRetireOnReply was lost when combined with WithReplyTo")
	}
}
