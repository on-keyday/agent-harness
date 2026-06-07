package peer

import (
	"context"
	"fmt"
	"time"

	pubsubproto "github.com/on-keyday/agent-harness/pubsub/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// pubTopic holds the per-topic singleflight state for Publish. The first
// goroutine to call Publish on a topic becomes the leader: it does the
// JOIN+lookup+header read once. Followers wait on init and then share the
// cached stream + error.
//
// On leader failure, err is set and stays set — Publish does not retry.
// The caller (typically a long-running publisher) is expected to surface
// the error and either give up or recreate the Conn entirely.
type pubTopic struct {
	init   chan struct{} // closed when stream/err are populated
	stream trsf.BidirectionalStream
	err    error
}

// JoinAndGetStream issues a JOIN for topic, awaits the broker's response,
// and returns the server-initiated bidi stream resolved by stream_id.
// Single-shot — does not cache. Suitable both for subscribers (which read
// payloads off the returned stream) and for callers that want one-off
// publish access. For long-running per-topic publishing, prefer Publish,
// which caches the stream and runs the JOIN exactly once per topic.
//
// nick is the JOIN nickname the broker logs (e.g. "cli", "runner",
// "watch"); semantically opaque.
func (c *Conn) JoinAndGetStream(ctx context.Context, nick, topic string) (trsf.BidirectionalStream, error) {
	respCh := make(chan *pubsubproto.PubSubResponse, 1)
	joinBytes := c.pub.JoinTopic(nick, topic, func(r *pubsubproto.PubSubResponse) { respCh <- r })
	if joinBytes == nil {
		return nil, fmt.Errorf("peer: encode JOIN failed (nickname %q too long?)", nick)
	}
	if _, _, err := c.conn.SendMessage(joinBytes); err != nil {
		return nil, fmt.Errorf("peer: send JOIN %q: %w", topic, err)
	}

	var resp *pubsubproto.PubSubResponse
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case resp = <-respCh:
	}
	if resp.Status != pubsubproto.Status_Ok && resp.Status != pubsubproto.Status_AlreadySubscribed {
		return nil, fmt.Errorf("peer: JOIN %q rejected: status %v", topic, resp.Status)
	}

	st := WaitForBidirectionalStream(ctx, c.trans, trsf.StreamID(resp.StreamId))
	if st == nil {
		return nil, fmt.Errorf("peer: stream %d not visible after JOIN %q", resp.StreamId, topic)
	}
	return st, nil
}

// Publish writes data to the bidirectional stream associated with topic.
// First call for a topic does JoinAndGetStream; later calls reuse the
// cached stream. Thread-safe; concurrent first-callers see exactly one
// JOIN flight via the leader/follower pattern in pubTopic.
func (c *Conn) Publish(ctx context.Context, nick, topic string, data []byte) error {
	c.pubmu.Lock()
	t, ok := c.pubTopics[topic]
	leader := false
	if !ok {
		t = &pubTopic{init: make(chan struct{})}
		c.pubTopics[topic] = t
		leader = true
	}
	c.pubmu.Unlock()

	if leader {
		t.stream, t.err = c.JoinAndGetStream(ctx, nick, topic)
		close(t.init)
	} else {
		select {
		case <-t.init:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if t.err != nil {
		return t.err
	}
	return t.stream.AppendData(false, data)
}

// BidirectionalStreamLookup is the narrow subset of trsf.Transport that
// callers needing per-stream lookup use. trsf.Transport satisfies it;
// callers with a smaller surface (e.g. tests, runner.Session) can
// implement it directly without taking a dep on the whole transport
// interface.
//
// GetReceiveStream is included for callers that resolve server-initiated
// send-streams (e.g. AssignTask body fetch) — these are receive-only on
// the runner side, not bidi.
type BidirectionalStreamLookup interface {
	GetBidirectionalStream(id trsf.StreamID) trsf.BidirectionalStream
	GetReceiveStream(id trsf.StreamID) trsf.ReceiveStream
}

// WaitForBidirectionalStream returns lookup.GetBidirectionalStream(id),
// polling briefly if the stream isn't yet visible — the response carrying
// the id can race ahead of the stream-creation trsf frame on the wire.
// Returns nil on ctx cancellation or after ~2s. Callers that need a
// different deadline should set their own ctx timeout.
func WaitForBidirectionalStream(ctx context.Context, lookup BidirectionalStreamLookup, id trsf.StreamID) trsf.BidirectionalStream {
	if st := lookup.GetBidirectionalStream(id); st != nil {
		return st
	}
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-deadline.C:
			return nil
		case <-tick.C:
			if st := lookup.GetBidirectionalStream(id); st != nil {
				return st
			}
		}
	}
}
