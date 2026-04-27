package server

import (
	"encoding/hex"
	"log/slog"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/trsf"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// ConnHandle is the minimal interface a handler needs to identify, reply to,
// and stream bulk data back to a peer. Decoupled from the concrete
// objproto.Connection / trsf.Transport so tests can fake it.
//
// CreateSendStream returns a server-initiated unidirectional stream. Handlers
// use it for responses too large to fit in a single objproto message
// (e.g. GetTaskLog returning a full log file). May return nil in tests where
// the fake doesn't wire trsf.
//
// CreateBidirectionalStream returns a server-initiated bidirectional stream
// for handlers that need to splice bytes both ways with the peer (e.g.
// OpenInteractive opening an interactive PTY claude over a frame-multiplexed
// stream that the runner writes to and the client reads from). Like
// CreateSendStream, may return nil in tests.
type ConnHandle interface {
	ConnectionID() objproto.ConnectionID
	SendMessage(b []byte) (int, uint64, error)
	CreateSendStream() trsf.SendStream
	CreateBidirectionalStream() trsf.BidirectionalStream
}

type Dispatcher struct {
	OnRunnerControl func(ConnHandle, []byte) // payload is everything after the kind byte
	OnTaskControl   func(ConnHandle, []byte)

	// Registry and Tasks are used by TryDispatch and OnCancel.
	Registry *Registry
	Tasks    *TaskStore
}

// Dispatch routes msg by inspecting the first byte (the wire kind).
// If msg is empty, Dispatch is a no-op.
// Unknown / unhandled kinds are ignored silently.
func (d *Dispatcher) Dispatch(conn ConnHandle, msg []byte) {
	if len(msg) == 0 {
		return
	}

	kind := wire.ApplicationPayloadKind(msg[0])
	payload := msg[1:]

	switch kind {
	case wire.ApplicationPayloadKind_RunnerControl:
		if d.OnRunnerControl != nil {
			d.OnRunnerControl(conn, payload)
		}
	case wire.ApplicationPayloadKind_TaskControl:
		if d.OnTaskControl != nil {
			d.OnTaskControl(conn, payload)
		}
	}
}

// buildAssignMsg constructs the wire bytes for a RunnerControl/AssignTask message.
func buildAssignMsg(task TaskEntry) ([]byte, error) {
	var tid protocol.TaskID
	raw, _ := hex.DecodeString(task.ID)
	copy(tid.Id[:], raw)

	assign := protocol.AssignTask{TaskId: tid}
	assign.SetRepoPath([]byte(task.RepoPath))
	assign.Prompt = []byte(task.Prompt)

	req := &protocol.RunnerRequest{Kind: protocol.RunnerRequestType_AssignTask}
	req.SetAssignTask(assign)
	data, err := req.Append([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)})
	return data, err
}

// TryDispatch attempts to find an available runner for the given queued task,
// bind it, and send an AssignTask message. Returns true on success (the task
// is transitioned to Running). Returns false if no suitable runner has
// capacity or if every send attempt fails.
//
// On send failure the reservation is rolled back via Registry.UnbindTask so the
// slot is immediately available for the next Tick. The task remains Queued.
func (d *Dispatcher) TryDispatch(task TaskEntry) bool {
	if d.Registry == nil || d.Tasks == nil {
		return false
	}

	candidates := d.Registry.Candidates(task.RepoPath, task.Selector)
	for _, runner := range candidates {
		// Skip runners that have no remaining capacity.
		if len(runner.ActiveTasks) >= runner.MaxTasks {
			continue
		}
		// Skip runners without an active connection.
		if runner.Conn == nil {
			continue
		}

		if !d.Registry.BindTask(runner.ID, task.ID) {
			// Race: another goroutine filled the slot between Candidates and BindTask.
			continue
		}

		msg, err := buildAssignMsg(task)
		if err != nil {
			slog.Error("dispatcher: buildAssignMsg failed", "task", task.ID, "err", err)
			d.Registry.UnbindTask(runner.ID, task.ID)
			continue
		}

		if _, _, err := runner.Conn.SendMessage(msg); err != nil {
			slog.Error("dispatcher: SendMessage failed, rolling back", "runner", runner.ID, "task", task.ID, "err", err)
			d.Registry.UnbindTask(runner.ID, task.ID)
			continue
		}

		// Send succeeded: transition task to Running.
		d.Tasks.Assign(task.ID, runner.ID, "")
		return true
	}
	return false
}

// OnCancel looks up the runner that is executing taskID (via AssignedTo, falling
// back to BoundRunnerID) and sends a CancelTask message to it. Capacity is
// intentionally NOT released here; the TaskFinished message from the runner (or
// the runner-disconnect path) will call UnbindTask.
func (d *Dispatcher) OnCancel(taskID string) {
	if d.Registry == nil || d.Tasks == nil {
		return
	}
	task, ok := d.Tasks.Get(taskID)
	if !ok {
		return
	}
	// AssignedTo is set by Assign() when TryDispatch succeeds; BoundRunnerID is
	// the pinned runner from the submit-time selector. Prefer AssignedTo.
	runnerID := task.AssignedTo
	if runnerID == "" {
		runnerID = task.BoundRunnerID
	}
	if runnerID == "" {
		// Task was never dispatched to a runner; nothing to forward.
		return
	}
	entry, ok := d.Registry.Get(runnerID)
	if !ok || entry.Conn == nil {
		return
	}

	var tid protocol.TaskID
	raw, _ := hex.DecodeString(taskID)
	copy(tid.Id[:], raw)

	req := &protocol.RunnerRequest{Kind: protocol.RunnerRequestType_CancelTask}
	req.SetCancelTask(protocol.CancelTask{TaskId: tid})
	data, err := req.Append([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)})
	if err != nil {
		slog.Error("dispatcher: OnCancel encode failed", "task", taskID, "err", err)
		return
	}
	if _, _, err := entry.Conn.SendMessage(data); err != nil {
		// Per spec: capacity is NOT released on send fail; TaskFinished path handles it.
		slog.Error("dispatcher: OnCancel send failed", "runner", runnerID, "task", taskID, "err", err)
	}
}
