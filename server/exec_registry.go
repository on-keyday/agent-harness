package server

import (
	"sort"
	"sync"
	"sync/atomic"
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
	// runnerID is TaskEntry.AssignedTo — an IDENTITY, so a kill can re-find the
	// runner even after it has reconnected
	// without re-reading the task — which may have been pruned meanwhile.
	runnerID   protocol.RunnerID
	argv       []string
	startedAt  time.Time
	control    trsf.SendStream
	clientCID  string
	clientKind protocol.ClientKind
	// pty: the exec runs under a terminal (ExecRunRequest.pty). Fixed at open;
	// the listing reports it so a row whose stderr stays 0 says why.
	pty bool

	// Per-channel payload counters and last activity, written by the relay
	// (exec_splice.go) and read by the listing and the stats sweep.
	stdinBytes     atomic.Uint64
	stdoutBytes    atomic.Uint64
	stderrBytes    atomic.Uint64
	lastActivityMs atomic.Int64

	// Taps reading this exec, and how it ended once it has. ended is set under
	// tapMu by endTaps, so a tap attached after the end is finished at once
	// instead of waiting on a registration that will never produce anything.
	tapMu     sync.Mutex
	taps      []*execTap
	ended     bool
	endedKind protocol.ExecEventKind
	endedCode int32
	// outputPending is true while the runner→client relay still runs. The
	// runner reports the end on its control stream and the last output on the
	// data stream, unordered at the server, so the taps are finished only once
	// both have happened (or execTapEndGrace has passed since the end).
	outputPending bool
	tapsFinished  bool

	// The stats sweep's last publish, compared to decide whether to publish.
	statsMu       sync.Mutex
	lastPublished publishedExecCounters
}

// execRegistry maps a server-assigned execId to its registration. Safe for
// concurrent use. Lives on TaskHandler beside portForwardRegistry, and is
// shaped like it for the same reason: the operator wants to see and stop
// something that belongs to no task row.
type execRegistry struct {
	mu   sync.Mutex
	next uint64
	m    map[uint64]*execRun
	// gone holds client connections whose teardown has already dropped their
	// execs, and when. An open runs off the receive loop (it reads its command
	// from a stream), so it can finish after its connection's
	// DropExecRunsForConn — and a registration then would leave the child
	// running with nothing left to stop it. Kept only as long as an open can
	// be in flight; see goneTTL.
	gone map[string]time.Time
}

// goneTTL is how long a dropped connection is remembered. An open in flight
// waits at most a couple of seconds for its body, so a minute is ample and the
// map stays small for the life of the server.
const goneTTL = time.Minute

func newExecRegistry() *execRegistry {
	return &execRegistry{m: map[uint64]*execRun{}, gone: map[string]time.Time{}}
}

// markConnGone records that connID's execs have been dropped, so a later add
// for it is refused. It also forgets entries older than goneTTL.
func (r *execRegistry) markConnGone(connID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	for id, at := range r.gone {
		if now.Sub(at) > goneTTL {
			delete(r.gone, id)
		}
	}
	r.gone[connID] = now
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
// add registers e and returns its id, or 0 — never a real id — when e's
// client connection has already been dropped (markConnGone).
func (r *execRegistry) add(e *execRun) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, gone := r.gone[e.clientCID]; gone && e.clientCID != "" {
		return 0
	}
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

// publishedExecCounters is the last set published for an exec by the stats
// sweep (forward_events.go).
type publishedExecCounters struct {
	stdin, stdout, stderr uint64
	taps                  uint16
	valid                 bool
}
