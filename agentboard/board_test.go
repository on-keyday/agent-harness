package agentboard

import (
	"context"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestBoard_SendThenInboxReturnsMessage(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()
	conn := b.Attach(RunnerID{}, TaskID{})
	defer b.Detach(conn)
	if err := b.Subscribe(conn, "topic/foo"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Send("topic/foo", []byte("hello")); err != nil {
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
	conn := b.Attach(RunnerID{}, TaskID{})
	defer b.Detach(conn)
	_ = b.Subscribe(conn, "topic/bar")

	go func() {
		time.Sleep(20 * time.Millisecond)
		_, _ = b.Send("topic/bar", []byte("ping"))
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
	conn := b.Attach(RunnerID{}, TaskID{})
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
	if _, err := b.Send("topic/big", []byte("toolong")); err == nil {
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
	c1 := b.Attach(rid, tid)
	if err := b.Subscribe(c1, "topic/persistent"); err != nil {
		t.Fatal(err)
	}
	b.Detach(c1)

	// Send while no connection is attached. Message should land in the topic
	// ring and become visible to a future Inbox call.
	if _, err := b.Send("topic/persistent", []byte("delivered")); err != nil {
		t.Fatal(err)
	}

	// Connection 2: same (rid, tid). Should inherit the subscription.
	c2 := b.Attach(rid, tid)
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

	c1 := b.Attach(rid, tid)
	_ = b.Subscribe(c1, "topic/scoped")
	b.Detach(c1)

	// Revoke the (rid, tid) — destroys the taskState.
	b.Revoke(protoRunnerIDFromBoard(rid), protoTaskIDFromBoard(tid))

	if _, err := b.Send("topic/scoped", []byte("after-revoke")); err != nil {
		t.Fatal(err)
	}

	// New attach — fresh taskState, no inherited subscription.
	c2 := b.Attach(rid, tid)
	defer b.Detach(c2)
	msgs, _ := b.Inbox(c2, 0)
	if len(msgs) != 0 {
		t.Fatalf("inbox after revoke = %+v, want empty (subscription should be gone)", msgs)
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
