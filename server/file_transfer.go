package server

import (
	"encoding/hex"
	"log/slog"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// handleOpenFileTransfer fans the client's request out to the assigned
// runner and bridges the two trsf bidi streams. The actual file I/O
// happens entirely on the runner end; this function is a routing primitive.
//
// Status codes only cover what the server can determine without consulting
// the runner (no_such_task, runner_offline, internal_error). File-level
// errors (path_invalid, not_found, already_exists, io_error) arrive in-band
// via the FileTransferAck written by the runner over the spliced stream.
func (h *TaskHandler) handleOpenFileTransfer(conn ConnHandle, req *protocol.OpenFileTransferRequest) protocol.OpenFileTransferResponse {
	errResp := func(s protocol.OpenFileTransferStatus) protocol.OpenFileTransferResponse {
		return protocol.OpenFileTransferResponse{Status: s}
	}
	taskIDHex := hex.EncodeToString(req.TaskId.Id[:])
	task, ok := h.Tasks.Get(taskIDHex)
	if !ok || task.Status != protocol.TaskStatus_Running {
		return errResp(protocol.OpenFileTransferStatus_NoSuchTask)
	}
	runner, ok := h.Registry.Get(task.AssignedTo)
	if !ok || runner.Conn == nil {
		return errResp(protocol.OpenFileTransferStatus_RunnerOffline)
	}
	if conn == nil {
		slog.Error("file_transfer: nil client conn (programmer error)")
		return errResp(protocol.OpenFileTransferStatus_InternalError)
	}
	clientStream := conn.CreateBidirectionalStream()
	if clientStream == nil {
		return errResp(protocol.OpenFileTransferStatus_InternalError)
	}
	runnerStream := runner.Conn.CreateBidirectionalStream()
	if runnerStream == nil {
		_ = clientStream.CloseBoth()
		return errResp(protocol.OpenFileTransferStatus_InternalError)
	}

	rreq := protocol.RunnerRequest{Kind: protocol.RunnerRequestType_OpenFileTransfer}
	body := protocol.RunnerOpenFileTransferRequest{
		TaskId:       req.TaskId,
		StreamId:     uint64(runnerStream.ID()),
		Direction:    req.Direction,
		ExpectedSize: req.ExpectedSize,
	}
	body.SetRelPath(req.RelPath)
	rreq.SetOpenFileTransfer(body)
	data := rreq.MustAppend([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)})
	if _, _, err := runner.Conn.SendMessage(data); err != nil {
		_ = clientStream.CloseBoth()
		_ = runnerStream.CloseBoth()
		slog.Error("file_transfer: send to runner failed", "task_id", taskIDHex, "err", err)
		return errResp(protocol.OpenFileTransferStatus_InternalError)
	}
	go spliceBidiHalfClose(clientStream, runnerStream, taskIDHex)
	return protocol.OpenFileTransferResponse{
		Status:   protocol.OpenFileTransferStatus_Ok,
		StreamId: uint64(clientStream.ID()),
	}
}

// handleListFiles is identical in shape to handleOpenFileTransfer but uses
// the list_files RunnerRequest variant. The two are kept separate (rather
// than parameterized) because the request/response brgen types differ.
func (h *TaskHandler) handleListFiles(conn ConnHandle, req *protocol.ListFilesRequest) protocol.ListFilesResponse {
	errResp := func(s protocol.ListFilesStatus) protocol.ListFilesResponse {
		return protocol.ListFilesResponse{Status: s}
	}
	taskIDHex := hex.EncodeToString(req.TaskId.Id[:])
	task, ok := h.Tasks.Get(taskIDHex)
	if !ok || task.Status != protocol.TaskStatus_Running {
		return errResp(protocol.ListFilesStatus_NoSuchTask)
	}
	runner, ok := h.Registry.Get(task.AssignedTo)
	if !ok || runner.Conn == nil {
		return errResp(protocol.ListFilesStatus_RunnerOffline)
	}
	if conn == nil {
		slog.Error("list_files: nil client conn (programmer error)")
		return errResp(protocol.ListFilesStatus_InternalError)
	}
	clientStream := conn.CreateBidirectionalStream()
	if clientStream == nil {
		return errResp(protocol.ListFilesStatus_InternalError)
	}
	runnerStream := runner.Conn.CreateBidirectionalStream()
	if runnerStream == nil {
		_ = clientStream.CloseBoth()
		return errResp(protocol.ListFilesStatus_InternalError)
	}

	rreq := protocol.RunnerRequest{Kind: protocol.RunnerRequestType_ListFiles}
	body := protocol.RunnerListFilesRequest{
		TaskId:   req.TaskId,
		StreamId: uint64(runnerStream.ID()),
	}
	body.SetRelPath(req.RelPath)
	rreq.SetListFiles(body)
	data := rreq.MustAppend([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)})
	if _, _, err := runner.Conn.SendMessage(data); err != nil {
		_ = clientStream.CloseBoth()
		_ = runnerStream.CloseBoth()
		slog.Error("list_files: send to runner failed", "task_id", taskIDHex, "err", err)
		return errResp(protocol.ListFilesStatus_InternalError)
	}
	go spliceBidiHalfClose(clientStream, runnerStream, taskIDHex)
	return protocol.ListFilesResponse{
		Status:   protocol.ListFilesStatus_Ok,
		StreamId: uint64(clientStream.ID()),
	}
}
