package runner

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/trsf"
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

// writeAck encodes ack with a 4-byte BE length prefix and writes it to the
// stream. Used by push/pull only.
func writeAck(st trsf.BidirectionalStream, status protocol.FileTransferStatus, size uint64) error {
	ack := protocol.FileTransferAck{Status: status, ActualSize: size}
	body, err := ack.Append(nil)
	if err != nil {
		return err
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(body)))
	return st.AppendData(false, hdr[:], body)
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

	switch req.Direction {
	case protocol.FileTransferDirection_Pull:
		s.runPull(stream, full)
	case protocol.FileTransferDirection_Push:
		s.runPush(stream, full)
	default:
		_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
	}
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
	_, _ = io.Copy(streamWriter{stream}, f)
	_ = stream.AppendData(true)
}

func (s *Session) runPush(stream trsf.BidirectionalStream, full string) {
	f, err := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		switch {
		case os.IsExist(err):
			_ = writeAck(stream, protocol.FileTransferStatus_AlreadyExists, 0)
		default:
			_ = writeAck(stream, protocol.FileTransferStatus_IoError, 0)
		}
		return
	}
	written, copyErr := io.Copy(f, streamReader2{stream})
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

// streamWriter / streamReader2 adapt a trsf.BidirectionalStream to the
// io.Writer / io.Reader interfaces (without involving CloseBoth).
type streamWriter struct{ s trsf.BidirectionalStream }

func (w streamWriter) Write(p []byte) (int, error) {
	if err := w.s.AppendData(false, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

type streamReader2 struct{ s trsf.BidirectionalStream }

func (r streamReader2) Read(p []byte) (int, error) {
	data, eof, err := r.s.ReadDirect(uint64(len(p)))
	if err != nil {
		return 0, err
	}
	n := copy(p, data)
	if eof && n == len(data) {
		if n == 0 {
			return 0, io.EOF
		}
		return n, io.EOF
	}
	return n, nil
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
	if err := st.AppendData(false, body); err != nil {
		return err
	}
	return st.AppendData(true)
}
