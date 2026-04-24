package server

import (
	"encoding/hex"
	"log/slog"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// RunnerHandler decodes inbound RunnerMessage payloads from runners
// and applies them to Registry and TaskStore.
type RunnerHandler struct {
	Registry *Registry
	Tasks    *TaskStore
	Now      func() time.Time
	OnChange func() // called after any state mutation, used to trigger Scheduler.Tick
}

// Handle decodes a RunnerMessage payload (the full bytes including the Kind byte,
// as produced by RunnerMessage.MustAppend) and applies it to the Registry and TaskStore.
// Decode failures and missing inner variants are logged and silently dropped (no panic).
func (h *RunnerHandler) Handle(conn ConnHandle, payload []byte) {
	var msg protocol.RunnerMessage
	if err := msg.DecodeExact(payload); err != nil {
		slog.Error("RunnerHandler: failed to decode RunnerMessage", "error", err)
		return
	}

	runnerID := conn.ConnectionID().String()
	now := h.Now()

	switch msg.Kind {
	case protocol.RunnerMessageType_Hello:
		hello := msg.Hello()
		if hello == nil {
			slog.Error("RunnerHandler: Hello variant is nil", "runnerID", runnerID)
			return
		}
		h.Registry.Add(&RunnerEntry{
			ID:          runnerID,
			RepoPath:    string(hello.RepoPath),
			Status:      protocol.RunnerStatus_Idle,
			ConnectedAt: now,
			LastSeen:    now,
			// Conn field is set by server.go later — left nil here.
		})

	case protocol.RunnerMessageType_TaskAccepted:
		ta := msg.TaskAccepted()
		if ta == nil {
			slog.Error("RunnerHandler: TaskAccepted variant is nil", "runnerID", runnerID)
			return
		}
		// Touching LastSeen only. We use the aliased pointer returned by Get
		// and write directly to LastSeen. This is acceptable because:
		// - Registry.Get documents that the returned pointer aliases the stored
		//   entry, and the aliasing contract allows direct field writes when the
		//   mutation is narrow (single field, no structural invariant broken).
		// - There is no SetLastSeen helper, and SetStatus would also overwrite
		//   Status and CurrentTask which we don't want to change here.
		entry, ok := h.Registry.Get(runnerID)
		if !ok {
			slog.Error("RunnerHandler: TaskAccepted from unknown runner", "runnerID", runnerID)
			return
		}
		entry.LastSeen = now

	case protocol.RunnerMessageType_TaskStarted:
		taskStarted := msg.TaskStarted()
		if taskStarted == nil {
			slog.Error("RunnerHandler: TaskStarted variant is nil", "runnerID", runnerID)
			return
		}
		taskID := hex.EncodeToString(taskStarted.TaskId.Id[:])
		task, ok := h.Tasks.Get(taskID)
		if !ok {
			slog.Error("RunnerHandler: TaskStarted for unknown task", "runnerID", runnerID, "taskID", taskID)
			return
		}
		// Direct field write on aliased pointer — same aliasing contract as Registry.
		// TaskStore.Get documents that returned pointers alias stored entries.
		task.WorktreeDir = string(taskStarted.WorktreeDir)

	case protocol.RunnerMessageType_TaskFinished:
		tf := msg.TaskFinished()
		if tf == nil {
			slog.Error("RunnerHandler: TaskFinished variant is nil", "runnerID", runnerID)
			return
		}
		taskID := hex.EncodeToString(tf.TaskId.Id[:])
		// Tasks.Finish silently no-ops if task is not found — that is acceptable.
		// The runner's status still resets to Idle regardless.
		h.Tasks.Finish(taskID, tf.ExitCode, tf.DiffInfo)
		h.Registry.SetStatus(runnerID, protocol.RunnerStatus_Idle, "")

	case protocol.RunnerMessageType_Heartbeat:
		// Touching LastSeen only. Same aliasing rationale as TaskAccepted.
		entry, ok := h.Registry.Get(runnerID)
		if !ok {
			slog.Error("RunnerHandler: Heartbeat from unknown runner", "runnerID", runnerID)
			return
		}
		entry.LastSeen = now

	default:
		slog.Error("RunnerHandler: unhandled message kind", "runnerID", runnerID, "kind", msg.Kind)
		return
	}

	if h.OnChange != nil {
		h.OnChange()
	}
}
