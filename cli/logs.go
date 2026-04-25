package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/pubsub"
	"github.com/on-keyday/agent-harness/topics"
	"github.com/on-keyday/agent-harness/trsf"
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
	go trsf.AutoSend(ctx, p, conn, nil)
	// AutoReceive blocks; run it in the background so it dispatches stream-related frames
	// into the trsf state machine while we wait on AcceptBidirectionalStream.
	go trsf.AutoReceive(ctx, p, conn, func(_ *objproto.Message, _ error) {
		// Logs is read-only on the data channel; we don't expect inbound control messages.
		// Stream-related frames are auto-dispatched by AutoReceive itself.
	})

	topic := topics.TaskLog(taskID)
	joinBytes := pubsub.JoinTopic("cli", topic)
	if _, _, err := conn.SendMessage(joinBytes); err != nil {
		return fmt.Errorf("send JOIN: %w", err)
	}

	// Wait for the broker to open the topic stream towards us.
	st, err := p.AcceptBidirectionalStream(ctx)
	if err != nil {
		return fmt.Errorf("accept stream: %w", err)
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
