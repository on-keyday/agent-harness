package cli

import (
	"context"
	"fmt"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// FileDelete removes the regular file at remoteRel from the worktree of
// taskIDHex. Directories are rejected (returns is_directory) — use
// FileDeleteDir for those. Reuses the OpenFileTransfer stream: the
// runner writes a FileTransferAck immediately after performing the
// unlink, then closes; no payload bytes flow either direction.
func (c *Client) FileDelete(ctx context.Context, taskIDHex, remoteRel string) error {
	return c.fileDeleteCommon(ctx, taskIDHex, protocol.FileTransferDirection_Delete, remoteRel, false, "delete")
}

// FileDeleteDir removes the directory at remoteRel from the worktree of
// taskIDHex. When force is false the directory must be empty (non-empty
// returns not_empty); when force is true the directory is removed
// recursively via os.RemoveAll on the runner. Regular files at the leaf
// are rejected (returns not_a_directory) — use FileDelete for those.
func (c *Client) FileDeleteDir(ctx context.Context, taskIDHex, remoteRel string, force bool) error {
	return c.fileDeleteCommon(ctx, taskIDHex, protocol.FileTransferDirection_DirDelete, remoteRel, force, "dir-delete")
}

func (c *Client) fileDeleteCommon(ctx context.Context, taskIDHex string, dir protocol.FileTransferDirection, remoteRel string, force bool, label string) error {
	stream, err := c.OpenFileTransfer(ctx, taskIDHex, dir, remoteRel, 0, force)
	if err != nil {
		return err
	}
	defer stream.CloseBoth()
	// Half-close our send side: delete / dir-delete use no client→runner
	// bytes, so the server-side splice's client→runner relay must EOF
	// to unblock and let the runner ack.
	if err := stream.AppendData(true); err != nil {
		return fmt.Errorf("file %s: half-close: %w", label, err)
	}
	ack, err := ReadFileTransferAck(stream)
	if err != nil {
		return fmt.Errorf("file %s: read ack: %w", label, err)
	}
	return ackError(label, ack)
}
