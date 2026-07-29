package server

import (
	"encoding/hex"
	"log/slog"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// handleListPortForwards streams the visible port-forward snapshot over a
// server-initiated trsf send-stream. Mirrors handleListConns: the
// TaskControlResponse{ListPortForwards} carries only the stream id, and the
// actual PortForwardListResultBody is encoded onto the stream so the response
// fits in any path MTU.
func (h *TaskHandler) handleListPortForwards(conn ConnHandle, requestID uint32, connID string, filter protocol.TaskID) {
	respond := func(streamID uint64) {
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_ListPortForwards, RequestId: requestID}
		resp.SetListPortForwards(protocol.PortForwardListResult{StreamId: streamID})
		out := resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck
	}

	forwards := h.visiblePortForwards(connID, filter)

	var body protocol.PortForwardListResultBody
	body.SetForwards(forwards)

	bodyBytes, err := body.EncodeCopy(nil)
	if err != nil {
		slog.Error("ListPortForwards: encode body failed", "err", err)
		respond(0)
		return
	}

	stream := conn.CreateSendStream()
	if stream == nil {
		respond(0)
		return
	}
	if werr := stream.AppendData(false, bodyBytes); werr != nil {
		slog.Warn("ListPortForwards: stream write failed", "err", werr)
		respond(0)
		return
	}
	if werr := stream.AppendData(true); werr != nil {
		slog.Warn("ListPortForwards: stream EOF failed", "err", werr)
		respond(0)
		return
	}
	respond(uint64(stream.ID()))
}

// visiblePortForwards returns the registrations connID may see, reaping any whose
// task has left Running/Detached on the way past. Visibility mirrors
// handleListConns: an operator (zero principal) or an InfoGlobal holder sees
// everything; anyone else sees only forwards for tasks in its own subtree.
//
// The reap is deliberately lazy — here, at the single call site — rather than
// hooked into TaskStore.Finish/Cancel/MarkFailed and the runner-loss paths.
// Intercepting a shared transition at some of its call sites is a failure mode
// this repo has already paid for (see implementation-pitfalls Pitfall 3 / 10).
func (h *TaskHandler) visiblePortForwards(connID string, filter protocol.TaskID) []protocol.PortForwardInfo {
	all, allowed := h.visibleToCaller(connID)
	filterHex := ""
	if filter.Id != ([16]byte{}) {
		filterHex = hex.EncodeToString(filter.Id[:])
	}
	var out []protocol.PortForwardInfo
	for _, pf := range h.pforwards().list() {
		task, ok := h.Tasks.Get(pf.taskIDHex)
		if !ok || (task.Status != protocol.TaskStatus_Running && task.Status != protocol.TaskStatus_Detached) {
			h.closePortForward(pf, protocol.PortForwardCloseReason_TaskGone)
			continue
		}
		if !all && !allowed[pf.taskIDHex] {
			continue
		}
		if filterHex != "" && pf.taskIDHex != filterHex {
			continue
		}
		out = append(out, portForwardInfo(pf))
	}
	return out
}

// portForwardInfo converts a registration into its wire form.
func portForwardInfo(pf *portForward) protocol.PortForwardInfo {
	var info protocol.PortForwardInfo
	info.ForwardId = pf.forwardID
	info.Direction = pf.direction
	if b, err := hex.DecodeString(pf.taskIDHex); err == nil && len(b) == 16 {
		copy(info.TaskId.Id[:], b)
	}
	info.SetBindAddr([]byte(pf.bindAddr))
	info.BindPort = pf.bindPort
	info.SetTargetHost([]byte(pf.targetHost))
	info.TargetPort = pf.targetPort
	info.OriginKind = pf.clientKind
	info.SetOriginCid([]byte(pf.clientCID))
	return info
}

// closePortForward drops a registration, tells the client why, and releases the
// runner listener for remote forwards. Reports whether THIS call removed it:
// registry.remove is the single point of arbitration, so two concurrent killers
// cannot both be told they succeeded.
func (h *TaskHandler) closePortForward(pf *portForward, reason protocol.PortForwardCloseReason) bool {
	if _, ok := h.pforwards().remove(pf.forwardID); !ok {
		return false
	}
	pushPortForwardClosed(pf, reason)
	if pf.direction != protocol.PortForwardDirection_Remote {
		return true
	}
	runner, ok := h.Registry.Get(pf.runnerID)
	if !ok || runner.Conn == nil {
		return true
	}
	sendClosePortForward(runner.Conn, pf.forwardID)
	return true
}

// killPortForward closes one registration on request. An id the caller cannot see
// answers no_such_forward, identical to an unknown id: a distinct "denied" would
// let a confined agent probe ids to learn which forwards exist.
func (h *TaskHandler) killPortForward(connID string, id uint64) protocol.KillPortForwardStatus {
	pf, ok := h.pforwards().get(id)
	if !ok {
		return protocol.KillPortForwardStatus_NoSuchForward
	}
	all, allowed := h.visibleToCaller(connID)
	if !all && !allowed[pf.taskIDHex] {
		return protocol.KillPortForwardStatus_NoSuchForward
	}
	if !h.closePortForward(pf, protocol.PortForwardCloseReason_Killed) {
		// Someone else removed it between the get and the remove.
		return protocol.KillPortForwardStatus_NoSuchForward
	}
	return protocol.KillPortForwardStatus_Ok
}
