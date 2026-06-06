package server

import (
	"context"
	"encoding/hex"
	"log/slog"
	"time"

	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/trsf"
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
	if req.Direction == protocol.PortForwardDirection_Remote {
		return h.registerRemoteForward(conn, req, taskIDHex, task.AssignedTo, runner)
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

// registerRemoteForward (ssh -R) records the server-created control stream, asks
// the runner to open a listener, and returns the control stream id + assigned
// forwardId. Per-connection data streams are created later in
// handleRemoteForwardConn when the runner reports an accepted connection.
func (h *TaskHandler) registerRemoteForward(conn ConnHandle, req *protocol.OpenPortForwardRequest, taskIDHex, runnerID string, runner RunnerEntry) protocol.OpenPortForwardResponse {
	errResp := func(s protocol.OpenPortForwardStatus) protocol.OpenPortForwardResponse {
		return protocol.OpenPortForwardResponse{Status: s}
	}
	// The server creates the control stream (matches the codebase pattern:
	// server creates, client picks up by id via WaitForBidirectionalStream).
	ctrl := conn.CreateBidirectionalStream()
	if ctrl == nil {
		return errResp(protocol.OpenPortForwardStatus_InternalError)
	}
	rf := &remoteForward{taskIDHex: taskIDHex, runnerID: runnerID, control: ctrl, clientCxn: conn}
	fid := h.rforwards().add(rf)
	// Register the pending bind channel BEFORE sending, so a fast runner reply
	// isn't missed.
	resultCh := h.rforwards().addPending(fid)
	defer h.rforwards().removePending(fid)

	rreq := protocol.RunnerRequest{Kind: protocol.RunnerRequestType_OpenPortForward}
	body := protocol.RunnerOpenPortForwardRequest{
		TaskId:    req.TaskId,
		Direction: protocol.PortForwardDirection_Remote,
		BindPort:  req.BindPort,
		ForwardId: fid,
	}
	body.SetBindAddr(req.BindAddr)
	rreq.SetOpenPortForward(body)
	data := rreq.MustAppend([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)})
	if _, _, err := runner.Conn.SendMessage(data); err != nil {
		h.rforwards().remove(fid)
		_ = ctrl.CloseBoth()
		slog.Error("port_forward: send listen request to runner failed", "task_id", taskIDHex, "err", err)
		return errResp(protocol.OpenPortForwardStatus_InternalError)
	}

	// Wait for the runner to report whether the listener bound, so the client
	// learns success/failure instead of a silent no-op (e.g. port already in use).
	var bound bool
	select {
	case bound = <-resultCh:
	case <-time.After(remoteForwardBindTimeout):
	}
	if !bound {
		h.rforwards().remove(fid)
		_ = ctrl.CloseBoth()
		// In case the runner DID bind but the result was slow/lost, tell it to
		// stop listening so no orphan listener is left behind.
		sendClosePortForward(runner.Conn, fid)
		return errResp(protocol.OpenPortForwardStatus_BindFailed)
	}

	// Tear the forward down when the client closes the control stream.
	go h.watchRemoteForwardControl(rf)
	return protocol.OpenPortForwardResponse{
		Status:    protocol.OpenPortForwardStatus_Ok,
		StreamId:  uint64(ctrl.ID()),
		ForwardId: fid,
	}
}

// remoteForwardBindTimeout bounds how long registration waits for the runner's
// bind result before giving up with BindFailed.
const remoteForwardBindTimeout = 5 * time.Second

// handleRemoteForwardBindResult delivers a runner's listener-bind result to the
// registration goroutine blocked in registerRemoteForward.
func (h *TaskHandler) handleRemoteForwardBindResult(_ ConnHandle, msg *protocol.RemoteForwardBindResult) {
	h.rforwards().signalBind(msg.ForwardId, msg.Ok())
}

// sendClosePortForward best-effort tells a runner to stop a remote-forward listener.
func sendClosePortForward(rc ConnHandle, forwardID uint64) {
	if rc == nil {
		return
	}
	rreq := protocol.RunnerRequest{Kind: protocol.RunnerRequestType_ClosePortForward}
	rreq.SetClosePortForward(protocol.ClosePortForwardRequest{ForwardId: forwardID})
	data := rreq.MustAppend([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)})
	_, _, _ = rc.SendMessage(data)
}

// handleRemoteForwardConn fires when a runner reports a new connection accepted
// on a remote-forward listener. It picks up the runner-created data stream,
// allocates a client-side stream, splices the two, and notifies the client over
// the control stream so it dials its local target and picks up the stream by id.
func (h *TaskHandler) handleRemoteForwardConn(runnerConn ConnHandle, msg *protocol.RemoteForwardConn) {
	rf, ok := h.rforwards().get(msg.ForwardId)
	if !ok {
		return // registration gone; the runner stream will EOF and clean up
	}
	runnerStream := peer.WaitForBidirectionalStream(context.Background(), runnerConn, trsf.StreamID(msg.StreamId))
	if runnerStream == nil {
		return
	}
	clientStream := rf.clientCxn.CreateBidirectionalStream()
	if clientStream == nil {
		_ = runnerStream.CloseBoth()
		return
	}
	notify := protocol.RemoteForwardConnNotify{StreamId: uint64(clientStream.ID())}
	nb, err := notify.Append(nil)
	if err != nil {
		_ = clientStream.CloseBoth()
		_ = runnerStream.CloseBoth()
		return
	}
	if err := rf.control.AppendData(false, nb); err != nil {
		_ = clientStream.CloseBoth()
		_ = runnerStream.CloseBoth()
		return
	}
	go spliceBidi(clientStream, runnerStream, rf.taskIDHex)
}

// watchRemoteForwardControl tears the forward down when the client closes the
// control stream. The client never writes on it, so any read returning EOF or
// error means the client is gone: drop the registration and tell the runner to
// stop listening (no orphan listener left behind).
func (h *TaskHandler) watchRemoteForwardControl(rf *remoteForward) {
	for {
		_, eof, err := rf.control.ReadDirect(4096)
		if eof || err != nil {
			break
		}
	}
	if _, ok := h.rforwards().remove(rf.forwardID); !ok {
		return
	}
	runner, ok := h.Registry.Get(rf.runnerID)
	if !ok || runner.Conn == nil {
		return
	}
	sendClosePortForward(runner.Conn, rf.forwardID)
}
