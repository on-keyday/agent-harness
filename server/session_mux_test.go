package server

import (
	"bytes"
	"context"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/trsf"
)

var _ trsf.BidirectionalStream = (*fakeBidiStream)(nil)

// fakeBidiStream is a test double for trsf.BidirectionalStream.
// It supports queuing bytes for ReadDirect, capturing writes, and
// simulating client-side EOF via CloseRead.
type fakeBidiStream struct {
	t        *testing.T
	streamID trsf.StreamID

	mu       sync.Mutex
	readQ    [][]byte // queued chunks for ReadDirect
	readCond *sync.Cond
	readEOF  bool // set by CloseRead

	writeMu sync.Mutex
	written []byte

	closed atomic.Bool
}

func newFakeStream(t *testing.T) *fakeBidiStream {
	t.Helper()
	f := &fakeBidiStream{
		t: t,
	}
	f.readCond = sync.NewCond(&f.mu)
	return f
}

// QueueRead enqueues a chunk that will be returned by the next ReadDirect call.
func (f *fakeBidiStream) QueueRead(data []byte) {
	f.mu.Lock()
	f.readQ = append(f.readQ, append([]byte{}, data...))
	f.readCond.Signal()
	f.mu.Unlock()
}

// CloseRead causes subsequent ReadDirect calls to return EOF.
func (f *fakeBidiStream) CloseRead() {
	f.mu.Lock()
	f.readEOF = true
	f.readCond.Broadcast()
	f.mu.Unlock()
}

// IsClosed reports whether CloseBoth has been called.
func (f *fakeBidiStream) IsClosed() bool {
	return f.closed.Load()
}

// WaitWritten blocks until at least n bytes have been written to this stream,
// then returns all written bytes so far.
func (f *fakeBidiStream) WaitWritten(t *testing.T, n int) []byte {
	t.Helper()
	deadline := time.Now().Add(1500 * time.Millisecond)
	for {
		f.writeMu.Lock()
		got := len(f.written)
		f.writeMu.Unlock()
		if got >= n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("WaitWritten: timeout waiting for %d bytes, got %d", n, got)
		}
		time.Sleep(5 * time.Millisecond)
	}
	f.writeMu.Lock()
	out := append([]byte{}, f.written...)
	f.writeMu.Unlock()
	return out
}

// --- trsf.BidirectionalStream implementation ---

func (f *fakeBidiStream) ID() trsf.StreamID { return f.streamID }

// Write captures bytes written to this stream (runner→tui direction).
func (f *fakeBidiStream) Write(p []byte) (int, error) {
	if f.closed.Load() {
		return 0, io.ErrClosedPipe
	}
	f.writeMu.Lock()
	f.written = append(f.written, p...)
	f.writeMu.Unlock()
	return len(p), nil
}

func (f *fakeBidiStream) Close() error { return nil }

func (f *fakeBidiStream) WriteContext(_ context.Context, p []byte) (int, error) {
	return f.Write(p)
}

func (f *fakeBidiStream) HasSendData() bool { return false }
func (f *fakeBidiStream) Completed() bool   { return false }

func (f *fakeBidiStream) AppendData(eof bool, data ...[]byte) error {
	for _, d := range data {
		if _, err := f.Write(d); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeBidiStream) AppendDataContext(_ context.Context, eof bool, data ...[]byte) error {
	return f.AppendData(eof, data...)
}

// ReadDirect blocks until a chunk is queued or CloseRead is called.
// It returns (data, eof, err) matching the trsf.ReceiveStream contract.
func (f *fakeBidiStream) ReadDirect(maxN uint64) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for len(f.readQ) == 0 && !f.readEOF && !f.closed.Load() {
		f.readCond.Wait()
	}
	if f.closed.Load() && len(f.readQ) == 0 {
		return nil, true, nil
	}
	if len(f.readQ) == 0 {
		// readEOF set
		return nil, true, nil
	}
	chunk := f.readQ[0]
	f.readQ = f.readQ[1:]
	eof := f.readEOF && len(f.readQ) == 0
	return chunk, eof, nil
}

func (f *fakeBidiStream) ReadDirectContext(_ context.Context, maxN uint64) ([]byte, bool, error) {
	return f.ReadDirect(maxN)
}

func (f *fakeBidiStream) Read(p []byte) (int, error) {
	data, _, err := f.ReadDirect(uint64(len(p)))
	if err != nil {
		return 0, err
	}
	if len(data) == 0 {
		return 0, io.EOF
	}
	n := copy(p, data)
	return n, nil
}

func (f *fakeBidiStream) ReadContext(_ context.Context, p []byte) (int, error) {
	return f.Read(p)
}

func (f *fakeBidiStream) HasRecvData() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.readQ) > 0
}

func (f *fakeBidiStream) EOF() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.readEOF && len(f.readQ) == 0
}

func (f *fakeBidiStream) Cancel() { f.CloseRead() }

func (f *fakeBidiStream) CloseBoth() error {
	f.closed.Store(true)
	f.mu.Lock()
	f.readEOF = true
	f.readCond.Broadcast()
	f.mu.Unlock()
	return nil
}

// waitFor polls cond until it returns true or 1s passes, failing the test on timeout.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(1 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("waitFor: condition not met within 1s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// --- Tests ---

func TestSessionMux_AttachReplaysRingBuffer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	runnerStream := newFakeStream(t)
	mux := NewSessionMux(ctx, "task-abc", runnerStream, NewRingBuffer(32), SessionHooks{})

	runnerStream.QueueRead([]byte("preattach payload"))
	waitFor(t, func() bool { return mux.RingBufferLen() == len("preattach payload") })

	tui := newFakeStream(t)
	if err := mux.Attach(ctx, tui); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	got := tui.WaitWritten(t, len("preattach payload"))
	if !bytes.Equal(got, []byte("preattach payload")) {
		t.Fatalf("replay got %q want %q", got, "preattach payload")
	}
}

func TestSessionMux_DetachKeepsRunnerStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	runnerStream := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runnerStream, NewRingBuffer(64), SessionHooks{})

	tui := newFakeStream(t)
	if err := mux.Attach(ctx, tui); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	tui.CloseRead()
	waitFor(t, func() bool { return !mux.IsAttached() })

	if runnerStream.IsClosed() {
		t.Fatal("runnerStream must NOT be closed on client detach")
	}

	runnerStream.QueueRead([]byte("post-detach"))
	waitFor(t, func() bool { return mux.RingBufferLen() == len("post-detach") })
}

func TestSessionMux_AttachTakeover(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(32), SessionHooks{})

	first := newFakeStream(t)
	if err := mux.Attach(ctx, first); err != nil {
		t.Fatalf("first Attach: %v", err)
	}

	second := newFakeStream(t)
	if err := mux.Attach(ctx, second); err != nil {
		t.Fatalf("second Attach: %v", err)
	}

	if !first.IsClosed() {
		t.Fatal("first tuiStream must be closed after takeover")
	}
	if !mux.IsAttached() {
		t.Fatal("mux must be attached to second")
	}
}
