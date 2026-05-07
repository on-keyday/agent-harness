# Interactive PTY Detach / Reattach Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `harness-cli session new` で起動した interactive PTY claude を、client 切断時に runner 上で生存させたまま detach し、`harness-cli session attach <id>` で raw ANSI ring buffer の replay 後に再接続できるようにする。tmux 的 UX。

**Architecture:** server は既に `spliceBidi` で client↔runner の bidi stream をサーバ内 proxy している (`server/task_handler.go:512`)。これを `SessionMux` (新規) に置き換え、`runnerStream` を session lifetime で永続化、client tuiStream と独立に管理する。ring buffer (raw bytes, default 1MiB) を `SessionMux` が所有し、attach 時に dump→live forward。runner 側は変更ほぼ無し (SIGHUP ladder の trigger は stdin pipe EOF なので、server が runnerStream を閉じない限り発火しない)。

**Tech Stack:** Go 1.25.x、`brgen`/ebm2go (`.bgn` → Go codec、`make protoregen ARGS=...` で再生成)、既存 `objproto` / `peer` / `trsf` / `wire` / `runner/protocol`。Spec: `docs/superpowers/specs/2026-05-08-interactive-detach-reattach-design.md`。

---

## Reference for implementers

### 既存コードの前提

- `server/task_handler.go:handleOpenInteractive` は既に **server-as-proxy** 構造で、tuiStream / runnerStream の 2 本を allocate して `spliceBidi` (`task_handler.go:512`) で双方向 pump している。
- `runner/session.go:handleOpenExec` (`session.go:379`) は `agentexec.ExecuteCommandWithOption` を呼ぶ。SIGHUP ladder は `exec/exec.go:205-221` で `io.Copy(p, pipeOut)` の EOF 時に発火する。pipeOut は bidi stream の Read 側 → server が runnerStream を閉じなければ発火しない。
- TaskStore は既に Resume 経路 (terminal interactive task の worktree 流用) を持つ (`server/task_handler.go:262`)。Detached は別経路で、resume 対象にしない。
- bgn の bit-field pattern は `trsf/wire/stream.bgn:33-37` の `:u1` flag + `reserved :uN` を踏襲。

### 用語

- **session**: detachable=1 で起動された interactive task。`TaskKind_Interactive` + `Detachable` flag。
- **SessionMux**: 1 session に 1 instance、server プロセス内に存在、runnerStream / 接続中 tuiStream / ring buffer を所有。
- **detached**: client tuiStream が居ない状態。`TaskStatus_Detached`。
- **takeover**: 既存 attach client がいるところに新 attach が来ると、旧 client を蹴って新 client が install される (Section 5 §6 の (b) 案)。

### .bgn → Go 再生成

`.bgn` を編集したら以下で再生成:

```bash
make protoregen ARGS=runner/protocol/message.bgn
```

再生成後 `git diff runner/protocol/message.go` で内容確認すること。

### 既存 server / runner / cli entry points

- `server/server.go`: WS 接続、TaskStore / LogStore / pubsub / agentboard wiring、`OnControl` で payload kind dispatch。
- `server/dispatch.go`: TaskQueue + RunnerRegistry + `handleTaskControl` のディスパッチ。
- `server/task_handler.go`: `handleSubmit` / `handleOpenInteractive` / `handleAttachSession` (新設) / `handleGetTaskLog` 等。
- `runner/session.go:handleOpenExec`: runner 側 PTY 起動の入口。
- `cli/open_interactive_native.go`: client 側 OpenInteractive RoundTrip。
- `cmd/harness-cli/main.go`: subcommand dispatcher。

---

## File structure

### Create

```
server/ring_buffer.go              # 固定サイズ raw byte ring buffer
server/ring_buffer_test.go
server/session_mux.go              # SessionMux: runnerStream / tuiStream / ring buffer の triplet 管理
server/session_mux_test.go
server/session_registry.go         # taskID → *SessionMux マップ
server/session_registry_test.go
cli/attach.go                      # AttachSession round-trip + stream wrap
cli/agent/session.go               # `session` subcommand 群 (new/attach/ls/kill)
testdata/fake-claude-loud.sh       # 大量出力で ring buffer wrap を起こす fake claude
integration/session_detach_test.go # E2E (build tag: integration)
```

### Modify

```
runner/protocol/message.bgn           # §7 の全 schema 追加
runner/protocol/message.go            # 自動再生成
server/task_handler.go                # handleOpenInteractive 分岐、handleAttachSession 追加
server/dispatch.go                    # attach_session ディスパッチ
server/taskstore.go                   # Detached 遷移ルール
server/server.go                      # 起動時に Detached → Cancelled マーク
cmd/harness-server/main.go            # 新 flag (--detach-ring-buffer-size, --detach-idle-timeout)
runner/session.go                     # handleOpenExec で oer.Detachable をログに残す (診断用)
cli/open_interactive_native.go        # OpenInteractive に detachable param
cmd/harness-cli/main.go               # session subcommand dispatch
tui/tasks.go (or similar)             # Detached 表示 + S キー
```

---

## Tasks

### Task 1: Schema additions (`runner/protocol/message.bgn`)

**Files:**
- Modify: `runner/protocol/message.bgn`
- Regen: `runner/protocol/message.go`

ユーザ memory の `feedback_no_split_schemas` に従い、§7 の全 schema 変更を 1 タスクで完了させる。

- [ ] **Step 1: TaskStatus に Detached を追加**

`enum TaskStatus` (現行 `Cancelled` の後) に追加:

```
enum TaskStatus:
    :u8
    Queued
    Running
    Succeeded
    Failed
    Cancelled
    Detached
```

- [ ] **Step 2: TaskControlKind に attach_session を追加**

```
enum TaskControlKind:
    :u8
    submit
    list
    cancel
    prune_tasks
    get_task_log
    open_interactive
    client_hello
    attach_session
```

- [ ] **Step 3: OpenInteractiveRequest / OpenExecRunnerRequest に detachable :u1 + reserved :u7 を追加**

```
format OpenInteractiveRequest:
    repo_path_len :u16
    repo_path :[repo_path_len]u8
    selector :RunnerSelector
    extra_args :ClaudeArgs
    resume_task_id :TaskID
    detachable :u1
    reserved :u7

format OpenExecRunnerRequest:
    task_id :TaskID
    auth_ticket :[16]u8
    repo_path_len :u16
    repo_path :[repo_path_len]u8
    stream_id :u64
    extra_args :ClaudeArgs
    detachable :u1
    reserved :u7
```

- [ ] **Step 4: AttachSession 系 format を追加**

```
format AttachSessionRequest:
    task_id :TaskID

enum AttachSessionStatus:
    :u8
    ok                  = "ok"
    not_found           = "not_found"
    not_interactive     = "not_interactive"
    not_detachable      = "not_detachable"
    already_terminal    = "already_terminal"
    runner_unreachable  = "runner_unreachable"
    internal_error      = "internal_error"

format AttachSessionResponse:
    status :AttachSessionStatus
    stream_id :u64
    replay_bytes :u64
```

- [ ] **Step 5: TaskControlRequest / TaskControlResponse の match に attach_session 分岐を追加**

```
format TaskControlRequest:
    kind :TaskControlKind
    request_id :u32
    match kind:
        TaskControlKind.submit => submit :SubmitRequest
        TaskControlKind.list => list :ListQuery
        TaskControlKind.cancel => cancel :CancelTask
        TaskControlKind.prune_tasks => prune :PruneTasksRequest
        TaskControlKind.get_task_log => get_log :GetTaskLogRequest
        TaskControlKind.open_interactive => open_interactive :OpenInteractiveRequest
        TaskControlKind.client_hello => client_hello :ClientHello
        TaskControlKind.attach_session => attach :AttachSessionRequest
        .. => error("Unexpected task")

format TaskControlResponse:
    kind :TaskControlKind
    request_id :u32
    match kind:
        TaskControlKind.submit => submit :SubmitResponse
        TaskControlKind.list => list :ListResult
        TaskControlKind.cancel => cancel :CancelStatus
        TaskControlKind.prune_tasks => prune :PruneTasksResponse
        TaskControlKind.get_task_log => get_log :GetTaskLogResponse
        TaskControlKind.open_interactive => open_interactive :OpenInteractiveResponse
        TaskControlKind.client_hello => client_hello :ClientHelloResponse
        TaskControlKind.attach_session => attach :AttachSessionResponse
```

- [ ] **Step 6: TaskInfo に bit flags + ring_buffer_bytes を追加**

```
format TaskInfo:
    id :TaskID
    status :TaskStatus
    kind :TaskKind
    origin_kind :ClientKind
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
    error_len :u32
    error_message :[error_len]u8
    detachable :u1
    is_attached :u1
    reserved :u6
    ring_buffer_bytes :u64
```

- [ ] **Step 7: 再生成**

```bash
make protoregen ARGS=runner/protocol/message.bgn
```

Expected: `runner/protocol/message.go` に `TaskStatus_Detached`, `TaskControlKind_AttachSession`, `AttachSessionRequest`, `AttachSessionResponse`, `AttachSessionStatus_*`, `OpenInteractiveRequest.Detachable`, `OpenExecRunnerRequest.Detachable`, `TaskInfo.Detachable / IsAttached / RingBufferBytes` が追加される。

- [ ] **Step 8: 既存 build を一旦通す**

```bash
go build ./...
```

新 enum/field を参照する場所はまだ無いため、build はクリーンに通るはず。失敗する場合は generated code に文法エラーが無いか確認 (bgn の u1/u7 サポート未実装など)。問題があれば user に相談 (memory `user_brgen_author` 参照)。

- [ ] **Step 9: Commit**

```bash
git add runner/protocol/message.bgn runner/protocol/message.go
git commit -m "protocol: add detach/reattach schema additions

TaskStatus_Detached, attach_session TaskControlKind, AttachSession*
formats/enums, OpenInteractive/OpenExec detachable bit, TaskInfo
detachable/is_attached/ring_buffer_bytes."
```

---

### Task 2: `RingBuffer` 固定サイズ raw byte ring

**Files:**
- Create: `server/ring_buffer.go`
- Test: `server/ring_buffer_test.go`

固定サイズの byte ring。`Append([]byte)` で書き込み、wrap-around で古いの破棄。`Snapshot() []byte` で現在の中身を古い→新しい順に dump。

- [ ] **Step 1: テストを書く** (`server/ring_buffer_test.go`)

```go
package server

import (
	"bytes"
	"testing"
)

func TestRingBuffer_AppendUnderCapacity(t *testing.T) {
	rb := NewRingBuffer(16)
	rb.Append([]byte("hello"))
	rb.Append([]byte(" world"))
	got := rb.Snapshot()
	if !bytes.Equal(got, []byte("hello world")) {
		t.Fatalf("got %q want %q", got, "hello world")
	}
	if rb.Len() != 11 {
		t.Fatalf("Len=%d want 11", rb.Len())
	}
}

func TestRingBuffer_WrapAround(t *testing.T) {
	rb := NewRingBuffer(8)
	rb.Append([]byte("abcdefghij")) // 10 bytes into 8-byte buffer
	got := rb.Snapshot()
	if !bytes.Equal(got, []byte("cdefghij")) {
		t.Fatalf("got %q want %q", got, "cdefghij")
	}
	if rb.Len() != 8 {
		t.Fatalf("Len=%d want 8", rb.Len())
	}
}

func TestRingBuffer_AppendLargerThanCap(t *testing.T) {
	rb := NewRingBuffer(4)
	rb.Append([]byte("0123456789"))
	got := rb.Snapshot()
	if !bytes.Equal(got, []byte("6789")) {
		t.Fatalf("got %q want %q", got, "6789")
	}
}

func TestRingBuffer_EmptySnapshot(t *testing.T) {
	rb := NewRingBuffer(8)
	if len(rb.Snapshot()) != 0 {
		t.Fatalf("empty buffer Snapshot should be empty")
	}
}
```

- [ ] **Step 2: テスト実行 (failing)**

```bash
go test ./server/ -run TestRingBuffer -v
```

Expected: `undefined: NewRingBuffer` でコンパイルエラー。

- [ ] **Step 3: 実装** (`server/ring_buffer.go`)

```go
package server

import "sync"

// RingBuffer is a fixed-size raw-byte ring. Appends past capacity overwrite
// the oldest bytes. Safe for concurrent use.
type RingBuffer struct {
	mu       sync.Mutex
	buf      []byte
	cap      int
	writeIdx int  // next write position
	full     bool // true once buf has wrapped at least once
}

func NewRingBuffer(capacity int) *RingBuffer {
	if capacity <= 0 {
		capacity = 1
	}
	return &RingBuffer{buf: make([]byte, capacity), cap: capacity}
}

func (r *RingBuffer) Append(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(p) >= r.cap {
		// only the last `cap` bytes survive
		copy(r.buf, p[len(p)-r.cap:])
		r.writeIdx = 0
		r.full = true
		return
	}
	tail := r.cap - r.writeIdx
	if len(p) <= tail {
		copy(r.buf[r.writeIdx:], p)
	} else {
		copy(r.buf[r.writeIdx:], p[:tail])
		copy(r.buf[0:], p[tail:])
		r.full = true
	}
	r.writeIdx = (r.writeIdx + len(p)) % r.cap
	if r.writeIdx == 0 && len(p) > 0 {
		r.full = true
	}
}

// Snapshot returns the buffer content in oldest-to-newest order.
func (r *RingBuffer) Snapshot() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.full {
		out := make([]byte, r.writeIdx)
		copy(out, r.buf[:r.writeIdx])
		return out
	}
	out := make([]byte, r.cap)
	copy(out, r.buf[r.writeIdx:])
	copy(out[r.cap-r.writeIdx:], r.buf[:r.writeIdx])
	return out
}

func (r *RingBuffer) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.full {
		return r.cap
	}
	return r.writeIdx
}
```

- [ ] **Step 4: テスト実行 (passing)**

```bash
go test ./server/ -run TestRingBuffer -v
```

Expected: 4 tests pass.

- [ ] **Step 5: Commit**

```bash
git add server/ring_buffer.go server/ring_buffer_test.go
git commit -m "server: add RingBuffer for detached session scrollback"
```

---

### Task 3: `SessionMux` core (lifecycle, attach, detach, takeover)

**Files:**
- Create: `server/session_mux.go`
- Test: `server/session_mux_test.go`

SessionMux 責務 (再掲):
- runnerStream を所有 (永続)
- 接続中 tuiStream を 0 or 1
- ring buffer (raw bytes)
- runner→client goroutine: runnerStream.Read → ring buffer + tuiStream.Write (tui あれば)
- client→runner goroutine: tuiStream.Read → runnerStream.Write (tui あれば)
- Attach(tuiStream): 既存があれば takeover (close)、ring buffer dump → live forward 接続。
- Detach(): tuiStream を nil 化、status=Detached への遷移 hook を呼ぶ。
- Stop(): 全部閉じる (cancel 用)。

`trsf.BidirectionalStream` の API は既存 `spliceBidi` (`server/task_handler.go:512`) と `relayBytes` (`task_handler.go:531`) を参考にする (`ReadDirect` / `Write` / `CloseBoth` 等)。

- [ ] **Step 1: テストを書く** (`server/session_mux_test.go`)

最小ケース 3 つ: append + replay、detach 後 ring に貯まる、takeover で旧 tuiStream が閉じる。

```go
package server

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"
)

// fakeStream は trsf.BidirectionalStream を充足する最小 mock。
// 必要なメソッド (ReadDirect, Write, CloseBoth, ID 等) は spliceBidi /
// relayBytes が呼んでいるものを実装。
type fakeStream struct {
	id        uint64
	readBuf   chan []byte // queued reads
	writeBuf  *bytes.Buffer
	closed    bool
	closeOnce chan struct{}
}

// fakeStream が trsf.BidirectionalStream を満たすための実装は
// `server/fakes_test.go` の既存 helper を流用する。新規 fakeStream を
// 作る場合は ReadDirect / Write / CloseBoth / ID() を最低限実装する。

func TestSessionMux_AttachReplaysRingBuffer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	runnerStream := newFakeStream(t, 100)
	mux := NewSessionMux(ctx, "task-abc", runnerStream, NewRingBuffer(32))

	// Inject runner output before any client attaches.
	runnerStream.QueueRead([]byte("preattach payload"))

	// Wait for SessionMux to consume.
	waitFor(t, func() bool { return mux.RingBufferLen() == len("preattach payload") })

	// Attach a client.
	tui := newFakeStream(t, 100)
	if err := mux.Attach(ctx, tui); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	// Client should see the replay.
	got := tui.WaitWritten(t, len("preattach payload"))
	if !bytes.Equal(got, []byte("preattach payload")) {
		t.Fatalf("replay got %q want %q", got, "preattach payload")
	}
}

func TestSessionMux_DetachKeepsRunnerStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runnerStream := newFakeStream(t, 100)
	mux := NewSessionMux(ctx, "task", runnerStream, NewRingBuffer(64))

	tui := newFakeStream(t, 100)
	mux.Attach(ctx, tui)

	tui.CloseRead() // simulate client disconnect
	waitFor(t, func() bool { return !mux.IsAttached() })
	if runnerStream.IsClosed() {
		t.Fatal("runnerStream must NOT be closed on client detach")
	}

	// new runner output should still be ringbuffered
	runnerStream.QueueRead([]byte("post-detach"))
	waitFor(t, func() bool { return mux.RingBufferLen() == len("post-detach") })
}

func TestSessionMux_AttachTakeover(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	runner := newFakeStream(t, 100)
	mux := NewSessionMux(ctx, "task", runner, NewRingBuffer(32))

	first := newFakeStream(t, 100)
	mux.Attach(ctx, first)

	second := newFakeStream(t, 100)
	if err := mux.Attach(ctx, second); err != nil {
		t.Fatalf("second Attach: %v", err)
	}
	if !first.IsClosed() {
		t.Fatal("first tuiStream must be closed after takeover")
	}
	if !mux.IsAttached() {
		t.Fatal("mux must be attached to second")
	}
}
```

`newFakeStream` / `waitFor` helper は `server/fakes_test.go` パターンで実装 (既存の同ファイル参照)。`io.EOF` は省略。

- [ ] **Step 2: テスト実行 (failing)**

```bash
go test ./server/ -run TestSessionMux -v
```

Expected: `undefined: NewSessionMux` 等でコンパイルエラー。

- [ ] **Step 3: 実装** (`server/session_mux.go`)

```go
package server

import (
	"context"
	"errors"
	"io"
	"sync"

	"github.com/on-keyday/agent-harness/trsf"
)

// SessionMux owns the runner-side bidi stream for a detachable interactive
// session. It pumps runner output into a RingBuffer, forwards to whatever
// tuiStream is currently attached, and accepts new client tuiStreams that
// take over from any existing attach.
type SessionMux struct {
	ctx       context.Context
	cancel    context.CancelFunc
	taskID    string
	runner    trsf.BidirectionalStream
	ring      *RingBuffer

	mu        sync.Mutex
	tui       trsf.BidirectionalStream // 0 or 1
	tuiCancel context.CancelFunc       // cancels the active tuiStream pump goroutines

	onDetach  func(taskID string) // status transition hook (Running→Detached)
	onAttach  func(taskID string) // status transition hook (Detached→Running)
	onStop    func(taskID string) // SessionMux exit (terminal)

	stopOnce  sync.Once
	stopped   chan struct{}
}

func NewSessionMux(parentCtx context.Context, taskID string, runner trsf.BidirectionalStream, ring *RingBuffer) *SessionMux {
	ctx, cancel := context.WithCancel(parentCtx)
	m := &SessionMux{
		ctx: ctx, cancel: cancel,
		taskID: taskID, runner: runner, ring: ring,
		stopped: make(chan struct{}),
	}
	go m.runnerPump()
	return m
}

// SetHooks wires status-transition callbacks. Call once before Attach.
func (m *SessionMux) SetHooks(onAttach, onDetach, onStop func(taskID string)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onAttach, m.onDetach, m.onStop = onAttach, onDetach, onStop
}

// runnerPump reads runner output forever, appends to ring, and forwards to
// the active tuiStream if any. Returns when runner stream EOFs (= terminal).
func (m *SessionMux) runnerPump() {
	buf := make([]byte, 32*1024)
	defer m.Stop()
	for {
		n, err := m.runner.ReadDirect(buf)
		if n > 0 {
			m.ring.Append(buf[:n])
			m.mu.Lock()
			tui := m.tui
			m.mu.Unlock()
			if tui != nil {
				if _, werr := tui.Write(buf[:n]); werr != nil {
					// tui write failed → drop tui (treat as detach)
					m.detachLocked(tui)
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// Attach installs tui as the current attached stream. If another tui is
// already attached, it is force-closed (takeover). Replay of the ring
// buffer happens BEFORE this method returns so the caller can immediately
// rely on live forward.
func (m *SessionMux) Attach(ctx context.Context, tui trsf.BidirectionalStream) error {
	m.mu.Lock()
	if m.ctx.Err() != nil {
		m.mu.Unlock()
		return errors.New("session_mux: stopped")
	}
	old := m.tui
	if m.tuiCancel != nil {
		m.tuiCancel()
	}
	m.tui = tui
	tuiCtx, tuiCancel := context.WithCancel(m.ctx)
	m.tuiCancel = tuiCancel
	onAttach := m.onAttach
	m.mu.Unlock()

	if old != nil {
		_ = old.CloseBoth()
	}

	// Replay ring buffer (raw bytes, oldest first) before live forwarding
	// resumes for this stream. The runnerPump goroutine ALSO writes to tui
	// concurrently — replay duplicate is acceptable because the ring buffer
	// already contains those bytes, and clients render raw ANSI.
	snap := m.ring.Snapshot()
	if len(snap) > 0 {
		if _, err := tui.Write(snap); err != nil {
			m.mu.Lock()
			if m.tui == tui {
				m.tui = nil
				m.tuiCancel = nil
			}
			m.mu.Unlock()
			return err
		}
	}

	if onAttach != nil {
		onAttach(m.taskID)
	}

	// Stdin pump (tui → runner) for this attach session.
	go m.tuiPump(tuiCtx, tui)
	return nil
}

func (m *SessionMux) tuiPump(ctx context.Context, tui trsf.BidirectionalStream) {
	buf := make([]byte, 32*1024)
	for {
		if ctx.Err() != nil {
			return
		}
		n, err := tui.ReadDirect(buf)
		if n > 0 {
			if _, werr := m.runner.Write(buf[:n]); werr != nil {
				m.Stop() // runner write fail = session lost
				return
			}
		}
		if err != nil {
			m.detachOnly(tui)
			return
		}
	}
}

// detachOnly drops the given tui (no-op if not current). Called when tui
// stream EOFs naturally (= client disconnect).
func (m *SessionMux) detachOnly(tui trsf.BidirectionalStream) {
	m.mu.Lock()
	if m.tui != tui {
		m.mu.Unlock()
		return
	}
	m.detachLocked(tui)
	m.mu.Unlock()
}

// detachLocked must be called with m.mu held. tui != m.tui races are caller's
// responsibility (use detachOnly for safe dispatch).
func (m *SessionMux) detachLocked(tui trsf.BidirectionalStream) {
	if m.tui == tui {
		m.tui = nil
		if m.tuiCancel != nil {
			m.tuiCancel()
			m.tuiCancel = nil
		}
		_ = tui.CloseBoth()
		if m.onDetach != nil {
			go m.onDetach(m.taskID)
		}
	}
}

func (m *SessionMux) IsAttached() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.tui != nil
}

func (m *SessionMux) RingBufferLen() int { return m.ring.Len() }

func (m *SessionMux) RingBufferBytes() []byte { return m.ring.Snapshot() }

// Stop tears down the session: closes tui (if any), closes runner stream,
// invokes onStop. Idempotent.
func (m *SessionMux) Stop() {
	m.stopOnce.Do(func() {
		m.cancel()
		m.mu.Lock()
		tui := m.tui
		m.tui = nil
		m.mu.Unlock()
		if tui != nil {
			_ = tui.CloseBoth()
		}
		_ = m.runner.CloseBoth()
		if m.onStop != nil {
			m.onStop(m.taskID)
		}
		close(m.stopped)
	})
}

// Wait blocks until Stop has been called and processing has ended. Used by
// callers that want to know when the SessionMux has fully terminated.
func (m *SessionMux) Wait() <-chan struct{} { return m.stopped }
```

注意: `io` パッケージ import は使用しない場合は外す (Go コンパイルエラー回避)。

- [ ] **Step 4: fakeStream helper を `server/fakes_test.go` に追加または `session_mux_test.go` に inline 化**

既存 `server/fakes_test.go` を参考に `newFakeStream`、`waitFor`、`(*fakeStream).QueueRead/WaitWritten/CloseRead/IsClosed` を実装。`trsf.BidirectionalStream` の interface 実装が要点。同 interface のメソッドを満たすため、まず `grep -n "type BidirectionalStream" trsf/` でメソッド一覧を確認する。

```bash
grep -n "BidirectionalStream\b" trsf/*.go | head -10
```

実装が複雑であれば、既存 `server/fakes_test.go` の test helper をそのまま流用 / 拡張する。

- [ ] **Step 5: テスト実行 (passing)**

```bash
go test ./server/ -run TestSessionMux -v -race
```

Expected: 3 tests pass。`-race` で race condition 無し。

- [ ] **Step 6: Commit**

```bash
git add server/session_mux.go server/session_mux_test.go server/fakes_test.go
git commit -m "server: add SessionMux for detachable interactive sessions"
```

---

### Task 4: `SessionRegistry` (taskID → *SessionMux)

**Files:**
- Create: `server/session_registry.go`
- Test: `server/session_registry_test.go`

- [ ] **Step 1: テスト**

```go
package server

import "testing"

func TestSessionRegistry_AddGetRemove(t *testing.T) {
	r := NewSessionRegistry()
	if got := r.Get("nope"); got != nil {
		t.Fatal("empty registry must return nil")
	}
	mux := &SessionMux{taskID: "t1"}
	r.Add("t1", mux)
	if got := r.Get("t1"); got != mux {
		t.Fatalf("Get returned %v want %v", got, mux)
	}
	r.Remove("t1")
	if got := r.Get("t1"); got != nil {
		t.Fatal("Remove failed")
	}
}

func TestSessionRegistry_AddReplaces(t *testing.T) {
	r := NewSessionRegistry()
	a, b := &SessionMux{taskID: "x"}, &SessionMux{taskID: "x"}
	r.Add("x", a)
	r.Add("x", b)
	if got := r.Get("x"); got != b {
		t.Fatal("Add must replace existing entry")
	}
}
```

- [ ] **Step 2: 実装** (`server/session_registry.go`)

```go
package server

import "sync"

type SessionRegistry struct {
	mu sync.RWMutex
	m  map[string]*SessionMux
}

func NewSessionRegistry() *SessionRegistry {
	return &SessionRegistry{m: map[string]*SessionMux{}}
}

func (r *SessionRegistry) Add(taskID string, mux *SessionMux) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[taskID] = mux
}

func (r *SessionRegistry) Get(taskID string) *SessionMux {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.m[taskID]
}

func (r *SessionRegistry) Remove(taskID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, taskID)
}

func (r *SessionRegistry) Snapshot() map[string]*SessionMux {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]*SessionMux, len(r.m))
	for k, v := range r.m {
		out[k] = v
	}
	return out
}
```

- [ ] **Step 3: テスト pass + commit**

```bash
go test ./server/ -run TestSessionRegistry -v
git add server/session_registry.go server/session_registry_test.go
git commit -m "server: add SessionRegistry mapping task id to SessionMux"
```

---

### Task 5: TaskStore に Detached 遷移ルールを追加

**Files:**
- Modify: `server/taskstore.go`
- Modify: `server/taskstore_test.go` (既存)

- [ ] **Step 1: 既存の遷移ルールを把握**

```bash
grep -n 'TaskStatus_\|allowedTransition\|setStatus' server/taskstore.go | head -30
```

`taskstore.go` 内の遷移検証関数 (たいてい `allowedTransition` か `transition` 名) を見つけ、Detached の許可表を追加する。

- [ ] **Step 2: 遷移ルール定義**

```
Running   → Detached   (client disconnect on detachable=1)
Detached  → Running    (new attach succeeds)
Detached  → Succeeded  (claude self-exit code 0)
Detached  → Failed     (claude self-exit non-zero, runner unreachable)
Detached  → Cancelled  (cancel / session kill)
```

具体パッチは `taskstore.go` の switch / map にこれらを追加する形 (既存実装が switch なら switch、map なら map に append)。

- [ ] **Step 3: TaskInfo の新フィールドへの値書き込み**

`Task` 内部構造体 (おそらく `entry` 等) に `Detachable bool`, `IsAttached bool`, `RingBufferBytes uint64` フィールドを追加。`ToProto()` 等の TaskInfo 生成箇所で値を埋める:

```go
info.Detachable = boolToU1(t.detachable)
info.IsAttached = boolToU1(t.isAttached)
info.RingBufferBytes = t.ringBufferBytes
```

`boolToU1` ヘルパー (8 bit field の値を 0/1 で埋める) は `server/` 内のユーティリティに追加するか、現場で `if t.detachable { info.Detachable = 1 }` のようにインライン化する。

- [ ] **Step 4: テスト追加** (`server/taskstore_test.go`)

各遷移を 1 ケースずつ assert:

```go
func TestTaskStore_DetachedTransitions(t *testing.T) {
	store := NewTaskStore()
	id := store.Create("/repo", "", protocol.TaskKind_Interactive, /*...*/)
	store.SetRunning(id, /*...*/)

	// Running → Detached
	if err := store.SetDetached(id); err != nil {
		t.Fatalf("SetDetached: %v", err)
	}
	if got := store.Get(id).Status; got != protocol.TaskStatus_Detached {
		t.Fatalf("got %v want Detached", got)
	}

	// Detached → Running (re-attach)
	if err := store.SetRunning(id, /*...*/); err != nil {
		t.Fatalf("SetRunning re-attach: %v", err)
	}

	// Running → Detached → Cancelled
	store.SetDetached(id)
	if err := store.SetCancelled(id); err != nil {
		t.Fatalf("SetCancelled: %v", err)
	}
}
```

`SetDetached` メソッドが TaskStore に無ければ追加する (Status 更新 + WAL 書き込み)。

- [ ] **Step 5: テスト pass + commit**

```bash
go test ./server/ -run TestTaskStore -v
git add server/taskstore.go server/taskstore_test.go
git commit -m "server: TaskStore Detached state transitions + TaskInfo fields"
```

---

### Task 6: `handleOpenInteractive` を Detachable=1 で SessionMux 経路に分岐

**Files:**
- Modify: `server/task_handler.go`

既存 `handleOpenInteractive` (`task_handler.go:328`) は detachable=0 のとき従来 `spliceBidi` パスを維持。`req.Detachable == 1` のとき:
1. `OpenExecRunnerRequest.Detachable = 1` を runner に伝える
2. `spliceBidi(tuiStream, runnerStream, taskIDHex)` の代わりに `SessionMux` を生成・登録・Run
3. `tuiStream` を最初の Attach として install

- [ ] **Step 1: Server struct に SessionRegistry を持たせる**

`server/server.go` (既存 `Server` struct) に field を追加:

```go
type Server struct {
    // ...existing...
    sessionRegistry *SessionRegistry
}
```

`Run()` などの初期化箇所で `s.sessionRegistry = NewSessionRegistry()`。

- [ ] **Step 2: TaskHandler が SessionRegistry にアクセスできるように**

`TaskHandler` struct に `Sessions *SessionRegistry` を追加、`server.go` で wiring。

- [ ] **Step 3: handleOpenInteractive 分岐**

`server/task_handler.go:328` 付近、stream allocate 後の `spliceBidi(...)` 呼び出しを置き換え:

```go
// 既存:
//   go func() { spliceBidi(tuiStream, runnerStream, taskIDHex); ... }()
//
// 新:
if req.Detachable == 1 {
    ringSize := h.RingBufferSize
    if ringSize <= 0 {
        ringSize = 1 << 20 // 1 MiB default
    }
    mux := NewSessionMux(h.Ctx, taskIDHex, runnerStream, NewRingBuffer(ringSize))
    mux.SetHooks(
        func(id string) { h.Tasks.SetRunning(id, runner.ID) },     // onAttach
        func(id string) { h.Tasks.SetDetached(id) },                // onDetach
        func(id string) {
            h.Sessions.Remove(id)
            // Stop で onStop が呼ばれた時は status はもう terminal を期待。
            // runner からの TaskFinished が status=Succeeded/Failed を立てる。
        },
    )
    h.Sessions.Add(taskIDHex, mux)
    if err := mux.Attach(h.Ctx, tuiStream); err != nil {
        slog.Error("initial Attach failed", "task", taskIDHex, "err", err)
        mux.Stop()
        return errResp(protocol.OpenInteractiveStatus_InternalError)
    }
} else {
    // Existing legacy path: kill on disconnect.
    go func() {
        spliceBidi(tuiStream, runnerStream, taskIDHex)
        // 旧: runner SIGHUP ladder、TaskFinished 等
    }()
}
```

OpenExec 側にも `Detachable: req.Detachable` を伝える:

```go
oer := protocol.OpenExecRunnerRequest{
    TaskId:     /* ... */,
    AuthTicket: ticket,
    /* ... */,
    Detachable: req.Detachable,
}
```

- [ ] **Step 4: Build + 既存テスト pass を確認**

```bash
go build ./...
go test ./server/ -run TestOpenInteractive -v
```

既存 OpenInteractive テスト (detachable=0 のケース) が通ること。

- [ ] **Step 5: 新 detachable=1 経路のテスト追加** (`server/task_handler_test.go` または resume_test 同階層)

```go
func TestHandleOpenInteractive_DetachableSpawnsSessionMux(t *testing.T) {
    // setup fake server, fake runner connection
    // submit OpenInteractiveRequest with Detachable=1
    // assert: Sessions.Get(taskID) != nil
    // assert: TaskStatus is Running
}
```

- [ ] **Step 6: Commit**

```bash
go test ./server/ -v
git add server/task_handler.go server/task_handler_test.go server/server.go
git commit -m "server: spawn SessionMux when OpenInteractive(Detachable=1)"
```

---

### Task 7: `handleAttachSession` + dispatch wire-in

**Files:**
- Modify: `server/task_handler.go` (新メソッド)
- Modify: `server/dispatch.go` (attach_session 分岐)

- [ ] **Step 1: handleAttachSession メソッド**

`server/task_handler.go` に追加:

```go
func (h *TaskHandler) handleAttachSession(conn ConnHandle, req *protocol.AttachSessionRequest) protocol.AttachSessionResponse {
    errResp := func(s protocol.AttachSessionStatus) protocol.AttachSessionResponse {
        return protocol.AttachSessionResponse{Status: s}
    }
    idHex := hex.EncodeToString(req.TaskId.Id[:])

    info, ok := h.Tasks.Get(idHex)
    if !ok {
        return errResp(protocol.AttachSessionStatus_NotFound)
    }
    if info.Kind != protocol.TaskKind_Interactive {
        return errResp(protocol.AttachSessionStatus_NotInteractive)
    }
    if !info.Detachable {
        return errResp(protocol.AttachSessionStatus_NotDetachable)
    }
    switch info.Status {
    case protocol.TaskStatus_Succeeded, protocol.TaskStatus_Failed, protocol.TaskStatus_Cancelled:
        return errResp(protocol.AttachSessionStatus_AlreadyTerminal)
    }
    mux := h.Sessions.Get(idHex)
    if mux == nil {
        return errResp(protocol.AttachSessionStatus_RunnerUnreachable)
    }

    // Allocate a new bidi stream toward this client.
    tuiStream, err := conn.OpenBidirectionalStream(h.Ctx)
    if err != nil {
        slog.Error("AttachSession: open client stream", "task", idHex, "err", err)
        return errResp(protocol.AttachSessionStatus_InternalError)
    }

    replayBytes := uint64(mux.RingBufferLen())
    if err := mux.Attach(h.Ctx, tuiStream); err != nil {
        _ = tuiStream.CloseBoth()
        slog.Error("AttachSession: mux.Attach", "task", idHex, "err", err)
        return errResp(protocol.AttachSessionStatus_InternalError)
    }

    return protocol.AttachSessionResponse{
        Status:      protocol.AttachSessionStatus_Ok,
        StreamId:    uint64(tuiStream.ID()),
        ReplayBytes: replayBytes,
    }
}
```

- [ ] **Step 2: dispatch wire-in** (`server/dispatch.go` `handleTaskControl`)

`TaskControlKind_OpenInteractive` の case の後に追加:

```go
case protocol.TaskControlKind_AttachSession:
    a := req.Attach()
    if a == nil {
        slog.Error("TaskHandler: AttachSession variant nil")
        return
    }
    aresp := h.handleAttachSession(conn, a)
    resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_AttachSession, RequestId: req.RequestId}
    resp.SetAttach(aresp)
    if err := conn.Send(&resp); err != nil { /* log */ }
```

- [ ] **Step 3: テスト追加** (`server/task_handler_test.go`)

各エラー path + Ok path を 1 ケースずつ。

- [ ] **Step 4: Commit**

```bash
go test ./server/ -run TestHandleAttachSession -v
git add server/task_handler.go server/dispatch.go server/task_handler_test.go
git commit -m "server: implement handleAttachSession + dispatch"
```

---

### Task 8: Server flags + idle TTL goroutine + restart-time Cancelled mark

**Files:**
- Modify: `cmd/harness-server/main.go`
- Modify: `server/server.go`

- [ ] **Step 1: flags 追加** (`cmd/harness-server/main.go`)

```go
ringSize := flag.Int64("detach-ring-buffer-size", 1<<20, "byte size of per-detached-session scrollback ring buffer")
idleTimeout := flag.Duration("detach-idle-timeout", 0, "auto-cancel detached sessions after this idle duration (0 = disabled)")
// ...
cfg.DetachRingBufferSize = *ringSize
cfg.DetachIdleTimeout = *idleTimeout
```

`server.Config` (`server/server.go`) に対応 field を追加。

- [ ] **Step 2: TaskHandler に RingBufferSize 伝播**

```go
type TaskHandler struct {
    /* ... */
    RingBufferSize int
}
// server.go の new TaskHandler 時に: th.RingBufferSize = int(cfg.DetachRingBufferSize)
```

- [ ] **Step 3: Restart-time Cancelled mark**

`server/server.go` の WAL replay 後 (既存の `s.tasks.ReplayEvents(events)` 直後):

```go
// Detached survivors cannot be restored: SessionMux state was in-memory.
for _, t := range s.tasks.List(0) {
    if t.Status == protocol.TaskStatus_Detached {
        s.tasks.SetCancelled(t.ID, "server restart")
    }
}
```

- [ ] **Step 4: idle TTL goroutine**

`server.go` 起動時、`cfg.DetachIdleTimeout > 0` の場合に背景 goroutine を起動:

```go
if s.cfg.DetachIdleTimeout > 0 {
    go s.runDetachIdleSweeper(ctx)
}
```

```go
func (s *Server) runDetachIdleSweeper(ctx context.Context) {
    interval := s.cfg.DetachIdleTimeout / 4
    if interval < 30*time.Second {
        interval = 30 * time.Second
    }
    t := time.NewTicker(interval)
    defer t.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case now := <-t.C:
            cutoff := now.Add(-s.cfg.DetachIdleTimeout).UnixNano()
            for _, info := range s.tasks.List(0) {
                if info.Status != protocol.TaskStatus_Detached {
                    continue
                }
                if info.DetachedAt > 0 && info.DetachedAt < uint64(cutoff) {
                    if mux := s.sessionRegistry.Get(info.ID); mux != nil {
                        mux.Stop()
                    }
                    s.tasks.SetCancelled(info.ID, "idle timeout")
                }
            }
        }
    }
}
```

`DetachedAt` は TaskStore で `SetDetached` 時に記録する (Task 5 で追加済の前提)。未追加なら Task 5 に戻って追加。

- [ ] **Step 5: テスト + commit**

```bash
go test ./server/ -v
go build ./...
git add cmd/harness-server/main.go server/server.go
git commit -m "server: detach config flags + idle sweeper + restart Cancelled mark"
```

---

### Task 9: Runner-side detachable logging (functional 変更なし)

**Files:**
- Modify: `runner/session.go:handleOpenExec`

- [ ] **Step 1: oer.Detachable をログに残す**

`runner/session.go:379` 付近、`taskAccepted` 直後に:

```go
log = log.With("detachable", oer.Detachable == 1)
log.Info("OpenExec received", "task_id", taskIDHex, "repo", repoPath, "detachable", oer.Detachable == 1)
```

`taskEntry` 構造体 (mu 内部) にも `detachable bool` を保存 (将来の cancel/log で使う):

```go
s.tasks[taskIDHex] = &taskEntry{cancel: cancel, repoPath: repoPath, detachable: oer.Detachable == 1}
```

`taskEntry` struct 定義箇所に `detachable bool` を追加。

- [ ] **Step 2: build + commit**

```bash
go build ./...
git add runner/session.go
git commit -m "runner: record detachable flag from OpenExec for diagnostics"
```

---

### Task 10: Client-side `attach.go` + OpenInteractive detachable param

**Files:**
- Create: `cli/attach.go`
- Modify: `cli/open_interactive_native.go`

- [ ] **Step 1: OpenInteractive に detachable param を追加**

`cli/open_interactive_native.go:41` の `OpenInteractiveWithSelectorAndArgs` に param を追加:

```go
func (c *Client) OpenInteractiveWithSelectorAndArgs(
    ctx context.Context, repoPath string,
    sel protocol.RunnerSelector, extraArgs []string,
    resumeTaskID string, detachable bool,
) (*agentexec.CommandExecutionStream, string, error) {
    // ...
    oi := protocol.OpenInteractiveRequest{}
    oi.SetRepoPath([]byte(repoPath))
    oi.Selector = sel
    oi.ExtraArgs = protocol.ClaudeArgsFromStrings(extraArgs)
    if resumeTaskID != "" { /* ... */ }
    if detachable { oi.Detachable = 1 }
    // ...
}
```

既存の `OpenInteractive`、`OpenInteractiveWithSelector` も signature 追従、内部で `false` を渡す。

```go
func (c *Client) OpenInteractive(ctx context.Context, repoPath string) (*agentexec.CommandExecutionStream, string, error) {
    return c.OpenInteractiveWithSelectorAndArgs(ctx, repoPath, /*selector*/, nil, "", false)
}
```

呼び出し元 (`cmd/harness-cli/main.go:interactive` や `tui/`) も新 signature 経由で `false` を渡すようまとめて更新。

- [ ] **Step 2: AttachSession round-trip 用 client メソッド** (`cli/attach.go`)

```go
package cli

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"

	agentexec "github.com/on-keyday/agent-harness/exec"
	"github.com/on-keyday/agent-harness/peer"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/agent-harness/trsf"
)

// AttachSession は detachable session に再接続する。
func (c *Client) AttachSession(ctx context.Context, taskIDHex string) (*agentexec.CommandExecutionStream, uint64, error) {
	tid, err := parseTaskIDHex(taskIDHex)
	if err != nil {
		return nil, 0, fmt.Errorf("AttachSession: parse id: %w", err)
	}
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AttachSession}
	req.SetAttach(protocol.AttachSessionRequest{TaskId: tid})

	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, 0, err
	}
	if resp.Kind != protocol.TaskControlKind_AttachSession {
		return nil, 0, fmt.Errorf("expected AttachSession response, got %v", resp.Kind)
	}
	a := resp.Attach()
	if a == nil {
		return nil, 0, fmt.Errorf("AttachSession response variant missing")
	}
	if err := attachStatusError(taskIDHex, a.Status); err != nil {
		return nil, 0, err
	}

	st := peer.WaitForBidirectionalStream(ctx, c.Transport(), trsf.StreamID(a.StreamId))
	if st == nil {
		return nil, 0, fmt.Errorf("attach stream %d not visible", a.StreamId)
	}
	return agentexec.NewCommandExecutionStream(st), a.ReplayBytes, nil
}

// SessionAttach is the high-level helper used by `harness-cli session attach`.
func (c *Client) SessionAttach(ctx context.Context, taskIDHex string) (string, error) {
	stream, replayBytes, err := c.AttachSession(ctx, taskIDHex)
	if err != nil {
		return taskIDHex, err
	}
	defer stream.Close()
	if replayBytes > 0 {
		fmt.Fprintf(os.Stderr, "harness-cli: attached to %s (replaying %d bytes)\n", taskIDHex, replayBytes)
	} else {
		fmt.Fprintf(os.Stderr, "harness-cli: attached to %s\n", taskIDHex)
	}
	if err := stream.RemoteShell(); err != nil {
		return taskIDHex, err
	}
	return taskIDHex, nil
}

func attachStatusError(taskID string, status protocol.AttachSessionStatus) error {
	switch status {
	case protocol.AttachSessionStatus_Ok:
		return nil
	case protocol.AttachSessionStatus_NotFound:
		return fmt.Errorf("attach failed: not_found %s", taskID)
	case protocol.AttachSessionStatus_NotInteractive:
		return fmt.Errorf("attach failed: not_interactive (task is oneshot)")
	case protocol.AttachSessionStatus_NotDetachable:
		return fmt.Errorf("attach failed: not_detachable (use 'interactive --resume' for legacy)")
	case protocol.AttachSessionStatus_AlreadyTerminal:
		return fmt.Errorf("attach failed: already_terminal (use 'interactive --resume' on the task id)")
	case protocol.AttachSessionStatus_RunnerUnreachable:
		return fmt.Errorf("attach failed: runner_unreachable")
	case protocol.AttachSessionStatus_InternalError:
		return fmt.Errorf("attach failed: internal_error")
	default:
		return fmt.Errorf("attach failed: status=%d", status)
	}
}
```

`parseTaskIDHex` は既存 helper (`cli/open_interactive_native.go` 内) を流用。

- [ ] **Step 3: build**

```bash
go build ./...
```

呼び出し元 build エラーは全て新 signature への追従で潰す (`false` を末尾に追加)。

- [ ] **Step 4: commit**

```bash
git add cli/attach.go cli/open_interactive_native.go cli/open_interactive_wasm.go cmd/harness-cli/*.go tui/*.go
git commit -m "cli: add AttachSession round-trip + detachable param to OpenInteractive"
```

`cli/open_interactive_wasm.go` も同 signature に揃える。

---

### Task 11: `session` subcommand group + `cmd/harness-cli` dispatch

**Files:**
- Create: `cli/agent/session.go`
- Modify: `cmd/harness-cli/main.go`

`session` は user 向け subcommand なので `cli/agent/` よりも `cli/` 直下 (もしくは `cmd/harness-cli/` 内のみ) のほうが命名整合性が高い。既存 `cli/agent/` は agent (= claude) 内部用なので、本タスクでは `cmd/harness-cli/session.go` に置くのを推奨。実装は同等。

- [ ] **Step 1: session.go** (`cmd/harness-cli/session.go`)

```go
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// runSession dispatches `harness-cli session <verb> ...`.
func runSession(args []string) error {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: harness-cli session <new|attach|ls|kill> ...")
		os.Exit(2)
	}
	verb := args[0]
	rest := args[1:]
	switch verb {
	case "new":
		return runSessionNew(rest)
	case "attach":
		return runSessionAttach(rest)
	case "ls":
		return runSessionLs(rest)
	case "kill":
		return runSessionKill(rest)
	default:
		return fmt.Errorf("unknown session verb %q", verb)
	}
}

func runSessionNew(args []string) error {
	fs := flag.NewFlagSet("session new", flag.ExitOnError)
	repo := fs.String("repo", "", "repo path")
	runnerSel := fs.String("runner", "", "runner selector (host / ip / id-prefix)")
	fs.Parse(args)
	if *repo == "" {
		return fmt.Errorf("--repo required")
	}
	ctx := context.Background()
	c, err := dialDefault(ctx) // 既存 helper 使用
	if err != nil {
		return err
	}
	defer c.Close()
	sel := selectorFromString(*runnerSel) // 既存 helper
	id, err := c.InteractiveWithSelectorAndArgs(ctx, *repo, sel, nil, "", true /*detachable*/)
	if err != nil {
		return err
	}
	fmt.Printf("session %s ended\n", id)
	return nil
}

func runSessionAttach(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: session attach <id>")
	}
	id := args[0]
	ctx := context.Background()
	c, err := dialDefault(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.SessionAttach(ctx, id); err != nil {
		return err
	}
	return nil
}

func runSessionLs(args []string) error {
	ctx := context.Background()
	c, err := dialDefault(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	tasks, err := c.List(ctx)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	for _, t := range tasks {
		if t.Kind != protocol.TaskKind_Interactive || t.Detachable == 0 {
			continue
		}
		_ = enc.Encode(map[string]any{
			"id":                fmt.Sprintf("%x", t.Id.Id),
			"status":            t.Status.String(),
			"is_attached":       t.IsAttached == 1,
			"repo":              string(t.RepoPath),
			"runner":            fmt.Sprintf("%x", t.AssignedTo),
			"created_at":        t.CreatedAt,
			"started_at":        t.StartedAt,
			"ring_buffer_bytes": t.RingBufferBytes,
		})
	}
	return nil
}

func runSessionKill(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: session kill <id>")
	}
	return runCancelByID(args[0]) // 既存 cancel ロジック流用
}
```

`dialDefault` / `selectorFromString` / `runCancelByID` は既存 `cmd/harness-cli/` 内 helper。同 file 内に該当関数が無い場合は既存 `submit.go` / `cancel.go` の patterns を参照して同等の helper を作る。

- [ ] **Step 2: main.go dispatch 追加**

`cmd/harness-cli/main.go` の subcommand switch に:

```go
case "session":
    if err := runSession(os.Args[2:]); err != nil {
        fmt.Fprintln(os.Stderr, err)
        os.Exit(1)
    }
```

- [ ] **Step 3: smoke build + commit**

```bash
go build ./...
./bin/harness-cli session ls
git add cmd/harness-cli/session.go cmd/harness-cli/main.go
git commit -m "harness-cli: add session subcommand (new/attach/ls/kill)"
```

---

### Task 12: TUI で Detached 表示 + 新キーバインド

**Files:**
- Modify: `tui/tasks.go` (or whichever file renders the task list — confirm via grep)

- [ ] **Step 1: 既存 tasks 描画位置を特定**

```bash
grep -rn 'TaskStatus_\|"Running"\|"Queued"' tui/ | head
```

該当ファイル (たいてい `tui/tasks.go` か `tui/styles.go`) の status → 表示文字列マップに `Detached` を追加。色は `Yellow` か `Cyan` 等の neutral。

- [ ] **Step 2: 既存 interactive キーバインド分岐**

`tui/app.go` (or interactive entry handler) で、選択中タスクが `Detached` で `Detachable=true` の場合は `AttachSession` 経由を呼ぶ。それ以外は既存 `interactive` パスを維持。

- [ ] **Step 3: `S` (大文字) キーで `session new` を起こすバインド追加**

既存の `s` (= submit popup) と区別。`S` で repo prompt → `Interactive(... detachable=true)` を叩く。実装方針は既存 `s` (popup) のコピー + detachable=true 渡し。

- [ ] **Step 4: build + smoke**

```bash
go build ./...
./bin/harness-tui --help
```

実機確認は手動 (PR レビュー時に user が触る)。

- [ ] **Step 5: commit**

```bash
git add tui/
git commit -m "tui: render Detached status + add S keybind for session new"
```

---

### Task 13: `testdata/fake-claude-loud.sh` + Integration test

**Files:**
- Create: `testdata/fake-claude-loud.sh`
- Create: `integration/session_detach_test.go`

- [ ] **Step 1: fake-claude-loud.sh**

```sh
#!/usr/bin/env bash
# Emit ~5 MiB to overflow the default 1 MiB ring buffer.
yes "loud line $(date -u +%s.%N)" | head -c 5000000
echo
sleep 0.1
```

```bash
chmod +x testdata/fake-claude-loud.sh
```

- [ ] **Step 2: integration test** (`integration/session_detach_test.go`, build tag `integration`)

```go
//go:build integration

package integration_test

import (
	"context"
	"os/exec"
	"testing"
	"time"
	// ...
)

func TestSessionDetachReattach(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	srv := startServer(t, ctx)
	defer srv.Close()
	rnr := startRunner(t, ctx, srv, "--claude-bin", "../testdata/fake-claude.sh")
	defer rnr.Close()
	c1 := dialClient(t, ctx, srv)
	defer c1.Close()

	// 1. session new
	streamA, taskID, err := c1.OpenInteractiveWithSelectorAndArgs(ctx, repo, /*sel*/, nil, "", true)
	if err != nil { t.Fatal(err) }
	go streamA.RemoteShell() // splice in goroutine
	time.Sleep(200 * time.Millisecond)

	// 2. fake-claude wrote initial output → ring buffer
	// 3. detach (close client stream)
	_ = streamA.Close()
	waitForStatus(t, srv, taskID, protocol.TaskStatus_Detached)

	// 4. fake-claude continues to write while detached (depending on fake-claude.sh impl, may be limited)

	// 5. reattach
	c2 := dialClient(t, ctx, srv)
	defer c2.Close()
	streamB, replay, err := c2.AttachSession(ctx, taskID)
	if err != nil { t.Fatal(err) }
	if replay == 0 {
		t.Fatal("expected non-zero replay bytes")
	}
	// drain a bit then close
	_ = streamB.Close()

	// 6. takeover: third client attaches, verify previous closed
	c3 := dialClient(t, ctx, srv)
	defer c3.Close()
	_, _, err = c3.AttachSession(ctx, taskID)
	if err != nil { t.Fatal(err) }
	// previous streamB.Read() should now return EOF
}

func TestSessionDetach_RingBufferWrap(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	srv := startServer(t, ctx, "--detach-ring-buffer-size=1024")
	defer srv.Close()
	rnr := startRunner(t, ctx, srv, "--claude-bin", "../testdata/fake-claude-loud.sh")
	defer rnr.Close()
	c := dialClient(t, ctx, srv)
	defer c.Close()

	streamA, taskID, err := c.OpenInteractiveWithSelectorAndArgs(ctx, repo, /*sel*/, nil, "", true)
	if err != nil { t.Fatal(err) }
	go streamA.RemoteShell()
	time.Sleep(500 * time.Millisecond)
	_ = streamA.Close()

	c2 := dialClient(t, ctx, srv)
	defer c2.Close()
	_, replay, err := c2.AttachSession(ctx, taskID)
	if err != nil { t.Fatal(err) }
	if replay != 1024 {
		t.Fatalf("expected ring full (replay=1024), got %d", replay)
	}
}
```

`startServer / startRunner / dialClient / waitForStatus` 等の helper は既存 `integration/` の patterns を流用。なければ新規作成 (既存 integration test を grep で確認)。

- [ ] **Step 3: 実行確認**

```bash
go test -tags integration ./integration/... -run TestSessionDetach -v
```

Expected: 2 tests pass。

- [ ] **Step 4: commit**

```bash
git add testdata/fake-claude-loud.sh integration/session_detach_test.go
git commit -m "test: integration coverage for session detach/reattach + ring wrap"
```

---

## Verification

### 全 unit / integration test

```bash
go test ./... -race
go test -tags integration ./integration/... -v
```

期待: 全 pass。

### Manual smoke (3-host config)

`project_deployment_topology` 通り Pi server / gmkhost runner / Windows client。

1. server 起動 (Pi):
   ```bash
   bin/harness-server --listen :8539 --data-dir ./harness-data
   ```
2. runner 起動 (gmkhost):
   ```bash
   bin/agent-runner --server-cid 'ws:RASPI:8539-*' --roots /repo --max-tasks 4
   ```
3. Windows client から:
   ```bash
   harness-cli session new --repo /repo
   ```
4. claude と数往復、Windows ターミナルを閉じる。
5. 別端末 (例: gmkhost で SSH したシェル) から:
   ```bash
   harness-cli session ls
   harness-cli session attach <id>
   ```
6. `[replaying NNN bytes]` → 続きの対話可。`exit` 等で claude 自体を終了させると status=Succeeded、再度 attach は `already_terminal`。

### 反例チェック

- 既存 `harness-cli interactive` は **kill-on-disconnect** のままであること。`session new` から起動したものだけが Detached になる。
- 旧 client (= detachable=0 で OpenInteractive) からの session が `attach_session` で `not_detachable` を返すこと。
- server 再起動: 起動前に Detached だった task が起動後 Cancelled になっていること。

---

## Spec coverage check

| Spec section | 対応 task |
|---|---|
| §4 UX (subcommands) | Task 11 |
| §4.2 TUI | Task 12 |
| §5 State machine | Task 5 (TaskStore) + Task 6/7 (transition triggers) |
| §6 Architecture | Task 3 (SessionMux core) |
| §6.3 Ring buffer | Task 2 (RingBuffer) |
| §7 Protocol | Task 1 (schema) |
| §8.1 Server-side | Task 3, 4, 6, 7, 8 |
| §8.2 Runner-side | Task 9 |
| §8.3 Client-side | Task 10, 11 |
| §9 Edge cases | Task 6/7/8 (各 status / runner unreachable / takeover / cancel race / restart Cancelled) |
| §10 Error handling | Task 7 (status enum mapping) |
| §11.1 Unit tests | Task 2/3/4/5 内 |
| §11.2 Integration | Task 13 |
| §11.3 Loud test | Task 13 + testdata/fake-claude-loud.sh |
| §11.4 Manual smoke | Verification 節 |
