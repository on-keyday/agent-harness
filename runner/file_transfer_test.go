package runner

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

func TestValidateRelPath(t *testing.T) {
	cases := []struct {
		name    string
		root    string
		rel     string
		wantOK  bool
		wantOut string // expected joined absolute path; "" if !wantOK
	}{
		{"ok plain", "/wt", "foo.txt", true, "/wt/foo.txt"},
		{"ok subdir", "/wt", "a/b/c.txt", true, "/wt/a/b/c.txt"},
		{"ok empty (root)", "/wt", "", true, "/wt"},
		{"reject absolute", "/wt", "/etc/passwd", false, ""},
		{"reject parent", "/wt", "../escape", false, ""},
		{"reject embedded parent", "/wt", "a/../../escape", false, ""},
		{"reject NUL", "/wt", "a\x00b", false, ""},
		{"reject leading dotdot after clean", "/wt", "./../x", false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ValidateRelPath(tc.root, tc.rel)
			if tc.wantOK && err != nil {
				t.Fatalf("expected ok, got err: %v", err)
			}
			if !tc.wantOK && err == nil {
				t.Fatalf("expected error, got ok with path=%q", got)
			}
			if tc.wantOK && got != tc.wantOut {
				t.Errorf("path mismatch: got %q want %q", got, tc.wantOut)
			}
		})
	}
}

func TestHandleOpenFileTransfer_PushOK(t *testing.T) {
	tmp := t.TempDir()
	taskIDHex := "00112233445566778899aabbccddeeff"
	taskID := mustParseTaskID(t, taskIDHex)

	sess := &Session{NoWorktree: true} // worktree dir == repoPath
	sess.initMaps()
	sess.tasks[taskIDHex] = &taskEntry{repoPath: tmp}

	clientEnd, runnerEnd := newMemoryBidiPair()
	sess.Streams = staticStreamLookup{1: runnerEnd}

	req := &protocol.RunnerOpenFileTransferRequest{
		TaskId:       taskID,
		StreamId:     1,
		Direction:    protocol.FileTransferDirection_Push,
		ExpectedSize: 5,
	}
	req.SetRelPath([]byte("hello.txt"))

	go sess.handleOpenFileTransfer(context.Background(), req)

	// Client writes the payload then EOF.
	if err := clientEnd.AppendData(false, []byte("hello")); err != nil {
		t.Fatalf("client write: %v", err)
	}
	if err := clientEnd.AppendData(true); err != nil {
		t.Fatalf("client EOF: %v", err)
	}

	ack := readAck(t, clientEnd)
	if ack.Status != protocol.FileTransferStatus_Ok {
		t.Fatalf("ack status = %v want ok", ack.Status)
	}
	if ack.ActualSize != 5 {
		t.Errorf("ack size = %d want 5", ack.ActualSize)
	}

	got, err := os.ReadFile(filepath.Join(tmp, "hello.txt"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("file content = %q want %q", got, "hello")
	}
}

func TestHandleOpenFileTransfer_PullOK(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "out.bin"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	taskIDHex := "ffffffffffffffffffffffffffffffff"
	taskID := mustParseTaskID(t, taskIDHex)

	sess := &Session{NoWorktree: true}
	sess.initMaps()
	sess.tasks[taskIDHex] = &taskEntry{repoPath: tmp}

	clientEnd, runnerEnd := newMemoryBidiPair()
	sess.Streams = staticStreamLookup{1: runnerEnd}

	req := &protocol.RunnerOpenFileTransferRequest{
		TaskId:    taskID,
		StreamId:  1,
		Direction: protocol.FileTransferDirection_Pull,
	}
	req.SetRelPath([]byte("out.bin"))

	go sess.handleOpenFileTransfer(context.Background(), req)

	ack := readAck(t, clientEnd)
	if ack.Status != protocol.FileTransferStatus_Ok {
		t.Fatalf("ack status = %v want ok", ack.Status)
	}
	if ack.ActualSize != 7 {
		t.Errorf("ack size = %d want 7", ack.ActualSize)
	}
	got, err := io.ReadAll(streamReader(clientEnd))
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if string(got) != "payload" {
		t.Errorf("body = %q want %q", got, "payload")
	}
}

func TestHandleOpenFileTransfer_PullNotFound(t *testing.T) {
	tmp := t.TempDir()
	taskIDHex := "00000000000000000000000000000001"
	taskID := mustParseTaskID(t, taskIDHex)

	sess := &Session{NoWorktree: true}
	sess.initMaps()
	sess.tasks[taskIDHex] = &taskEntry{repoPath: tmp}

	clientEnd, runnerEnd := newMemoryBidiPair()
	sess.Streams = staticStreamLookup{1: runnerEnd}

	req := &protocol.RunnerOpenFileTransferRequest{
		TaskId:    taskID,
		StreamId:  1,
		Direction: protocol.FileTransferDirection_Pull,
	}
	req.SetRelPath([]byte("nope.bin"))

	go sess.handleOpenFileTransfer(context.Background(), req)
	ack := readAck(t, clientEnd)
	if ack.Status != protocol.FileTransferStatus_NotFound {
		t.Fatalf("ack status = %v want not_found", ack.Status)
	}
}

func TestHandleOpenFileTransfer_PushAlreadyExists(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "x.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	taskIDHex := "00000000000000000000000000000002"
	taskID := mustParseTaskID(t, taskIDHex)

	sess := &Session{NoWorktree: true}
	sess.initMaps()
	sess.tasks[taskIDHex] = &taskEntry{repoPath: tmp}

	clientEnd, runnerEnd := newMemoryBidiPair()
	sess.Streams = staticStreamLookup{1: runnerEnd}

	req := &protocol.RunnerOpenFileTransferRequest{
		TaskId:    taskID,
		StreamId:  1,
		Direction: protocol.FileTransferDirection_Push,
	}
	req.SetRelPath([]byte("x.txt"))

	go sess.handleOpenFileTransfer(context.Background(), req)
	// Client closes its write side without sending bytes — runner already
	// failed at OpenFile.
	_ = clientEnd.AppendData(true)
	ack := readAck(t, clientEnd)
	if ack.Status != protocol.FileTransferStatus_AlreadyExists {
		t.Fatalf("ack status = %v want already_exists", ack.Status)
	}
}

func TestHandleListFiles_OK(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "a.txt"), []byte("aa"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(tmp, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	taskIDHex := "00000000000000000000000000000003"
	taskID := mustParseTaskID(t, taskIDHex)

	sess := &Session{NoWorktree: true}
	sess.initMaps()
	sess.tasks[taskIDHex] = &taskEntry{repoPath: tmp}

	clientEnd, runnerEnd := newMemoryBidiPair()
	sess.Streams = staticStreamLookup{1: runnerEnd}

	req := &protocol.RunnerListFilesRequest{TaskId: taskID, StreamId: 1}
	go sess.handleListFiles(context.Background(), req)

	body, err := io.ReadAll(streamReader(clientEnd))
	if err != nil {
		t.Fatalf("read listing: %v", err)
	}
	listing := &protocol.FileListing{}
	if _, err := listing.Decode(body); err != nil {
		t.Fatalf("decode listing: %v", err)
	}
	if int(listing.Count) != 2 {
		t.Fatalf("count = %d want 2", listing.Count)
	}
	got := []string{
		string(listing.Entries[0].Name),
		string(listing.Entries[1].Name),
	}
	want := []string{"a.txt", "sub"}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("entry[%d] name = %q want %q", i, got[i], want[i])
		}
	}
	if listing.Entries[1].IsDir() != true {
		t.Errorf("entry[1] (sub) IsDir = false want true")
	}
}

// TestHandleOpenFileTransfer_PullRejectSymlink covers the runner-side defense
// against symlink traversal: a worktree containing a symlink such as
// `secret → /etc` must NOT let the client pull `secret/passwd`. Lexical
// ValidateRelPath cannot detect this; the symlink prefix walk in
// rejectIfSymlinkInPath is what blocks it.
func TestHandleOpenFileTransfer_PullRejectSymlink(t *testing.T) {
	tmp := t.TempDir()
	if err := os.Symlink("/etc", filepath.Join(tmp, "secret")); err != nil {
		t.Skipf("symlink create failed (filesystem may not support): %v", err)
	}
	taskIDHex := "00000000000000000000000000000010"
	taskID := mustParseTaskID(t, taskIDHex)

	sess := &Session{NoWorktree: true}
	sess.initMaps()
	sess.tasks[taskIDHex] = &taskEntry{repoPath: tmp}

	clientEnd, runnerEnd := newMemoryBidiPair()
	sess.Streams = staticStreamLookup{1: runnerEnd}

	req := &protocol.RunnerOpenFileTransferRequest{
		TaskId:    taskID,
		StreamId:  1,
		Direction: protocol.FileTransferDirection_Pull,
	}
	req.SetRelPath([]byte("secret/passwd"))

	go sess.handleOpenFileTransfer(context.Background(), req)
	ack := readAck(t, clientEnd)
	if ack.Status != protocol.FileTransferStatus_PathInvalid {
		t.Fatalf("symlink traversal must be rejected, got status=%v", ack.Status)
	}
}

// TestHandleOpenFileTransfer_PushRejectSymlinkParent covers the symmetric
// push case: a symlinked intermediate directory must reject the write so
// data is never landed outside the worktree.
func TestHandleOpenFileTransfer_PushRejectSymlinkParent(t *testing.T) {
	tmp := t.TempDir()
	outsideDir := t.TempDir() // real dir; the symlink points here so a misdirected write would land outside `tmp`
	if err := os.Symlink(outsideDir, filepath.Join(tmp, "outside")); err != nil {
		t.Skipf("symlink create failed: %v", err)
	}
	taskIDHex := "00000000000000000000000000000011"
	taskID := mustParseTaskID(t, taskIDHex)

	sess := &Session{NoWorktree: true}
	sess.initMaps()
	sess.tasks[taskIDHex] = &taskEntry{repoPath: tmp}

	clientEnd, runnerEnd := newMemoryBidiPair()
	sess.Streams = staticStreamLookup{1: runnerEnd}

	req := &protocol.RunnerOpenFileTransferRequest{
		TaskId:    taskID,
		StreamId:  1,
		Direction: protocol.FileTransferDirection_Push,
	}
	req.SetRelPath([]byte("outside/new.txt"))

	go sess.handleOpenFileTransfer(context.Background(), req)
	// Close client send so a hypothetical (broken) push that did open the
	// file would EOF and complete; we still expect PathInvalid below.
	_ = clientEnd.AppendData(true)
	ack := readAck(t, clientEnd)
	if ack.Status != protocol.FileTransferStatus_PathInvalid {
		t.Fatalf("push under symlinked parent must be rejected, got status=%v", ack.Status)
	}
	// Defense in depth: confirm nothing actually landed in the symlink target.
	if _, err := os.Stat(filepath.Join(outsideDir, "new.txt")); err == nil {
		t.Fatalf("file leaked outside the worktree at %s/new.txt", outsideDir)
	}
}

// readAck reads a u32-BE-length-prefixed FileTransferAck from the stream.
func readAck(t *testing.T, st trsf.BidirectionalStream) *protocol.FileTransferAck {
	t.Helper()
	body := make([]byte, protocol.FileTransferAckSize)
	if _, err := io.ReadFull(st, body); err != nil {
		t.Fatalf("read ack body: %v", err)
	}
	ack := &protocol.FileTransferAck{}
	if _, err := ack.Decode(body); err != nil {
		t.Fatalf("decode ack: %v", err)
	}
	return ack
}

func mustParseTaskID(t *testing.T, hexStr string) protocol.TaskID {
	t.Helper()
	var id protocol.TaskID
	b, err := hex.DecodeString(hexStr)
	if err != nil || len(b) != len(id.Id) {
		t.Fatalf("parse task id: %v", err)
	}
	copy(id.Id[:], b)
	return id
}

// streamReader adapts a BidirectionalStream's read side to io.Reader.
func streamReader(s trsf.BidirectionalStream) io.Reader { return s }

// staticStreamLookup is a test stub that satisfies peer.BidirectionalStreamLookup
// by returning preconfigured streams keyed by trsf.StreamID.
type staticStreamLookup map[trsf.StreamID]trsf.BidirectionalStream

func (m staticStreamLookup) GetBidirectionalStream(id trsf.StreamID) trsf.BidirectionalStream {
	return m[id]
}

func (m staticStreamLookup) GetReceiveStream(id trsf.StreamID) trsf.ReceiveStream {
	if s, ok := m[id]; ok {
		return s
	}
	return nil
}

// Compile-time assertion that staticStreamLookup satisfies the lookup interface.
var _ peer.BidirectionalStreamLookup = staticStreamLookup{}

// memBidi is an in-memory trsf.BidirectionalStream backed by io.Pipe pairs.
// Only the methods exercised by these tests (AppendData, Read, ReadDirect,
// CloseBoth, ID) are required to behave correctly.
type memBidi struct {
	id        trsf.StreamID
	r         *io.PipeReader
	w         *io.PipeWriter
	closeOnce sync.Once
}

func newMemoryBidiPair() (*memBidi, *memBidi) {
	aR, bW := io.Pipe()
	bR, aW := io.Pipe()
	a := &memBidi{id: 1, r: aR, w: aW}
	b := &memBidi{id: 1, r: bR, w: bW}
	return a, b
}

func (m *memBidi) ID() trsf.StreamID { return m.id }

func (m *memBidi) AppendData(eof bool, data ...[]byte) error {
	for _, d := range data {
		if len(d) == 0 {
			continue
		}
		if _, err := m.w.Write(d); err != nil {
			return err
		}
	}
	if eof {
		_ = m.w.Close()
	}
	return nil
}

func (m *memBidi) AppendDataContext(ctx context.Context, eof bool, data ...[]byte) error {
	return m.AppendData(eof, data...)
}

func (m *memBidi) Read(p []byte) (int, error) { return m.r.Read(p) }

func (m *memBidi) ReadContext(ctx context.Context, p []byte) (int, error) {
	return m.r.Read(p)
}

func (m *memBidi) ReadDirect(maxN uint64) ([]byte, bool, error) {
	buf := make([]byte, maxN)
	n, err := m.r.Read(buf)
	if err == io.EOF {
		return buf[:n], true, nil
	}
	if err != nil {
		return nil, false, err
	}
	return buf[:n], false, nil
}

func (m *memBidi) ReadDirectContext(ctx context.Context, maxN uint64) ([]byte, bool, error) {
	return m.ReadDirect(maxN)
}

func (m *memBidi) Write(p []byte) (int, error) { return m.w.Write(p) }

func (m *memBidi) WriteContext(ctx context.Context, p []byte) (int, error) {
	return m.w.Write(p)
}

func (m *memBidi) Close() error { return m.w.Close() }

func (m *memBidi) CloseBoth() error {
	m.closeOnce.Do(func() {
		_ = m.w.Close()
		_ = m.r.Close()
	})
	return nil
}

func (m *memBidi) HasSendData() bool { return false }
func (m *memBidi) Completed() bool   { return false }
func (m *memBidi) HasRecvData() bool { return false }
func (m *memBidi) EOF() bool         { return false }
func (m *memBidi) Cancel()           { _ = m.CloseBoth() }

func TestHandleOpenFileTransfer_DeleteOK(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "doomed.txt")
	if err := os.WriteFile(target, []byte("bye"), 0o644); err != nil {
		t.Fatal(err)
	}
	taskIDHex := "00000000000000000000000000000020"
	taskID := mustParseTaskID(t, taskIDHex)

	sess := &Session{NoWorktree: true}
	sess.initMaps()
	sess.tasks[taskIDHex] = &taskEntry{repoPath: tmp}

	clientEnd, runnerEnd := newMemoryBidiPair()
	sess.Streams = staticStreamLookup{1: runnerEnd}

	req := &protocol.RunnerOpenFileTransferRequest{
		TaskId:    taskID,
		StreamId:  1,
		Direction: protocol.FileTransferDirection_Delete,
	}
	req.SetRelPath([]byte("doomed.txt"))

	go sess.handleOpenFileTransfer(context.Background(), req)
	_ = clientEnd.AppendData(true) // half-close: delete sends no client bytes
	ack := readAck(t, clientEnd)
	if ack.Status != protocol.FileTransferStatus_Ok {
		t.Fatalf("ack status = %v want ok", ack.Status)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("file was not removed: stat err=%v", err)
	}
}

func TestHandleOpenFileTransfer_DeleteNotFound(t *testing.T) {
	tmp := t.TempDir()
	taskIDHex := "00000000000000000000000000000021"
	taskID := mustParseTaskID(t, taskIDHex)

	sess := &Session{NoWorktree: true}
	sess.initMaps()
	sess.tasks[taskIDHex] = &taskEntry{repoPath: tmp}

	clientEnd, runnerEnd := newMemoryBidiPair()
	sess.Streams = staticStreamLookup{1: runnerEnd}

	req := &protocol.RunnerOpenFileTransferRequest{
		TaskId:    taskID,
		StreamId:  1,
		Direction: protocol.FileTransferDirection_Delete,
	}
	req.SetRelPath([]byte("absent.txt"))

	go sess.handleOpenFileTransfer(context.Background(), req)
	_ = clientEnd.AppendData(true)
	ack := readAck(t, clientEnd)
	if ack.Status != protocol.FileTransferStatus_NotFound {
		t.Fatalf("ack status = %v want not_found", ack.Status)
	}
}

func TestHandleOpenFileTransfer_PushForceOverwrites(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "x.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	taskIDHex := "00000000000000000000000000000030"
	taskID := mustParseTaskID(t, taskIDHex)

	sess := &Session{NoWorktree: true}
	sess.initMaps()
	sess.tasks[taskIDHex] = &taskEntry{repoPath: tmp}

	clientEnd, runnerEnd := newMemoryBidiPair()
	sess.Streams = staticStreamLookup{1: runnerEnd}

	req := &protocol.RunnerOpenFileTransferRequest{
		TaskId:    taskID,
		StreamId:  1,
		Direction: protocol.FileTransferDirection_Push,
	}
	req.SetRelPath([]byte("x.txt"))
	req.SetForce(true)

	go sess.handleOpenFileTransfer(context.Background(), req)
	if err := clientEnd.AppendData(false, []byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := clientEnd.AppendData(true); err != nil {
		t.Fatal(err)
	}
	ack := readAck(t, clientEnd)
	if ack.Status != protocol.FileTransferStatus_Ok {
		t.Fatalf("ack status = %v want ok", ack.Status)
	}
	got, _ := os.ReadFile(filepath.Join(tmp, "x.txt"))
	if string(got) != "new" {
		t.Errorf("file = %q want %q", got, "new")
	}
}

func TestHandleOpenFileTransfer_DeleteRejectDirectory(t *testing.T) {
	tmp := t.TempDir()
	if err := os.Mkdir(filepath.Join(tmp, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	taskIDHex := "00000000000000000000000000000022"
	taskID := mustParseTaskID(t, taskIDHex)

	sess := &Session{NoWorktree: true}
	sess.initMaps()
	sess.tasks[taskIDHex] = &taskEntry{repoPath: tmp}

	clientEnd, runnerEnd := newMemoryBidiPair()
	sess.Streams = staticStreamLookup{1: runnerEnd}

	req := &protocol.RunnerOpenFileTransferRequest{
		TaskId:    taskID,
		StreamId:  1,
		Direction: protocol.FileTransferDirection_Delete,
	}
	req.SetRelPath([]byte("subdir"))

	go sess.handleOpenFileTransfer(context.Background(), req)
	_ = clientEnd.AppendData(true)
	ack := readAck(t, clientEnd)
	if ack.Status != protocol.FileTransferStatus_IsDirectory {
		t.Fatalf("ack status = %v want is_directory", ack.Status)
	}
	if _, err := os.Stat(filepath.Join(tmp, "subdir")); err != nil {
		t.Fatalf("dir should still exist: %v", err)
	}
}

// pushTar packs the entries map (rel name → bytes; "/" suffix marks a dir)
// and feeds the bytes into the client end of the bidi pair as a tar stream.
// The actual write happens in a background goroutine so the caller can
// concurrently read the runner's ack — required because the runner may
// reject the stream mid-flight (e.g. symlink entry, path traversal) and
// then block writing its ack until the client drains it. Best-effort write
// errors after rejection are ignored.
func pushTar(t *testing.T, clientEnd trsf.BidirectionalStream, entries map[string][]byte) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, body := range entries {
		hdr := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}
		if strings.HasSuffix(name, "/") {
			hdr.Typeflag = tar.TypeDir
			hdr.Size = 0
			hdr.Mode = 0o755
		} else {
			hdr.Typeflag = tar.TypeReg
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("WriteHeader(%q): %v", name, err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write(body); err != nil {
				t.Fatalf("Write(%q): %v", name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tw.Close: %v", err)
	}
	go func(payload []byte) {
		_ = clientEnd.AppendData(false, payload)
		_ = clientEnd.AppendData(true)
	}(buf.Bytes())
}

func pushSymlinkTar(t *testing.T, clientEnd trsf.BidirectionalStream, name, target string) {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := &tar.Header{Name: name, Linkname: target, Typeflag: tar.TypeSymlink, Mode: 0o644}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	go func(payload []byte) {
		_ = clientEnd.AppendData(false, payload)
		_ = clientEnd.AppendData(true)
	}(buf.Bytes())
}

func TestHandleOpenFileTransfer_DirPushOK(t *testing.T) {
	tmp := t.TempDir()
	taskIDHex := "00000000000000000000000000000040"
	taskID := mustParseTaskID(t, taskIDHex)

	sess := &Session{NoWorktree: true}
	sess.initMaps()
	sess.tasks[taskIDHex] = &taskEntry{repoPath: tmp}

	clientEnd, runnerEnd := newMemoryBidiPair()
	sess.Streams = staticStreamLookup{1: runnerEnd}

	req := &protocol.RunnerOpenFileTransferRequest{
		TaskId:    taskID,
		StreamId:  1,
		Direction: protocol.FileTransferDirection_DirPush,
	}
	req.SetRelPath([]byte("incoming"))

	go sess.handleOpenFileTransfer(context.Background(), req)
	pushTar(t, clientEnd, map[string][]byte{
		"a.txt":     []byte("AA"),
		"sub/":      nil,
		"sub/b.txt": []byte("BBB"),
	})

	ack := readAck(t, clientEnd)
	if ack.Status != protocol.FileTransferStatus_Ok {
		t.Fatalf("ack status = %v want ok", ack.Status)
	}
	if got, _ := os.ReadFile(filepath.Join(tmp, "incoming", "a.txt")); string(got) != "AA" {
		t.Errorf("a.txt = %q", got)
	}
	if got, _ := os.ReadFile(filepath.Join(tmp, "incoming", "sub", "b.txt")); string(got) != "BBB" {
		t.Errorf("sub/b.txt = %q", got)
	}
	entries, _ := os.ReadDir(filepath.Join(tmp, ".harness-staging"))
	if len(entries) != 0 {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("staging not cleaned: %v", names)
	}
}

func TestHandleOpenFileTransfer_DirPushRejectExisting(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "incoming"), 0o755); err != nil {
		t.Fatal(err)
	}
	taskIDHex := "00000000000000000000000000000041"
	taskID := mustParseTaskID(t, taskIDHex)

	sess := &Session{NoWorktree: true}
	sess.initMaps()
	sess.tasks[taskIDHex] = &taskEntry{repoPath: tmp}

	clientEnd, runnerEnd := newMemoryBidiPair()
	sess.Streams = staticStreamLookup{1: runnerEnd}

	req := &protocol.RunnerOpenFileTransferRequest{
		TaskId:    taskID,
		StreamId:  1,
		Direction: protocol.FileTransferDirection_DirPush,
	}
	req.SetRelPath([]byte("incoming"))

	go sess.handleOpenFileTransfer(context.Background(), req)
	_ = clientEnd.AppendData(true)

	ack := readAck(t, clientEnd)
	if ack.Status != protocol.FileTransferStatus_AlreadyExists {
		t.Fatalf("ack status = %v want already_exists", ack.Status)
	}
}

func TestHandleOpenFileTransfer_DirPushForceOverwrites(t *testing.T) {
	tmp := t.TempDir()
	old := filepath.Join(tmp, "incoming")
	if err := os.MkdirAll(old, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(old, "stale.txt"), []byte("OLD"), 0o644); err != nil {
		t.Fatal(err)
	}
	taskIDHex := "00000000000000000000000000000042"
	taskID := mustParseTaskID(t, taskIDHex)

	sess := &Session{NoWorktree: true}
	sess.initMaps()
	sess.tasks[taskIDHex] = &taskEntry{repoPath: tmp}

	clientEnd, runnerEnd := newMemoryBidiPair()
	sess.Streams = staticStreamLookup{1: runnerEnd}

	req := &protocol.RunnerOpenFileTransferRequest{
		TaskId:    taskID,
		StreamId:  1,
		Direction: protocol.FileTransferDirection_DirPush,
	}
	req.SetRelPath([]byte("incoming"))
	req.SetForce(true)

	go sess.handleOpenFileTransfer(context.Background(), req)
	pushTar(t, clientEnd, map[string][]byte{"fresh.txt": []byte("NEW")})

	ack := readAck(t, clientEnd)
	if ack.Status != protocol.FileTransferStatus_Ok {
		t.Fatalf("ack status = %v want ok", ack.Status)
	}
	if _, err := os.Stat(filepath.Join(old, "stale.txt")); !os.IsNotExist(err) {
		t.Errorf("stale.txt should have been replaced; err=%v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(old, "fresh.txt")); string(got) != "NEW" {
		t.Errorf("fresh.txt = %q", got)
	}
}

func TestHandleOpenFileTransfer_DirPushRejectSymlinkEntry(t *testing.T) {
	tmp := t.TempDir()
	taskIDHex := "00000000000000000000000000000043"
	taskID := mustParseTaskID(t, taskIDHex)

	sess := &Session{NoWorktree: true}
	sess.initMaps()
	sess.tasks[taskIDHex] = &taskEntry{repoPath: tmp}

	clientEnd, runnerEnd := newMemoryBidiPair()
	sess.Streams = staticStreamLookup{1: runnerEnd}

	req := &protocol.RunnerOpenFileTransferRequest{
		TaskId:    taskID,
		StreamId:  1,
		Direction: protocol.FileTransferDirection_DirPush,
	}
	req.SetRelPath([]byte("incoming"))

	go sess.handleOpenFileTransfer(context.Background(), req)
	pushSymlinkTar(t, clientEnd, "evil", "/etc/passwd")

	ack := readAck(t, clientEnd)
	if ack.Status != protocol.FileTransferStatus_PathInvalid {
		t.Fatalf("ack status = %v want path_invalid", ack.Status)
	}
	if _, err := os.Stat(filepath.Join(tmp, "incoming")); !os.IsNotExist(err) {
		t.Errorf("dest must not exist after rejected push; err=%v", err)
	}
}

func TestHandleOpenFileTransfer_DirPullOK(t *testing.T) {
	tmp := t.TempDir()
	src := filepath.Join(tmp, "outgoing")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("AA"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "b.txt"), []byte("BBB"), 0o644); err != nil {
		t.Fatal(err)
	}
	taskIDHex := "00000000000000000000000000000050"
	taskID := mustParseTaskID(t, taskIDHex)

	sess := &Session{NoWorktree: true}
	sess.initMaps()
	sess.tasks[taskIDHex] = &taskEntry{repoPath: tmp}

	clientEnd, runnerEnd := newMemoryBidiPair()
	sess.Streams = staticStreamLookup{1: runnerEnd}

	req := &protocol.RunnerOpenFileTransferRequest{
		TaskId:    taskID,
		StreamId:  1,
		Direction: protocol.FileTransferDirection_DirPull,
	}
	req.SetRelPath([]byte("outgoing"))

	go sess.handleOpenFileTransfer(context.Background(), req)

	ack := readAck(t, clientEnd)
	if ack.Status != protocol.FileTransferStatus_Ok {
		t.Fatalf("ack status = %v want ok", ack.Status)
	}
	body, err := io.ReadAll(streamReader(clientEnd))
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	tr := tar.NewReader(bytes.NewReader(body))
	files := map[string][]byte{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			b, _ := io.ReadAll(tr)
			files[hdr.Name] = b
		}
	}
	if string(files["a.txt"]) != "AA" {
		t.Errorf("a.txt = %q want AA", files["a.txt"])
	}
	if string(files["sub/b.txt"]) != "BBB" {
		t.Errorf("sub/b.txt = %q want BBB", files["sub/b.txt"])
	}
}

func TestHandleOpenFileTransfer_DirPullNotADirectory(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "afile"), []byte("X"), 0o644); err != nil {
		t.Fatal(err)
	}
	taskIDHex := "00000000000000000000000000000051"
	taskID := mustParseTaskID(t, taskIDHex)

	sess := &Session{NoWorktree: true}
	sess.initMaps()
	sess.tasks[taskIDHex] = &taskEntry{repoPath: tmp}

	clientEnd, runnerEnd := newMemoryBidiPair()
	sess.Streams = staticStreamLookup{1: runnerEnd}

	req := &protocol.RunnerOpenFileTransferRequest{
		TaskId:    taskID,
		StreamId:  1,
		Direction: protocol.FileTransferDirection_DirPull,
	}
	req.SetRelPath([]byte("afile"))

	go sess.handleOpenFileTransfer(context.Background(), req)
	ack := readAck(t, clientEnd)
	if ack.Status != protocol.FileTransferStatus_NotADirectory {
		t.Fatalf("ack status = %v want not_a_directory", ack.Status)
	}
}

func TestHandleOpenFileTransfer_DirPushRejectPathTraversal(t *testing.T) {
	tmp := t.TempDir()
	taskIDHex := "00000000000000000000000000000044"
	taskID := mustParseTaskID(t, taskIDHex)

	sess := &Session{NoWorktree: true}
	sess.initMaps()
	sess.tasks[taskIDHex] = &taskEntry{repoPath: tmp}

	clientEnd, runnerEnd := newMemoryBidiPair()
	sess.Streams = staticStreamLookup{1: runnerEnd}

	req := &protocol.RunnerOpenFileTransferRequest{
		TaskId:    taskID,
		StreamId:  1,
		Direction: protocol.FileTransferDirection_DirPush,
	}
	req.SetRelPath([]byte("incoming"))

	go sess.handleOpenFileTransfer(context.Background(), req)
	pushTar(t, clientEnd, map[string][]byte{"../escape": []byte("X")})

	ack := readAck(t, clientEnd)
	if ack.Status != protocol.FileTransferStatus_PathInvalid {
		t.Fatalf("ack status = %v want path_invalid", ack.Status)
	}
}

// --- dir_delete -----------------------------------------------------------

// dirDeleteRequest builds the boilerplate Session + stream pair used by the
// dir_delete tests below. Returns the ack-reader half of the bidi pair.
func dirDeleteRequest(t *testing.T, tmp, taskIDHex, rel string, force bool) *memBidi {
	t.Helper()
	taskID := mustParseTaskID(t, taskIDHex)
	sess := &Session{NoWorktree: true}
	sess.initMaps()
	sess.tasks[taskIDHex] = &taskEntry{repoPath: tmp}

	clientEnd, runnerEnd := newMemoryBidiPair()
	sess.Streams = staticStreamLookup{1: runnerEnd}

	req := &protocol.RunnerOpenFileTransferRequest{
		TaskId:    taskID,
		StreamId:  1,
		Direction: protocol.FileTransferDirection_DirDelete,
	}
	req.SetRelPath([]byte(rel))
	req.SetForce(force)

	go sess.handleOpenFileTransfer(context.Background(), req)
	_ = clientEnd.AppendData(true)
	return clientEnd
}

func TestHandleOpenFileTransfer_DirDeleteEmpty(t *testing.T) {
	tmp := t.TempDir()
	if err := os.Mkdir(filepath.Join(tmp, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	client := dirDeleteRequest(t, tmp, "00000000000000000000000000000040", "empty", false)
	ack := readAck(t, client)
	if ack.Status != protocol.FileTransferStatus_Ok {
		t.Fatalf("ack status = %v want ok", ack.Status)
	}
	if _, err := os.Stat(filepath.Join(tmp, "empty")); !os.IsNotExist(err) {
		t.Fatalf("dir should be gone: stat err=%v", err)
	}
}

func TestHandleOpenFileTransfer_DirDeleteNonEmptyWithoutForce(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "notempty")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "inside.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	client := dirDeleteRequest(t, tmp, "00000000000000000000000000000041", "notempty", false)
	ack := readAck(t, client)
	if ack.Status != protocol.FileTransferStatus_NotEmpty {
		t.Fatalf("ack status = %v want not_empty", ack.Status)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("dir should still exist: %v", err)
	}
}

func TestHandleOpenFileTransfer_DirDeleteNonEmptyWithForce(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "tree")
	if err := os.MkdirAll(filepath.Join(dir, "nested", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", "b.txt"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	client := dirDeleteRequest(t, tmp, "00000000000000000000000000000042", "tree", true)
	ack := readAck(t, client)
	if ack.Status != protocol.FileTransferStatus_Ok {
		t.Fatalf("ack status = %v want ok", ack.Status)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("tree should be gone recursively: stat err=%v", err)
	}
}

func TestHandleOpenFileTransfer_DirDeleteRejectsFile(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "leaf.txt")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Both force values should be rejected — dir_delete is dirs only.
	for _, force := range []bool{false, true} {
		client := dirDeleteRequest(t, tmp, "00000000000000000000000000000043", "leaf.txt", force)
		ack := readAck(t, client)
		if ack.Status != protocol.FileTransferStatus_NotADirectory {
			t.Fatalf("force=%v: ack status = %v want not_a_directory", force, ack.Status)
		}
		if _, err := os.Stat(target); err != nil {
			t.Fatalf("force=%v: file should still exist: %v", force, err)
		}
	}
}

func TestHandleOpenFileTransfer_DirDeleteNotFound(t *testing.T) {
	tmp := t.TempDir()
	client := dirDeleteRequest(t, tmp, "00000000000000000000000000000044", "absent", false)
	ack := readAck(t, client)
	if ack.Status != protocol.FileTransferStatus_NotFound {
		t.Fatalf("ack status = %v want not_found", ack.Status)
	}
}

// pushReq builds a RunnerOpenFileTransferRequest for tests below.
func pushReq(t *testing.T, taskIDHex, rel string, dir protocol.FileTransferDirection, parents bool) *protocol.RunnerOpenFileTransferRequest {
	t.Helper()
	req := &protocol.RunnerOpenFileTransferRequest{
		TaskId:    mustParseTaskID(t, taskIDHex),
		StreamId:  1,
		Direction: dir,
	}
	req.SetRelPath([]byte(rel))
	req.SetMkdirParents(parents)
	return req
}

func newFileSession(t *testing.T, tmp, taskIDHex string) (*Session, trsf.BidirectionalStream) {
	t.Helper()
	sess := &Session{NoWorktree: true}
	sess.initMaps()
	sess.tasks[taskIDHex] = &taskEntry{repoPath: tmp}
	clientEnd, runnerEnd := newMemoryBidiPair()
	sess.Streams = staticStreamLookup{1: runnerEnd}
	return sess, clientEnd
}

func TestHandleOpenFileTransfer_PushMissingParentNotFound(t *testing.T) {
	tmp := t.TempDir()
	taskIDHex := "00000000000000000000000000000010"
	sess, clientEnd := newFileSession(t, tmp, taskIDHex)
	req := pushReq(t, taskIDHex, "no/such/dir/f.txt", protocol.FileTransferDirection_Push, false)

	go sess.handleOpenFileTransfer(context.Background(), req)
	// Runner fails at OpenFile before reading any payload; just close
	// our write side (same shape as the AlreadyExists test).
	if err := clientEnd.AppendData(true); err != nil {
		t.Fatalf("client EOF: %v", err)
	}
	ack := readAck(t, clientEnd)
	if ack.Status != protocol.FileTransferStatus_NotFound {
		t.Fatalf("ack status = %v want not_found", ack.Status)
	}
}

func TestHandleOpenFileTransfer_PushMkdirParentsOK(t *testing.T) {
	tmp := t.TempDir()
	taskIDHex := "00000000000000000000000000000011"
	sess, clientEnd := newFileSession(t, tmp, taskIDHex)
	req := pushReq(t, taskIDHex, "a/b/f.txt", protocol.FileTransferDirection_Push, true)
	req.ExpectedSize = 5

	go sess.handleOpenFileTransfer(context.Background(), req)
	// Payload writes happen in a background goroutine (not inline) because
	// the runner may reject the push before reading any payload (e.g. the
	// RED phase against the old runPush, which fails at OpenFile without
	// ever draining the stream); an inline AppendData would then block
	// forever on the unread io_error ack. Errors are ignored here — the
	// ack assertion below is the real check.
	go func() {
		_ = clientEnd.AppendData(false, []byte("hello"))
		_ = clientEnd.AppendData(true)
	}()
	ack := readAck(t, clientEnd)
	if ack.Status != protocol.FileTransferStatus_Ok {
		t.Fatalf("ack status = %v want ok", ack.Status)
	}
	got, err := os.ReadFile(filepath.Join(tmp, "a", "b", "f.txt"))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("content = %q want hello", got)
	}
}

func TestHandleOpenFileTransfer_DirPushMissingParent(t *testing.T) {
	tmp := t.TempDir()
	taskIDHex := "00000000000000000000000000000012"

	// Without parents: not_found.
	sess, clientEnd := newFileSession(t, tmp, taskIDHex)
	req := pushReq(t, taskIDHex, "no/such/dest", protocol.FileTransferDirection_DirPush, false)
	go sess.handleOpenFileTransfer(context.Background(), req)
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	if err := tw.WriteHeader(&tar.Header{Name: "inner.txt", Typeflag: tar.TypeReg, Size: 2, Mode: 0o644}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("hi")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := clientEnd.AppendData(false, tarBuf.Bytes()); err != nil {
		t.Fatalf("client write: %v", err)
	}
	if err := clientEnd.AppendData(true); err != nil {
		t.Fatalf("client EOF: %v", err)
	}
	if ack := readAck(t, clientEnd); ack.Status != protocol.FileTransferStatus_NotFound {
		t.Fatalf("no-parents ack = %v want not_found", ack.Status)
	}

	// With parents: ok, tree lands.
	sess2, clientEnd2 := newFileSession(t, tmp, taskIDHex)
	req2 := pushReq(t, taskIDHex, "no/such/dest", protocol.FileTransferDirection_DirPush, true)
	go sess2.handleOpenFileTransfer(context.Background(), req2)
	if err := clientEnd2.AppendData(false, tarBuf.Bytes()); err != nil {
		t.Fatalf("client write: %v", err)
	}
	if err := clientEnd2.AppendData(true); err != nil {
		t.Fatalf("client EOF: %v", err)
	}
	if ack := readAck(t, clientEnd2); ack.Status != protocol.FileTransferStatus_Ok {
		t.Fatalf("parents ack = %v want ok", ack.Status)
	}
	if _, err := os.Stat(filepath.Join(tmp, "no", "such", "dest", "inner.txt")); err != nil {
		t.Errorf("pushed tree missing: %v", err)
	}
}

func TestHandleOpenFileTransfer_Mkdir(t *testing.T) {
	tmp := t.TempDir()
	taskIDHex := "00000000000000000000000000000013"

	run := func(rel string, parents bool) protocol.FileTransferStatus {
		sess, clientEnd := newFileSession(t, tmp, taskIDHex)
		req := pushReq(t, taskIDHex, rel, protocol.FileTransferDirection_Mkdir, parents)
		go sess.handleOpenFileTransfer(context.Background(), req)
		if err := clientEnd.AppendData(true); err != nil {
			t.Fatalf("client EOF: %v", err)
		}
		return readAck(t, clientEnd).Status
	}

	// Strict mkdir with missing parent → not_found.
	if got := run("deep/nested/dir", false); got != protocol.FileTransferStatus_NotFound {
		t.Errorf("strict missing parent = %v want not_found", got)
	}
	// -p creates the whole chain.
	if got := run("deep/nested/dir", true); got != protocol.FileTransferStatus_Ok {
		t.Errorf("-p nested = %v want ok", got)
	}
	if fi, err := os.Stat(filepath.Join(tmp, "deep", "nested", "dir")); err != nil || !fi.IsDir() {
		t.Fatalf("dir not created: fi=%v err=%v", fi, err)
	}
	// Strict on an existing dir → already_exists; -p is idempotent ok.
	if got := run("deep/nested/dir", false); got != protocol.FileTransferStatus_AlreadyExists {
		t.Errorf("strict existing = %v want already_exists", got)
	}
	if got := run("deep/nested/dir", true); got != protocol.FileTransferStatus_Ok {
		t.Errorf("-p existing = %v want ok", got)
	}
	// Strict with existing parent → ok.
	if got := run("deep/sibling", false); got != protocol.FileTransferStatus_Ok {
		t.Errorf("strict existing parent = %v want ok", got)
	}
	// Leaf is a regular file → not_a_directory in both modes.
	if err := os.WriteFile(filepath.Join(tmp, "plainfile"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := run("plainfile", false); got != protocol.FileTransferStatus_NotADirectory {
		t.Errorf("strict on file = %v want not_a_directory", got)
	}
	if got := run("plainfile", true); got != protocol.FileTransferStatus_NotADirectory {
		t.Errorf("-p on file = %v want not_a_directory", got)
	}
}

// TestHandleOpenFileTransfer_MkdirRejectSymlinkParent covers the mkdir
// dispatch's use of rejectIfSymlinkInPath: a symlinked intermediate
// directory must reject the request so no directory is ever created under
// the symlink's target, mirroring TestHandleOpenFileTransfer_PushRejectSymlinkParent
// above.
func TestHandleOpenFileTransfer_MkdirRejectSymlinkParent(t *testing.T) {
	tmp := t.TempDir()
	outsideDir := t.TempDir() // real dir; the symlink points here so a misdirected mkdir would land outside `tmp`
	if err := os.Symlink(outsideDir, filepath.Join(tmp, "outside")); err != nil {
		t.Skipf("symlink create failed: %v", err)
	}
	taskIDHex := "00000000000000000000000000000014"
	sess, clientEnd := newFileSession(t, tmp, taskIDHex)
	req := pushReq(t, taskIDHex, "outside/new", protocol.FileTransferDirection_Mkdir, true)

	go sess.handleOpenFileTransfer(context.Background(), req)
	if err := clientEnd.AppendData(true); err != nil {
		t.Fatalf("client EOF: %v", err)
	}
	ack := readAck(t, clientEnd)
	if ack.Status != protocol.FileTransferStatus_PathInvalid {
		t.Fatalf("mkdir under symlinked parent must be rejected, got status=%v", ack.Status)
	}
	// Defense in depth: confirm nothing actually landed in the symlink target.
	if _, err := os.Stat(filepath.Join(outsideDir, "new")); err == nil {
		t.Fatalf("dir leaked outside the worktree at %s/new", outsideDir)
	}
}
