package server

import (
	"log/slog"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// runnerForTask returns the ConnHandle for the runner currently executing tid,
// or nil if no live runner is associated with the task. The lookup uses
// TaskStore.AssignedTo (set by Assign on successful TryDispatch) and
// Registry.Get to resolve the connection handle.
//
// The second return names WHICH lookup came up empty, for the caller to log.
// Three different states reach the same nil here — an unknown task, one no
// runner has picked up, and one whose runner has gone — and a wake that is
// dropped is otherwise indistinguishable from a wake that was delivered and
// ignored by the agent. That ambiguity cost a whole investigation on
// 2026-08-29: a message was marked shown_to=1/1 on the board while no turn
// ever started, and nothing anywhere recorded which half had failed.
func (s *Server) runnerForTask(tid protocol.TaskID) (ConnHandle, string) {
	taskIDHex := hexTaskIDProto(tid)
	task, ok := s.tasks.Get(taskIDHex)
	if !ok {
		return nil, "no such task"
	}
	runnerID := task.AssignedTo
	if runnerID == "" {
		return nil, "task not assigned to a runner"
	}
	entry, ok := s.registry.Get(runnerID)
	if !ok {
		return nil, "assigned runner not in the registry"
	}
	if entry.Conn == nil {
		return nil, "assigned runner has no live connection"
	}
	return entry.Conn, ""
}

// hexTaskIDProto converts a protocol.TaskID to its hex string representation,
// matching the key format used by TaskStore.
func hexTaskIDProto(tid protocol.TaskID) string {
	const hextable = "0123456789abcdef"
	buf := make([]byte, 32)
	for i, b := range tid.Id {
		buf[i*2] = hextable[b>>4]
		buf[i*2+1] = hextable[b&0xf]
	}
	return string(buf)
}

// emitTaskWake builds a RunnerRequest{task_wake} and sends it to the
// runner hosting tid. No-op if no live runner is associated with tid
// (race against TaskFinished is benign — the wake is dropped silently).
func (s *Server) emitTaskWake(tid protocol.TaskID) {
	taskIDHex := hexTaskIDProto(tid)
	conn, why := s.runnerForTask(tid)
	if conn == nil {
		// Info, not Warn: losing the race against TaskFinished is ordinary and
		// the message itself is never at risk — it stays on the board and
		// reaches the agent through its next UserPromptSubmit inbox read. What
		// this line buys is the ability to tell that ordinary case apart from a
		// wake that WAS sent, which is the question a missed wake actually
		// raises.
		slog.Info("task wake not emitted", "task_id", taskIDHex, "reason", why)
		return
	}
	req := &protocol.RunnerRequest{Kind: protocol.RunnerRequestType_TaskWake}
	req.SetTaskWake(protocol.TaskWakeRequest{TaskId: tid})
	wireBytes, err := req.Append([]byte{byte(appwire.AppKind_RunnerControl)})
	if err != nil {
		slog.Warn("emitTaskWake encode failed", "err", err)
		return
	}
	if _, _, err := conn.SendMessage(wireBytes); err != nil {
		slog.Warn("emitTaskWake send failed", "task_id", taskIDHex, "err", err)
		return
	}
	slog.Info("task wake emitted", "task_id", taskIDHex)
}

// wireAgentBoardWake registers the wake-on-delivery hook with the board.
// Called once during server initialisation (from SetBoard).
func (s *Server) wireAgentBoardWake(b *agentboard.Board) {
	b.SetOnDeliver(func(_ protocol.RunnerID, tid protocol.TaskID) {
		s.emitTaskWake(tid)
	})
}
