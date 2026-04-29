package agentboard

import (
	"testing"
	"time"
)

func TestTopic_AppendInRing(t *testing.T) {
	topic := newTopic("conv/x/messages", 4)
	for i := 0; i < 6; i++ {
		topic.append(uint64(i+1), []byte{byte(i)})
	}
	got := topic.since(0)
	if len(got) != 4 {
		t.Fatalf("ring should hold last 4 only, got %d", len(got))
	}
	if got[0].Seq != 3 || got[3].Seq != 6 {
		t.Errorf("ring oldest/newest seq = %d/%d, want 3/6", got[0].Seq, got[3].Seq)
	}
}

func TestTopic_SinceFiltersByCursor(t *testing.T) {
	topic := newTopic("topic/foo", 8)
	for i := uint64(1); i <= 5; i++ {
		topic.append(i, []byte{byte(i)})
	}
	got := topic.since(2)
	if len(got) != 3 {
		t.Fatalf("since=2 should yield seq 3,4,5, got len=%d", len(got))
	}
	if got[0].Seq != 3 {
		t.Errorf("first seq = %d, want 3", got[0].Seq)
	}
}

func TestTopic_LastPublishedAtUpdates(t *testing.T) {
	topic := newTopic("status/x/y", 4)
	t0 := time.Now()
	topic.append(1, []byte("a"))
	if topic.lastPublishedAt.Before(t0) {
		t.Error("lastPublishedAt did not update after append")
	}
}
