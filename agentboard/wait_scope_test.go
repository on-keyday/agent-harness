package agentboard

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
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
	_, timedOut, _ := b.Wait(ctx, conn, "topic/transient", 0, 0)
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
	msgs, timedOut, _ := b.Wait(ctx, conn, "topic/unsubscribed", 0, 0)
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
		_, _, _ = b.Wait(ctx, conn, "topic/abandoned", 0, 0)
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

// The wake this suppresses is the one a script's own wait causes: the blocked
// CLI gets the message AND the interactive agent sharing that task id gets a
// <harness:agentboard-wake> prompt typed into its PTY about it. The skip is
// keyed on (task, topic), so a second task subscribed to the same topic is
// unaffected.
func TestBoard_SendSkipsWakeForTheTaskWaitingOnThatTopic(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()

	waiter := b.Attach(RunnerID{}, boardTaskIDFromByte(1), "host-a", "")
	defer b.Detach(waiter)
	bystander := b.Attach(RunnerID{}, boardTaskIDFromByte(2), "host-b", "")
	defer b.Detach(bystander)
	_ = b.Subscribe(bystander, "topic/shared")

	var mu sync.Mutex
	woke := map[byte]int{}
	b.SetOnDeliver(func(_ protocol.RunnerID, tid protocol.TaskID) {
		mu.Lock()
		woke[tid.Id[0]]++
		mu.Unlock()
	})

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _, _ = b.Wait(ctx, waiter, "topic/shared", 0, 0)
	}()
	time.Sleep(50 * time.Millisecond) // let the wait register

	if _, _, err := b.Send("topic/shared", []byte("x"), testRid, testTid, "h", "", 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if woke[1] != 0 {
		t.Errorf("the waiting task was woken %d times, want 0", woke[1])
	}
	if woke[2] != 1 {
		t.Errorf("the bystander subscriber was woken %d times, want 1", woke[2])
	}
}

func TestBoard_WaitingTaskIsStillWokenForItsOtherTopics(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()

	conn := b.Attach(RunnerID{}, boardTaskIDFromByte(3), "host-c", "")
	defer b.Detach(conn)
	_ = b.Subscribe(conn, "topic/other")

	var mu sync.Mutex
	wakes := 0
	b.SetOnDeliver(func(_ protocol.RunnerID, _ protocol.TaskID) {
		mu.Lock()
		wakes++
		mu.Unlock()
	})

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _, _ = b.Wait(ctx, conn, "topic/awaited", 0, 0)
	}()
	time.Sleep(50 * time.Millisecond)

	if _, _, err := b.Send("topic/other", []byte("y"), testRid, testTid, "h", "", 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if wakes != 1 {
		t.Errorf("wakes = %d, want 1 — a wait on one topic must not silence the others", wakes)
	}
}

// dispatch's correlation rests on this: neither an unrelated publish nor a
// reply to somebody else's message may satisfy a wait that named a seq.
func TestBoard_WaitFiltersByInReplyTo(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()
	conn := b.Attach(RunnerID{}, TaskID{}, "test-host", "")
	defer b.Detach(conn)
	_ = b.Subscribe(conn, "topic/replies")

	if _, _, err := b.Send("topic/replies", []byte("noise"), testRid, testTid, "h", "", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Send("topic/replies", []byte("wrong"), testRid, testTid, "h", "", 999); err != nil {
		t.Fatal(err)
	}

	go func() {
		time.Sleep(40 * time.Millisecond)
		_, _, _ = b.Send("topic/replies", []byte("right"), testRid, testTid, "h", "", 42)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	msgs, timedOut, _ := b.Wait(ctx, conn, "topic/replies", 0, 42)
	if timedOut {
		t.Fatal("the wait timed out instead of matching the reply to seq 42")
	}
	if len(msgs) != 1 || string(msgs[0].Payload) != "right" {
		t.Fatalf("wait = %+v, want only the reply to seq 42", msgs)
	}
}

// in_reply_to = 0 must keep meaning "no filter", not "only non-replies".
func TestBoard_WaitWithoutFilterAcceptsAnything(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()
	conn := b.Attach(RunnerID{}, TaskID{}, "test-host", "")
	defer b.Detach(conn)

	if _, _, err := b.Send("topic/any", []byte("a reply"), testRid, testTid, "h", "", 7); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	msgs, timedOut, _ := b.Wait(ctx, conn, "topic/any", 0, 0)
	if timedOut || len(msgs) != 1 {
		t.Fatalf("wait = %+v timedOut=%v, want the reply to be accepted with no filter", msgs, timedOut)
	}
}
