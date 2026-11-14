package server

import (
	"encoding/hex"
	"log/slog"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// handleTranscript answers transcript_list and transcript_get (pillar B,
// docs/superpowers/specs/2026-11-14-pillar-b-transcripts-design.md).
//
// The bit (transcript_read) is enforced centrally via requiredCap; the TARGET
// is gated here against the bit's own scope, so a task out of scope answers
// no_such_task, as git_query does. Whether the caller may also read sessions
// recorded in a shared directory (a --no-worktree task's repo) is exec_run's
// scope on the same task (B-D2); the runner applies it, since only the runner
// knows which sessions are shared. The answer is never parsed here: it is
// spliced from the runner's stream to the caller's, as git queries are.
func (h *TaskHandler) handleTranscript(cid string, conn ConnHandle, req *protocol.TaskControlRequest) {
	resp := protocol.TaskControlResponse{Kind: req.Header.Kind, RequestId: req.Header.RequestId}
	var (
		taskID        protocol.TaskID
		op            protocol.TranscriptOp
		session, file []byte
	)
	switch req.Header.Kind {
	case protocol.TaskControlKind_TranscriptList:
		r := req.TranscriptList()
		if r == nil {
			slog.Error("TaskHandler: TranscriptList variant is nil")
			return
		}
		taskID, session, op = r.TaskId, r.Session, protocol.TranscriptOp_Sessions
		if len(session) > 0 {
			op = protocol.TranscriptOp_Files
		}
	case protocol.TaskControlKind_TranscriptGet:
		r := req.TranscriptGet()
		if r == nil {
			slog.Error("TaskHandler: TranscriptGet variant is nil")
			return
		}
		taskID, session, file, op = r.TaskId, r.Session, r.File, protocol.TranscriptOp_Get
	default:
		return
	}
	idHex := hex.EncodeToString(taskID.Id[:])
	out := protocol.TranscriptResponse{Status: protocol.TranscriptStatus_NoSuchTask}
	// A name past the bounds answers the status the runner would give, before
	// a relay that could not fit one datagram (Task 5 review, I2).
	switch {
	case !h.inScope(cid, protocol.Capability_TranscriptRead, idHex):
		// no_such_task, as out was initialised: no existence oracle.
	case len(session) > protocol.TranscriptSessionMax:
		out.Status = protocol.TranscriptStatus_NoSuchSession
	case len(file) > protocol.TranscriptFileMax:
		out.Status = protocol.TranscriptStatus_NoSuchFile
	default:
		includeShared := hasCap(h.callerCaps(cid), protocol.Capability_ExecRun) &&
			h.inScope(cid, protocol.Capability_ExecRun, idHex)
		out = h.transcriptQuery(conn, taskID, op, session, file, includeShared)
	}
	if req.Header.Kind == protocol.TaskControlKind_TranscriptList {
		resp.SetTranscriptList(out)
	} else {
		resp.SetTranscriptGet(out)
	}
	conn.SendMessage(resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})) //nolint:errcheck
}

// transcriptQuery forwards one request to the task's assigned runner and
// bridges the two streams, like handleGitQuery. Statuses here cover only what
// the server decides alone; the runner's answer arrives on the stream.
func (h *TaskHandler) transcriptQuery(conn ConnHandle, taskID protocol.TaskID, op protocol.TranscriptOp, session, file []byte, includeShared bool) protocol.TranscriptResponse {
	fail := func(s protocol.TranscriptStatus) protocol.TranscriptResponse {
		return protocol.TranscriptResponse{Status: s}
	}
	idHex := hex.EncodeToString(taskID.Id[:])
	task, ok := h.Tasks.Get(idHex)
	if !ok {
		return fail(protocol.TranscriptStatus_NoSuchTask)
	}
	// The task's assigned runner (B-D6). A resumed task waiting in the queue
	// has none and answers runner_offline until it is placed again.
	runner, ok := h.Registry.GetByIdentity(task.AssignedTo)
	if !ok || runner.Conn == nil {
		return fail(protocol.TranscriptStatus_RunnerOffline)
	}
	// A runner from before pillar B would drop the connection over an
	// unknown request type: never send it one.
	if !runner.Transcripts {
		return fail(protocol.TranscriptStatus_RunnerUnsupported)
	}
	if conn == nil {
		slog.Error("transcript: nil client conn (programmer error)")
		return fail(protocol.TranscriptStatus_InternalError)
	}
	clientStream := conn.CreateBidirectionalStream()
	if clientStream == nil {
		return fail(protocol.TranscriptStatus_InternalError)
	}
	runnerStream := runner.Conn.CreateBidirectionalStream()
	if runnerStream == nil {
		_ = clientStream.CloseBoth()
		return fail(protocol.TranscriptStatus_InternalError)
	}
	rreq := protocol.RunnerRequest{Kind: protocol.RunnerRequestType_Transcript}
	rreq.SetTranscript(runnerTranscriptRequest(taskID, op, session, file, task.RepoPath, includeShared, uint64(runnerStream.ID())))
	if err := sendRunnerRequest(runner.Conn, &rreq); err != nil {
		_ = clientStream.CloseBoth()
		_ = runnerStream.CloseBoth()
		slog.Error("transcript: send to runner failed", "task_id", idHex, "err", err)
		return fail(protocol.TranscriptStatus_InternalError)
	}
	go spliceBidiHalfClose(clientStream, runnerStream, idHex)
	return protocol.TranscriptResponse{Status: protocol.TranscriptStatus_Ok, StreamId: uint64(clientStream.ID())}
}

// runnerTranscriptRequest builds the runner's request: one relay site for a
// struct that may grow, so a test asserts every field reaches it.
func runnerTranscriptRequest(taskID protocol.TaskID, op protocol.TranscriptOp, session, file []byte, repoPath string, includeShared bool, streamID uint64) protocol.RunnerTranscriptRequest {
	r := protocol.RunnerTranscriptRequest{TaskId: taskID, StreamId: streamID, Op: op}
	r.SetIncludeShared(includeShared)
	r.SetRepoPath([]byte(repoPath))
	r.SetSession(session)
	r.SetFile(file)
	return r
}
