package agentboard

import (
	"context"
	"testing"
	"time"
)

func TestBoard_SendThenInboxReturnsMessage(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()
	conn := b.Attach()
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
	conn := b.Attach()
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
	conn := b.Attach()
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
