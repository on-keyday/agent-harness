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
	// A SEND stream, like the ExecRunList body's: the outcome only ever flows
	// server to client, and asking for a bidirectional one would be actively
	// wrong here. Finishing a bidi stream means CloseBoth, whose recv half
	// cancels the PEER's send half of the same id — and GetBidirectionalStream
	// resolves an id only while BOTH halves exist, so the client would lose the
	// ability to look this stream up seconds before it reads it. That raced and
	// was reproduced in trsf directly (TestCloseBothKeepsTheStreamResolvableByID).
	// A unidirectional stream has no peer send half to cancel.
	//
	// It carries nothing until the exec ends, so the client deliberately does
	// not look for it until the output is over. See (*Client).ExecRun.
	ctrlStream := conn.CreateSendStream()
	if ctrlStream == nil {
		_ = dataStream.CloseBoth()
		return errResp(protocol.ExecRunStatus_InternalError)
	}
	runnerStream := runner.Conn.CreateBidirectionalStream()
	if runnerStream == nil {
		_ = dataStream.CloseBoth()
		_ = ctrlStream.Close()
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

	// The child runs in the task's worktree AS that task, so it carries the
	// task's own ticket. Looked up, never reissued: Register overwrites, so a
	// fresh ticket here would invalidate the credential the running agent is
	// holding. A task with no registered ticket — one whose agent has ended,
	// which exec still serves as long as the worktree is there — yields the
	// zero value, and BuildAgentEnv omits the variable rather than advertising
	// a credential that cannot work.
	var ticket [16]byte
	if h.Board != nil {
		if t, ok := h.Board.Registry().Ticket(runnerIDFromConnID(runner.ID), req.TaskId); ok {
			ticket = t
		}
	}

	rreq := protocol.RunnerRequest{Kind: protocol.RunnerRequestType_OpenExecRun}
	rreq.SetOpenExecRun(runnerExecRunRequest(req, execID, task.RepoPath, uint64(runnerStream.ID()), ticket))
	data := rreq.MustAppend([]byte{byte(appwire.AppKind_RunnerControl)})
	if _, _, err := runner.Conn.SendMessage(data); err != nil {
		h.removeExec(execID)
		_ = dataStream.CloseBoth()
		_ = ctrlStream.Close()
		_ = runnerStream.CloseBoth()
		slog.Error("exec_run: send to runner failed", "task_id", taskIDHex, "err", err)
		return errResp(protocol.ExecRunStatus_InternalError)
	}
	// Announced only once the runner has been told, so a send failure does not
	// publish an exec that never started — the same rule the remote forward's
	// registered event follows around its bind result.
	h.emitExecEvent(protocol.StatusEventKind_ExecStarted, e)

	// HalfClose, not spliceBidi: the full-teardown variant closes BOTH streams
	// the moment either direction ends, and for a command that finishes in
	// milliseconds that can tear the client's data stream down before the
	// client has resolved it by id. git_query and the file transfers splice the
	// same way for the same reason — a request/response exchange, not an
	// interactive PTY where a dead direction should end everything.
	go spliceBidiHalfClose(dataStream, runnerStream, taskIDHex)

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
//
// That guard did not catch auth_ticket, and the reason is worth keeping: the
// ticket is not RELAYED from the client's request, it is supplied by the
// server, so a test asking "did every field of req survive?" cannot see it
// missing. It is a parameter rather than something read off req for exactly
// that reason — a caller has to pass it, and one that forgets is a compile
// error rather than a zero.
func runnerExecRunRequest(req *protocol.ExecRunRequest, execID uint64, repoPath string, streamID uint64, authTicket [16]byte) protocol.RunnerExecRunRequest {
	body := protocol.RunnerExecRunRequest{
		ExecId:     execID,
		TaskId:     req.TaskId,
		StreamId:   streamID,
		Argv:       req.Argv,
		AuthTicket: authTicket,
	}
	body.SetRepoPath([]byte(repoPath))
	body.SetShellLine(req.ShellLine())
	body.SetSshdParent(req.SshdParent())
	body.SetStdinEnabled(req.StdinEnabled())
	return body
}

// onExecRunFinished delivers the outcome to the waiting client and
// deregisters. An unknown id is a no-op: the client may have gone away first,
// taking the registration with it.
func (h *TaskHandler) onExecRunFinished(fin *protocol.ExecRunFinished) {
	e, ok := h.removeExec(fin.ExecId)
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
		// EOF anyway: a client blocked on this stream would otherwise wait out
		// its whole deadline for bytes that are never coming.
		_ = e.control.AppendData(true)
		return
	}
	// Body then EOF, exactly as handleExecRunList writes its rows. NOT Close():
	// the EOF frame IS the end-of-outcome signal, and a separate close would
	// only add the cancel this stream shape exists to avoid.
	if werr := e.control.AppendData(false, body); werr != nil {
		slog.Info("exec_run: client gone before the outcome landed", "exec_id", e.execID)
	}
	if werr := e.control.AppendData(true); werr != nil {
		slog.Info("exec_run: client gone before the outcome EOF landed", "exec_id", e.execID)
	}
}

// handleExecRunList reports the running execs the caller may see.
//
// No capability is required and the bound is task VISIBILITY, following
// ListPortForwards — and for the reason await_idle needs no cap either: gating
// a fact `ls` already hands out would make the direct path cost more authority
// than polling for the same thing.
// The rows travel on their own send stream rather than inline, as
// ListPortForwards' and ls' do: a listing whose rows carry whole argvs has no
// bound that fits a UDP path MTU.
func (h *TaskHandler) handleExecRunList(conn ConnHandle, requestID uint32, connID string, filter protocol.TaskID) {
	respond := func(streamID uint64) {
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_ExecRunList, RequestId: requestID}
		resp.SetExecRunList(protocol.ExecRunListResponse{StreamId: streamID})
		out := resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck
	}

	var body protocol.ExecRunListBody
	body.SetExecs(h.visibleExecRuns(connID, filter))

	bodyBytes, err := body.EncodeCopy(nil)
	if err != nil {
		slog.Error("ExecRunList: encode body failed", "err", err)
		respond(0)
		return
	}
	stream := conn.CreateSendStream()
	if stream == nil {
		respond(0)
		return
	}
	if werr := stream.AppendData(false, bodyBytes); werr != nil {
		slog.Warn("ExecRunList: stream write failed", "err", werr)
		_ = stream.Close()
		respond(0)
		return
	}
	if werr := stream.AppendData(true); werr != nil {
		slog.Warn("ExecRunList: stream EOF failed", "err", werr)
		_ = stream.Close()
		respond(0)
		return
	}
	respond(uint64(stream.ID()))
}

// visibleExecRuns returns the registrations connID may see.
//
// No capability is required and the bound is task VISIBILITY, following
// visiblePortForwards — and for the reason await_idle needs no cap either:
// gating a fact `ls` already hands out would make the direct path cost more
// authority than polling for the same thing.
func (h *TaskHandler) visibleExecRuns(connID string, filter protocol.TaskID) []protocol.ExecRunInfo {
	taskFilter := ""
	if filter.Id != ([16]byte{}) {
		taskFilter = hex.EncodeToString(filter.Id[:])
	}
	out := make([]protocol.ExecRunInfo, 0, 8)
	for _, e := range h.execs().list(taskFilter) {
		if !h.execVisibleTo(connID, e) {
			continue
		}
		out = append(out, execRunInfo(e))
	}
	return out
}

// execVisibleTo reports whether connID may see e. Factored out of the listing
// so execs.status gates delivery with the SAME predicate — a subscriber must
// not be told about an exec the listing would deny, which is the rule
// publishConnEvent already states for conns.
func (h *TaskHandler) execVisibleTo(connID string, e *execRun) bool {
	all, allowed := h.visibleToCaller(connID)
	return all || allowed[e.taskIDHex]
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
	if _, still := h.removeExec(req.ExecId); !still {
		return protocol.ExecRunKillResponse{Status: protocol.ExecRunStatus_NotFound}
	}
	h.stopExecOnRunner(e, "exec kill")
	// The client is told regardless: it asked for a stop and the registration
	// is gone either way, so leaving it waiting on a runner that may never
	// answer would be the worse failure.
	writeExecEvent(e, protocol.ExecEventKind_Killed, -1, "killed by exec kill")
	return protocol.ExecRunKillResponse{Status: protocol.ExecRunStatus_Ok}
}

// stopExecOnRunner asks the runner to cancel a child. The caller has already
// dropped the registration; this is the half that reaches the process.
//
// It needs its own request rather than a stream close: closing the exec's
// stream does NOT stop the child — the runner's frame pump reads the EOF and
// ends its input goroutine while the process runs on. Same reason
// ClosePortForward exists.
func (h *TaskHandler) stopExecOnRunner(e *execRun, why string) {
	runner, ok := h.Registry.Get(e.runnerID)
	if !ok || runner.Conn == nil {
		slog.Info("exec_run: runner gone, cannot stop the child", "exec_id", e.execID, "why", why)
		return
	}
	rreq := protocol.RunnerRequest{Kind: protocol.RunnerRequestType_CloseExecRun}
	rreq.SetCloseExecRun(protocol.CloseExecRunRequest{ExecId: e.execID})
	data := rreq.MustAppend([]byte{byte(appwire.AppKind_RunnerControl)})
	if _, _, err := runner.Conn.SendMessage(data); err != nil {
		slog.Error("exec_run: close request to runner failed", "exec_id", e.execID, "why", why, "err", err)
	}
}

// DropExecRunsForConn kills every exec this connection started, and is what
// makes "an exec dies with its caller" true rather than merely intended.
//
// Nothing else can do it. The client's data stream ending is NOT the signal —
// see stopExecOnRunner — and a client that dies abruptly closes nothing at all:
// SIGKILL, or the SIGHUP a terminal sends its foreground group when its window
// is closed.
//
// Verified live before this existed: SIGINT to a `harness-cli exec … -- sh -c
// 'sleep 121'` left the registration in `exec ls` and the `sleep` running under
// the runner indefinitely. Same hook, same reason and the same live symptom as
// DropPortForwardsForConn, which sits beside this one in handleConnection's
// teardown.
//
// No outcome is written back: the stream it would go on belongs to the
// connection that just died.
func (h *TaskHandler) DropExecRunsForConn(connID string) {
	for _, e := range h.execs().list("") {
		if e.clientCID != connID {
			continue
		}
		if _, still := h.removeExec(e.execID); !still {
			continue
		}
		h.stopExecOnRunner(e, "client disconnected")
	}
}
