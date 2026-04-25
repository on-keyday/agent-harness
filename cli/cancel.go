package cli

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func Cancel(ctx context.Context, addr, taskIDHex string) error {
	raw, err := hex.DecodeString(taskIDHex)
	if err != nil {
		return fmt.Errorf("invalid task id %q: %w", taskIDHex, err)
	}
	if len(raw) != 16 {
		return fmt.Errorf("task id must be 16 bytes (32 hex chars)")
	}
	c, err := Dial(ctx, addr)
	if err != nil {
		return err
	}
	defer c.Close()

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
