package server

import (
	"encoding/hex"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
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
		taskID := h.Tasks.Create(string(sub.RepoPath), string(sub.Prompt))

		// Decode hex task ID back to 16 raw bytes for the response.
		raw, _ := hex.DecodeString(taskID)
		var tid protocol.TaskID
		copy(tid.Id[:], raw)

		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_Submit, RequestId: req.RequestId}
		resp.SetSubmit(protocol.SubmitResponse{TaskId: tid})

		out := resp.MustAppend([]byte{byte(wire.ApplicationPayloadKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck

		if h.OnChange != nil {
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

	default:
		slog.Error("TaskHandler: unhandled kind", "kind", req.Kind)
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
		Status:      r.Status,
		ConnectedAt: uint64(r.ConnectedAt.UnixNano()),
		LastSeen:    uint64(r.LastSeen.UnixNano()),
	}
	info.SetRepoPath([]byte(r.RepoPath))
	info.Id = placeholderRunnerID()
	if r.CurrentTask != "" {
		raw, _ := hex.DecodeString(r.CurrentTask)
		copy(info.CurrentTask.Id[:], raw)
	}
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
		Id:        tid,
		Status:    t.Status,
		CreatedAt: uint64(t.CreatedAt.UnixNano()),
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
