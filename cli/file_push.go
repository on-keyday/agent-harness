package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// FilePush copies localPath into the worktree of taskIDHex at remoteRel.
// Returns an error if the runner rejects (already_exists, path_invalid)
// or the local file cannot be read.
func (c *Client) FilePush(ctx context.Context, taskIDHex, localPath, remoteRel string, force bool) error {
	src, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("file push: open local: %w", err)
	}
	defer src.Close()
	st, err := src.Stat()
	if err != nil {
		return fmt.Errorf("file push: stat local: %w", err)
	}
	return c.filePushFromReader(ctx, taskIDHex, src, uint64(st.Size()), remoteRel, force)
}

// FilePushBytes is the bytes-in variant of FilePush. Used by the WebUI
// wasm bridge to push file contents that the browser obtained via the
// FileReader / File.arrayBuffer() APIs — there is no local fs path on
// that side, so the runner-facing protocol is driven directly from a
// byte slice.
func (c *Client) FilePushBytes(ctx context.Context, taskIDHex string, data []byte, remoteRel string, force bool) error {
	return c.filePushFromReader(ctx, taskIDHex, bytes.NewReader(data), uint64(len(data)), remoteRel, force)
}

// filePushFromReader is the shared protocol path: open the push stream,
// copy size bytes from src into it, EOF, and wait for the ack. Both the
// file-backed FilePush and the bytes-backed FilePushBytes go through
// here so the wire side has one well-tested code path.
func (c *Client) filePushFromReader(ctx context.Context, taskIDHex string, src io.Reader, size uint64, remoteRel string, force bool) error {
	stream, err := c.OpenFileTransfer(ctx, taskIDHex, protocol.FileTransferDirection_Push, remoteRel, size, force)
	if err != nil {
		return err
	}
	defer stream.CloseBoth()
	if _, err := io.Copy(stream, src); err != nil {
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

// FilePushDir packs localDir into a tar stream and pushes it to the runner,
// which extracts it under worktreeRel using staging-dir + atomic rename.
// Refuses to overwrite an existing remote dest unless force is set.
func (c *Client) FilePushDir(ctx context.Context, taskIDHex, localDir, remoteRel string, force bool) error {
	info, err := os.Stat(localDir)
	if err != nil {
		return fmt.Errorf("file push --recursive: stat local: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("file push --recursive: %s is not a directory", localDir)
	}
	stream, err := c.OpenFileTransfer(ctx, taskIDHex, protocol.FileTransferDirection_DirPush, remoteRel, 0, force)
	if err != nil {
		return err
	}
	defer stream.CloseBoth()

	tw := tar.NewWriter(stream)
	walkErr := filepath.WalkDir(localDir, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if path == localDir {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		if info.Mode()&os.ModeType != 0 && !info.IsDir() {
			return nil
		}
		hdr, herr := tar.FileInfoHeader(info, "")
		if herr != nil {
			return herr
		}
		rel, rerr := filepath.Rel(localDir, path)
		if rerr != nil {
			return rerr
		}
		hdr.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			hdr.Name += "/"
			hdr.Typeflag = tar.TypeDir
			hdr.Size = 0
			return tw.WriteHeader(hdr)
		}
		hdr.Typeflag = tar.TypeReg
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.Copy(tw, f)
		_ = f.Close()
		return err
	})
	if walkErr != nil {
		return fmt.Errorf("file push --recursive: walk %s: %w", localDir, walkErr)
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("file push --recursive: tar close: %w", err)
	}
	if err := stream.AppendData(true); err != nil {
		return fmt.Errorf("file push --recursive: stream EOF: %w", err)
	}

	ack, err := ReadFileTransferAck(stream)
	if err != nil {
		return fmt.Errorf("file push --recursive: read ack: %w", err)
	}
	return ackError("push --recursive", ack)
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
		return fmt.Errorf("file %s: destination already exists (use --force to overwrite)", op)
	case protocol.FileTransferStatus_IoError:
		return fmt.Errorf("file %s: runner I/O error", op)
	case protocol.FileTransferStatus_Canceled:
		return fmt.Errorf("file %s: canceled", op)
	case protocol.FileTransferStatus_IsDirectory:
		return fmt.Errorf("file %s: is a directory", op)
	case protocol.FileTransferStatus_NotADirectory:
		return fmt.Errorf("file %s: not a directory", op)
	case protocol.FileTransferStatus_NotEmpty:
		return fmt.Errorf("file %s: directory not empty (use --force to remove recursively)", op)
	default:
		return fmt.Errorf("file %s: unknown status %d", op, ack.Status)
	}
}

