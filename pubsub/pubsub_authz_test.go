package pubsub

import (
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/pubsub/protocol"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/trsf"
)

// writerStream is a subscriber's stream as the broker's reader sees it: each
// ReadDirect hands back the next write the subscriber made, then io.EOF.
// drained closes once the reader has asked past the last write, by which point
// it has finished handling every earlier one.
type writerStream struct {
	trsf.BidirectionalStream
	mu      sync.Mutex
	writes  [][]byte
	drained chan struct{}
}

func (w *writerStream) ID() trsf.StreamID { return 1 }

func (w *writerStream) ReadDirect(uint64) ([]byte, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.writes) == 0 {
		close(w.drained)
		return nil, false, io.EOF
	}
	next := w.writes[0]
	w.writes = w.writes[1:]
	return next, false, nil
}

func (w *writerStream) AppendData(bool, ...[]byte) error { return nil }

type writerTransport struct {
	trsf.Transport
	stream *writerStream
}

func (t *writerTransport) CreateBidirectionalStream() trsf.BidirectionalStream { return t.stream }

func newWriter(writes ...string) *writerTransport {
	ws := &writerStream{drained: make(chan struct{})}
	for _, w := range writes {
		ws.writes = append(ws.writes, []byte(w))
	}
	return &writerTransport{stream: ws}
}

func waitDrained(t *testing.T, tp *writerTransport) {
	t.Helper()
	select {
	case <-tp.stream.drained:
	case <-time.After(5 * time.Second):
		t.Fatal("the broker never read the subscriber's writes")
	}
}

func TestSubscribe_RefusedJoinMakesNoSubscription(t *testing.T) {
	ps := NewPubSub(slog.Default())
	hooked := false
	ps.OnSubscribe = func(string, func([]byte) error) { hooked = true }
	sub := NewSubscriber(objproto.ConnectionID{}, newWriter())
	sub.SetAuthorizer(func(string) JoinGrant { return JoinGrant{} })

	resp := ps.Subscribe(7, "t", "nick", sub)
	if resp.Status != protocol.Status_Forbidden || resp.RequestId != 7 {
		t.Fatalf("refused JOIN answered %v (request %d), want Forbidden for request 7", resp.Status, resp.RequestId)
	}
	if len(sub.topics) != 0 || len(ps.topics) != 0 {
		t.Errorf("a refused JOIN left a subscription: sub=%v broker=%v", sub.topics, ps.topics)
	}
	if hooked {
		t.Error("OnSubscribe ran for a refused JOIN (it replays the notification backlog)")
	}
}

// Without Publish a subscriber's writes reach nobody, taps included — the log
// tap is how an injected line would have become a persisted one.
func TestSubscribe_WritesWithoutPublishAreDropped(t *testing.T) {
	for _, tc := range []struct {
		name    string
		grant   *JoinGrant // nil = no authorizer
		publish bool
	}{
		{"no authorizer publishes", nil, true},
		{"Publish publishes", &JoinGrant{Allow: true, Publish: true}, true},
		{"read-only drops", &JoinGrant{Allow: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ps := NewPubSub(slog.Default())
			other := &recordingStream{got: make(chan []byte, 4)}
			joinDirect(ps, "t", other)
			var mu sync.Mutex
			var tapped [][]byte
			ps.TapSubscribe("t", func(_ string, msg []byte) { mu.Lock(); tapped = append(tapped, msg); mu.Unlock() })

			tp := newWriter("one", "two")
			sub := NewSubscriber(objproto.ConnectionID{}, tp)
			if tc.grant != nil {
				g := *tc.grant
				sub.SetAuthorizer(func(string) JoinGrant { return g })
			}
			if resp := ps.Subscribe(1, "t", "nick", sub); resp.Status != protocol.Status_Ok {
				t.Fatalf("Subscribe: %v", resp.Status)
			}
			waitDrained(t, tp)

			mu.Lock()
			n := len(tapped)
			mu.Unlock()
			if want := map[bool]int{true: 2, false: 0}[tc.publish]; n != want {
				t.Errorf("taps got %d writes, want %d", n, want)
			}
			if got := len(other.got); tc.publish != (got > 0) {
				t.Errorf("another subscriber got %d writes, publish=%v", got, tc.publish)
			}
		})
	}
}

// What a subscriber publishes goes through the broker's DeliverFilter for the
// topic; the writer's own connection id plays no part in it.
func TestSubscribe_PublishedWritesUseDeliverFilter(t *testing.T) {
	ps := NewPubSub(slog.Default())
	allowed := objproto.MustParseConnectionID("ws:127.0.0.1:9811-1")
	denied := objproto.MustParseConnectionID("ws:127.0.0.1:9812-1")
	join := func(id objproto.ConnectionID) *recordingStream {
		rs := &recordingStream{got: make(chan []byte, 4)}
		s := NewSubscriber(id, nil)
		s.topics["t"] = &topicJoinInfo{name: "nick", conn: rs}
		ps.m.Lock()
		if _, ok := ps.topics["t"]; !ok {
			ps.topics["t"] = &SubscriberList{}
		}
		ps.topics["t"].AddSubscriber(s)
		ps.m.Unlock()
		return rs
	}
	gotAllowed, gotDenied := join(allowed), join(denied)
	ps.DeliverFilter = func(topic string) func(objproto.ConnectionID) bool {
		return func(id objproto.ConnectionID) bool { return id == allowed }
	}

	tp := newWriter("line")
	sub := NewSubscriber(objproto.MustParseConnectionID("ws:127.0.0.1:9813-1"), tp)
	if resp := ps.Subscribe(1, "t", "runner", sub); resp.Status != protocol.Status_Ok {
		t.Fatalf("Subscribe: %v", resp.Status)
	}
	waitDrained(t, tp)
	if len(gotAllowed.got) != 1 {
		t.Errorf("allowed subscriber got %d writes, want 1", len(gotAllowed.got))
	}
	if len(gotDenied.got) != 0 {
		t.Errorf("filtered-out subscriber got %d writes, want 0", len(gotDenied.got))
	}
}
