//go:build js

package cli

import (
	"context"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/exec/frame"
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
//
// On wasm, the stream carries the exec frame mux (frame.Frame with Stdout/Stderr/Control
// types), not raw PTY bytes. This implementation parses frames directly because
// objtrsf/exec (CommandExecutionStream) is excluded from js builds. Payload bytes and
// the TerminalWindowSize control frame mirror what native LastWindowSize sees.
func (c *Client) CollectRaw(ctx context.Context, taskIDHex string, settle time.Duration) (captured []byte, rows, cols uint16, hasSize bool, err error) {
	stream, _, err := c.attachSessionRPC(ctx, taskIDHex, protocol.AttachMode_View)
	if err != nil {
		return nil, 0, 0, false, err
	}
	defer stream.Close()

	var mu sync.Mutex
	var data []byte
	var szRows, szCols uint16
	var szOK bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			f := &frame.Frame{}
			if rerr := f.Read(stream); rerr != nil {
				return
			}
			switch f.Header.Type {
			case frame.FrameType_Stdout, frame.FrameType_Stderr:
				if f.Header.Len == 0 {
					continue
				}
				payload := *f.Data()
				if len(payload) == 0 {
					continue
				}
				mu.Lock()
				data = append(data, payload...)
				full := len(data) > 8*1024*1024
				mu.Unlock()
				if full {
					return
				}
			default:
				if ctrl := f.Control(); ctrl != nil && ctrl.Type == frame.ControlType_TerminalWindowSize {
					ws := ctrl.TerminalWindowSize()
					mu.Lock()
					szRows, szCols, szOK = ws.Rows, ws.Columns, true
					mu.Unlock()
				}
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
	rows, cols, hasSize = szRows, szCols, szOK
	mu.Unlock()
	return captured, rows, cols, hasSize, nil
}
