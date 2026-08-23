package cli

import (
	"context"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/exec/frame"
)

// LiveActivity is how much output arrived in REAL TIME during a capture, as
// opposed to replayed history.
//
// The distinction is the whole content of the measurement. A view-attach opens
// with the server replaying its ring, which arrives at wire speed: a megabyte
// of a week-old transcript can land in milliseconds, so counting the whole
// settle window would report a rate that describes the link, not the session.
// The server ends that replay with a synthesised screen repaint, and everything
// after it is the session producing output as it happens — so the window starts
// at the LAST synthesised frame.
//
// What it is good for is the state a rendered screen cannot report. A pane
// border, a resize, an unrecognised UI — anything that defeats the screen rules
// — leaves the arrival rate untouched, and an agent running a turn keeps
// emitting (Claude Code re-paints its spinner and re-emits its OSC title on
// every tick). What it CANNOT do is separate "waiting for a human" from
// "finished": both are silent, which is the same wall byte-quiescence hits.
type LiveActivity struct {
	// WindowMs is how long the live window actually lasted. Reported so a
	// reader can tell a rate from a count: 0 frames in 1500 ms is a
	// measurement, 0 frames in 3 ms is not.
	WindowMs int `json:"window_ms"`
	// Frames is how many PTY output frames arrived in the window. Frames are
	// TRANSPORT boundaries, not screen repaints — one repaint may span several
	// and several may coalesce into one — so this is an arrival rate, never a
	// frame rate in the display sense.
	Frames int `json:"frames"`
	// Bytes is how many PTY bytes arrived in the window.
	Bytes int `json:"bytes"`
	// Anchored reports that the window begins at a synthesised frame — i.e.
	// the server's end-of-replay repaint was seen and the counts describe live
	// output only. False means none arrived (an older server, or a capture cut
	// short), so the window still holds replayed history delivered at wire
	// speed and the counts are NOT a rate. Without this flag the two cases
	// produce the same shape of number and only one of them means anything.
	Anchored bool `json:"anchored"`
}

// liveMeter accumulates a LiveActivity. It exists as a type rather than as four
// variables inside the frame loop so the RESET RULE — a synthesised frame
// restarts the window and discards what came before it — has one spelling that
// a test can drive directly, without a server and without a clock.
type liveMeter struct {
	start    time.Time
	frames   int
	bytes    int
	anchored bool
}

// pty records one frame of PTY output.
func (m *liveMeter) pty(n int) {
	m.frames++
	m.bytes += n
}

// synth restarts the window. Called for EVERY synthesised frame, so the anchor
// lands on the last one — the replay's closing repaint — rather than on the
// mode preamble that opens it.
func (m *liveMeter) synth(now time.Time) {
	m.start = now
	m.frames, m.bytes = 0, 0
	m.anchored = true
}

func (m *liveMeter) result(now time.Time) LiveActivity {
	return LiveActivity{
		WindowMs: int(now.Sub(m.start) / time.Millisecond),
		Frames:   m.frames,
		Bytes:    m.bytes,
		Anchored: m.anchored,
	}
}

// RawCapture is one view-attach capture: the bytes, the size the server
// replayed ahead of them, and the two measurements ABOUT the capture that the
// bytes themselves cannot carry.
type RawCapture struct {
	// Bytes is the verbatim burst, escape sequences intact.
	Bytes []byte
	// Rows/Cols is the PTY size the server replays ahead of the ring;
	// HasSize=false when the session reports none (e.g. an older server), in
	// which case a renderer must fall back to a default and say so.
	Rows, Cols uint16
	HasSize    bool
	// Synthesised is how many bytes of SERVER-invented replay were seen —
	// the mode preamble and the screen repaint. Counted whether or not they
	// were kept, so a caller can always say what was there.
	Synthesised int
	// Live is the real-time tail of the capture. See LiveActivity.
	Live LiveActivity
}

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
// includeSynth decides only whether the synthesised bytes also appear IN
// RawCapture.Bytes, interleaved where they arrived; they are counted, and they
// anchor the live window, either way. See RawCapture for what each field means.
//
// This file is untagged: exec/frame is untagged too, so the same frame reader
// serves native and js builds.
// It wraps attachSessionRPC directly instead of AttachSession because the two
// builds define AttachSession with different signatures (the js variant
// installs the browser xterm singleton — the wrong tool for a peek).
func (c *Client) CollectRaw(ctx context.Context, taskIDHex string, settle time.Duration, includeSynth bool) (RawCapture, error) {
	// Deliberately kind-agnostic: this is the low-level "give me the replay
	// bytes" read, and for an event-stream task those bytes are raw NDJSON —
	// which is exactly what `session snapshot --raw` means there. The VT-render
	// callers above this sit on the PTY side; the stream kind's formatted view
	// is `session stream snapshot` (not built yet), not a VT screen.
	st, _, err := c.attachSessionRPC(ctx, taskIDHex, protocol.AttachMode_View, 0)
	if err != nil {
		return RawCapture{}, err
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
	// Started before the first frame is read, so an attach that receives no
	// synthesised frame at all still reports a window it actually observed —
	// unanchored, which is what Anchored says.
	meter := liveMeter{start: time.Now()}
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
					meter.pty(len(*d))
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
					// Restarts the live window: everything before this frame
					// is replayed history, however fast it arrived.
					meter.synth(time.Now())
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
	out := RawCapture{
		Bytes:       append([]byte(nil), data...),
		Rows:        gotRows,
		Cols:        gotCols,
		HasSize:     gotSize,
		Synthesised: synth,
		Live:        meter.result(time.Now()),
	}
	mu.Unlock()

	return out, nil
}
