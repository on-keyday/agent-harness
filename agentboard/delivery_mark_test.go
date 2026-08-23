package agentboard

import (
	"testing"
	"time"
)

func newMarkBoard(t *testing.T) *Board {
	t.Helper()
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	t.Cleanup(b.Close)
	return b
}

func TestBoard_InboxAdvanceReturnsEachMessageOnce(t *testing.T) {
	b := newMarkBoard(t)
	conn := b.Attach(RunnerID{}, TaskID{}, "test-host", "")
	defer b.Detach(conn)
	_ = b.Subscribe(conn, "topic/mark")

	if _, _, err := b.Send("topic/mark", []byte("one"), testRid, testTid, "h", "", 0); err != nil {
		t.Fatal(err)
	}
	first := b.InboxAdvance(conn)
	if len(first) != 1 || string(first[0].Payload) != "one" {
		t.Fatalf("first advance = %+v, want one message 'one'", first)
	}
	if second := b.InboxAdvance(conn); len(second) != 0 {
		t.Fatalf("second advance = %+v, want empty", second)
	}
	if _, _, err := b.Send("topic/mark", []byte("two"), testRid, testTid, "h", "", 0); err != nil {
		t.Fatal(err)
	}
	third := b.InboxAdvance(conn)
	if len(third) != 1 || string(third[0].Payload) != "two" {
		t.Fatalf("third advance = %+v, want one message 'two'", third)
	}
}

// This is the regression the whole change exists for. Under the old
// client-side cursor -- one board-global seq watermark for every topic --
// taking a message on one topic moved the mark past an unread message on
// another, and the hook never delivered it.
func TestBoard_InboxAdvanceIsPerTopic(t *testing.T) {
	b := newMarkBoard(t)
	conn := b.Attach(RunnerID{}, TaskID{}, "test-host", "")
	defer b.Detach(conn)
	_ = b.Subscribe(conn, "topic/quiet")
	_ = b.Subscribe(conn, "topic/busy")

	// seq N   -> topic/quiet, nobody has read it
	// seq N+1 -> topic/busy, taken by a reader that covers only that topic
	if _, _, err := b.Send("topic/quiet", []byte("unread"), testRid, testTid, "h", "", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Send("topic/busy", []byte("taken"), testRid, testTid, "h", "", 0); err != nil {
		t.Fatal(err)
	}

	got := map[string]bool{}
	for _, m := range b.InboxAdvance(conn) {
		got[string(m.Payload)] = true
	}
	if !got["unread"] {
		t.Fatal("the message on the other topic was skipped — the mark is not per topic")
	}
	if !got["taken"] {
		t.Fatal("the busy topic's message was not returned")
	}
}

func TestBoard_InboxDoesNotMoveTheMark(t *testing.T) {
	b := newMarkBoard(t)
	conn := b.Attach(RunnerID{}, TaskID{}, "test-host", "")
	defer b.Detach(conn)
	_ = b.Subscribe(conn, "topic/plain")

	if _, _, err := b.Send("topic/plain", []byte("hello"), testRid, testTid, "h", "", 0); err != nil {
		t.Fatal(err)
	}
	if msgs, _ := b.Inbox(conn, 0); len(msgs) != 1 {
		t.Fatalf("Inbox = %+v, want one message", msgs)
	}
	if msgs, _ := b.Inbox(conn, 0); len(msgs) != 1 {
		t.Fatal("Inbox must be idempotent")
	}
	if adv := b.InboxAdvance(conn); len(adv) != 1 {
		t.Fatal("Inbox must not have advanced the mark")
	}
}

// The mark belongs to the task, not the connection: harness-cli is a fresh
// process (and so a fresh ConnState) per subcommand, and the hook that
// advances it runs in one of those.
func TestBoard_InboxAdvanceMarkIsPerTaskNotPerConnection(t *testing.T) {
	b := newMarkBoard(t)
	first := b.Attach(RunnerID{}, boardTaskIDFromByte(9), "h", "")
	_ = b.Subscribe(first, "topic/reconnect")
	if _, _, err := b.Send("topic/reconnect", []byte("m"), testRid, testTid, "h", "", 0); err != nil {
		t.Fatal(err)
	}
	if adv := b.InboxAdvance(first); len(adv) != 1 {
		t.Fatalf("first advance = %+v, want one message", adv)
	}
	b.Detach(first)

	second := b.Attach(RunnerID{}, boardTaskIDFromByte(9), "h", "")
	defer b.Detach(second)
	if adv := b.InboxAdvance(second); len(adv) != 0 {
		t.Fatalf("a new connection for the same task re-delivered %+v", adv)
	}
}

// Two concurrent advances must not both return the same message: collecting
// and marking happen under one acquisition of the task's lock.
func TestBoard_InboxAdvanceIsAtomic(t *testing.T) {
	b := newMarkBoard(t)
	conn := b.Attach(RunnerID{}, TaskID{}, "test-host", "")
	defer b.Detach(conn)
	_ = b.Subscribe(conn, "topic/race")

	const n = 50
	for i := 0; i < n; i++ {
		if _, _, err := b.Send("topic/race", []byte("x"), testRid, testTid, "h", "", 0); err != nil {
			t.Fatal(err)
		}
	}

	type result []RetainedMessage
	results := make(chan result, 8)
	for i := 0; i < 8; i++ {
		go func() { results <- b.InboxAdvance(conn) }()
	}
	seen := map[uint64]int{}
	for i := 0; i < 8; i++ {
		for _, m := range <-results {
			seen[m.Seq]++
		}
	}
	for seq, count := range seen {
		if count != 1 {
			t.Fatalf("seq %d returned %d times across concurrent advances, want 1", seq, count)
		}
	}
	if len(seen) != n {
		t.Fatalf("saw %d distinct seqs across concurrent advances, want %d", len(seen), n)
	}
}
