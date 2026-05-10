package cli

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// FilePush copies localPath into the worktree of taskIDHex at remoteRel.
// Returns an error if the runner rejects (already_exists, path_invalid)
// or the local file cannot be read.
func (c *Client) FilePush(ctx context.Context, taskIDHex, localPath, remoteRel string) error {
	src, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("file push: open local: %w", err)
	}
	defer src.Close()
	st, err := src.Stat()
	if err != nil {
		return fmt.Errorf("file push: stat local: %w", err)
	}
	stream, err := c.OpenFileTransfer(ctx, taskIDHex, protocol.FileTransferDirection_Push, remoteRel, uint64(st.Size()))
	if err != nil {
		return err
	}
	defer stream.CloseBoth()
	if _, err := io.Copy(streamWriter{stream}, src); err != nil {
		return fmt.Errorf("file push: stream write: %w", err)
	}
	if err := stream.AppendData(true); err != nil {
		return fmt.Errorf("file push: stream EOF: %w", err)
	}
	ack, err := ReadFileTransferAck(stream)
	if err != nil {
		return fmt.Errorf("file push: read ack: %w", err)
	}
	return ackError("push", ack)
}

func ackError(op string, ack *protocol.FileTransferAck) error {
	switch ack.Status {
	case protocol.FileTransferStatus_Ok:
		return nil
	case protocol.FileTransferStatus_PathInvalid:
		return fmt.Errorf("file %s: path invalid (escapes worktree or empty)", op)
	case protocol.FileTransferStatus_NotFound:
		return fmt.Errorf("file %s: not found", op)
	case protocol.FileTransferStatus_AlreadyExists:
		return fmt.Errorf("file %s: destination already exists", op)
	case protocol.FileTransferStatus_IoError:
		return fmt.Errorf("file %s: runner I/O error", op)
	case protocol.FileTransferStatus_Canceled:
		return fmt.Errorf("file %s: canceled", op)
	default:
		return fmt.Errorf("file %s: unknown status %d", op, ack.Status)
	}
}

// streamWriter adapts a trsf.BidirectionalStream's send side to io.Writer.
// Kept private to the cli package.
type streamWriter struct {
	s interface {
		AppendData(eof bool, data ...[]byte) error
	}
}

func (w streamWriter) Write(p []byte) (int, error) {
	if err := w.s.AppendData(false, p); err != nil {
		return 0, err
	}
	return len(p), nil
}
