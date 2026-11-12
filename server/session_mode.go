package server

import (
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// handleSessionMode switches a running session task between foreground and
// background (pillar A spec, A-D19). The cancel bit is checked centrally
// (requiredCap); the target is gated here, as the cancel handler gates it: an
// out-of-scope task answers not_found. A task may not switch itself to
// foreground — that would remove its own deadline.
func (h *TaskHandler) handleSessionMode(cid string, conn ConnHandle, req *protocol.TaskControlRequest) {
	respond := func(st protocol.SessionModeStatus, msg string) {
		r := protocol.SessionModeResponse{Status: st}
		if msg != "" {
			r.SetErrorMsg([]byte(msg))
		}
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_SessionMode, RequestId: req.Header.RequestId}
		resp.SetSessionMode(r)
		conn.SendMessage(resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})) //nolint:errcheck
	}
	sm := req.SessionMode()
	if sm == nil {
		respond(protocol.SessionModeStatus_InternalError, "session_mode: empty request")
		return
	}
	taskID := hex.EncodeToString(sm.TaskId.Id[:])
	if !h.inScope(cid, protocol.Capability_Cancel, taskID) {
		respond(protocol.SessionModeStatus_NotFound, "")
		return
	}
	t, ok := h.Tasks.Get(taskID)
	if !ok {
		respond(protocol.SessionModeStatus_NotFound, "")
		return
	}

	mode := protocol.RunMode_Foreground
	var cond RunConditions
	if sm.Background() {
		mode = protocol.RunMode_Background
		wc := sm.Conditions()
		if !sm.ConditionsPresent() || wc == nil || wc.DeadlineMs == 0 {
			respond(protocol.SessionModeStatus_DeadlineRequired, "a switch to background must state its deadline")
			return
		}
		if t.Kind == protocol.TaskKind_Stream && (wc.TurnEnd == protocol.TurnEndSignal_Quiet || wc.TurnEnd == protocol.TurnEndSignal_Program) {
			respond(protocol.SessionModeStatus_TurnEndUnsupported,
				fmt.Sprintf("turn end %v is not available on a stream session: it reports lifecycle only", wc.TurnEnd))
			return
		}
		cond = ConditionsFromWire(wc).WithDefaults(t.Kind)
		cond.Rows, cond.Cols = t.Cond.Rows, t.Cond.Cols
	} else if self := h.lookupPrincipal(cid); self == sm.TaskId && self.Id != ([16]byte{}) {
		respond(protocol.SessionModeStatus_SelfForegroundRefused, "a task may not switch itself to foreground")
		return
	}

	if _, err := h.Tasks.SwitchRunMode(taskID, mode, cond, time.Now().UnixNano()); err != nil {
		switch {
		case errors.Is(err, ErrSwitchQueued):
			respond(protocol.SessionModeStatus_Queued, "the task is queued: cancel it and resume it in the other mode")
		case errors.Is(err, ErrSwitchNotFound):
			respond(protocol.SessionModeStatus_NotFound, "")
		case errors.Is(err, ErrSwitchNotSession):
			respond(protocol.SessionModeStatus_NotSession, "a oneshot task has no session to switch")
		case errors.Is(err, ErrSwitchNotActive):
			respond(protocol.SessionModeStatus_NotSession, "the task is not running")
		default:
			respond(protocol.SessionModeStatus_InternalError, err.Error())
		}
		return
	}
	if mode == protocol.RunMode_Background {
		if mux := h.sessionMux(taskID); mux != nil {
			h.armRunWatcher(taskID, mux)
		}
	} else {
		h.disarmRunWatcher(taskID)
	}
	respond(protocol.SessionModeStatus_Ok, "")
	if h.OnChange != nil {
		h.OnChange()
	}
}
