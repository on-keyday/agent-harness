package pubsub

import (
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/pubsub/protocol"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/trsf"
)

// noopPNIssuer satisfies trsf.PacketNumberIssuer for tests that don't need
// real packet-number sequencing (the Transport is not backed by a real conn).
type noopPNIssuer struct {
	cur atomic.Uint64
}

func (n *noopPNIssuer) ConsumePacketNumber() objproto.PacketNumber {
	return objproto.PacketNumber(n.cur.Add(1) - 1)
}

// newTestTransport returns a client-side trsf.Transport backed by no
// underlaying connection — sufficient for Subscribe, which only calls
// CreateBidirectionalStream() and then reads from the stream in a goroutine.
func newTestTransport(t *testing.T) trsf.Transport {
	t.Helper()
	tp := trsf.NewStreams(t.Context(), false /*isServer*/, 1200, 1500, &noopPNIssuer{}, slog.Default())
	if tp == nil {
		t.Fatal("trsf.NewStreams returned nil")
	}
	return tp
}

func TestPubSub_OnSubscribeHookFires(t *testing.T) {
	ps := NewPubSub(slog.Default())

	tp := newTestTransport(t)
	sub := NewSubscriber(objproto.ConnectionID{}, tp)

	var (
		hookCalls  int
		hookTopic  string
		hookStream trsf.BidirectionalStream
	)
	ps.OnSubscribe = func(topic string, stream trsf.BidirectionalStream) {
		hookCalls++
		hookTopic = topic
		hookStream = stream
	}

	resp := ps.Subscribe(1, "T", "nick", sub)
	if resp.Status != 0 { // protocol.Status_Ok == 0
		t.Fatalf("Subscribe returned non-OK status: %v", resp.Status)
	}

	if hookCalls != 1 {
		t.Fatalf("expected OnSubscribe to be called exactly once, got %d", hookCalls)
	}
	if hookTopic != "T" {
		t.Fatalf("expected topic %q, got %q", "T", hookTopic)
	}
	if hookStream == nil {
		t.Fatal("expected non-nil stream in OnSubscribe hook")
	}

	resp2 := ps.Subscribe(2, "T", "nick", sub)
	if resp2.Status != protocol.Status_AlreadySubscribed {
		t.Fatalf("second subscribe status = %v, want AlreadySubscribed", resp2.Status)
	}
	if hookCalls != 1 {
		t.Fatalf("hook must NOT fire on AlreadySubscribed; hookCalls = %d, want 1", hookCalls)
	}
}

// blockingStream stands in for a subscriber that has stopped reading its
// stream: trsf's AppendData waits for send-buffer space, so the publishing
// goroutine parks inside it. Only AppendData is ever called on it, so the
// embedded nil interface is never dereferenced.
type blockingStream struct {
	trsf.BidirectionalStream
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingStream) AppendData(eof bool, data ...[]byte) error {
	b.once.Do(func() { close(b.entered) })
	<-b.release
	return nil
}

// recordingStream is a subscriber that accepts data immediately.
type recordingStream struct {
	trsf.BidirectionalStream
	got chan []byte
}

func (r *recordingStream) AppendData(eof bool, data ...[]byte) error {
	for _, d := range data {
		select {
		case r.got <- d:
		default:
		}
	}
	return nil
}

// joinDirect wires a subscriber into the broker without going through
// Subscribe, so a test can install a stream of its choosing.
func joinDirect(ps *PubSub, topic string, conn trsf.BidirectionalStream) {
	sub := NewSubscriber(objproto.ConnectionID{}, nil)
	sub.topics[topic] = &topicJoinInfo{name: "nick", conn: conn}
	ps.m.Lock()
	defer ps.m.Unlock()
	if _, ok := ps.topics[topic]; !ok {
		ps.topics[topic] = &SubscriberList{}
	}
	ps.topics[topic].AddSubscriber(sub)
}

// AppendData blocks on a subscriber that stopped reading (a TUI suspended in
// tea.Exec with a busy task log followed is one). While it is parked, the rest
// of the broker must keep working: holding ps.m across it stalled every topic
// and every Subscribe/Unsubscribe behind that one subscriber.
func TestPubSub_SlowSubscriberDoesNotStallOtherTopics(t *testing.T) {
	ps := NewPubSub(slog.Default())

	slow := &blockingStream{entered: make(chan struct{}), release: make(chan struct{})}
	joinDirect(ps, "slow", slow)
	fast := &recordingStream{got: make(chan []byte, 1)}
	joinDirect(ps, "fast", fast)
	defer close(slow.release)

	go ps.Publish("nick", "slow", []byte("stuck"))
	select {
	case <-slow.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("publish never reached the slow subscriber's AppendData")
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		ps.Publish("nick", "fast", []byte("live"))       // needs ps.m
		ps.TapSubscribe("fast", func(string, []byte) {}) // needs ps.m
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("broker was blocked by one stalled subscriber: ps.m is still held across AppendData")
	}

	select {
	case got := <-fast.got:
		if string(got) != "live" {
			t.Errorf("fast subscriber got %q, want %q", got, "live")
		}
	default:
		t.Error("fast subscriber received nothing")
	}
}
