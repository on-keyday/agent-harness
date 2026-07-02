package agentboard

import (
	"context"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

var testRid protocol.RunnerID
var testTid protocol.TaskID

func TestBoard_SendThenInboxReturnsMessage(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()
	conn := b.Attach(RunnerID{}, TaskID{}, "test-host")
	defer b.Detach(conn)
	if err := b.Subscribe(conn, "topic/foo"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Send("topic/foo", []byte("hello"), testRid, testTid, "test-host"); err != nil {
		t.Fatal(err)
	}
	msgs, _ := b.Inbox(conn, 0)
	if len(msgs) != 1 || string(msgs[0].Payload) != "hello" {
		t.Fatalf("inbox = %+v, want one message 'hello'", msgs)
	}
}

func TestBoard_WaitBlocksUntilMessageArrives(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()
	conn := b.Attach(RunnerID{}, TaskID{}, "test-host")
	defer b.Detach(conn)
	_ = b.Subscribe(conn, "topic/bar")

	go func() {
		time.Sleep(20 * time.Millisecond)
		_, _ = b.Send("topic/bar", []byte("ping"), testRid, testTid, "test-host")
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	msgs, timedOut, _ := b.Wait(ctx, conn, "topic/bar", 0)
	if timedOut {
		t.Fatal("Wait timed out unexpectedly")
	}
	if len(msgs) != 1 || string(msgs[0].Payload) != "ping" {
		t.Fatalf("wait = %+v, want one message 'ping'", msgs)
	}
}

func TestBoard_WaitTimesOut(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()
	conn := b.Attach(RunnerID{}, TaskID{}, "test-host")
	defer b.Detach(conn)
	_ = b.Subscribe(conn, "topic/quiet")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, timedOut, _ := b.Wait(ctx, conn, "topic/quiet", 0)
	if !timedOut {
		t.Fatal("Wait should have timed out")
	}
}

func TestBoard_PayloadTooLargeRejected(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 4})
	defer b.Close()
	if _, err := b.Send("topic/big", []byte("toolong"), testRid, testTid, "test-host"); err == nil {
		t.Fatal("expected payload_too_large error")
	}
}

// TestBoard_SubscriptionSurvivesDetach verifies the design fix: the per-task
// subscription set persists across ConnState lifecycles, so a subsequent
// agent reconnect for the same (rid, tid) sees previously-subscribed topics.
// This is what the dogfood UserPromptSubmit hook relies on.
func TestBoard_SubscriptionSurvivesDetach(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()

	rid, tid := RunnerID{}, TaskID{}

	// Connection 1: subscribe.
	c1 := b.Attach(rid, tid, "test-host")
	if err := b.Subscribe(c1, "topic/persistent"); err != nil {
		t.Fatal(err)
	}
	b.Detach(c1)

	// Send while no connection is attached. Message should land in the topic
	// ring and become visible to a future Inbox call.
	if _, err := b.Send("topic/persistent", []byte("delivered"), testRid, testTid, "test-host"); err != nil {
		t.Fatal(err)
	}

	// Connection 2: same (rid, tid). Should inherit the subscription.
	c2 := b.Attach(rid, tid, "test-host")
	defer b.Detach(c2)
	msgs, _ := b.Inbox(c2, 0)
	if len(msgs) != 1 || string(msgs[0].Payload) != "delivered" {
		t.Fatalf("inbox after reattach = %+v, want one message 'delivered'", msgs)
	}
}

// TestBoard_RevokeDestroysTaskState verifies that Revoke clears the persistent
// subscription set, so a subsequent reconnect with the same (rid, tid) does
// NOT see the old subscriptions.
func TestBoard_RevokeDestroysTaskState(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()

	rid, tid := RunnerID{}, TaskID{}

	c1 := b.Attach(rid, tid, "test-host")
	_ = b.Subscribe(c1, "topic/scoped")
	b.Detach(c1)

	// Revoke the (rid, tid) — destroys the taskState.
	b.Revoke(protoRunnerIDFromBoard(rid), protoTaskIDFromBoard(tid))

	if _, err := b.Send("topic/scoped", []byte("after-revoke"), testRid, testTid, "test-host"); err != nil {
		t.Fatal(err)
	}

	// New attach — fresh taskState, no inherited subscription.
	c2 := b.Attach(rid, tid, "test-host")
	defer b.Detach(c2)
	msgs, _ := b.Inbox(c2, 0)
	if len(msgs) != 0 {
		t.Fatalf("inbox after revoke = %+v, want empty (subscription should be gone)", msgs)
	}
}

// TestBoard_RevokeEvictsOrphanedTopics verifies that Revoke immediately removes
// topics that are no longer subscribed by any remaining task, while preserving
// topics that other tasks are still subscribed to.
func TestBoard_RevokeEvictsOrphanedTopics(t *testing.T) {
	b := New(Config{RingN: 4, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()

	var rid1, rid2 RunnerID
	rid1.SetTransport([]byte("ws"))
	rid2.SetTransport([]byte("ws"))
	rid2.Port = 2
	tid1, tid2 := TaskID{Id: [16]byte{1}}, TaskID{Id: [16]byte{2}}

	c1 := b.Attach(rid1, tid1, "host1")
	c2 := b.Attach(rid2, tid2, "host2")

	_ = b.Subscribe(c1, "chat.task1")  // exclusive to task1
	_ = b.Subscribe(c1, "harness.hello") // shared
	_ = b.Subscribe(c2, "harness.hello") // shared

	// Publish to both topics so they exist in b.topics.
	if _, err := b.Send("chat.task1", []byte("hi"), protoRunnerIDFromBoard(rid1), protoTaskIDFromBoard(tid1), "host1"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Send("harness.hello", []byte("hi"), protoRunnerIDFromBoard(rid1), protoTaskIDFromBoard(tid1), "host1"); err != nil {
		t.Fatal(err)
	}

	b.Detach(c1)
	b.Revoke(protoRunnerIDFromBoard(rid1), protoTaskIDFromBoard(tid1))

	topics := b.ListTopics()
	names := make(map[string]bool, len(topics))
	for _, ts := range topics {
		names[ts.Name] = true
	}

	if names["chat.task1"] {
		t.Error("chat.task1 should have been evicted after Revoke (no remaining subscribers)")
	}
	if !names["harness.hello"] {
		t.Error("harness.hello should still be present (task2 is still subscribed)")
	}
}

// TestBoard_SendSkipsOnDeliverForPublisher verifies the self-wake fix:
// when a (rid, tid) publishes on a topic it is itself subscribed to, the
// onDeliver callback fires for *other* matching subscribers but not for
// the publisher itself — so the server's task_wake hook does not inject
// <harness:agentboard-wake> into the publisher's own stdin.
func TestBoard_SendSkipsOnDeliverForPublisher(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()

	var pubRid, subRid RunnerID
	pubRid.SetTransport([]byte("ws"))
	pubRid.SetIpAddr([]byte{127, 0, 0, 1})
	pubRid.UniqueNumber = 1
	subRid.SetTransport([]byte("ws"))
	subRid.SetIpAddr([]byte{127, 0, 0, 2})
	subRid.UniqueNumber = 2
	var pubTid, subTid TaskID
	pubTid.Id[0] = 0xaa
	subTid.Id[0] = 0xbb

	pubConn := b.Attach(pubRid, pubTid, "pub-host")
	defer b.Detach(pubConn)
	subConn := b.Attach(subRid, subTid, "sub-host")
	defer b.Detach(subConn)

	if err := b.Subscribe(pubConn, "topic/x"); err != nil {
		t.Fatal(err)
	}
	if err := b.Subscribe(subConn, "topic/x"); err != nil {
		t.Fatal(err)
	}

	pubProtoTid := protoTaskIDFromBoard(pubTid)
	subProtoTid := protoTaskIDFromBoard(subTid)

	var pubDeliveries, subDeliveries int
	b.SetOnDeliver(func(_ protocol.RunnerID, tid protocol.TaskID) {
		switch tid.Id {
		case pubProtoTid.Id:
			pubDeliveries++
		case subProtoTid.Id:
			subDeliveries++
		}
	})

	fromRid := protoRunnerIDFromBoard(pubRid)
	fromTid := protoTaskIDFromBoard(pubTid)
	if _, err := b.Send("topic/x", []byte("hello"), fromRid, fromTid, "pub-host"); err != nil {
		t.Fatal(err)
	}

	if pubDeliveries != 0 {
		t.Errorf("publisher onDeliver fired %d times; want 0 (self-wake should be skipped)", pubDeliveries)
	}
	if subDeliveries != 1 {
		t.Errorf("subscriber onDeliver fired %d times; want 1", subDeliveries)
	}

	// Publisher's own inbox still sees the message via the topic ring —
	// only the wake hook is suppressed.
	msgs, _ := b.Inbox(pubConn, 0)
	if len(msgs) != 1 || string(msgs[0].Payload) != "hello" {
		t.Errorf("publisher inbox = %+v, want one message 'hello'", msgs)
	}
}

func TestBoard_PurgeTopicDropsRetainedAndKeepsCursorValid(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()
	conn := b.Attach(RunnerID{}, TaskID{}, "test-host")
	defer b.Detach(conn)
	if err := b.Subscribe(conn, "chat.poison"); err != nil {
		t.Fatal(err)
	}

	// Retain three messages, advance the consumer cursor past them.
	for _, p := range []string{"m1", "m2", "m3"} {
		if _, err := b.Send("chat.poison", []byte(p), testRid, testTid, "test-host"); err != nil {
			t.Fatal(err)
		}
	}
	_, cursor := b.Inbox(conn, 0)
	if cursor == 0 {
		t.Fatalf("cursor = 0 after three sends, want > 0")
	}

	// Purge drops the whole ring and reports the count.
	purged, found := b.PurgeTopic("chat.poison")
	if !found || purged != 3 {
		t.Fatalf("PurgeTopic = (purged=%d, found=%v), want (3, true)", purged, found)
	}

	// A since=0 re-read no longer resurfaces the purged payloads — this is the
	// whole point (a desync fallback that reads from 0 must come back empty).
	if msgs, _ := b.Inbox(conn, 0); len(msgs) != 0 {
		t.Fatalf("post-purge since=0 inbox = %+v, want empty", msgs)
	}

	// Purging again (or any unknown topic) is a no-op not_found.
	if purged, found := b.PurgeTopic("chat.poison"); found || purged != 0 {
		t.Fatalf("re-purge = (purged=%d, found=%v), want (0, false)", purged, found)
	}

	// seq is board-global, so a post-purge message gets a strictly higher seq
	// than the old cursor: the consumer's persisted cursor stays valid and the
	// fresh message is delivered exactly once.
	if _, err := b.Send("chat.poison", []byte("after"), testRid, testTid, "test-host"); err != nil {
		t.Fatal(err)
	}
	msgs, newCursor := b.Inbox(conn, cursor)
	if len(msgs) != 1 || string(msgs[0].Payload) != "after" {
		t.Fatalf("post-purge inbox(since=cursor) = %+v, want one message 'after'", msgs)
	}
	if newCursor <= cursor {
		t.Fatalf("post-purge seq = %d, want > old cursor %d", newCursor, cursor)
	}
}

func TestBoard_PurgeSeqAndListRetained(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()
	conn := b.Attach(RunnerID{}, TaskID{}, "test-host")
	defer b.Detach(conn)
	if err := b.Subscribe(conn, "chat.mix"); err != nil {
		t.Fatal(err)
	}

	var seqs []uint64
	for _, p := range []string{"a", "b", "c"} {
		s, err := b.Send("chat.mix", []byte(p), testRid, testTid, "test-host")
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, s)
	}

	// ListRetained exposes metadata for all three (incl. a populated ReceivedAt).
	msgs, found := b.ListRetained("chat.mix")
	if !found || len(msgs) != 3 {
		t.Fatalf("ListRetained = (%d msgs, found=%v), want (3, true)", len(msgs), found)
	}
	if msgs[0].ReceivedAt.IsZero() {
		t.Fatalf("ReceivedAt not populated on retained message")
	}

	// Drop only the middle seq; the other two and the topic itself survive.
	removed, found := b.PurgeSeq("chat.mix", seqs[1])
	if !found || !removed {
		t.Fatalf("PurgeSeq middle = (removed=%v, found=%v), want (true, true)", removed, found)
	}
	after, _ := b.ListRetained("chat.mix")
	if len(after) != 2 {
		t.Fatalf("after seq-purge ListRetained len=%d, want 2", len(after))
	}
	for _, m := range after {
		if m.Seq == seqs[1] {
			t.Fatalf("purged seq %d still present", seqs[1])
		}
	}
	in, _ := b.Inbox(conn, 0)
	if len(in) != 2 || string(in[0].Payload) != "a" || string(in[1].Payload) != "c" {
		t.Fatalf("inbox after seq-purge = %+v, want payloads [a c]", in)
	}

	// A seq not in the ring: topic found, nothing removed.
	if removed, found := b.PurgeSeq("chat.mix", 999999); !found || removed {
		t.Fatalf("PurgeSeq absent = (removed=%v, found=%v), want (false, true)", removed, found)
	}
	// Unknown topic: not found for both list and seq-purge.
	if _, found := b.ListRetained("nope"); found {
		t.Fatalf("ListRetained unknown topic returned found=true")
	}
	if _, found := b.PurgeSeq("nope", 1); found {
		t.Fatalf("PurgeSeq unknown topic returned found=true")
	}
}

// protoRunnerIDFromBoard / protoTaskIDFromBoard are test-only helpers to
// bridge the agentboard.RunnerID/TaskID (Hello-side) and protocol.RunnerID/
// TaskID (server-dispatch side). The two have the same field shape; both
// stringify identically via the runnerIDString*/hexTaskID* helpers.
func protoRunnerIDFromBoard(r RunnerID) protocol.RunnerID {
	var p protocol.RunnerID
	p.SetTransport([]byte(r.Transport))
	if len(r.IpAddr) > 0 {
		p.SetIpAddr(r.IpAddr)
	}
	p.Port = r.Port
	p.UniqueNumber = r.UniqueNumber
	return p
}

func protoTaskIDFromBoard(t TaskID) protocol.TaskID {
	var p protocol.TaskID
	copy(p.Id[:], t.Id[:])
	return p
}

// TestBoard_SeqSeedDefaultsToLegacy pins the backward-compatible default:
// with no SeqSeed (the value tests and pre-fix callers pass), the first
// published message still gets seq 1.
func TestBoard_SeqSeedDefaultsToLegacy(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()
	seq, err := b.Send("topic/first", []byte("x"), testRid, testTid, "test-host")
	if err != nil {
		t.Fatal(err)
	}
	if seq != 1 {
		t.Fatalf("first seq = %d, want 1 (legacy default when SeqSeed=0)", seq)
	}
}

// TestBoard_SeqSeedKeepsCursorValidAcrossRestart reproduces the auto-inbox
// wedge: the board-global seq lives only in memory, so a bare restart resets
// it to 0 and re-issues low seqs, but a consumer's persisted --since-last
// cursor survives the restart. If the post-restart seq lands at or below that
// cursor, Inbox(conn, cursor) filters out every new message and the hook goes
// silent. Seeding the new board with a strictly-higher boot epoch keeps the
// stale cursor valid.
func TestBoard_SeqSeedKeepsCursorValidAcrossRestart(t *testing.T) {
	// Boot 1: publish enough to advance a consumer cursor to a high value.
	b1 := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	c1 := b1.Attach(RunnerID{}, TaskID{}, "test-host")
	_ = b1.Subscribe(c1, "chat.task")
	var cursor uint64
	for i := 0; i < 56; i++ {
		if _, err := b1.Send("chat.task", []byte("old"), testRid, testTid, "test-host"); err != nil {
			t.Fatal(err)
		}
	}
	_, cursor = b1.Inbox(c1, 0) // cursor now == 56 (max seq seen)
	b1.Close()
	if cursor == 0 {
		t.Fatalf("precondition: cursor should have advanced, got %d", cursor)
	}

	// Boot 2 (simulated restart) with a strictly-higher boot epoch seed.
	// Without the seed this new board would restart at seq 0 and the message
	// below would get seq 1 <= cursor, so Inbox(c2, cursor) would return it
	// as empty — the bug.
	b2 := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024, SeqSeed: cursor + 1000})
	defer b2.Close()
	c2 := b2.Attach(RunnerID{}, TaskID{}, "test-host")
	_ = b2.Subscribe(c2, "chat.task")
	newSeq, err := b2.Send("chat.task", []byte("new"), testRid, testTid, "test-host")
	if err != nil {
		t.Fatal(err)
	}
	if newSeq <= cursor {
		t.Fatalf("post-restart seq %d must exceed stale cursor %d", newSeq, cursor)
	}
	msgs, _ := b2.Inbox(c2, cursor) // the hook's `--since-last` read
	if len(msgs) != 1 || string(msgs[0].Payload) != "new" {
		t.Fatalf("since-last inbox = %+v, want the post-restart 'new' message (stale cursor %d)", msgs, cursor)
	}
}
