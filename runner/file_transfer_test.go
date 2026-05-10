package runner

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/trsf"
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

// readAck reads a u32-BE-length-prefixed FileTransferAck from the stream.
func readAck(t *testing.T, st trsf.BidirectionalStream) *protocol.FileTransferAck {
	t.Helper()
	var lenBuf [4]byte
	if _, err := io.ReadFull(streamReader(st), lenBuf[:]); err != nil {
		t.Fatalf("read ack length: %v", err)
	}
	n := binary.BigEndian.Uint32(lenBuf[:])
	body := make([]byte, n)
	if _, err := io.ReadFull(streamReader(st), body); err != nil {
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
