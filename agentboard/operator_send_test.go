package agentboard

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestBoard_SendStampsAgentKindByDefault(t *testing.T) {
	b := newOperatorTestBoard(t)
	seq, _, err := b.Send("chat.k", []byte("x"), protocol.RunnerID{}, protocol.TaskID{}, "h", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := b.Retained(seq)
	if !ok || m.SenderKind != protocol.SenderKind_Agent {
		t.Fatalf("SenderKind = %v, want agent", m.SenderKind)
	}
}

func TestBoard_WithSenderKindIsRecorded(t *testing.T) {
	b := newOperatorTestBoard(t)
	seq, _, err := b.Send("chat.k", []byte("x"), protocol.RunnerID{}, protocol.TaskID{}, "", "", 0,
		WithSenderKind(protocol.SenderKind_Operator))
	if err != nil {
		t.Fatal(err)
	}
	m, _ := b.Retained(seq)
	if m.SenderKind != protocol.SenderKind_Operator {
		t.Fatalf("SenderKind = %v, want operator", m.SenderKind)
	}
}

func TestBoard_NoWakeRetainsAndDeliversButDoesNotWake(t *testing.T) {
	b, sub, woken := subscribedBoard(t, "chat.nw")
	_, delivered, err := b.Send("chat.nw", []byte("quiet"), protocol.RunnerID{}, protocol.TaskID{}, "", "", 0, WithNoWake())
	if err != nil {
		t.Fatal(err)
	}
	if delivered != 1 {
		t.Errorf("deliveredTo = %d, want 1 (no-wake still counts subscribers)", delivered)
	}
	if n := woken(); n != 0 {
		t.Errorf("onDeliver called %d times, want 0", n)
	}
	msgs, _ := b.Inbox(sub, 0)
	if len(msgs) != 1 || string(msgs[0].Payload) != "quiet" {
		t.Fatalf("inbox = %+v, want the quiet message", msgs)
	}
}

func TestBoard_WakeWakesSubscribersAndPublishesNothing(t *testing.T) {
	b, _, woken := subscribedBoard(t, "chat.wk")
	if n := b.Wake("chat.wk"); n != 1 {
		t.Errorf("Wake = %d, want 1", n)
	}
	if n := woken(); n != 1 {
		t.Errorf("onDeliver called %d times, want 1", n)
	}
	if _, found := b.ListRetained("chat.wk"); found {
		t.Errorf("Wake created the topic; it must publish nothing")
	}
	if n := b.Wake("chat.nobody"); n != 0 {
		t.Errorf("Wake on an unsubscribed topic = %d, want 0", n)
	}
}

// A task waiting on the topic is handed the messages by its wait, so Wake
// skips it exactly as Send does.
func TestBoard_WakeSkipsTheTaskWaitingOnThatTopic(t *testing.T) {
	b, sub, woken := subscribedBoard(t, "chat.ww")
	done := make(chan struct{})
	go func() {
		defer close(done)
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		defer cancel()
		_, _, _ = b.Wait(ctx, sub, "chat.ww", 0, 0)
	}()
	time.Sleep(50 * time.Millisecond) // let the wait register
	if n := b.Wake("chat.ww"); n != 0 {
		t.Errorf("Wake = %d, want 0 while the only subscriber waits", n)
	}
	if n := woken(); n != 0 {
		t.Errorf("onDeliver called %d times, want 0", n)
	}
	<-done
}

func TestBoard_SubscribeRefusesOperatorTopic(t *testing.T) {
	b, sub, _ := subscribedBoard(t, "chat.other")
	if err := b.Subscribe(sub, OperatorTopic); !errors.Is(err, ErrReservedTopic) {
		t.Fatalf("Subscribe(%q) err = %v, want ErrReservedTopic", OperatorTopic, err)
	}
}

func newOperatorTestBoard(t *testing.T) *Board {
	t.Helper()
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	t.Cleanup(b.Close)
	return b
}

// subscribedBoard attaches one task subscribed to topic and counts the wakes
// the board emits.
func subscribedBoard(t *testing.T, topic string) (*Board, *ConnState, func() int) {
	t.Helper()
	b := newOperatorTestBoard(t)
	c := b.Attach(protocol.RunnerID{}, boardTaskIDFromByte(1), "host", "")
	t.Cleanup(func() { b.Detach(c) })
	if err := b.Subscribe(c, topic); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	n := 0
	b.SetOnDeliver(func(protocol.RunnerID, protocol.TaskID) {
		mu.Lock()
		n++
		mu.Unlock()
	})
	return b, c, func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}
