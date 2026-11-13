package server

import (
	"encoding/hex"
	"log/slog"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// handleRecord answers the five PTY-record verbs (pillar A spec, A1). Every
// verb names a task and is gated on the bit it needs within that bit's own
// scope — so an override's exclude_self keeps a task off its own record
// (A-D22, A-D26). A missing bit answers PermissionDenied; a task out of
// scope answers not_found, as GetTaskLog does.
func (h *TaskHandler) handleRecord(cid string, conn ConnHandle, req *protocol.TaskControlRequest) {
	resp := protocol.TaskControlResponse{Kind: req.Header.Kind, RequestId: req.Header.RequestId}
	send := func() {
		conn.SendMessage(resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})) //nolint:errcheck
	}
	status := func(st protocol.RecordStatus) {
		switch req.Header.Kind {
		case protocol.TaskControlKind_RecordList:
			resp.SetRecordList(protocol.RecordListResponse{Status: st})
		case protocol.TaskControlKind_RecordGet:
			resp.SetRecordGet(protocol.RecordGetResponse{Status: st})
		case protocol.TaskControlKind_RecordDelete:
			resp.SetRecordDelete(protocol.RecordDeleteResponse{Status: st})
		case protocol.TaskControlKind_RecordKeep:
			resp.SetRecordKeep(protocol.RecordKeepResponse{Status: st})
		case protocol.TaskControlKind_RecordSwitch:
			resp.SetRecordSwitch(protocol.RecordSwitchResponse{Status: st})
		default:
			return
		}
		send()
	}
	// gate checks recordBit on the target; false = already answered.
	gate := func(recordBit protocol.Capability, idHex string) bool {
		if !hasCap(h.callerCaps(cid), recordBit) {
			h.denyTaskControl(conn, req.Header.Kind, req.Header.RequestId, recordBit)
			return false
		}
		if !h.inScope(cid, recordBit, idHex) {
			status(protocol.RecordStatus_NotFound)
			return false
		}
		return true
	}
	if h.Records == nil {
		// No data dir: there are no records, and none can be made.
		if req.Header.Kind == protocol.TaskControlKind_RecordList {
			h.respondRecordList(conn, &resp, nil)
			return
		}
		status(protocol.RecordStatus_NotFound)
		return
	}

	switch req.Header.Kind {
	case protocol.TaskControlKind_RecordList:
		r := req.RecordList()
		if r == nil {
			status(protocol.RecordStatus_InternalError)
			return
		}
		task := ""
		if r.TaskPresent() && r.TaskId() != nil {
			task = hex.EncodeToString(r.TaskId().Id[:])
			if !gate(protocol.Capability_ExecView, task) {
				return
			}
		}
		all, allowed := h.scopeSet(cid, protocol.Capability_ExecView)
		var out []protocol.RecordInfo
		for _, info := range h.Records.List(task) {
			id := hex.EncodeToString(info.TaskId.Id[:])
			if !all && !allowed[id] {
				continue
			}
			out = append(out, h.withRunEnd(id, info))
		}
		h.respondRecordList(conn, &resp, out)

	case protocol.TaskControlKind_RecordGet:
		r := req.RecordGet()
		if r == nil {
			status(protocol.RecordStatus_InternalError)
			return
		}
		task := hex.EncodeToString(r.Ref.TaskId.Id[:])
		if !gate(protocol.Capability_ExecView, task) {
			return
		}
		if _, _, ok := h.Records.Retention(task, r.Ref.Run); !ok {
			status(protocol.RecordStatus_NotFound)
			return
		}
		stream := conn.CreateSendStream()
		if stream == nil {
			status(protocol.RecordStatus_InternalError)
			return
		}
		resp.SetRecordGet(protocol.RecordGetResponse{Status: protocol.RecordStatus_Ok, StreamId: uint64(stream.ID())})
		send()
		run := r.Ref.Run
		go func() {
			err := h.Records.Read(task, run, func(e protocol.RecordEntry) error {
				return writeStreamAll(stream, e.MustAppend(nil))
			})
			if err != nil {
				// The stream has no reset: a failed read ends like a complete
				// one, so it is at least logged (wire review M3, deferred).
				slog.Warn("record get: read failed", "task", task, "run", run, "err", err)
			}
			_ = stream.AppendData(true)
		}()

	case protocol.TaskControlKind_RecordDelete:
		r := req.RecordDelete()
		if r == nil {
			status(protocol.RecordStatus_InternalError)
			return
		}
		task := hex.EncodeToString(r.Ref.TaskId.Id[:])
		if !gate(protocol.Capability_RecordDelete, task) {
			return
		}
		run := r.Ref.Run
		if r.AllRuns() {
			run = 0
		} else if run == 0 {
			if run = h.latestRecordedRun(task); run == 0 {
				status(protocol.RecordStatus_NotFound)
				return
			}
		}
		// Deleting the run being recorded stops recording it (and clears the
		// task's switch, so a re-adoption does not start again): the record
		// is gone, and a writer left on it would be inert but still pinned.
		if t, have := h.Tasks.Get(task); have && (run == 0 || run == t.Run) {
			if mux := h.sessionMux(task); mux != nil && mux.Recording() {
				mux.StopRecording()
			}
			if t.Record {
				_ = h.Tasks.SetRecord(task, false)
			}
		}
		ok, err := h.Records.Delete(task, run)
		switch {
		case err != nil:
			slog.Warn("record delete", "task", task, "err", err)
			status(protocol.RecordStatus_InternalError)
		case !ok:
			status(protocol.RecordStatus_NotFound)
		default:
			h.notifyRecording(task)
			status(protocol.RecordStatus_Ok)
		}

	case protocol.TaskControlKind_RecordKeep:
		r := req.RecordKeep()
		if r == nil {
			status(protocol.RecordStatus_InternalError)
			return
		}
		task := hex.EncodeToString(r.Ref.TaskId.Id[:])
		caps := h.callerCaps(cid)
		if !hasAnyCap(caps, protocol.Capability_RecordPersist|protocol.Capability_RecordDelete) {
			h.denyTaskControl(conn, req.Header.Kind, req.Header.RequestId, protocol.Capability_RecordPersist)
			return
		}
		// The direction needs the record's current retention, so the caller
		// must be able to act on the task with a bit it holds before the
		// record is looked at — otherwise which denial comes back tells an
		// out-of-scope caller whether a record exists and how long it is kept.
		reach := false
		for _, b := range []protocol.Capability{protocol.Capability_RecordPersist, protocol.Capability_RecordDelete} {
			if hasCap(caps, b) && h.inScope(cid, b, task) {
				reach = true
			}
		}
		if !reach {
			status(protocol.RecordStatus_NotFound)
			return
		}
		curForever, curTTL, ok := h.Records.Retention(task, r.Ref.Run)
		if !ok {
			status(protocol.RecordStatus_NotFound)
			return
		}
		// A later expiry keeps (record_persist); an earlier one deletes sooner
		// (record_delete). ttl 0 and not forever expires at the run's end.
		ttl := time.Duration(r.TtlMs) * time.Millisecond
		recordBit := protocol.Capability_RecordPersist
		if !r.Forever() && (curForever || ttl < curTTL) {
			recordBit = protocol.Capability_RecordDelete
		}
		if !gate(recordBit, task) {
			return
		}
		if err := h.Records.SetRetention(task, r.Ref.Run, r.Forever(), ttl, time.Now()); err != nil {
			status(protocol.RecordStatus_NotFound)
			return
		}
		status(protocol.RecordStatus_Ok)

	case protocol.TaskControlKind_RecordSwitch:
		r := req.RecordSwitch()
		if r == nil {
			status(protocol.RecordStatus_InternalError)
			return
		}
		task := hex.EncodeToString(r.TaskId.Id[:])
		recordBit := protocol.Capability_RecordDelete
		if r.On() {
			recordBit = protocol.Capability_RecordPersist
		}
		if !gate(recordBit, task) {
			return
		}
		t, ok := h.Tasks.Get(task)
		mux := h.sessionMux(task)
		if !ok || mux == nil || !mux.HasTerminal() {
			status(protocol.RecordStatus_NotRecordable)
			return
		}
		if !r.On() {
			mux.StopRecording()
			_ = h.Tasks.SetRecord(task, false)
			h.notifyRecording(task)
			status(protocol.RecordStatus_Ok)
			return
		}
		if mux.Recording() {
			status(protocol.RecordStatus_Ok)
			return
		}
		how := recordFromRing
		if _, _, had := h.Records.Retention(task, t.Run); had {
			how = recordAgain
		}
		w, err := h.Records.OpenRun(task, t.Run, time.Now())
		if err != nil {
			slog.Warn("record switch on", "task", task, "err", err)
			status(protocol.RecordStatus_InternalError)
			return
		}
		if err := mux.StartRecording(w, how); err != nil {
			_ = w.Detach()
			status(protocol.RecordStatus_NotRecordable)
			return
		}
		_ = h.Tasks.SetRecord(task, true)
		h.notifyRecording(task)
		status(protocol.RecordStatus_Ok)
	}
}

// respondRecordList answers a list with its records on a stream.
func (h *TaskHandler) respondRecordList(conn ConnHandle, resp *protocol.TaskControlResponse, records []protocol.RecordInfo) {
	body := &protocol.RecordListBody{}
	body.SetRecords(records)
	respondWithBody(conn, "record_list", body, func(streamID uint64) {
		st := protocol.RecordStatus_Ok
		if streamID == 0 {
			st = protocol.RecordStatus_InternalError
		}
		resp.SetRecordList(protocol.RecordListResponse{Status: st, StreamId: streamID})
		conn.SendMessage(resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})) //nolint:errcheck
	})
}

// withRunEnd fills a record's end reason from its task when the run is the
// task's current one and its record has none yet (the run ended, but no
// FinishRun has reached the record).
func (h *TaskHandler) withRunEnd(task string, info protocol.RecordInfo) protocol.RecordInfo {
	if info.EndReason != protocol.EndReason_None {
		return info
	}
	if t, ok := h.Tasks.Get(task); ok && t.Run == info.Run {
		info.EndReason = t.EndReason
	}
	return info
}

// latestRecordedRun is the task's newest run with a record, 0 = none.
func (h *TaskHandler) latestRecordedRun(task string) uint32 {
	var run uint32
	for _, info := range h.Records.List(task) {
		if info.Run > run {
			run = info.Run
		}
	}
	return run
}

// notifyRecording tells event-driven clients a task's recording changed: a
// switch moves no task status, so without an event the row would stay stale.
// The observers event exists for the same reason and carries the bit.
func (h *TaskHandler) notifyRecording(task string) {
	if h.OnSessionObservers != nil {
		h.OnSessionObservers(task)
	}
}
