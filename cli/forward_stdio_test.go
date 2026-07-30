//go:build !js

package cli

import (
	"bytes"
	"context"
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
	case <-ctx.Done():
		return nil, false, ctx.Err()
	}
}

func (f *fakeRawStream) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
	return nil
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
		if err != nil {
			t.Fatalf("spliceStdio: %v", err)
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
	case <-done:
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
