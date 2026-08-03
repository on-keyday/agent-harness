package server

import (
	"encoding/hex"
	"log/slog"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// handleGitQuery fans a read-only git request out to the task's runner and
// bridges the two trsf streams, exactly like handleListFiles. The git
// invocation itself happens entirely on the runner end; this function is a
// routing primitive.
//
// The one deliberate difference from handleOpenFileTransfer / handleListFiles:
// there is no Running/Detached gate. Those two need a live worktree; a git
// query does not, because the runner keeps branch harness/<taskID> after a
// clean task end and falls back to it. Reviewing what a finished task did is
// exactly what that gate would otherwise make impossible.
//
// The repo path travels from the server's task record rather than being
// resolved runner-side: the runner drops its own per-task entry when the task
// ends, so worktreeDirFor cannot answer for a terminal task. The runner
// re-validates the path against its --roots before running anything.
//
// Statuses here cover only what the server can decide alone. Everything the
// runner determines (repo not allowed, no source, bad rev, git failed) arrives
// in-band as a GitQueryResult on the spliced stream.
func (h *TaskHandler) handleGitQuery(conn ConnHandle, req *protocol.GitQueryRequest) protocol.GitQueryResponse {
	errResp := func(s protocol.GitQueryStatus) protocol.GitQueryResponse {
		return protocol.GitQueryResponse{Status: s}
	}
	taskIDHex := hex.EncodeToString(req.TaskId.Id[:])
	task, ok := h.Tasks.Get(taskIDHex)
	if !ok {
		return errResp(protocol.GitQueryStatus_NoSuchTask)
	}
	runner, ok := h.Registry.Get(task.AssignedTo)
	if !ok || runner.Conn == nil {
		return errResp(protocol.GitQueryStatus_RunnerOffline)
	}
	if conn == nil {
		slog.Error("git_query: nil client conn (programmer error)")
		return errResp(protocol.GitQueryStatus_InternalError)
	}
	clientStream := conn.CreateBidirectionalStream()
	if clientStream == nil {
		return errResp(protocol.GitQueryStatus_InternalError)
	}
	runnerStream := runner.Conn.CreateBidirectionalStream()
	if runnerStream == nil {
		_ = clientStream.CloseBoth()
		return errResp(protocol.GitQueryStatus_InternalError)
	}

	rreq := protocol.RunnerRequest{Kind: protocol.RunnerRequestType_GitQuery}
	body := protocol.RunnerGitQueryRequest{
		TaskId:     req.TaskId,
		StreamId:   uint64(runnerStream.ID()),
		Kind:       req.Kind,
		Target:     req.Target,
		MaxCommits: req.MaxCommits,
		MaxBytes:   req.MaxBytes,
	}
	body.SetRepoPath([]byte(task.RepoPath))
	body.SetBaseRev(req.BaseRev)
	body.SetTargetRev(req.TargetRev)
	body.SetPath(req.Path)
	rreq.SetGitQuery(body)
	data := rreq.MustAppend([]byte{byte(appwire.AppKind_RunnerControl)})
	if _, _, err := runner.Conn.SendMessage(data); err != nil {
		_ = clientStream.CloseBoth()
		_ = runnerStream.CloseBoth()
		slog.Error("git_query: send to runner failed", "task_id", taskIDHex, "err", err)
		return errResp(protocol.GitQueryStatus_InternalError)
	}
	go spliceBidiHalfClose(clientStream, runnerStream, taskIDHex)
	return protocol.GitQueryResponse{
		Status:   protocol.GitQueryStatus_Ok,
		StreamId: uint64(clientStream.ID()),
	}
}
