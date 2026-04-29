package agentboard

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestBoard_ListTopics_Empty(t *testing.T) {
	b := New(Config{RingN: 8, TopicTTL: time.Hour, MaxTopics: 8, MaxPayload: 1024})
	defer b.Close()
	got := b.ListTopics()
	if len(got) != 0 {
		t.Errorf("ListTopics on empty board = %d, want 0", len(got))
	}
}

func TestBoard_ListTopics_AfterSends(t *testing.T) {
	b := New(Config{RingN: 8, TopicTTL: time.Hour, MaxTopics: 8, MaxPayload: 1024})
	defer b.Close()

	var rid protocol.RunnerID
	rid.SetTransport([]byte("ws"))
	rid.SetIpAddr([]byte{1, 2, 3, 4})
	var tid protocol.TaskID
	tid.Id[0] = 1

	if _, err := b.Send("a/x", []byte("1"), rid, tid, "h"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Send("a/x", []byte("2"), rid, tid, "h"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Send("b/y", []byte("3"), rid, tid, "h"); err != nil {
		t.Fatal(err)
	}

	got := b.ListTopics()
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	byName := map[string]BoardTopicSummary{}
	for _, s := range got {
		byName[s.Name] = s
	}
	if byName["a/x"].MsgCount != 2 {
		t.Errorf("a/x msg_count = %d, want 2", byName["a/x"].MsgCount)
	}
	// a/x has seq 1 and 2 (two sends); b/y has seq 3 (last send)
	// LastSeq returns the last appended seq for that topic
	if byName["a/x"].LastSeq != 2 {
		t.Errorf("a/x last_seq = %d, want 2", byName["a/x"].LastSeq)
	}
	if byName["b/y"].LastSeq != 3 {
		t.Errorf("b/y last_seq = %d, want 3", byName["b/y"].LastSeq)
	}
}
