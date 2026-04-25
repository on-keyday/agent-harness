package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	pubsubproto "github.com/on-keyday/agent-harness/pubsub/protocol"
	"github.com/on-keyday/agent-harness/topics"
	"github.com/on-keyday/agent-harness/trsf"
)

// Logs subscribes to task.<taskID>.log and writes each chunk to out until ctx is cancelled
// or the stream ends (task finished). Uses the Client's pre-wired pubsub correlator.
func Logs(ctx context.Context, addr, taskID string, out io.Writer) error {
	c, err := Dial(ctx, addr)
	if err != nil {
		return err
	}
	defer c.Close()

	topic := topics.TaskLog(taskID)
	respCh := make(chan *pubsubproto.PubSubResponse, 1)
	joinBytes := c.Pubsub().JoinTopic("cli", topic, func(r *pubsubproto.PubSubResponse) { respCh <- r })
	if joinBytes == nil {
		return fmt.Errorf("encode JOIN failed (nickname too long?)")
	}
	if _, _, err := c.Conn().SendMessage(joinBytes); err != nil {
		return fmt.Errorf("send JOIN: %w", err)
	}

	var resp *pubsubproto.PubSubResponse
	select {
	case <-ctx.Done():
		return ctx.Err()
	case resp = <-respCh:
	}
	if resp.Status != pubsubproto.Status_Ok {
		return fmt.Errorf("JOIN rejected: status %v", resp.Status)
	}

	st := waitForStream(ctx, c.Transport(), trsf.StreamID(resp.StreamId))
	if st == nil {
		return fmt.Errorf("stream %d not visible after JOIN", resp.StreamId)
	}

	// First read: discard the topic-name header line.
	if err := readUntilNewline(ctx, st); err != nil {
		return fmt.Errorf("read topic header: %w", err)
	}

	// Subsequent reads: copy bytes to out until EOF or ctx cancellation.
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		data, eof, err := st.ReadDirect(4096)
		if err != nil {
			return err
		}
		if len(data) > 0 {
			if _, werr := out.Write(data); werr != nil {
				return werr
			}
		}
		if eof {
			return nil
		}
	}
}

// readUntilNewline reads from st one byte at a time until a '\n' is consumed.
func readUntilNewline(ctx context.Context, st trsf.BidirectionalStream) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		data, eof, err := st.ReadDirect(1)
		if err != nil {
			return err
		}
		if len(data) > 0 && data[0] == '\n' {
			return nil
		}
		if eof {
			return io.EOF
		}
	}
}

// waitForStream returns p.GetBidirectionalStream(id), polling briefly if the
// stream isn't yet visible (the JOIN response may race ahead of the
// stream-creation trsf frame on the wire). Returns nil on ctx cancellation
// or if the stream doesn't appear within ~2s.
func waitForStream(ctx context.Context, p trsf.Transport, id trsf.StreamID) trsf.BidirectionalStream {
	if st := p.GetBidirectionalStream(id); st != nil {
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
			if st := p.GetBidirectionalStream(id); st != nil {
				return st
			}
		}
	}
}
