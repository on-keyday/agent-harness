package server

import (
	"encoding/hex"
	"errors"
	"time"
	"unicode/utf8"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// maxProgressBytes bounds a progress line: one line in a task list, and well
// inside the one-datagram budget of the request that carries it.
const maxProgressBytes = 256

var errBadProgressText = errors.New("progress text refused")

// validateProgressText refuses, rather than sanitizes, a line that is too long,
// not UTF-8, or carries a C0 / DEL / C1 control character, so the agent learns
// that its text did not land. Every surface renders the line into a terminal
// or a page; an escape sequence there is the operator's screen, not the task's.
func validateProgressText(text string, clear bool) error {
	if clear {
		if text != "" {
			return errBadProgressText
		}
		return nil
	}
	if len(text) > maxProgressBytes || !utf8.ValidString(text) {
		return errBadProgressText
	}
	for _, r := range text {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			return errBadProgressText
		}
	}
	return nil
}

// handleProgressSet writes the caller's OWN progress line. The target is the
// connection's principal, never a task id from the request — the request has
// none — so no capability is needed: it cannot touch anyone else's row. An
// operator connection has no principal and is refused.
func (h *TaskHandler) handleProgressSet(conn ConnHandle, req *protocol.TaskControlRequest) {
	respond := func(status protocol.ProgressSetStatus, at int64) {
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_ProgressSet, RequestId: req.RequestId}
		resp.SetProgressSet(protocol.ProgressSetResponse{Status: status, ProgressAt: uint64(at)})
		conn.SendMessage(resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})) //nolint:errcheck
	}
	ps := req.ProgressSet()
	if ps == nil {
		respond(protocol.ProgressSetStatus_BadText, 0)
		return
	}
	pid := h.lookupPrincipal(conn.ConnectionID().String())
	if pid.Id == ([16]byte{}) {
		respond(protocol.ProgressSetStatus_NoPrincipal, 0)
		return
	}
	text := string(ps.Text)
	if err := validateProgressText(text, ps.Clear()); err != nil {
		respond(protocol.ProgressSetStatus_BadText, 0)
		return
	}
	taskHex := hex.EncodeToString(pid.Id[:])
	snap, ok := h.Tasks.SetProgress(taskHex, text, time.Now().UnixNano())
	if !ok {
		respond(protocol.ProgressSetStatus_NotFound, 0)
		return
	}
	if h.OnTaskProgress != nil {
		h.OnTaskProgress(taskHex)
	}
	h.fireProgressWatchers(taskHex, protocol.AwaitProgressStatus_Fired, text, snap.ProgressAt)
	respond(protocol.ProgressSetStatus_Ok, snap.ProgressAt)
}

// progressAgeMs is now - progressAt on the server clock, as output_idle_ms is,
// so a client on another host never subtracts across clocks. 0 when never
// written, which the wire documents as meaningless beside progress_at == 0.
// Also 0 for a stamp in the server's future: the stamp is stored and replayed,
// so a clock that has stepped back since (a board with no RTC booting before
// NTP) would otherwise wrap the unsigned age to ~1.8e19.
func progressAgeMs(progressAt int64) uint64 {
	if progressAt <= 0 {
		return 0
	}
	if d := time.Since(time.Unix(0, progressAt)); d > 0 {
		return uint64(d / time.Millisecond)
	}
	return 0
}
