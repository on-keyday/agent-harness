# Parallel Agent Harness v1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Claude Code CLI を並列起動・管理できる local-only task dispatcher (spec: `docs/superpowers/specs/2026-04-25-parallel-agent-harness-design.md`)。

**Architecture:** 既存の objproto (secure session) + trsf (stream mux) + pubsub (topic broker) の上に、`server / runner / cli` 3 コンポーネントを載せる。wire kind `RunnerControl` / `TaskControl` を埋めて制御系、log は pubsub topic (`task.<id>.log`) で fanout する。

**Tech Stack:** Go 1.25.7、`.bgn` (brgen) schema、既存の WebSocket + ECDH transport、git worktree。

**Open question 確定:** §9.1 auto-commit は **(b) 触らない** で v1 確定。runner は git 操作を worktree 作成のみで止め、`TaskFinished.diff_info` は空 byte 列を送る。v2 で opt-in flag 化を再検討。

---

## File structure

### Create
- `runner/worktree.go` — git worktree add/remove
- `runner/process.go` — claude subprocess exec + stdout/stderr streaming
- `runner/session.go` — server への接続 / Hello / AssignTask loop
- `runner/worktree_test.go`, `runner/process_test.go`, `runner/session_test.go`
- `server/registry.go` — RunnerRegistry (in-memory)
- `server/taskstore.go` — TaskStore (in-memory + WAL)
- `server/scheduler.go` — Queued task → Idle runner assignment
- `server/dispatch.go` — wire kind router
- `server/logstore.go` — pubsub subscriber で `task.<id>.log` を disk へ persist
- `server/wal.go` — JSONL append-only state log
- `server/server.go` — public entry (`Server` struct, `Run()`)
- `server/*_test.go`
- `cli/client.go` — server への WS 接続ヘルパ
- `cli/submit.go`, `cli/list.go`, `cli/logs.go`, `cli/cancel.go`, `cli/watch.go`, `cli/prune.go`
- `topics/topics.go` — topic 名生成関数群
- `cmd/harness-cli/main.go` — CLI entry
- `testdata/fake-claude.sh` — fake claude binary for tests
- `integration/e2e_test.go` — server + runner + cli を 1 プロセス内で起動する E2E

### Modify
- `runner/protocol/message.bgn` — typo 修正 (`hertbeat` → `heartbeat`)、`task_started` を RunnerMessage match に追加、`ListResult` に `RunnerInfo` / `TaskInfo` を入れる、pubsub event payload schema を追加
- `runner/protocol/message.go` — 再生成
- `runner/runner.go` — 既存 struct に `LastSeen` 以外の更新メソッドを足す
- `cmd/harness-server/main.go` — `server.Run()` へ委譲する薄い main に差し替え

### Delete (dead code)
- `cmd/harness-client/main.go` — echo sample、CLI に置き換え後不要

---

## Phase 1: Protocol schema の整合

### Task 1.1: bgn schema を v1 仕様に揃える

**Files:**
- Modify: `runner/protocol/message.bgn`

- [ ] **Step 1: 現状の schema を読んで差分を把握**

Run: `cat runner/protocol/message.bgn`

確認する差分:
- typo: `hertbeat` を `heartbeat` に
- `RunnerMessage` match 句に `task_started` が未登録
- `ListResult` が ID のみで状態情報を運べない
- status event 用の format がない

- [ ] **Step 2: schema を書き換える**

`runner/protocol/message.bgn` を以下に差し替える:

```
config.go.package = "protocol"

enum RunnerMessageType:
    :u8
    hello
    task_accepted
    task_started
    task_finished
    heartbeat

enum RunnerRequestType:
    :u8
    assign_task
    cancel_task

format RunnerHello:
    version :u8
    repo_path_len :u16
    repo_path :[repo_path_len]u8

format TaskID:
    id :[16]u8

format AssignTask:
    task_id :TaskID
    prompt :[..]u8

format CancelTask:
    task_id :TaskID

format TaskAccepted:
    task_id :TaskID

format TaskStarted:
    task_id :TaskID
    worktree_dir_len :u16
    worktree_dir :[worktree_dir_len]u8

format TaskFinished:
    task_id :TaskID
    exit_code :i32
    diff_info :[..]u8

format RunnerMessage:
    kind :RunnerMessageType
    match kind:
        RunnerMessageType.hello => hello :RunnerHello
        RunnerMessageType.task_accepted => task_accepted :TaskAccepted
        RunnerMessageType.task_started => task_started :TaskStarted
        RunnerMessageType.task_finished => task_finished :TaskFinished
        RunnerMessageType.heartbeat => ..
        .. => error("Unexpected message")

format RunnerRequest:
    kind :RunnerRequestType
    match kind:
        RunnerRequestType.assign_task => assign_task :AssignTask
        RunnerRequestType.cancel_task => cancel_task :CancelTask

enum TaskControlKind:
    :u8
    submit
    list
    cancel

format ListQuery:
    query :[..]u8 # currently opaque

format RunnerID:
    transport_len :u8
    transport :[transport_len]u8
    ip_addr_len :u8
    ip_addr_len == 4 || ip_addr_len == 16
    ip_addr :[ip_addr_len]u8
    port :u16
    unique_number :u16

enum TaskStatus:
    :u8
    Queued
    Running
    Succeeded
    Failed
    Cancelled

enum RunnerStatus:
    :u8
    Offline
    Idle
    Busy

format RunnerInfo:
    id :RunnerID
    status :RunnerStatus
    repo_path_len :u16
    repo_path :[repo_path_len]u8
    current_task :TaskID
    connected_at :u64  # unix nano
    last_seen :u64

format TaskInfo:
    id :TaskID
    status :TaskStatus
    repo_path_len :u16
    repo_path :[repo_path_len]u8
    assigned_to :RunnerID
    worktree_dir_len :u16
    worktree_dir :[worktree_dir_len]u8
    created_at :u64
    started_at :u64
    ended_at :u64
    exit_code :i32
    prompt_len :u32
    prompt :[prompt_len]u8

format ListResult:
    runners_len :u16
    runners :[runners_len]RunnerInfo
    tasks_len :u16
    tasks :[tasks_len]TaskInfo

format SubmitResponse:
    task_id :TaskID

format TaskControlRequest:
    kind :TaskControlKind
    match kind:
        TaskControlKind.submit => submit :AssignTask
        TaskControlKind.list => list :ListQuery
        TaskControlKind.cancel => cancel :CancelTask
        .. => error("Unexpected task")

format CancelStatus:
    status :u8

format TaskControlResponse:
    kind :TaskControlKind
    match kind:
        TaskControlKind.submit => submit :SubmitResponse
        TaskControlKind.list => list :ListResult
        TaskControlKind.cancel => cancel :CancelStatus

# Pubsub event payloads
enum StatusEventKind:
    :u8
    task_queued
    task_assigned
    task_started
    task_ended
    runner_registered
    runner_offline

format TaskStatusEvent:
    kind :StatusEventKind
    task_id :TaskID
    ts :u64
    task_status :TaskStatus
    exit_code :i32

format RunnerStatusEvent:
    kind :StatusEventKind
    runner_id :RunnerID
    ts :u64
    runner_status :RunnerStatus
```

- [ ] **Step 3: brgen で code 再生成を試す**

Run: プロジェクトで bgn → go 生成に使っている ebm2go / rebrgen を起動 (既存 `.go` ファイル上部の "Code generated by ebm2go at https://github.com/on-keyday/rebrgen" を参照し同じコマンドで)。

Expected: `runner/protocol/message.go` が再生成され、`RunnerMessageType_Heartbeat` 等の改名、`RunnerInfo` / `TaskInfo` / `*StatusEvent` の Encode/Decode が出力される。

brgen が特定機能 (例: nested variable-length struct in array) でエラーを出したら、該当箇所だけ手書き struct + 独自 Encode/Decode に fall back する判断を記録し、spec の §9 に追記する。

- [ ] **Step 4: schema ファイルと生成物を commit**

```bash
git add runner/protocol/message.bgn runner/protocol/message.go
git commit -m "protocol: extend runner/task schema for v1 dispatcher"
```

### Task 1.2: Protocol round-trip テスト

**Files:**
- Create: `runner/protocol/message_test.go`

- [ ] **Step 1: 失敗する round-trip テストを書く**

```go
package protocol

import (
	"bytes"
	"testing"
)

func TestRunnerHelloRoundTrip(t *testing.T) {
	orig := &RunnerHello{
		Version:     1,
		RepoPathLen: 4,
		RepoPath:    []byte("/foo"),
	}
	enc := orig.MustAppend(nil)
	dec := &RunnerHello{}
	if _, err := dec.Decode(enc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if dec.Version != orig.Version || !bytes.Equal(dec.RepoPath, orig.RepoPath) {
		t.Fatalf("mismatch: %+v vs %+v", dec, orig)
	}
}

func TestTaskInfoRoundTrip(t *testing.T) {
	orig := &TaskInfo{
		Id:           TaskID{Id: [16]byte{1, 2, 3}},
		Status:       TaskStatus_Running,
		RepoPathLen:  4,
		RepoPath:     []byte("/bar"),
		WorktreeDirLen: 10,
		WorktreeDir:  []byte("/tmp/wtr/1"),
		PromptLen:    5,
		Prompt:       []byte("hello"),
		CreatedAt:    111,
		StartedAt:    222,
	}
	enc := orig.MustAppend(nil)
	dec := &TaskInfo{}
	if _, err := dec.Decode(enc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !bytes.Equal(dec.Prompt, orig.Prompt) || dec.StartedAt != 222 {
		t.Fatalf("mismatch: %+v", dec)
	}
}
```

- [ ] **Step 2: テスト実行、fail 確認**

Run: `go test ./runner/protocol/... -run TestRunnerHelloRoundTrip -v`
Expected: 生成された field 名と微妙に違う場合 FAIL、一致していれば PASS。FAIL なら実際の生成 field 名に合わせて test を修正。

- [ ] **Step 3: PASS するまで調整して commit**

```bash
git add runner/protocol/message_test.go
git commit -m "protocol: add round-trip tests"
```

### Task 1.3: Topic 名生成ヘルパ

**Files:**
- Create: `topics/topics.go`, `topics/topics_test.go`

- [ ] **Step 1: テスト**

```go
package topics

import "testing"

func TestTaskLog(t *testing.T) {
	got := TaskLog("01HAA")
	if got != "task.01HAA.log" {
		t.Fatalf("got %q", got)
	}
}

func TestTaskStatusAllTopic(t *testing.T) {
	if TasksStatus() != "tasks.status" {
		t.Fatalf("bad: %q", TasksStatus())
	}
}
```

- [ ] **Step 2: 失敗確認**

Run: `go test ./topics/... -v`
Expected: FAIL (package がない)

- [ ] **Step 3: 実装**

```go
package topics

import "fmt"

func RunnersStatus() string       { return "runners.status" }
func TasksStatus() string         { return "tasks.status" }
func TaskLog(taskID string) string    { return fmt.Sprintf("task.%s.log", taskID) }
func TaskStatus(taskID string) string { return fmt.Sprintf("task.%s.status", taskID) }
```

- [ ] **Step 4: PASS & commit**

```bash
go test ./topics/... -v
git add topics/
git commit -m "topics: add topic name helpers"
```

---

## Phase 2: Server core

### Task 2.1: RunnerRegistry

**Files:**
- Create: `server/registry.go`, `server/registry_test.go`

`RunnerRegistry` は runner の状態を保持する concurrency-safe map。server のみが触る。

- [ ] **Step 1: テスト**

```go
package server

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestRegistryAddFindRemove(t *testing.T) {
	r := NewRegistry()
	entry := &RunnerEntry{ID: "A", RepoPath: "/x", Status: protocol.RunnerStatus_Idle, ConnectedAt: time.Now()}
	r.Add(entry)
	got, ok := r.Get("A")
	if !ok || got.RepoPath != "/x" {
		t.Fatalf("get: %+v ok=%v", got, ok)
	}
	r.Remove("A")
	if _, ok := r.Get("A"); ok {
		t.Fatalf("expected removed")
	}
}

func TestRegistryIdleByRepo(t *testing.T) {
	r := NewRegistry()
	r.Add(&RunnerEntry{ID: "A", RepoPath: "/x", Status: protocol.RunnerStatus_Busy, ConnectedAt: time.Unix(1, 0)})
	r.Add(&RunnerEntry{ID: "B", RepoPath: "/x", Status: protocol.RunnerStatus_Idle, ConnectedAt: time.Unix(2, 0)})
	r.Add(&RunnerEntry{ID: "C", RepoPath: "/x", Status: protocol.RunnerStatus_Idle, ConnectedAt: time.Unix(1, 0)})
	got := r.OldestIdleForRepo("/x")
	if got == nil || got.ID != "C" {
		t.Fatalf("want C, got %+v", got)
	}
}
```

- [ ] **Step 2: 失敗確認**

Run: `go test ./server/... -run TestRegistry -v`
Expected: FAIL (パッケージ無し)

- [ ] **Step 3: 実装**

```go
package server

import (
	"sort"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

type RunnerEntry struct {
	ID          string
	RepoPath    string
	Status      protocol.RunnerStatus
	CurrentTask string
	ConnectedAt time.Time
	LastSeen    time.Time
}

type Registry struct {
	mu      sync.Mutex
	runners map[string]*RunnerEntry
}

func NewRegistry() *Registry {
	return &Registry{runners: map[string]*RunnerEntry{}}
}

func (r *Registry) Add(e *RunnerEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runners[e.ID] = e
}

func (r *Registry) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.runners, id)
}

func (r *Registry) Get(id string) (*RunnerEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.runners[id]
	return e, ok
}

func (r *Registry) SetStatus(id string, s protocol.RunnerStatus, currentTask string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.runners[id]; ok {
		e.Status = s
		e.CurrentTask = currentTask
		e.LastSeen = time.Now()
	}
}

func (r *Registry) OldestIdleForRepo(repo string) *RunnerEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	var candidates []*RunnerEntry
	for _, e := range r.runners {
		if e.RepoPath == repo && e.Status == protocol.RunnerStatus_Idle {
			candidates = append(candidates, e)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].ConnectedAt.Before(candidates[j].ConnectedAt)
	})
	return candidates[0]
}

func (r *Registry) List() []*RunnerEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*RunnerEntry, 0, len(r.runners))
	for _, e := range r.runners {
		out = append(out, e)
	}
	return out
}
```

- [ ] **Step 4: PASS & commit**

```bash
go test ./server/... -run TestRegistry -v
git add server/registry.go server/registry_test.go
git commit -m "server: add runner registry"
```

### Task 2.2: TaskStore (in-memory, without WAL)

**Files:**
- Create: `server/taskstore.go`, `server/taskstore_test.go`

ID 発番、queue/running/done の lifecycle、filtering。WAL は Task 2.8 で足す。

- [ ] **Step 1: テスト**

```go
package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestTaskStoreCreate(t *testing.T) {
	s := NewTaskStore()
	id := s.Create("/repo", "prompt")
	if id == "" {
		t.Fatal("want id")
	}
	got, ok := s.Get(id)
	if !ok || got.Status != protocol.TaskStatus_Queued || got.Prompt != "prompt" {
		t.Fatalf("bad: %+v", got)
	}
}

func TestTaskStoreAssignAndFinish(t *testing.T) {
	s := NewTaskStore()
	id := s.Create("/r", "p")
	s.Assign(id, "runner-1", "/tmp/worktree")
	got, _ := s.Get(id)
	if got.Status != protocol.TaskStatus_Running || got.AssignedTo != "runner-1" {
		t.Fatalf("bad: %+v", got)
	}
	s.Finish(id, 0, []byte{})
	got, _ = s.Get(id)
	if got.Status != protocol.TaskStatus_Succeeded {
		t.Fatalf("bad status: %v", got.Status)
	}
}

func TestTaskStoreFinishNonZero(t *testing.T) {
	s := NewTaskStore()
	id := s.Create("/r", "p")
	s.Assign(id, "runner", "/wt")
	s.Finish(id, 7, nil)
	got, _ := s.Get(id)
	if got.Status != protocol.TaskStatus_Failed || got.ExitCode == nil || *got.ExitCode != 7 {
		t.Fatalf("bad: %+v", got)
	}
}

func TestTaskStoreNextQueuedForRepo(t *testing.T) {
	s := NewTaskStore()
	a := s.Create("/x", "a")
	b := s.Create("/x", "b")
	_ = s.Create("/y", "c")
	next := s.NextQueuedForRepo("/x")
	if next == nil || next.ID != a {
		t.Fatalf("want first queued %s, got %+v", a, next)
	}
	s.Assign(a, "r", "/wt")
	next = s.NextQueuedForRepo("/x")
	if next == nil || next.ID != b {
		t.Fatalf("want %s, got %+v", b, next)
	}
}
```

- [ ] **Step 2: 失敗確認**

Run: `go test ./server/... -run TestTaskStore -v`
Expected: FAIL (未実装)

- [ ] **Step 3: 実装**

```go
package server

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

type TaskEntry struct {
	ID          string
	RepoPath    string
	Prompt      string
	Status      protocol.TaskStatus
	AssignedTo  string
	WorktreeDir string
	CreatedAt   time.Time
	StartedAt   *time.Time
	EndedAt     *time.Time
	ExitCode    *int32
	DiffInfo    []byte
}

type TaskStore struct {
	mu    sync.Mutex
	tasks map[string]*TaskEntry
	order []string // insertion order for FIFO
}

func NewTaskStore() *TaskStore {
	return &TaskStore{tasks: map[string]*TaskEntry{}}
}

func newTaskID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (s *TaskStore) Create(repo, prompt string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := newTaskID()
	s.tasks[id] = &TaskEntry{
		ID: id, RepoPath: repo, Prompt: prompt,
		Status: protocol.TaskStatus_Queued, CreatedAt: time.Now(),
	}
	s.order = append(s.order, id)
	return id
}

func (s *TaskStore) Get(id string) (*TaskEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.tasks[id]
	return e, ok
}

func (s *TaskStore) Assign(id, runnerID, worktreeDir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return
	}
	now := time.Now()
	t.Status = protocol.TaskStatus_Running
	t.AssignedTo = runnerID
	t.WorktreeDir = worktreeDir
	t.StartedAt = &now
}

func (s *TaskStore) Finish(id string, exit int32, diff []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return
	}
	now := time.Now()
	t.ExitCode = &exit
	t.DiffInfo = diff
	t.EndedAt = &now
	if exit == 0 {
		t.Status = protocol.TaskStatus_Succeeded
	} else {
		t.Status = protocol.TaskStatus_Failed
	}
}

func (s *TaskStore) Cancel(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.tasks[id]; ok {
		now := time.Now()
		t.Status = protocol.TaskStatus_Cancelled
		t.EndedAt = &now
	}
}

func (s *TaskStore) NextQueuedForRepo(repo string) *TaskEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range s.order {
		t := s.tasks[id]
		if t.Status == protocol.TaskStatus_Queued && t.RepoPath == repo {
			return t
		}
	}
	return nil
}

func (s *TaskStore) List(limit int) []*TaskEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*TaskEntry, 0, len(s.order))
	start := 0
	if limit > 0 && len(s.order) > limit {
		start = len(s.order) - limit
	}
	for _, id := range s.order[start:] {
		out = append(out, s.tasks[id])
	}
	return out
}
```

- [ ] **Step 4: PASS & commit**

```bash
go test ./server/... -run TestTaskStore -v
git add server/taskstore.go server/taskstore_test.go
git commit -m "server: add in-memory task store"
```

### Task 2.3: Scheduler (repo match, notify on new task/runner)

**Files:**
- Create: `server/scheduler.go`, `server/scheduler_test.go`

Scheduler は `Tick()` を呼ばれると 1 pair (task, runner) を assign する純粋関数に近い設計。Notify 起動は dispatch 側から。

- [ ] **Step 1: テスト**

```go
package server

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestSchedulerAssignsOnePair(t *testing.T) {
	reg := NewRegistry()
	reg.Add(&RunnerEntry{ID: "r1", RepoPath: "/x", Status: protocol.RunnerStatus_Idle, ConnectedAt: time.Unix(1, 0)})
	store := NewTaskStore()
	store.Create("/x", "p1")

	var assigned []string
	sch := NewScheduler(reg, store, func(runnerID, taskID string) error {
		assigned = append(assigned, runnerID+":"+taskID)
		return nil
	})
	sch.Tick()

	if len(assigned) != 1 || assigned[0][:3] != "r1:" {
		t.Fatalf("bad: %v", assigned)
	}
	entry, _ := reg.Get("r1")
	if entry.Status != protocol.RunnerStatus_Busy {
		t.Fatalf("runner not Busy: %+v", entry)
	}
}

func TestSchedulerNoMatch(t *testing.T) {
	reg := NewRegistry()
	reg.Add(&RunnerEntry{ID: "r1", RepoPath: "/y", Status: protocol.RunnerStatus_Idle})
	store := NewTaskStore()
	store.Create("/x", "p1")
	sch := NewScheduler(reg, store, func(string, string) error { t.Fatal("should not assign"); return nil })
	sch.Tick()
}
```

- [ ] **Step 2: 失敗確認**

Run: `go test ./server/... -run TestScheduler -v`
Expected: FAIL

- [ ] **Step 3: 実装**

```go
package server

import (
	"log/slog"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

type AssignFunc func(runnerID, taskID string) error

type Scheduler struct {
	reg    *Registry
	store  *TaskStore
	assign AssignFunc
}

func NewScheduler(reg *Registry, store *TaskStore, assign AssignFunc) *Scheduler {
	return &Scheduler{reg: reg, store: store, assign: assign}
}

func (s *Scheduler) Tick() {
	for _, runner := range s.reg.List() {
		if runner.Status != protocol.RunnerStatus_Idle {
			continue
		}
		task := s.store.NextQueuedForRepo(runner.RepoPath)
		if task == nil {
			continue
		}
		if err := s.assign(runner.ID, task.ID); err != nil {
			slog.Error("assign failed", "runner", runner.ID, "task", task.ID, "err", err)
			continue
		}
		s.store.Assign(task.ID, runner.ID, "")
		s.reg.SetStatus(runner.ID, protocol.RunnerStatus_Busy, task.ID)
	}
}
```

- [ ] **Step 4: PASS & commit**

```bash
go test ./server/... -run TestScheduler -v
git add server/scheduler.go server/scheduler_test.go
git commit -m "server: add scheduler with repo-match assignment"
```

### Task 2.4: Wire kind dispatch skeleton

**Files:**
- Modify: `cmd/harness-server/main.go`
- Create: `server/dispatch.go`, `server/dispatch_test.go`

既存 `cmd/harness-server/main.go` の `switch kind` 空 case を `server.Dispatch()` に繋ぐ。

- [ ] **Step 1: テスト (dispatch が kind 毎に handler を呼ぶ)**

```go
package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/trsf/wire"
)

func TestDispatchRoutesByKind(t *testing.T) {
	var called wire.ApplicationPayloadKind
	d := &Dispatcher{
		OnRunnerControl: func(conn ConnHandle, payload []byte) { called = wire.ApplicationPayloadKind_RunnerControl },
		OnTaskControl:   func(conn ConnHandle, payload []byte) { called = wire.ApplicationPayloadKind_TaskControl },
	}
	d.Dispatch(nil, []byte{byte(wire.ApplicationPayloadKind_RunnerControl), 0x00})
	if called != wire.ApplicationPayloadKind_RunnerControl {
		t.Fatalf("bad: %v", called)
	}
	d.Dispatch(nil, []byte{byte(wire.ApplicationPayloadKind_TaskControl), 0x00})
	if called != wire.ApplicationPayloadKind_TaskControl {
		t.Fatalf("bad: %v", called)
	}
}
```

- [ ] **Step 2: 失敗確認**

Run: `go test ./server/... -run TestDispatch -v`
Expected: FAIL

- [ ] **Step 3: 実装**

```go
package server

import (
	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// ConnHandle は send 能力だけ必要 (テストで mock 可能に)
type ConnHandle interface {
	ConnectionID() objproto.ConnectionID
	SendMessage(b []byte) (int, uint64, error)
}

type Dispatcher struct {
	OnRunnerControl func(ConnHandle, []byte)
	OnTaskControl   func(ConnHandle, []byte)
}

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
```

- [ ] **Step 4: PASS & commit**

```bash
go test ./server/... -run TestDispatch -v
git add server/dispatch.go server/dispatch_test.go
git commit -m "server: add wire-kind dispatcher"
```

### Task 2.5: RunnerControl handler (Hello / TaskAccepted / TaskStarted / TaskFinished / Heartbeat)

**Files:**
- Create: `server/runner_handler.go`, `server/runner_handler_test.go`

Runner → Server の RunnerMessage を registry / taskstore に反映。

- [ ] **Step 1: テスト**

```go
package server

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

type fakeConn struct {
	id   objproto.ConnectionID
	sent [][]byte
}

func (f *fakeConn) ConnectionID() objproto.ConnectionID { return f.id }
func (f *fakeConn) SendMessage(b []byte) (int, uint64, error) {
	f.sent = append(f.sent, append([]byte{}, b...))
	return len(b), 0, nil
}

func TestHelloRegistersRunner(t *testing.T) {
	reg := NewRegistry()
	store := NewTaskStore()
	h := &RunnerHandler{Registry: reg, Tasks: store, Now: func() time.Time { return time.Unix(10, 0) }}

	fc := &fakeConn{id: mustCID(t, "ws:127.0.0.1:8539-1")}
	hello := &protocol.RunnerMessage{
		Kind:  protocol.RunnerMessageType_Hello,
		Hello: &protocol.RunnerHello{Version: 1, RepoPathLen: 4, RepoPath: []byte("/foo")},
	}
	h.Handle(fc, mustEncodeRunnerMsg(t, hello))

	entry, ok := reg.Get(fc.id.String())
	if !ok || entry.RepoPath != "/foo" || entry.Status != protocol.RunnerStatus_Idle {
		t.Fatalf("bad entry: %+v (ok=%v)", entry, ok)
	}
}

func TestTaskFinishedUpdatesStore(t *testing.T) {
	reg := NewRegistry()
	store := NewTaskStore()
	h := &RunnerHandler{Registry: reg, Tasks: store, Now: time.Now}

	fc := &fakeConn{id: mustCID(t, "ws:127.0.0.1:8539-2")}
	reg.Add(&RunnerEntry{ID: fc.id.String(), RepoPath: "/r", Status: protocol.RunnerStatus_Busy})
	taskID := store.Create("/r", "p")
	store.Assign(taskID, fc.id.String(), "/wt")

	tidBytes := protocol.TaskID{}
	copy(tidBytes.Id[:], decodeHex(t, taskID))
	finished := &protocol.RunnerMessage{
		Kind: protocol.RunnerMessageType_TaskFinished,
		TaskFinished: &protocol.TaskFinished{
			TaskId: tidBytes, ExitCode: 0, DiffInfo: nil,
		},
	}
	h.Handle(fc, mustEncodeRunnerMsg(t, finished))

	got, _ := store.Get(taskID)
	if got.Status != protocol.TaskStatus_Succeeded {
		t.Fatalf("bad: %v", got.Status)
	}
	re, _ := reg.Get(fc.id.String())
	if re.Status != protocol.RunnerStatus_Idle {
		t.Fatalf("runner not idle: %+v", re)
	}
}

// test helpers (mustCID, mustEncodeRunnerMsg, decodeHex) は同 file 内に追加
```

- [ ] **Step 2: 失敗確認**

Run: `go test ./server/... -run TestHello -v`
Expected: FAIL

- [ ] **Step 3: 実装**

```go
package server

import (
	"encoding/hex"
	"log/slog"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

type RunnerHandler struct {
	Registry *Registry
	Tasks    *TaskStore
	Now      func() time.Time
	// OnChange is called after any state mutation (triggers scheduler tick)
	OnChange func()
}

func (h *RunnerHandler) Handle(conn ConnHandle, payload []byte) {
	msg := &protocol.RunnerMessage{}
	if _, err := msg.Decode(payload); err != nil {
		slog.Error("runner msg decode", "err", err)
		return
	}
	id := conn.ConnectionID().String()
	switch msg.Kind {
	case protocol.RunnerMessageType_Hello:
		h.Registry.Add(&RunnerEntry{
			ID:          id,
			RepoPath:    string(msg.Hello.RepoPath),
			Status:      protocol.RunnerStatus_Idle,
			ConnectedAt: h.Now(),
			LastSeen:    h.Now(),
		})
	case protocol.RunnerMessageType_TaskAccepted:
		// scheduler は既に Assign 済みなので特別処理不要。LastSeen だけ更新。
		if e, ok := h.Registry.Get(id); ok {
			e.LastSeen = h.Now()
		}
	case protocol.RunnerMessageType_TaskStarted:
		taskID := hex.EncodeToString(msg.TaskStarted.TaskId.Id[:])
		if t, ok := h.Tasks.Get(taskID); ok {
			t.WorktreeDir = string(msg.TaskStarted.WorktreeDir)
		}
	case protocol.RunnerMessageType_TaskFinished:
		taskID := hex.EncodeToString(msg.TaskFinished.TaskId.Id[:])
		h.Tasks.Finish(taskID, msg.TaskFinished.ExitCode, msg.TaskFinished.DiffInfo)
		h.Registry.SetStatus(id, protocol.RunnerStatus_Idle, "")
	case protocol.RunnerMessageType_Heartbeat:
		if e, ok := h.Registry.Get(id); ok {
			e.LastSeen = h.Now()
		}
	}
	if h.OnChange != nil {
		h.OnChange()
	}
}
```

(test helpers は test file に別途書く: `mustCID` は `objproto.MustParseConnectionID`、`decodeHex` は `hex.DecodeString`)

- [ ] **Step 4: PASS & commit**

```bash
go test ./server/... -run TestHello -v
git add server/runner_handler.go server/runner_handler_test.go
git commit -m "server: handle runner-control messages"
```

### Task 2.6: TaskControl handler (Submit / List / Cancel)

**Files:**
- Create: `server/task_handler.go`, `server/task_handler_test.go`

- [ ] **Step 1: テスト**

```go
package server

import (
	"bytes"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestSubmitCreatesTask(t *testing.T) {
	store := NewTaskStore()
	reg := NewRegistry()
	h := &TaskHandler{Tasks: store, Registry: reg, OnChange: func() {}}

	fc := &fakeConn{id: mustCID(t, "ws:127.0.0.1:8539-3")}
	req := &protocol.TaskControlRequest{
		Kind: protocol.TaskControlKind_Submit,
		Submit: &protocol.AssignTask{
			TaskId: protocol.TaskID{},
			Prompt: []byte("do stuff"),
		},
	}
	h.Handle(fc, mustEncodeTaskReq(t, req, "/repo"))

	if len(fc.sent) != 1 {
		t.Fatalf("want 1 response, got %d", len(fc.sent))
	}
	resp := &protocol.TaskControlResponse{}
	_, err := resp.Decode(fc.sent[0][1:])
	if err != nil || resp.Kind != protocol.TaskControlKind_Submit {
		t.Fatalf("resp: %+v err=%v", resp, err)
	}
}

func TestListReturnsRunnersAndTasks(t *testing.T) {
	store := NewTaskStore()
	reg := NewRegistry()
	reg.Add(&RunnerEntry{ID: "r", RepoPath: "/x", Status: protocol.RunnerStatus_Idle})
	store.Create("/x", "p")
	h := &TaskHandler{Tasks: store, Registry: reg}

	fc := &fakeConn{id: mustCID(t, "ws:127.0.0.1:8539-4")}
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_List, List: &protocol.ListQuery{Query: nil}}
	h.Handle(fc, mustEncodeTaskReq(t, req, ""))

	if !bytes.Contains(bytes.Join(fc.sent, nil), []byte("/x")) {
		t.Fatalf("repo_path not in response")
	}
}
```

**注:** `AssignTask` は `repo_path` を持たないので submit の signature 設計に問題あり。spec §7.3 は `Submit { repo_path, prompt }` だった。Task 1.1 の schema 修正で **`Submit` を `AssignTask` 流用せず専用 format** `SubmitRequest { repo_path, prompt }` を追加すべき。Task 1.1 に戻って schema 修正を追加で入れる。

- [ ] **Step 1.5: Task 1.1 の schema に `SubmitRequest` 追加**

`runner/protocol/message.bgn` の `TaskControlRequest` 付近を修正:

```
format SubmitRequest:
    repo_path_len :u16
    repo_path :[repo_path_len]u8
    prompt_len :u32
    prompt :[prompt_len]u8

format TaskControlRequest:
    kind :TaskControlKind
    match kind:
        TaskControlKind.submit => submit :SubmitRequest
        TaskControlKind.list => list :ListQuery
        TaskControlKind.cancel => cancel :CancelTask
        .. => error("Unexpected task")
```

再生成して commit:

```bash
# regenerate
git add runner/protocol/message.bgn runner/protocol/message.go
git commit -m "protocol: use dedicated SubmitRequest for repo_path+prompt"
```

- [ ] **Step 2: 失敗確認**

Run: `go test ./server/... -run TestSubmit -v`
Expected: FAIL

- [ ] **Step 3: 実装**

```go
package server

import (
	"encoding/hex"
	"log/slog"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

type TaskHandler struct {
	Tasks    *TaskStore
	Registry *Registry
	OnChange func()
}

func (h *TaskHandler) Handle(conn ConnHandle, payload []byte) {
	req := &protocol.TaskControlRequest{}
	if _, err := req.Decode(payload); err != nil {
		slog.Error("task req decode", "err", err)
		return
	}
	resp := &protocol.TaskControlResponse{Kind: req.Kind}
	switch req.Kind {
	case protocol.TaskControlKind_Submit:
		taskID := h.Tasks.Create(string(req.Submit.RepoPath), string(req.Submit.Prompt))
		var tid protocol.TaskID
		raw, _ := hex.DecodeString(taskID)
		copy(tid.Id[:], raw)
		resp.Submit = &protocol.SubmitResponse{TaskId: tid}
		if h.OnChange != nil {
			h.OnChange()
		}
	case protocol.TaskControlKind_List:
		resp.List = h.buildListResult()
	case protocol.TaskControlKind_Cancel:
		taskID := hex.EncodeToString(req.Cancel.TaskId.Id[:])
		h.Tasks.Cancel(taskID)
		resp.Cancel = &protocol.CancelStatus{Status: 0}
		if h.OnChange != nil {
			h.OnChange()
		}
	}
	data := append([]byte{byte(wire.ApplicationPayloadKind_TaskControl)}, resp.MustAppend(nil)...)
	conn.SendMessage(data)
}

func (h *TaskHandler) buildListResult() *protocol.ListResult {
	res := &protocol.ListResult{}
	for _, r := range h.Registry.List() {
		res.Runners = append(res.Runners, toRunnerInfo(r))
		res.RunnersLen++
	}
	for _, t := range h.Tasks.List(100) {
		res.Tasks = append(res.Tasks, toTaskInfo(t))
		res.TasksLen++
	}
	return res
}

func toRunnerInfo(r *RunnerEntry) protocol.RunnerInfo {
	// RunnerID を objproto.ConnectionID から復元 (parse-round-trip)
	// 簡易: transport="ws" 固定、他 field は 0 で済ませる (本実装は objproto 側の分解関数を借りる)
	info := protocol.RunnerInfo{
		Status:      r.Status,
		RepoPathLen: uint16(len(r.RepoPath)),
		RepoPath:    []byte(r.RepoPath),
		ConnectedAt: uint64(r.ConnectedAt.UnixNano()),
		LastSeen:    uint64(r.LastSeen.UnixNano()),
	}
	// CurrentTask を埋める
	if r.CurrentTask != "" {
		raw, _ := hex.DecodeString(r.CurrentTask)
		copy(info.CurrentTask.Id[:], raw)
	}
	return info
}

func toTaskInfo(t *TaskEntry) protocol.TaskInfo {
	var tid protocol.TaskID
	raw, _ := hex.DecodeString(t.ID)
	copy(tid.Id[:], raw)
	info := protocol.TaskInfo{
		Id:             tid,
		Status:         t.Status,
		RepoPathLen:    uint16(len(t.RepoPath)),
		RepoPath:       []byte(t.RepoPath),
		WorktreeDirLen: uint16(len(t.WorktreeDir)),
		WorktreeDir:    []byte(t.WorktreeDir),
		PromptLen:      uint32(len(t.Prompt)),
		Prompt:         []byte(t.Prompt),
		CreatedAt:      uint64(t.CreatedAt.UnixNano()),
	}
	if t.StartedAt != nil {
		info.StartedAt = uint64(t.StartedAt.UnixNano())
	}
	if t.EndedAt != nil {
		info.EndedAt = uint64(t.EndedAt.UnixNano())
	}
	if t.ExitCode != nil {
		info.ExitCode = *t.ExitCode
	}
	return info
}
```

**注:** `RunnerInfo.Id` の完全再構築はこの task では保留。CurrentTask と status さえあれば CLI `ls` は情報足る。完全な objproto.ConnectionID → protocol.RunnerID 変換ヘルパは Task 4.2 の CLI 表示で必要になったら足す。

- [ ] **Step 4: PASS & commit**

```bash
go test ./server/... -run TestSubmit -v
go test ./server/... -run TestList -v
git add server/task_handler.go server/task_handler_test.go
git commit -m "server: handle submit/list/cancel task-control messages"
```

### Task 2.7: Server のメインループ統合

**Files:**
- Create: `server/server.go`
- Modify: `cmd/harness-server/main.go`

既存 `cmd/harness-server/main.go` を、`server.Run(ctx, cfg)` に委譲する薄い main に書き直す。

- [ ] **Step 1: `server/server.go` を書く**

```go
package server

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/pubsub"
	"github.com/on-keyday/agent-harness/transport"
	"github.com/on-keyday/agent-harness/trsf"
)

type Config struct {
	Addr    string // ws://host:port の host:port 部分
	DataDir string
	Logger  *slog.Logger
}

type Server struct {
	cfg       Config
	registry  *Registry
	tasks     *TaskStore
	scheduler *Scheduler
	pubsub    *pubsub.PubSub
	assign    AssignFunc
	// runnerConns: runnerID -> ConnHandle (AssignTask 送信用)
	runnerConns map[string]ConnHandle
}

func Run(ctx context.Context, cfg Config) error {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	sess, err := transport.WebSocketSession(cfg.Logger, cfg.Addr, nil, objproto.SessionModeServer)
	if err != nil {
		return fmt.Errorf("ws session: %w", err)
	}
	go objproto.AutoGarbageCollect(sess, 10*time.Second, 30*time.Second, 1*time.Minute, 5*time.Minute)

	s := &Server{
		cfg:         cfg,
		registry:    NewRegistry(),
		tasks:       NewTaskStore(),
		pubsub:      pubsub.NewPubSub(cfg.Logger),
		runnerConns: map[string]ConnHandle{},
	}
	s.assign = s.sendAssign
	s.scheduler = NewScheduler(s.registry, s.tasks, s.assign)

	runnerHandler := &RunnerHandler{Registry: s.registry, Tasks: s.tasks, Now: time.Now, OnChange: s.scheduler.Tick}
	taskHandler := &TaskHandler{Tasks: s.tasks, Registry: s.registry, OnChange: s.scheduler.Tick}
	dispatcher := &Dispatcher{
		OnRunnerControl: runnerHandler.Handle,
		OnTaskControl:   taskHandler.Handle,
	}

	activeSessChan := sess.GetNewActiveSessionChannel()
	for {
		select {
		case <-ctx.Done():
			return nil
		case session, ok := <-activeSessChan:
			if !ok {
				return nil
			}
			go s.handleConnection(ctx, session, dispatcher)
		}
	}
}

func (s *Server) handleConnection(ctx context.Context, session objproto.Connection, d *Dispatcher) {
	connCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	p := trsf.NewStreams(connCtx, true, trsf.DefaultInitialMTU, trsf.DefaultMaxMTU, session, s.cfg.Logger)
	subscriber := pubsub.NewSubscriber(session.ConnectionID(), p)
	defer subscriber.LeaveAll(s.pubsub)
	// register runner conn handle (may not be runner yet; registered on Hello)
	// TaskControl messages come on same conn so adding key on session done; we keep conn under id in handler instead.
	go trsf.AutoSend(connCtx, p, session, nil)
	trsf.AutoReceive(connCtx, p, session, func(msg *objproto.Message, err error) {
		if err != nil {
			return
		}
		if len(msg.Data) == 0 {
			return
		}
		// Pubsub kind は既存 subscriber 経由
		if wire.ApplicationPayloadKind(msg.Data[0]) == wire.ApplicationPayloadKind_Pubsub {
			if resp := subscriber.HandleMessage(s.pubsub, msg.Data[1:]); resp != nil {
				session.SendMessage(resp)
			}
			return
		}
		d.Dispatch(session, msg.Data)
	})
	// disconnect: mark runner offline
	s.registry.Remove(session.ConnectionID().String())
	s.scheduler.Tick()
}

func (s *Server) sendAssign(runnerID, taskID string) error {
	// runner conn は runnerConns map に Hello 時に登録する必要あり。
	// それを行うのは runner_handler の OnChange 以外で別経路が要るので、
	// 簡単のため: registry に conn pointer を持たせる改修を次 task で入れる。
	return fmt.Errorf("not yet wired: see Task 2.7b")
}
```

**注:** この task は意図的に不完全。`sendAssign` は Task 2.7b で完成させる。まずコンパイル可能な骨組みだけ作り、unit test 済 component を繋ぐ。

- [ ] **Step 2: `cmd/harness-server/main.go` を差し替え**

```go
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"

	"github.com/on-keyday/agent-harness/server"
)

var (
	port    = flag.String("port", "8539", "listen port")
	dataDir = flag.String("data-dir", "./harness-data", "persistent data dir")
)

func main() {
	flag.Parse()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := server.Run(ctx, server.Config{
		Addr:    "localhost:" + *port,
		DataDir: *dataDir,
		Logger:  slog.Default(),
	}); err != nil {
		slog.Error("server exited", "err", err)
		os.Exit(1)
	}
}
```

- [ ] **Step 3: build 通過確認**

Run: `go build ./...`
Expected: PASS (sendAssign は `fmt.Errorf` を返すだけなので build は通る)

- [ ] **Step 4: commit**

```bash
git add server/server.go cmd/harness-server/main.go
git commit -m "server: wire up main loop skeleton"
```

### Task 2.7b: sendAssign 完成 (Registry に conn 参照を持たせる)

**Files:**
- Modify: `server/registry.go`, `server/server.go`, `server/runner_handler.go`

- [ ] **Step 1: Registry に `Conn ConnHandle` field を追加**

`RunnerEntry` に `Conn ConnHandle` を追加、`Add()` の呼び出し側で set。

- [ ] **Step 2: `sendAssign` の実装**

`server/server.go`:

```go
import (
	"encoding/hex"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

func (s *Server) sendAssign(runnerID, taskID string) error {
	entry, ok := s.registry.Get(runnerID)
	if !ok || entry.Conn == nil {
		return fmt.Errorf("runner %s not connected", runnerID)
	}
	task, ok := s.tasks.Get(taskID)
	if !ok {
		return fmt.Errorf("task %s not found", taskID)
	}
	var tid protocol.TaskID
	raw, _ := hex.DecodeString(taskID)
	copy(tid.Id[:], raw)

	req := &protocol.RunnerRequest{
		Kind: protocol.RunnerRequestType_AssignTask,
		AssignTask: &protocol.AssignTask{
			TaskId: tid,
			Prompt: []byte(task.Prompt),
		},
	}
	data := append([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)}, req.MustAppend(nil)...)
	_, _, err := entry.Conn.SendMessage(data)
	return err
}
```

- [ ] **Step 3: RunnerHandler で Hello 時に `Conn` を記録**

`server/runner_handler.go` の Hello case を:

```go
case protocol.RunnerMessageType_Hello:
    h.Registry.Add(&RunnerEntry{
        ID: id, RepoPath: string(msg.Hello.RepoPath),
        Status: protocol.RunnerStatus_Idle,
        ConnectedAt: h.Now(), LastSeen: h.Now(),
        Conn: conn,
    })
```

- [ ] **Step 4: integration 風の test 追加**

`server/server_assign_test.go`:

```go
func TestSendAssignReachesRunner(t *testing.T) {
	fc := &fakeConn{id: mustCID(t, "ws:127.0.0.1:8539-9")}
	reg := NewRegistry()
	tasks := NewTaskStore()
	reg.Add(&RunnerEntry{ID: fc.id.String(), RepoPath: "/r", Status: protocol.RunnerStatus_Idle, Conn: fc})
	taskID := tasks.Create("/r", "prompt-x")
	tasks.Assign(taskID, fc.id.String(), "")

	s := &Server{registry: reg, tasks: tasks}
	if err := s.sendAssign(fc.id.String(), taskID); err != nil {
		t.Fatalf("send: %v", err)
	}
	if len(fc.sent) != 1 {
		t.Fatalf("want 1, got %d", len(fc.sent))
	}
	// Kind byte
	if fc.sent[0][0] != byte(wire.ApplicationPayloadKind_RunnerControl) {
		t.Fatalf("bad kind byte")
	}
}
```

- [ ] **Step 5: PASS & commit**

```bash
go test ./server/... -v
git add server/
git commit -m "server: wire sendAssign through registry conn handle"
```

### Task 2.8: WAL (task state 永続化)

**Files:**
- Create: `server/wal.go`, `server/wal_test.go`
- Modify: `server/taskstore.go`, `server/server.go`

JSONL で append。re-read して in-memory store を復元するのは「v1 は Running → Failed にマークして drop」で良いので、format は "state 遷移の event log" と割り切る。

- [ ] **Step 1: テスト (append と read-back)**

```go
package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWALAppendAndReadBack(t *testing.T) {
	dir := t.TempDir()
	w, err := OpenWAL(filepath.Join(dir, "events.log"))
	if err != nil {
		t.Fatal(err)
	}
	w.Write(WALEvent{Type: "task_created", TaskID: "abc", RepoPath: "/r", Prompt: "p"})
	w.Write(WALEvent{Type: "task_assigned", TaskID: "abc", RunnerID: "r1", WorktreeDir: "/wt"})
	w.Close()

	events, err := ReadWAL(filepath.Join(dir, "events.log"))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].TaskID != "abc" || events[1].WorktreeDir != "/wt" {
		t.Fatalf("bad: %+v", events)
	}

	_ = os.Stat
}
```

- [ ] **Step 2: 失敗確認**

Run: `go test ./server/... -run TestWAL -v`
Expected: FAIL

- [ ] **Step 3: 実装**

```go
package server

import (
	"bufio"
	"encoding/json"
	"os"
	"sync"
)

type WALEvent struct {
	Type        string `json:"type"`
	TaskID      string `json:"task_id,omitempty"`
	RunnerID    string `json:"runner_id,omitempty"`
	RepoPath    string `json:"repo_path,omitempty"`
	Prompt      string `json:"prompt,omitempty"`
	WorktreeDir string `json:"worktree_dir,omitempty"`
	ExitCode    *int32 `json:"exit_code,omitempty"`
	Ts          int64  `json:"ts"`
}

type WAL struct {
	mu sync.Mutex
	f  *os.File
	w  *bufio.Writer
}

func OpenWAL(path string) (*WAL, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &WAL{f: f, w: bufio.NewWriter(f)}, nil
}

func (w *WAL) Write(ev WALEvent) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	if _, err := w.w.Write(b); err != nil {
		return err
	}
	if err := w.w.WriteByte('\n'); err != nil {
		return err
	}
	return w.w.Flush()
}

func (w *WAL) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.w.Flush()
	return w.f.Close()
}

func ReadWAL(path string) ([]WALEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []WALEvent
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1<<16), 1<<20)
	for s.Scan() {
		var ev WALEvent
		if err := json.Unmarshal(s.Bytes(), &ev); err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, s.Err()
}
```

- [ ] **Step 4: TaskStore に WAL hook を追加**

`TaskStore` に `wal *WAL` field を持たせ、`Create` / `Assign` / `Finish` / `Cancel` 内で `w.Write(...)` を呼ぶ。nil なら無視する安全設計に。

- [ ] **Step 5: `server.Run` で WAL をオープン & replay**

```go
// in Run()
walPath := filepath.Join(cfg.DataDir, "events.log")
_ = os.MkdirAll(cfg.DataDir, 0o755)
events, _ := ReadWAL(walPath)
// replay: Queued -> Queued, Running -> Failed("server restart")
s.tasks.ReplayEvents(events)
wal, _ := OpenWAL(walPath)
s.tasks.wal = wal
defer wal.Close()
```

`ReplayEvents` は TaskStore に追加:

```go
func (s *TaskStore) ReplayEvents(events []WALEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, ev := range events {
		switch ev.Type {
		case "task_created":
			s.tasks[ev.TaskID] = &TaskEntry{
				ID: ev.TaskID, RepoPath: ev.RepoPath, Prompt: ev.Prompt,
				Status: protocol.TaskStatus_Queued,
			}
			s.order = append(s.order, ev.TaskID)
		case "task_assigned":
			if t, ok := s.tasks[ev.TaskID]; ok {
				t.Status = protocol.TaskStatus_Running
				t.AssignedTo = ev.RunnerID
				t.WorktreeDir = ev.WorktreeDir
			}
		case "task_finished":
			if t, ok := s.tasks[ev.TaskID]; ok {
				t.Status = protocol.TaskStatus_Succeeded
				if ev.ExitCode != nil {
					if *ev.ExitCode != 0 {
						t.Status = protocol.TaskStatus_Failed
					}
					t.ExitCode = ev.ExitCode
				}
			}
		}
	}
	// Running のまま残ったものは Failed にする (server restart 耐性)
	for _, t := range s.tasks {
		if t.Status == protocol.TaskStatus_Running {
			t.Status = protocol.TaskStatus_Failed
		}
	}
}
```

- [ ] **Step 6: test PASS & commit**

```bash
go test ./server/... -v
git add server/
git commit -m "server: add WAL for task state persistence"
```

### Task 2.9: LogStore (pubsub subscriber で task log を disk に append)

**Files:**
- Create: `server/logstore.go`, `server/logstore_test.go`
- Modify: `server/server.go`

Server は自らが `task.<id>.log` topic の subscriber になり、publish されるたびに `<data_dir>/logs/<id>.log` に append する。

- [ ] **Step 1: テスト (単純な file append 動作のみ、pubsub 経由は integration test 任せ)**

```go
package server

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLogStoreAppend(t *testing.T) {
	dir := t.TempDir()
	ls := NewLogStore(dir)
	if err := ls.Append("abc", []byte("hello\n")); err != nil {
		t.Fatal(err)
	}
	ls.Append("abc", []byte("world\n"))
	got, _ := os.ReadFile(filepath.Join(dir, "abc.log"))
	if string(got) != "hello\nworld\n" {
		t.Fatalf("got %q", got)
	}
}
```

- [ ] **Step 2: 失敗確認**

Run: `go test ./server/... -run TestLogStore -v`
Expected: FAIL

- [ ] **Step 3: 実装**

```go
package server

import (
	"os"
	"path/filepath"
	"sync"
)

type LogStore struct {
	mu    sync.Mutex
	dir   string
	files map[string]*os.File
}

func NewLogStore(dir string) *LogStore {
	_ = os.MkdirAll(dir, 0o755)
	return &LogStore{dir: dir, files: map[string]*os.File{}}
}

func (l *LogStore) Append(taskID string, data []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	f, ok := l.files[taskID]
	if !ok {
		var err error
		f, err = os.OpenFile(filepath.Join(l.dir, taskID+".log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		l.files[taskID] = f
	}
	_, err := f.Write(data)
	return err
}

func (l *LogStore) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, f := range l.files {
		f.Close()
	}
}
```

- [ ] **Step 4: server.Run で pubsub への subscribe 起動**

`task.<id>.log` への server-side subscribe は現在 pubsub 実装が「外部 subscriber のみ」を想定している可能性あり。既存 `pubsub/pubsub.go` を読み直し、内部 publish-tap が難しければ Task 2.9 後半として **runner 側で `task.<id>.log` にだけ publish → server は同じ topic を `NewSubscriber` で join しない**、別経路として **`RunnerControl` に `LogChunk` message を足して runner → server 直送** に切り替える選択肢もある。

judgment: pubsub 側に internal tap を生やす方が綺麗。Task 2.9b で取り組む。

- [ ] **Step 5: 単体テスト PASS & commit**

```bash
go test ./server/... -run TestLogStore -v
git add server/logstore.go server/logstore_test.go
git commit -m "server: add log store for per-task append"
```

### Task 2.9b: pubsub に internal tap を足す

**Files:**
- Modify: `pubsub/pubsub.go`
- Create: `pubsub/pubsub_tap_test.go`

- [ ] **Step 1: 現 pubsub.go を読み、`Publish` を内部から呼べる API を確認**

既に `(ps *PubSub) Publish(nickName, topic, msg)` は public。`TapSubscribe(topic string, cb func([]byte))` 的なメソッドを追加し、外部 Subscriber 無しで fanout を受け取れる抜き口を提供する。

- [ ] **Step 2: Tap 実装**

```go
// add to pubsub.go

type Tap struct {
	cb func(nickName string, msg []byte)
}

type PubSub struct {
	// existing fields...
	taps map[string][]*Tap
}

func (ps *PubSub) TapSubscribe(topic string, cb func(nickName string, msg []byte)) *Tap {
	ps.m.Lock()
	defer ps.m.Unlock()
	if ps.taps == nil {
		ps.taps = map[string][]*Tap{}
	}
	t := &Tap{cb: cb}
	ps.taps[topic] = append(ps.taps[topic], t)
	return t
}

func (ps *PubSub) TapUnsubscribe(topic string, t *Tap) {
	ps.m.Lock()
	defer ps.m.Unlock()
	arr := ps.taps[topic]
	for i, x := range arr {
		if x == t {
			ps.taps[topic] = append(arr[:i], arr[i+1:]...)
			return
		}
	}
}

// modify Publish() to also call taps
func (ps *PubSub) Publish(nickName string, topic string, msg []byte) {
	ps.m.Lock()
	// (fanout to subscriber streams - existing code)
	taps := ps.taps[topic]
	ps.m.Unlock()
	for _, t := range taps {
		t.cb(nickName, msg)
	}
}
```

- [ ] **Step 3: テスト**

```go
package pubsub

import (
	"log/slog"
	"sync"
	"testing"
)

func TestTapReceivesPublish(t *testing.T) {
	ps := NewPubSub(slog.Default())
	var mu sync.Mutex
	var got [][]byte
	ps.TapSubscribe("t", func(_ string, msg []byte) {
		mu.Lock()
		got = append(got, append([]byte{}, msg...))
		mu.Unlock()
	})
	ps.Publish("nick", "t", []byte("hi"))
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || string(got[0]) != "hi" {
		t.Fatalf("bad: %v", got)
	}
}
```

- [ ] **Step 4: server.Run で tap 起動**

```go
// in Run()
logStore := NewLogStore(filepath.Join(cfg.DataDir, "logs"))
// TapSubscribe is per-task-id; simpler: tap all topics via naming convention at publish time.
// v1: runner側で "task.<id>.log" にpublish する際、server は taskstore.Create 時に
// TapSubscribe(topics.TaskLog(id), logStore.Append(id, msg)) を登録する
```

TaskStore.Create を呼んだ時点で対応 tap を張るように Server 側で hook:

```go
// server.go
oldOnCreate := func(id string) {
    s.pubsub.TapSubscribe(topics.TaskLog(id), func(_ string, msg []byte) {
        _ = logStore.Append(id, msg)
    })
}
```

(TaskStore に `OnCreate func(id string)` callback field を追加し、Create 内で呼ぶ)

- [ ] **Step 5: 既存 pubsub テスト green 確認、commit**

```bash
go test ./pubsub/... -v
go test ./server/... -v
git add pubsub/pubsub.go pubsub/pubsub_tap_test.go server/
git commit -m "pubsub: add internal tap; server: persist task logs via tap"
```

---

## Phase 3: Runner

### Task 3.1: Worktree 管理

**Files:**
- Create: `runner/worktree.go`, `runner/worktree_test.go`

- [ ] **Step 1: テスト (実 git repo を tempdir で構築して worktree 操作)**

```go
package runner

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README")
	run("commit", "-m", "init")
	return dir
}

func TestCreateWorktree(t *testing.T) {
	repo := initRepo(t)
	wm := &WorktreeManager{Repo: repo}
	dir, err := wm.Create("abc123")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("worktree dir missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "README")); err != nil {
		t.Fatalf("README missing: %v", err)
	}

	// Remove
	if err := wm.Remove("abc123"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("worktree not removed")
	}
}
```

- [ ] **Step 2: 失敗確認**

Run: `go test ./runner/... -run TestCreateWorktree -v`
Expected: FAIL

- [ ] **Step 3: 実装**

```go
package runner

import (
	"fmt"
	"os/exec"
	"path/filepath"
)

type WorktreeManager struct {
	Repo string // abs path to the main repo
}

func (wm *WorktreeManager) Create(taskID string) (string, error) {
	dir := filepath.Join(wm.Repo, ".harness-worktrees", taskID)
	branch := "harness/" + taskID
	cmd := exec.Command("git", "worktree", "add", "-b", branch, dir)
	cmd.Dir = wm.Repo
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("worktree add: %w\n%s", err, out)
	}
	return dir, nil
}

func (wm *WorktreeManager) Remove(taskID string) error {
	dir := filepath.Join(wm.Repo, ".harness-worktrees", taskID)
	cmd := exec.Command("git", "worktree", "remove", "--force", dir)
	cmd.Dir = wm.Repo
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("worktree remove: %w\n%s", err, out)
	}
	return nil
}
```

- [ ] **Step 4: PASS & commit**

```bash
go test ./runner/... -run TestCreateWorktree -v
git add runner/worktree.go runner/worktree_test.go
git commit -m "runner: add git worktree manager"
```

### Task 3.2: Claude process exec + log streaming

**Files:**
- Create: `runner/process.go`, `runner/process_test.go`, `testdata/fake-claude.sh`

- [ ] **Step 1: fake-claude.sh**

```bash
#!/bin/bash
# testdata/fake-claude.sh
# Prints stdin prompt echo, writes "hello" file, writes stderr, exits.
set -e
echo "stdout: prompt=$*"
echo "stderr line" >&2
touch hello.txt
exit 0
```

```bash
chmod +x testdata/fake-claude.sh
```

- [ ] **Step 2: テスト**

```go
package runner

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRunClaudeStreamsLogs(t *testing.T) {
	repo := initRepo(t)
	wm := &WorktreeManager{Repo: repo}
	dir, _ := wm.Create("t1")

	var mu sync.Mutex
	var chunks [][]byte
	sink := func(data []byte) { mu.Lock(); chunks = append(chunks, append([]byte{}, data...)); mu.Unlock() }

	p := &Process{
		ClaudeBin: "../testdata/fake-claude.sh",
		CWD:       dir,
		Timeout:   5 * time.Second,
	}
	exit, err := p.Run(context.Background(), "hello", sink)
	if err != nil {
		t.Fatal(err)
	}
	if exit != 0 {
		t.Fatalf("exit=%d", exit)
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(toStrs(chunks), "")
	if !strings.Contains(joined, "[out]") || !strings.Contains(joined, "[err]") {
		t.Fatalf("prefix missing: %q", joined)
	}
	if !strings.Contains(joined, "stdout: prompt=hello") || !strings.Contains(joined, "stderr line") {
		t.Fatalf("content missing: %q", joined)
	}
}

func toStrs(b [][]byte) []string {
	r := make([]string, len(b))
	for i, x := range b {
		r[i] = string(x)
	}
	return r
}
```

- [ ] **Step 3: 失敗確認**

Run: `go test ./runner/... -run TestRunClaude -v`
Expected: FAIL

- [ ] **Step 4: 実装**

```go
package runner

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

type Process struct {
	ClaudeBin string
	CWD       string
	Timeout   time.Duration
}

type LogSink func(data []byte)

func (p *Process) Run(ctx context.Context, prompt string, sink LogSink) (int, error) {
	timeout := p.Timeout
	if timeout == 0 {
		timeout = 30 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, p.ClaudeBin, "-p", prompt)
	cmd.Dir = p.CWD

	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		return -1, fmt.Errorf("start: %w", err)
	}

	var wg sync.WaitGroup
	scan := func(r io.Reader, prefix []byte) {
		defer wg.Done()
		br := bufio.NewReader(r)
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				out := append([]byte{}, prefix...)
				out = append(out, line...)
				sink(out)
			}
			if err != nil {
				return
			}
		}
	}
	wg.Add(2)
	go scan(stdout, []byte("[out]"))
	go scan(stderr, []byte("[err]"))

	err := cmd.Wait()
	wg.Wait()
	exit := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		} else {
			exit = -1
		}
	}
	return exit, nil
}
```

- [ ] **Step 5: PASS & commit**

```bash
go test ./runner/... -run TestRunClaude -v
git add runner/process.go runner/process_test.go testdata/fake-claude.sh
git commit -m "runner: add claude process exec with log streaming"
```

### Task 3.3: Runner のメインループ (Hello / AssignTask / TaskFinished)

**Files:**
- Create: `runner/session.go`, `runner/session_test.go`

`session.go` は WS 接続 → Hello → loop で AssignTask を受け、per-task goroutine で worktree + process を回す。

- [ ] **Step 1: Unit test (mock connection で message 往復)**

Runner のメインループは I/O 結合が強いので、unit test は「`handleAssign(conn, req)` が worktree 作成 + process 起動 + TaskStarted / TaskFinished 送信まで到達する」に絞る。

```go
package runner

import (
	"context"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

type mockSender struct {
	sent [][]byte
}

func (m *mockSender) Send(data []byte) error {
	m.sent = append(m.sent, append([]byte{}, data...))
	return nil
}
func (m *mockSender) ID() objproto.ConnectionID { return objproto.ConnectionID{} }
func (m *mockSender) Publish(topic string, data []byte) error {
	return nil
}

func TestHandleAssignRunsFakeClaudeAndReportsFinished(t *testing.T) {
	repo := initRepo(t)
	ms := &mockSender{}
	s := &Session{
		RepoPath:  repo,
		ClaudeBin: "../testdata/fake-claude.sh",
		Timeout:   5 * time.Second,
		Sender:    ms,
		Now:       time.Now,
	}
	req := &protocol.AssignTask{
		TaskId: protocol.TaskID{Id: [16]byte{1}},
		Prompt: []byte("hello"),
	}
	s.handleAssign(context.Background(), req)

	// 最後の Runner message は TaskFinished
	if len(ms.sent) < 2 {
		t.Fatalf("expected at least 2 (started+finished), got %d", len(ms.sent))
	}
	last := ms.sent[len(ms.sent)-1]
	msg := &protocol.RunnerMessage{}
	_, err := msg.Decode(last[1:])
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if msg.Kind != protocol.RunnerMessageType_TaskFinished {
		t.Fatalf("last kind: %v", msg.Kind)
	}
}
```

- [ ] **Step 2: 失敗確認**

Run: `go test ./runner/... -run TestHandleAssign -v`
Expected: FAIL

- [ ] **Step 3: 実装**

```go
package runner

import (
	"context"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/topics"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

type Sender interface {
	Send(data []byte) error
	ID() objproto.ConnectionID
	Publish(topic string, data []byte) error
}

type Session struct {
	RepoPath  string
	ClaudeBin string
	Timeout   time.Duration
	Sender    Sender
	Now       func() time.Time
	wm        *WorktreeManager
}

func (s *Session) sendRunnerMsg(m *protocol.RunnerMessage) error {
	data := append([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)}, m.MustAppend(nil)...)
	return s.Sender.Send(data)
}

func (s *Session) handleAssign(ctx context.Context, req *protocol.AssignTask) {
	if s.wm == nil {
		s.wm = &WorktreeManager{Repo: s.RepoPath}
	}
	taskIDHex := hex(req.TaskId.Id[:])

	// 1. Accept
	s.sendRunnerMsg(&protocol.RunnerMessage{
		Kind:         protocol.RunnerMessageType_TaskAccepted,
		TaskAccepted: &protocol.TaskAccepted{TaskId: req.TaskId},
	})

	// 2. Worktree
	dir, err := s.wm.Create(taskIDHex)
	if err != nil {
		s.sendRunnerMsg(&protocol.RunnerMessage{
			Kind: protocol.RunnerMessageType_TaskFinished,
			TaskFinished: &protocol.TaskFinished{
				TaskId: req.TaskId, ExitCode: -1, DiffInfo: []byte("worktree_error:" + err.Error()),
			},
		})
		return
	}

	// 3. Started
	s.sendRunnerMsg(&protocol.RunnerMessage{
		Kind: protocol.RunnerMessageType_TaskStarted,
		TaskStarted: &protocol.TaskStarted{
			TaskId: req.TaskId,
			WorktreeDirLen: uint16(len(dir)),
			WorktreeDir: []byte(dir),
		},
	})

	// 4. pubsub JOIN task.<id>.log は現実装では外部 subscriber として JOIN message を送る必要あり
	//    v1 単純化: runner から直接 Publish を呼ぶ Sender.Publish 抽象を用意 (server 側 tap が拾う)
	topic := topics.TaskLog(taskIDHex)

	// 5. Exec
	p := &Process{ClaudeBin: s.ClaudeBin, CWD: dir, Timeout: s.Timeout}
	exit, err := p.Run(ctx, string(req.Prompt), func(data []byte) {
		_ = s.Sender.Publish(topic, data)
	})
	_ = err // Process.Run 内でも exit に吸収される

	// 6. Finished
	s.sendRunnerMsg(&protocol.RunnerMessage{
		Kind: protocol.RunnerMessageType_TaskFinished,
		TaskFinished: &protocol.TaskFinished{
			TaskId: req.TaskId, ExitCode: int32(exit), DiffInfo: nil,
		},
	})
}

func hex(b []byte) string {
	const hextab = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hextab[v>>4]
		out[i*2+1] = hextab[v&0xf]
	}
	return string(out)
}
```

- [ ] **Step 4: PASS & commit**

```bash
go test ./runner/... -run TestHandleAssign -v
git add runner/session.go runner/session_test.go
git commit -m "runner: implement assign-task handler"
```

### Task 3.4: Runner のフル接続ループ + cmd entry

**Files:**
- Create: `runner/connect.go`, `cmd/agent-runner/main.go`

- [ ] **Step 1: `runner/connect.go` — WS 接続 / Hello / receive loop**

```go
package runner

import (
	"context"
	"crypto/ecdh"
	"log/slog"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/pubsub"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/transport"
	"github.com/on-keyday/agent-harness/trsf"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

type Config struct {
	ServerAddr string // e.g. "localhost:8539"
	RepoPath   string
	ClaudeBin  string
	Logger     *slog.Logger
}

func Run(ctx context.Context, cfg Config) error {
	sess, err := transport.WebSocketSession(cfg.Logger, cfg.ServerAddr, nil, objproto.SessionModeClient)
	if err != nil {
		return err
	}
	conn, err := objproto.DoECDHHandshake(ctx, sess,
		objproto.MustParseConnectionID(cfg.ServerAddr+"-1111"),
		ecdh.P521(), objproto.AES128GCM)
	if err != nil {
		return err
	}

	p := trsf.NewStreams(ctx, false, trsf.DefaultInitialMTU, trsf.DefaultMaxMTU, conn, cfg.Logger)
	go trsf.AutoSend(ctx, p, conn, nil)

	// Hello
	hello := &protocol.RunnerMessage{
		Kind: protocol.RunnerMessageType_Hello,
		Hello: &protocol.RunnerHello{
			Version: 1,
			RepoPathLen: uint16(len(cfg.RepoPath)),
			RepoPath: []byte(cfg.RepoPath),
		},
	}
	if _, _, err := conn.SendMessage(append([]byte{byte(wire.ApplicationPayloadKind_RunnerControl)}, hello.MustAppend(nil)...)); err != nil {
		return err
	}

	sender := &connSender{conn: conn, id: conn.ConnectionID(), pubsubProtoSender: subscribeToLogTopic(conn, p)}
	session := &Session{
		RepoPath: cfg.RepoPath, ClaudeBin: cfg.ClaudeBin,
		Sender: sender,
	}

	trsf.AutoReceive(ctx, p, conn, func(msg *objproto.Message, err error) {
		if err != nil || len(msg.Data) == 0 {
			return
		}
		if wire.ApplicationPayloadKind(msg.Data[0]) == wire.ApplicationPayloadKind_RunnerControl {
			req := &protocol.RunnerRequest{}
			if _, derr := req.Decode(msg.Data[1:]); derr != nil {
				return
			}
			if req.Kind == protocol.RunnerRequestType_AssignTask {
				go session.handleAssign(ctx, req.AssignTask)
			}
			// cancel は v1 では runner 側未実装
		}
	})
	return nil
}

type connSender struct {
	conn               objproto.Connection
	id                 objproto.ConnectionID
	pubsubProtoSender  func(topic string, data []byte) error
}

func (c *connSender) Send(data []byte) error {
	_, _, err := c.conn.SendMessage(data)
	return err
}
func (c *connSender) ID() objproto.ConnectionID { return c.id }
func (c *connSender) Publish(topic string, data []byte) error {
	return c.pubsubProtoSender(topic, data)
}

// subscribeToLogTopic は runner が pubsub JOIN し、戻ってきた stream に publish データを書くための
// ヘルパを作って返す。publish 送信先は「この runner 自身が所有する topic stream」で、
// server 側 broker が全 subscriber (server tap を含む) に fanout する。
func subscribeToLogTopic(conn objproto.Connection, p trsf.Transport) func(topic string, data []byte) error {
	joined := map[string]trsf.BidirectionalStream{}
	return func(topic string, data []byte) error {
		st, ok := joined[topic]
		if !ok {
			// JOIN
			joinMsg := pubsub.JoinTopic("runner", topic)
			if _, _, err := conn.SendMessage(joinMsg); err != nil {
				return err
			}
			// 生成される stream ID を受け取るには broker の応答を待つ必要あり。
			// v1 簡易: JOIN 直後に sleep せず stream を新規作成して放り込む方式は使わず、
			// 代わりに runner 側で CreateBidirectionalStream() して topic 名を prefix として先頭書き込みする。
			newSt := p.CreateBidirectionalStream()
			newSt.AppendData(false, []byte(topic), []byte("\n"))
			joined[topic] = newSt
			st = newSt
		}
		return st.AppendData(false, data)
	}
}
```

**注:** `subscribeToLogTopic` は pubsub の既存 API 前提で簡略化。JOIN 応答を待って stream_id を使う正確な実装は `pubsub/pubsub.go` の既存パターンを trace して Task 3.4b で精度上げる。まず fake claude で E2E が動くところまで持っていく。

- [ ] **Step 2: `cmd/agent-runner/main.go`**

```go
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"

	"github.com/on-keyday/agent-harness/runner"
)

var (
	server    = flag.String("server", "localhost:8539", "server host:port")
	repo      = flag.String("repo", ".", "absolute path to repo")
	claudeBin = flag.String("claude-bin", "claude", "path to claude binary")
)

func main() {
	flag.Parse()
	abs, _ := os.Getwd()
	if *repo != "" {
		abs = *repo
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	if err := runner.Run(ctx, runner.Config{
		ServerAddr: *server, RepoPath: abs, ClaudeBin: *claudeBin, Logger: slog.Default(),
	}); err != nil {
		slog.Error("runner exit", "err", err)
		os.Exit(1)
	}
}
```

- [ ] **Step 3: build 通る事を確認**

Run: `go build ./...`
Expected: PASS

- [ ] **Step 4: commit**

```bash
git add runner/connect.go cmd/agent-runner/
git commit -m "runner: add connection loop and cmd entry"
```

---

## Phase 4: CLI

### Task 4.1: CLI 共通接続ヘルパ & submit subcommand

**Files:**
- Create: `cli/client.go`, `cli/submit.go`, `cli/submit_test.go`, `cmd/harness-cli/main.go`

- [ ] **Step 1: `cli/client.go` (server への request/response を 1 回飛ばすヘルパ)**

```go
package cli

import (
	"context"
	"crypto/ecdh"
	"fmt"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/transport"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

type Client struct {
	conn objproto.Connection
}

func Dial(ctx context.Context, addr string) (*Client, error) {
	sess, err := transport.WebSocketSession(nil, addr, nil, objproto.SessionModeClient)
	if err != nil {
		return nil, err
	}
	conn, err := objproto.DoECDHHandshake(ctx, sess,
		objproto.MustParseConnectionID(addr+"-2222"),
		ecdh.P521(), objproto.AES128GCM)
	if err != nil {
		return nil, err
	}
	return &Client{conn: conn}, nil
}

func (c *Client) roundTripTaskControl(req *protocol.TaskControlRequest) (*protocol.TaskControlResponse, error) {
	data := append([]byte{byte(wire.ApplicationPayloadKind_TaskControl)}, req.MustAppend(nil)...)
	if _, _, err := c.conn.SendMessage(data); err != nil {
		return nil, err
	}
	msg, err := c.conn.ReceiveMessage()
	if err != nil {
		return nil, err
	}
	if len(msg.Data) == 0 || wire.ApplicationPayloadKind(msg.Data[0]) != wire.ApplicationPayloadKind_TaskControl {
		return nil, fmt.Errorf("unexpected response kind")
	}
	resp := &protocol.TaskControlResponse{}
	if _, err := resp.Decode(msg.Data[1:]); err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *Client) Close() { _ = c.conn.Close() }
```

- [ ] **Step 2: submit コマンド**

`cli/submit.go`:

```go
package cli

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func Submit(ctx context.Context, addr, repo, prompt string) (string, error) {
	c, err := Dial(ctx, addr)
	if err != nil {
		return "", err
	}
	defer c.Close()

	req := &protocol.TaskControlRequest{
		Kind: protocol.TaskControlKind_Submit,
		Submit: &protocol.SubmitRequest{
			RepoPathLen: uint16(len(repo)),
			RepoPath:    []byte(repo),
			PromptLen:   uint32(len(prompt)),
			Prompt:      []byte(prompt),
		},
	}
	resp, err := c.roundTripTaskControl(req)
	if err != nil {
		return "", err
	}
	if resp.Kind != protocol.TaskControlKind_Submit || resp.Submit == nil {
		return "", fmt.Errorf("bad response")
	}
	return hex.EncodeToString(resp.Submit.TaskId.Id[:]), nil
}
```

- [ ] **Step 3: cli main dispatcher**

`cmd/harness-cli/main.go`:

```go
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/on-keyday/agent-harness/cli"
)

var server = flag.String("server", "localhost:8539", "server host:port")

func main() {
	flag.Parse()
	if flag.NArg() == 0 {
		usage()
		os.Exit(2)
	}
	ctx := context.Background()
	sub := flag.Arg(0)
	args := flag.Args()[1:]
	switch sub {
	case "submit":
		fs := flag.NewFlagSet("submit", flag.ExitOnError)
		repo := fs.String("repo", "", "absolute path to repo")
		task := fs.String("task", "", "prompt text")
		fs.Parse(args)
		id, err := cli.Submit(ctx, *server, *repo, *task)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println(id)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: harness-cli [--server HOST:PORT] <submit|ls|logs|cancel|watch|prune> ...")
}
```

- [ ] **Step 4: unit test (submit が正しい request を投げるか — mock Client 用 interface を抽出して test)**

要望に応じて、`cli/submit.go` は `RoundTripper interface { roundTripTaskControl(req) (*resp, err) }` を受け取る関数に refactor、mock で test 可能にする。省略可だが推奨。

- [ ] **Step 5: build & commit**

```bash
go build ./...
git add cli/ cmd/harness-cli/
git commit -m "cli: add submit subcommand"
```

### Task 4.2: CLI ls subcommand

**Files:**
- Create: `cli/list.go`
- Modify: `cmd/harness-cli/main.go`

- [ ] **Step 1: 実装**

```go
// cli/list.go
package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func List(ctx context.Context, addr string, out io.Writer) error {
	c, err := Dial(ctx, addr)
	if err != nil {
		return err
	}
	defer c.Close()
	resp, err := c.roundTripTaskControl(&protocol.TaskControlRequest{
		Kind: protocol.TaskControlKind_List,
		List: &protocol.ListQuery{Query: nil},
	})
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "RUNNERS")
	for _, r := range resp.List.Runners {
		fmt.Fprintf(out, "  %s  %s  repo=%s  task=%x\n",
			statusStr(r.Status), "runner", string(r.RepoPath), r.CurrentTask.Id[:4])
	}
	fmt.Fprintln(out, "TASKS")
	for _, t := range resp.List.Tasks {
		fmt.Fprintf(out, "  %x  %s  repo=%s  prompt=%q\n",
			t.Id.Id[:6], taskStatusStr(t.Status), string(t.RepoPath), string(t.Prompt))
	}
	return nil
}

func statusStr(s protocol.RunnerStatus) string {
	switch s {
	case protocol.RunnerStatus_Idle:
		return "Idle   "
	case protocol.RunnerStatus_Busy:
		return "Busy   "
	default:
		return "Offline"
	}
}
func taskStatusStr(s protocol.TaskStatus) string {
	switch s {
	case protocol.TaskStatus_Queued:
		return "Queued   "
	case protocol.TaskStatus_Running:
		return "Running  "
	case protocol.TaskStatus_Succeeded:
		return "Succeeded"
	case protocol.TaskStatus_Failed:
		return "Failed   "
	case protocol.TaskStatus_Cancelled:
		return "Cancelled"
	}
	return "?"
}
```

- [ ] **Step 2: main.go の switch に `ls` case 追加**

```go
case "ls":
    if err := cli.List(ctx, *server, os.Stdout); err != nil { /* ... */ }
```

- [ ] **Step 3: build & commit**

```bash
go build ./...
git add cli/list.go cmd/harness-cli/main.go
git commit -m "cli: add ls subcommand"
```

### Task 4.3: CLI logs -f (pubsub subscriber)

**Files:**
- Create: `cli/logs.go`
- Modify: `cmd/harness-cli/main.go`

- [ ] **Step 1: 実装**

```go
// cli/logs.go
package cli

import (
	"context"
	"io"

	"github.com/on-keyday/agent-harness/pubsub"
	"github.com/on-keyday/agent-harness/topics"
	"github.com/on-keyday/agent-harness/trsf"
	"github.com/on-keyday/agent-harness/trsf/wire"
)

// cli は pubsub を使うので trsf stream 層も用意。Dial とは別に FullDial を作る。
func Logs(ctx context.Context, addr, taskID string, out io.Writer) error {
	c, err := Dial(ctx, addr)
	if err != nil {
		return err
	}
	defer c.Close()

	p := trsf.NewStreams(ctx, false, trsf.DefaultInitialMTU, trsf.DefaultMaxMTU, c.conn, nil)
	go trsf.AutoSend(ctx, p, c.conn, nil)

	topic := topics.TaskLog(taskID)
	joinData := pubsub.JoinTopic("cli", topic)
	if _, _, err := c.conn.SendMessage(joinData); err != nil {
		return err
	}

	// expect a stream to be accepted with topic header
	st, err := p.AcceptBidirectionalStream(ctx)
	if err != nil {
		return err
	}
	// first message contains the topic name line; skip
	_, _, _ = st.ReadDirect(1024)

	buf := make([]byte, 4096)
	for {
		// unused: just to avoid import removal
		_ = wire.ApplicationPayloadKind_Pubsub
		data, eof, err := st.ReadDirect(uint64(len(buf)))
		if err != nil {
			return err
		}
		if len(data) > 0 {
			out.Write(data)
		}
		if eof {
			return nil
		}
	}
}
```

**注:** pubsub の JOIN 応答 + stream 到着の順序が曖昧なので、実装後 E2E で挙動確認、必要なら schedule 調整。

- [ ] **Step 2: main.go 追加**

```go
case "logs":
    fs := flag.NewFlagSet("logs", flag.ExitOnError)
    fs.Parse(args)
    if fs.NArg() == 0 { usage(); os.Exit(2) }
    if err := cli.Logs(ctx, *server, fs.Arg(0), os.Stdout); err != nil { /* ... */ }
```

- [ ] **Step 3: build & commit**

```bash
go build ./...
git add cli/logs.go cmd/harness-cli/main.go
git commit -m "cli: add logs subcommand"
```

### Task 4.4: CLI cancel subcommand

**Files:**
- Create: `cli/cancel.go`
- Modify: `cmd/harness-cli/main.go`

- [ ] **Step 1: 実装**

```go
// cli/cancel.go
package cli

import (
	"context"
	"encoding/hex"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func Cancel(ctx context.Context, addr, taskIDHex string) error {
	c, err := Dial(ctx, addr)
	if err != nil {
		return err
	}
	defer c.Close()
	raw, _ := hex.DecodeString(taskIDHex)
	var tid protocol.TaskID
	copy(tid.Id[:], raw)
	_, err = c.roundTripTaskControl(&protocol.TaskControlRequest{
		Kind: protocol.TaskControlKind_Cancel,
		Cancel: &protocol.CancelTask{TaskId: tid},
	})
	return err
}
```

- [ ] **Step 2: main.go 追加**

```go
case "cancel":
    // fs with NArg check
    if err := cli.Cancel(ctx, *server, flag.Arg(1)); err != nil { /* ... */ }
```

- [ ] **Step 3: build & commit**

```bash
go build ./...
git add cli/cancel.go cmd/harness-cli/main.go
git commit -m "cli: add cancel subcommand"
```

### Task 4.5: CLI watch subcommand

**Files:**
- Create: `cli/watch.go`
- Modify: `cmd/harness-cli/main.go`, `server/server.go`

`tasks.status` / `runners.status` topic を subscribe して tail 風表示。**server 側で event を publish する場所が未実装なので、まず server に publish hook を足す**。

- [ ] **Step 1: server 側で status event を publish**

`server/server.go` で `TaskStore.OnCreate` 等の callback を受けて `tasks.status` topic に `TaskStatusEvent` を publish する hook を追加。`TaskStore` に `OnChange func(ev WALEvent)` を足し、server がそれを受けて pubsub へ publish。

- [ ] **Step 2: cli/watch.go 実装** (logs と同様の pubsub subscribe)

- [ ] **Step 3: main.go に case 追加**

- [ ] **Step 4: build & 手動確認**

```bash
# 2 terminal
go run ./cmd/harness-server &
go run ./cmd/harness-cli watch
# 3rd: submit a task
go run ./cmd/harness-cli submit --repo /tmp/foo --task echo
```

Expected: watch 側に event が流れる。

- [ ] **Step 5: commit**

```bash
git add cli/watch.go cmd/harness-cli/main.go server/server.go
git commit -m "cli: add watch subcommand"
```

### Task 4.6: CLI prune subcommand

**Files:**
- Create: `cli/prune.go`, `runner/prune.go`

v1 最小: CLI は "runner に prune 要求を投げる" のではなく、**ローカルに直接 `git worktree remove`** を試す。server を介さない純 local utility で十分。

- [ ] **Step 1: 実装**

```go
// cli/prune.go
package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func Prune(repo string, before time.Duration, out io.Writer) error {
	dir := filepath.Join(repo, ".harness-worktrees")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-before)
	for _, e := range entries {
		info, _ := e.Info()
		if info.ModTime().After(cutoff) {
			continue
		}
		path := filepath.Join(dir, e.Name())
		cmd := exec.Command("git", "worktree", "remove", "--force", path)
		cmd.Dir = repo
		out2, cerr := cmd.CombinedOutput()
		if cerr != nil {
			fmt.Fprintf(out, "skip %s: %s\n", e.Name(), out2)
			continue
		}
		fmt.Fprintf(out, "removed %s\n", e.Name())
	}
	return nil
}
```

- [ ] **Step 2: main.go に case 追加**

- [ ] **Step 3: build & commit**

```bash
go build ./...
git add cli/prune.go cmd/harness-cli/main.go
git commit -m "cli: add prune subcommand"
```

---

## Phase 5: Integration & cleanup

### Task 5.1: E2E integration test

**Files:**
- Create: `integration/e2e_test.go`

1 プロセス内で `server.Run` を goroutine 起動 → `runner.Run` を goroutine 起動 → `cli.Submit` → 成果物確認。`claude` は fake-claude.sh を使う。

- [ ] **Step 1: テスト**

```go
//go:build integration

package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner"
	"github.com/on-keyday/agent-harness/server"
)

func TestSubmitFakeClaudeE2E(t *testing.T) {
	repo := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		cmd.Run()
	}
	run("init", "-b", "main")
	os.WriteFile(filepath.Join(repo, "README"), []byte("x\n"), 0o644)
	run("add", "README"); run("commit", "-m", "init")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go server.Run(ctx, server.Config{
		Addr:    "localhost:18539",
		DataDir: t.TempDir(),
	})
	time.Sleep(200 * time.Millisecond)

	go runner.Run(ctx, runner.Config{
		ServerAddr: "localhost:18539",
		RepoPath:   repo,
		ClaudeBin:  "../testdata/fake-claude.sh",
	})
	time.Sleep(300 * time.Millisecond)

	id, err := cli.Submit(ctx, "localhost:18539", repo, "hi")
	if err != nil {
		t.Fatal(err)
	}
	t.Log("submitted", id)

	// poll until task is done
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		// use cli.List indirectly — or expose a helper. simplest: sleep.
		time.Sleep(500 * time.Millisecond)
		wt := filepath.Join(repo, ".harness-worktrees", id)
		if _, err := os.Stat(filepath.Join(wt, "hello.txt")); err == nil {
			return
		}
	}
	t.Fatal("timeout waiting for task")
}
```

- [ ] **Step 2: Run with build tag**

Run: `go test -tags integration ./integration/... -v -run TestSubmitFakeClaudeE2E`
Expected: PASS

flaky なら sleep を 1s ずつ伸ばす。安定性最優先。

- [ ] **Step 3: commit**

```bash
git add integration/e2e_test.go
git commit -m "integration: e2e smoke test with fake claude"
```

### Task 5.2: Dead code 削除

**Files:**
- Delete: `cmd/harness-client/main.go`, `cmd/harness-client/`

- [ ] **Step 1: 確認**

Run: `git grep -l harness-client`
Expected: hits のみ。依存なしを確認。

- [ ] **Step 2: 削除**

```bash
git rm -r cmd/harness-client
```

- [ ] **Step 3: build 通過**

Run: `go build ./...`

- [ ] **Step 4: commit**

```bash
git commit -m "chore: remove unused echo sample (cmd/harness-client)"
```

### Task 5.3: README に使い方だけ最低限

**Files:**
- Create: `README.md` (exists? check)

- [ ] **Step 1: 確認**

Run: `ls README.md`
Expected: なければ create、あれば append。

- [ ] **Step 2: 書く**

```markdown
# agent-harness

Parallel Claude Code CLI harness (v1, local-only).

See `docs/superpowers/specs/2026-04-25-parallel-agent-harness-design.md` for design.

## Quick start

```
# terminal 1: server
go run ./cmd/harness-server --port 8539 --data-dir ./harness-data

# terminal 2: runner (bound to a repo)
go run ./cmd/agent-runner --server localhost:8539 --repo /abs/path/to/repo

# terminal 3: submit
go run ./cmd/harness-cli submit --repo /abs/path/to/repo --task "write hello.txt"
go run ./cmd/harness-cli ls
go run ./cmd/harness-cli logs <task-id>
```

**v1 注意:** runner は起動時の repo 専用。並列度は runner を複数起動して稼ぐ。auto-commit は無し — 成果物は worktree 内 `.harness-worktrees/<task-id>/` に dirty のまま残る。
```

- [ ] **Step 3: commit**

```bash
git add README.md
git commit -m "docs: add README with quick-start"
```

---

## Self-review checklist (run before starting execution)

1. **Spec coverage:**
   - §4 Architecture: Phase 2.7 で server.Run が 3 components を繋ぐ ✓
   - §5.1 server: Registry / TaskStore / Scheduler / Dispatcher ✓
   - §5.2 runner: worktree / process / session / connect ✓
   - §5.3 cli: submit / ls / logs / cancel / watch / prune ✓
   - §6 data model: RunnerEntry / TaskEntry ✓
   - §7 protocol: bgn 拡張 (Task 1.1) + wire kind dispatcher ✓
   - §8.1 persistence: WAL (Task 2.8) + LogStore (Task 2.9) ✓
   - §8.2 障害ケース: replay で Running → Failed (Task 2.8), runner disconnect で `Registry.Remove` (Task 2.7) ✓
   - §9.1 auto-commit: (b) 触らない確定、plan header で明記 ✓
   - §10 testing: unit (Phase 1-4), integration (Task 5.1) ✓

2. **Placeholder scan:** "TBD", "TODO" の意図的明示 (Task 2.7 の `sendAssign` → Task 2.7b、Task 3.4 の pubsub JOIN 応答 → 3.4b ノート) 以外にない ✓

3. **Type consistency:**
   - `RunnerEntry.ID` = `string` (= ConnectionID.String()), `TaskEntry.ID` = hex string (16 bytes)
   - `protocol.TaskID.Id` = `[16]byte`、CLI 表示は hex
   - `RunnerMessageType_Heartbeat` を Task 1.1 schema 修正で確定、全 test / handler で `Heartbeat` を使用 ✓

4. **v1 非対応の明示:**
   - Remote / 分散 / Proxy / Probe: §2 Non-goals
   - PTY / attach / session multiplex: §2 Non-goals
   - Auto-commit: open question を (b) で v1 確定 ✓
