package cli

import (
	"sync"
	"sync/atomic"

	agentexec "github.com/on-keyday/objtrsf/exec"
)

// The pane slot registry behind the WebUI's live previews. It holds no JS and
// is built on every platform, so the generation rules (a stop or a newer start
// supersedes an older reservation) are tested by plain `go test`; the attach
// and the pump that use it are in preview_wasm.go.

// previewSlot is one live pane's stream plus a generation guard, so a
// superseded StartPreview (or a StopPreview) makes the old pump exit
// silently. Panes are keyed by an opaque paneKey string chosen by the JS
// side, so N panes can each hold an independent cowrite stream over the one
// shared client — the single preview the WebUI has today is just one pane
// keyed "preview".
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

// ReservePreview claims paneKey's slot for a start that has not issued its
// attach yet and returns the generation that claim holds. A caller that must
// WAIT before attaching (the WebUI bridge waits for a client after a
// reconnect) reserves first, so a StopPreview or a newer start for the same
// paneKey that lands during the wait supersedes it — reserving only once the
// wait is over would let whichever start reserves SECOND win, which is not
// necessarily the newer one. The previous stream for paneKey is closed here so
// it cannot outlive its (now stale) generation.
func ReservePreview(paneKey string) uint64 {
	previewMu.Lock()
	old := previewSlots[paneKey]
	gen := previewGen.Add(1)
	previewSlots[paneKey] = &previewSlot{gen: gen}
	previewMu.Unlock()
	if old != nil && old.stream != nil {
		_ = old.stream.Close()
	}
	return gen
}

// PreviewReserved reports whether gen is still paneKey's current reservation:
// false once a StopPreview or a newer ReservePreview has superseded it.
func PreviewReserved(paneKey string, gen uint64) bool {
	previewMu.Lock()
	defer previewMu.Unlock()
	slot := previewSlots[paneKey]
	return slot != nil && slot.gen == gen
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
