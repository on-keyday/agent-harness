package server

import (
	"context"
	"encoding/hex"
	"log/slog"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
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
	// Detached is a non-terminal state for detachable interactive tasks
	// (the TUI/CLI client disconnected but the runner-side worktree is
	// still reachable). File ops must remain available so the user can
	// pull/push/ls without first re-attaching.
	if !ok || (task.Status != protocol.TaskStatus_Running && task.Status != protocol.TaskStatus_Detached) {
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
	// Preferred route: hand the client a grant and forward its packets, so
	// these bytes cross this process without being decrypted. Falls back to the
	// splice below when the hook is absent, the transports differ, or the
	// runner refuses -- the client can tell the routes apart by grant_id.
	if grantID, slot, rcid, mtu, ok := h.tryDataPlane(conn, &runner,
		protocol.TaskControlKind_OpenFileTransfer, req.Direction, req.TaskId, req.NoDataPlane()); ok {
		return protocol.OpenFileTransferResponse{
			Status:    protocol.OpenFileTransferStatus_Ok,
			GrantId:   grantID,
			SlotId:    slot,
			Mtu:       mtu,
			RunnerCid: rcid,
		}
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
		// The relay rebuilds the request field by field, so a field left out
		// here reaches the runner as zero and the range is silently ignored —
		// no error anywhere, just the whole file.
		Offset: req.Offset,
		Length: req.Length,
	}
	body.SetRelPath(req.RelPath)
	body.SetForce(req.Force())
	body.SetMkdirParents(req.MkdirParents())
	rreq.SetOpenFileTransfer(body)
	data := rreq.MustAppend([]byte{byte(appwire.AppKind_RunnerControl)})
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
	// Detached is a non-terminal state for detachable interactive tasks
	// (the TUI/CLI client disconnected but the runner-side worktree is
	// still reachable). File ops must remain available so the user can
	// pull/push/ls without first re-attaching. See handleOpenFileTransfer.
	if !ok || (task.Status != protocol.TaskStatus_Running && task.Status != protocol.TaskStatus_Detached) {
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
	if grantID, slot, rcid, mtu, ok := h.tryDataPlane(conn, &runner,
		protocol.TaskControlKind_ListFiles, 0, req.TaskId, req.NoDataPlane()); ok {
		return protocol.ListFilesResponse{
			Status:    protocol.ListFilesStatus_Ok,
			GrantId:   grantID,
			SlotId:    slot,
			Mtu:       mtu,
			RunnerCid: rcid,
		}
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
	data := rreq.MustAppend([]byte{byte(appwire.AppKind_RunnerControl)})
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

// tryDataPlane mints a grant and installs the forwarding route for one request.
// It reports ok=false for every reason the splice path should be used instead,
// so a caller reads it as "was this routed end to end?" and never has to know
// which of the reasons applied.
func (h *TaskHandler) tryDataPlane(
	conn ConnHandle,
	runner *RunnerEntry,
	kind protocol.TaskControlKind,
	dir protocol.FileTransferDirection,
	taskID protocol.TaskID,
	refused bool,
) (grantID [16]uint8, slot uint16, runnerCID protocol.RunnerID, mtu uint16, ok bool) {
	// The caller asked for the splice. The end-to-end route is the default and
	// needs no opting in; this bit exists so a single invocation can get file
	// transfer back when that route is at fault, with no restart and no
	// rebuild, and so the two can be compared on one file.
	if refused {
		return grantID, 0, runnerCID, 0, false
	}
	if h.SetupDataPlane == nil || runner == nil || runner.Conn == nil {
		return grantID, 0, runnerCID, 0, false
	}
	clientCID := conn.ConnectionID()
	rc := runner.Conn.ConnectionID()
	if !dataPlaneRoute(clientCID, rc) {
		return grantID, 0, runnerCID, 0, false
	}
	grant := mintGrant(kind, dir, taskID, dataPlaneGrantTTL)
	ctx, cancel := context.WithTimeout(context.Background(), dataPlaneSetupTimeout)
	defer cancel()
	slot, err := h.SetupDataPlane(ctx, clientCID, runner, grant)
	if err != nil {
		slog.Warn("file_transfer: data plane setup failed, splicing instead", "err", err)
		return grantID, 0, runnerCID, 0, false
	}
	return grant.GrantId, slot, protocol.ConnIDToRunnerID(rc), negotiatedMTU(clientCID.Transport, rc.Transport), true
}

// dataPlaneSetupTimeout bounds the runner round trip in tryDataPlane. It is
// short because the fallback is a working path, not an error.
const dataPlaneSetupTimeout = 5 * time.Second
