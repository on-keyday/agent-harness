//go:build !js

package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRawStream reports EOF from the far end once its queued bytes (if any)
// are drained, and never produces data beyond what's queued. Its Send records
// bytes so the reverse direction can be checked.
//
// Recv drains recv before reporting EOF: closing the channel (rather than
// setting a timing-dependent flag) is what makes
// TestSpliceStdio_ForwardsStdinAndFarEndBytes deterministic — a closed,
// empty channel receive returns immediately with ok=false, so the queued
// bytes are always delivered before EOF regardless of goroutine scheduling.
type fakeRawStream struct {
	mu     sync.Mutex
	sent   []byte
	closed bool
	recv   chan []byte
	eof    bool

	// sendCh, when non-nil, is closed the first time Send is called. Test 3
	// uses it to observe "the stdin bytes were recorded" without polling or
	// sleeping: spliceStdio's near-end (stdin) pump runs in a goroutine that
	// is deliberately never waited on (see spliceStdio's comment), so a test
	// that wants to assert on both directions after spliceStdio returns must
	// synchronize on the near-end's progress itself, or risk the far-end EOF
	// path winning the race and returning before Send ever ran.
	sendOnce sync.Once
	sendCh   chan struct{}

	// closeCh is closed by Close, and Recv reports EOF once it is. Without this
	// the fake kept handing out queued bytes after a teardown — which is exactly
	// what let the "reply discarded on stdin EOF" bug pass this file's tests.
	closeOnce sync.Once
	closeCh   chan struct{}
}

// closedSignal lazily creates the close channel so tests can keep using struct
// literals.
func (f *fakeRawStream) closedSignal() chan struct{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closeCh == nil {
		f.closeCh = make(chan struct{})
	}
	return f.closeCh
}

func (f *fakeRawStream) Send(b []byte) error {
	f.mu.Lock()
	f.sent = append(f.sent, b...)
	f.mu.Unlock()
	if f.sendCh != nil {
		f.sendOnce.Do(func() { close(f.sendCh) })
	}
	return nil
}

func (f *fakeRawStream) Recv(ctx context.Context) ([]byte, bool, error) {
	if f.eof {
		return nil, true, nil
	}
	select {
	case b, ok := <-f.recv:
		if !ok {
			return nil, true, nil
		}
		return b, false, nil
	case <-f.closedSignal():
		return nil, true, nil
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
}

func (f *fakeRawStream) Close() error {
	ch := f.closedSignal()
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	f.closeOnce.Do(func() { close(ch) })
	return nil
}

// TestSpliceStdio_StdinEOFKeepsReadingForTheReply is the regression for a
// request/response use: `printf 'GET …' | forward -W host:80` closes stdin the
// moment the request is written, and an implementation that tore the forward
// down there printed nothing at all. stdin EOF must half-close the send
// direction and leave the read side running until the far end is done.
func TestSpliceStdio_StdinEOFKeepsReadingForTheReply(t *testing.T) {
	f := &fakeRawStream{recv: make(chan []byte, 1), sendCh: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// The reply is produced only AFTER the request was sent and stdin hit EOF,
	// which is the ordering a real server imposes.
	// The reply is produced only after the request was observed, and after a
	// delay long enough that a teardown-on-stdin-EOF implementation will have
	// closed the stream first (the fake's Close makes Recv report EOF, as the
	// real one does).
	go func() {
		select {
		case <-f.sendCh:
		case <-ctx.Done():
			return
		}
		time.Sleep(150 * time.Millisecond)
		select {
		case f.recv <- []byte("pong"):
			close(f.recv)
		default:
		}
	}()

	var out bytes.Buffer
	if err := spliceStdio(ctx, f, strings.NewReader("ping"), &out); err != nil {
		t.Fatalf("spliceStdio: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "pong") {
		t.Fatalf("reply arriving after stdin EOF was lost: out=%q", got)
	}
}

// TestSpliceStdio_ReturnsWhenFarEndEndsWithIdleStdin is the regression this
// shape exists for: a `-W` session with an idle operator must exit when the far
// end closes or the forward is killed. A stdin pump that the function waits on
// would hang here forever — and because main() installs signal.NotifyContext,
// a hung splice also swallows Ctrl-C.
func TestSpliceStdio_ReturnsWhenFarEndEndsWithIdleStdin(t *testing.T) {
	idleStdin, w, err := os.Pipe() // never written to: a parked, uninterruptible read
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	defer idleStdin.Close()

	f := &fakeRawStream{eof: true}
	done := make(chan error, 1)
	go func() { done <- spliceStdio(context.Background(), f, idleStdin, io.Discard) }()

	select {
	case err := <-done:
		// Nothing moved in either direction, so this doubles as the
		// ErrNoDataTransferred case: `-W` must not exit 0 when the target was
		// never reached, or a pipeline reads that as a successful transfer.
		if !errors.Is(err, ErrNoDataTransferred) {
			t.Fatalf("spliceStdio err = %v, want ErrNoDataTransferred", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("spliceStdio did not return after the far end reported EOF with an idle stdin")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		t.Fatal("the forward must be closed on the way out")
	}
}

// TestSpliceStdio_ReturnsOnContextCancel covers the Ctrl-C path with the same
// idle stdin.
func TestSpliceStdio_ReturnsOnContextCancel(t *testing.T) {
	idleStdin, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	defer idleStdin.Close()

	f := &fakeRawStream{recv: make(chan []byte)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- spliceStdio(ctx, f, idleStdin, io.Discard) }()
	cancel()

	select {
	case err := <-done:
		// Ctrl-C is the operator stopping us, not a failed transfer, so it must
		// exit cleanly however few bytes moved.
		if err != nil {
			t.Fatalf("cancellation must not report a transfer failure: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("spliceStdio did not return on context cancellation")
	}
}

// TestSpliceStdio_ForwardsStdinAndFarEndBytes checks both directions still
// move. It queues "pong" on the far end up front, but only closes recv (the
// far end's EOF signal) after observing "ping" was recorded via f.sendCh —
// see fakeRawStream.sendCh's comment for why that synchronization is required
// rather than assumed: without it, spliceStdio's foreground loop can drain
// "pong" and see EOF (and so return) before the backgrounded stdin pump's
// Send ever runs, making the "stdin bytes not sent" assertion flaky-to-always
// fail depending on goroutine scheduling, not on real behavior.
func TestSpliceStdio_ForwardsStdinAndFarEndBytes(t *testing.T) {
	in := strings.NewReader("ping")
	f := &fakeRawStream{recv: make(chan []byte, 1), sendCh: make(chan struct{})}
	f.recv <- []byte("pong")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		select {
		case <-f.sendCh:
		case <-ctx.Done():
		}
		close(f.recv) // drained first (buffered "pong"), then EOF once empty and closed.
	}()

	var out bytes.Buffer
	if err := spliceStdio(ctx, f, in, &out); err != nil {
		t.Fatalf("spliceStdio: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "pong") {
		t.Fatalf("far-end bytes not written to out: %q", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if got := string(f.sent); got != "ping" {
		t.Fatalf("stdin bytes not sent: %q", got)
	}
}
