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
