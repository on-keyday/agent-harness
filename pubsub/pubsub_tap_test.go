package pubsub

import (
	"fmt"
	"log/slog"
	"sync"
	"testing"
)

func TestTapReceivesPublish(t *testing.T) {
	ps := NewPubSub(slog.Default())
	var mu sync.Mutex
	var got []string
	ps.TapSubscribe("topic.x", func(nick string, msg []byte) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, fmt.Sprintf("%s|%s", nick, string(msg)))
	})
	ps.Publish("alice", "topic.x", []byte("hello"))
	ps.Publish("bob", "topic.x", []byte("world"))
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 || got[0] != "alice|hello" || got[1] != "bob|world" {
		t.Fatalf("got %v", got)
	}
}

func TestTapUnsubscribeStopsDelivery(t *testing.T) {
	ps := NewPubSub(slog.Default())
	var calls int
	var mu sync.Mutex
	tap := ps.TapSubscribe("t", func(nick string, msg []byte) {
		mu.Lock()
		calls++
		mu.Unlock()
	})
	ps.Publish("a", "t", []byte("1"))
	ps.TapUnsubscribe("t", tap)
	ps.Publish("a", "t", []byte("2"))
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("expected 1 call, got %d", calls)
	}
}

func TestTapMultipleTopics(t *testing.T) {
	ps := NewPubSub(slog.Default())
	var aCalls, bCalls int
	var mu sync.Mutex
	ps.TapSubscribe("t.a", func(_ string, _ []byte) { mu.Lock(); aCalls++; mu.Unlock() })
	ps.TapSubscribe("t.b", func(_ string, _ []byte) { mu.Lock(); bCalls++; mu.Unlock() })
	ps.Publish("nick", "t.a", []byte("x"))
	ps.Publish("nick", "t.a", []byte("y"))
	ps.Publish("nick", "t.b", []byte("z"))
	mu.Lock()
	defer mu.Unlock()
	if aCalls != 2 || bCalls != 1 {
		t.Fatalf("a=%d b=%d", aCalls, bCalls)
	}
}
