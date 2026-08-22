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
// synthesised reports how many bytes of SERVER-invented replay were seen — the
// mode preamble and the screen repaint. They are counted whether or not they
// are kept, so a caller can always say what was there; includeSynth decides
// whether they also appear IN captured, interleaved where they arrived.
//
// This file is untagged: exec/frame is untagged too, so the same frame reader
// serves native and js builds.
// It wraps attachSessionRPC directly instead of AttachSession because the two
// builds define AttachSession with different signatures (the js variant
// installs the browser xterm singleton — the wrong tool for a peek).
func (c *Client) CollectRaw(ctx context.Context, taskIDHex string, settle time.Duration, includeSynth bool) (captured []byte, rows, cols uint16, hasSize bool, synthesised int, err error) {
	// Deliberately kind-agnostic: this is the low-level "give me the replay
	// bytes" read, and for an event-stream task those bytes are raw NDJSON —
	// which is exactly what `session snapshot --raw` means there. The VT-render
	// callers above this sit on the PTY side; the stream kind's formatted view
	// is `session stream snapshot` (not built yet), not a VT screen.
	st, _, err := c.attachSessionRPC(ctx, taskIDHex, protocol.AttachMode_View, 0)
	if err != nil {
		return nil, 0, 0, false, 0, err
	}
	// Read FRAMES rather than wrapping the stream in a CommandExecutionStream.
	// That wrapper merges Stdout and Synth into one pipe — correct for anything
	// that renders, and exactly wrong here: a Synth frame carries bytes the
	// SERVER invented (a mode preamble, a screen repaint), and this function's
	// whole purpose is telling those from bytes the PTY produced. The
	// distinction only survives as frames.
	var mu sync.Mutex
	var data []byte
	var synth int
	var gotRows, gotCols uint16
	var gotSize bool
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			f := &frame.Frame{}
			if rerr := f.Read(st); rerr != nil {
				return
			}
			switch f.Header.Type {
			case frame.FrameType_Stdout, frame.FrameType_Stderr:
				if d := f.Data(); d != nil {
					mu.Lock()
					data = append(data, (*d)...)
					full := len(data) > 8*1024*1024
					mu.Unlock()
					if full {
						return
					}
				}
			case frame.FrameType_Synth:
				if d := f.Data(); d != nil {
					mu.Lock()
					synth += len(*d)
					// includeSynth keeps them IN, in place. Excluding them is
					// the right default for "show me what the PTY produced",
					// but it is the wrong answer for the one debugging session
					// that is about the synthesised bytes themselves — which is
					// exactly when someone reaches for raw output. Dropping
					// them unconditionally would force that person off this
					// tool and onto a live attach to see what the server sent.
					if includeSynth {
						data = append(data, (*d)...)
					}
					mu.Unlock()
				}
			case frame.FrameType_Control:
				// The PTY size the server replays ahead of the ring, which the
				// caller needs to size a render. Same job CommandExecutionStream
				// did with it.
				if ctrl := f.Control(); ctrl != nil && ctrl.Type == frame.ControlType_TerminalWindowSize {
					if ws := ctrl.TerminalWindowSize(); ws != nil {
						mu.Lock()
						gotRows, gotCols, gotSize = ws.Rows, ws.Columns, true
						mu.Unlock()
					}
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
	synthesised = synth
	rows, cols, hasSize = gotRows, gotCols, gotSize
	mu.Unlock()

	return captured, rows, cols, hasSize, synthesised, nil
}
