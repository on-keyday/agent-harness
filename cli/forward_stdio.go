//go:build !js

package cli

import (
	"context"
	"os"
	"sync"
)

// RunStdioForward opens a raw forward to host:port and splices it to this
// process's stdin/stdout — the harness equivalent of `ssh -W`. It returns when
// either side ends: EOF on stdin, the far end closing, or the forward being
// killed from another surface (which closes the data stream via RawConn's
// control watcher).
//
// Teardown is either-side-wins, matching spliceConnStream: a half-closed peer
// must not leave the reverse direction blocked forever.
func RunStdioForward(ctx context.Context, c *Client, taskIDHex, host string, port int, logf func(string)) error {
	rc, err := OpenRawForward(ctx, c, taskIDHex, host, port, logf)
	if err != nil {
		return err
	}
	var once sync.Once
	teardown := func() { once.Do(func() { _ = rc.Close() }) }
	defer teardown()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { // stdin -> forward
		defer wg.Done()
		defer teardown()
		buf := make([]byte, 32*1024)
		for {
			n, rerr := os.Stdin.Read(buf)
			if n > 0 {
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
	go func() { // forward -> stdout
		defer wg.Done()
		defer teardown()
		for {
			data, eof, rerr := rc.Recv(ctx)
			if len(data) > 0 {
				if _, werr := os.Stdout.Write(data); werr != nil {
					return
				}
			}
			if eof || rerr != nil {
				return
			}
		}
	}()
	wg.Wait()
	return nil
}
