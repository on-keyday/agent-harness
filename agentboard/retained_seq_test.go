package agentboard

import (
	"testing"
	"time"
)

// TestBoard_Retained_FindsMessageAcrossTopics is the storage half of reading
// one message by seq. LookupSeq already scans every ring for a seq but answers
// only with the sender, which is all in_reply_to validation needs; a reader
// needs the body and the topic it came from.
func TestBoard_Retained_FindsMessageAcrossTopics(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()

	if _, err := b.Send("topic/a", []byte("first"), testRid, testTid, "h", "", 0); err != nil {
		t.Fatal(err)
	}
	seq, err := b.Send("topic/b", []byte("second"), testRid, testTid, "h", "", 0)
	if err != nil {
		t.Fatal(err)
	}

	m, ok := b.Retained(seq)
	if !ok {
		t.Fatalf("Retained(%d) not found", seq)
	}
	if m.Topic != "topic/b" {
		t.Errorf("topic = %q, want topic/b: the caller needs it to check its own subscriptions", m.Topic)
	}
	if string(m.Payload) != "second" {
		t.Errorf("payload = %q, want %q", m.Payload, "second")
	}
	if m.Seq != seq {
		t.Errorf("seq = %d, want %d", m.Seq, seq)
	}
}

// TestBoard_Retained_MissesEvictedMessage keeps the lookup honest about the
// ring: a seq that has rotated out is gone, not stale-readable. This is the
// case the read path reports as not_found, and it is the likely one — a
// pointer handed out with a message outlives the message.
func TestBoard_Retained_MissesEvictedMessage(t *testing.T) {
	b := New(Config{RingN: 2, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()

	first, err := b.Send("topic/a", []byte("1"), testRid, testTid, "h", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"2", "3"} {
		if _, err := b.Send("topic/a", []byte(body), testRid, testTid, "h", "", 0); err != nil {
			t.Fatal(err)
		}
	}

	if _, ok := b.Retained(first); ok {
		t.Errorf("Retained(%d) found a message the ring dropped", first)
	}
}

// TestBoard_Retained_RejectsZeroSeq mirrors LookupSeq: seq 0 is the wire's
// "no message" sentinel, never an entry.
func TestBoard_Retained_RejectsZeroSeq(t *testing.T) {
	b := New(Config{RingN: 8, TopicTTL: time.Hour, MaxTopics: 8, MaxPayload: 1024})
	defer b.Close()
	if _, ok := b.Retained(0); ok {
		t.Error("Retained(0) reported a message")
	}
}
