package cli

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// AwaitProgress arms a one-shot watcher on a task's next progress write.
// since is the baseline (a progress_at read off `ls --json`): when the task
// has written since, it fires at once; 0 waits for the next write. sink=Reply
// LONG-POLLS until the fire; Notify/Board return Armed and deliver later.
func (c *Client) AwaitProgress(ctx context.Context, taskIDHex string, since uint64, sink protocol.AwaitSink, topic string) (*protocol.AwaitProgressResponse, error) {
	raw, err := hex.DecodeString(taskIDHex)
	if err != nil {
		return nil, fmt.Errorf("invalid task id %q: %w", taskIDHex, err)
	}
	if len(raw) != 16 {
		return nil, fmt.Errorf("task id must be 16 bytes (32 hex chars)")
	}
	var tid protocol.TaskID
	copy(tid.Id[:], raw)
	ap := protocol.AwaitProgressRequest{TaskId: tid, SinceProgressAt: since, Sink: sink}
	ap.SetTopic([]byte(topic))
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AwaitProgress}
	req.SetAwaitProgress(ap)
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, err
	}
	out := resp.AwaitProgress()
	if resp.Kind != protocol.TaskControlKind_AwaitProgress || out == nil {
		return nil, fmt.Errorf("unexpected response kind: %v", resp.Kind)
	}
	return out, nil
}

// AwaitProgressStatusString is the status in the schema's snake_case.
func AwaitProgressStatusString(s protocol.AwaitProgressStatus) string {
	switch s {
	case protocol.AwaitProgressStatus_Fired:
		return "fired"
	case protocol.AwaitProgressStatus_Armed:
		return "armed"
	case protocol.AwaitProgressStatus_TaskEnded:
		return "task_ended"
	case protocol.AwaitProgressStatus_NotFound:
		return "not_found"
	case protocol.AwaitProgressStatus_BadRequest:
		return "bad_request"
	case protocol.AwaitProgressStatus_Cancelled:
		return "cancelled"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}
