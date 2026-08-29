package server

import (
	"context"
	"encoding/hex"
	"log/slog"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// handleOpenPortForward mirrors handleOpenFileTransfer: it allocates a
// client/runner stream pair, forwards a RunnerOpenPortForward request, and
// splices the two streams. Unlike file transfer it uses spliceBidi
// (tear-down-on-either-close variant) because a TCP forward is not a
// guaranteed both-EOF request/response. The actual net.Dial happens on the
// runner. Local-forward only: -R registration goes through
// handleRegisterPortForward instead.
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
	// The registration these bytes belong to. Refused when it names nothing —
	// 0 included, which no caller in this tree sends and a version-skewed one
	// cannot reach (a short request fails to decode instead). A stream naming
	// no registration would be a forward no listing shows, no counter counts
	// and no `forward kill` can name.
	pf, ok := h.pforwards().get(req.ForwardId)
	if !ok {
		return errResp(protocol.OpenPortForwardStatus_NoSuchForward)
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
		Direction:  protocol.PortForwardDirection_Local,
		RemotePort: req.RemotePort,
	}
	body.SetRemoteHost(req.RemoteHost)
	rreq.SetOpenPortForward(body)
	data := rreq.MustAppend([]byte{byte(appwire.AppKind_RunnerControl)})
	if _, _, err := runner.Conn.SendMessage(data); err != nil {
		_ = clientStream.CloseBoth()
		_ = runnerStream.CloseBoth()
		slog.Error("port_forward: send to runner failed", "task_id", taskIDHex, "err", err)
		return errResp(protocol.OpenPortForwardStatus_InternalError)
	}
	connSeq := pf.openConn()
	pf.tapConnOpen(connSeq, string(req.RemoteHost), req.RemotePort)
	go spliceBidiCounted(clientStream, runnerStream, taskIDHex, pf, connSeq)
	return protocol.OpenPortForwardResponse{
		Status:   protocol.OpenPortForwardStatus_Ok,
		StreamId: uint64(clientStream.ID()),
	}
}

// handleRegisterPortForward registers one forward and returns its id plus the
// server-created control stream. Local registrations never contact the runner:
// the listener is already bound client-side, so only task liveness is checked.
func (h *TaskHandler) handleRegisterPortForward(conn ConnHandle, req *protocol.RegisterPortForwardRequest, cid string) protocol.RegisterPortForwardResponse {
	errResp := func(s protocol.OpenPortForwardStatus) protocol.RegisterPortForwardResponse {
		return protocol.RegisterPortForwardResponse{Status: s}
	}
	taskIDHex := hex.EncodeToString(req.TaskId.Id[:])
	task, ok := h.Tasks.Get(taskIDHex)
	if !ok || (task.Status != protocol.TaskStatus_Running && task.Status != protocol.TaskStatus_Detached) {
		return errResp(protocol.OpenPortForwardStatus_NoSuchTask)
	}
	if conn == nil {
		slog.Error("port_forward: nil client conn (programmer error)")
		return errResp(protocol.OpenPortForwardStatus_InternalError)
	}
	pf := &portForward{
		direction:      req.Direction,
		taskIDHex:      taskIDHex,
		runnerID:       task.AssignedTo,
		clientCxn:      conn,
		clientCID:      cid,
		clientKind:     h.lookupClientKind(cid),
		bindAddr:       string(req.BindAddr),
		bindPort:       req.BindPort,
		targetHost:     string(req.TargetHost),
		targetPort:     req.TargetPort,
		clientEndpoint: req.ClientEndpoint,
	}
	// A runner-side listener whose accepted connections are answered by an
	// in-process handler on the client is a separate design (the browser as a
	// service endpoint). Refuse it rather than letting it half-work: the
	// runner's listener would bind and nothing would ever answer.
	// The PREDICATE, not equality with the bare member: a specifically-named
	// in-process kind would otherwise slip past this refusal and bind a runner
	// listener that nothing would ever answer.
	if req.Direction == protocol.PortForwardDirection_Remote &&
		req.ClientEndpoint.IsInProcess() {
		slog.Warn("port_forward: remote x in_process registration refused (unimplemented combination)",
			"task_id", taskIDHex)
		return errResp(protocol.OpenPortForwardStatus_InternalError)
	}
	if req.Direction == protocol.PortForwardDirection_Remote {
		runner, ok := h.Registry.Get(task.AssignedTo)
		if !ok || runner.Conn == nil {
			return errResp(protocol.OpenPortForwardStatus_RunnerOffline)
		}
		return h.registerRemoteForward(pf, req, runner)
	}
	ctrl := conn.CreateBidirectionalStream()
	if ctrl == nil {
		return errResp(protocol.OpenPortForwardStatus_InternalError)
	}
	pf.control = ctrl
	fid := h.pforwards().add(pf)
	go h.watchRemoteForwardControl(pf)
	return protocol.RegisterPortForwardResponse{
		Status:    protocol.OpenPortForwardStatus_Ok,
		ForwardId: fid,
		StreamId:  uint64(ctrl.ID()),
	}
}

// registerRemoteForward (ssh -R) records the server-created control stream, asks
// the runner to open a listener, and returns the control stream id + assigned
// forwardId. Per-connection data streams are created later in
// handleRemoteForwardConn when the runner reports an accepted connection.
func (h *TaskHandler) registerRemoteForward(pf *portForward, req *protocol.RegisterPortForwardRequest, runner RunnerEntry) protocol.RegisterPortForwardResponse {
	errResp := func(s protocol.OpenPortForwardStatus) protocol.RegisterPortForwardResponse {
		return protocol.RegisterPortForwardResponse{Status: s}
	}
	// The server creates the control stream (matches the codebase pattern:
	// server creates, client picks up by id via WaitForBidirectionalStream).
	ctrl := pf.clientCxn.CreateBidirectionalStream()
	if ctrl == nil {
		return errResp(protocol.OpenPortForwardStatus_InternalError)
	}
	pf.control = ctrl
	fid := h.pforwards().add(pf)
	// Register the pending bind channel BEFORE sending, so a fast runner reply
	// isn't missed.
	resultCh := h.pforwards().addPending(fid)
	defer h.pforwards().removePending(fid)

	rreq := protocol.RunnerRequest{Kind: protocol.RunnerRequestType_OpenPortForward}
	body := protocol.RunnerOpenPortForwardRequest{
		TaskId:    req.TaskId,
		Direction: protocol.PortForwardDirection_Remote,
		BindPort:  req.BindPort,
		ForwardId: fid,
	}
	body.SetBindAddr(req.BindAddr)
	rreq.SetOpenPortForward(body)
	data := rreq.MustAppend([]byte{byte(appwire.AppKind_RunnerControl)})
	if _, _, err := runner.Conn.SendMessage(data); err != nil {
		h.pforwards().remove(fid)
		_ = ctrl.CloseBoth()
		slog.Error("port_forward: send listen request to runner failed", "task_id", pf.taskIDHex, "err", err)
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
		h.pforwards().remove(fid)
		_ = ctrl.CloseBoth()
		// In case the runner DID bind but the result was slow/lost, tell it to
		// stop listening so no orphan listener is left behind.
		sendClosePortForward(runner.Conn, fid)
		return errResp(protocol.OpenPortForwardStatus_BindFailed)
	}

	// Tear the forward down when the client closes the control stream.
	go h.watchRemoteForwardControl(pf)
	return protocol.RegisterPortForwardResponse{
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
	h.pforwards().signalBind(msg.ForwardId, msg.Ok())
}

// sendClosePortForward best-effort tells a runner to stop a remote-forward listener.
func sendClosePortForward(rc ConnHandle, forwardID uint64) {
	if rc == nil {
		return
	}
	rreq := protocol.RunnerRequest{Kind: protocol.RunnerRequestType_ClosePortForward}
	rreq.SetClosePortForward(protocol.ClosePortForwardRequest{ForwardId: forwardID})
	data := rreq.MustAppend([]byte{byte(appwire.AppKind_RunnerControl)})
	_, _, _ = rc.SendMessage(data)
}

// handleRemoteForwardConn fires when a runner reports a new connection accepted
// on a remote-forward listener. It picks up the runner-created data stream,
// allocates a client-side stream, splices the two, and notifies the client over
// the control stream so it dials its local target and picks up the stream by id.
func (h *TaskHandler) handleRemoteForwardConn(runnerConn ConnHandle, msg *protocol.RemoteForwardConn) {
	pf, ok := h.pforwards().get(msg.ForwardId)
	if !ok {
		return // registration gone; the runner stream will EOF and clean up
	}
	runnerStream := peer.WaitForBidirectionalStream(context.Background(), runnerConn, trsf.StreamID(msg.StreamId))
	if runnerStream == nil {
		slog.Info("port_forward: runner data stream not visible", "fwd", msg.ForwardId, "runner_stream", msg.StreamId)
		return
	}
	clientStream := pf.clientCxn.CreateBidirectionalStream()
	if clientStream == nil {
		_ = runnerStream.CloseBoth()
		return
	}
	var ev protocol.PortForwardEvent
	ev.Kind = protocol.PortForwardEventKind_ConnNotify
	ev.SetConnNotify(protocol.RemoteForwardConnNotify{StreamId: uint64(clientStream.ID())})
	nb, err := ev.Append(nil)
	if err != nil {
		_ = clientStream.CloseBoth()
		_ = runnerStream.CloseBoth()
		return
	}
	if err := pf.control.AppendData(false, nb); err != nil {
		_ = clientStream.CloseBoth()
		_ = runnerStream.CloseBoth()
		return
	}
	connSeq := pf.openConn()
	pf.tapConnOpen(connSeq, pf.targetHost, pf.targetPort)
	go spliceBidiCounted(clientStream, runnerStream, pf.taskIDHex, pf, connSeq)
}

// pushPortForwardClosed tells the client to stop this forward. Sent as an
// explicit record — never as a bare stream close — so the client can tell
// "killed" apart from "the server went away" (which arrives as EOF).
func pushPortForwardClosed(pf *portForward, reason protocol.PortForwardCloseReason) {
	if pf == nil || pf.control == nil {
		return
	}
	var ev protocol.PortForwardEvent
	ev.Kind = protocol.PortForwardEventKind_Closed
	ev.SetClosed(protocol.PortForwardClosed{Reason: reason})
	b, err := ev.Append(nil)
	if err != nil {
		slog.Error("port_forward: encode closed event", "fwd", pf.forwardID, "err", err)
		return
	}
	if werr := pf.control.AppendData(false, b); werr != nil {
		slog.Info("port_forward: closed event not delivered", "fwd", pf.forwardID, "err", werr)
		// The registration is already removed by the caller by this point,
		// so a client that never saw the closed record would otherwise keep
		// forwarding through a forward the server no longer knows about.
		// CloseBoth at least gives it EOF so it tears down instead of
		// hanging forever.
		_ = pf.control.CloseBoth()
	}
}

// watchRemoteForwardControl tears the forward down when the client closes the
// control stream. The client never writes on it, so any read returning EOF or
// error means the client is gone: drop the registration and tell the runner to
// stop listening (no orphan listener left behind). Runs for both directions;
// for a local forward the runner was never contacted, so sendClosePortForward
// is only reached (and meaningful) for a remote registration.
func (h *TaskHandler) watchRemoteForwardControl(pf *portForward) {
	for {
		_, eof, err := pf.control.ReadDirect(4096)
		if eof || err != nil {
			break
		}
	}
	// notify=false: the client's own control stream just EOF'd (that's why
	// we're here), so it already knows it hung up — pushing a `closed`
	// record onto a stream the client has already walked away from would be
	// a no-op at best. The reason argument is unused on this path.
	h.teardownPortForward(pf, protocol.PortForwardCloseReason_Killed, false)
}
