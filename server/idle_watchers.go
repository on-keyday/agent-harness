package server

import (
	"encoding/hex"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// idleWatcher is one armed await-idle watcher. The server holds it so
// `session await-idle ls` can report it and `kill` can reach it. Nothing is
// persisted: a server restart drops every watcher, as it always has.
//
// Exactly one of fire, session stop, kill or connection teardown ends a
// watcher, and the one that does is whichever removes it from the registry
// first.
type idleWatcher struct {
	id        uint64
	taskIDHex string
	sink      protocol.AwaitIdleSink
	topic     string
	threshold time.Duration
	armedAt   time.Time
	// requester is the arming principal, resolved at arm time because the
	// board/notify sinks outlive the arming connection. Zero = operator.
	requester  protocol.TaskID
	clientCID  string
	clientKind protocol.ClientKind
	// stop ends the SessionMux goroutine early. Closed only by the path that
	// won the remove, so it is closed at most once.
	stop chan struct{}
	// onCancel is what a kill owes the requester: the reply sink's deferred
	// response with status=cancelled. nil for board and notify, which owe
	// nothing — not waking the recipient is the point of killing them.
	onCancel func()
}

// idleWatcherRegistry maps a server-assigned id to its watcher. Shaped like
// execRegistry, for the same reason: something the operator wants to see and
// stop that belongs to no task row.
type idleWatcherRegistry struct {
	mu   sync.Mutex
	next uint64
	m    map[uint64]*idleWatcher
}

func newIdleWatcherRegistry() *idleWatcherRegistry {
	return &idleWatcherRegistry{m: map[uint64]*idleWatcher{}}
}

// idleWatchers returns the registry, created on first use so struct-literal
// handlers in tests need not set it — the same shape as execs().
func (h *TaskHandler) idleWatchers() *idleWatcherRegistry {
	h.idleWatchersOnce.Do(func() {
		h.idleWatcherReg = newIdleWatcherRegistry()
	})
	return h.idleWatcherReg
}

// add assigns the next id (from 1, so 0 is never real) and stores w.
func (r *idleWatcherRegistry) add(w *idleWatcher) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.next++
	w.id = r.next
	if w.armedAt.IsZero() {
		w.armedAt = time.Now()
	}
	r.m[w.id] = w
	return w.id
}

func (r *idleWatcherRegistry) get(id uint64) (*idleWatcher, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.m[id]
	return w, ok
}

// remove drops the watcher and reports whether it was there. The caller that
// gets true is the one that ended it.
func (r *idleWatcherRegistry) remove(id uint64) (*idleWatcher, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	w, ok := r.m[id]
	if ok {
		delete(r.m, id)
	}
	return w, ok
}

// list returns the watchers ascending by id; taskFilter "" means all.
func (r *idleWatcherRegistry) list(taskFilter string) []*idleWatcher {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*idleWatcher, 0, len(r.m))
	for _, w := range r.m {
		if taskFilter != "" && w.taskIDHex != taskFilter {
			continue
		}
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// idleWatcherVisibleTo is the ONE predicate behind list and kill.
//
// Two conjuncts. Armed by the caller: a worker that can see its supervisor's
// task must not be able to strip the supervisor's insurance on it — which is
// why this is narrower than the exec and forward siblings. Target still
// visible: arming required the target in scope, a later caps set can narrow
// that, and the rest of the repo then reports the task as absent.
func (h *TaskHandler) idleWatcherVisibleTo(connID string, w *idleWatcher) bool {
	all, allowed := h.visibleToCaller(connID)
	if all {
		return true
	}
	return h.lookupPrincipal(connID) == w.requester && allowed[w.taskIDHex]
}

func (h *TaskHandler) visibleIdleWatchers(connID string, filter protocol.TaskID) []protocol.AwaitIdleWatcherInfo {
	taskFilter := ""
	if filter.Id != ([16]byte{}) {
		taskFilter = hex.EncodeToString(filter.Id[:])
	}
	out := make([]protocol.AwaitIdleWatcherInfo, 0, 8)
	for _, w := range h.idleWatchers().list(taskFilter) {
		if !h.idleWatcherVisibleTo(connID, w) {
			continue
		}
		out = append(out, idleWatcherInfo(w))
	}
	return out
}

func idleWatcherInfo(w *idleWatcher) protocol.AwaitIdleWatcherInfo {
	info := protocol.AwaitIdleWatcherInfo{
		WatcherId:   w.id,
		Sink:        w.sink,
		ThresholdMs: uint32(w.threshold / time.Millisecond),
		ArmedUnixMs: uint64(w.armedAt.UnixMilli()),
		Requester:   w.requester,
		OriginKind:  w.clientKind,
	}
	if raw, err := hex.DecodeString(w.taskIDHex); err == nil && len(raw) == 16 {
		copy(info.TaskId.Id[:], raw)
	}
	info.SetTopic([]byte(w.topic))
	info.SetOriginCid([]byte(w.clientCID))
	return info
}

// handleAwaitIdleList streams the visible watchers, exactly as
// handleExecRunList streams execs: the response names a stream, the rows ride
// it until EOF.
func (h *TaskHandler) handleAwaitIdleList(conn ConnHandle, requestID uint32, connID string, filter protocol.TaskID) {
	respond := func(streamID uint64) {
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_AwaitIdleList, RequestId: requestID}
		resp.SetAwaitIdleList(protocol.AwaitIdleListResponse{StreamId: streamID})
		out := resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck
	}
	var body protocol.AwaitIdleListBody
	body.SetWatchers(h.visibleIdleWatchers(connID, filter))
	bodyBytes, err := body.EncodeCopy(nil)
	if err != nil {
		slog.Error("AwaitIdleList: encode body failed", "err", err)
		respond(0)
		return
	}
	stream := conn.CreateSendStream()
	if stream == nil {
		respond(0)
		return
	}
	if werr := stream.AppendData(false, bodyBytes); werr != nil {
		slog.Warn("AwaitIdleList: stream write failed", "err", werr)
		_ = stream.Close()
		respond(0)
		return
	}
	if werr := stream.AppendData(true); werr != nil {
		slog.Warn("AwaitIdleList: stream EOF failed", "err", werr)
		_ = stream.Close()
		respond(0)
		return
	}
	respond(uint64(stream.ID()))
}

// handleAwaitIdleKill disarms one watcher. A watcher the predicate refuses is
// reported as absent, as handleExecRunKill does, so the answer does not
// confirm that someone else's watcher exists.
func (h *TaskHandler) handleAwaitIdleKill(connID string, req *protocol.AwaitIdleKillRequest) protocol.AwaitIdleKillResponse {
	notFound := protocol.AwaitIdleKillResponse{Status: protocol.AwaitIdleKillStatus_NotFound}
	w, ok := h.idleWatchers().get(req.WatcherId)
	if !ok || !h.idleWatcherVisibleTo(connID, w) {
		return notFound
	}
	if _, still := h.idleWatchers().remove(req.WatcherId); !still {
		return notFound // it fired between the lookup and here
	}
	close(w.stop)
	if w.onCancel != nil {
		w.onCancel()
	}
	return protocol.AwaitIdleKillResponse{Status: protocol.AwaitIdleKillStatus_Ok}
}

// DropIdleWatchersForConn ends the reply-sink watchers this connection armed.
// Their result can only go to that connection, so once it is gone they are
// rows that look armed and deliver nowhere. Board and notify watchers are left
// alone: outliving the arming request is what those sinks are for. Sits beside
// DropExecRunsForConn in handleConnection's teardown.
func (h *TaskHandler) DropIdleWatchersForConn(connID string) {
	for _, w := range h.idleWatchers().list("") {
		if w.clientCID != connID || w.sink != protocol.AwaitIdleSink_Reply {
			continue
		}
		if _, still := h.idleWatchers().remove(w.id); !still {
			continue
		}
		close(w.stop)
	}
}
