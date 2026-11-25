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

// Which requests carry payload streams: the central denial cancels them.
func TestPayloadStreamsOf(t *testing.T) {
	one := func(what string, req *protocol.TaskControlRequest, want ...uint64) {
		t.Helper()
		got := payloadStreamsOf(req)
		if len(got) != len(want) {
			t.Errorf("%s: %v, want %v", what, got, want)
			return
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%s: %v, want %v", what, got, want)
			}
		}
	}
	ex := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_OpenExecRun}}
	ex.SetOpenExecRun(protocol.ExecRunRequest{PayloadStreamId: 9})
	one("exec", ex, 9)
	bs := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_BoardSend}}
	bs.SetBoardSend(protocol.BoardSendRequest{PayloadStreamId: 7})
	one("board send", bs, 7)
	one("whoami", &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_Whoami}})

	// The grant-writing requests' grant-conditions streams (grant conditions C6).
	oi := protocol.OpenInteractiveRequest{}
	oi.SetPromptPresent(true)
	oi.SetPromptStreamId(3)
	var ext protocol.OpenInteractiveExt
	ext.SetGrantConditionsPresent(true)
	ext.SetGrantConditionsStreamId(4)
	oi.SetExtPresent(true)
	oi.SetExt(ext)
	op := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_OpenInteractive}}
	op.SetOpenInteractive(oi)
	one("open", op, 3, 4)
	sc := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_SetCaps}}
	sc.SetSetCaps(protocol.SetCapsRequest{GrantConditionsStreamId: 5})
	one("set_caps", sc, 5)
	ci := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_CredentialIssue}}
	ci.SetCredentialIssue(protocol.CredentialIssueRequest{GrantConditionsStreamId: 6})
	one("credential issue", ci, 6)
	cr := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_CredentialRekey}}
	cr.SetCredentialRekey(protocol.CredentialRekeyRequest{GrantConditionsStreamId: 7})
	one("credential rekey", cr, 7)
	spec := protocol.ScheduleSpec{GrantConditionsStreamId: 9}
	spec.SetPromptPresent(true)
	spec.SetPromptStreamId(8)
	se := &protocol.TaskControlRequest{Header: protocol.TaskControlRequestHeader{Kind: protocol.TaskControlKind_ScheduleEdit}}
	se.SetScheduleEdit(protocol.ScheduleEditRequest{Spec: spec})
	one("schedule edit", se, 8, 9)
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
