package agentboard

import (
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func toAgentboardRunnerID(r protocol.RunnerID) RunnerID {
	var out RunnerID
	out.SetTransport(r.Transport)
	out.SetIpAddr(r.IpAddr)
	out.Port = r.Port
	out.UniqueNumber = r.UniqueNumber
	return out
}

func toAgentboardTaskID(t protocol.TaskID) TaskID {
	var out TaskID
	copy(out.Id[:], t.Id[:])
	return out
}

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

func TestBoard_ListSubscriptions(t *testing.T) {
	b := New(Config{RingN: 8, TopicTTL: time.Hour, MaxTopics: 8, MaxPayload: 1024})
	defer b.Close()
	var rid RunnerID
	rid.SetTransport([]byte("ws"))
	rid.SetIpAddr([]byte{1, 2, 3, 4})
	var tid TaskID
	tid.Id[0] = 1
	c := b.Attach(rid, tid, "host")
	if err := b.Subscribe(c, "alpha/x"); err != nil {
		t.Fatal(err)
	}
	if err := b.Subscribe(c, "beta/y"); err != nil {
		t.Fatal(err)
	}
	got := b.ListSubscriptions(c)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	set := map[string]bool{got[0]: true, got[1]: true}
	if !set["alpha/x"] || !set["beta/y"] {
		t.Errorf("subs = %v", got)
	}
}

func TestBoard_ListSubscriptions_Detached(t *testing.T) {
	b := New(Config{RingN: 8, TopicTTL: time.Hour, MaxTopics: 8, MaxPayload: 1024})
	defer b.Close()
	if got := b.ListSubscriptions(nil); got != nil {
		t.Errorf("nil ConnState should yield nil, got %v", got)
	}
}

func TestBoard_OnDeliver_FiresPerSubscriber(t *testing.T) {
	b := New(Config{RingN: 8, TopicTTL: time.Hour, MaxTopics: 8, MaxPayload: 1024})
	defer b.Close()

	type hit struct {
		rid protocol.RunnerID
		tid protocol.TaskID
	}
	var got []hit
	var mu sync.Mutex
	b.SetOnDeliver(func(rid protocol.RunnerID, tid protocol.TaskID) {
		mu.Lock()
		got = append(got, hit{rid, tid})
		mu.Unlock()
	})

	mkRid := func(uniq uint16) protocol.RunnerID {
		var r protocol.RunnerID
		r.SetTransport([]byte("ws"))
		r.SetIpAddr([]byte{1, 2, 3, 4})
		r.UniqueNumber = uniq
		return r
	}
	mkTid := func(b byte) protocol.TaskID {
		var t protocol.TaskID
		t.Id[0] = b
		return t
	}

	cA := b.Attach(toAgentboardRunnerID(mkRid(1)), toAgentboardTaskID(mkTid(0xAA)), "host-A")
	cB := b.Attach(toAgentboardRunnerID(mkRid(2)), toAgentboardTaskID(mkTid(0xBB)), "host-B")
	cC := b.Attach(toAgentboardRunnerID(mkRid(3)), toAgentboardTaskID(mkTid(0xCC)), "host-C") // does not subscribe

	if err := b.Subscribe(cA, "topic/x"); err != nil {
		t.Fatal(err)
	}
	if err := b.Subscribe(cB, "topic/x"); err != nil {
		t.Fatal(err)
	}

	if _, err := b.Send("topic/x", []byte("hello"), mkRid(99), mkTid(0x99), "host-S"); err != nil {
		t.Fatal(err)
	}

	// Give async OnDeliver callbacks time to fire if they're not synchronous.
	// If implementation is synchronous, this is a no-op.
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("OnDeliver fired %d times, want 2", len(got))
	}
	tids := map[byte]bool{}
	for _, h := range got {
		tids[h.tid.Id[0]] = true
	}
	if !tids[0xAA] || !tids[0xBB] || tids[0xCC] {
		t.Errorf("got tids = %v, want {AA, BB} only", tids)
	}
	_ = cC
}
