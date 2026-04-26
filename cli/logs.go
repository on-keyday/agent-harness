package cli

import (
	"context"
	"io"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/topics"
)

// Logs subscribes to task.<taskID>.log and writes each chunk to out until ctx is cancelled
// or the stream ends (task finished). Uses the Client's pre-wired pubsub correlator.
func Logs(ctx context.Context, peerCID objproto.ConnectionID, taskID string, out io.Writer) error {
	c, err := Dial(ctx, peerCID)
	if err != nil {
		return err
	}
	defer c.Close()

	st, err := c.Peer().JoinAndGetStream(ctx, "cli", topics.TaskLog(taskID))
	if err != nil {
		return err
	}

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
