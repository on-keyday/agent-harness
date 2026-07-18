package runner

import (
	"archive/tar"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// ErrPathInvalid is returned by ValidateRelPath when the input cannot be
// safely resolved inside the worktree root. Callers map this to
// FileTransferStatus_PathInvalid / ListFilesStatus_PathInvalid.
var ErrPathInvalid = errors.New("rel path invalid")

// ValidateRelPath resolves a worktree-relative POSIX path against worktreeRoot.
// Returns the joined absolute path on success.
//
// Rejected:
//   - absolute paths (must be relative to the worktree)
//   - paths containing a NUL byte
//   - paths that, after filepath.Clean, escape worktreeRoot via "..".
//
// An empty rel string resolves to worktreeRoot itself (used by ls of the
// root directory). Trailing slashes are normalized away.
func ValidateRelPath(worktreeRoot, rel string) (string, error) {
	if strings.ContainsRune(rel, 0) {
		return "", ErrPathInvalid
	}
	if rel == "" {
		return filepath.Clean(worktreeRoot), nil
	}
	if filepath.IsAbs(rel) {
		return "", ErrPathInvalid
	}
	cleaned := filepath.Clean(rel)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", ErrPathInvalid
	}
	full := filepath.Join(worktreeRoot, cleaned)
	rootClean := filepath.Clean(worktreeRoot)
	// Defense in depth: confirm the join did not escape the root.
	if full != rootClean && !strings.HasPrefix(full, rootClean+string(filepath.Separator)) {
		return "", ErrPathInvalid
	}
	return full, nil
}

// rejectIfSymlinkInPath returns ErrPathInvalid if any existing prefix of
// fullPath (after worktreeRoot) is a symlink. Non-existent leaf is fine
// (push creates new files); a symlinked intermediate dir is rejected.
//
// This is the runner-side defense: lexical ValidateRelPath cannot detect
// symlinks because they only manifest at filesystem traversal time.
// Without this, a worktree symlink such as `evil → /etc` would let a pull
// of `evil/passwd` exfiltrate `/etc/passwd`, and a push under `evil/` would
// write outside the worktree.
//
// os.Lstat (NOT os.Stat) is mandatory: Stat follows symlinks and would
// return the target's mode, defeating the check.
func rejectIfSymlinkInPath(worktreeRoot, fullPath string) error {
	rootClean := filepath.Clean(worktreeRoot)
	rel, err := filepath.Rel(rootClean, fullPath)
	if err != nil {
		return ErrPathInvalid
	}
	if rel == "." {
		return nil
	}
	// Walk component by component.
	cur := rootClean
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		if err != nil {
			if os.IsNotExist(err) {
				// We've reached a component that doesn't exist yet (push of
				// a new file under a real directory). The remaining tail
				// cannot be a symlink because it doesn't exist. Done.
				return nil
			}
			return ErrPathInvalid
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return ErrPathInvalid
		}
	}
	return nil
}

// worktreeDirFor returns the on-disk worktree path for taskIDHex, mirroring
// the logic in handleOpenExec / handleAssign. Returns "" if the task is
// unknown to this session. The lock is released before returning so callers
// may safely perform I/O on the result.
func (s *Session) worktreeDirFor(taskIDHex string) string {
	s.mu.Lock()
	te, ok := s.tasks[taskIDHex]
	noWorktree := s.NoWorktree
	s.mu.Unlock()
	if !ok || te == nil {
		return ""
	}
	if noWorktree {
		return te.repoPath
	}
	return filepath.Join(te.repoPath, ".harness-worktrees", taskIDHex)
}

// writeAck encodes ack and writes it to the stream. FileTransferAck is a
// fixed-size payload (protocol.FileTransferAckSize bytes); the reader pulls
// exactly that many bytes off the stream and decodes — no in-band length
// prefix needed.
func writeAck(st trsf.BidirectionalStream, status protocol.FileTransferStatus, size uint64) error {
	ack := protocol.FileTransferAck{Status: status, ActualSize: size}
	body, err := ack.Append(nil)
	if err != nil {
		return err
	}
	if _, err := st.Write(body); err != nil {
		return err
	}
	return nil
}

// handleOpenFileTransfer is the runner-side dispatcher for push/pull. It
// owns the stream after this call: it writes the FileTransferAck and
// closes the stream regardless of outcome.
func (s *Session) handleOpenFileTransfer(ctx context.Context, req *protocol.RunnerOpenFileTransferRequest) {
	log := s.logger()
	stream := peer.WaitForBidirectionalStream(ctx, s.Streams, trsf.StreamID(req.StreamId))
	if stream == nil {
		log.Error("file_transfer: stream not visible", "stream_id", req.StreamId)
		return
	}
	defer stream.CloseBoth()

	taskIDHex := hex.EncodeToString(req.TaskId.Id[:])
	worktreeDir := s.worktreeDirFor(taskIDHex)
	if worktreeDir == "" {
		_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
		return
	}

	// Empty rel_path is rejected for push/pull (see spec); ValidateRelPath
	// alone allows it because list_files needs ls-of-root to mean "".
	if len(req.RelPath) == 0 {
		_ = writeAck(stream, protocol.FileTransferStatus_PathInvalid, 0)
		return
	}

	full, err := ValidateRelPath(worktreeDir, string(req.RelPath))
	if err != nil {
		_ = writeAck(stream, protocol.FileTransferStatus_PathInvalid, 0)
		return
	}
	// Defense against symlink traversal: ValidateRelPath is lexical only.
	// A worktree containing a symlink (e.g. `evil → /etc`) would otherwise
	// let `pull evil/passwd` exfiltrate /etc/passwd or `push evil/foo`
	// write outside the worktree.
	if err := rejectIfSymlinkInPath(worktreeDir, full); err != nil {
		_ = writeAck(stream, protocol.FileTransferStatus_PathInvalid, 0)
		return
	}

	switch req.Direction {
	case protocol.FileTransferDirection_Pull:
		s.runPull(stream, full)
	case protocol.FileTransferDirection_Push:
		s.runPush(stream, full, req.Force(), req.MkdirParents())
	case protocol.FileTransferDirection_Delete:
		s.runDelete(stream, full)
	case protocol.FileTransferDirection_DirPush:
		s.runDirPush(stream, worktreeDir, full, req.Force(), req.MkdirParents())
	case protocol.FileTransferDirection_DirPull:
		s.runDirPull(stream, full)
	case protocol.FileTransferDirection_DirDelete:
		s.runDirDelete(stream, full, req.Force())
	case protocol.FileTransferDirection_Mkdir:
		s.runMkdir(stream, full, req.MkdirParents())
	default:
		_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
	}
}

// runDirDelete removes the directory at full. force=false uses os.Remove
// (succeeds only when the directory is empty — ENOTEMPTY is mapped to
// FileTransferStatus_NotEmpty). force=true uses os.RemoveAll (recursive,
// includes non-empty trees and regular-file children but still refuses
// to traverse a symlink — rejectIfSymlinkInPath has already been called
// for the leaf, but interior symlinks within the tree are followed by
// RemoveAll only when removing them as ordinary directory entries, never
// as targets, so this is safe). Regular files at the leaf are rejected
// with NotADirectory; use `delete` for those.
func (s *Session) runDirDelete(stream trsf.BidirectionalStream, full string, force bool) {
	fi, err := os.Lstat(full)
	if err != nil {
		switch {
		case os.IsNotExist(err):
			_ = writeAck(stream, protocol.FileTransferStatus_NotFound, 0)
		default:
			_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
		}
		return
	}
	if !fi.IsDir() {
		_ = writeAck(stream, protocol.FileTransferStatus_NotADirectory, 0)
		return
	}
	if force {
		if err := os.RemoveAll(full); err != nil {
			_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
			return
		}
	} else {
		if err := os.Remove(full); err != nil {
			switch {
			case os.IsNotExist(err):
				_ = writeAck(stream, protocol.FileTransferStatus_NotFound, 0)
			case errors.Is(err, syscall.ENOTEMPTY):
				_ = writeAck(stream, protocol.FileTransferStatus_NotEmpty, 0)
			default:
				_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
			}
			return
		}
	}
	_ = writeAck(stream, protocol.FileTransferStatus_Ok, 0)
}

// runDelete unlinks the file at full and acks the result. Directories are
// rejected (use `dir_delete` for those — files-only is intentional here
// to keep the delete semantics narrow). Symlink check has already been
// performed by handleOpenFileTransfer.
func (s *Session) runDelete(stream trsf.BidirectionalStream, full string) {
	fi, err := os.Lstat(full)
	if err != nil {
		switch {
		case os.IsNotExist(err):
			_ = writeAck(stream, protocol.FileTransferStatus_NotFound, 0)
		default:
			_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
		}
		return
	}
	if fi.IsDir() {
		_ = writeAck(stream, protocol.FileTransferStatus_IsDirectory, 0)
		return
	}
	if err := os.Remove(full); err != nil {
		switch {
		case os.IsNotExist(err):
			_ = writeAck(stream, protocol.FileTransferStatus_NotFound, 0)
		default:
			_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
		}
		return
	}
	_ = writeAck(stream, protocol.FileTransferStatus_Ok, 0)
}

// runMkdir creates the directory at full. parents=false is strict
// os.Mkdir: a missing parent acks not_found, an existing directory acks
// already_exists. parents=true is os.MkdirAll: parents are created and
// an existing directory is ok (idempotent). A regular file at the leaf
// acks not_a_directory in both modes. Mirrors Unix mkdir / mkdir -p.
// Path validation + symlink rejection already ran in the dispatcher.
func (s *Session) runMkdir(stream trsf.BidirectionalStream, full string, parents bool) {
	if fi, err := os.Lstat(full); err == nil {
		if !fi.IsDir() {
			_ = writeAck(stream, protocol.FileTransferStatus_NotADirectory, 0)
			return
		}
		if parents {
			_ = writeAck(stream, protocol.FileTransferStatus_Ok, 0)
		} else {
			_ = writeAck(stream, protocol.FileTransferStatus_AlreadyExists, 0)
		}
		return
	} else if !os.IsNotExist(err) {
		_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
		return
	}
	var err error
	if parents {
		err = os.MkdirAll(full, 0o755)
	} else {
		err = os.Mkdir(full, 0o755)
	}
	if err != nil {
		if os.IsNotExist(err) {
			_ = writeAck(stream, protocol.FileTransferStatus_NotFound, 0)
		} else {
			_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
		}
		return
	}
	_ = writeAck(stream, protocol.FileTransferStatus_Ok, 0)
}

func (s *Session) runPull(stream trsf.BidirectionalStream, full string) {
	f, err := os.Open(full)
	if err != nil {
		switch {
		case os.IsNotExist(err):
			_ = writeAck(stream, protocol.FileTransferStatus_NotFound, 0)
		default:
			_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
		}
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
		return
	}
	if err := writeAck(stream, protocol.FileTransferStatus_Ok, uint64(st.Size())); err != nil {
		return
	}
	// Stream the file body to the client. Errors are silent; the client
	// will see a short read.
	_, _ = io.Copy(stream, f)
	_ = stream.AppendData(true)
}

func (s *Session) runPush(stream trsf.BidirectionalStream, full string, force, mkdirParents bool) {
	if mkdirParents {
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
			return
		}
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if force {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	f, err := os.OpenFile(full, flags, 0o644)
	if err != nil {
		switch {
		case os.IsExist(err):
			_ = writeAck(stream, protocol.FileTransferStatus_AlreadyExists, 0)
		case os.IsNotExist(err):
			// The leaf is created by O_CREATE, so ENOENT can only mean
			// a missing parent directory — diagnosable, unlike io_error.
			_ = writeAck(stream, protocol.FileTransferStatus_NotFound, 0)
		default:
			_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
		}
		return
	}
	written, copyErr := io.Copy(f, stream)
	if copyErr != nil {
		_ = f.Close()
		_ = os.Remove(full)
		_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
		return
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
		return
	}
	if err := f.Close(); err != nil {
		_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
		return
	}
	_ = writeAck(stream, protocol.FileTransferStatus_Ok, uint64(written))
}

// handleListFiles is the runner-side dispatcher for ls. It writes a single
// FileListing payload and closes the stream.
func (s *Session) handleListFiles(ctx context.Context, req *protocol.RunnerListFilesRequest) {
	log := s.logger()
	stream := peer.WaitForBidirectionalStream(ctx, s.Streams, trsf.StreamID(req.StreamId))
	if stream == nil {
		log.Error("list_files: stream not visible", "stream_id", req.StreamId)
		return
	}
	defer stream.CloseBoth()

	taskIDHex := hex.EncodeToString(req.TaskId.Id[:])
	worktreeDir := s.worktreeDirFor(taskIDHex)
	if worktreeDir == "" {
		_ = writeListing(stream, nil)
		return
	}
	full, err := ValidateRelPath(worktreeDir, string(req.RelPath))
	if err != nil {
		_ = writeListing(stream, nil)
		return
	}
	// Same symlink-traversal defense as handleOpenFileTransfer; matches the
	// existing failure-degradation pattern (empty FileListing on any path
	// validation failure rather than a typed error code).
	if err := rejectIfSymlinkInPath(worktreeDir, full); err != nil {
		_ = writeListing(stream, nil)
		return
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		_ = writeListing(stream, nil)
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	out := make([]protocol.FileEntry, 0, len(entries))
	for _, e := range entries {
		fe := protocol.FileEntry{}
		fe.SetName([]byte(e.Name()))
		info, statErr := e.Info()
		if statErr == nil {
			fe.Size = uint64(info.Size())
			fe.Mode = uint32(info.Mode().Perm())
		}
		if e.IsDir() {
			fe.SetIsDir(true)
		}
		out = append(out, fe)
	}
	_ = writeListing(stream, out)
}

func writeListing(st trsf.BidirectionalStream, entries []protocol.FileEntry) error {
	listing := protocol.FileListing{Count: uint32(len(entries)), Entries: entries}
	body, err := listing.Append(nil)
	if err != nil {
		return err
	}
	if _, err := st.Write(body); err != nil {
		return err
	}
	return st.Close()
}

// stagingRoot is the on-disk dir under which dir_push stages incoming trees.
// Lives inside the worktree so rename(2) into the final dest stays on the
// same filesystem.
const stagingRoot = ".harness-staging"

// runDirPush extracts a tar stream into <worktree>/<rel_path>, staging via
// a sibling directory and renaming atomically on success. Refuses to clobber
// an existing dest unless force is set.
func (s *Session) runDirPush(stream trsf.BidirectionalStream, worktreeDir, dest string, force, mkdirParents bool) {
	// Reject existing dest when !force; reject when dest is a regular file
	// (we won't replace a file with a directory regardless of force).
	if fi, err := os.Lstat(dest); err == nil {
		if !fi.IsDir() {
			_ = writeAck(stream, protocol.FileTransferStatus_NotADirectory, 0)
			return
		}
		if !force {
			_ = writeAck(stream, protocol.FileTransferStatus_AlreadyExists, 0)
			return
		}
	} else if !os.IsNotExist(err) {
		_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
		return
	}

	staging, err := mkStagingDir(worktreeDir)
	if err != nil {
		_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
		return
	}
	cleanupStaging := true
	defer func() {
		if cleanupStaging {
			_ = os.RemoveAll(staging)
		}
	}()

	tr := tar.NewReader(stream)
	bytesIn := uint64(0)
	for {
		hdr, terr := tr.Next()
		if terr == io.EOF {
			break
		}
		if terr != nil {
			_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
			return
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeDir {
			_ = writeAck(stream, protocol.FileTransferStatus_PathInvalid, 0)
			return
		}
		entryFull, perr := ValidateRelPath(staging, hdr.Name)
		if perr != nil {
			_ = writeAck(stream, protocol.FileTransferStatus_PathInvalid, 0)
			return
		}
		if hdr.Typeflag == tar.TypeDir {
			if err := os.MkdirAll(entryFull, 0o755); err != nil {
				_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
				return
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(entryFull), 0o755); err != nil {
			_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
			return
		}
		mode := os.FileMode(hdr.Mode & 0o777)
		if mode == 0 {
			mode = 0o644
		}
		f, oerr := os.OpenFile(entryFull, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if oerr != nil {
			_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
			return
		}
		n, cerr := io.Copy(f, tr)
		if cerr != nil {
			_ = f.Close()
			_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
			return
		}
		if err := f.Sync(); err != nil {
			_ = f.Close()
			_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
			return
		}
		if err := f.Close(); err != nil {
			_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
			return
		}
		bytesIn += uint64(n)
	}

	if force {
		if err := os.RemoveAll(dest); err != nil {
			_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
			return
		}
	}
	if mkdirParents {
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
			return
		}
	}
	if err := os.Rename(staging, dest); err != nil {
		if os.IsNotExist(err) {
			_ = writeAck(stream, protocol.FileTransferStatus_NotFound, 0)
		} else {
			_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
		}
		return
	}
	cleanupStaging = false
	_ = writeAck(stream, protocol.FileTransferStatus_Ok, bytesIn)
}

// runDirPull walks the directory at full and writes a tar stream of its
// contents to the client. Symlinks, hard links, and special files are
// silently skipped (only regular files and directories are emitted).
func (s *Session) runDirPull(stream trsf.BidirectionalStream, full string) {
	fi, err := os.Stat(full)
	if err != nil {
		switch {
		case os.IsNotExist(err):
			_ = writeAck(stream, protocol.FileTransferStatus_NotFound, 0)
		default:
			_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
		}
		return
	}
	if !fi.IsDir() {
		_ = writeAck(stream, protocol.FileTransferStatus_NotADirectory, 0)
		return
	}
	if err := writeAck(stream, protocol.FileTransferStatus_Ok, 0); err != nil {
		return
	}

	tw := tar.NewWriter(stream)
	walkErr := filepath.WalkDir(full, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		if path == full {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		// Skip symlinks, hard links, devices, FIFOs.
		if info.Mode()&os.ModeType != 0 && !info.IsDir() {
			return nil
		}
		hdr, herr := tar.FileInfoHeader(info, "")
		if herr != nil {
			return herr
		}
		rel, rerr := filepath.Rel(full, path)
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
		// Mid-stream error after ack: best we can do is close prematurely.
		// Client surfaces this as io.ErrUnexpectedEOF on tar.Reader.Next.
		return
	}
	_ = tw.Close()
	_ = stream.AppendData(true)
}

// mkStagingDir creates <worktreeDir>/.harness-staging/<random>/ and returns
// its absolute path. The parent .harness-staging/ is created as needed and
// left in place after success/failure for future transfers (and for prune
// cleanup on runner crash).
func mkStagingDir(worktreeDir string) (string, error) {
	parent := filepath.Join(worktreeDir, stagingRoot)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	dir := filepath.Join(parent, hex.EncodeToString(id[:]))
	if err := os.Mkdir(dir, 0o755); err != nil {
		return "", err
	}
	return dir, nil
}
