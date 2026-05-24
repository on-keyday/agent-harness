package cli

import (
	"context"
	"encoding/hex"
	"io"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/topics"
)

// Logs writes the task's log to out, mirroring the TUI's two-source view:
// (1) the historical log file (via GetTaskLog) is written first, then
// (2) if follow is true and the task is not in a terminal state, the live
// pubsub topic task.<taskID>.log is subscribed and chunks are appended to
// out until ctx is cancelled or the stream ends.
//
// For terminal tasks (Succeeded / Failed / Cancelled) live subscription is
// skipped even with follow=true, since the runner will never publish more
// chunks — otherwise the call would block waiting for a topic that no one
// will write to.
func (c *Client) Logs(ctx context.Context, taskID string, out io.Writer, follow bool) error {
	hist, _, err := c.GetTaskLog(ctx, taskID)
	if err != nil {
		return err
	}
	if len(hist) > 0 {
		if _, werr := out.Write(hist); werr != nil {
			return werr
		}
	}
	if !follow {
		return nil
	}
	if terminal, err := c.isTaskTerminal(ctx, taskID); err != nil {
		return err
	} else if terminal {
		return nil
	}

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
		data, eof, err := st.ReadDirect(64 * 1024)
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

// isTaskTerminal reports whether the server's current view of taskIDHex is in
// a terminal status (Succeeded / Failed / Cancelled). Returns false when the
// task is not present in the snapshot — caller treats "unknown" as "skip live"
// since there's no producer to subscribe to either.
func (c *Client) isTaskTerminal(ctx context.Context, taskIDHex string) (bool, error) {
	raw, err := hex.DecodeString(taskIDHex)
	if err != nil || len(raw) != 16 {
		return true, nil
	}
	snap, err := c.Snapshot(ctx)
	if err != nil {
		return false, err
	}
	for i := range snap.Tasks {
		if string(snap.Tasks[i].Id.Id[:]) != string(raw) {
			continue
		}
		switch snap.Tasks[i].Status {
		case protocol.TaskStatus_Succeeded,
			protocol.TaskStatus_Failed,
			protocol.TaskStatus_Cancelled:
			return true, nil
		}
		return false, nil
	}
	return true, nil
}

// Logs (package-level) is a thin wrapper that opens a fresh Client per call.
// Suitable for short-lived CLI processes (harness-cli). Long-lived consumers
// should hold a *Client and call (*Client).Logs instead.
func Logs(ctx context.Context, peerCID objproto.ConnectionID, taskID string, out io.Writer, follow bool) error {
	c, err := Dial(ctx, peerCID)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Logs(ctx, taskID, out, follow)
}
