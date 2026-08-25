package server

import (
	"sort"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/trsf"
)

// execRun is one running out-of-band exec: a command the server asked a runner
// to run in a task's worktree, as its own process rather than through the
// session's PTY.
//
// The server holds it so `exec ls` can report it and `exec kill` can reach it.
// Nothing here is persisted: an exec dies with its caller, so its registration
// cannot outlive the process that would replay a WAL.
//
// "Dies with its caller" is enforced by DropExecRunsForConn, not by the stream:
// a client that dies abruptly closes nothing, and the runner's frame pump reads
// the resulting EOF as end-of-input while the child runs on.
type execRun struct {
	execID    uint64
	taskIDHex string
	// runnerID is TaskEntry.AssignedTo, kept so a kill can re-find the runner
	// without re-reading the task — which may have been pruned meanwhile.
	runnerID   string
	argv       []string
	startedAt  time.Time
	control    trsf.SendStream
	clientCID  string
	clientKind protocol.ClientKind
}

// execRegistry maps a server-assigned execId to its registration. Safe for
// concurrent use. Lives on TaskHandler beside portForwardRegistry, and is
// shaped like it for the same reason: the operator wants to see and stop
// something that belongs to no task row.
type execRegistry struct {
	mu   sync.Mutex
	next uint64
	m    map[uint64]*execRun
}

func newExecRegistry() *execRegistry {
	return &execRegistry{m: map[uint64]*execRun{}}
}

// execs returns the registry, creating it on first use so struct-literal
// construction in tests need not set it — the same shape as pforwards().
func (h *TaskHandler) execs() *execRegistry {
	h.execRunsOnce.Do(func() {
		h.execRuns = newExecRegistry()
	})
	return h.execRuns
}

// add assigns the next execId, stores e under it, and returns the id.
//
// Ids start at 1, so 0 is never a real one and a zero-valued field is
// unambiguously "none" — the forward registry's counter draws the same line.
func (r *execRegistry) add(e *execRun) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	e.execID = r.next
	if e.startedAt.IsZero() {
		e.startedAt = time.Now()
	}
	r.m[e.execID] = e
	return e.execID
}

func (r *execRegistry) get(id uint64) (*execRun, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.m[id]
	return e, ok
}

// remove drops the registration and reports whether it was there. Removing
// twice is not an error: a runner can report a finish for an exec whose client
// already went away and took the registration with it.
func (r *execRegistry) remove(id uint64) (*execRun, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.m[id]
	if ok {
		delete(r.m, id)
	}
	return e, ok
}

// list returns the registrations ascending by id. taskFilter "" means all.
//
// Ordered because the operator reads it as a table: an unstable order makes a
// stable set look like it is churning between two calls.
func (r *execRegistry) list(taskFilter string) []*execRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*execRun, 0, len(r.m))
	for _, e := range r.m {
		if taskFilter != "" && e.taskIDHex != taskFilter {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].execID < out[j].execID })
	return out
}

// countForTask is what TaskInfo.exec_count reports. uint16 because that is the
// field's width; a task with more than 65535 concurrent execs has a different
// problem than a truncated count.
func (r *execRegistry) countForTask(taskIDHex string) uint16 {
	r.mu.Lock()
	defer r.mu.Unlock()
	var n uint16
	for _, e := range r.m {
		if e.taskIDHex == taskIDHex {
			n++
		}
	}
	return n
}
