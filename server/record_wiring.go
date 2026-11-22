package server

import (
	"log/slog"
	"time"

	"github.com/on-keyday/agent-harness/authority"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// RecordSettings are the server's PTY-record flags (pillar A spec, A1).
type RecordSettings struct {
	Default   bool          // --record: an open that says nothing is recorded
	RunMax    int64         // --record-run-max: entry bytes kept per run
	TotalMax  int64         // --record-total-max: bytes on disk in all
	Retention time.Duration // --record-retention: 0 = forever
}

// recordForOpen resolves an open's record switch (A-D22): its own on or off,
// else the server default. Saying on needs record_persist — keeping takes the
// server's disk and pushes older records out under the cap — and refused
// reports false in ok. Saying nothing or off needs nothing. A stream session
// is never recorded (A-D21), whatever was asked.
func (h *TaskHandler) recordForOpen(req *protocol.OpenInteractiveRequest, callerCaps protocol.Capability) (record, ok bool) {
	if ext := req.Ext(); req.ExtPresent() && ext != nil && ext.RecordSet() {
		if !ext.Record() {
			return false, true
		}
		if !hasCap(callerCaps, authority.RecordOnCap) {
			return false, false
		}
		record = true
	} else {
		record = h.RecordDefault
	}
	return record && interactiveKind(req) == protocol.TaskKind_Interactive, true
}

// startRecording starts recording a session whose task says so: at placement
// or an open (from the ring, which holds exactly what arrived since the mux
// was built — so the record starts at the PTY's first byte), or at a
// re-adoption (continuing the held run's open segment).
func (h *TaskHandler) startRecording(taskID string, mux *SessionMux, how recordStart) {
	if h.Records == nil || mux == nil || !mux.HasTerminal() {
		return
	}
	t, ok := h.Tasks.Get(taskID)
	if !ok || !t.Record {
		return
	}
	w, err := h.Records.OpenRun(taskID, t.Run, time.Now())
	if err != nil {
		slog.Warn("record: cannot open the run's record", "task", taskID, "run", t.Run, "err", err)
		return
	}
	if err := mux.StartRecording(w, how); err != nil {
		slog.Warn("record: cannot start recording", "task", taskID, "err", err)
		_ = w.Detach()
	}
}

// finishRecording stamps a finished run's end and reason on its record and
// closes it. Called when the session is over — from the end of the task and
// from the mux stopping — and harmless when there is no record, when the
// run is not over yet, or a second time.
func (h *TaskHandler) finishRecording(taskID string) {
	if h == nil || h.Records == nil {
		return
	}
	t, ok := h.Tasks.Get(taskID)
	if !ok {
		return
	}
	switch t.Status {
	case protocol.TaskStatus_Succeeded, protocol.TaskStatus_Failed, protocol.TaskStatus_Cancelled:
	default:
		return
	}
	ended := time.Now()
	if t.EndedAt != nil {
		ended = *t.EndedAt
	}
	if err := h.Records.FinishRun(taskID, t.Run, ended, t.EndReason); err != nil {
		slog.Warn("record: cannot finish the run's record", "task", taskID, "run", t.Run, "err", err)
	}
}

// oneshotOpen reports whether an open asks for `session new --oneshot`
// (A3, A-D29), carried in the open's second flag byte.
func oneshotOpen(req *protocol.OpenInteractiveRequest) bool {
	ext := req.Ext()
	return req.ExtPresent() && ext != nil && ext.Oneshot()
}

// ptyOpen reports the open's own --pty / --no-pty (A-D33): set is false when
// it says neither, carried in the open's second flag byte.
func ptyOpen(req *protocol.OpenInteractiveRequest) (pty, set bool) {
	ext := req.Ext()
	if !req.ExtPresent() || ext == nil || !ext.PtySet() {
		return false, false
	}
	return ext.Pty(), true
}

// resolveNoPTY is whether the run's child goes without a PTY (A-D33): what
// the open says; else, on an exact resume, what the task recorded — the PTY is
// a terminal-shape setting like -d and the size, which resume_exact lets an
// open change and otherwise keeps; else the mode's default, a PTY for an
// interactive open and none for a --oneshot one. A plain resume reuses
// nothing (A-D7). A stream session never has one, and its kind says so: it is
// stored false there, so the bit only ever qualifies an interactive run.
func resolveNoPTY(req *protocol.OpenInteractiveRequest, oneshot bool, exactRec *TaskEntry) bool {
	if req.EventStream() {
		return false
	}
	if pty, set := ptyOpen(req); set {
		return !pty
	}
	if exactRec != nil {
		return exactRec.NoPTY
	}
	return oneshot
}
