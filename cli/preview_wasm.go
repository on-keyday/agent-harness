//go:build js

package cli

import (
	"context"
	"syscall/js"

	"github.com/on-keyday/agent-harness/runner/protocol"
	agentexec "github.com/on-keyday/objtrsf/exec"
)

// GridPaneReplayLimit caps a grid pane's replay the same way the TUI does — a
// pane shows a small crop, not the full ~1 MiB ring. The single preview passes
// 0 (uncapped): it is a full-size terminal, and the scrollback is the point.
//
// This is the ONLY difference left between the two kinds of pane. It used to
// ride on the same boolean as the attach mode, so making the single preview
// typable would silently have shrunk its scrollback to 128 KiB as well — two
// unrelated properties welded to one flag.
const GridPaneReplayLimit = 128 * 1024

// StartPreview cowrite-attaches taskIDHex and pumps its output to the JS hooks
// tagged with paneKey. Any previous preview stream for paneKey is superseded
// and closed first.
//
// AttachMode_Cowrite observes output exactly like a viewer and additionally
// accepts input; it does NOT take over the session's controlling client and
// carries no size authority (the server drops a cowriter's winsize frames,
// session_mux.go forwardCowriterFrames). So every pane — grid cell and single
// preview alike — can be typed into without disturbing whoever is driving,
// which is why the read-only variant is gone: a preview you cannot type into
// looked identical to one you could, and the only feedback was the keystroke
// vanishing.
//
// replayLimit caps the replayed ring (0 = uncapped); see GridPaneReplayLimit.
func (c *Client) StartPreview(ctx context.Context, paneKey, taskIDHex string, replayLimit uint32) error {
	return c.StartReservedPreview(ctx, paneKey, ReservePreview(paneKey), taskIDHex, replayLimit)
}

// StartReservedPreview is StartPreview for a generation already reserved with
// ReservePreview. A superseded reservation returns nil without attaching.
func (c *Client) StartReservedPreview(ctx context.Context, paneKey string, gen uint64, taskIDHex string, replayLimit uint32) error {
	if !PreviewReserved(paneKey, gen) {
		return nil // superseded while the caller waited: whoever superseded us owns paneKey
	}
	st, _, err := c.attachSessionRPC(ctx, taskIDHex, protocol.AttachMode_Cowrite, replayLimit)
	if err != nil {
		return err
	}
	stream := agentexec.NewCommandExecutionStream(st)

	previewMu.Lock()
	slot := previewSlots[paneKey]
	if slot == nil || slot.gen != gen {
		// Superseded while attaching — whoever superseded us owns paneKey's
		// UI state; just discard the freshly-opened stream.
		previewMu.Unlock()
		_ = stream.Close()
		return nil
	}
	slot.stream = stream
	previewMu.Unlock()

	go previewPump(paneKey, stream, gen)
	return nil
}

// SendPreviewInput forwards raw bytes (a key's xterm data) to paneKey's session
// over its cowrite stream. No-op if the pane has no stream — a keystroke racing
// teardown is dropped rather than written to a closed stream.
func SendPreviewInput(paneKey string, data []byte) {
	previewMu.Lock()
	slot := previewSlots[paneKey]
	var s *agentexec.CommandExecutionStream
	if slot != nil {
		s = slot.stream
	}
	previewMu.Unlock()
	if s == nil || len(data) == 0 {
		return
	}
	_, _ = s.Stdin().Write(data)
}

// previewPump reads paneKey's stream and forwards it to the JS hooks.
// The size control frame the server replays ahead of the ring is parsed by
// the CommandExecutionStream demux before the first Stdout bytes become
// readable, so LastWindowSize at the first successful read is the
// replayed size; harness_previewOpen fires exactly once, before the first
// write. Mid-stream size changes surface as harness_previewResize. All
// hooks are generation-gated; a raced late invoke after StopPreview is
// additionally ignored by the JS side's modal-open/live flags.
func previewPump(paneKey string, stream *agentexec.CommandExecutionStream, gen uint64) {
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
				if !previewCall(paneKey, gen, "harness_previewOpen", int(rows), int(cols), ok) {
					return
				}
			} else if ok && rows > 0 && cols > 0 && (rows != lastRows || cols != lastCols) {
				lastRows, lastCols = rows, cols
				if !previewCall(paneKey, gen, "harness_previewResize", int(rows), int(cols)) {
					return
				}
			}
			arr := js.Global().Get("Uint8Array").New(n)
			js.CopyBytesToJS(arr, buf[:n])
			if !previewCall(paneKey, gen, "harness_previewWrite", arr) {
				return
			}
		}
		if err != nil {
			previewCall(paneKey, gen, "harness_previewClosed")
			return
		}
	}
}

// previewCall invokes the named JS hook with paneKey as its first argument,
// iff gen is still paneKey's current generation; returns false when
// superseded so the pump exits silently. A missing hook (non-WebUI wasm
// host) is a no-op that keeps the pump alive.
func previewCall(paneKey string, gen uint64, fn string, args ...any) bool {
	previewMu.Lock()
	slot := previewSlots[paneKey]
	live := slot != nil && slot.gen == gen
	previewMu.Unlock()
	if !live {
		return false
	}
	f := js.Global().Get(fn)
	if f.Type() == js.TypeFunction {
		all := make([]any, 0, len(args)+1)
		all = append(all, paneKey)
		all = append(all, args...)
		f.Invoke(all...)
	}
	return true
}
