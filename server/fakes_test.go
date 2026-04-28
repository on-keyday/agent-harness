package server

import (
	"context"
	"io"
	"sync/atomic"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/trsf"
)

type fakeConn struct {
	id   objproto.ConnectionID
	sent [][]byte

	// nextStreamID is the StreamID to assign to the next CreateBidirectionalStream
	// result. When zero, CreateBidirectionalStream returns nil (legacy behaviour).
	nextStreamID trsf.StreamID
	// bidiStreams collects every non-nil stream returned, so tests can assert
	// that they were torn down via CloseBoth.
	bidiStreams []*noopBidiStream
}

func (f *fakeConn) ConnectionID() objproto.ConnectionID { return f.id }
func (f *fakeConn) SendMessage(b []byte) (int, uint64, error) {
	f.sent = append(f.sent, append([]byte{}, b...))
	return len(b), 0, nil
}

// CreateSendStream returns nil; tests that rely on streamed responses
// (GetTaskLog) wire a real connection or skip the assertion.
func (f *fakeConn) CreateSendStream() trsf.SendStream { return nil }

// CreateBidirectionalStream returns a noopBidiStream when nextStreamID is set,
// otherwise nil. Tests that exercise OpenInteractive's Ok path set nextStreamID
// before invoking the handler so the splice has a non-nil stream to operate on.
func (f *fakeConn) CreateBidirectionalStream() trsf.BidirectionalStream {
	if f.nextStreamID == 0 {
		return nil
	}
	s := &noopBidiStream{streamID: f.nextStreamID}
	f.bidiStreams = append(f.bidiStreams, s)
	f.nextStreamID = 0
	return s
}

// noopBidiStream is a minimal trsf.BidirectionalStream stub. Reads return EOF
// immediately, writes are dropped, and CloseBoth flips a flag so tests can
// assert teardown happened. Sufficient for unit tests that drive the splice
// path without exchanging real bytes.
type noopBidiStream struct {
	streamID trsf.StreamID
	closed   atomic.Bool
}

func (s *noopBidiStream) ID() trsf.StreamID                       { return s.streamID }
func (s *noopBidiStream) Write(p []byte) (int, error)             { return len(p), nil }
func (s *noopBidiStream) Close() error                            { return nil }
func (s *noopBidiStream) WriteContext(_ context.Context, p []byte) (int, error) {
	return len(p), nil
}
func (s *noopBidiStream) HasSendData() bool                       { return false }
func (s *noopBidiStream) Completed() bool                         { return true }
func (s *noopBidiStream) AppendData(_ bool, _ ...[]byte) error    { return nil }
func (s *noopBidiStream) AppendDataContext(_ context.Context, _ bool, _ ...[]byte) error {
	return nil
}
func (s *noopBidiStream) Read([]byte) (int, error)                            { return 0, io.EOF }
func (s *noopBidiStream) ReadContext(_ context.Context, _ []byte) (int, error) { return 0, io.EOF }
func (s *noopBidiStream) ReadDirect(_ uint64) ([]byte, bool, error)           { return nil, true, nil }
func (s *noopBidiStream) ReadDirectContext(_ context.Context, _ uint64) ([]byte, bool, error) {
	return nil, true, nil
}
func (s *noopBidiStream) HasRecvData() bool { return false }
func (s *noopBidiStream) EOF() bool         { return true }
func (s *noopBidiStream) Cancel()           {}
func (s *noopBidiStream) CloseBoth() error  { s.closed.Store(true); return nil }
