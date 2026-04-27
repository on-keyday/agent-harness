package server

import (
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/trsf"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// TaskHandler decodes inbound TaskControlRequest payloads from the CLI
// and replies with a TaskControlResponse. It also triggers the scheduler
// via OnChange after mutating operations (Submit, Cancel).
type TaskHandler struct {
	Tasks    *TaskStore
	Registry *Registry
	OnChange func() // called after Submit / Cancel mutations

	// PruneFn handles a CLI-driven prune request. If nil, prune requests reply
	// with removed=0. Server.New wires this to TaskStore.PruneTerminal with the
	// configured logs directory.
	PruneFn func(cutoff time.Time) int

	// LogsDir is the directory containing per-task log files
	// (<LogsDir>/<task-id>.log). Empty disables GetTaskLog responses
	// (always returns Found=0). Server.New wires it from cfg.DataDir.
	LogsDir string

	// clientKinds maps connection ID → the kind of client that announced
	// itself via ClientHello on that connection. Submit / OpenInteractive
	// look it up to attribute task origin (ClientKind_Cli / Tui / Webui).
	// Entries are added on ClientHello and never explicitly removed; for
	// individual-dogfood scale the leak is acceptable. Tasks created on a
	// connection that never sent ClientHello get ClientKind_Unspecified
	// (the zero value of a missing map entry).
	clientKindsMu sync.Mutex
	clientKinds   map[string]protocol.ClientKind
}

// lookupClientKind returns the ClientKind associated with connID.
// Unknown connections return ClientKind_Unspecified (zero value).
func (h *TaskHandler) lookupClientKind(connID string) protocol.ClientKind {
	h.clientKindsMu.Lock()
	defer h.clientKindsMu.Unlock()
	return h.clientKinds[connID]
}

// Handle decodes a TaskControlRequest payload (bytes after the wire-kind byte) and replies via conn.SendMessage.
// Decode failures are logged and silently dropped (no panic).
func (h *TaskHandler) Handle(conn ConnHandle, payload []byte) {
	var req protocol.TaskControlRequest
	if err := req.DecodeExact(payload); err != nil {
		slog.Error("TaskHandler: failed to decode TaskControlRequest", "error", err)
		return
	}

	switch req.Kind {
	case protocol.TaskControlKind_Submit:
		sub := req.Submit()
		if sub == nil {
			slog.Error("TaskHandler: Submit variant is nil")
			return
		}
		origin := h.lookupClientKind(conn.ConnectionID().String())
		submitResp := h.handleSubmit(sub, origin)

		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_Submit, RequestId: req.RequestId}
		resp.SetSubmit(submitResp)

		out := resp.MustAppend([]byte{byte(wire.ApplicationPayloadKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck

		if submitResp.Status == protocol.SubmitStatus_Ok && h.OnChange != nil {
			h.OnChange()
		}

	case protocol.TaskControlKind_List:
		runners := h.Registry.List()
		tasks := h.Tasks.List(100)

		runnerInfos := make([]protocol.RunnerInfo, len(runners))
		for i, r := range runners {
			runnerInfos[i] = toRunnerInfo(r)
		}

		taskInfos := make([]protocol.TaskInfo, len(tasks))
		for i, t := range tasks {
			taskInfos[i] = toTaskInfo(t)
		}

		var list protocol.ListResult
		list.SetRunners(runnerInfos)
		list.SetTasks(taskInfos)

		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_List, RequestId: req.RequestId}
		resp.SetList(list)

		out := resp.MustAppend([]byte{byte(wire.ApplicationPayloadKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck

	case protocol.TaskControlKind_Cancel:
		can := req.Cancel()
		if can == nil {
			slog.Error("TaskHandler: Cancel variant is nil")
			return
		}
		taskID := hex.EncodeToString(can.TaskId.Id[:])
		h.Tasks.Cancel(taskID)

		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_Cancel, RequestId: req.RequestId}
		resp.SetCancel(protocol.CancelStatus{Status: 0})

		out := resp.MustAppend([]byte{byte(wire.ApplicationPayloadKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck

		if h.OnChange != nil {
			h.OnChange()
		}

	case protocol.TaskControlKind_PruneTasks:
		pr := req.Prune()
		if pr == nil {
			slog.Error("TaskHandler: Prune variant is nil")
			return
		}
		var removed uint32
		if h.PruneFn != nil {
			cutoff := time.Unix(0, int64(pr.BeforeTs))
			removed = uint32(h.PruneFn(cutoff))
		}
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_PruneTasks, RequestId: req.RequestId}
		resp.SetPrune(protocol.PruneTasksResponse{Removed: removed})

		out := resp.MustAppend([]byte{byte(wire.ApplicationPayloadKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck

	case protocol.TaskControlKind_GetTaskLog:
		gl := req.GetLog()
		if gl == nil {
			slog.Error("TaskHandler: GetLog variant is nil")
			return
		}
		taskID := hex.EncodeToString(gl.TaskId.Id[:])
		h.handleGetTaskLog(conn, req.RequestId, taskID)

	case protocol.TaskControlKind_OpenInteractive:
		oi := req.OpenInteractive()
		if oi == nil {
			slog.Error("TaskHandler: OpenInteractive variant is nil")
			return
		}
		origin := h.lookupClientKind(conn.ConnectionID().String())
		oresp := h.handleOpenInteractive(conn, oi, origin)
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_OpenInteractive, RequestId: req.RequestId}
		resp.SetOpenInteractive(oresp)
		out := resp.MustAppend([]byte{byte(wire.ApplicationPayloadKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck

	case protocol.TaskControlKind_ClientHello:
		hello := req.ClientHello()
		if hello == nil {
			slog.Error("TaskHandler: ClientHello variant is nil")
			return
		}
		cid := conn.ConnectionID().String()
		slog.Info("client hello", "kind", hello.Kind.String(), "cid", cid)

		// Remember this connection's kind so subsequent Submit /
		// OpenInteractive requests on the same connection can attribute
		// task origin without the client having to re-send it.
		h.clientKindsMu.Lock()
		if h.clientKinds == nil {
			h.clientKinds = make(map[string]protocol.ClientKind)
		}
		h.clientKinds[cid] = hello.Kind
		h.clientKindsMu.Unlock()

		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_ClientHello, RequestId: req.RequestId}
		resp.SetClientHello(protocol.ClientHelloResponse{Status: protocol.ClientHelloStatus_Ok})

		out := resp.MustAppend([]byte{byte(wire.ApplicationPayloadKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck

	default:
		slog.Error("TaskHandler: unhandled kind", "kind", req.Kind)
	}
}

// handleSubmit resolves the runner selector and either enqueues the task or
// returns a synchronous error status. The four possible outcomes:
//
//   - SubmitStatus_Ok            — exactly one candidate, task created and queued
//   - SubmitStatus_NoRunner      — no candidates match (Any selector)
//   - SubmitStatus_PinnedNotFound — no candidates match (pinned selector)
//   - SubmitStatus_AmbiguousRunner — more than one candidate (error_msg lists hostnames)
//
// On Ok, the returned SubmitResponse carries the new TaskId.
func (h *TaskHandler) handleSubmit(req *protocol.SubmitRequest, origin protocol.ClientKind) protocol.SubmitResponse {
	repo := filepath.Clean(string(req.RepoPath))
	cands := h.Registry.Candidates(repo, req.Selector)
	switch {
	case len(cands) == 0 && req.Selector.Kind != protocol.RunnerSelectorKind_Any:
		return protocol.SubmitResponse{Status: protocol.SubmitStatus_PinnedNotFound}
	case len(cands) == 0:
		return protocol.SubmitResponse{Status: protocol.SubmitStatus_NoRunner}
	case len(cands) > 1:
		hostnames := make([]string, len(cands))
		for i, c := range cands {
			hostnames[i] = c.Hostname
		}
		msg := []byte("ambiguous: " + strings.Join(hostnames, ", "))
		resp := protocol.SubmitResponse{Status: protocol.SubmitStatus_AmbiguousRunner}
		resp.SetErrorMsg(msg)
		return resp
	}
	bound := cands[0]
	taskIDHex := h.Tasks.Create(repo, string(req.Prompt), protocol.TaskKind_Oneshot, origin, bound.ID, req.Selector)
	var tid protocol.TaskID
	raw, _ := hex.DecodeString(taskIDHex)
	copy(tid.Id[:], raw)
	return protocol.SubmitResponse{Status: protocol.SubmitStatus_Ok, TaskId: tid}
}

// handleOpenInteractive resolves the runner selector, gates on capacity, and
// (when all checks pass) allocates two bidirectional streams, binds the task,
// sends OpenExec to the runner, and starts the splice goroutine.
//
// Return value is the synchronous protocol response; the caller sends it over
// tuiConn. tuiConn may be nil when called from tests that only exercise the
// error paths (NoRunnerForRepo, RunnerBusy, AmbiguousRunner, PinnedNotFound).
//
// Synchronous decision logic (four cases):
//   - PinnedNotFound   — pinned selector (Kind != Any) but no candidates match
//   - NoRunnerForRepo  — Any selector and no candidates match
//   - AmbiguousRunner  — more than one candidate matches
//   - RunnerBusy       — exactly one candidate but it is at capacity
//
// If none of the above apply, the method proceeds with stream setup. Stream
// errors (CreateBidirectionalStream, SendMessage) return InternalError.
//
// The task is registered as Running before the splice begins so the scheduler
// does not pick it up for a parallel AssignTask. The runner finalizes the task
// lifecycle by sending TaskStarted (worktree dir filled in) and TaskFinished
// (exit code from claude) over the regular RunnerControl path.
func (h *TaskHandler) handleOpenInteractive(tuiConn ConnHandle, req *protocol.OpenInteractiveRequest, origin protocol.ClientKind) protocol.OpenInteractiveResponse {
	errResp := func(status protocol.OpenInteractiveStatus) protocol.OpenInteractiveResponse {
		return protocol.OpenInteractiveResponse{Status: status}
	}

	repo := filepath.Clean(string(req.RepoPath))
	cands := h.Registry.Candidates(repo, req.Selector)
	switch {
	case len(cands) == 0 && req.Selector.Kind != protocol.RunnerSelectorKind_Any:
		return errResp(protocol.OpenInteractiveStatus_PinnedNotFound)
	case len(cands) == 0:
		return errResp(protocol.OpenInteractiveStatus_NoRunnerForRepo)
	case len(cands) > 1:
		return errResp(protocol.OpenInteractiveStatus_AmbiguousRunner)
	}
	runner := cands[0]

	// Capacity gate — interactive sessions cannot queue; fail fast if runner is full.
	if len(runner.ActiveTasks) >= runner.MaxTasks {
		return errResp(protocol.OpenInteractiveStatus_RunnerBusy)
	}

	// tuiConn is nil in test invocations that only exercise the error paths above.
	// A nil conn here indicates a programming error in production callers.
	if tuiConn == nil {
		slog.Error("handleOpenInteractive: tuiConn is nil on Ok path")
		return errResp(protocol.OpenInteractiveStatus_InternalError)
	}

	// Allocate the task entry. The TaskKind_Interactive value is the
	// authoritative marker — empty prompt is incidental.
	taskIDHex := h.Tasks.Create(repo, "", protocol.TaskKind_Interactive, origin, runner.ID, req.Selector)
	var tid protocol.TaskID
	raw, _ := hex.DecodeString(taskIDHex)
	copy(tid.Id[:], raw)

	// Mark the task Running and bound to this runner immediately so the
	// scheduler doesn't try to AssignTask it. The runner will fill in the
	// real worktree dir via TaskStarted shortly after open_exec arrives.
	h.Tasks.Assign(taskIDHex, runner.ID, "")
	h.Registry.BindTask(runner.ID, taskIDHex)

	finishWithError := func(reason string) {
		h.Tasks.Finish(taskIDHex, -1, []byte("server: "+reason))
		h.Registry.UnbindTask(runner.ID, taskIDHex)
	}

	tuiStream := tuiConn.CreateBidirectionalStream()
	if tuiStream == nil {
		finishWithError("create client-side stream failed")
		return errResp(protocol.OpenInteractiveStatus_InternalError)
	}
	runnerConn := runner.Conn
	if runnerConn == nil {
		_ = tuiStream.CloseBoth()
		finishWithError("runner conn nil")
		return errResp(protocol.OpenInteractiveStatus_InternalError)
	}
	runnerStream := runnerConn.CreateBidirectionalStream()
	if runnerStream == nil {
		_ = tuiStream.CloseBoth()
		finishWithError("create runner-side stream failed")
		return errResp(protocol.OpenInteractiveStatus_InternalError)
	}

	// Tell the runner to wire its stream end to claude.
	rreq := protocol.RunnerRequest{Kind: protocol.RunnerRequestType_OpenExec}
	rreq.SetOpenExec(protocol.OpenExecRunnerRequest{
		TaskId:   tid,
		StreamId: uint64(runnerStream.ID()),
	})
	rdata := rreq.MustAppend([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)})
	if _, _, err := runnerConn.SendMessage(rdata); err != nil {
		_ = tuiStream.CloseBoth()
		_ = runnerStream.CloseBoth()
		finishWithError("send open_exec to runner: " + err.Error())
		return errResp(protocol.OpenInteractiveStatus_InternalError)
	}

	go func() {
		spliceBidi(tuiStream, runnerStream, taskIDHex)

		// Defensive cleanup. The expected path is that the runner's
		// ExecuteCommand returns once the splice tears down both stream
		// ends, and the runner sends TaskFinished — RunnerHandler then
		// flips the task to Succeeded/Failed and the runner back to Idle.
		//
		// But if the runner crashed mid-session, the runner connection
		// dropped, or the runner is wedged before it can reach the
		// TaskFinished step, the task we marked Running in this handler
		// (and the runner we marked Busy) would otherwise stay that way
		// forever. Finalize them here, but only if the state we set is
		// still in place — a TaskFinished that genuinely arrived first
		// already moved the task terminal and the runner Idle, and we
		// must not undo that (the scheduler may have re-bound the
		// runner to a different task by now).
		// Cancel is idempotent (skips terminal states), so a TaskFinished
		// that genuinely arrived first is unaffected. UnbindTask is
		// idempotent (no-op if the task slot is no longer present), so it
		// cannot clobber a fresh scheduler assignment that already moved the
		// runner to a different task.
		if t, ok := h.Tasks.Get(taskIDHex); ok && t.Status == protocol.TaskStatus_Running {
			h.Tasks.Cancel(taskIDHex)
		}
		h.Registry.UnbindTask(runner.ID, taskIDHex)
		if h.OnChange != nil {
			h.OnChange()
		}
	}()

	return protocol.OpenInteractiveResponse{
		Status:   protocol.OpenInteractiveStatus_Ok,
		TaskId:   tid,
		StreamId: uint64(tuiStream.ID()),
	}
}

// spliceBidi pumps bytes between two bidirectional streams in both directions
// until either side closes or errors. Each side's EOF is propagated to the
// other so exec.ExecuteCommand can react to TUI detach (close stream → EOF
// on PTY stdin pipe → claude exits via SIGHUP/SIGTERM/SIGKILL ladder).
//
// When either direction returns — clean EOF or error — both streams are
// fully closed via CloseBoth. For an interactive PTY, half-closes are not
// meaningful: if one direction is dead the other should be torn down too,
// otherwise the surviving relay goroutine blocks forever on ReadDirect
// (e.g. TUI vanished mid-session, claude is sitting idle waiting for
// stdin) and the splice never finishes.
func spliceBidi(a, b trsf.BidirectionalStream, taskIDHex string) {
	var wg sync.WaitGroup
	var once sync.Once
	teardown := func() {
		once.Do(func() {
			_ = a.CloseBoth()
			_ = b.CloseBoth()
		})
	}
	wg.Add(2)
	go func() { defer wg.Done(); defer teardown(); relayBytes(a, b) }()
	go func() { defer wg.Done(); defer teardown(); relayBytes(b, a) }()
	wg.Wait()
	slog.Info("OpenInteractive: splice ended", "task_id", taskIDHex)
}

// relayBytes copies bytes from src to dst, propagating EOF. Returns on the
// first read error or write error — the spliceBidi caller force-closes both
// streams once either direction returns, which unblocks the reverse relay.
func relayBytes(src, dst trsf.BidirectionalStream) {
	for {
		data, eof, err := src.ReadDirect(64 * 1024)
		if err != nil {
			return
		}
		if len(data) > 0 {
			if werr := dst.AppendData(eof, data); werr != nil {
				return
			}
		} else if eof {
			_ = dst.AppendData(true)
		}
		if eof {
			return
		}
	}
}

// handleGetTaskLog responds to a GetTaskLog request by opening the per-task
// log file at <LogsDir>/<taskID>.log, allocating a server-initiated
// unidirectional stream, sending a TaskControlResponse referencing that
// stream's id, and then streaming the file content + EOF asynchronously.
//
// If LogsDir is empty (server started without --data-dir) or the file does
// not exist, the response carries Found=0 and StreamId=0.
func (h *TaskHandler) handleGetTaskLog(conn ConnHandle, requestID uint32, taskID string) {
	respond := func(found uint8, streamID uint64) {
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_GetTaskLog, RequestId: requestID}
		resp.SetGetLog(protocol.GetTaskLogResponse{Found: found, StreamId: streamID})
		out := resp.MustAppend([]byte{byte(wire.ApplicationPayloadKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck
	}

	if h.LogsDir == "" {
		respond(0, 0)
		return
	}
	path := filepath.Join(h.LogsDir, taskID+".log")
	f, err := os.Open(path)
	if err != nil {
		// Includes os.ErrNotExist (no log yet) and any I/O error.
		respond(0, 0)
		return
	}
	stream := conn.CreateSendStream()
	if stream == nil {
		// Test or non-streaming connection: degrade to "no log".
		f.Close()
		respond(0, 0)
		return
	}

	respond(1, uint64(stream.ID()))

	// Stream the file content asynchronously so the response goes out first.
	go func() {
		defer f.Close()
		buf := make([]byte, 32*1024)
		for {
			n, rerr := f.Read(buf)
			if n > 0 {
				if werr := stream.AppendData(false, buf[:n]); werr != nil {
					slog.Warn("GetTaskLog: stream write failed", "task_id", taskID, "err", werr)
					break
				}
			}
			if rerr == io.EOF {
				break
			}
			if rerr != nil {
				slog.Warn("GetTaskLog: file read failed", "task_id", taskID, "err", rerr)
				break
			}
		}
		// Signal EOF on the stream so the client knows we're done.
		if err := stream.AppendData(true); err != nil {
			slog.Warn("GetTaskLog: stream EOF failed", "task_id", taskID, "err", err)
		}
	}()
}

// toRunnerInfo converts a RunnerEntry value snapshot to the wire-format RunnerInfo.
// NOTE: RunnerInfo.Id is filled with an IPv4 placeholder; the real objproto.ConnectionID
// → protocol.RunnerID round-trip is deferred to a later task (CLI-side display will not show
// the runner identity precisely, but server-internal logic uses RunnerEntry.ID directly).
func toRunnerInfo(r RunnerEntry) protocol.RunnerInfo {
	info := protocol.RunnerInfo{
		Status:      r.Status(),
		MaxTasks:    uint16(r.MaxTasks),
		ConnectedAt: uint64(r.ConnectedAt.UnixNano()),
		LastSeen:    uint64(r.LastSeen.UnixNano()),
	}
	info.SetHostname([]byte(r.Hostname))
	info.Id = placeholderRunnerID()

	// Populate AllowedRoots.
	roots := make([]protocol.AllowedRoot, len(r.AllowedRoots))
	for i, root := range r.AllowedRoots {
		var ar protocol.AllowedRoot
		ar.SetPath([]byte(root))
		roots[i] = ar
	}
	info.SetAllowedRoots(roots)

	// Populate ActiveTasks.
	activeTasks := make([]protocol.ActiveTaskRef, 0, len(r.ActiveTasks))
	for taskID := range r.ActiveTasks {
		var ref protocol.ActiveTaskRef
		raw, _ := hex.DecodeString(taskID)
		copy(ref.TaskId.Id[:], raw)
		activeTasks = append(activeTasks, ref)
	}
	info.ActiveTasksLen = uint16(len(activeTasks))
	info.ActiveTasks = activeTasks

	return info
}

// toTaskInfo converts a TaskEntry value snapshot to the wire-format TaskInfo.
// NOTE: TaskInfo.AssignedTo is filled with an IPv4 placeholder for the same reason
// as RunnerInfo.Id — the real round-trip is deferred (see comment in toRunnerInfo).
func toTaskInfo(t TaskEntry) protocol.TaskInfo {
	var tid protocol.TaskID
	raw, _ := hex.DecodeString(t.ID)
	copy(tid.Id[:], raw)

	info := protocol.TaskInfo{
		Id:         tid,
		Status:     t.Status,
		Kind:       t.Kind,
		OriginKind: t.OriginKind,
		CreatedAt:  uint64(t.CreatedAt.UnixNano()),
	}
	info.SetRepoPath([]byte(t.RepoPath))
	info.SetWorktreeDir([]byte(t.WorktreeDir))
	info.SetPrompt([]byte(t.Prompt))
	info.AssignedTo = placeholderRunnerID()

	if t.StartedAt != nil {
		info.StartedAt = uint64(t.StartedAt.UnixNano())
	}
	if t.EndedAt != nil {
		info.EndedAt = uint64(t.EndedAt.UnixNano())
	}
	if t.ExitCode != nil {
		info.ExitCode = *t.ExitCode
	}
	return info
}

// placeholderRunnerID returns a safe, encodable RunnerID with a loopback IPv4 address.
// The RunnerID encoder has a hard assertion that IpAddrLen == 4 || IpAddrLen == 16;
// encoding a zero-value RunnerID PANICS. Always use this function when building a
// RunnerInfo or TaskInfo that requires a RunnerID field.
func placeholderRunnerID() protocol.RunnerID {
	rid := protocol.RunnerID{
		Port:         0,
		UniqueNumber: 0,
	}
	rid.SetTransport([]byte("ws"))
	rid.SetIpAddr([]byte{127, 0, 0, 1})
	return rid
}
