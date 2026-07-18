//go:build !js

package cli

import (
	"context"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// CollectRaw view-attaches to a detachable interactive session and drains the
// replayed (and briefly-live) PTY byte burst for `settle`, returning the
// verbatim bytes — escape sequences intact — plus the terminal size the server
// replays ahead of the ring (hasSize=false when the session reports none, e.g.
// an older server). It uses AttachMode_View, so it never takes over the
// controlling client (a live operator keeps typing undisturbed). Shared by the
// raw path (SessionSnapshotRaw, which returns these bytes as-is), the rendered
// path (collectScreen, which feeds them through a VT emulator), and the wasm
// WebUI preview (which renders them in the browser's xterm instead — the VT
// emulator stays native-only).
func (c *Client) CollectRaw(ctx context.Context, taskIDHex string, settle time.Duration) (captured []byte, rows, cols uint16, hasSize bool, err error) {
	stream, _, err := c.AttachSession(ctx, taskIDHex, protocol.AttachMode_View)
	if err != nil {
		return nil, 0, 0, false, err
	}
	defer stream.Close()

	var mu sync.Mutex
	var data []byte
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 32*1024)
		out := stream.Stdout()
		for {
			n, rerr := out.Read(buf)
			if n > 0 {
				mu.Lock()
				data = append(data, buf[:n]...)
				full := len(data) > 8*1024*1024
				mu.Unlock()
				if full {
					return
				}
			}
			if rerr != nil {
				return
			}
		}
	}()

	select {
	case <-time.After(settle):
	case <-done:
	case <-ctx.Done():
	}

	mu.Lock()
	captured = append([]byte(nil), data...)
	mu.Unlock()

	rows, cols, hasSize = stream.LastWindowSize()
	return captured, rows, cols, hasSize, nil
}
