package server

import (
	"encoding/hex"
	"log/slog"
	"path"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// RunnerHandler decodes inbound RunnerMessage payloads from runners
// and applies them to Registry and TaskStore.
type RunnerHandler struct {
	Registry       *Registry
	Tasks          *TaskStore
	Now            func() time.Time
	OnChange       func()             // called after any state mutation, used to trigger Scheduler.Tick
	OnTaskStarted  func(taskID string) // optional; called when the runner reports TaskStarted
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
		maxTasks := int(hello.MaxTasks)
		if maxTasks < 1 {
			maxTasks = 1
		}
		roots := make([]string, len(hello.AllowedRoots))
		for i, ar := range hello.AllowedRoots {
			// Wire is POSIX '/'-paths; use path.Clean (not filepath.Clean) so a
			// Windows-running server doesn't convert '/' to '\' and break the
			// boundary predicate.
			roots[i] = path.Clean(string(ar.Path))
		}
		entry := &RunnerEntry{
			ID:           runnerID,
			Hostname:     string(hello.Hostname),
			AllowedRoots: roots,
			MaxTasks:     maxTasks,
			ActiveTasks:  make(map[string]struct{}),
			ConnectedAt:  now,
			LastSeen:     now,
		}
		entry.Conn = conn
		h.Registry.Add(entry)

	case protocol.RunnerMessageType_TaskAccepted:
		ta := msg.TaskAccepted()
		if ta == nil {
			slog.Error("runner_handler: TaskAccepted variant missing", "runner", runnerID)
			return
		}
		accepted := hex.EncodeToString(ta.TaskId.Id[:])
		if e, ok := h.Registry.Get(runnerID); ok {
			if _, has := e.ActiveTasks[accepted]; !has && len(e.ActiveTasks) > 0 {
				slog.Warn("runner_handler: runner accepted task not in ActiveTasks",
					"runner", runnerID, "accepted", accepted)
			}
		}
		if !h.Registry.SetLastSeen(runnerID, now) {
			slog.Error("runner_handler: SetLastSeen on unknown runner", "runner", runnerID)
			return
		}

	case protocol.RunnerMessageType_TaskStarted:
		taskStarted := msg.TaskStarted()
		if taskStarted == nil {
			slog.Error("RunnerHandler: TaskStarted variant is nil", "runnerID", runnerID)
			return
		}
		taskID := hex.EncodeToString(taskStarted.TaskId.Id[:])
		if !h.Tasks.SetWorktreeDir(taskID, string(taskStarted.WorktreeDir)) {
			slog.Error("RunnerHandler: TaskStarted for unknown task", "runnerID", runnerID, "taskID", taskID)
			return
		}
		if h.OnTaskStarted != nil {
			h.OnTaskStarted(taskID)
		}

	case protocol.RunnerMessageType_TaskFinished:
		tf := msg.TaskFinished()
		if tf == nil {
			slog.Error("RunnerHandler: TaskFinished variant is nil", "runnerID", runnerID)
			return
		}
		taskID := hex.EncodeToString(tf.TaskId.Id[:])
		// Tasks.Finish silently no-ops if task is not found — that is acceptable.
		h.Tasks.Finish(taskID, tf.ExitCode, tf.DiffInfo)
		// Release the capacity slot so the dispatcher can re-use it.
		h.Registry.UnbindTask(runnerID, taskID)

	case protocol.RunnerMessageType_Heartbeat:
		if !h.Registry.SetLastSeen(runnerID, now) {
			slog.Error("RunnerHandler: Heartbeat from unknown runner", "runnerID", runnerID)
			return
		}

	default:
		slog.Error("RunnerHandler: unhandled message kind", "runnerID", runnerID, "kind", msg.Kind)
		return
	}

	if h.OnChange != nil {
		h.OnChange()
	}
}
