//go:build js

package cli

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"syscall/js"

	"github.com/on-keyday/agent-harness/exec/frame"
)

// AttachSession (WASM) re-attaches to an existing detachable interactive
// session identified by taskIDHex. It performs the AttachSession RPC, acquires
// the bidirectional stream, and installs it as the singleton
// activeInteractiveSession — exactly like InteractiveWithSelectorAndArgs does
// for a fresh session. The browser xterm will receive replayed + live output
// via harness_xtermWrite without any additional wiring.
//
// Returns the task's hex id (same as taskIDHex) on success.
func (c *Client) AttachSession(ctx context.Context, taskIDHex string) (string, error) {
	stream, _, err := c.attachSessionRPC(ctx, taskIDHex)
	if err != nil {
		return "", err
	}

	sessCtx, cancel := context.WithCancel(ctx)
	session := &InteractiveSession{
		stream:    stream,
		taskIDHex: taskIDHex,
		cancel:    cancel,
	}

	// Detach any pre-existing session (same pattern as InteractiveWithSelectorAndArgs).
	activeInteractiveMu.Lock()
	if old := activeInteractiveSession; old != nil {
		old.detach()
	}
	activeInteractiveSession = session
	activeInteractiveMu.Unlock()

	// recv goroutine: stream → frame parser → harness_xtermWrite.
	// Mirrors the goroutine in open_interactive_wasm.go verbatim.
	go func() {
		for {
			select {
			case <-sessCtx.Done():
				return
			default:
			}
			f := &frame.Frame{}
			if err := f.Read(stream); err != nil {
				if !errors.Is(err, io.EOF) {
					slog.Info("attachSession recv ended", "err", err)
				}
				activeInteractiveMu.Lock()
				if activeInteractiveSession == session {
					activeInteractiveSession = nil
				}
				activeInteractiveMu.Unlock()
				session.markClosed()
				return
			}
			switch f.Header.Type {
			case frame.FrameType_Stdout, frame.FrameType_Stderr:
				if f.Header.Len == 0 {
					continue
				}
				data := *f.Data()
				if len(data) == 0 {
					continue
				}
				arr := js.Global().Get("Uint8Array").New(len(data))
				js.CopyBytesToJS(arr, data)
				js.Global().Call("harness_xtermWrite", arr)
			default:
				// Stdin / Control frames going back to the client are
				// not part of the wire contract here.
			}
		}
	}()

	return taskIDHex, nil
}
