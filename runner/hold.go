package runner

import (
	"context"
	"encoding/hex"
	"log/slog"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
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

func (r *TaskRegistry) get(taskIDHex string) (*taskEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.tasks[taskIDHex]
	return e, ok
}

func (r *TaskRegistry) put(taskIDHex string, e *taskEntry) {
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
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hold != nil && r.hold.tasks[taskIDHex] {
		return
	}
	delete(r.tasks, taskIDHex)
}

func (r *TaskRegistry) snapshot() map[string]*taskEntry {
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
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.hold != nil
}

// liveHeldTasks is the set a hold may cover: tasks whose child is actually
// RUNNING. An entry exists from registration, which is before the worktree is
// created and before anything is spawned, so acking every entry would promise
// children that do not exist and the server would write task_held for them.
func (r *TaskRegistry) liveHeldTasks() []string {
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
