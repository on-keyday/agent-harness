package server

import (
	"encoding/hex"
	"log/slog"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// handleOpenPortForward mirrors handleOpenFileTransfer: it allocates a
// client/runner stream pair, forwards a RunnerOpenPortForward request, and
// splices the two streams. Unlike file transfer it uses spliceBidi
// (tear-down-on-either-close variant) because a TCP forward is not a
// guaranteed both-EOF request/response. The actual net.Dial happens on the
// runner.
func (h *TaskHandler) handleOpenPortForward(conn ConnHandle, req *protocol.OpenPortForwardRequest) protocol.OpenPortForwardResponse {
	errResp := func(s protocol.OpenPortForwardStatus) protocol.OpenPortForwardResponse {
		return protocol.OpenPortForwardResponse{Status: s}
	}
	taskIDHex := hex.EncodeToString(req.TaskId.Id[:])
	task, ok := h.Tasks.Get(taskIDHex)
	if !ok || (task.Status != protocol.TaskStatus_Running && task.Status != protocol.TaskStatus_Detached) {
		return errResp(protocol.OpenPortForwardStatus_NoSuchTask)
	}
	runner, ok := h.Registry.Get(task.AssignedTo)
	if !ok || runner.Conn == nil {
		return errResp(protocol.OpenPortForwardStatus_RunnerOffline)
	}
	if conn == nil {
		slog.Error("port_forward: nil client conn (programmer error)")
		return errResp(protocol.OpenPortForwardStatus_InternalError)
	}
	clientStream := conn.CreateBidirectionalStream()
	if clientStream == nil {
		return errResp(protocol.OpenPortForwardStatus_InternalError)
	}
	runnerStream := runner.Conn.CreateBidirectionalStream()
	if runnerStream == nil {
		_ = clientStream.CloseBoth()
		return errResp(protocol.OpenPortForwardStatus_InternalError)
	}

	rreq := protocol.RunnerRequest{Kind: protocol.RunnerRequestType_OpenPortForward}
	body := protocol.RunnerOpenPortForwardRequest{
		TaskId:     req.TaskId,
		StreamId:   uint64(runnerStream.ID()),
		Direction:  req.Direction,
		RemotePort: req.RemotePort,
	}
	body.SetRemoteHost(req.RemoteHost)
	rreq.SetOpenPortForward(body)
	data := rreq.MustAppend([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)})
	if _, _, err := runner.Conn.SendMessage(data); err != nil {
		_ = clientStream.CloseBoth()
		_ = runnerStream.CloseBoth()
		slog.Error("port_forward: send to runner failed", "task_id", taskIDHex, "err", err)
		return errResp(protocol.OpenPortForwardStatus_InternalError)
	}
	go spliceBidi(clientStream, runnerStream, taskIDHex)
	return protocol.OpenPortForwardResponse{
		Status:   protocol.OpenPortForwardStatus_Ok,
		StreamId: uint64(clientStream.ID()),
	}
}
