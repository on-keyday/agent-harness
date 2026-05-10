package cli

import (
	"context"
	"fmt"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// FileDelete removes remoteRel from the worktree of taskIDHex. Refuses to
// delete directories in v1 (returns is_directory). Reuses the OpenFileTransfer
// stream — the runner writes a FileTransferAck immediately after performing
// the unlink, then closes; no payload bytes flow either direction.
func (c *Client) FileDelete(ctx context.Context, taskIDHex, remoteRel string) error {
	stream, err := c.OpenFileTransfer(ctx, taskIDHex, protocol.FileTransferDirection_Delete, remoteRel, 0, false)
	if err != nil {
		return err
	}
	defer stream.CloseBoth()
	// Half-close our send side: delete uses no client→runner bytes, so the
	// server-side splice's client→runner relay must EOF to unblock.
	if err := stream.AppendData(true); err != nil {
		return fmt.Errorf("file delete: half-close: %w", err)
	}
	ack, err := ReadFileTransferAck(stream)
	if err != nil {
		return fmt.Errorf("file delete: read ack: %w", err)
	}
	return ackError("delete", ack)
}
