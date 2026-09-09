package runner

import (
	"context"
	"encoding/hex"
	"log/slog"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// Holding tasks across a DELIBERATE server restart — the runner's half.
// docs/superpowers/specs/2026-09-10-task-hold-across-server-restart-design.md

// TaskRegistry holds this runner PROCESS's live tasks.
//
// It exists ABOVE the connection, and that is the whole reason it exists.
// Session is created once per connection, so a task map living in it takes
// every task's cancel func, wake writer and relay with it when the link drops
// — which is fine when a disconnect kills the children and fatal when the
// point is to keep them. Same move the identity change made for RunnerID:
// minted once by the process, handed to each Session.
//
// Mint it with NewTaskRegistry and put it on Config before PersistLoop starts.
// A nil one is tolerated by Session (it makes its own, per connection) so that
// tests and the single-shot Run path keep working unchanged; only a runner that
// wants holds needs to supply one.
type TaskRegistry struct {
	mu    sync.Mutex
	tasks map[string]*taskEntry

	// hold is the hold currently armed, if any. Set by a HoldTasksRequest and
	// cleared when it expires, when the children are killed, or when the
	// server accepts or refuses each task after the reconnect.
	hold *armedHold
}

// armedHold is a promise this runner made to a server that was going down: keep
// these children alive with no server until the deadline.
type armedHold struct {
	id       protocol.HoldID
	deadline time.Time
	// tasks is what was acked, by task id hex. The report on reconnect is
	// built from this, and anything the server does not accept is killed.
	tasks map[string]bool
	timer *time.Timer
}

func NewTaskRegistry() *TaskRegistry {
	return &TaskRegistry{tasks: make(map[string]*taskEntry)}
}

// Every method tolerates a nil receiver. Reading a nil MAP is legal in Go and
// several call sites relied on that before the map became this type, so a nil
// registry has to keep meaning "no tasks" rather than panicking — which is
// what it did the moment the map moved behind a mutex.
func (r *TaskRegistry) get(taskIDHex string) (*taskEntry, bool) {
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.tasks[taskIDHex]
	return e, ok
}

func (r *TaskRegistry) put(taskIDHex string, e *taskEntry) {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.tasks == nil {
		r.tasks = make(map[string]*taskEntry)
	}
	r.tasks[taskIDHex] = e
}

// remove drops a task, unless a hold is armed and covers it: a held task
// outlives the goroutine that ran it, because that goroutine's deferred
// cleanup fires on the disconnect this hold exists to survive.
func (r *TaskRegistry) remove(taskIDHex string) {
	if r == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hold != nil && r.hold.tasks[taskIDHex] {
		return
	}
	delete(r.tasks, taskIDHex)
}

func (r *TaskRegistry) snapshot() map[string]*taskEntry {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	out := make(map[string]*taskEntry, len(r.tasks))
	for k, v := range r.tasks {
		out[k] = v
	}
	return out
}

// holdArmed reports whether a hold is currently in force. The disconnect path
// reads it to decide whether to cancel every task context or leave the
// children running — which is the difference between today's behaviour and a
// hold, made into one visible decision.
func (r *TaskRegistry) holdArmed() bool {
	if r == nil {
		return false
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hold != nil
}

// liveHeldTasks is the set a hold may cover: tasks whose child is actually
// RUNNING. An entry exists from registration, which is before the worktree is
// created and before anything is spawned, so acking every entry would promise
// children that do not exist and the server would write task_held for them.
func (r *TaskRegistry) liveHeldTasks() []string {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.tasks))
	for id, e := range r.tasks {
		if e != nil && e.childLive() {
			out = append(out, id)
		}
	}
	return out
}

// arm records a hold and starts its timer. Returns the task ids it covers.
func (r *TaskRegistry) arm(id protocol.HoldID, window time.Duration, ids []string, onExpire func()) {
	if r == nil {
		return
	}

	r.mu.Lock()
	if r.hold != nil && r.hold.timer != nil {
		r.hold.timer.Stop()
	}
	covered := make(map[string]bool, len(ids))
	for _, t := range ids {
		covered[t] = true
	}
	h := &armedHold{id: id, deadline: time.Now().Add(window), tasks: covered}
	r.hold = h
	r.mu.Unlock()
	// The timer runs on the runner's own monotonic clock from the moment the
	// request ARRIVED, not from the server's stamp: the two are never
	// compared, and this one is the promise actually being kept.
	h.timer = time.AfterFunc(window, onExpire)
}

// disarm forgets the hold and returns it, stopping its timer.
func (r *TaskRegistry) disarm() *armedHold {
	if r == nil {
		return nil
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	h := r.hold
	r.hold = nil
	if h != nil && h.timer != nil {
		h.timer.Stop()
	}
	return h
}

// heldReport is what rides in RunnerHello: everything this process is still
// holding, each entry carrying the ticket its agent is still presenting.
func (r *TaskRegistry) heldReport() protocol.HeldTasksReport {
	if r == nil {
		return protocol.HeldTasksReport{}
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	var rep protocol.HeldTasksReport
	if r.hold == nil {
		return rep
	}
	rep.HoldId = r.hold.id
	tasks := make([]protocol.HeldTask, 0, len(r.hold.tasks))
	for id := range r.hold.tasks {
		e := r.tasks[id]
		if e == nil || !e.childLive() {
			continue // died during the gap; do not promise it
		}
		var tid protocol.TaskID
		raw, err := hex.DecodeString(id)
		if err != nil || len(raw) != len(tid.Id) {
			continue
		}
		copy(tid.Id[:], raw)
		tasks = append(tasks, protocol.HeldTask{TaskId: tid, Ticket: e.ticket})
	}
	rep.SetTasks(tasks)
	return rep
}

// killHeldExcept ends every held child the server did not accept, and clears
// the hold. Called with the accepted list from RunnerHelloResponse.
//
// Absence is the signal, not presence: an accepted-list the server forgot to
// fill kills children, which is loud and recoverable, where a refused-list it
// forgot to fill would strand them silently. It is also the only path that
// reaches a task cancelled while it was held — CancelTask can never be
// delivered to one.
func (r *TaskRegistry) killHeldExcept(accepted map[string]bool, log *slog.Logger) {
	h := r.disarm()
	if h == nil {
		return
	}
	for id := range h.tasks {
		if accepted[id] {
			continue
		}
		e, ok := r.get(id)
		if !ok || e == nil {
			continue
		}
		log.Info("hold: killing a child the server did not re-adopt", "task", id)
		e.cancel()
		r.remove(id)
	}
}

// killAllHeld ends every held child. The expiry path: the window passed with
// no server coming back.
func (r *TaskRegistry) killAllHeld(log *slog.Logger) {
	h := r.disarm()
	if h == nil {
		return
	}
	for id := range h.tasks {
		e, ok := r.get(id)
		if !ok || e == nil {
			continue
		}
		log.Warn("hold: window expired, killing the child", "task", id)
		e.cancel()
		r.remove(id)
	}
}

// holdCtxRoot is the context every task is rooted at when a TaskRegistry is
// supplied: the PROCESS's context, not the connection's.
//
// Without this, taskCtx descends from the per-connection runCtx and every
// child dies the moment the link drops — which is correct today and is exactly
// what a hold has to prevent. With it, the disconnect path makes an explicit
// choice instead (see Session.cancelTasksUnlessHeld).
func (r *TaskRegistry) rootCtx(processCtx context.Context) context.Context {
	if processCtx == nil {
		return context.Background()
	}
	return processCtx
}

// --- Session side of the hold ------------------------------------------------

// handleHoldTasks answers a shutting-down server's request to keep this
// runner's children alive.
//
// Order matters: stop draining FIRST, then ack. The ack means "I have stopped
// reading these tasks' output", which is what lets the server treat the drain
// boundary as a statement instead of a guess — after it, no new frame can
// follow for a held session, so whatever is still in flight is bounded and the
// server can capture a screen that is actually current.
func (s *Session) handleHoldTasks(req *protocol.HoldTasksRequest) {
	log := s.logger()
	if s.reg == nil {
		// No process-level registry: this runner cannot hold anything, and
		// acking would promise what it will not keep.
		log.Info("hold: requested but this runner has no task registry")
		return
	}
	window := time.Duration(req.HoldMs) * time.Millisecond
	if window <= 0 {
		return
	}

	// Only tasks with a LIVE child. An entry exists from registration, which
	// precedes the worktree and the spawn, so acking every entry would have
	// the server writing task_held for children that do not exist.
	ids := s.reg.liveHeldTasks()

	// ARM FIRST, THEN DETACH. This order is not cosmetic and the other one is
	// not merely racy — it fails every time.
	//
	// detach() wakes whatever is parked in the relay, and the relay decides
	// between "gap" and "end" by asking whether a hold is armed. Detaching
	// before arming therefore wakes the frame reader into a window where
	// far == nil and holdArmed() is still false, so it takes the END branch
	// and returns io.EOF. That return closes agentexec's stdin pipe, which
	// ends io.Copy(pty, pipeOut), which fires its SIGHUP -> SIGTERM -> SIGKILL
	// ladder at the child. Measured on a dummy instance: the server armed, the
	// runner logged "leaving children alive", and the child was dead before
	// the next sample.
	s.reg.arm(req.HoldId, window, ids, func() {
		// The window passed with no server coming back.
		s.reg.killAllHeld(s.logger())
	})

	// Now stop draining. Each relay parks its reads and retains what it cannot
	// forward, so the PTY buffer fills behind it and nothing is dropped; the
	// kernel is the gap buffer.
	for _, id := range ids {
		if e, ok := s.reg.get(id); ok && e != nil && e.relay != nil {
			e.relay.detach()
		}
	}

	var m protocol.RunnerMessage
	m.Kind = protocol.RunnerMessageType_HoldTasksAck
	ack := protocol.HoldTasksAck{HoldId: req.HoldId}
	tasks := make([]protocol.TaskID, 0, len(ids))
	for _, id := range ids {
		var tid protocol.TaskID
		raw, err := hex.DecodeString(id)
		if err != nil || len(raw) != len(tid.Id) {
			continue
		}
		copy(tid.Id[:], raw)
		tasks = append(tasks, tid)
	}
	if !ack.SetTasks(tasks) {
		log.Error("hold: too many tasks for one ack", "count", len(tasks))
		return
	}
	m.SetHoldTasksAck(ack)
	if err := s.Sender.Send(m.MustAppend([]byte{byte(appwire.AppKind_RunnerControl)})); err != nil {
		log.Warn("hold: ack send failed", "err", err)
		return
	}
	log.Info("hold: armed", "tasks", len(tasks), "window", window)
}

// handleRebindSession points a held task's relay at a fresh server stream.
//
// The PTY, the child and everything spliced to it are untouched: only the far
// end of the relay changes, which is why agentexec — holding the relay as its
// stream for the whole session — never learns that anything happened.
func (s *Session) handleRebindSession(req *protocol.RebindSessionRequest) {
	log := s.logger()
	taskIDHex := hex.EncodeToString(req.TaskId.Id[:])
	e, ok := s.reg.get(taskIDHex)
	if !ok || e == nil || e.relay == nil {
		s.reportRebindFailed(req.TaskId, "no held session for this task")
		return
	}
	if !e.childLive() {
		// The child died between the report and this request. Answered with
		// the existing lifecycle vocabulary rather than a new message: the
		// server already handles TaskFinished as Finish + UnbindTask + Revoke.
		s.reportRebindFailed(req.TaskId, "child exited during the gap")
		return
	}
	// The same wait handleOpenExec uses: the server creates the stream and we
	// look it up by id, which can take a moment to arrive.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream := peer.WaitForBidirectionalStream(ctx, s.Streams, trsf.StreamID(req.StreamId))
	if stream == nil {
		s.reportRebindFailed(req.TaskId, "stream lookup failed")
		return
	}
	e.relay.rebind(stream)
	log.Info("hold: session rebound", "task", taskIDHex, "stream", req.StreamId)
}

// reportRebindFailed tells the server a rebind cannot be honoured, using
// TaskFinished so no new message type is needed for a case the existing
// lifecycle already describes: the task is over.
func (s *Session) reportRebindFailed(tid protocol.TaskID, why string) {
	s.logger().Warn("hold: rebind failed", "task", hex.EncodeToString(tid.Id[:]), "reason", why)
	var m protocol.RunnerMessage
	m.Kind = protocol.RunnerMessageType_TaskFinished
	m.SetTaskFinished(protocol.TaskFinished{
		TaskId:       tid,
		ExitCode:     -1,
		ErrorMessage: []byte("rebind_failed: " + why),
	})
	_ = s.Sender.Send(m.MustAppend([]byte{byte(appwire.AppKind_RunnerControl)}))
}

// cancelTasksUnlessHeld is the disconnect decision, and it is the whole reason
// task contexts were moved off the connection.
//
// With a hold armed the children stay and their relays park. Without one every
// task is cancelled, which is what used to happen implicitly through context
// parentage — the same outcome, now written down where it can be read.
func (s *Session) cancelTasksUnlessHeld() {
	if s.reg == nil {
		return
	}
	if s.reg.holdArmed() {
		s.logger().Info("disconnect: a hold is armed, leaving children alive")
		return
	}
	for id, e := range s.reg.snapshot() {
		if e == nil {
			continue
		}
		e.cancel()
		s.reg.remove(id)
	}
}
