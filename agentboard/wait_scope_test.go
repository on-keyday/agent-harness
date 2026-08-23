package agentboard

import (
	"context"
	"testing"
	"time"
)

// boardTaskIDFromByte builds a distinct agentboard.TaskID so two taskStates can
// coexist on one board. Named apart from retract_test.go's taskIDFromByte,
// which returns the protocol.TaskID that Send wants; Attach wants this one.
func boardTaskIDFromByte(b byte) TaskID {
	var t TaskID
	t.Id[0] = b
	return t
}

func TestBoard_WaitLeavesNoSubscriptionBehind(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()
	conn := b.Attach(RunnerID{}, TaskID{}, "test-host", "")
	defer b.Detach(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, timedOut, _ := b.Wait(ctx, conn, "topic/transient", 0)
	if !timedOut {
		t.Fatal("expected the wait to time out")
	}
	if b.Subscribes(conn, "topic/transient") {
		t.Fatal("the wait ended; topic/transient must not remain subscribed")
	}
}

// A wait must still receive the publish it is waiting for — the subscription it
// installs for its own duration is what makes Send count it as a target.
func TestBoard_WaitStillReceivesWithoutPriorSubscribe(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()
	conn := b.Attach(RunnerID{}, TaskID{}, "test-host", "")
	defer b.Detach(conn)

	go func() {
		time.Sleep(20 * time.Millisecond)
		_, _, _ = b.Send("topic/unsubscribed", []byte("ping"), testRid, testTid, "h", "", 0)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	msgs, timedOut, _ := b.Wait(ctx, conn, "topic/unsubscribed", 0)
	if timedOut {
		t.Fatal("the wait timed out; its own subscription did not make it a delivery target")
	}
	if len(msgs) != 1 || string(msgs[0].Payload) != "ping" {
		t.Fatalf("wait = %+v, want one message 'ping'", msgs)
	}
}

// The server builds the wait context from context.Background() plus the
// client's timeout, so a killed CLI would otherwise leave this Wait running to
// the full timeout -- and with it the waiter count that suppresses the wake.
func TestBoard_WaitEndsWhenConnectionDetaches(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()
	conn := b.Attach(RunnerID{}, TaskID{}, "test-host", "")

	done := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _, _ = b.Wait(ctx, conn, "topic/abandoned", 0)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	b.Detach(conn)

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Wait did not return after its connection was detached")
	}
	if b.Subscribes(conn, "topic/abandoned") {
		t.Fatal("the abandoned wait must still have released its subscription")
	}
}

func TestBoard_DetachTwiceIsSafe(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()
	conn := b.Attach(RunnerID{}, TaskID{}, "test-host", "")
	b.Detach(conn)
	b.Detach(conn) // must not panic on a second close
}
