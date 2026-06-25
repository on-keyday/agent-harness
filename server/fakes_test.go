package server

import (
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/trsf"
)

type fakeConn struct {
	id     objproto.ConnectionID
	sentMu sync.Mutex
	sent   [][]byte

	// nextStreamID is the StreamID to assign to the next CreateBidirectionalStream
	// result. When zero, CreateBidirectionalStream returns nil (legacy behaviour).
	nextStreamID trsf.StreamID
	// nextBidi, when non-nil, is returned by the next CreateBidirectionalStream
	// call (and then cleared) instead of constructing a noopBidiStream. Lets a
	// test inject a recording/blocking control stream.
	nextBidi trsf.BidirectionalStream
	// bidiByID is consulted by GetBidirectionalStream to resolve a peer-created
	// stream by id (e.g. a runner-created remote-forward data stream).
	bidiByID map[trsf.StreamID]trsf.BidirectionalStream
	// bidiStreams collects every non-nil stream returned, so tests can assert
	// that they were torn down via CloseBoth.
	bidiStreams []*noopBidiStream

	// nextSendStreamID, when non-zero, is used as the stream ID for the next
	// CreateSendStream call (then cleared). When zero, an auto-incrementing
	// counter allocates the ID so tests need not set it explicitly.
	nextSendStreamID trsf.StreamID
	// autoStreamCounter is incremented each time CreateSendStream allocates
	// a stream ID automatically (i.e., nextSendStreamID is zero).
	autoStreamCounter atomic.Uint64
	// sendStreams collects every non-nil send stream so tests can assert
	// the streamed body was written + EOF'd.
	sendStreams []*recordingSendStream
}

func (f *fakeConn) ConnectionID() objproto.ConnectionID { return f.id }
func (f *fakeConn) SendMessage(b []byte) (int, uint64, error) {
	f.sentMu.Lock()
	f.sent = append(f.sent, append([]byte{}, b...))
	f.sentMu.Unlock()
	return len(b), 0, nil
}

// Sent returns a snapshot of the sent messages, safe for concurrent access.
func (f *fakeConn) Sent() [][]byte {
	f.sentMu.Lock()
	defer f.sentMu.Unlock()
	out := make([][]byte, len(f.sent))
	copy(out, f.sent)
	return out
}

// CreateSendStream returns a recordingSendStream whose bytes are captured for
// test assertions. When nextSendStreamID is set it is used as the stream ID
// (then cleared); otherwise an auto-incrementing counter allocates one so
// callers need not set nextSendStreamID explicitly.
func (f *fakeConn) CreateSendStream() trsf.SendStream {
	id := f.nextSendStreamID
	if id == 0 {
		id = trsf.StreamID(f.autoStreamCounter.Add(1))
	} else {
		f.nextSendStreamID = 0
	}
	s := &recordingSendStream{streamID: id, done: make(chan struct{})}
	f.sendStreams = append(f.sendStreams, s)
	return s
}

// sendStreamBytes waits for the send-stream with the given id to be closed
// (signalling all payload writes are complete) then returns the captured bytes.
// Fails the test if the stream is not found.
func (f *fakeConn) sendStreamBytes(t *testing.T, streamID uint64) []byte {
	t.Helper()
	sid := trsf.StreamID(streamID)
	for _, s := range f.sendStreams {
		if s.streamID == sid {
			<-s.done // wait for goroutine to call Close()
			return s.bytes
		}
	}
	t.Fatalf("sendStreamBytes: stream id %d not found in sendStreams (len=%d)", streamID, len(f.sendStreams))
	return nil
}

// lastTaskControlResponse decodes the last message sent on this conn as a
// TaskControlResponse (stripping the leading AppKind byte). Returns a pointer
// so callers can chain pointer-receiver methods (e.g. .BoardRead()) directly
// on the result. Both this method form and the package-level
// lastTaskControlResponse(t, conn) helper coexist; existing callers are unaffected.
func (f *fakeConn) lastTaskControlResponse(t *testing.T) *protocol.TaskControlResponse {
	t.Helper()
	resp := lastTaskControlResponse(t, f)
	return &resp
}

// recordingSendStream captures AppendData calls so tests can decode and
// assert on the streamed body. done is closed by Close() to let
// sendStreamBytes wait for the goroutine to finish writing.
type recordingSendStream struct {
	streamID  trsf.StreamID
	bytes     []byte
	eofSent   bool
	done      chan struct{}
	closeOnce sync.Once
}

func (s *recordingSendStream) ID() trsf.StreamID { return s.streamID }
func (s *recordingSendStream) Write(p []byte) (int, error) {
	s.bytes = append(s.bytes, p...)
	return len(p), nil
}
func (s *recordingSendStream) WriteContext(_ context.Context, p []byte) (int, error) {
	return s.Write(p)
}
func (s *recordingSendStream) Close() error {
	s.eofSent = true
	if s.done != nil {
		s.closeOnce.Do(func() { close(s.done) })
	}
	return nil
}
func (s *recordingSendStream) Cancel() {}
func (s *recordingSendStream) HasSendData() bool { return len(s.bytes) > 0 }
func (s *recordingSendStream) Completed() bool   { return s.eofSent }
func (s *recordingSendStream) AppendData(eof bool, payloads ...[]byte) error {
	for _, p := range payloads {
		s.bytes = append(s.bytes, p...)
	}
	if eof {
		s.eofSent = true
	}
	return nil
}
func (s *recordingSendStream) AppendDataContext(_ context.Context, eof bool, payloads ...[]byte) error {
	return s.AppendData(eof, payloads...)
}

// CreateBidirectionalStream returns a noopBidiStream when nextStreamID is set,
// otherwise nil. Tests that exercise OpenInteractive's Ok path set nextStreamID
// before invoking the handler so the splice has a non-nil stream to operate on.
func (f *fakeConn) CreateBidirectionalStream() trsf.BidirectionalStream {
	if f.nextBidi != nil {
		s := f.nextBidi
		f.nextBidi = nil
		return s
	}
	if f.nextStreamID == 0 {
		return nil
	}
	s := &noopBidiStream{streamID: f.nextStreamID}
	f.bidiStreams = append(f.bidiStreams, s)
	f.nextStreamID = 0
	return s
}

// GetReceiveStream returns nil; tests that need a non-nil receive stream
// (agentboard payload-stream paths) wire a different stub.
func (f *fakeConn) GetReceiveStream(_ trsf.StreamID) trsf.ReceiveStream { return nil }

// GetBidirectionalStream resolves a peer-created stream by id from bidiByID
// (nil if absent), mirroring how the real conn looks up a runner-created
// remote-forward data stream.
func (f *fakeConn) GetBidirectionalStream(id trsf.StreamID) trsf.BidirectionalStream {
	return f.bidiByID[id]
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
