package server

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"

	"github.com/on-keyday/agent-harness/agentboard"
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
	OnAgentMessage  func(ConnHandle, []byte) // payload is the full AgentMessage bytes (kind byte stripped)

	// Registry and Tasks are used by TryDispatch and OnCancel.
	Registry *Registry
	Tasks    *TaskStore

	// Board is the agentboard instance for ticket lifecycle management.
	// When nil, ticket registration is skipped (safe for tests that do not wire a Board).
	Board *agentboard.Board
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
	case wire.ApplicationPayloadKind_AgentMessage:
		if d.OnAgentMessage != nil {
			d.OnAgentMessage(conn, payload)
		}
	}
}

// buildAssignMsg constructs the wire bytes for a RunnerControl/AssignTask message.
// ticket is included verbatim in AssignTask.AuthTicket; pass [16]byte{} when no
// ticket is available (e.g. Scheduler path that does not yet generate tickets).
func buildAssignMsg(task TaskEntry, ticket [16]byte) ([]byte, error) {
	var tid protocol.TaskID
	raw, _ := hex.DecodeString(task.ID)
	copy(tid.Id[:], raw)

	assign := protocol.AssignTask{TaskId: tid, AuthTicket: ticket}
	assign.SetRepoPath([]byte(task.RepoPath))
	assign.Prompt = []byte(task.Prompt)

	req := &protocol.RunnerRequest{Kind: protocol.RunnerRequestType_AssignTask}
	req.SetAssignTask(assign)
	data, err := req.Append([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)})
	return data, err
}

// runnerIDFromConnID parses a runner's string ID (objproto.ConnectionID.String() format:
// "transport:ip:port-unique") into a protocol.RunnerID suitable for agentboard registry calls.
// Returns a placeholder RunnerID on parse error so callers can still proceed (the
// ticket will simply not match on validation, which is safe — the agent will get
// HelloStatusUnknownTask and reconnect).
func runnerIDFromConnID(id string) protocol.RunnerID {
	cid, err := objproto.ParseConnectionID(id, 0)
	if err != nil {
		// Fallback to loopback placeholder (safe: validation will fail, not panic).
		var rid protocol.RunnerID
		rid.SetTransport([]byte("ws"))
		rid.SetIpAddr([]byte{127, 0, 0, 1})
		return rid
	}
	var rid protocol.RunnerID
	rid.SetTransport([]byte(cid.Transport))
	ip := cid.Addr.Addr().AsSlice()
	rid.SetIpAddr(ip)
	rid.Port = uint16(cid.Addr.Port())
	rid.UniqueNumber = cid.ID
	return rid
}

// taskIDFromHex converts a hex task ID string to a protocol.TaskID.
func taskIDFromHex(taskIDHex string) protocol.TaskID {
	var tid protocol.TaskID
	raw, _ := hex.DecodeString(taskIDHex)
	copy(tid.Id[:], raw)
	return tid
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

		// Generate a fresh ticket for the agent Hello handshake.
		var ticket [16]byte
		if _, err := rand.Read(ticket[:]); err != nil {
			slog.Error("dispatcher: ticket generation failed", "task", task.ID, "err", err)
			d.Registry.UnbindTask(runner.ID, task.ID)
			continue
		}
		if d.Board != nil {
			d.Board.Registry().Register(runnerIDFromConnID(runner.ID), taskIDFromHex(task.ID), ticket)
		}

		msg, err := buildAssignMsg(task, ticket)
		if err != nil {
			slog.Error("dispatcher: buildAssignMsg failed", "task", task.ID, "err", err)
			if d.Board != nil {
				d.Board.Registry().Revoke(runnerIDFromConnID(runner.ID), taskIDFromHex(task.ID))
			}
			d.Registry.UnbindTask(runner.ID, task.ID)
			continue
		}

		if _, _, err := runner.Conn.SendMessage(msg); err != nil {
			slog.Error("dispatcher: SendMessage failed, rolling back", "runner", runner.ID, "task", task.ID, "err", err)
			if d.Board != nil {
				d.Board.Registry().Revoke(runnerIDFromConnID(runner.ID), taskIDFromHex(task.ID))
			}
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
