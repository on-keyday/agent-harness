package cli

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/trsf"
)

// FileEntryView is a Go-side projection of protocol.FileEntry so callers
// (CLI code, integration tests) do not have to import the brgen-generated
// type directly.
type FileEntryView struct {
	Name  string
	Size  uint64
	Mode  uint32
	IsDir bool
}

// OpenFileTransfer initiates a push or pull and returns the bidi stream.
// Caller drives the stream (write file bytes for push, read for pull) and
// is responsible for reading the trailing FileTransferAck.
func (c *Client) OpenFileTransfer(
	ctx context.Context,
	taskIDHex string,
	direction protocol.FileTransferDirection,
	relPath string,
	expectedSize uint64,
	force bool,
) (trsf.BidirectionalStream, error) {
	tid, err := parseTaskIDHex(taskIDHex)
	if err != nil {
		return nil, fmt.Errorf("file: parse task id: %w", err)
	}
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_OpenFileTransfer}
	body := protocol.OpenFileTransferRequest{
		TaskId:       tid,
		Direction:    direction,
		ExpectedSize: expectedSize,
	}
	body.SetRelPath([]byte(relPath))
	body.SetForce(force)
	req.SetOpenFileTransfer(body)

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.Kind != protocol.TaskControlKind_OpenFileTransfer {
		return nil, fmt.Errorf("file: unexpected response kind %v", resp.Kind)
	}
	r := resp.OpenFileTransfer()
	if r == nil {
		return nil, errors.New("file: response variant missing")
	}
	if err := openFileTransferStatusError(r.Status); err != nil {
		return nil, err
	}
	st := peer.WaitForBidirectionalStream(ctx, c.Transport(), trsf.StreamID(r.StreamId))
	if st == nil {
		return nil, fmt.Errorf("file: stream %d not visible", r.StreamId)
	}
	return st, nil
}

// ListFiles round-trips a list_files request and decodes the FileListing
// payload. Returns the entries in name order.
func (c *Client) ListFiles(ctx context.Context, taskIDHex, relPath string) ([]FileEntryView, error) {
	tid, err := parseTaskIDHex(taskIDHex)
	if err != nil {
		return nil, fmt.Errorf("file ls: parse task id: %w", err)
	}
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_ListFiles}
	body := protocol.ListFilesRequest{TaskId: tid}
	body.SetRelPath([]byte(relPath))
	req.SetListFiles(body)

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.Kind != protocol.TaskControlKind_ListFiles {
		return nil, fmt.Errorf("file ls: unexpected response kind %v", resp.Kind)
	}
	r := resp.ListFiles()
	if r == nil {
		return nil, errors.New("file ls: response variant missing")
	}
	if err := listFilesStatusError(r.Status); err != nil {
		return nil, err
	}
	st := peer.WaitForBidirectionalStream(ctx, c.Transport(), trsf.StreamID(r.StreamId))
	if st == nil {
		return nil, fmt.Errorf("file ls: stream %d not visible", r.StreamId)
	}
	defer st.CloseBoth()
	if err := st.AppendData(true); err != nil {
		return nil, fmt.Errorf("file ls: half-close: %w", err)
	}
	body2, err := io.ReadAll(streamReadAll{st})
	if err != nil {
		return nil, fmt.Errorf("file ls: read listing: %w", err)
	}
	listing := &protocol.FileListing{}
	if _, err := listing.Decode(body2); err != nil {
		return nil, fmt.Errorf("file ls: decode: %w", err)
	}
	out := make([]FileEntryView, 0, listing.Count)
	for _, e := range listing.Entries {
		out = append(out, FileEntryView{
			Name:  string(e.Name),
			Size:  e.Size,
			Mode:  e.Mode,
			IsDir: e.IsDir(),
		})
	}
	return out, nil
}

// ReadFileTransferAck reads a length-prefixed FileTransferAck from the stream.
// Used by file push (after sending bytes + EOF) and file pull (before reading
// bytes).
func ReadFileTransferAck(st trsf.BidirectionalStream) (*protocol.FileTransferAck, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(streamReadAll{st}, lenBuf[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(lenBuf[:])
	body := make([]byte, n)
	if _, err := io.ReadFull(streamReadAll{st}, body); err != nil {
		return nil, err
	}
	ack := &protocol.FileTransferAck{}
	if _, err := ack.Decode(body); err != nil {
		return nil, err
	}
	return ack, nil
}

func openFileTransferStatusError(s protocol.OpenFileTransferStatus) error {
	switch s {
	case protocol.OpenFileTransferStatus_Ok:
		return nil
	case protocol.OpenFileTransferStatus_NoSuchTask:
		return errors.New("file: no such task (id unknown or task already finished)")
	case protocol.OpenFileTransferStatus_RunnerOffline:
		return errors.New("file: runner offline")
	default:
		return fmt.Errorf("file: server error (status=%d)", s)
	}
}

func listFilesStatusError(s protocol.ListFilesStatus) error {
	switch s {
	case protocol.ListFilesStatus_Ok:
		return nil
	case protocol.ListFilesStatus_NoSuchTask:
		return errors.New("file ls: no such task")
	case protocol.ListFilesStatus_RunnerOffline:
		return errors.New("file ls: runner offline")
	case protocol.ListFilesStatus_PathInvalid:
		return errors.New("file ls: path invalid")
	case protocol.ListFilesStatus_NotFound:
		return errors.New("file ls: not found")
	case protocol.ListFilesStatus_NotADirectory:
		return errors.New("file ls: not a directory")
	default:
		return fmt.Errorf("file ls: server error (status=%d)", s)
	}
}

// streamReadAll adapts a trsf.BidirectionalStream's recv side to io.Reader,
// translating EOF (eof flag from ReadDirect) into io.EOF.
type streamReadAll struct{ s trsf.BidirectionalStream }

func (r streamReadAll) Read(p []byte) (int, error) {
	data, eof, err := r.s.ReadDirect(uint64(len(p)))
	if err != nil {
		return 0, err
	}
	n := copy(p, data)
	if eof && n == len(data) {
		return n, io.EOF
	}
	return n, nil
}
