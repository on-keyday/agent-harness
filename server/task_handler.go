package server

import (
	"encoding/hex"
	"log/slog"

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

		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_Submit}
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

		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_List}
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

		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_Cancel}
		resp.SetCancel(protocol.CancelStatus{Status: 0})

		out := resp.MustAppend([]byte{byte(wire.ApplicationPayloadKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck

		if h.OnChange != nil {
			h.OnChange()
		}

	default:
		slog.Error("TaskHandler: unhandled kind", "kind", req.Kind)
	}
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
