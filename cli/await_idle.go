package cli

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// AwaitIdle sends an AwaitIdle TaskControl request over an existing *Client.
// sink=Reply LONG-POLLS: the server defers the response until the session's
// PTY output has been quiescent for the threshold (or the session stops), so
// the call blocks until then — bound it with ctx if needed. sink=Notify/Board
// return immediately with Status_Armed and the server delivers the fire
// out-of-band. Method form: long-lived consumers (TUI/WebUI) call this on
// their held client.
func (c *Client) AwaitIdle(ctx context.Context, taskIDHex string, thresholdMs uint32, sink protocol.AwaitIdleSink, topic string) (*protocol.AwaitIdleResponse, error) {
	raw, err := hex.DecodeString(taskIDHex)
	if err != nil {
		return nil, fmt.Errorf("invalid task id %q: %w", taskIDHex, err)
	}
	if len(raw) != 16 {
		return nil, fmt.Errorf("task id must be 16 bytes (32 hex chars)")
	}
	var tid protocol.TaskID
	copy(tid.Id[:], raw)
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AwaitIdle}
	ar := protocol.AwaitIdleRequest{TaskId: tid, ThresholdMs: thresholdMs, Sink: sink}
	ar.SetTopic([]byte(topic))
	req.SetAwaitIdle(ar)
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, err
	}
	ai := resp.AwaitIdle()
	if resp.Kind != protocol.TaskControlKind_AwaitIdle || ai == nil {
		return nil, fmt.Errorf("unexpected response kind: %v", resp.Kind)
	}
	return ai, nil
}
