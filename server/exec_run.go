package server

import (
	"encoding/hex"
	"log/slog"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// handleOpenExecRun starts one out-of-band exec: allocate an id, create the
// data stream toward the client and the runner plus a control stream toward the
// client, tell the runner, register, answer.
//
// Same shape as handleGitQuery, with two differences. An exec needs a SECOND
// stream toward the client, because its exit code cannot ride the exec frame
// protocol — that protocol has no exit shape and deliberately does not grow
// one. And it is registered, because it lives long enough to be listed and
// killed.
func (h *TaskHandler) handleOpenExecRun(conn ConnHandle, req *protocol.ExecRunRequest) protocol.ExecRunResponse {
	errResp := func(s protocol.ExecRunStatus) protocol.ExecRunResponse {
		return protocol.ExecRunResponse{Status: s}
	}
	if req.Argv.ArgvLen == 0 {
		return errResp(protocol.ExecRunStatus_EmptyArgv)
	}
	taskIDHex := hex.EncodeToString(req.TaskId.Id[:])
	task, ok := h.Tasks.Get(taskIDHex)
	if !ok {
		return errResp(protocol.ExecRunStatus_NotFound)
	}
	runner, ok := h.Registry.Get(task.AssignedTo)
	if !ok || runner.Conn == nil {
		return errResp(protocol.ExecRunStatus_RunnerUnreachable)
	}
	if conn == nil {
		slog.Error("exec_run: nil client conn (programmer error)")
		return errResp(protocol.ExecRunStatus_InternalError)
	}

	dataStream := conn.CreateBidirectionalStream()
	if dataStream == nil {
		return errResp(protocol.ExecRunStatus_InternalError)
	}
	ctrlStream := conn.CreateBidirectionalStream()
	if ctrlStream == nil {
		_ = dataStream.CloseBoth()
		return errResp(protocol.ExecRunStatus_InternalError)
	}
	runnerStream := runner.Conn.CreateBidirectionalStream()
	if runnerStream == nil {
		_ = dataStream.CloseBoth()
		_ = ctrlStream.CloseBoth()
		return errResp(protocol.ExecRunStatus_InternalError)
	}

	e := &execRun{
		taskIDHex:  taskIDHex,
		runnerID:   task.AssignedTo,
		argv:       execArgvStrings(req.Argv),
		control:    ctrlStream,
		clientCID:  conn.ConnectionID().String(),
		clientKind: h.lookupClientKind(conn.ConnectionID().String()),
	}
	execID := h.execs().add(e)

	rreq := protocol.RunnerRequest{Kind: protocol.RunnerRequestType_OpenExecRun}
	rreq.SetOpenExecRun(runnerExecRunRequest(req, execID, task.RepoPath, uint64(runnerStream.ID())))
	data := rreq.MustAppend([]byte{byte(appwire.AppKind_RunnerControl)})
	if _, _, err := runner.Conn.SendMessage(data); err != nil {
		h.execs().remove(execID)
		_ = dataStream.CloseBoth()
		_ = ctrlStream.CloseBoth()
		_ = runnerStream.CloseBoth()
		slog.Error("exec_run: send to runner failed", "task_id", taskIDHex, "err", err)
		return errResp(protocol.ExecRunStatus_InternalError)
	}
	go spliceBidi(dataStream, runnerStream, taskIDHex)

	return protocol.ExecRunResponse{
		Status:          protocol.ExecRunStatus_Ok,
		ExecId:          execID,
		DataStreamId:    uint64(dataStream.ID()),
		ControlStreamId: uint64(ctrlStream.ID()),
	}
}

// execArgvStrings flattens the wire argv for the registry's listing.
func execArgvStrings(a protocol.ExecArgv) []string {
	out := make([]string, 0, a.ArgvLen)
	for i := range a.Argv {
		out = append(out, string(a.Argv[i].Arg))
	}
	return out
}

// runnerExecRunRequest relays the client's request to the runner, adding the
// exec id, the stream id and the repo path the server holds.
//
// A separate function so a test can assert EVERY field survives: one relay site
// for a growing struct is the shape that has silently dropped a new field on
// this project before — runnerGitRequest carries the same note for the same
// reason.
func runnerExecRunRequest(req *protocol.ExecRunRequest, execID uint64, repoPath string, streamID uint64) protocol.RunnerExecRunRequest {
	body := protocol.RunnerExecRunRequest{
		ExecId:   execID,
		TaskId:   req.TaskId,
		StreamId: streamID,
		Argv:     req.Argv,
	}
	body.SetRepoPath([]byte(repoPath))
	body.SetStdinEnabled(req.StdinEnabled())
	return body
}

// onExecRunFinished delivers the outcome to the waiting client and
// deregisters. An unknown id is a no-op: the client may have gone away first,
// taking the registration with it.
func (h *TaskHandler) onExecRunFinished(fin *protocol.ExecRunFinished) {
	e, ok := h.execs().remove(fin.ExecId)
	if !ok {
		return
	}
	writeExecEvent(e, fin.Kind, fin.ExitCode, string(fin.Detail))
}

// writeExecEvent puts the one ExecEvent on the control stream and closes it.
// The close is what tells a reading client the outcome is complete; there is
// never a second record.
func writeExecEvent(e *execRun, kind protocol.ExecEventKind, code int32, detail string) {
	ev := protocol.ExecEvent{Kind: kind, ExitCode: code}
	ev.Detail = []byte(detail)
	body, err := ev.Append(nil)
	if err != nil {
		slog.Error("exec_run: encode event", "exec_id", e.execID, "err", err)
		_ = e.control.CloseBoth()
		return
	}
	if _, werr := e.control.Write(body); werr != nil {
		slog.Info("exec_run: client gone before the outcome landed", "exec_id", e.execID)
	}
	_ = e.control.CloseBoth()
}

// handleExecRunList reports the running execs the caller may see.
//
// No capability is required and the bound is task VISIBILITY, following
// ListPortForwards — and for the reason await_idle needs no cap either: gating
// a fact `ls` already hands out would make the direct path cost more authority
// than polling for the same thing.
func (h *TaskHandler) handleExecRunList(connID string, req *protocol.ExecRunListRequest) protocol.ExecRunListResponse {
	filter := ""
	if req.TaskFilter.Id != ([16]byte{}) {
		filter = hex.EncodeToString(req.TaskFilter.Id[:])
	}
	all, allowed := h.visibleToCaller(connID)
	var out protocol.ExecRunListResponse
	for _, e := range h.execs().list(filter) {
		if !all && !allowed[e.taskIDHex] {
			continue
		}
		out.Execs = append(out.Execs, execRunInfo(e))
	}
	out.ExecsLen = uint16(len(out.Execs))
	return out
}

// execRunInfo renders one registration for the listing.
func execRunInfo(e *execRun) protocol.ExecRunInfo {
	info := protocol.ExecRunInfo{
		ExecId:        e.execID,
		StartedUnixMs: uint64(e.startedAt.UnixMilli()),
		OriginKind:    e.clientKind,
	}
	if raw, err := hex.DecodeString(e.taskIDHex); err == nil && len(raw) == 16 {
		copy(info.TaskId.Id[:], raw)
	}
	var argv protocol.ExecArgv
	for _, a := range e.argv {
		var one protocol.ExecArg
		one.SetArg([]byte(a))
		argv.Argv = append(argv.Argv, one)
	}
	argv.ArgvLen = uint16(len(argv.Argv))
	info.Argv = argv
	info.SetOriginCid([]byte(e.clientCID))
	return info
}

// handleExecRunKill stops one.
//
// Gated on exec_run against the TARGET TASK rather than on who started it: the
// bit that authorizes running commands in a tree authorizes stopping them, and
// requiring `cancel` instead would leave a holder able to start what it cannot
// stop. An out-of-scope target is reported as absent, because answering
// "denied" would confirm that an exec the caller may not see exists.
func (h *TaskHandler) handleExecRunKill(connID string, req *protocol.ExecRunKillRequest) protocol.ExecRunKillResponse {
	e, ok := h.execs().get(req.ExecId)
	if !ok {
		return protocol.ExecRunKillResponse{Status: protocol.ExecRunStatus_NotFound}
	}
	if !h.authorize(connID, protocol.Capability_ExecRun, e.taskIDHex) {
		return protocol.ExecRunKillResponse{Status: protocol.ExecRunStatus_NotFound}
	}
	if _, still := h.execs().remove(req.ExecId); !still {
		return protocol.ExecRunKillResponse{Status: protocol.ExecRunStatus_NotFound}
	}
	// Tell the runner to cancel the child. This needs its own request:
	// closing the exec's stream does NOT stop the process — the frame pump
	// reads EOF and ends its input goroutine while the child runs on. Same
	// reason ClosePortForward exists rather than relying on a stream close.
	if runner, ok := h.Registry.Get(e.runnerID); ok && runner.Conn != nil {
		rreq := protocol.RunnerRequest{Kind: protocol.RunnerRequestType_CloseExecRun}
		rreq.SetCloseExecRun(protocol.CloseExecRunRequest{ExecId: req.ExecId})
		data := rreq.MustAppend([]byte{byte(appwire.AppKind_RunnerControl)})
		if _, _, err := runner.Conn.SendMessage(data); err != nil {
			slog.Error("exec_run: close request to runner failed", "exec_id", req.ExecId, "err", err)
		}
	}
	// The client is told regardless: it asked for a stop and the registration
	// is gone either way, so leaving it waiting on a runner that may never
	// answer would be the worse failure.
	writeExecEvent(e, protocol.ExecEventKind_Killed, -1, "killed by exec kill")
	return protocol.ExecRunKillResponse{Status: protocol.ExecRunStatus_Ok}
}
