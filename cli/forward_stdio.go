//go:build !js

package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
)

// rawStream is the part of *RawConn the stdio splice needs. It exists so the
// splice can be tested without a server: the interesting property (this
// function returns when the FAR end ends, even though the near end is parked in
// a read that cannot be interrupted) is otherwise only reachable end-to-end.
type rawStream interface {
	Send(b []byte) error
	Recv(ctx context.Context) ([]byte, bool, error)
	Close() error
}

// ErrNoDataTransferred reports that the forward ended without a single byte
// moving in either direction. The runner deliberately maps a failed dial onto a
// plain stream close — "connection-refused semantics", see
// runner/port_forward.go's handleOpenPortForward — so no reason for the close
// ever reaches the client. This is therefore the only signal available, and it
// exists because the alternative was worse: `-W` used to exit 0 silently when
// the target was unreachable, telling a script in a pipeline that the transfer
// had succeeded.
//
// It is a heuristic about the CAUSE and deliberately does not assert one: a
// connection that legitimately transfers nothing and closes reports the same
// thing.
var ErrNoDataTransferred = errors.New("connection ended without transferring any data")

// RunHTTPRequestForward sends one built request over a raw forward and copies
// the response to out until the far end closes.
//
// stdin is deliberately NOT spliced: a request fully specified by flags has
// nothing to read from it, and splicing would leave the command waiting on a
// terminal after the response had already arrived. That makes this the
// one-shot counterpart to RunStdioForward rather than a mode of it.
//
// The spec is built before anything is dialled, so a mistyped header costs an
// error message rather than a forward that is established, registered and then
// torn down.
func RunHTTPRequestForward(ctx context.Context, c *Client, taskIDHex, host string, port int, spec HTTPRequestSpec, out io.Writer, logf func(string)) error {
	req, err := BuildHTTPRequest(spec, host, port)
	if err != nil {
		return err
	}
	rc, err := OpenRawForward(ctx, c, taskIDHex, host, port, logf)
	if err != nil {
		return err
	}
	defer rc.Close()
	if err := rc.Send(req); err != nil {
		return err
	}
	for {
		data, eof, rerr := rc.Recv(ctx)
		if len(data) > 0 {
			if _, werr := out.Write(data); werr != nil {
				return werr
			}
		}
		if eof {
			return nil
		}
		if rerr != nil {
			return rerr
		}
	}
}

// RunStdioForward opens a raw forward to host:port and splices it to this
// process's stdin/stdout — the harness equivalent of `ssh -W`.
func RunStdioForward(ctx context.Context, c *Client, taskIDHex, host string, port int, logf func(string)) error {
	rc, err := OpenRawForward(ctx, c, taskIDHex, host, port, logf)
	if err != nil {
		return err
	}
	if err := spliceStdio(ctx, rc, os.Stdin, os.Stdout); err != nil {
		if errors.Is(err, ErrNoDataTransferred) {
			return fmt.Errorf("forward %s:%d: %w (the runner may have failed to dial it)", host, port, err)
		}
		return err
	}
	return nil
}

// spliceStdio pumps bytes both ways and returns when either side ends: EOF on
// in, the far end closing, the forward being killed from another surface (which
// closes the data stream, so Recv reports EOF), or ctx cancellation (Ctrl-C).
//
// The asymmetry is load-bearing. `spliceConnStream` can tear down both
// directions because closing the net.Conn unblocks the read that is parked on
// it. Here the near side is os.Stdin, and closing the forward has no OS-level
// relationship to a blocked os.Stdin.Read — nothing can interrupt it. So the
// far-side pump runs in the FOREGROUND and decides when this function returns,
// and the stdin pump is deliberately NOT waited on: it may stay parked in its
// read until the process exits, which is fine for a foreground CLI and is the
// only alternative to hanging forever with Ctrl-C neutralised (main installs a
// signal.NotifyContext, so a hung splice would swallow SIGINT too).
// Returns ErrNoDataTransferred when the forward ended with nothing having moved
// in either direction and the caller did not cancel — see that error's comment.
// Byte counts are atomic because the stdin pump outlives this function.
func spliceStdio(ctx context.Context, rc rawStream, in io.Reader, out io.Writer) error {
	var once sync.Once
	var sent, received atomic.Int64
	stop := make(chan struct{})
	teardown := func() { once.Do(func() { _ = rc.Close(); close(stop) }) }
	defer teardown()

	go func() { // in -> forward; may outlive this function, by design
		buf := make([]byte, 32*1024)
		for {
			n, rerr := in.Read(buf)
			if n > 0 {
				select {
				case <-stop: // torn down while we were parked in Read
					return
				default:
				}
				// Send copies, so reusing buf is safe.
				if serr := rc.Send(buf[:n]); serr != nil {
					teardown()
					return
				}
				sent.Add(int64(n))
			}
			if rerr != nil {
				// EOF on the near side ends the REQUEST, not the session: stop
				// reading and leave the connection up so the far end can still
				// answer. `printf 'GET …' | forward -W host:80` closes stdin the
				// instant the request is written, and tearing down here printed
				// nothing at all.
				//
				// Signalling EOF on the stream instead is NOT an option: the
				// runner splices with either-side-wins teardown
				// (spliceConnStream, "correct for TCP, where a half-closed/RST
				// peer must not leave the reverse relay blocked forever"), so a
				// half-close propagates as a full close and discards the reply
				// just the same. Verified against a dummy harness: both variants
				// returned zero bytes. There is no half-close in this path, so
				// the session ends when the far end closes, when the operator
				// interrupts, or when the forward is killed.
				//
				// A non-EOF read error is a broken near side, so that does end it.
				if !errors.Is(rerr, io.EOF) {
					teardown()
				}
				return
			}
		}
	}()

	done := func() error {
		// A cancelled context is the operator stopping us (Ctrl-C); that is not
		// a failed transfer, so it exits cleanly however few bytes moved.
		if ctx.Err() == nil && sent.Load() == 0 && received.Load() == 0 {
			return ErrNoDataTransferred
		}
		return nil
	}

	for { // forward -> out, in the foreground
		data, eof, rerr := rc.Recv(ctx)
		if len(data) > 0 {
			if _, werr := out.Write(data); werr != nil {
				return done()
			}
			received.Add(int64(len(data)))
		}
		if eof || rerr != nil {
			return done()
		}
	}
}
