package server

import (
	"bytes"
	"errors"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// fakeRecv serves one chunk then EOF, and records a Cancel.
type fakeRecv struct {
	trsf.ReceiveStream
	data      []byte
	served    bool
	cancelled bool
}

func (f *fakeRecv) ReadDirect(uint64) ([]byte, bool, error) {
	if f.served {
		return nil, true, nil
	}
	f.served = true
	return f.data, false, nil
}
func (f *fakeRecv) Cancel() { f.cancelled = true }

// recvConn is a ConnHandle that only knows one receive stream.
type recvConn struct {
	ConnHandle
	st *fakeRecv
}

func (c recvConn) GetReceiveStream(trsf.StreamID) trsf.ReceiveStream { return c.st }

// A body that does not decode is bad_body — through the real read, not only
// the status mapping.
func TestReadExecBodyRefusesAMalformedBody(t *testing.T) {
	_, st := readExecBody(recvConn{st: &fakeRecv{data: []byte{0xff}}}, 1)
	if st != protocol.ExecRunStatus_BadBody {
		t.Fatalf("status = %v, want bad_body", st)
	}
}

// Past the cap the stream is CANCELLED, not drained, and the caller is told
// body_too_large.
func TestReadExecBodyRefusesAnOversizeBodyAndCancels(t *testing.T) {
	rs := &fakeRecv{data: bytes.Repeat([]byte{1}, execBodyMax+1)}
	_, st := readExecBody(recvConn{st: rs}, 1)
	if st != protocol.ExecRunStatus_BodyTooLarge || !rs.cancelled {
		t.Fatalf("status = %v cancelled = %v, want body_too_large and a cancel", st, rs.cancelled)
	}
}

// A refused request's body is cancelled, not left buffered on the server until
// the connection ends.
func TestDiscardPayloadStreamCancels(t *testing.T) {
	rs := &fakeRecv{}
	discardPayloadStream(recvConn{st: rs}, 1)
	if !rs.cancelled {
		t.Fatal("the refused request's payload stream was not cancelled")
	}
}

// Which requests carry a payload stream: the central denial cancels it.
func TestPayloadStreamOf(t *testing.T) {
	ex := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_OpenExecRun}
	ex.SetOpenExecRun(protocol.ExecRunRequest{PayloadStreamId: 9})
	if got := payloadStreamOf(ex); got != 9 {
		t.Errorf("exec: %d, want 9", got)
	}
	bs := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_BoardSend}
	bs.SetBoardSend(protocol.BoardSendRequest{PayloadStreamId: 7})
	if got := payloadStreamOf(bs); got != 7 {
		t.Errorf("board send: %d, want 7", got)
	}
	if got := payloadStreamOf(&protocol.TaskControlRequest{Kind: protocol.TaskControlKind_Whoami}); got != 0 {
		t.Errorf("whoami: %d, want 0", got)
	}
}

// fakeSend fails AppendData and records Close.
type fakeSend struct {
	trsf.SendStream
	closed bool
}

func (f *fakeSend) AppendData(bool, ...[]byte) error { return errors.New("broken") }
func (f *fakeSend) Close() error                     { f.closed = true; return nil }

// A body write that fails half-way still ends the stream, so the runner's read
// ends and it reports the exec failed instead of waiting forever.
func TestWriteRunnerExecBodyClosesOnFailure(t *testing.T) {
	fs := &fakeSend{}
	if err := writeRunnerExecBody(fs, []byte("x")); err == nil || !fs.closed {
		t.Fatalf("err = %v closed = %v, want an error and the stream closed", err, fs.closed)
	}
}
