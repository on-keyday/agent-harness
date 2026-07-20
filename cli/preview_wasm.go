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

// previewSlot is one live pane's view stream plus a generation guard, so a
// superseded StartPreview (or a StopPreview) makes the old pump exit
// silently. Panes are keyed by an opaque paneKey string chosen by the JS
// side, so N panes can each hold an independent read-only view stream over
// the one shared client — the single preview the WebUI has today is just
// one pane keyed "preview".
//
// previewGen mirrors the interactiveGen pattern, but is shared across ALL
// panes rather than per-pane: every Start/Stop of any pane reserves the
// next generation, and a pump whose reserved generation no longer matches
// its slot's current generation stops invoking JS hooks and exits.
// StartPreview reserves its generation BEFORE the attach RPC (not after),
// so a StopPreview or a newer StartPreview for the same paneKey landing
// while the RPC is still in flight supersedes it and the stop-wins
// invariant holds across that window too — the attach result is discarded
// instead of silently installing a stream nobody asked for anymore.
var (
	previewMu    sync.Mutex
	previewSlots = map[string]*previewSlot{}
	previewGen   atomic.Uint64 // monotonic across all panes; each start/stop reserves one
)

type previewSlot struct {
	stream *agentexec.CommandExecutionStream
	gen    uint64
}

// StartPreview view-attaches taskIDHex read-only and pumps its output to
// the JS hooks tagged with paneKey. Any previous preview stream for
// paneKey is superseded and closed first. The attach uses
// AttachMode_View, so it never takes over the session's controlling
// client.
// gridPreviewReplayLimit caps a grid pane's replay the same way the TUI does —
// a pane shows a small crop, not the full ~1 MiB ring. The single preview
// (cowrite=false) keeps the full replay (limit 0).
const gridPreviewReplayLimit = 128 * 1024

func (c *Client) StartPreview(ctx context.Context, paneKey, taskIDHex string, cowrite bool) error {
	// Reserve a generation BEFORE the RPC: a StopPreview (modal close) or a
	// newer StartPreview for this paneKey that lands while our attach is
	// still in flight supersedes us, and we discard our stream instead of
	// installing it (stop-wins). The old stream for paneKey is closed here
	// too so it cannot outlive its (now stale) generation while our RPC
	// runs.
	previewMu.Lock()
	old := previewSlots[paneKey]
	gen := previewGen.Add(1)
	previewSlots[paneKey] = &previewSlot{gen: gen}
	previewMu.Unlock()
	if old != nil && old.stream != nil {
		_ = old.stream.Close()
	}

	// Cowrite (grid panes) observes output like a viewer AND can forward input;
	// view (single preview) is read-only. Grid panes also cap the replay.
	mode := protocol.AttachMode_View
	var replayLimit uint32
	if cowrite {
		mode = protocol.AttachMode_Cowrite
		replayLimit = gridPreviewReplayLimit
	}
	st, _, err := c.attachSessionRPC(ctx, taskIDHex, mode, replayLimit)
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

// StopPreview closes paneKey's preview stream, if any. Idempotent. The
// generation bump silences the pump's remaining callbacks immediately, so
// JS sees no harness_previewClosed for a locally-initiated stop.
func StopPreview(paneKey string) {
	previewMu.Lock()
	old := previewSlots[paneKey]
	delete(previewSlots, paneKey)
	previewGen.Add(1)
	previewMu.Unlock()
	if old != nil && old.stream != nil {
		_ = old.stream.Close()
	}
}

// SendPreviewInput forwards raw bytes (a key's xterm data) to paneKey's session
// over its cowrite stream. No-op if the pane has no stream, or was attached
// read-only (view) — a view stream's input is discarded by the server anyway.
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

// previewPump reads paneKey's view stream and forwards it to the JS hooks.
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
