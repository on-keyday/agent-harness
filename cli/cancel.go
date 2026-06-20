package cli

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
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
	return nil
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
