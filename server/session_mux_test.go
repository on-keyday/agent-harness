package server

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/on-keyday/objtrsf/trsf"
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

	blockWrites atomic.Bool // when true, Write spins until cleared or closed
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

// SetBlockWrites makes Write block (spin) until cleared or the stream is closed.
// Used to simulate a viewer whose client cannot keep up.
func (f *fakeBidiStream) SetBlockWrites(b bool) { f.blockWrites.Store(b) }

// Written returns a snapshot copy of all bytes written so far (non-blocking).
func (f *fakeBidiStream) Written() []byte {
	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	return append([]byte{}, f.written...)
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
	for f.blockWrites.Load() && !f.closed.Load() {
		time.Sleep(time.Millisecond)
	}
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
// It returns (data, eof, err) matching the trsf.ReceiveStream contract:
// at most maxN bytes are returned, with any leftover left at the head of
// the queue so a subsequent Read picks it up. Without this, an io.ReadFull
// asking for 5 bytes against a 30-byte queued chunk would silently
// discard the trailing 25 bytes.
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
	if uint64(len(chunk)) > maxN {
		out := chunk[:maxN]
		f.readQ[0] = chunk[maxN:]
		return out, false, nil
	}
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

// makeWireFrame builds a wire-encoded exec/frame frame: 1 byte Type + 4
// byte big-endian Len + payload. Type value is opaque from the server's
// perspective; SessionMux only cares about boundaries.
func makeWireFrame(typ byte, payload []byte) []byte {
	out := make([]byte, frameHeaderSize+len(payload))
	out[0] = typ
	binary.BigEndian.PutUint32(out[1:5], uint32(len(payload)))
	copy(out[frameHeaderSize:], payload)
	return out
}

func TestSessionMux_AttachReplaysRingBuffer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	runnerStream := newFakeStream(t)
	mux := NewSessionMux(ctx, "task-abc", runnerStream, NewRingBuffer(64), SessionHooks{})

	wire := makeWireFrame(1, []byte("preattach payload"))
	runnerStream.QueueRead(wire)
	waitFor(t, func() bool { return mux.RingBufferLen() == len(wire) })

	tui := newFakeStream(t)
	if err := mux.Attach(ctx, tui); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	got := tui.WaitWritten(t, len(wire))
	if !bytes.Equal(got, wire) {
		t.Fatalf("replay got %q want %q", got, wire)
	}
}

func TestSessionMux_DetachKeepsRunnerStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	runnerStream := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runnerStream, NewRingBuffer(128), SessionHooks{})

	tui := newFakeStream(t)
	if err := mux.Attach(ctx, tui); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	tui.CloseRead()
	waitFor(t, func() bool { return !mux.IsAttached() })

	if runnerStream.IsClosed() {
		t.Fatal("runnerStream must NOT be closed on client detach")
	}

	wire := makeWireFrame(1, []byte("post-detach"))
	runnerStream.QueueRead(wire)
	waitFor(t, func() bool { return mux.RingBufferLen() == len(wire) })
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

func TestSessionMux_AttachViewer_ReplaysThenStreams(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})

	pre := makeWireFrame(1, []byte("scrollback"))
	runner.QueueRead(pre)
	waitFor(t, func() bool { return mux.RingBufferLen() == len(pre) })

	viewer := newFakeStream(t)
	if err := mux.AttachViewer(ctx, viewer); err != nil {
		t.Fatalf("AttachViewer: %v", err)
	}
	if got := viewer.WaitWritten(t, len(pre)); !bytes.Equal(got, pre) {
		t.Fatalf("viewer replay got %q want %q", got, pre)
	}
	if mux.IsAttached() {
		t.Fatal("AttachViewer must NOT occupy the writer slot")
	}

	live := makeWireFrame(1, []byte("live"))
	runner.QueueRead(live)
	want := append(append([]byte{}, pre...), live...)
	if got := viewer.WaitWritten(t, len(want)); !bytes.Equal(got, want) {
		t.Fatalf("viewer live got %q want %q", got, want)
	}
}

func TestSessionMux_FanOutWriterAndViewers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})

	writer := newFakeStream(t)
	if err := mux.Attach(ctx, writer); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	v1 := newFakeStream(t)
	if err := mux.AttachViewer(ctx, v1); err != nil {
		t.Fatalf("v1: %v", err)
	}
	v2 := newFakeStream(t)
	if err := mux.AttachViewer(ctx, v2); err != nil {
		t.Fatalf("v2: %v", err)
	}

	fr := makeWireFrame(1, []byte("broadcast"))
	runner.QueueRead(fr)
	for name, s := range map[string]*fakeBidiStream{"writer": writer, "v1": v1, "v2": v2} {
		if got := s.WaitWritten(t, len(fr)); !bytes.Equal(got, fr) {
			t.Fatalf("%s got %q want %q", name, got, fr)
		}
	}
}

func TestSessionMux_SlowViewerDroppedWithoutWedge(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(1<<20), SessionHooks{})

	writer := newFakeStream(t)
	if err := mux.Attach(ctx, writer); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	slow := newFakeStream(t)
	slow.SetBlockWrites(true) // its output pump blocks on the first frame
	if err := mux.AttachViewer(ctx, slow); err != nil {
		t.Fatalf("AttachViewer: %v", err)
	}

	const n = viewerQueueDepth + 50
	var want []byte
	for i := 0; i < n; i++ {
		fr := makeWireFrame(1, []byte{byte(i)})
		want = append(want, fr...)
		runner.QueueRead(fr)
	}
	if got := writer.WaitWritten(t, len(want)); !bytes.Equal(got, want) {
		t.Fatalf("writer missing frames — pump wedged on slow viewer?")
	}
	waitFor(t, func() bool { return mux.ViewerCount() == 0 })
	if !slow.IsClosed() {
		t.Fatal("dropped viewer stream must be CloseBoth'd")
	}
}

func TestSessionMux_ViewerInputDiscarded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})

	viewer := newFakeStream(t)
	if err := mux.AttachViewer(ctx, viewer); err != nil {
		t.Fatalf("AttachViewer: %v", err)
	}
	viewer.QueueRead([]byte("rm -rf / # should never reach runner\n"))
	waitFor(t, func() bool { return !viewer.HasRecvData() })
	time.Sleep(50 * time.Millisecond)
	if w := runner.Written(); len(w) != 0 {
		t.Fatalf("viewer input was forwarded to runner: %q", w)
	}
}

func TestSessionMux_ViewerDoesNotFireOnAttach(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var attaches int32
	hooks := SessionHooks{OnAttach: func(string) { atomic.AddInt32(&attaches, 1) }}
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), hooks)

	v := newFakeStream(t)
	if err := mux.AttachViewer(ctx, v); err != nil {
		t.Fatalf("AttachViewer: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if n := atomic.LoadInt32(&attaches); n != 0 {
		t.Fatalf("onAttach fired %d times for a viewer; want 0", n)
	}
	w := newFakeStream(t)
	if err := mux.Attach(ctx, w); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	waitFor(t, func() bool { return atomic.LoadInt32(&attaches) == 1 })
}

func TestSessionMux_StopClosesViewers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})

	v := newFakeStream(t)
	if err := mux.AttachViewer(ctx, v); err != nil {
		t.Fatalf("AttachViewer: %v", err)
	}
	mux.Stop()
	waitFor(t, func() bool { return v.IsClosed() })
	waitFor(t, func() bool { return mux.ViewerCount() == 0 })
}

// A control takeover (second Attach) must not disturb existing viewers: the
// old writer is closed, but viewers keep their slot and keep streaming.
func TestSessionMux_TakeoverLeavesViewersStreaming(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})

	w1 := newFakeStream(t)
	if err := mux.Attach(ctx, w1); err != nil {
		t.Fatalf("w1 Attach: %v", err)
	}
	viewer := newFakeStream(t)
	if err := mux.AttachViewer(ctx, viewer); err != nil {
		t.Fatalf("AttachViewer: %v", err)
	}

	// Second control attach takes over the writer slot.
	w2 := newFakeStream(t)
	if err := mux.Attach(ctx, w2); err != nil {
		t.Fatalf("w2 Attach: %v", err)
	}
	if !w1.IsClosed() {
		t.Fatal("first writer must be closed by takeover")
	}
	if mux.ViewerCount() != 1 {
		t.Fatalf("ViewerCount=%d want 1 (takeover must not drop viewers)", mux.ViewerCount())
	}

	// A live frame after the takeover still reaches the viewer.
	fr := makeWireFrame(1, []byte("after-takeover"))
	runner.QueueRead(fr)
	if got := viewer.WaitWritten(t, len(fr)); !bytes.Equal(got, fr) {
		t.Fatalf("viewer stopped streaming after writer takeover: got %q want %q", got, fr)
	}
}
