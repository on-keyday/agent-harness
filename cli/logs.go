package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/pubsub"
	pubsubproto "github.com/on-keyday/agent-harness/pubsub/protocol"
	"github.com/on-keyday/agent-harness/topics"
	"github.com/on-keyday/agent-harness/trsf"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// Logs subscribes to task.<taskID>.log and writes each chunk to out until ctx is cancelled
// or the stream ends (task finished).
func Logs(ctx context.Context, addr, taskID string, out io.Writer) error {
	c, err := Dial(ctx, addr)
	if err != nil {
		return err
	}
	defer c.Close()

	conn := c.Conn()
	p := trsf.NewStreams(ctx, false, trsf.DefaultInitialMTU, trsf.DefaultMaxMTU, conn, slog.Default())
	pubClient := pubsub.NewClient()
	go trsf.AutoSend(ctx, p, conn, nil)
	go trsf.AutoReceive(ctx, p, conn, func(msg *objproto.Message, err error) {
		if err != nil || len(msg.Data) == 0 {
			return
		}
		if wire.ApplicationPayloadKind(msg.Data[0]) == wire.ApplicationPayloadKind_Pubsub {
			pubClient.HandleResponse(msg.Data[1:])
		}
	})
	// Keep the objproto session alive — server's AutoGarbageCollect drops idle sessions
	// after 1 minute, and Logs may sit waiting for output much longer than that.
	go trsf.AutoPing(ctx, conn, 30*time.Second)

	topic := topics.TaskLog(taskID)
	respCh := make(chan *pubsubproto.PubSubResponse, 1)
	joinBytes := pubClient.JoinTopic("cli", topic, func(r *pubsubproto.PubSubResponse) { respCh <- r })
	if joinBytes == nil {
		return fmt.Errorf("encode JOIN failed (nickname too long?)")
	}
	if _, _, err := conn.SendMessage(joinBytes); err != nil {
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

	st := waitForStream(ctx, p, trsf.StreamID(resp.StreamId))
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
