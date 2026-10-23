package agentboard

import (
	"github.com/on-keyday/agent-harness/runner/protocol"
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
	conn := b.Attach(protocol.RunnerID{}, protocol.TaskID{}, "test-host", "")
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
	conn := b.Attach(protocol.RunnerID{}, protocol.TaskID{}, "test-host", "")
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
	conn := b.Attach(protocol.RunnerID{}, protocol.TaskID{}, "test-host", "")
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
	first := b.Attach(protocol.RunnerID{}, boardTaskIDFromByte(9), "h", "")
	_ = b.Subscribe(first, "topic/reconnect")
	if _, _, err := b.Send("topic/reconnect", []byte("m"), testRid, testTid, "h", "", 0); err != nil {
		t.Fatal(err)
	}
	if adv := b.InboxAdvance(first); len(adv) != 1 {
		t.Fatalf("first advance = %+v, want one message", adv)
	}
	b.Detach(first)

	second := b.Attach(protocol.RunnerID{}, boardTaskIDFromByte(9), "h", "")
	defer b.Detach(second)
	if adv := b.InboxAdvance(second); len(adv) != 0 {
		t.Fatalf("a new connection for the same task re-delivered %+v", adv)
	}
}

// The operator view is the point of moving the mark server-side: the position
// used to be a file on the runner host that no surface showed.
func TestBoard_ListSubscribersCarriesShownAndPending(t *testing.T) {
	b := newMarkBoard(t)
	conn := b.Attach(protocol.RunnerID{}, protocol.TaskID{}, "test-host", "")
	defer b.Detach(conn)
	_ = b.Subscribe(conn, "topic/watched")

	var thirdSeq uint64
	for _, body := range []string{"a", "b", "c"} {
		seq, _, err := b.Send("topic/watched", []byte(body), testRid, testTid, "h", "", 0)
		if err != nil {
			t.Fatal(err)
		}
		thirdSeq = seq
	}
	b.InboxAdvance(conn) // shows all three
	if _, _, err := b.Send("topic/watched", []byte("d"), testRid, testTid, "h", "", 0); err != nil {
		t.Fatal(err)
	}

	rows := b.ListSubscribers("")
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	var got SubscriberPattern
	for _, p := range rows[0].Patterns {
		if p.Name == "topic/watched" {
			got = p
		}
	}
	if got.Name == "" {
		t.Fatalf("topic/watched missing from the row's patterns: %+v", rows[0].Patterns)
	}
	if got.Shown != thirdSeq {
		t.Errorf("Shown = %d, want %d (the seq of the third message)", got.Shown, thirdSeq)
	}
	if got.Pending != 1 {
		t.Errorf("Pending = %d, want 1 (the message published after the advance)", got.Pending)
	}
}

// A subscribed topic nothing has ever been published to must still appear, with
// a zero mark: "subscribed, nothing yet" is a real state and the operator view
// exists to tell it apart from "subscribed, everything read".
func TestBoard_ListSubscribersShowsUnpublishedTopic(t *testing.T) {
	b := newMarkBoard(t)
	conn := b.Attach(protocol.RunnerID{}, protocol.TaskID{}, "test-host", "")
	defer b.Detach(conn)
	_ = b.Subscribe(conn, "topic/silent")

	rows := b.ListSubscribers("")
	if len(rows) != 1 || len(rows[0].Patterns) != 1 {
		t.Fatalf("rows = %+v, want one row with one pattern", rows)
	}
	p := rows[0].Patterns[0]
	if p.Name != "topic/silent" || p.Shown != 0 || p.Pending != 0 {
		t.Errorf("pattern = %+v, want topic/silent with a zero mark", p)
	}
}

// A resume must not be handed again what its previous agent was given. The
// task's taskState goes at Revoke while its topic, still holding the message,
// stays; and the resume may be on another runner, so the key changes too.
// Observed on a Windows session that resumed after its runner restarted.
func TestBoard_InboxAdvanceMarkSurvivesRevokeAndResume(t *testing.T) {
	b := newMarkBoard(t)
	tid := boardTaskIDFromByte(7)
	before, after := protocol.RunnerID{Id: [16]byte{1}}, protocol.RunnerID{Id: [16]byte{2}}

	b.RegisterTask(before, tid, [16]byte{1}, "claude")
	first := b.Attach(before, tid, "h", "claude")
	if _, _, err := b.Send(SelfTopic(tid), []byte("old"), testRid, testTid, "h", "", 0); err != nil {
		t.Fatal(err)
	}
	if adv := b.InboxAdvance(first); len(adv) != 1 {
		t.Fatalf("first advance = %+v, want the one message", adv)
	}
	b.Detach(first)
	b.Revoke(before, tid)

	b.RegisterTask(after, tid, [16]byte{2}, "claude")
	resumed := b.Attach(after, tid, "h", "claude")
	defer b.Detach(resumed)
	if adv := b.InboxAdvance(resumed); len(adv) != 0 {
		t.Fatalf("the resumed task was handed again %q", adv[0].Payload)
	}
	if _, _, err := b.Send(SelfTopic(tid), []byte("new"), testRid, testTid, "h", "", 0); err != nil {
		t.Fatal(err)
	}
	if adv := b.InboxAdvance(resumed); len(adv) != 1 || string(adv[0].Payload) != "new" {
		t.Fatalf("advance after the resume = %+v, want only 'new'", adv)
	}
}

// An agent that starts a new conversation under the same task id has never seen
// what the earlier one was given, so ForgetShown hands it everything retained.
func TestBoard_ForgetShownHandsEverythingAgain(t *testing.T) {
	b := newMarkBoard(t)
	tid := boardTaskIDFromByte(8)
	rid := protocol.RunnerID{Id: [16]byte{3}}
	b.RegisterTask(rid, tid, [16]byte{3}, "claude")
	conn := b.Attach(rid, tid, "h", "claude")
	defer b.Detach(conn)
	_ = b.Subscribe(conn, "topic/other")
	for _, topic := range []string{SelfTopic(tid), "topic/other"} {
		if _, _, err := b.Send(topic, []byte(topic), testRid, testTid, "h", "", 0); err != nil {
			t.Fatal(err)
		}
	}
	if adv := b.InboxAdvance(conn); len(adv) != 2 {
		t.Fatalf("first advance = %+v, want both messages", adv)
	}

	b.ForgetShown(tid)

	if adv := b.InboxAdvance(conn); len(adv) != 2 {
		t.Fatalf("advance after ForgetShown = %+v, want both messages again", adv)
	}
	// Only this task's mark goes: another task's position on a shared topic is
	// not touched.
	other := b.Attach(protocol.RunnerID{Id: [16]byte{4}}, boardTaskIDFromByte(9), "h", "")
	defer b.Detach(other)
	_ = b.Subscribe(other, "topic/other")
	if adv := b.InboxAdvance(other); len(adv) != 1 {
		t.Fatalf("other task's first advance = %+v, want one", adv)
	}
	b.ForgetShown(tid)
	if adv := b.InboxAdvance(other); len(adv) != 0 {
		t.Fatalf("ForgetShown of one task re-delivered to another: %+v", adv)
	}
}

// Two concurrent advances must not both return the same message: collecting
// and marking happen under one acquisition of the topic's lock.
func TestBoard_InboxAdvanceIsAtomic(t *testing.T) {
	b := newMarkBoard(t)
	conn := b.Attach(protocol.RunnerID{}, protocol.TaskID{}, "test-host", "")
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
