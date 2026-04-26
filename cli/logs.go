package cli

import (
	"context"
	"io"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/topics"
)

// Logs subscribes to task.<taskID>.log and writes each chunk to out until ctx
// is cancelled or the stream ends (task finished). Method form: callable on
// an existing *Client without re-dialing. Uses the Client's pre-wired pubsub
// correlator.
func (c *Client) Logs(ctx context.Context, taskID string, out io.Writer) error {
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

// Logs (package-level) is a thin wrapper that opens a fresh Client per call.
// Suitable for short-lived CLI processes (harness-cli). Long-lived consumers
// should hold a *Client and call (*Client).Logs instead.
func Logs(ctx context.Context, peerCID objproto.ConnectionID, taskID string, out io.Writer) error {
	c, err := Dial(ctx, peerCID)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Logs(ctx, taskID, out)
}
