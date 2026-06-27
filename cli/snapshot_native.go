//go:build !js

package cli

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/charmbracelet/x/vt"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// SessionSnapshot view-attaches to a detachable interactive session, feeds the
// replayed (and briefly-live) PTY byte stream through a headless VT emulator,
// and returns the current screen as plain text — a non-intrusive,
// terminal-free alternative to `session attach` for reading what a session
// currently shows.
//
// It uses AttachMode_View, so it never takes over the controlling client (a
// live operator keeps typing undisturbed). The emulator is sized from the
// TerminalWindowSize the server replays ahead of the ring (the controlling
// client's PTY size); defRows/defCols are the fallback when the session reports
// no size (e.g. an older server that does not replay it), in which case a
// full-screen TUI may mis-render.
//
// settle is how long to keep collecting bytes after attach before rendering;
// the replay arrives in a burst, so a short window (e.g. 1.5s) is enough for a
// static screen.
func (c *Client) SessionSnapshot(ctx context.Context, taskIDHex string, defRows, defCols uint16, settle time.Duration) (string, error) {
	stream, _, err := c.AttachSession(ctx, taskIDHex, protocol.AttachMode_View)
	if err != nil {
		return taskIDHex, err
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
	captured := append([]byte(nil), data...)
	mu.Unlock()

	rows, cols, ok := stream.LastWindowSize()
	if !ok || rows == 0 || cols == 0 {
		rows, cols = defRows, defCols
		fmt.Fprintf(os.Stderr,
			"harness-cli: session %s reported no terminal size; rendering at %dx%d (full-screen TUIs may mis-render)\n",
			taskIDHex, cols, rows)
	}

	emu := vt.NewEmulator(int(cols), int(rows))
	emu.Write(captured)
	return emu.String(), nil
}
