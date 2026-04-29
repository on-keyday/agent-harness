package server

import (
	"log/slog"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// runnerForTask returns the ConnHandle for the runner currently executing tid,
// or nil if no live runner is associated with the task. The lookup uses
// TaskStore.AssignedTo (set by Assign on successful TryDispatch) and
// Registry.Get to resolve the connection handle.
func (s *Server) runnerForTask(tid protocol.TaskID) ConnHandle {
	taskIDHex := hexTaskIDProto(tid)
	task, ok := s.tasks.Get(taskIDHex)
	if !ok {
		return nil
	}
	runnerID := task.AssignedTo
	if runnerID == "" {
		return nil
	}
	entry, ok := s.registry.Get(runnerID)
	if !ok || entry.Conn == nil {
		return nil
	}
	return entry.Conn
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
	conn := s.runnerForTask(tid)
	if conn == nil {
		return
	}
	req := &protocol.RunnerRequest{Kind: protocol.RunnerRequestType_TaskWake}
	req.SetTaskWake(protocol.TaskWakeRequest{TaskId: tid})
	wireBytes, err := req.Append([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)})
	if err != nil {
		slog.Warn("emitTaskWake encode failed", "err", err)
		return
	}
	if _, _, err := conn.SendMessage(wireBytes); err != nil {
		slog.Warn("emitTaskWake send failed", "err", err)
	}
}

// wireAgentBoardWake registers the wake-on-delivery hook with the board.
// Called once during server initialisation (from SetBoard).
func (s *Server) wireAgentBoardWake(b *agentboard.Board) {
	b.SetOnDeliver(func(_ protocol.RunnerID, tid protocol.TaskID) {
		s.emitTaskWake(tid)
	})
}
