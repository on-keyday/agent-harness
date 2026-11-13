package server

import (
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/trsf"
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
	GetReceiveStream(id trsf.StreamID) trsf.ReceiveStream
	// GetBidirectionalStream looks up a peer-created bidi stream by id. Used to
	// pick up a runner-created remote-forward data stream (the runner creates it
	// per accepted connection and sends the id via RemoteForwardConn).
	GetBidirectionalStream(id trsf.StreamID) trsf.BidirectionalStream
	// SendDatagram relays one unreliable payload; MaxDatagramSize says what
	// currently fits in one and MOVES with PLPMTUD.
	//
	// Here rather than only on streamingConn because the udp forward relay
	// reaches the FAR leg through this interface: the registry hands back a
	// runner's ConnHandle, and that is the only reference the relay has to it.
	//
	// The error is returned rather than logged inside, because only the caller
	// knows which forward the payload belonged to — and a drop counted against
	// no row is one no operator can find.
	SendDatagram(b []byte) error
	MaxDatagramSize() int
}

type Dispatcher struct {
	OnRunnerControl func(ConnHandle, []byte) // payload is everything after the kind byte
	OnTaskControl   func(ConnHandle, []byte)

	// OnTelemetryResponse receives a peer's answer to a question this server
	// asked it. The only server -> peer request direction there is; see
	// server/telemetry.go for why one exists.
	OnTelemetryResponse func(payload []byte)

	// RecordClientIdentity records the client kind / principal for the given
	// connection WITHOUT sending a wire response. Called by pskDispatchIdentity
	// for the client role so the merged-PSK gate does not emit a redundant
	// TaskControlResponse{ClientHello} on top of the gate's PskAuthResponse.
	// Wired by Server.New to s.taskHandler.RecordClientIdentity.
	// When nil, pskDispatchIdentity falls back to the re-dispatch path (old
	// behaviour, still correct for tests that do not wire this field).
	RecordClientIdentity func(cid string, conn ConnHandle, hello *protocol.ClientHello) protocol.ClientHelloStatus

	// Registry and Tasks are used by OnCancel.
	Registry *Registry
	Tasks    *TaskStore

	// Board is the agentboard instance for ticket lifecycle management.
	// When nil, ticket registration is skipped (safe for tests that do not wire a Board).
	Board *agentboard.Board

	// CancelResendDelays overrides defaultCancelResendDelays; nil means the
	// default. Tests shorten it.
	CancelResendDelays []time.Duration

	resendMu  sync.Mutex
	resending map[string]bool // task id hex → a resend loop is running
}

// Dispatch routes msg by inspecting the first byte (the wire kind).
// If msg is empty, Dispatch is a no-op.
// Unknown / unhandled kinds are ignored silently.
func (d *Dispatcher) Dispatch(conn ConnHandle, msg []byte) {
	if len(msg) == 0 {
		return
	}

	kind := appwire.AppKind(msg[0])
	payload := msg[1:]

	switch kind {
	case appwire.AppKind_RunnerControl:
		if d.OnRunnerControl != nil {
			d.OnRunnerControl(conn, payload)
		}
	case appwire.AppKind_TaskControl:
		if d.OnTaskControl != nil {
			d.OnTaskControl(conn, payload)
		}
	case appwire.AppKind_Telemetry:
		// The ANSWER to something this server asked. The request travels the
		// other way on the same kind, and the direction is what tells them
		// apart -- a peer never asks and this server never answers.
		if d.OnTelemetryResponse != nil {
			d.OnTelemetryResponse(payload)
		}
	default:
		// An unroutable kind used to be dropped in silence, which is the worst
		// shape a version skew can take: the peer's command waits out its own
		// timeout and nothing anywhere says why. The case that makes this
		// concrete is 0x44, the retired agent_message kind -- a harness-cli
		// built before the agent verbs moved to task control still sends it,
		// and the answer it needs is "your binary is older than this server",
		// which only a line like this can give.
		//
		// The conn guard is not defensive clutter: this arm exists FOR
		// unexpected input, so it must not itself be a way to crash the
		// server.
		cid := "<none>"
		if conn != nil {
			cid = conn.ConnectionID().String()
		}
		slog.Warn("dispatch: unroutable app kind; dropping",
			"kind", fmt.Sprintf("0x%02x", byte(kind)), "bytes", len(msg), "cid", cid)
	}
}

// buildAssignMsg constructs the RunnerControl/AssignTask request
// message that carries only TaskID + StreamID. The actual body
// (auth_ticket / repo_path / prompt / extra_args) is encoded separately
// and written to the trsf send-stream with the returned ID; the runner
// reads from that stream and decodes AssignTaskBody.
//
// Streamed via trsf instead of inline so long prompts don't exceed path
// MTU on UDP transport.
func buildAssignMsg(task TaskEntry, ticket [16]byte, streamID uint64) (*protocol.RunnerRequest, []byte, error) {
	var tid protocol.TaskID
	raw, _ := hex.DecodeString(task.ID)
	copy(tid.Id[:], raw)

	assign := protocol.AssignTask{
		TaskId:   tid,
		StreamId: streamID,
	}

	req := &protocol.RunnerRequest{Kind: protocol.RunnerRequestType_AssignTask}
	req.SetAssignTask(assign)

	body := protocol.AssignTaskBody{
		AuthTicket: ticket,
		ExtraArgs:  protocol.AgentArgsFromStrings(task.ExtraArgs),
	}
	body.SetResumeConversation(task.ResumeConversation)
	body.SetRepoPath([]byte(task.RepoPath))
	body.SetPrompt([]byte(task.Prompt))
	body.SetAgentProfile([]byte(task.AgentProfile))
	bodyBytes, err := body.EncodeCopy(nil)
	if err != nil {
		return nil, nil, err
	}
	return req, bodyBytes, nil
}

// taskIDFromHex converts a hex task ID string to a protocol.TaskID.
func taskIDFromHex(taskIDHex string) protocol.TaskID {
	var tid protocol.TaskID
	raw, _ := hex.DecodeString(taskIDHex)
	copy(tid.Id[:], raw)
	return tid
}

// defaultCancelResendDelays is how long OnCancel waits before each resend of a
// CancelTask that has not been answered by a TaskFinished: about 2.5 minutes in
// all. On the PTY path a cancel hangs the child up (SIGHUP, then SIGTERM and
// SIGKILL within 2s; a context cancel backstops it at 5s), and a stream
// session's process group gets SIGTERM and SIGKILL after 2s (pillar A spec,
// A-D16); a oneshot's SIGTERM has 5s of grace (runner/process.go). So the first
// resend can precede the answer; a resent CancelTask is harmless (the runner's
// second cancel of a session is a no-op).
var defaultCancelResendDelays = []time.Duration{
	3 * time.Second, 6 * time.Second, 12 * time.Second, 24 * time.Second,
	30 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second,
}

// OnCancel looks up the runner that is executing taskID (AssignedTo) and sends a
// CancelTask message to it. A task that was never dispatched gets nothing: it
// has no runner-side state, and handleSubmit's BoundRunnerID (the candidate it
// picked at submit) is not where it runs. Sending there was once harmless — the
// runner ignored an unknown id — but a runner now answers one with
// TaskFinished(-1), which would turn a cancelled Queued task into Failed. Capacity is
// intentionally NOT released here; the TaskFinished message from the runner (or
// the runner-disconnect path) will call UnbindTask.
//
// The task's agentboard ticket is revoked HERE, not when the TaskFinished
// arrives, because the CancelTask is one objproto datagram — unacknowledged,
// and over UDP lost without a word. Measured on a dummy instance with the send
// dropped: the row read Cancelled while the agent ran on and its ticket still
// published to the board. Revoking at the decision makes the cancel take the
// agent's harness access whether or not the runner ever hears of it.
//
// It is also why the CancelTask is resent until a TaskFinished turns the row
// from Cancelled into its real outcome (see resendCancelUntilFinished).
func (d *Dispatcher) OnCancel(taskID string) {
	if d.Registry == nil || d.Tasks == nil {
		return
	}
	task, ok := d.Tasks.Get(taskID)
	if !ok {
		return
	}
	if task.AssignedTo.IsZero() {
		return
	}
	boardRevokeTask(d.Board, task.AssignedTo, taskID)
	entry, found := d.Registry.GetByIdentity(task.AssignedTo)
	if !found || entry.Conn == nil {
		return
	}
	d.sendCancel(entry, taskID)
	d.resendCancelUntilFinished(taskID)
}

func (d *Dispatcher) sendCancel(entry RunnerEntry, taskID string) {
	req := &protocol.RunnerRequest{Kind: protocol.RunnerRequestType_CancelTask}
	req.SetCancelTask(protocol.CancelTask{TaskId: taskIDFromHex(taskID)})
	if err := sendRunnerRequest(entry.Conn, req); err != nil {
		// Per spec: capacity is NOT released on send fail; TaskFinished path handles it.
		slog.Error("dispatcher: OnCancel send failed", "runner", entry.ID, "task", taskID, "err", err)
	}
}

// resendCancelUntilFinished resends the CancelTask on the schedule in
// CancelResendDelays while the row still reads Cancelled — the state between
// the decision and the runner's TaskFinished, which TaskStore.Finish replaces
// with the real outcome. It stops when that happens, when the runner is no
// longer registered (its disconnect path ends the task instead), or when the
// schedule runs out. One loop per task: a repeated cancel joins the running one.
//
// The runner answers a CancelTask for a task it does not have with a
// TaskFinished (runner.Session.handleCancelTask), so a lost TaskFinished, or an
// AssignTask that never arrived, ends the loop too rather than exhausting it.
func (d *Dispatcher) resendCancelUntilFinished(taskID string) {
	delays := d.CancelResendDelays
	if delays == nil {
		delays = defaultCancelResendDelays
	}
	d.resendMu.Lock()
	if d.resending == nil {
		d.resending = make(map[string]bool)
	}
	if d.resending[taskID] {
		d.resendMu.Unlock()
		return
	}
	d.resending[taskID] = true
	d.resendMu.Unlock()

	go func() {
		defer func() {
			d.resendMu.Lock()
			delete(d.resending, taskID)
			d.resendMu.Unlock()
		}()
		for _, delay := range delays {
			time.Sleep(delay)
			task, ok := d.Tasks.Get(taskID)
			if !ok || task.Status != protocol.TaskStatus_Cancelled {
				return
			}
			entry, found := d.Registry.GetByIdentity(task.AssignedTo)
			if !found || entry.Conn == nil {
				return
			}
			slog.Info("dispatcher: resending CancelTask; no TaskFinished yet", "runner", entry.ID, "task", taskID)
			d.sendCancel(entry, taskID)
		}
		slog.Warn("dispatcher: CancelTask never answered by a TaskFinished; giving up", "task", taskID, "sends", len(delays)+1)
	}()
}
