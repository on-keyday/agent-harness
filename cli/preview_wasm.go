//go:build js

package cli

import (
	"context"
	"sync"
	"sync/atomic"
	"syscall/js"

	"github.com/on-keyday/agent-harness/runner/protocol"
	agentexec "github.com/on-keyday/objtrsf/exec"
)

// The preview singleton: a read-only AttachMode_View stream feeding the
// WebUI's session-preview modal, fully independent of the interactive
// singleton (activeInteractiveSession) so a preview can never disturb the
// main terminal. previewGen mirrors the interactiveGen pattern: every
// Start/Stop bumps it, and a pump whose generation is stale stops invoking
// JS hooks and exits. Pause in the UI is just StopPreview (the frozen xterm
// stays client-side); resume is a fresh StartPreview whose ring replay
// reconstructs the current screen — no bytes are buffered while paused.
var (
	previewMu     sync.Mutex
	previewStream *agentexec.CommandExecutionStream
	previewGen    atomic.Uint64
)

// StartPreview view-attaches to taskIDHex and starts pumping its output to
// the harness_preview* JS hooks. Any previous preview stream is superseded
// and closed first. The attach uses AttachMode_View, so it never takes over
// the session's controlling client.
func (c *Client) StartPreview(ctx context.Context, taskIDHex string) error {
	st, _, err := c.attachSessionRPC(ctx, taskIDHex, protocol.AttachMode_View)
	if err != nil {
		return err
	}
	stream := agentexec.NewCommandExecutionStream(st)

	previewMu.Lock()
	old := previewStream
	gen := previewGen.Add(1)
	previewStream = stream
	previewMu.Unlock()
	if old != nil {
		_ = old.Close()
	}

	go previewPump(stream, gen)
	return nil
}

// StopPreview closes the current preview stream, if any. Idempotent. The
// generation bump silences the pump's remaining callbacks immediately, so
// JS sees no harness_previewClosed for a locally-initiated stop.
func StopPreview() {
	previewMu.Lock()
	old := previewStream
	previewStream = nil
	previewGen.Add(1)
	previewMu.Unlock()
	if old != nil {
		_ = old.Close()
	}
}

// previewPump reads the view stream and forwards it to the JS hooks. The
// size control frame the server replays ahead of the ring is parsed by the
// CommandExecutionStream demux before the first Stdout bytes become
// readable, so LastWindowSize at the first successful read is the replayed
// size; harness_previewOpen fires exactly once, before the first write.
// Mid-stream size changes surface as harness_previewResize. All hooks are
// generation-gated; a raced late invoke after StopPreview is additionally
// ignored by the JS side's modal-open/live flags.
func previewPump(stream *agentexec.CommandExecutionStream, gen uint64) {
	defer stream.Close()
	out := stream.Stdout()
	buf := make([]byte, 32*1024)
	opened := false
	var lastRows, lastCols uint16
	for {
		n, err := out.Read(buf)
		if n > 0 {
			rows, cols, ok := stream.LastWindowSize()
			if !opened {
				opened = true
				lastRows, lastCols = rows, cols
				if !previewCall(gen, "harness_previewOpen", int(rows), int(cols), ok) {
					return
				}
			} else if ok && rows > 0 && cols > 0 && (rows != lastRows || cols != lastCols) {
				lastRows, lastCols = rows, cols
				if !previewCall(gen, "harness_previewResize", int(rows), int(cols)) {
					return
				}
			}
			arr := js.Global().Get("Uint8Array").New(n)
			js.CopyBytesToJS(arr, buf[:n])
			if !previewCall(gen, "harness_previewWrite", arr) {
				return
			}
		}
		if err != nil {
			previewCall(gen, "harness_previewClosed")
			return
		}
	}
}

// previewCall invokes the named JS hook iff gen is still the current
// generation; returns false when superseded so the pump exits silently. A
// missing hook (non-WebUI wasm host) is a no-op that keeps the pump alive.
func previewCall(gen uint64, fn string, args ...any) bool {
	if previewGen.Load() != gen {
		return false
	}
	f := js.Global().Get(fn)
	if f.Type() == js.TypeFunction {
		f.Invoke(args...)
	}
	return true
}
