package peer

import (
	"context"
	"fmt"
	"time"

	pubsubproto "github.com/on-keyday/agent-harness/pubsub/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// JoinAndGetStream issues a JOIN for topic, awaits the broker's response,
// and returns the server-initiated bidi stream resolved by stream_id.
// Single-shot — does not cache. Subscribers read payloads off the returned
// stream.
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

// BidirectionalStreamLookup is the narrow subset of trsf.Transport that
// callers needing per-stream lookup use. trsf.Transport satisfies it;
// callers with a smaller surface (e.g. tests, runner.Session) can
// implement it directly without taking a dep on the whole transport
// interface.
//
// GetReceiveStream is included for callers that resolve server-initiated
// send-streams (e.g. a session's prompt body) — these are receive-only on
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
