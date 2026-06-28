package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// FilePull copies remoteRel from the task's worktree to localPath. If the
// runner reports a non-ok ack, no local file is created.
func (c *Client) FilePull(ctx context.Context, taskIDHex, remoteRel, localPath string, force bool) error {
	return c.filePullDo(ctx, taskIDHex, remoteRel, func(stream trsf.BidirectionalStream, expectedSize uint64) error {
		flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
		if force {
			flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		}
		dst, err := os.OpenFile(localPath, flags, 0o644)
		if err != nil {
			if os.IsExist(err) {
				return fmt.Errorf("file pull: %s already exists (use --force to overwrite)", localPath)
			}
			return fmt.Errorf("file pull: open local: %w", err)
		}
		defer dst.Close()
		n, err := io.Copy(dst, stream)
		if err != nil {
			return fmt.Errorf("file pull: stream read: %w", err)
		}
		if uint64(n) != expectedSize {
			return fmt.Errorf("file pull: short read (got %d, expected %d)", n, expectedSize)
		}
		return nil
	})
}

// FilePullBytes is the bytes-out variant of FilePull. Used by the WebUI
// wasm bridge to deliver file contents into a browser-side Blob — there
// is no local fs to write into on that side. Returns the file contents
// in a freshly allocated slice; the caller is responsible for whatever
// download / save flow it needs to drive next.
func (c *Client) FilePullBytes(ctx context.Context, taskIDHex, remoteRel string, onProgress ProgressFunc) ([]byte, error) {
	var buf bytes.Buffer
	if err := c.filePullDo(ctx, taskIDHex, remoteRel, func(stream trsf.BidirectionalStream, expectedSize uint64) error {
		buf.Grow(int(expectedSize))
		n, err := copyWithProgress(&buf, stream, expectedSize, onProgress)
		if err != nil {
			return fmt.Errorf("file pull: stream read: %w", err)
		}
		if n != expectedSize {
			return fmt.Errorf("file pull: short read (got %d, expected %d)", n, expectedSize)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// filePullDo opens the pull stream, validates the runner's ack, and
// then invokes body with the live stream and the announced size so the
// caller can route bytes wherever it needs (local file, Buffer for
// WebUI, etc.). Centralises the protocol handshake so the two public
// variants share one tested code path.
//
// Runner ignores the protocol-level force flag for pull (it's a
// read-only op on the runner side); body decides what client-side
// "force" means (overwrite vs. always-new-buffer).
func (c *Client) filePullDo(ctx context.Context, taskIDHex, remoteRel string, body func(stream trsf.BidirectionalStream, expectedSize uint64) error) error {
	stream, err := c.OpenFileTransfer(ctx, taskIDHex, protocol.FileTransferDirection_Pull, remoteRel, 0, false)
	if err != nil {
		return err
	}
	defer stream.CloseBoth()
	if err := stream.AppendData(true); err != nil {
		return fmt.Errorf("file pull: half-close: %w", err)
	}
	ack, err := ReadFileTransferAck(stream)
	if err != nil {
		return fmt.Errorf("file pull: read ack: %w", err)
	}
	if err := ackError("pull", ack); err != nil {
		return err
	}
	return body(stream, ack.ActualSize)
}

// FilePullDir pulls the worktree directory at remoteRel into localDir. Stages
// the extracted tree at <localDir>.staging-<random>/ and renames atomically
// on success. Refuses to overwrite an existing local dest unless force is set.
func (c *Client) FilePullDir(ctx context.Context, taskIDHex, remoteRel, localDir string, force bool) error {
	if fi, err := os.Lstat(localDir); err == nil {
		if !fi.IsDir() {
			return fmt.Errorf("file pull --recursive: %s exists and is not a directory", localDir)
		}
		if !force {
			return fmt.Errorf("file pull --recursive: %s already exists (use --force to overwrite)", localDir)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("file pull --recursive: stat local: %w", err)
	}

	stream, err := c.OpenFileTransfer(ctx, taskIDHex, protocol.FileTransferDirection_DirPull, remoteRel, 0, false)
	if err != nil {
		return err
	}
	defer stream.CloseBoth()
	if err := stream.AppendData(true); err != nil {
		return fmt.Errorf("file pull --recursive: half-close: %w", err)
	}
	ack, err := ReadFileTransferAck(stream)
	if err != nil {
		return fmt.Errorf("file pull --recursive: read ack: %w", err)
	}
	if err := ackError("pull --recursive", ack); err != nil {
		return err
	}

	staging, err := mkLocalStaging(localDir)
	if err != nil {
		return fmt.Errorf("file pull --recursive: create staging: %w", err)
	}
	cleanupStaging := true
	defer func() {
		if cleanupStaging {
			_ = os.RemoveAll(staging)
		}
	}()

	tr := tar.NewReader(stream)
	for {
		hdr, terr := tr.Next()
		if terr == io.EOF {
			break
		}
		if terr != nil {
			return fmt.Errorf("file pull --recursive: tar read: %w", terr)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeDir {
			return fmt.Errorf("file pull --recursive: unexpected entry type %d in %s", hdr.Typeflag, hdr.Name)
		}
		full, perr := validateRelPathLocal(staging, hdr.Name)
		if perr != nil {
			return fmt.Errorf("file pull --recursive: invalid entry %s: %w", hdr.Name, perr)
		}
		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(full, 0o755); err != nil {
				return fmt.Errorf("file pull --recursive: mkdir %s: %w", full, err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return fmt.Errorf("file pull --recursive: mkdir parent of %s: %w", full, err)
		}
		mode := os.FileMode(hdr.Mode & 0o777)
		if mode == 0 {
			mode = 0o644
		}
		f, oerr := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if oerr != nil {
			return fmt.Errorf("file pull --recursive: create %s: %w", full, oerr)
		}
		if _, err := io.Copy(f, tr); err != nil {
			_ = f.Close()
			return fmt.Errorf("file pull --recursive: write %s: %w", full, err)
		}
		if err := f.Close(); err != nil {
			return fmt.Errorf("file pull --recursive: close %s: %w", full, err)
		}
	}

	if force {
		if err := os.RemoveAll(localDir); err != nil {
			return fmt.Errorf("file pull --recursive: replace existing dest: %w", err)
		}
	}
	if err := os.Rename(staging, localDir); err != nil {
		return fmt.Errorf("file pull --recursive: rename staging: %w", err)
	}
	cleanupStaging = false
	return nil
}

// FilePullDirBytes is the bytes-out variant of FilePullDir: it pulls the
// worktree directory at remoteRel and returns the raw tar stream as a
// freshly allocated slice, without extracting to any local fs. Used by the
// WebUI wasm bridge, which has no local filesystem to stage into — the
// browser saves the bytes as a .tar for the user to extract. The returned
// bytes are a complete tar archive (the same stream FilePullDir untars).
func (c *Client) FilePullDirBytes(ctx context.Context, taskIDHex, remoteRel string, onProgress ProgressFunc) ([]byte, error) {
	stream, err := c.OpenFileTransfer(ctx, taskIDHex, protocol.FileTransferDirection_DirPull, remoteRel, 0, false)
	if err != nil {
		return nil, err
	}
	defer stream.CloseBoth()
	if err := stream.AppendData(true); err != nil {
		return nil, fmt.Errorf("file pull --recursive: half-close: %w", err)
	}
	ack, err := ReadFileTransferAck(stream)
	if err != nil {
		return nil, fmt.Errorf("file pull --recursive: read ack: %w", err)
	}
	if err := ackError("pull --recursive", ack); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	// total is unknown for a dir tar (the runner doesn't announce it), so pass
	// 0 — the UI shows bytes-so-far without a percentage.
	if _, err := copyWithProgress(&buf, stream, 0, onProgress); err != nil {
		return nil, fmt.Errorf("file pull --recursive: stream read: %w", err)
	}
	return buf.Bytes(), nil
}

// mkLocalStaging creates <localDir>.staging-<random>/ as a sibling of
// localDir and returns its path. Sibling placement guarantees the rename
// stays on the same filesystem.
func mkLocalStaging(localDir string) (string, error) {
	parent := filepath.Dir(localDir)
	if parent == "" {
		parent = "."
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	dir := filepath.Join(parent, filepath.Base(localDir)+".staging-"+hex.EncodeToString(id[:]))
	if err := os.Mkdir(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}

// validateRelPathLocal is the client-side mirror of runner.ValidateRelPath:
// rejects absolute paths, NUL bytes, and entries whose cleaned form escapes
// staging via "..". Kept private to the cli package.
func validateRelPathLocal(stagingRoot, rel string) (string, error) {
	if strings.ContainsRune(rel, 0) {
		return "", fmt.Errorf("rel path contains NUL")
	}
	if rel == "" {
		return "", fmt.Errorf("rel path empty")
	}
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf("rel path is absolute")
	}
	cleaned := filepath.Clean(rel)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("rel path escapes root")
	}
	full := filepath.Join(stagingRoot, cleaned)
	rootClean := filepath.Clean(stagingRoot)
	if full != rootClean && !strings.HasPrefix(full, rootClean+string(filepath.Separator)) {
		return "", fmt.Errorf("rel path escapes root")
	}
	return full, nil
}
