package server

import (
	"encoding/hex"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// handleRecord answers the five PTY-record verbs (pillar A spec, A1). The
// target is gated here on the recordBit the request needs, within that recordBit's own
// scope (so an override's exclude_self holds); out of scope answers
// not_found, as GetTaskLog does. Until the record store is wired, an
// in-scope request answers internal_error.
func (h *TaskHandler) handleRecord(cid string, conn ConnHandle, req *protocol.TaskControlRequest) {
	resp := protocol.TaskControlResponse{Kind: req.Header.Kind, RequestId: req.Header.RequestId}
	answer := func(st protocol.RecordStatus) {
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
		conn.SendMessage(resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})) //nolint:errcheck
	}
	var target protocol.TaskID
	var recordBit protocol.Capability
	switch req.Header.Kind {
	case protocol.TaskControlKind_RecordList:
		if r := req.RecordList(); r != nil && r.TaskPresent() {
			if id := r.TaskId(); id != nil {
				target, recordBit = *id, protocol.Capability_ExecView
			}
		}
	case protocol.TaskControlKind_RecordGet:
		if r := req.RecordGet(); r != nil {
			target, recordBit = r.Ref.TaskId, protocol.Capability_ExecView
		}
	case protocol.TaskControlKind_RecordDelete:
		if r := req.RecordDelete(); r != nil {
			target, recordBit = r.Ref.TaskId, protocol.Capability_RecordDelete
		}
	case protocol.TaskControlKind_RecordKeep:
		if r := req.RecordKeep(); r != nil {
			target, recordBit = r.Ref.TaskId, protocol.Capability_RecordDelete
			if r.Forever() {
				recordBit = protocol.Capability_RecordPersist
			}
		}
	case protocol.TaskControlKind_RecordSwitch:
		if r := req.RecordSwitch(); r != nil {
			target, recordBit = r.TaskId, protocol.Capability_RecordDelete
			if r.On() {
				recordBit = protocol.Capability_RecordPersist
			}
		}
	}
	if recordBit != 0 {
		idHex := hex.EncodeToString(target.Id[:])
		if !hasCap(h.callerCaps(cid), recordBit) {
			h.denyTaskControl(conn, req.Header.Kind, req.Header.RequestId, recordBit)
			return
		}
		if !h.inScope(cid, recordBit, idHex) {
			answer(protocol.RecordStatus_NotFound)
			return
		}
	}
	answer(protocol.RecordStatus_InternalError)
}
