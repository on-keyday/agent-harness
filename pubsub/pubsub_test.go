package pubsub

import (
	"log/slog"
	"sync/atomic"
	"testing"

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
