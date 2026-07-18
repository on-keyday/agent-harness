package cli

import (
	"context"
	"fmt"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// FileMkdir creates a directory at remoteRel inside the worktree of
// taskIDHex. parents=false is strict os.Mkdir on the runner (missing
// parent → not_found, existing dir → already_exists); parents=true is
// os.MkdirAll (parents created, existing dir is ok). Mirrors Unix
// mkdir / mkdir -p. Reuses the OpenFileTransfer stream the way delete
// does: no payload bytes flow either direction, the runner acks and
// closes.
func (c *Client) FileMkdir(ctx context.Context, taskIDHex, remoteRel string, parents bool) error {
	stream, err := c.OpenFileTransfer(ctx, taskIDHex, protocol.FileTransferDirection_Mkdir, remoteRel, 0, false, parents)
	if err != nil {
		return err
	}
	defer stream.CloseBoth()
	// Half-close our send side so the server-side splice's
	// client→runner relay EOFs and the runner can ack (see
	// fileDeleteCommon for the same dance).
	if err := stream.AppendData(true); err != nil {
		return fmt.Errorf("file mkdir: half-close: %w", err)
	}
	ack, err := ReadFileTransferAck(stream)
	if err != nil {
		return fmt.Errorf("file mkdir: read ack: %w", err)
	}
	return ackError("mkdir", ack)
}
