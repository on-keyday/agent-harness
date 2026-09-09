package runner

import (
	"context"
	"io"
	"sync"

	"github.com/on-keyday/objtrsf/trsf"
)

// sessionRelay is the runner's own trsf.BidirectionalStream, handed to
// agentexec in place of the server's stream.
//
// agentexec takes its stream as a CONSTRUCTOR ARGUMENT to a call that blocks
// for the whole session and defers CloseBoth, and the package has no
// stream-swap entry point — so a rebind is impossible from outside unless the
// stream it holds is one we control. The parameter is an interface, so it can
// be.
//
// Everything the hold needs turns out to be a property of this one object,
// which is why it is the right shape rather than a workaround:
//
//   - stop draining: Read blocks instead of returning, so the PTY buffer fills
//     and the child blocks in write() with nothing dropped.
//   - never close the PTY master: CloseBoth is not forwarded while held. The
//     child is a session leader with that PTY as its controlling terminal
//     (go-pty sets Setsid+Setctty), so closing the master would hang up its
//     session — the exact thing being preserved.
//   - the EOF→SIGHUP ladder becomes UNREACHABLE rather than suppressed. That
//     ladder fires when the stream agentexec holds reaches EOF; the stream it
//     holds is this, and this does not EOF because a server went away.
//
// The alternative was teaching objtrsf's exec package about holds. That would
// put harness policy inside a general-purpose package which cannot know when
// "do not close the PTY" is right — it runs a command against a stream and has
// no notion of a server that will come back.
type sessionRelay struct {
	mu sync.Mutex
	// far is the stream currently pointed at the server. nil means the link is
	// gone: reads park and writes are held rather than failing.
	far trsf.BidirectionalStream
	// wake is closed and replaced whenever far changes, so a parked reader
	// re-checks.
	wake chan struct{}
	// pending is the one chunk read from the far side that could not be
	// delivered, or the bytes that must be written first after a rebind. It is
	// what keeps the gap continuous: a chunk already taken off the wire exists
	// nowhere else.
	pending []byte
	closed  bool
}

// Compile-time proof that the relay can stand in for the server's stream.
// Without this the file builds happily until the first assignment, and the
// whole design rests on this substitution being legal.
var _ trsf.BidirectionalStream = (*sessionRelay)(nil)

func newSessionRelay(far trsf.BidirectionalStream) *sessionRelay {
	return &sessionRelay{far: far, wake: make(chan struct{})}
}

// rebind points the relay at a new far stream and wakes anything parked.
func (r *sessionRelay) rebind(far trsf.BidirectionalStream) {
	r.mu.Lock()
	r.far = far
	close(r.wake)
	r.wake = make(chan struct{})
	r.mu.Unlock()
}

// detach drops the far stream without closing the near side, which is what a
// disconnect does while a hold is armed.
func (r *sessionRelay) detach() {
	r.mu.Lock()
	r.far = nil
	close(r.wake)
	r.wake = make(chan struct{})
	r.mu.Unlock()
}

func (r *sessionRelay) current() (trsf.BidirectionalStream, chan struct{}, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.far, r.wake, r.closed
}

// Read carries the server's bytes toward the child. With no far stream it
// PARKS rather than returning EOF: an EOF here is what agentexec's reaper
// ladder watches for, and the child must not be reaped because a server went
// away.
func (r *sessionRelay) Read(p []byte) (int, error) {
	for {
		far, wake, closed := r.current()
		if closed {
			return 0, io.EOF
		}
		if far == nil {
			<-wake
			continue
		}
		n, err := far.Read(p)
		if err == nil || n > 0 {
			return n, err
		}
		// The far side died. Park for a rebind instead of surfacing the error:
		// while a hold is armed this is a gap, not an end.
		r.mu.Lock()
		if r.far == far {
			r.far = nil
			close(r.wake)
			r.wake = make(chan struct{})
		}
		closedNow := r.closed
		r.mu.Unlock()
		if closedNow {
			return 0, io.EOF
		}
	}
}

// Write carries the child's bytes toward the server. With no far stream the
// bytes are RETAINED, not dropped: they were already taken off the PTY, so
// they exist nowhere else. The retained chunk is flushed ahead of the first
// write after a rebind.
func (r *sessionRelay) Write(p []byte) (int, error) {
	r.mu.Lock()
	far, closed := r.far, r.closed
	if closed {
		r.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	if far == nil {
		// Bounded by one chunk per gap: the drain stops after this, because
		// agentexec's copier blocks on the next Write until a rebind.
		if r.pending == nil {
			r.pending = append([]byte(nil), p...)
		}
		wake := r.wake
		r.mu.Unlock()
		<-wake // park: the PTY buffer fills behind us, and nothing is lost
		return len(p), nil
	}
	pending := r.pending
	r.pending = nil
	r.mu.Unlock()
	if len(pending) > 0 {
		if _, err := far.Write(pending); err != nil {
			return 0, err
		}
	}
	return far.Write(p)
}

// CloseBoth is NOT forwarded: agentexec defers it, and forwarding it would
// close the PTY master and hang up the child's session. The relay closes when
// the task really ends, through close().
func (r *sessionRelay) CloseBoth() error { return nil }

// close ends the relay for good and unblocks anything parked.
func (r *sessionRelay) close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	close(r.wake)
	r.wake = make(chan struct{})
	far := r.far
	r.far = nil
	r.mu.Unlock()
	if far != nil {
		_ = far.CloseBoth()
	}
}

// The rest of trsf.BidirectionalStream. agentexec uses Read/Write/CloseBoth
// (and ID for logging); the remainder is delegated to whatever far stream is
// current, or answered conservatively while there is none — "no data, not
// completed, not EOF" is the truthful answer during a gap, and it is also the
// answer that keeps a poller from concluding the session ended.

func (r *sessionRelay) ID() trsf.StreamID {
	if far, _, _ := r.current(); far != nil {
		return far.ID()
	}
	return 0
}

func (r *sessionRelay) Close() error { return nil } // see CloseBoth

func (r *sessionRelay) WriteContext(ctx context.Context, data []byte) (int, error) {
	done := make(chan struct{})
	var n int
	var err error
	go func() { defer close(done); n, err = r.Write(data) }()
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-done:
		return n, err
	}
}

func (r *sessionRelay) HasSendData() bool {
	if far, _, _ := r.current(); far != nil {
		return far.HasSendData()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending) > 0
}

func (r *sessionRelay) Completed() bool {
	_, _, closed := r.current()
	return closed
}

func (r *sessionRelay) AppendData(eof bool, data ...[]byte) error {
	for _, d := range data {
		if _, err := r.Write(d); err != nil {
			return err
		}
	}
	return nil
}

func (r *sessionRelay) AppendDataContext(ctx context.Context, eof bool, data ...[]byte) error {
	for _, d := range data {
		if _, err := r.WriteContext(ctx, d); err != nil {
			return err
		}
	}
	return nil
}

func (r *sessionRelay) ReadContext(ctx context.Context, p []byte) (int, error) {
	done := make(chan struct{})
	var n int
	var err error
	go func() { defer close(done); n, err = r.Read(p) }()
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-done:
		return n, err
	}
}

func (r *sessionRelay) ReadDirect(maxN uint64) ([]byte, bool, error) {
	buf := make([]byte, maxN)
	n, err := r.Read(buf)
	return buf[:n], false, err
}

func (r *sessionRelay) ReadDirectContext(ctx context.Context, maxN uint64) ([]byte, bool, error) {
	buf := make([]byte, maxN)
	n, err := r.ReadContext(ctx, buf)
	return buf[:n], false, err
}

func (r *sessionRelay) HasRecvData() bool {
	if far, _, _ := r.current(); far != nil {
		return far.HasRecvData()
	}
	return false
}

// EOF stays false while a hold is in force. It is the predicate the reaper
// ladder consults, and during a gap the session has not ended.
func (r *sessionRelay) EOF() bool {
	_, _, closed := r.current()
	return closed
}

func (r *sessionRelay) Cancel() {
	if far, _, _ := r.current(); far != nil {
		far.Cancel()
	}
}
