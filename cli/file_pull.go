package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// FilePull copies remoteRel from the task's worktree to localPath. If the
// runner reports a non-ok ack, no local file is created.
func (c *Client) FilePull(ctx context.Context, taskIDHex, remoteRel, localPath string) error {
	stream, err := c.OpenFileTransfer(ctx, taskIDHex, protocol.FileTransferDirection_Pull, remoteRel, 0)
	if err != nil {
		return err
	}
	defer stream.CloseBoth()
	ack, err := ReadFileTransferAck(stream)
	if err != nil {
		return fmt.Errorf("file pull: read ack: %w", err)
	}
	if err := ackError("pull", ack); err != nil {
		return err
	}
	dst, err := os.OpenFile(localPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("file pull: open local: %w", err)
	}
	defer dst.Close()
	n, err := io.Copy(dst, streamReadAll{stream})
	if err != nil {
		return fmt.Errorf("file pull: stream read: %w", err)
	}
	if uint64(n) != ack.ActualSize {
		return fmt.Errorf("file pull: short read (got %d, expected %d)", n, ack.ActualSize)
	}
	return nil
}
