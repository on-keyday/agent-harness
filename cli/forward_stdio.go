//go:build !js

package cli

import (
	"context"
	"io"
	"os"
	"sync"
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

// RunStdioForward opens a raw forward to host:port and splices it to this
// process's stdin/stdout — the harness equivalent of `ssh -W`.
func RunStdioForward(ctx context.Context, c *Client, taskIDHex, host string, port int, logf func(string)) error {
	rc, err := OpenRawForward(ctx, c, taskIDHex, host, port, logf)
	if err != nil {
		return err
	}
	return spliceStdio(ctx, rc, os.Stdin, os.Stdout)
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
func spliceStdio(ctx context.Context, rc rawStream, in io.Reader, out io.Writer) error {
	var once sync.Once
	stop := make(chan struct{})
	teardown := func() { once.Do(func() { _ = rc.Close(); close(stop) }) }
	defer teardown()

	go func() { // in -> forward; may outlive this function, by design
		defer teardown()
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
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	for { // forward -> out, in the foreground
		data, eof, rerr := rc.Recv(ctx)
		if len(data) > 0 {
			if _, werr := out.Write(data); werr != nil {
				return nil
			}
		}
		if eof || rerr != nil {
			return nil
		}
	}
}
