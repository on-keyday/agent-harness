package cli

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// fakeBidiStream is an in-memory trsf.BidirectionalStream: reads come from
// whatever feed() pushed, writes are recorded, and CloseBoth is observable.
// Enough to exercise the control-stream state machine without a server.
type fakeBidiStream struct {
	id      trsf.StreamID
	mu      sync.Mutex
	written []byte
	closed  bool
	recv    chan []byte
	done    chan struct{}
}

func newFakeBidiStream(id trsf.StreamID) *fakeBidiStream {
	return &fakeBidiStream{id: id, recv: make(chan []byte, 8), done: make(chan struct{})}
}

func (s *fakeBidiStream) feed(b []byte) { s.recv <- b }

func (s *fakeBidiStream) eof() {
	s.mu.Lock()
	defer s.mu.Unlock()
	select {
	case <-s.done:
	default:
		close(s.done)
	}
}

func (s *fakeBidiStream) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

func (s *fakeBidiStream) Written() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.written...)
}

func (s *fakeBidiStream) ID() trsf.StreamID { return s.id }

func (s *fakeBidiStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.written = append(s.written, p...)
	return len(p), nil
}

func (s *fakeBidiStream) WriteContext(_ context.Context, p []byte) (int, error) { return s.Write(p) }

func (s *fakeBidiStream) Close() error { return s.CloseBoth() }

func (s *fakeBidiStream) CloseBoth() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.eof()
	return nil
}

func (s *fakeBidiStream) HasSendData() bool { return false }
func (s *fakeBidiStream) Completed() bool   { return false }

func (s *fakeBidiStream) AppendData(_ bool, payloads ...[]byte) error {
	for _, p := range payloads {
		if _, err := s.Write(p); err != nil {
			return err
		}
	}
	return nil
}

func (s *fakeBidiStream) AppendDataContext(_ context.Context, eof bool, payloads ...[]byte) error {
	return s.AppendData(eof, payloads...)
}

func (s *fakeBidiStream) Read(p []byte) (int, error) {
	data, eof, err := s.ReadDirect(uint64(len(p)))
	if err != nil {
		return 0, err
	}
	if eof && len(data) == 0 {
		return 0, io.EOF
	}
	return copy(p, data), nil
}

func (s *fakeBidiStream) ReadContext(ctx context.Context, p []byte) (int, error) {
	data, eof, err := s.ReadDirectContext(ctx, uint64(len(p)))
	if err != nil {
		return 0, err
	}
	if eof && len(data) == 0 {
		return 0, io.EOF
	}
	return copy(p, data), nil
}

func (s *fakeBidiStream) ReadDirect(maxN uint64) ([]byte, bool, error) {
	return s.ReadDirectContext(context.Background(), maxN)
}

func (s *fakeBidiStream) ReadDirectContext(ctx context.Context, _ uint64) ([]byte, bool, error) {
	select {
	case b := <-s.recv:
		return b, false, nil
	case <-s.done:
		return nil, true, nil
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
}

func (s *fakeBidiStream) HasRecvData() bool { return len(s.recv) > 0 }
func (s *fakeBidiStream) EOF() bool         { return s.isClosed() }
func (s *fakeBidiStream) Cancel()           {}

func closedEventBytes(t *testing.T, reason protocol.PortForwardCloseReason) []byte {
	t.Helper()
	var ev protocol.PortForwardEvent
	ev.Kind = protocol.PortForwardEventKind_Closed
	ev.SetClosed(protocol.PortForwardClosed{Reason: reason})
	b, err := ev.Append(nil)
	if err != nil {
		t.Fatalf("encode closed event: %v", err)
	}
	return b
}

// TestRawConn_ControlClosedClosesData is what makes `forward kill` reach a raw
// connection: the server pushes a Closed record on the control stream, and the
// data stream — the pane's or the -W process's actual bytes — must go down with
// it rather than sit there relaying through a forward the server forgot.
func TestRawConn_ControlClosedClosesData(t *testing.T) {
	ctrl := newFakeBidiStream(11)
	data := newFakeBidiStream(12)
	rc := &RawConn{data: data, ctrl: ctrl, forwardID: 7}

	done := make(chan struct{})
	go func() {
		rc.watchControl(context.Background(), func(string) {})
		close(done)
	}()
	ctrl.feed(closedEventBytes(t, protocol.PortForwardCloseReason_Killed))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchControl did not return after a Closed record")
	}
	if !data.isClosed() {
		t.Fatal("data stream must be closed when the control stream reports Closed")
	}
	if !ctrl.isClosed() {
		t.Fatal("control stream must be closed on the way out")
	}
}

// TestRawConn_ControlEOFClosesData covers the other end of the same rule: the
// server connection dying is indistinguishable to us from a kill as far as the
// data stream's fate is concerned.
func TestRawConn_ControlEOFClosesData(t *testing.T) {
	ctrl := newFakeBidiStream(21)
	data := newFakeBidiStream(22)
	rc := &RawConn{data: data, ctrl: ctrl, forwardID: 8}

	var lines []string
	done := make(chan struct{})
	go func() {
		rc.watchControl(context.Background(), func(s string) { lines = append(lines, s) })
		close(done)
	}()
	ctrl.eof()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watchControl did not return on control EOF")
	}
	if !data.isClosed() {
		t.Fatal("data stream must be closed when the control stream EOFs")
	}
	if len(lines) == 0 {
		t.Fatal("an unexpected control EOF must be reported, not swallowed")
	}
}

// TestRawConn_SendCopiesCallerBuffer pins the AppendData contract: it stores the
// slice by reference and copies asynchronously, so a caller reusing its buffer
// (every pump does) must not be able to corrupt bytes already handed over.
func TestRawConn_SendCopiesCallerBuffer(t *testing.T) {
	data := newFakeBidiStream(31)
	rc := &RawConn{data: data, ctrl: newFakeBidiStream(32)}
	buf := []byte("ping")
	if err := rc.Send(buf); err != nil {
		t.Fatalf("send: %v", err)
	}
	copy(buf, "XXXX")
	if got := string(data.Written()); got != "ping" {
		t.Fatalf("Send must copy its argument; stream saw %q", got)
	}
}
