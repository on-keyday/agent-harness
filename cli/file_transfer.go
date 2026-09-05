package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"sync"

	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
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

// FileTransferRange selects a byte range of a pull. The zero value is the whole
// file, so a caller that does not care passes FileTransferRange{} and keeps
// exactly the behaviour it had before ranges existed. Only pull honours it: the
// runner answers range_invalid for any other direction rather than discarding
// the part of the request the caller cared about.
type FileTransferRange struct {
	Offset uint64
	Length uint64 // 0 = to EOF
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
	rng FileTransferRange,
	force bool,
	mkdirParents bool,
	noDataPlane bool,
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
		Offset:       rng.Offset,
		Length:       rng.Length,
	}
	body.SetRelPath([]byte(relPath))
	body.SetForce(force)
	body.SetMkdirParents(mkdirParents)
	body.SetNoDataPlane(noDataPlane)
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

	// The server routed this end to end: dial the slot it allocated, redeem the
	// grant, and carry the bytes on a connection it cannot read. A zero grant
	// means it spliced instead, and the stream below is the one it allocated.
	target := dataPlaneTarget{GrantID: r.GrantId, TaskID: tid, SlotID: r.SlotId}
	if target.use() {
		st, closer, err := c.openDataPlaneStream(ctx, target, func(streamID uint64) protocol.RunnerRequest {
			rr := protocol.RunnerRequest{Kind: protocol.RunnerRequestType_OpenFileTransfer}
			b := protocol.RunnerOpenFileTransferRequest{
				TaskId:       tid,
				StreamId:     streamID,
				Direction:    direction,
				ExpectedSize: expectedSize,
				Offset:       rng.Offset,
				Length:       rng.Length,
			}
			b.SetRelPath([]byte(relPath))
			b.SetForce(force)
			b.SetMkdirParents(mkdirParents)
			rr.SetOpenFileTransfer(b)
			return rr
		})
		if err != nil {
			return nil, err
		}
		return &dataPlaneStream{BidirectionalStream: st, closeConn: closer}, nil
	}

	st := peer.WaitForBidirectionalStream(ctx, c.Transport(), trsf.StreamID(r.StreamId))
	if st == nil {
		return nil, fmt.Errorf("file: stream %d not visible", r.StreamId)
	}
	return st, nil
}

// dataPlaneStream ties the connection's lifetime to the stream's, so the seven
// call sites that already `defer stream.CloseBoth()` need no change: the
// connection exists only to carry this one request.
type dataPlaneStream struct {
	trsf.BidirectionalStream
	closeConn func()
	once      sync.Once
}

func (s *dataPlaneStream) CloseBoth() error {
	err := s.BidirectionalStream.CloseBoth()
	s.once.Do(s.closeConn)
	return err
}

// ListFiles round-trips a list_files request and decodes the FileListing
// payload. Returns the entries in name order.
func (c *Client) ListFiles(ctx context.Context, taskIDHex, relPath string, noDataPlane bool) ([]FileEntryView, error) {
	tid, err := parseTaskIDHex(taskIDHex)
	if err != nil {
		return nil, fmt.Errorf("file ls: parse task id: %w", err)
	}
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_ListFiles}
	body := protocol.ListFilesRequest{TaskId: tid}
	body.SetRelPath([]byte(relPath))
	body.SetNoDataPlane(noDataPlane)
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

	var st trsf.BidirectionalStream
	target := dataPlaneTarget{GrantID: r.GrantId, TaskID: tid, SlotID: r.SlotId}
	if target.use() {
		s2, closer, err := c.openDataPlaneStream(ctx, target, func(streamID uint64) protocol.RunnerRequest {
			rr := protocol.RunnerRequest{Kind: protocol.RunnerRequestType_ListFiles}
			b := protocol.RunnerListFilesRequest{TaskId: tid, StreamId: streamID}
			b.SetRelPath([]byte(relPath))
			rr.SetListFiles(b)
			return rr
		})
		if err != nil {
			return nil, err
		}
		defer closer()
		st = s2
	} else {
		st = peer.WaitForBidirectionalStream(ctx, c.Transport(), trsf.StreamID(r.StreamId))
	}
	if st == nil {
		return nil, fmt.Errorf("file ls: stream %d not visible", r.StreamId)
	}
	defer st.CloseBoth()
	if err := st.AppendData(true); err != nil {
		return nil, fmt.Errorf("file ls: half-close: %w", err)
	}
	body2, err := io.ReadAll(st)
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

// ReadFileTransferAck reads a fixed-size FileTransferAck (protocol.FileTransferAckSize
// bytes) from the stream. Used by file push (after sending bytes + EOF) and
// file pull (before reading bytes).
func ReadFileTransferAck(st trsf.BidirectionalStream) (*protocol.FileTransferAck, error) {
	body := make([]byte, protocol.FileTransferAckSize)
	if _, err := io.ReadFull(st, body); err != nil {
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
