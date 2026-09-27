# Listing and killing await-idle watchers — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An armed `session await-idle` watcher gets an id, can be listed, and can be killed, on CLI, TUI and WebUI.

**Architecture:** A server-side watcher registry shaped like the exec registry (`server/exec_registry.go`), a stop channel on `SessionMux.ArmIdleWatcher`, two new TaskControl kinds (`await_idle_list` / `await_idle_kill`), and three surfaces built on the `exec ls` / `exec kill` siblings.

**Tech Stack:** Go, `.bgn` schema → ebm2go (`make protoregen`), bubbletea TUI, wasm bridge + vanilla JS WebUI.

**Spec:** `docs/superpowers/specs/2026-09-28-await-idle-cancel-design.md` — read its **Problem** section before starting any task.

## Global Constraints

- Work in THIS worktree (`.harness-worktrees/70fbad4a6eb6f1e992be8a669f1bcefd/`); verify `git rev-parse --abbrev-ref HEAD` = `harness/70fbad4a6eb6f1e992be8a669f1bcefd` before the first commit. Absolute paths must include the `.harness-worktrees/<hash>/` segment — a bare `/…/remote-agent-harness/x` path edits the PARENT checkout.
- Read `.claude/skills/implementation-pitfalls/SKILL.md` in full before writing code.
- TUI/WebUI call the `*With` client methods on their held client — never the dial+close form.
- Never `go build ./cmd/<x>/` bare (drops a binary in the worktree). Verify with `make check`, `make test`, `make vet`.
- Watcher ids start at 1; 0 means "none" everywhere.
- Kill of an unknown / foreign / already-ended watcher answers `not_found`, never a distinct "denied".
- New TaskControlKind values are APPENDED after `trsf_state` (ordinals are implicit).
- Comment density and idiom follow the neighbouring file.

---

### Task 1: Wire schema

**Files:**
- Modify: `runner/protocol/message.bgn`
- Regenerate: `runner/protocol/message.go` (via `make protoregen`)

**Interfaces:**
- Produces (generated Go): `protocol.AwaitIdleStatus_Cancelled`; `AwaitIdleResponse.WatcherId uint64`; `TaskControlKind_AwaitIdleList`, `TaskControlKind_AwaitIdleKill`; `AwaitIdleListRequest{TaskId TaskID}`; `AwaitIdleListResponse{StreamId uint64}`; `AwaitIdleListBody{Watchers []AwaitIdleWatcherInfo}`; `AwaitIdleWatcherInfo{WatcherId, TaskId, Sink, Topic, ThresholdMs, ArmedUnixMs, Requester, OriginKind, OriginCid}` with `SetTopic` / `SetOriginCid`; `AwaitIdleKillRequest{WatcherId}`; `AwaitIdleKillResponse{Status AwaitIdleKillStatus}`; `AwaitIdleKillStatus_Ok`, `AwaitIdleKillStatus_NotFound`; request/response accessors `AwaitIdleList()`, `SetAwaitIdleList(…)`, `AwaitIdleKill()`, `SetAwaitIdleKill(…)`.

- [ ] **Step 1: Edit the schema**

In `enum TaskControlKind`, after the `trsf_state` line and its comment block, append:

```
    await_idle_list         # appended: enumerate armed await-idle watchers.
                            # No capability. An agent sees the ones it armed on
                            # tasks still visible to it; the operator sees all.
    await_idle_kill         # appended: disarm one watcher by id. Same predicate
                            # as await_idle_list; anything else is not_found.
```

In `enum AwaitIdleStatus`, after `bad_request = 4 …`:

```
    cancelled = 5             # killed via await_idle_kill before it fired
```

Replace `format AwaitIdleResponse` with:

```
format AwaitIdleResponse:
    status :AwaitIdleStatus
    last_output_at :u64       # unix nanos at decision time (0 if none)
    watcher_id :u64           # the registry id; 0 for not_found / bad_request
```

After `format AwaitIdleResponse`, add:

```
format AwaitIdleListRequest:
    task_id :TaskID           # all-zero = every watcher visible to the caller

# Rows travel on their own send stream, as ExecRunListResponse's do: a row
# carries a topic of up to 64KiB, and a control message must fit one datagram.
format AwaitIdleListResponse:
    stream_id :u64            # server-initiated send-stream carrying
                              # AwaitIdleListBody until EOF. 0 = none allocated.

format AwaitIdleListBody:
    watchers_len :u16
    watchers :[watchers_len]AwaitIdleWatcherInfo

format AwaitIdleWatcherInfo:
    watcher_id     :u64
    task_id        :TaskID
    sink           :AwaitIdleSink
    topic_len      :u16
    topic          :[topic_len]u8     # sink=board only; empty otherwise
    threshold_ms   :u32               # after the server default is applied
    armed_unix_ms  :u64
    requester      :TaskID            # the arming principal; zero = operator
    origin_kind    :ClientKind
    origin_cid_len :u8
    origin_cid     :[origin_cid_len]u8  # ConnectionID canonical String()

format AwaitIdleKillRequest:
    watcher_id :u64

enum AwaitIdleKillStatus:
    :u8
    ok = 0
    not_found = 1             # unknown, already ended, or not the caller's

format AwaitIdleKillResponse:
    status :AwaitIdleKillStatus
```

In `format TaskControlRequest`'s `match kind:`, after the `trsf_state` arm (append at the end of the match):

```
        TaskControlKind.await_idle_list => await_idle_list :AwaitIdleListRequest
        TaskControlKind.await_idle_kill => await_idle_kill :AwaitIdleKillRequest
```

In `format TaskControlResponse`'s `match kind:`, same position:

```
        TaskControlKind.await_idle_list => await_idle_list :AwaitIdleListResponse
        TaskControlKind.await_idle_kill => await_idle_kill :AwaitIdleKillResponse
```

(Locate the exact match blocks with `grep -n "TaskControlKind.trsf_state" runner/protocol/message.bgn`; there are two.)

- [ ] **Step 2: Regenerate**

Run: `make protoregen`
Expected: exits 0; `git diff --stat runner/protocol/message.go` shows a large diff (ebm2go regen churn is normal — do not try to shrink it).

- [ ] **Step 3: Confirm the new symbols exist and the tree builds**

Run: `grep -c "AwaitIdleStatus_Cancelled\|TaskControlKind_AwaitIdleList\|TaskControlKind_AwaitIdleKill\|AwaitIdleKillStatus_NotFound\|func (t \*AwaitIdleWatcherInfo) SetTopic" runner/protocol/message.go`
Expected: ≥ 5.
Run: `make check`
Expected: PASS. (The server does not handle the new kinds yet; `cap_completeness_test` / `scope_completeness_test` will go red — that is Task 3's job, do not touch them here.)

- [ ] **Step 4: Commit**

```bash
git add runner/protocol/message.bgn runner/protocol/message.go
git commit -m "protocol: await-idle watcher ids, list and kill"
```

---

### Task 2: Server registry, stop channel, watcher ids on arm

**Files:**
- Create: `server/idle_watchers.go`
- Modify: `server/task_handler.go` (struct fields beside `execRunsOnce` / `execRuns`, ~line 76)
- Modify: `server/session_mux.go` (`ArmIdleWatcher`, ~line 1080)
- Modify: `server/await_idle_handler.go` (`handleAwaitIdle`)
- Modify: `server/await_idle_test.go` (existing `ArmIdleWatcher` calls gain `nil`)
- Test: `server/idle_watchers_test.go`

**Interfaces:**
- Consumes: Task 1's generated types.
- Produces:
  - `type idleWatcher struct { id uint64; taskIDHex string; sink protocol.AwaitIdleSink; topic string; threshold time.Duration; armedAt time.Time; requester protocol.TaskID; clientCID string; clientKind protocol.ClientKind; stop chan struct{}; onCancel func() }`
  - `func (h *TaskHandler) idleWatchers() *idleWatcherRegistry` with `add(*idleWatcher) uint64`, `get(uint64) (*idleWatcher, bool)`, `remove(uint64) (*idleWatcher, bool)`, `list(taskFilter string) []*idleWatcher`
  - `func (m *SessionMux) ArmIdleWatcher(threshold time.Duration, stop <-chan struct{}, fn func(stopped bool, lastOutputUnixNano int64))`

- [ ] **Step 1: Write the failing tests**

`server/idle_watchers_test.go`:

```go
package server

import (
	"context"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/exec/frame"
)

// A watcher that is armed and then stopped through its channel returns
// without calling fn: the kill path owns what (if anything) is sent.
func TestSessionMux_IdleWatcherStopChannelEndsWithoutFiring(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})
	defer mux.Stop()

	stop := make(chan struct{})
	fired := make(chan bool, 1)
	// No output yet (lastOutput == 0), so the watcher is waiting.
	mux.ArmIdleWatcher(10*time.Millisecond, stop, func(stopped bool, _ int64) { fired <- stopped })
	close(stop)
	runner.QueueRead(makeWireFrame(byte(frame.FrameType_Stdout), []byte("x")))
	select {
	case <-fired:
		t.Fatal("fn ran after stop was closed")
	case <-time.After(3 * idleWatchTick):
	}
}

func TestIdleWatcherRegistry_IdsStartAtOneAndRemoveIsOnce(t *testing.T) {
	r := newIdleWatcherRegistry()
	a := r.add(&idleWatcher{taskIDHex: "aa"})
	b := r.add(&idleWatcher{taskIDHex: "bb"})
	if a != 1 || b != 2 {
		t.Fatalf("ids = %d,%d, want 1,2", a, b)
	}
	if _, ok := r.remove(a); !ok {
		t.Fatal("first remove reported absent")
	}
	if _, ok := r.remove(a); ok {
		t.Fatal("second remove reported present")
	}
	if got := r.list("bb"); len(got) != 1 || got[0].id != b {
		t.Fatalf("list(bb) = %+v", got)
	}
}

// The arm response carries the registry id, and a fire removes the entry.
func TestHandleAwaitIdle_BoardArmCarriesIdAndFireDeregisters(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	reg := NewSessionRegistry()
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})
	defer mux.Stop()
	var tid protocol.TaskID
	tid.Id[0] = 0xDD
	reg.Add("dd000000000000000000000000000000", mux)
	board := agentboardForTest(t)
	h := &TaskHandler{Tasks: NewTaskStore(), Sessions: reg, Board: board}
	conn := &fakeConn{}

	runner.QueueRead(makeWireFrame(byte(frame.FrameType_Stdout), []byte("output")))
	waitFor(t, func() bool { return mux.LastOutputUnixNano() != 0 })

	req := protocol.AwaitIdleRequest{TaskId: tid, ThresholdMs: 50, Sink: protocol.AwaitIdleSink_Board}
	req.SetTopic([]byte("chat.deadbeef"))
	resp := awaitIdleViaHandle(t, h, conn, req)
	if resp.Status != protocol.AwaitIdleStatus_Armed || resp.WatcherId == 0 {
		t.Fatalf("resp = %+v, want Armed with a non-zero watcher id", resp)
	}
	waitForWithin(t, 3*time.Second, func() bool { return len(h.idleWatchers().list("")) == 0 })
}
```

Add the helper once, at the bottom of the same file (the existing board test builds its board inline; this is the same call):

```go
func agentboardForTest(t *testing.T) *agentboard.Board {
	t.Helper()
	b := agentboard.New(agentboard.Config{RingN: 8, MaxTopics: 16, MaxPayload: 4096})
	t.Cleanup(b.Close)
	return b
}
```

(and add `"github.com/on-keyday/agent-harness/agentboard"` to the imports).

- [ ] **Step 2: Run to confirm they fail**

Run: `go test ./server -run 'IdleWatcher|BoardArmCarriesId' -count=1`
Expected: FAIL to compile (`newIdleWatcherRegistry`, `idleWatchers`, `WatcherId` in use, `ArmIdleWatcher` arity).

- [ ] **Step 3: Implement the registry**

`server/idle_watchers.go`:

```go
package server

import (
	"sort"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// idleWatcher is one armed await-idle watcher. The server holds it so
// `session await-idle ls` can report it and `kill` can reach it. Nothing is
// persisted: a server restart drops every watcher, as it always has.
//
// Exactly one of fire, session stop, kill or connection teardown ends a
// watcher, and the one that does is whichever removes it from the registry
// first — see removeIdleWatcher's callers.
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
```

In `server/task_handler.go`, directly under `execRuns *execRegistry`:

```go
	// idleWatchersOnce / idleWatcherReg are the same lazy pair for armed
	// await-idle watchers; see idleWatchers().
	idleWatchersOnce sync.Once
	idleWatcherReg   *idleWatcherRegistry
```

- [ ] **Step 4: Give `ArmIdleWatcher` a stop channel**

In `server/session_mux.go`, change the signature and the select:

```go
func (m *SessionMux) ArmIdleWatcher(threshold time.Duration, stop <-chan struct{}, fn func(stopped bool, lastOutputUnixNano int64)) {
```

and in the loop's `select`, add a third case before `case <-t.C:`:

```go
			case <-stop:
				// Killed or its requester went away. Whoever closed stop owns
				// what is sent; this goroutine only leaves.
				return
```

Update the doc comment above the function with one sentence: "A closed stop ends the watcher without calling fn; a nil stop never fires." Update the three calls in `server/await_idle_test.go` to pass `nil` as the new second argument.

- [ ] **Step 5: Register on arm and carry the id**

Rewrite the tail of `handleAwaitIdle` in `server/await_idle_handler.go`. First change `respond` to take the id:

```go
	respond := func(status protocol.AwaitIdleStatus, lastOutputUnixNano int64, watcherID uint64) {
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_AwaitIdle, RequestId: requestID}
		lo := uint64(0)
		if lastOutputUnixNano > 0 {
			lo = uint64(lastOutputUnixNano)
		}
		resp.SetAwaitIdle(protocol.AwaitIdleResponse{Status: status, LastOutputAt: lo, WatcherId: watcherID})
		out := resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck
	}
```

Every existing early `respond(X, 0)` becomes `respond(X, 0, 0)`. Then replace everything from `// Resolve the requester's identity NOW` to the end of the function with:

```go
	// Resolve the requester's identity NOW — by fire time the conn may be
	// gone (sink=notify/board deliberately outlive the request).
	requesterConnID := conn.ConnectionID().String()
	requester := h.lookupPrincipal(requesterConnID)

	w := &idleWatcher{
		taskIDHex:  taskIDHex,
		sink:       ai.Sink,
		topic:      topic,
		threshold:  threshold,
		requester:  requester,
		clientCID:  requesterConnID,
		clientKind: h.clientKindOf(requesterConnID),
		stop:       make(chan struct{}),
	}
	// Registered BEFORE arming: an already-idle session fires on the first
	// check, and the fire must find its own entry to remove.
	id := h.idleWatchers().add(w)

	var deliver func(stopped bool, lo int64)
	switch ai.Sink {
	case protocol.AwaitIdleSink_Reply:
		w.onCancel = func() { respond(protocol.AwaitIdleStatus_Cancelled, mux.LastOutputUnixNano(), id) }
		deliver = func(stopped bool, lo int64) {
			st := protocol.AwaitIdleStatus_Fired
			if stopped {
				st = protocol.AwaitIdleStatus_SessionStopped
			}
			respond(st, lo, id)
		}
	case protocol.AwaitIdleSink_Notify:
		respond(protocol.AwaitIdleStatus_Armed, mux.LastOutputUnixNano(), id)
		deliver = func(stopped bool, lo int64) {
			h.fireIdleNotify(taskIDHex, requesterConnID, stopped, lo)
		}
	case protocol.AwaitIdleSink_Board:
		respond(protocol.AwaitIdleStatus_Armed, mux.LastOutputUnixNano(), id)
		deliver = func(stopped bool, lo int64) {
			h.fireIdleBoard(topic, taskIDHex, requester, stopped, lo)
		}
	}
	mux.ArmIdleWatcher(threshold, w.stop, func(stopped bool, lo int64) {
		// A kill or a teardown that removed it first owns the outcome.
		if _, still := h.idleWatchers().remove(id); !still {
			return
		}
		deliver(stopped, lo)
	})
}
```

`h.clientKindOf` — find the existing accessor for a connection's recorded `ClientKind` (`grep -n "clientKinds\[" server/task_handler.go`); if none returns it, add beside `lookupPrincipal`:

```go
func (h *TaskHandler) clientKindOf(connID string) protocol.ClientKind {
	h.clientKindsMu.Lock()
	defer h.clientKindsMu.Unlock()
	return h.clientKinds[connID]
}
```

(match the map's real name; do not add a second map.)

- [ ] **Step 6: Run the tests**

Run: `go test ./server -run 'IdleWatcher|AwaitIdle' -count=1 -race`
Expected: PASS, including every pre-existing `TestHandleAwaitIdle_*` / `TestSessionMux_IdleWatcher*`.

- [ ] **Step 7: Commit**

```bash
git add server/idle_watchers.go server/idle_watchers_test.go server/task_handler.go server/session_mux.go server/await_idle_handler.go server/await_idle_test.go
git commit -m "server: register await-idle watchers and return their id"
```

---

### Task 3: Server list, kill, teardown, completeness tables

**Files:**
- Modify: `server/idle_watchers.go` (predicate, list/kill handlers, teardown)
- Modify: `server/task_handler.go` (two dispatch cases after `TaskControlKind_ExecRunKill`)
- Modify: `server/server.go` (~line 1260, teardown beside `DropExecRunsForConn`)
- Modify: `server/cap_completeness_test.go`, `server/scope_completeness_test.go`
- Test: `server/idle_watchers_test.go`

**Interfaces:**
- Consumes: Task 2's registry and `idleWatcher`.
- Produces: `func (h *TaskHandler) visibleIdleWatchers(connID string, filter protocol.TaskID) []protocol.AwaitIdleWatcherInfo`; `func (h *TaskHandler) handleAwaitIdleKill(connID string, req *protocol.AwaitIdleKillRequest) protocol.AwaitIdleKillResponse`; `func (h *TaskHandler) handleAwaitIdleList(conn ConnHandle, requestID uint32, connID string, filter protocol.TaskID)`; `func (h *TaskHandler) DropIdleWatchersForConn(connID string)`.

- [ ] **Step 1: Write the failing tests**

Append to `server/idle_watchers_test.go`:

```go
// armBoardWatcher registers a board-sink watcher directly, bypassing the
// session mux: these tests are about who may see and end one, not about
// firing.
func armBoardWatcher(h *TaskHandler, taskHex string, requester protocol.TaskID, cid string) uint64 {
	return h.idleWatchers().add(&idleWatcher{
		taskIDHex: taskHex, sink: protocol.AwaitIdleSink_Board, topic: "chat.x",
		requester: requester, clientCID: cid, stop: make(chan struct{}),
	})
}

func TestIdleWatchers_OperatorSeesAllAgentSeesOwn(t *testing.T) {
	h := newTestHandler(t)
	scope := Scope{Base: protocol.ScopeBase_Subtree, IDs: []string{"ee000000000000000000000000000000"}}
	aHex := h.Tasks.Create("repo", "a", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_None, scope, "")
	bHex := h.Tasks.Create("repo", "b", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_None, scope, "")
	a, b := hexToTaskID(t, aHex), hexToTaskID(t, bHex)
	if h.principals == nil {
		h.principals = make(map[string]protocol.TaskID)
	}
	aConn := "ws:127.0.0.1:9601-1"
	bConn := "ws:127.0.0.1:9601-2"
	h.principals[aConn] = a
	h.principals[bConn] = b
	target := "ee000000000000000000000000000000"

	wa := armBoardWatcher(h, target, a, aConn)
	wb := armBoardWatcher(h, target, b, bConn)
	wo := armBoardWatcher(h, target, protocol.TaskID{}, "ws:127.0.0.1:9601-3")

	if got := h.visibleIdleWatchers("ws:127.0.0.1:9601-9", protocol.TaskID{}); len(got) != 3 {
		t.Fatalf("operator sees %d watchers, want 3", len(got))
	}
	got := h.visibleIdleWatchers(aConn, protocol.TaskID{})
	if len(got) != 1 || got[0].WatcherId != wa {
		t.Fatalf("agent A sees %+v, want only watcher %d", got, wa)
	}
	for _, id := range []uint64{wb, wo} {
		r := h.handleAwaitIdleKill(aConn, &protocol.AwaitIdleKillRequest{WatcherId: id})
		if r.Status != protocol.AwaitIdleKillStatus_NotFound {
			t.Fatalf("A killing %d: %v, want NotFound", id, r.Status)
		}
		if _, ok := h.idleWatchers().get(id); !ok {
			t.Fatalf("watcher %d gone after a refused kill", id)
		}
	}
	if r := h.handleAwaitIdleKill(aConn, &protocol.AwaitIdleKillRequest{WatcherId: wa}); r.Status != protocol.AwaitIdleKillStatus_Ok {
		t.Fatalf("A killing its own: %v, want Ok", r.Status)
	}
	if r := h.handleAwaitIdleKill("ws:127.0.0.1:9601-9", &protocol.AwaitIdleKillRequest{WatcherId: wb}); r.Status != protocol.AwaitIdleKillStatus_Ok {
		t.Fatalf("operator killing B's: %v, want Ok", r.Status)
	}
}

// An agent whose scope no longer covers the target does not see its own
// watcher on it: the rest of the repo already reports that task as absent.
func TestIdleWatchers_AgentLosesSightWhenScopeNarrows(t *testing.T) {
	h := newTestHandler(t)
	aHex := h.Tasks.Create("repo", "a", protocol.TaskKind_Oneshot, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, protocol.Capability_None,
		Scope{Base: protocol.ScopeBase_Subtree}, "")
	a := hexToTaskID(t, aHex)
	h.principals = map[string]protocol.TaskID{"ws:127.0.0.1:9601-1": a}
	armBoardWatcher(h, "ee000000000000000000000000000000", a, "ws:127.0.0.1:9601-1")
	if got := h.visibleIdleWatchers("ws:127.0.0.1:9601-1", protocol.TaskID{}); len(got) != 0 {
		t.Fatalf("agent sees %d watchers on an out-of-scope task, want 0", len(got))
	}
}

func TestDropIdleWatchersForConn_ReplyOnly(t *testing.T) {
	h := &TaskHandler{Tasks: NewTaskStore(), Sessions: NewSessionRegistry()}
	cid := "ws:127.0.0.1:9601-1"
	reply := h.idleWatchers().add(&idleWatcher{taskIDHex: "aa", sink: protocol.AwaitIdleSink_Reply, clientCID: cid, stop: make(chan struct{})})
	board := armBoardWatcher(h, "aa", protocol.TaskID{}, cid)
	other := h.idleWatchers().add(&idleWatcher{taskIDHex: "aa", sink: protocol.AwaitIdleSink_Reply, clientCID: "ws:127.0.0.1:9601-2", stop: make(chan struct{})})

	h.DropIdleWatchersForConn(cid)

	if _, ok := h.idleWatchers().get(reply); ok {
		t.Fatal("reply watcher of the closed conn survived")
	}
	for _, id := range []uint64{board, other} {
		if _, ok := h.idleWatchers().get(id); !ok {
			t.Fatalf("watcher %d was dropped, want kept", id)
		}
	}
}

// A killed reply-sink watcher answers its blocked caller with cancelled.
func TestHandleAwaitIdle_ReplyKilledAnswersCancelled(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	reg := NewSessionRegistry()
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})
	defer mux.Stop()
	var tid protocol.TaskID
	tid.Id[0] = 0xBB
	reg.Add("bb000000000000000000000000000000", mux)
	h := &TaskHandler{Tasks: NewTaskStore(), Sessions: reg}
	conn := &fakeConn{}

	// No output: the watcher waits indefinitely until killed.
	tcr := protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AwaitIdle, RequestId: 7}
	tcr.SetAwaitIdle(protocol.AwaitIdleRequest{TaskId: tid, ThresholdMs: 50, Sink: protocol.AwaitIdleSink_Reply})
	h.Handle(conn, tcr.MustAppend(nil))
	waitFor(t, func() bool { return len(h.idleWatchers().list("")) == 1 })
	id := h.idleWatchers().list("")[0].id

	if r := h.handleAwaitIdleKill("", &protocol.AwaitIdleKillRequest{WatcherId: id}); r.Status != protocol.AwaitIdleKillStatus_Ok {
		t.Fatalf("kill: %v", r.Status)
	}
	waitFor(t, func() bool { return len(conn.Sent()) == 1 })
	resp := decodeAwaitIdleResponse(t, conn.Sent()[0])
	if resp.Status != protocol.AwaitIdleStatus_Cancelled || resp.WatcherId != id {
		t.Fatalf("resp = %+v, want Cancelled for watcher %d", resp, id)
	}
	// Output after the kill must not produce a second response.
	runner.QueueRead(makeWireFrame(byte(frame.FrameType_Stdout), []byte("x")))
	time.Sleep(3 * idleWatchTick)
	if n := len(conn.Sent()); n != 1 {
		t.Fatalf("%d responses, want exactly 1", n)
	}
}

// A killed board-sink watcher publishes nothing, even once the session idles.
func TestHandleAwaitIdle_BoardKilledPublishesNothing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	runner := newFakeStream(t)
	reg := NewSessionRegistry()
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})
	defer mux.Stop()
	var tid protocol.TaskID
	tid.Id[0] = 0xDD
	reg.Add("dd000000000000000000000000000000", mux)
	board := agentboardForTest(t)
	h := &TaskHandler{Tasks: NewTaskStore(), Sessions: reg, Board: board}
	conn := &fakeConn{}

	req := protocol.AwaitIdleRequest{TaskId: tid, ThresholdMs: 50, Sink: protocol.AwaitIdleSink_Board}
	req.SetTopic([]byte("chat.deadbeef"))
	resp := awaitIdleViaHandle(t, h, conn, req)
	if r := h.handleAwaitIdleKill("", &protocol.AwaitIdleKillRequest{WatcherId: resp.WatcherId}); r.Status != protocol.AwaitIdleKillStatus_Ok {
		t.Fatalf("kill: %v", r.Status)
	}
	runner.QueueRead(makeWireFrame(byte(frame.FrameType_Stdout), []byte("x")))
	waitFor(t, func() bool { return mux.LastOutputUnixNano() != 0 })
	time.Sleep(50*time.Millisecond + 3*idleWatchTick)
	if msgs, _ := board.ListRetained("chat.deadbeef"); len(msgs) != 0 {
		t.Fatalf("%d messages published after kill, want 0", len(msgs))
	}
}

// Fire and kill racing at the threshold edge: exactly one of them wins.
func TestIdleWatcher_FireKillRaceHasOneWinner(t *testing.T) {
	for i := 0; i < 50; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		runner := newFakeStream(t)
		reg := NewSessionRegistry()
		mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(256), SessionHooks{})
		var tid protocol.TaskID
		tid.Id[0] = 0xBB
		reg.Add("bb000000000000000000000000000000", mux)
		h := &TaskHandler{Tasks: NewTaskStore(), Sessions: reg}
		conn := &fakeConn{}
		runner.QueueRead(makeWireFrame(byte(frame.FrameType_Stdout), []byte("x")))
		waitFor(t, func() bool { return mux.LastOutputUnixNano() != 0 })

		tcr := protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AwaitIdle, RequestId: 7}
		tcr.SetAwaitIdle(protocol.AwaitIdleRequest{TaskId: tid, ThresholdMs: 1, Sink: protocol.AwaitIdleSink_Reply})
		h.Handle(conn, tcr.MustAppend(nil))
		// id is 1: a fresh handler per iteration.
		kr := h.handleAwaitIdleKill("", &protocol.AwaitIdleKillRequest{WatcherId: 1})
		waitFor(t, func() bool { return len(conn.Sent()) >= 1 })
		time.Sleep(2 * idleWatchTick)
		if n := len(conn.Sent()); n != 1 {
			t.Fatalf("iter %d: %d responses, want 1", i, n)
		}
		st := decodeAwaitIdleResponse(t, conn.Sent()[0]).Status
		killWon := kr.Status == protocol.AwaitIdleKillStatus_Ok
		if killWon != (st == protocol.AwaitIdleStatus_Cancelled) {
			t.Fatalf("iter %d: kill=%v but response=%v", i, kr.Status, st)
		}
		mux.Stop()
		cancel()
	}
}
```

- [ ] **Step 2: Run to confirm they fail**

Run: `go test ./server -run 'IdleWatchers|DropIdleWatchers|Killed|FireKillRace' -count=1`
Expected: FAIL to compile (`visibleIdleWatchers`, `handleAwaitIdleKill`, `DropIdleWatchersForConn` undefined).

- [ ] **Step 3: Implement predicate, list, kill, teardown**

Append to `server/idle_watchers.go` (add imports `encoding/hex`, `log/slog`, `github.com/on-keyday/agent-harness/appwire`):

```go
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
```

(`body.SetWatchers` / `EncodeCopy` — use whatever the generated `AwaitIdleListBody` exposes, mirroring `ExecRunListBody` in `handleExecRunList`; if the generated setter is named differently, `grep -n "func (t \*AwaitIdleListBody)" runner/protocol/message.go`.)

- [ ] **Step 4: Dispatch**

In `server/task_handler.go`, after the `case protocol.TaskControlKind_ExecRunKill:` block:

```go
	case protocol.TaskControlKind_AwaitIdleList:
		// No capability: bounded inside the handler by idleWatcherVisibleTo.
		al := req.AwaitIdleList()
		if al == nil {
			slog.Error("TaskHandler: AwaitIdleList variant is nil")
			return
		}
		h.handleAwaitIdleList(conn, req.RequestId, cid, al.TaskId)

	case protocol.TaskControlKind_AwaitIdleKill:
		// No capability, and gated inline for exec_run_kill's reason: the
		// target is only known after the registry lookup.
		ak := req.AwaitIdleKill()
		if ak == nil {
			slog.Error("TaskHandler: AwaitIdleKill variant is nil")
			return
		}
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_AwaitIdleKill, RequestId: req.RequestId}
		resp.SetAwaitIdleKill(h.handleAwaitIdleKill(cid, ak))
		out := resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck
```

In `server/server.go`, directly under `s.taskHandler.DropExecRunsForConn(session.ConnectionID().String())`:

```go
			s.taskHandler.DropIdleWatchersForConn(session.ConnectionID().String())
```

- [ ] **Step 5: Classify in the completeness tables**

`server/cap_completeness_test.go`, beside `protocol.TaskControlKind_ExecRunList: capNone,`:

```go
	// await_idle_list / await_idle_kill read no capability bit: they reveal
	// and end only what the caller armed (the operator: everything), and
	// arming needed none either.
	protocol.TaskControlKind_AwaitIdleList: capNone,
	protocol.TaskControlKind_AwaitIdleKill: capNone,
```

`server/scope_completeness_test.go`: `protocol.TaskControlKind_AwaitIdleList: infoScoped,` beside `ExecRunList`, and `protocol.TaskControlKind_AwaitIdleKill: targetGated,` beside `ExecRunKill`, each with a one-line comment naming `idleWatcherVisibleTo`.

- [ ] **Step 6: Run**

Run: `go test ./server -count=1 -race`
Expected: PASS. (A known flake family exists in this package at ~1/6 package runs — if a failure is outside the tests this task touched, re-run once and report both outputs.)

- [ ] **Step 7: Commit**

```bash
git add server/
git commit -m "server: list and kill await-idle watchers; drop reply watchers with their conn"
```

---

### Task 4: Client methods and renderers

**Files:**
- Modify: `cli/await_idle.go`
- Modify: `cmd/harness-cli/session.go` (delete `awaitIdleStatusStr`, call `cli.AwaitIdleStatusString`)
- Modify: `cmd/harness-webui-wasm/main.go` (delete its `awaitIdleStatusStr`, call `cli.AwaitIdleStatusString`)
- Test: `cli/await_idle_test.go` (create)

**Interfaces:**
- Consumes: Task 1 types.
- Produces:
  - `func (c *Client) AwaitIdleListWith(ctx context.Context, taskFilter string) ([]protocol.AwaitIdleWatcherInfo, error)`
  - `func AwaitIdleList(ctx context.Context, peerCID objproto.ConnectionID, taskFilter string) ([]protocol.AwaitIdleWatcherInfo, error)`
  - `func (c *Client) AwaitIdleKillWith(ctx context.Context, id uint64) error`
  - `func AwaitIdleKill(ctx context.Context, peerCID objproto.ConnectionID, id uint64) error`
  - `func AwaitIdleStatusString(s protocol.AwaitIdleStatus) string` — snake_case, incl. `"cancelled"`
  - `func AwaitIdleSinkString(s protocol.AwaitIdleSink) string` — `reply` / `notify` / `board`
  - `func AwaitIdleWatcherBy(w *protocol.AwaitIdleWatcherInfo) string` — `operator` or the 8-hex task prefix
  - `func AwaitIdleWatcherLines(ws []protocol.AwaitIdleWatcherInfo) []string`
  - `func AwaitIdleWatcherJSONLine(w *protocol.AwaitIdleWatcherInfo) string`

- [ ] **Step 1: Write the failing tests**

`cli/await_idle_test.go`:

```go
package cli

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestAwaitIdleStatusString_CoversEveryStatus(t *testing.T) {
	for s, want := range map[protocol.AwaitIdleStatus]string{
		protocol.AwaitIdleStatus_Fired:          "fired",
		protocol.AwaitIdleStatus_Armed:          "armed",
		protocol.AwaitIdleStatus_SessionStopped: "session_stopped",
		protocol.AwaitIdleStatus_NotFound:       "not_found",
		protocol.AwaitIdleStatus_BadRequest:     "bad_request",
		protocol.AwaitIdleStatus_Cancelled:      "cancelled",
	} {
		if got := AwaitIdleStatusString(s); got != want {
			t.Errorf("%v -> %q, want %q", s, got, want)
		}
	}
}

func TestAwaitIdleWatcherLines_EmptyIsSaid(t *testing.T) {
	got := AwaitIdleWatcherLines(nil)
	if len(got) != 1 || got[0] != "no armed watchers" {
		t.Fatalf("got %q", got)
	}
}

func TestAwaitIdleWatcherLines_Row(t *testing.T) {
	w := protocol.AwaitIdleWatcherInfo{
		WatcherId: 7, Sink: protocol.AwaitIdleSink_Board, ThresholdMs: 2500,
		ArmedUnixMs: uint64(time.Now().Add(-3 * time.Second).UnixMilli()),
	}
	w.TaskId.Id[0] = 0xab
	w.Requester.Id[0] = 0x12
	w.SetTopic([]byte("chat.12000000"))
	lines := AwaitIdleWatcherLines([]protocol.AwaitIdleWatcherInfo{w})
	row := lines[len(lines)-1]
	for _, want := range []string{"7", "ab000000", "sink=board", "topic=chat.12000000", "threshold=2500ms", "by=12000000"} {
		if !strings.Contains(row, want) {
			t.Errorf("row %q missing %q", row, want)
		}
	}
	var op protocol.AwaitIdleWatcherInfo
	if by := AwaitIdleWatcherBy(&op); by != "operator" {
		t.Errorf("zero requester -> %q, want operator", by)
	}
}

func TestAwaitIdleWatcherJSONLine_CarriesFullIDs(t *testing.T) {
	w := protocol.AwaitIdleWatcherInfo{WatcherId: 3, Sink: protocol.AwaitIdleSink_Reply}
	w.TaskId.Id[0] = 0xab
	var got map[string]any
	if err := json.Unmarshal([]byte(AwaitIdleWatcherJSONLine(&w)), &got); err != nil {
		t.Fatal(err)
	}
	if got["task_id"] != "ab000000000000000000000000000000" || got["sink"] != "reply" || got["watcher_id"] != float64(3) {
		t.Fatalf("got %v", got)
	}
	if got["requester"] != "" {
		t.Fatalf("operator requester = %v, want empty string", got["requester"])
	}
}
```

- [ ] **Step 2: Run to confirm they fail**

Run: `go test ./cli -run AwaitIdle -count=1`
Expected: FAIL to compile.

- [ ] **Step 3: Implement**

Append to `cli/await_idle.go` (imports: add `encoding/json`, `errors`, `time`, `github.com/on-keyday/objtrsf/objproto`, `github.com/on-keyday/objtrsf/trsf` — the same ones `cli/exec_run.go` uses):

```go
// AwaitIdleListWith reports the armed watchers this caller can see: its own on
// tasks still visible to it, or every one for the operator.
func (c *Client) AwaitIdleListWith(ctx context.Context, taskFilter string) ([]protocol.AwaitIdleWatcherInfo, error) {
	var q protocol.AwaitIdleListRequest
	if taskFilter != "" {
		tid, err := parseTaskIDHex(taskFilter)
		if err != nil {
			return nil, fmt.Errorf("await-idle ls: parse task id: %w", err)
		}
		q.TaskId = tid
	}
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AwaitIdleList}
	req.SetAwaitIdleList(q)
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, err
	}
	lr := resp.AwaitIdleList()
	if lr == nil {
		return nil, fmt.Errorf("await-idle ls: expected AwaitIdleList response, got kind=%v", resp.Kind)
	}
	if lr.StreamId == 0 {
		return nil, errors.New("await-idle ls: server returned no stream id (could not allocate)")
	}
	st := waitForReceiveStream(ctx, c.Transport(), trsf.StreamID(lr.StreamId))
	if st == nil {
		return nil, fmt.Errorf("await-idle ls: stream %d not visible after response", lr.StreamId)
	}
	var raw []byte
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, eof, rerr := st.ReadDirect(64 * 1024)
		if rerr != nil {
			return nil, fmt.Errorf("await-idle ls: read: %w", rerr)
		}
		raw = append(raw, data...)
		if eof {
			break
		}
	}
	var body protocol.AwaitIdleListBody
	if derr := body.DecodeExactCopy(raw); derr != nil {
		return nil, fmt.Errorf("await-idle ls: decode: %w", derr)
	}
	return body.Watchers, nil
}

// AwaitIdleList is the short-lived-CLI form of AwaitIdleListWith.
func AwaitIdleList(ctx context.Context, peerCID objproto.ConnectionID, taskFilter string) ([]protocol.AwaitIdleWatcherInfo, error) {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.AwaitIdleListWith(ctx, taskFilter)
}

// AwaitIdleKillWith disarms one watcher by id.
func (c *Client) AwaitIdleKillWith(ctx context.Context, id uint64) error {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AwaitIdleKill}
	req.SetAwaitIdleKill(protocol.AwaitIdleKillRequest{WatcherId: id})
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return err
	}
	kr := resp.AwaitIdleKill()
	if kr == nil {
		return fmt.Errorf("await-idle kill: expected AwaitIdleKill response, got kind=%v", resp.Kind)
	}
	switch kr.Status {
	case protocol.AwaitIdleKillStatus_Ok:
		return nil
	case protocol.AwaitIdleKillStatus_NotFound:
		// Unknown, already fired, or not the caller's — deliberately one
		// answer, so a foreign id is not an existence oracle.
		return fmt.Errorf("await-idle kill: no such watcher %d", id)
	default:
		return fmt.Errorf("await-idle kill: %s", kr.Status.String())
	}
}

// AwaitIdleKill is the short-lived-CLI form of AwaitIdleKillWith.
func AwaitIdleKill(ctx context.Context, peerCID objproto.ConnectionID, id uint64) error {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.AwaitIdleKillWith(ctx, id)
}

// AwaitIdleStatusString renders the wire enum in the schema's snake_case (the
// generated String() is CamelCase). The CLI's JSON and the wasm bridge both
// print it; there used to be one hand-written copy in each.
func AwaitIdleStatusString(s protocol.AwaitIdleStatus) string {
	switch s {
	case protocol.AwaitIdleStatus_Fired:
		return "fired"
	case protocol.AwaitIdleStatus_Armed:
		return "armed"
	case protocol.AwaitIdleStatus_SessionStopped:
		return "session_stopped"
	case protocol.AwaitIdleStatus_NotFound:
		return "not_found"
	case protocol.AwaitIdleStatus_BadRequest:
		return "bad_request"
	case protocol.AwaitIdleStatus_Cancelled:
		return "cancelled"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

func AwaitIdleSinkString(s protocol.AwaitIdleSink) string {
	switch s {
	case protocol.AwaitIdleSink_Reply:
		return "reply"
	case protocol.AwaitIdleSink_Notify:
		return "notify"
	case protocol.AwaitIdleSink_Board:
		return "board"
	default:
		return fmt.Sprintf("unknown(%d)", int(s))
	}
}

// AwaitIdleWatcherBy names who armed a watcher: "operator" for the zero
// principal, otherwise the task's 8-hex prefix.
func AwaitIdleWatcherBy(w *protocol.AwaitIdleWatcherInfo) string {
	if w.Requester.Id == ([16]byte{}) {
		return "operator"
	}
	return hex.EncodeToString(w.Requester.Id[:])[:8]
}

// AwaitIdleWatcherLines renders the listing as text. An empty list says so
// rather than printing nothing, so "none armed" and "the command printed
// nothing" cannot be confused.
func AwaitIdleWatcherLines(ws []protocol.AwaitIdleWatcherInfo) []string {
	if len(ws) == 0 {
		return []string{"no armed watchers"}
	}
	out := make([]string, 0, len(ws))
	for i := range ws {
		w := &ws[i]
		age := "-"
		if w.ArmedUnixMs > 0 {
			age = time.Since(time.UnixMilli(int64(w.ArmedUnixMs))).Truncate(time.Second).String()
		}
		sink := "sink=" + AwaitIdleSinkString(w.Sink)
		if w.Sink == protocol.AwaitIdleSink_Board {
			sink += " topic=" + string(w.Topic)
		}
		out = append(out, fmt.Sprintf("%-6d %-8s  %s  threshold=%dms  armed=%s  by=%s",
			w.WatcherId, hex.EncodeToString(w.TaskId.Id[:])[:8], sink, w.ThresholdMs, age, AwaitIdleWatcherBy(w)))
	}
	return out
}

// AwaitIdleWatcherJSONLine renders one row with the full ids. requester is ""
// for the operator.
func AwaitIdleWatcherJSONLine(w *protocol.AwaitIdleWatcherInfo) string {
	requester := ""
	if w.Requester.Id != ([16]byte{}) {
		requester = hex.EncodeToString(w.Requester.Id[:])
	}
	row := struct {
		WatcherID   uint64 `json:"watcher_id"`
		TaskID      string `json:"task_id"`
		Sink        string `json:"sink"`
		Topic       string `json:"topic"`
		ThresholdMs uint32 `json:"threshold_ms"`
		ArmedUnixMs uint64 `json:"armed_unix_ms"`
		Requester   string `json:"requester"`
		OriginKind  string `json:"origin_kind"`
		OriginCID   string `json:"origin_cid"`
	}{
		WatcherID:   w.WatcherId,
		TaskID:      hex.EncodeToString(w.TaskId.Id[:]),
		Sink:        AwaitIdleSinkString(w.Sink),
		Topic:       string(w.Topic),
		ThresholdMs: w.ThresholdMs,
		ArmedUnixMs: w.ArmedUnixMs,
		Requester:   requester,
		OriginKind:  w.OriginKind.String(),
		OriginCID:   string(w.OriginCid),
	}
	b, err := json.Marshal(row)
	if err != nil {
		return "{}"
	}
	return string(b)
}
```

Delete `awaitIdleStatusStr` from `cmd/harness-cli/session.go` and from `cmd/harness-webui-wasm/main.go`; replace every call with `cli.AwaitIdleStatusString` (`grep -rn awaitIdleStatusStr cmd/` must return nothing afterwards).

- [ ] **Step 4: Run**

Run: `go test ./cli -run AwaitIdle -count=1` → PASS. Run: `make check` → PASS (this also builds the wasm target).

- [ ] **Step 5: Commit**

```bash
git add cli/await_idle.go cli/await_idle_test.go cmd/harness-cli/session.go cmd/harness-webui-wasm/main.go
git commit -m "cli: await-idle list/kill client and one status renderer"
```

---

### Task 5: Verb rows and the CLI surface

**Files:**
- Modify: `cli/verb/table.go` (two rows after `session await-idle`)
- Regenerate: `cli/verb/actions_gen.go` (`go generate ./cli/verb`)
- Modify: `cmd/harness-cli/dispatch.go` (two handlers)
- Modify: `cmd/harness-cli/session.go` (`runSessionAwaitIdleWith`: `watcher_id` in JSON, exit 4)
- Modify: `cli/verb/consumers_test.go` (`verbConsumers` + `actionFor` entries for the two paths, as `exec ls` / `exec kill` have)
- Test: `cli/verb/route_test.go` (or the file holding path-routing tests — `grep -ln "func TestRoute" cli/verb/*_test.go`)

**Interfaces:**
- Consumes: Task 4 client methods and renderers.
- Produces: `SessionAction.TaskFilter string`, `SessionAction.WatcherIDs []uint64`; generated handler methods `SessionAwaitIdleLs(SessionAction)` / `SessionAwaitIdleKill(SessionAction)` on the CLI, TUI and WebUI handler interfaces (check the generated names after `go generate`; later tasks use whatever it emits).

- [ ] **Step 1: Write the failing routing test**

Append to `cli/verb/route_test.go`. `ParseCLICommand` / `ParseTUICommand` (generated, `actions_gen.go`) match the verb path longest-first, which is what must send `ls` to the child and a task id to the parent — the first depth-2 verb-and-parent in the table:

```go
func TestSessionAwaitIdleChildrenRoute(t *testing.T) {
	const id = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, parse := range []struct {
		name string
		fn   func([]string, map[string]string) (Action, bool, error)
	}{{"cli", ParseCLICommand}, {"tui", ParseTUICommand}} {
		for _, tc := range []struct {
			line string
			sub  string
		}{
			{"session await-idle ls", "await-idle-ls"},
			{"session await-idle ls --task " + id, "await-idle-ls"},
			{"session await-idle kill 3 4", "await-idle-kill"},
			{"session await-idle " + id, "await-idle"},
		} {
			act, handled, err := parse.fn(strings.Fields(tc.line), nil)
			if err != nil || !handled {
				t.Fatalf("%s %q: handled=%v err=%v", parse.name, tc.line, handled, err)
			}
			sa, ok := act.(SessionAction)
			if !ok || sa.Sub != tc.sub {
				t.Fatalf("%s %q: got %#v, want SessionAction Sub=%q", parse.name, tc.line, act, tc.sub)
			}
			if tc.sub == "await-idle-kill" && !reflect.DeepEqual(sa.WatcherIDs, []uint64{3, 4}) {
				t.Fatalf("%s %q: WatcherIDs = %v", parse.name, tc.line, sa.WatcherIDs)
			}
			if tc.sub == "await-idle" && sa.TaskID != id {
				t.Fatalf("%s %q: TaskID = %q", parse.name, tc.line, sa.TaskID)
			}
		}
	}
	// `kill` with no id is a mistyped line, as `exec kill` is.
	if _, _, err := ParseCLICommand(strings.Fields("session await-idle kill"), nil); err == nil {
		t.Fatal("bare `session await-idle kill` parsed")
	}
}
```

(add `reflect` and `strings` to the file's imports if absent; if `WatcherIDs` generates as `[]uint`, compare against that type — read `SessionAction` in `actions_gen.go` after Step 3.)

- [ ] **Step 2: Run to confirm it fails**

Run: `go test ./cli/verb -run SessionAwaitIdleChildrenRoute -count=1`
Expected: FAIL (no such path).

- [ ] **Step 3: Declare the rows**

In `cli/verb/table.go`, directly after the `session await-idle` row:

```go
	{
		Path:          []string{"session", "await-idle", "ls"},
		WebUIDispatch: WebUIDispatch{Fn: "awaitIdleList"},
		SurfaceNotes: map[Surface][]string{
			TUI:   {"list the armed await-idle watchers (I opens the list; x kills a row)"},
			WebUI: {"list the armed await-idle watchers / kill one"},
		},
		ModalSurfaces: []ModalSurface{
			{Surface: TUI, At: "tui/idlewatchersmodal.go:IdleWatchersModal"},
			{Surface: WebUI, At: "webui/index.html#await-idle-list"},
		},
		Notes: []string{
			"list armed watchers: an agent sees the ones it armed, the operator every one; --task filters, --json emits JSON lines",
		},
		Action:          "SessionAction",
		Const:           map[string]string{"Sub": "await-idle-ls"},
		CmdlineSurfaces: CLI | TUI | WebUI,
		Flags: []Flag{
			{Name: "task", Type: FlagString, Default: "", Field: "TaskFilter", Help: "only watchers on this task id"},
			{Name: "json", Type: FlagBool, Default: false, Field: "JSON",
				CmdlineSurfaces: CLI | WebUI,
				SurfaceReason:   "the TUI renders into a results pane, not a pipe, so there is nothing for JSON to be read by",
				Help:            "one JSON object per watcher"},
		},
		Examples: []string{"session await-idle ls", "session await-idle ls --json"},
	},
	{
		Path:          []string{"session", "await-idle", "kill"},
		WebUIDispatch: WebUIDispatch{Fn: "awaitIdleKill"},
		SurfaceNotes:  map[Surface][]string{TUI: {"disarm one watcher"}},
		ModalSurfaces: []ModalSurface{
			{Surface: TUI, At: "tui/idlewatchersmodal.go:IdleWatchersModal"},
			// Each row of the watcher list carries its own kill button.
			{Surface: WebUI, At: "webui/index.html#await-idle-list"},
		},
		Notes: []string{
			"disarm one or more watchers by id (from `session await-idle ls`, or the `watcher_id` an arm printed); nothing is delivered for a killed watcher, except `cancelled` to a caller still blocked on it",
		},
		// At least one id, as `exec kill`: none is a mistyped line.
		MinArgs:         1,
		Action:          "SessionAction",
		Const:           map[string]string{"Sub": "await-idle-kill"},
		CmdlineSurfaces: CLI | TUI | WebUI,
		Args:            []Arg{{Name: "watcher-id", Type: ArgUint, Variadic: true, Field: "WatcherIDs"}},
		Examples:        []string{"session await-idle kill 3"},
	},
```

Update the `session await-idle` row's `Notes` second line to: `"default long-polls; --notify/--topic arm a server-side sink and return. Every arm prints its watcher_id; see `session await-idle ls` / `kill`"`.

Run: `go generate ./cli/verb`

- [ ] **Step 4: CLI handlers**

In `cmd/harness-cli/dispatch.go`, beside `SessionAwaitIdle`:

```go
func (h cliVerbs) SessionAwaitIdleLs(a verb.SessionAction) error {
	ws, err := cli.AwaitIdleList(h.ctx, h.cid(), a.TaskFilter)
	if err != nil {
		return err
	}
	if a.JSON {
		for i := range ws {
			fmt.Println(cli.AwaitIdleWatcherJSONLine(&ws[i]))
		}
		return nil
	}
	for _, line := range cli.AwaitIdleWatcherLines(ws) {
		fmt.Println(line)
	}
	return nil
}

// Every id, even after one fails — the shape ExecKill records the reason for.
func (h cliVerbs) SessionAwaitIdleKill(a verb.SessionAction) error {
	var failed error
	for _, id := range a.WatcherIDs {
		if err := cli.AwaitIdleKill(h.ctx, h.cid(), id); err != nil {
			fmt.Fprintf(os.Stderr, "await-idle kill %d: %v\n", id, err)
			failed = err
			continue
		}
		fmt.Printf("killed await-idle watcher %d\n", id)
	}
	return failed
}
```

In `cmd/harness-cli/session.go` `runSessionAwaitIdleWith`, the JSON map gains `"watcher_id": resp.WatcherId`, and the status switch gains:

```go
	case protocol.AwaitIdleStatus_Cancelled:
		os.Exit(4) // killed via `session await-idle kill`; distinct from fired and session_stopped
```

In `cli/verb/consumers_test.go`, add `verbConsumers` rows for both paths on CLI (`../../cmd/harness-cli/dispatch.go`), TUI (`../../tui/dispatch.go`, `../../tui/idlewatchersmodal.go`) and WebUI (`../../webui/static/main.js`), and `actionFor` entries `"session await-idle ls": verb.SessionAction{}`, `"session await-idle kill": verb.SessionAction{}` — copy the exact shape of the `exec ls` / `exec kill` entries. The TUI/WebUI rows will stay red until Tasks 6–7; that is expected.

- [ ] **Step 5: Run**

Run: `go test ./cli/verb -count=1`
Expected: the routing test PASSES; only the TUI/WebUI consumer rows for the two new paths fail (they are satisfied by Tasks 6–7). Any OTHER failure is this task's to fix — in particular `TestUsageNamesItsVerb` / `TestUsagePositionalsParse` on a depth-2 verb-and-parent, which the spec flags as untested ground.

- [ ] **Step 6: Commit**

```bash
git add cli/verb cmd/harness-cli
git commit -m "cli: session await-idle ls / kill; arm prints watcher_id, cancelled exits 4"
```

---

### Task 6: TUI

**Files:**
- Create: `tui/idlewatchersmodal.go`
- Modify: `tui/keys.go` (`IdleWatchers: "I"` + binding row)
- Modify: `tui/actions.go` (`onIdleWatchers`)
- Modify: `tui/overlays.go` (`appOverlays` entry + `inIdleWatchersModal`)
- Modify: `tui/app.go` (field, constructor, `SetSize` beside `execsModal`, `View` beside `execsModal`, the two messages, `AwaitIdleResultMsg` gains `WatcherID` + `Cancelled` case)
- Modify: `tui/client.go` (`AwaitIdleResultMsg.WatcherID`; `DoAwaitIdle` fills it; `DoIdleWatcherList`, `DoIdleWatcherKill`, their msgs)
- Modify: `tui/dispatch.go` (`SessionAwaitIdleLs`, `SessionAwaitIdleKill`)
- Test: `tui/idlewatchersmodal_test.go`

**Interfaces:**
- Consumes: `(*cli.Client).AwaitIdleListWith`, `AwaitIdleKillWith`, `cli.AwaitIdleWatcherLines`, `cli.AwaitIdleSinkString`, `cli.AwaitIdleWatcherBy`.
- Produces: `IdleWatcherListMsg{Watchers []protocol.AwaitIdleWatcherInfo; Err error; ToCmdresult bool}`, `IdleWatcherKillMsg{WatcherID uint64; Err error}`.

- [ ] **Step 1: Write the failing modal test**

`tui/idlewatchersmodal_test.go`:

```go
package tui

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestIdleWatchersModal_KillTargetsSelectedRowOnly(t *testing.T) {
	m := NewIdleWatchersModal()
	m.SetSize(120, 30)
	ws := []protocol.AwaitIdleWatcherInfo{{WatcherId: 4}, {WatcherId: 9}}
	m.ApplySnapshot(ws)
	m.table.SetCursor(1)
	if !m.BeginKillConfirm() {
		t.Fatal("BeginKillConfirm with a row selected returned false")
	}
	id, ok := m.ConfirmKill()
	if !ok || id != 9 {
		t.Fatalf("ConfirmKill = %d,%v, want 9,true", id, ok)
	}
	if m.IsConfirming() {
		t.Fatal("still confirming after ConfirmKill")
	}
}

func TestIdleWatchersModal_EmptySaysSo(t *testing.T) {
	m := NewIdleWatchersModal()
	m.SetSize(120, 30)
	m.ApplySnapshot(nil)
	if v := stripANSI(m.View()); !strings.Contains(v, "no armed watchers") {
		t.Fatalf("view = %q", v)
	}
}
```

(`stripANSI` is the package's test helper in `tui/editbuf_test.go`; add `strings` to the imports.)

- [ ] **Step 2: Run to confirm it fails**

Run: `go test ./tui -run IdleWatchersModal -count=1` → FAIL to compile.

- [ ] **Step 3: The modal**

`tui/idlewatchersmodal.go` — `ExecsModal`'s structure with its `ApplyEvent` removed (watchers have no push subscription, like forwards):

```go
package tui

import (
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// idleWatcherRow maps one armed watcher to its row. Sink and "by" go through
// the cli renderers so this and `session await-idle ls` cannot disagree.
// Age is computed here, against now, for execRunInfoRow's reason.
func idleWatcherRow(w *protocol.AwaitIdleWatcherInfo, now time.Time) table.Row {
	age := "-"
	if w.ArmedUnixMs > 0 {
		age = now.Sub(time.UnixMilli(int64(w.ArmedUnixMs))).Truncate(time.Second).String()
	}
	sink := cli.AwaitIdleSinkString(w.Sink)
	if w.Sink == protocol.AwaitIdleSink_Board {
		sink += " " + string(w.Topic)
	}
	return table.Row{
		fmt.Sprintf("%d", w.WatcherId),
		pfShortID(FormatTaskID(w.TaskId)),
		sink,
		fmt.Sprintf("%dms", w.ThresholdMs),
		age,
		cli.AwaitIdleWatcherBy(w),
	}
}

// IdleWatchersModal lists the armed await-idle watchers this operator can
// see. Opened with `I`, closed with Esc, `x` arms a y/n kill confirmation.
//
// ForwardsModal-shaped: no ApplyEvent, because watchers have no push
// subscription — it is fetched on open and after a kill.
type IdleWatchersModal struct {
	open     bool
	table    table.Model
	baseCols []table.Column
	watchers []protocol.AwaitIdleWatcherInfo

	// confirmID == 0 means none: watcher ids start at 1.
	confirmID   uint64
	confirmTask string
}

// sink is the flex column: a board topic has no bound.
func NewIdleWatchersModal() IdleWatchersModal {
	cols := []table.Column{
		{Title: "id", Width: 6},
		{Title: "task", Width: 12},
		{Title: "sink", Width: 30},
		{Title: "threshold", Width: 10},
		{Title: "armed", Width: 9},
		{Title: "by", Width: 10},
	}
	t := table.New(table.WithColumns(cols), table.WithFocused(true))
	return IdleWatchersModal{table: t, baseCols: cols}
}

func (m *IdleWatchersModal) IsOpen() bool { return m.open }
func (m *IdleWatchersModal) Open()        { m.open = true }
func (m *IdleWatchersModal) Close()       { m.open = false }

func (m *IdleWatchersModal) SetSize(w, h int) {
	m.table.SetWidth(w - 4)
	m.table.SetColumns(fitColumns(m.baseCols, w-4, flexColumn(m.baseCols, "sink")))
	m.table.SetHeight(h - 4)
}

func (m *IdleWatchersModal) ApplySnapshot(ws []protocol.AwaitIdleWatcherInfo) {
	m.watchers = make([]protocol.AwaitIdleWatcherInfo, len(ws))
	copy(m.watchers, ws)
	now := time.Now()
	rows := make([]table.Row, 0, len(m.watchers))
	for i := range m.watchers {
		rows = append(rows, idleWatcherRow(&m.watchers[i], now))
	}
	setTableRows(&m.table, rows)
}

func (m *IdleWatchersModal) SelectedID() (uint64, bool) {
	if len(m.watchers) == 0 {
		return 0, false
	}
	i := m.table.Cursor()
	if i < 0 || i >= len(m.watchers) {
		return 0, false
	}
	return m.watchers[i].WatcherId, true
}

func (m *IdleWatchersModal) IsConfirming() bool { return m.confirmID != 0 }

// BeginKillConfirm asks before killing, as ExecsModal does: the operator sees
// every agent's watchers, so the row under the cursor may be someone else's
// insurance.
func (m *IdleWatchersModal) BeginKillConfirm() bool {
	id, ok := m.SelectedID()
	if !ok {
		return false
	}
	m.confirmID = id
	m.confirmTask = FormatTaskID(m.watchers[m.table.Cursor()].TaskId)
	return true
}

func (m *IdleWatchersModal) CancelKillConfirm() { m.confirmID, m.confirmTask = 0, "" }

func (m *IdleWatchersModal) ConfirmKill() (uint64, bool) {
	if m.confirmID == 0 {
		return 0, false
	}
	id := m.confirmID
	m.CancelKillConfirm()
	return id, true
}

func (m IdleWatchersModal) Update(msg tea.Msg) (IdleWatchersModal, tea.Cmd) {
	if !m.open {
		return m, nil
	}
	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

func (m IdleWatchersModal) View() string {
	header := HeaderStyle.Render(fmt.Sprintf("armed await-idle watchers (%d)", len(m.watchers)))
	box := PanelStyleFocused.Padding(0, 1)
	if m.confirmID != 0 {
		prompt := fmt.Sprintf("kill watcher %d (%s) ? (y/n)", m.confirmID, pfShortID(m.confirmTask))
		return box.Render(header + "\n" + m.table.View() + "\n" + FooterStyle.Render(prompt))
	}
	footer := FooterStyle.Render("x: kill · Esc: close")
	if len(m.watchers) == 0 {
		return box.Render(header + "\n" + "no armed watchers" + "\n" + footer)
	}
	return box.Render(header + "\n" + m.table.View() + "\n" + footer)
}
```

- [ ] **Step 4: Wire it**

`tui/client.go`, beside `DoAwaitIdle`:

```go
// IdleWatcherListMsg carries a watcher listing. ToCmdresult marks the cmdline
// path, whose text belongs in the result pane — see ExecRunListMsg.
type IdleWatcherListMsg struct {
	Watchers    []protocol.AwaitIdleWatcherInfo
	Err         error
	ToCmdresult bool
}

type IdleWatcherKillMsg struct {
	WatcherID uint64
	Err       error
}

func DoIdleWatcherList(c *cli.Client, taskFilter string, toCmdresult bool) tea.Cmd {
	return func() tea.Msg {
		if c == nil {
			return IdleWatcherListMsg{Err: fmt.Errorf("not connected to server"), ToCmdresult: toCmdresult}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ws, err := c.AwaitIdleListWith(ctx, taskFilter)
		return IdleWatcherListMsg{Watchers: ws, Err: err, ToCmdresult: toCmdresult}
	}
}

func DoIdleWatcherKill(c *cli.Client, id uint64) tea.Cmd {
	return func() tea.Msg {
		if c == nil {
			return IdleWatcherKillMsg{WatcherID: id, Err: fmt.Errorf("not connected to server")}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return IdleWatcherKillMsg{WatcherID: id, Err: c.AwaitIdleKillWith(ctx, id)}
	}
}
```

`AwaitIdleResultMsg` gains `WatcherID uint64`; `DoAwaitIdle` sets it from `resp.WatcherId`.

`tui/keys.go`: field `IdleWatchers string` beside `Execs`; value `IdleWatchers: "I",`; binding row after the `Execs` row:

```go
	{Keys: []string{mainKeys.IdleWatchers}, Scope: scopeGlobal, Do: (*App).onIdleWatchers, Short: "I watchers", Long: "armed await-idle watchers (x kills the selected row)"},
```

`tui/actions.go`, after `onExecs`:

```go
// `I` opens the armed await-idle watcher list.
func (a *App) onIdleWatchers(msg tea.KeyMsg) (tea.Cmd, bool) {
	if a.client == nil {
		a.cmdresult.Append(WarnStyle.Render("watchers: not connected"))
		return nil, true
	}
	a.idleWatchersModal.SetSize(a.width, a.height)
	a.idleWatchersModal.Open()
	return DoIdleWatcherList(a.client, "", false), true
}
```

`tui/overlays.go`: add `{func(a *App) bool { return a.idleWatchersModal.IsOpen() }, (*App).inIdleWatchersModal},` directly after the `execsModal` entry, and `inIdleWatchersModal` — `inExecsModal` with `execsModal` → `idleWatchersModal` and `DoExecRunKill` → `DoIdleWatcherKill`.

`tui/app.go`:
- field `idleWatchersModal IdleWatchersModal` beside `execsModal`; `idleWatchersModal: NewIdleWatchersModal(),` in the constructor;
- `a.idleWatchersModal.SetSize(a.width, a.height)` beside `a.execsModal.SetSize` (~line 1444);
- in `View`, beside the `execsModal.IsOpen()` branch (~line 1778), the same `lipgloss.Place` for `a.idleWatchersModal.View()`;
- message cases beside `ExecRunListMsg` / `ExecRunKillMsg`:

```go
	case IdleWatcherListMsg:
		if msg.Err != nil {
			a.cmdresult.Append(ErrorStyle.Render(fmt.Sprintf("await-idle ls: %v", msg.Err)))
			return a, nil
		}
		a.idleWatchersModal.ApplySnapshot(msg.Watchers)
		if msg.ToCmdresult {
			for _, line := range cli.AwaitIdleWatcherLines(msg.Watchers) {
				a.cmdresult.Append(line)
			}
		}
		return a, nil

	case IdleWatcherKillMsg:
		if msg.Err != nil {
			a.cmdresult.Append(ErrorStyle.Render(fmt.Sprintf("await-idle kill %d: %v", msg.WatcherID, msg.Err)))
			return a, nil
		}
		a.cmdresult.Append(OKStyle.Render(fmt.Sprintf("killed await-idle watcher %d", msg.WatcherID)))
		if a.idleWatchersModal.IsOpen() {
			return a, DoIdleWatcherList(a.client, "", false)
		}
		return a, nil
```

- in the `AwaitIdleResultMsg` switch: the `Armed` line becomes `"await-idle " + short + fmt.Sprintf(": armed (watcher %d)", msg.WatcherID)`, and add

```go
		case protocol.AwaitIdleStatus_Cancelled:
			a.cmdresult.Append(WarnStyle.Render(fmt.Sprintf("await-idle %s: cancelled (watcher %d killed)", short, msg.WatcherID)))
```

`tui/dispatch.go`, beside `SessionAwaitIdle`:

```go
func (h tuiVerbs) SessionAwaitIdleLs(v verb.SessionAction) tea.Cmd {
	a := h.a
	// TaskFilter narrows a listing, as `exec ls --task` does.
	filter := ""
	if v.TaskFilter != "" {
		full, errStr := a.resolveTaskIDPrefix(v.TaskFilter)
		if errStr != "" {
			a.cmdresult.Append(ErrorStyle.Render("await-idle ls: " + errStr))
			return nil
		}
		filter = full
	}
	return DoIdleWatcherList(a.client, filter, true)
}

func (h tuiVerbs) SessionAwaitIdleKill(v verb.SessionAction) tea.Cmd {
	var cmds []tea.Cmd
	for _, id := range v.WatcherIDs {
		cmds = append(cmds, DoIdleWatcherKill(h.a.client, id))
	}
	return tea.Batch(cmds...)
}
```

- [ ] **Step 5: Run**

Run: `go test ./tui -count=1` → PASS (includes `keys_test`'s map/binding pair rule).
Run: `go test ./cli/verb -count=1` → only the WebUI consumer rows remain red.

- [ ] **Step 6: Commit**

```bash
git add tui/
git commit -m "tui: armed await-idle watcher list (I) with kill, and the ids on arm results"
```

---

### Task 7: WebUI and the wasm bridge

**Files:**
- Modify: `cmd/harness-webui-wasm/main.go` (bridge exports `awaitIdleList`, `awaitIdleKill`; `awaitIdle` returns `watcherId`; snapshot key `idle_watchers`)
- Modify: `webui/index.html` (panel after `#exec-list`)
- Modify: `webui/static/main.js` (`renderIdleWatcherList`, call it from the snapshot render, 🔔 results name the id, `session` case routes `ls` / `kill`)
- Test: `webui/static/cmd_test.mjs`

**Interfaces:**
- Consumes: Task 4 client methods and `cli.AwaitIdleSinkString`, `cli.AwaitIdleWatcherBy`, `cli.AwaitIdleStatusString`.
- Produces (JS): `harness.awaitIdleList(taskFilterHex?) -> Promise<[{watcherId, taskId, sink, topic, thresholdMs, armedUnixMs, by, originKind, originCid}]>`; `harness.awaitIdleKill(id) -> Promise<id>`; `harness.awaitIdle(...)` resolves `{status, lastOutputAt, watcherId}`; snapshot `idle_watchers: [{watcher_id, task, sink, topic, threshold_ms, armed_unix_ms, by}]`.

- [ ] **Step 1: Write the failing JS test**

Append to `webui/static/cmd_test.mjs`:

```js
test("session await-idle kill acts on every id; ls asks the bridge", async () => {
  const k = await run("session await-idle kill 3 4");
  eq(named(k.calls, "awaitIdleKill").map((c) => c[1]), [3, 4]);
  const l = await run("session await-idle ls");
  eq(named(l.calls, "awaitIdleList").length, 1);
});
```

If the fake harness in this file enumerates its methods rather than recording any call, add `awaitIdleList: async () => []` and `awaitIdleKill: async (id) => id` to it, beside `execRunKill`.

- [ ] **Step 2: Run to confirm it fails**

Run: `make js-test` → the new test FAILS.

- [ ] **Step 3: Bridge**

In `cmd/harness-webui-wasm/main.go`, register beside `"execRunKill"`:

```go
		"awaitIdleList": js.FuncOf(harnessAwaitIdleList),
		"awaitIdleKill": js.FuncOf(harnessAwaitIdleKill),
```

and, after `harnessExecRunKill`, two functions with `harnessExecRunList` / `harnessExecRunKill`'s exact structure (currentClient, rootCtx, rejectErr), calling `c.AwaitIdleListWith(rootCtx, filter)` and `c.AwaitIdleKillWith(rootCtx, id)`. The list row map:

```go
				rows = append(rows, map[string]any{
					"watcherId":   float64(w.WatcherId),
					"taskId":      hex.EncodeToString(w.TaskId.Id[:]),
					"sink":        cli.AwaitIdleSinkString(w.Sink),
					"topic":       string(w.Topic),
					"thresholdMs": float64(w.ThresholdMs),
					"armedUnixMs": float64(w.ArmedUnixMs),
					"by":          cli.AwaitIdleWatcherBy(w),
					"originKind":  w.OriginKind.String(),
					"originCid":   string(w.OriginCid),
				})
```

with doc comments in the `//	harness.awaitIdleList(taskFilterHex?) -> Promise<[…]>` form the neighbours use.

`harnessAwaitIdle`'s resolve map gains `"watcherId": float64(resp.WatcherId)`, and its doc line's return type names it.

In the snapshot builder, after the `execs` block:

```go
			// Watchers ride the same poll as execs and forwards: a shared
			// server-side registry with no push subscription.
			watcherInfos, wErr := c.AwaitIdleListWith(rootCtx, "")
			if wErr != nil {
				slog.Warn("snapshot: AwaitIdleListWith failed (watchers will be empty)", "err", wErr)
			}
			idleWatchers := make([]any, 0, len(watcherInfos))
			for i := range watcherInfos {
				w := &watcherInfos[i]
				idleWatchers = append(idleWatchers, map[string]any{
					"watcher_id":    float64(w.WatcherId),
					"task":          hex.EncodeToString(w.TaskId.Id[:]),
					"sink":          cli.AwaitIdleSinkString(w.Sink),
					"topic":         string(w.Topic),
					"threshold_ms":  float64(w.ThresholdMs),
					// RAW, for the exec rows' reason: the page re-renders per poll.
					"armed_unix_ms": float64(w.ArmedUnixMs),
					"by":            cli.AwaitIdleWatcherBy(w),
				})
			}
```

and `"idle_watchers": idleWatchers,` in the resolved map.

- [ ] **Step 4: Page**

`webui/index.html`, after `<div id="exec-list"></div>`:

```html
      <h3>await-idle watcher</h3>
      <div id="await-idle-list"></div>
```

`webui/static/main.js`: after `renderExecList(snap.execs || []);` add `renderIdleWatcherList(snap.idle_watchers || []);`, and after `renderExecList`'s definition:

```js
  // renderIdleWatcherList draws one row per armed await-idle watcher this
  // operator can see, each with a kill button — renderExecList's shape and
  // CSS, because `session await-idle ls` / `kill` is the same list-and-kill
  // pair. It is the answer to "did I arm one on that task?".
  function renderIdleWatcherList(watchers) {
    const host = document.getElementById("await-idle-list");
    if (!host) return;
    host.textContent = "";
    if (!watchers.length) {
      const empty = document.createElement("div");
      empty.className = "forward-list-empty";
      empty.textContent = "armed な watcher はありません";
      host.appendChild(empty);
      return;
    }
    const now = Date.now();
    for (const w of watchers) {
      const row = document.createElement("div");
      row.className = "exec-row";
      const taskShort = w.task ? w.task.slice(0, 8) + "…" : "-";
      const age = w.armed_unix_ms
        ? `${Math.max(0, Math.round((now - w.armed_unix_ms) / 1000))}s`
        : "-";
      const sink = w.sink === "board" ? `board ${w.topic}` : w.sink;
      for (const text of [`#${w.watcher_id}`, taskShort, sink, `${w.threshold_ms}ms`, age, `by ${w.by}`]) {
        const cell = document.createElement("span");
        cell.className = "forward-cell";
        cell.textContent = text;
        row.appendChild(cell);
      }
      const kill = document.createElement("button");
      kill.type = "button";
      kill.className = "btn-danger";
      kill.textContent = "kill";
      kill.addEventListener("click", async () => {
        // Confirmed: the operator sees every agent's watchers, so this row
        // may be someone else's insurance.
        if (!window.confirm(`Kill await-idle watcher #${w.watcher_id} on ${taskShort}?`)) return;
        kill.disabled = true;
        try {
          await window.harness.awaitIdleKill(w.watcher_id);
          appendCmdOutput(`killed await-idle watcher #${w.watcher_id}`);
          refreshSnapshot();
        } catch (err) {
          appendCmdOutput(`await-idle kill error: ${err.message}`);
          kill.disabled = false;
        }
      });
      row.appendChild(kill);
      host.appendChild(row);
    }
  }
```

The three 🔔 result sites (`grep -n "awaitIdle({" webui/static/main.js`) print the id: e.g. `appendCmdOutput(\`await-idle ${id.slice(0, 12)}: ${r.status}${r.watcherId ? \` (watcher ${r.watcherId})\` : ""}\`, true);` — and the header button's transient label likewise.

In the `session` case, replace the `if (tokens[1] === "await-idle") { … }` block with:

```js
      if (tokens[1] === "await-idle") {
        const b = parseOrHelp(ctx, ["session", "await-idle", ...tokens.slice(2)], {});
        const sub = b.path.length > 2 ? b.path[2] : "arm";
        if (sub === "ls") {
          const ws = await ctx.harness.awaitIdleList(b.flags.task || undefined);
          out = ws.length
            ? (b.flags.json
                ? ws.map((w) => JSON.stringify(w)).join("\n")
                : ws.map((w) => `#${w.watcherId}  ${String(w.taskId).slice(0, 8)}…  ${w.sink}${w.sink === "board" ? ` ${w.topic}` : ""}  by ${w.by}`).join("\n"))
            : "(no armed watchers)";
          break;
        }
        if (sub === "kill") {
          // Every id, even after one fails, as `exec kill`.
          const failed = [];
          for (const id of b.args) {
            try { await ctx.harness.awaitIdleKill(Number(id)); }
            catch (e) { failed.push(`${id}: ${e.message}`); }
          }
          if (failed.length) throw new Error(`await-idle kill: ${failed.join("; ")}`);
          out = `killed await-idle watcher ${b.args.join(", ")}`;
          break;
        }
        // Parsed by the shared declaration, which also refuses --notify
        // with --topic: two sinks for one fire.
        const sink = b.flags.notify ? "notify" : (b.flags.topic ? "board" : "reply");
        if (sink === "reply") ctx.echo("await-idle: waiting for the session to go idle…");
        const r = await ctx.harness.awaitIdle({
          taskId: b.args[0], thresholdMs: b.flags["threshold-ms"] || 0,
          sink, topic: b.flags.topic || undefined,
        });
        out = `await-idle ${b.args[0].slice(0, 12)}: ${r.status}${r.watcherId ? ` (watcher ${r.watcherId})` : ""}`;
        break;
      }
```

(Confirm `b.path` for the arm form is length 2 by logging it once in the JS test if unsure.)

- [ ] **Step 5: Run**

Run: `make js-test` → PASS. Run: `make check` → PASS. Run: `go test ./cli/verb -count=1` → PASS (all consumer rows satisfied now).

- [ ] **Step 6: Commit**

```bash
git add cmd/harness-webui-wasm/main.go webui/
git commit -m "webui: armed await-idle watcher panel with kill; ids on arm results"
```

---

### Task 8: Documentation, skill, skew check

**Files:**
- Modify: `README.md` (session verbs section + TUI cmdline verb list + TUI key list)
- Modify: `runner/agentskills/supervising-workers/SKILL.md`, then copy byte-identical to `.claude/skills/supervising-workers/SKILL.md` and `.agents/skills/supervising-workers/SKILL.md`
- Modify: `.claude/skills/surface-parity-checklist/firing-log.md` (one entry)

- [ ] **Step 1: README**

Read README.md in full first (it is prose — Read, then Edit). Find where `session await-idle` would be documented (`grep -n "session await-idle\|await-idle" README.md`; the earlier survey found none, so add it beside the other `session` verbs), and the TUI cmdline verb list and key list. Add:
- `session await-idle ls [--task T] [--json]` — armed watchers (an agent: its own; the operator: all)
- `session await-idle kill <id>...` — disarm; a blocked caller gets `cancelled` (exit 4)
- TUI key `I` — watcher list, `x` kills

- [ ] **Step 2: Skill**

Read `runner/agentskills/supervising-workers/SKILL.md` in full. In the await-idle section:
- Under the `--topic T` bullet, state that the `armed` reply carries `watcher_id`.
- Replace the sentence beginning "And **an armed watcher cannot be disarmed**" through "does not get the wake back." with: "If you armed one anyway and the reply arrived first, `harness-cli session await-idle kill <watcher_id>` disarms it — nothing is published for a killed watcher. `session await-idle ls` shows what you have armed. A redundant fire costs one extra wake, so this is tidiness, not damage control."
- Keep "**Do not arm it for a peer you asked to report back.**" as the default.

Then: `cp runner/agentskills/supervising-workers/SKILL.md .claude/skills/supervising-workers/SKILL.md && cp runner/agentskills/supervising-workers/SKILL.md .agents/skills/supervising-workers/SKILL.md`
Run: `go test ./runner/agentskills -count=1` → PASS (`TestMirrorsMatchEmbeddedSkills`).

- [ ] **Step 3: Skew check**

Run: `scripts/wire-skew-check.sh`
Expected: PASS or its documented no-runner-format exit; record its full output in the commit message body. A setup error (exit 2) is not a pass — fix the setup and re-run.

- [ ] **Step 4: Full verification**

Run: `make check && make vet && make test && make js-test && make test-integration`
Expected: all PASS. Report any failure with its output.

- [ ] **Step 5: Commit**

```bash
git add README.md runner/agentskills .claude/skills/supervising-workers .agents/skills/supervising-workers
git commit -m "docs: await-idle ls / kill in README and supervising-workers"
```

---

### Task 9: Live check on the dummy harness, item 39

**Files:**
- Modify: `.claude/skills/surface-parity-checklist/firing-log.md`
- Modify: `docs/superpowers/specs/2026-09-28-await-idle-cancel-design.md` (item 39 verdict)

- [ ] **Step 1: Bring up a dummy harness**

Follow `.claude/skills/dummy-harness/SKILL.md` (`scripts/dummy-harness.sh`), after `make build` so the binaries under `bin/` are this tree's. Start one interactive session.

- [ ] **Step 2: CLI, in the spelling the help prints**

- `harness-cli session await-idle <task> --topic chat.test` → JSON with `"status":"armed"` and a non-zero `"watcher_id"`.
- `harness-cli session await-idle ls` → that row. `--json` → one object.
- `harness-cli session await-idle kill <id>` → `killed await-idle watcher <id>`; `ls` → `no armed watchers`; `board read chat.test` shows nothing after the session idles.
- Blocking form in one terminal, `kill` from another → the first prints `"status":"cancelled"` and `echo $?` = 4.
- Blocking form, then Ctrl-C it → `ls` no longer lists it (teardown).

- [ ] **Step 3: TUI** — `W` on a task → result line names the watcher; `I` → row; `x`, `y` → gone; cmdline `session await-idle ls` prints the row text.

- [ ] **Step 4: WebUI** — 🔔 → output names the watcher; the panel shows the row within one poll; kill → confirm → gone; command input `session await-idle ls` / `kill <id>`. Save a screenshot of the panel under the scratchpad and report its path (do not delete it).

- [ ] **Step 5: Item 39** — walk the spec's Surfaces table row by row against the code, write the verdict into item 39 of the spec's walk, and add a `firing-log.md` entry (items that came back `done`/`omitted`; note that item 7's wording names `WEBUI_DISPATCH` in `main.js`, which no longer exists — the dispatch map is `WebUIDispatch` in `cli/verb/table.go`).

- [ ] **Step 6: Tear down** the dummy harness (per its skill), then commit:

```bash
git add docs/superpowers/specs/2026-09-28-await-idle-cancel-design.md .claude/skills/surface-parity-checklist/firing-log.md
git commit -m "docs: await-idle cancel — item 39 walked after the live run"
```
