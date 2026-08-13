package cli

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// Cancel sends a Cancel TaskControl request for taskIDHex over an existing
// *Client. Method form: callable repeatedly without re-dialing.
func (c *Client) Cancel(ctx context.Context, taskIDHex string) error {
	raw, err := hex.DecodeString(taskIDHex)
	if err != nil {
		return fmt.Errorf("invalid task id %q: %w", taskIDHex, err)
	}
	if len(raw) != 16 {
		return fmt.Errorf("task id must be 16 bytes (32 hex chars)")
	}
	var tid protocol.TaskID
	copy(tid.Id[:], raw)
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_Cancel}
	req.SetCancel(protocol.CancelTask{TaskId: tid})
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return err
	}
	if resp.Kind != protocol.TaskControlKind_Cancel {
		return fmt.Errorf("unexpected response kind: %v", resp.Kind)
	}
	// The status used to be a bare u8 the server always wrote as 0, so there
	// was nothing to check and this returned nil unconditionally. It now
	// carries no_such_task, which is also the answer for a task outside the
	// caller's scope — reporting that as success would tell an agent it had
	// cancelled something it had not.
	return cancelStatusErr(resp, taskIDHex)
}

// cancelStatusErr maps the response status to an error. Split out so it can be
// tested without a live connection.
func cancelStatusErr(resp *protocol.TaskControlResponse, taskIDHex string) error {
	st := resp.Cancel()
	if st == nil {
		return fmt.Errorf("cancel: response missing status")
	}
	switch st.Status {
	case protocol.CancelResult_Ok:
		return nil
	case protocol.CancelResult_NoSuchTask:
		return fmt.Errorf("no such task %s (it does not exist, or it is outside your scope)", taskIDHex)
	default:
		return fmt.Errorf("cancel failed: %v", st.Status)
	}
}

// Cancel (package-level) is a thin wrapper that opens a fresh Client per call.
// Suitable for short-lived CLI processes (harness-cli). Long-lived consumers
// should hold a *Client and call (*Client).Cancel instead.
func Cancel(ctx context.Context, peerCID objproto.ConnectionID, taskIDHex string) error {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Cancel(ctx, taskIDHex)
}
