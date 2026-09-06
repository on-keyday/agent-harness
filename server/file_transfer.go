package server

import (
	"context"
	"encoding/hex"
	"fmt"
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
	out, err := h.openDataPlane(conn, &runner,
		protocol.TaskControlKind_OpenFileTransfer, req.Direction, req.TaskId, req.Route)
	if err != nil {
		slog.Warn("file_transfer: requested route unavailable", "task_id", taskIDHex, "err", err)
		return errResp(protocol.OpenFileTransferStatus_RouteUnavailable)
	}
	if out != nil {
		return protocol.OpenFileTransferResponse{
			Status:    protocol.OpenFileTransferStatus_Ok,
			GrantId:   out.GrantID,
			SlotId:    out.Slot,
			Mtu:       out.MTU,
			RunnerCid: out.DialAt,
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
	out, err := h.openDataPlane(conn, &runner,
		protocol.TaskControlKind_ListFiles, 0, req.TaskId, req.Route)
	if err != nil {
		slog.Warn("list_files: requested route unavailable", "task_id", taskIDHex, "err", err)
		return errResp(protocol.ListFilesStatus_RouteUnavailable)
	}
	if out != nil {
		return protocol.ListFilesResponse{
			Status:    protocol.ListFilesStatus_Ok,
			GrantId:   out.GrantID,
			SlotId:    out.Slot,
			Mtu:       out.MTU,
			RunnerCid: out.DialAt,
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

// dataPlaneOutcome is what a non-splice route produced: the credential, the
// connection id the bytes will cross at, and where the client is to dial.
type dataPlaneOutcome struct {
	GrantID [16]uint8
	Slot    uint16
	// DialAt is zero for forwarded -- the client dials the server's own address
	// at Slot and the server relays. It names the runner for direct, which is
	// legal only because the runner has been punched toward this client.
	DialAt protocol.RunnerID
	MTU    uint16
}

// openDataPlane sets up the route the request NAMED.
//
//	(nil, nil)  splice: the caller does what it always did
//	(out, nil)  the named route is up
//	(nil, err)  the named route cannot be taken here
//
// The third case is answered with route_unavailable, never by quietly splicing.
// A caller that named forwarded or direct asked for the server not to read
// these bytes; substituting the splice would hand over exactly what was
// withheld, and would do it silently. Retrying on another route is the caller's
// decision to make, which it cannot make if it is not told.
//
// This is the shape the earlier version got wrong: it collapsed "the route is
// not wanted" and "the route is not possible" into one false, so a transport
// mismatch, an absent endpoint and a runner refusal all came out as a splice
// nobody asked for.
func (h *TaskHandler) openDataPlane(
	conn ConnHandle,
	runner *RunnerEntry,
	kind protocol.TaskControlKind,
	dir protocol.FileTransferDirection,
	taskID protocol.TaskID,
	route protocol.FileTransferRoute,
) (*dataPlaneOutcome, error) {
	if route == protocol.FileTransferRoute_Splice {
		return nil, nil
	}
	if h.SetupDataPlane == nil {
		return nil, fmt.Errorf("route %v: this server has no data-plane endpoint", route)
	}
	if runner == nil || runner.Conn == nil {
		return nil, fmt.Errorf("route %v: runner offline", route)
	}
	clientCID := conn.ConnectionID()
	rc := runner.Conn.ConnectionID()
	if !dataPlaneRoute(clientCID, rc) {
		return nil, fmt.Errorf("route %v: one end has no transport (client=%q runner=%q)",
			route, clientCID.Transport, rc.Transport)
	}
	direct := route == protocol.FileTransferRoute_Direct
	if direct && !dataPlaneDirectOK(clientCID, rc) {
		// Naming a transport pair that cannot dial is the common way to ask for
		// direct by mistake -- a browser, or a ws client against a udp runner --
		// so say which pair rather than just refusing.
		return nil, fmt.Errorf("route direct: needs both ends on udp (client=%q runner=%q)",
			clientCID.Transport, rc.Transport)
	}
	grant := mintGrant(kind, dir, taskID, dataPlaneGrantTTL)
	ctx, cancel := context.WithTimeout(context.Background(), dataPlaneSetupTimeout)
	defer cancel()
	slot, err := h.SetupDataPlane(ctx, clientCID, runner, grant, direct)
	if err != nil {
		return nil, fmt.Errorf("route %v: %w", route, err)
	}
	out := &dataPlaneOutcome{
		GrantID: grant.GrantId,
		Slot:    slot,
		MTU:     negotiatedMTU(clientCID.Transport, rc.Transport),
	}
	if direct {
		out.DialAt = protocol.ConnIDToRunnerID(rc)
	}
	return out, nil
}

// dataPlaneSetupTimeout bounds the runner round trip in openDataPlane. Short
// because exceeding it is now an answer the caller acts on, not a silent
// downgrade it never learns about.
const dataPlaneSetupTimeout = 5 * time.Second
