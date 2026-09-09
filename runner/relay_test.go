package runner

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/on-keyday/objtrsf/trsf"
)

// fakeFar is the minimum of a far stream the relay needs: a byte sink and a
// byte source that can be made to fail.
type fakeFar struct {
	trsfStub
	in      chan []byte
	written [][]byte
	fail    bool
}

func newFakeFar() *fakeFar { return &fakeFar{in: make(chan []byte, 8)} }

func (f *fakeFar) Read(p []byte) (int, error) {
	if f.fail {
		return 0, io.EOF
	}
	b, ok := <-f.in
	if !ok {
		return 0, io.EOF
	}
	return copy(p, b), nil
}

func (f *fakeFar) Write(p []byte) (int, error) {
	if f.fail {
		return 0, io.ErrClosedPipe
	}
	f.written = append(f.written, append([]byte(nil), p...))
	return len(p), nil
}

// The behaviour the whole design rests on: with the far side gone, a Write
// PARKS and its bytes are retained rather than dropped, and the first write
// after a rebind carries them.
func TestRelayRetainsTheChunkItCouldNotForward(t *testing.T) {
	far := newFakeFar()
	r := newSessionRelay(far, func() bool { return true })
	r.detach()

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := r.Write([]byte("during-the-gap")); err != nil {
			t.Errorf("write during a gap: %v", err)
		}
	}()

	select {
	case <-done:
		t.Fatal("Write returned while there was no far stream — the bytes were dropped")
	case <-time.After(50 * time.Millisecond):
	}

	next := newFakeFar()
	r.rebind(next)
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Write never unparked after the rebind")
	}
	if _, err := r.Write([]byte("after")); err != nil {
		t.Fatalf("write after rebind: %v", err)
	}

	var got string
	for _, w := range next.written {
		got += string(w)
	}
	if got != "during-the-gapafter" {
		t.Errorf("far stream saw %q, want the gap chunk first then the new bytes", got)
	}
}

// EOF is what agentexec's reaper ladder watches. While the relay is merely
// detached it must NOT report EOF, or the child is killed because a server
// went away — the exact failure the relay exists to prevent.
func TestRelayDoesNotReportEOFWhileMerelyDetached(t *testing.T) {
	r := newSessionRelay(newFakeFar(), func() bool { return true })
	r.detach()
	if r.EOF() {
		t.Error("EOF() is true with no far stream — the reaper ladder would fire")
	}
	if r.Completed() {
		t.Error("Completed() is true with no far stream")
	}
	r.close()
	if !r.EOF() {
		t.Error("EOF() is false after close — the session can never end")
	}
}

// A Read with no far stream parks instead of returning EOF, for the same
// reason.
func TestRelayReadParksInsteadOfEndingTheSession(t *testing.T) {
	far := newFakeFar()
	r := newSessionRelay(far, func() bool { return true })
	r.detach()

	type res struct {
		n   int
		err error
	}
	out := make(chan res, 1)
	go func() {
		buf := make([]byte, 8)
		n, err := r.Read(buf)
		out <- res{n, err}
	}()
	select {
	case got := <-out:
		t.Fatalf("Read returned (%d, %v) during a gap, want it to park", got.n, got.err)
	case <-time.After(50 * time.Millisecond):
	}

	next := newFakeFar()
	next.in <- []byte("hi")
	r.rebind(next)
	select {
	case got := <-out:
		if got.err != nil || got.n != 2 {
			t.Errorf("after rebind Read = (%d, %v), want (2, nil)", got.n, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("Read never unparked after the rebind")
	}
}

// CloseBoth must not reach the far stream: agentexec defers it, and forwarding
// it would close the PTY master and hang up the child's session, since the
// child is a session leader with that PTY as its controlling terminal.
func TestRelayDoesNotForwardCloseBoth(t *testing.T) {
	far := newFakeFar()
	r := newSessionRelay(far, func() bool { return true })
	if err := r.CloseBoth(); err != nil {
		t.Fatalf("CloseBoth: %v", err)
	}
	if far.closedBoth {
		t.Error("CloseBoth reached the far stream — the PTY master would be closed")
	}
	r.close()
	if !far.closedBoth {
		t.Error("close() did not close the far stream")
	}
}

// trsfStub answers the parts of trsf.BidirectionalStream this test does not
// exercise, so each fake only writes the two methods it cares about.
type trsfStub struct{ closedBoth bool }

func (s *trsfStub) ID() trsf.StreamID { return 0 }
func (s *trsfStub) Close() error      { return nil }
func (s *trsfStub) CloseBoth() error  { s.closedBoth = true; return nil }
func (s *trsfStub) WriteContext(context.Context, []byte) (int, error) {
	return 0, nil
}
func (s *trsfStub) HasSendData() bool { return false }
func (s *trsfStub) Completed() bool   { return false }
func (s *trsfStub) AppendData(bool, ...[]byte) error {
	return nil
}
func (s *trsfStub) AppendDataContext(context.Context, bool, ...[]byte) error { return nil }
func (s *trsfStub) ReadContext(context.Context, []byte) (int, error)         { return 0, nil }
func (s *trsfStub) ReadDirect(uint64) ([]byte, bool, error)                  { return nil, false, nil }
func (s *trsfStub) ReadDirectContext(context.Context, uint64) ([]byte, bool, error) {
	return nil, false, nil
}
func (s *trsfStub) HasRecvData() bool { return false }
func (s *trsfStub) EOF() bool         { return false }
func (s *trsfStub) Cancel()           {}

// The distinction the relay exists to make, and the one it got wrong first:
// with NO hold armed, a dead far stream is the ordinary end of a session and
// must propagate, or agentexec's reaper never runs and the task hangs forever.
// Parking unconditionally hung every teardown in the suite.
func TestRelayPassesTheEndThroughWhenNoHoldIsArmed(t *testing.T) {
	far := newFakeFar()
	r := newSessionRelay(far, func() bool { return false })
	far.fail = true

	if _, err := r.Read(make([]byte, 4)); err == nil {
		t.Error("Read swallowed the end of the session with no hold armed")
	}
	r.detach()
	if _, err := r.Write([]byte("x")); err == nil {
		t.Error("Write parked with no hold armed — the session can never end")
	}
}

// And the same relay parks once a hold IS armed, so the pair proves the
// behaviour is conditional rather than absent.
func TestRelayParksOnlyWhileHeld(t *testing.T) {
	held := false
	r := newSessionRelay(newFakeFar(), func() bool { return held })
	r.detach()

	if _, err := r.Write([]byte("x")); err == nil {
		t.Fatal("Write should report an end while not held")
	}
	held = true
	done := make(chan struct{})
	go func() { defer close(done); _, _ = r.Write([]byte("y")) }()
	select {
	case <-done:
		t.Error("Write returned while held — the bytes were dropped")
	case <-time.After(50 * time.Millisecond):
	}
	r.rebind(newFakeFar())
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("Write never unparked after the rebind")
	}
}

// A dying trsf stream can return buffered bytes TOGETHER with the error. While
// held, the error must be swallowed and the bytes delivered: the caller is a
// frame decoder, and an error mid-header ends its loop, closes agentexec's
// stdin pipe and fires the reaper ladder at the child. This is the bug that
// killed every held session, and it was invisible — the exec call stays parked
// in its other goroutines, so nothing is ever reported.
func TestRelaySwallowsAnErrorThatArrivesWithBytesWhileHeld(t *testing.T) {
	far := &partialFar{data: []byte("hdr")}
	r := newSessionRelay(far, func() bool { return true })

	buf := make([]byte, 8)
	n, err := r.Read(buf)
	if err != nil {
		t.Fatalf("Read returned %v with bytes in hand — the frame decoder would abort", err)
	}
	if string(buf[:n]) != "hdr" {
		t.Errorf("read %q, want the buffered bytes", buf[:n])
	}
}

// partialFar returns its bytes and an error in the SAME call, once.
type partialFar struct {
	trsfStub
	data []byte
	done bool
}

func (f *partialFar) Read(p []byte) (int, error) {
	if f.done {
		return 0, io.EOF
	}
	f.done = true
	return copy(p, f.data), io.ErrUnexpectedEOF
}
func (f *partialFar) Write(p []byte) (int, error) { return len(p), nil }
