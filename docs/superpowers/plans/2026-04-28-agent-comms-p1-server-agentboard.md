# Agent Comms P1: Wire Protocol + Agentboard Server Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** harness server に agentboard component を追加し、agent (claude) 間メッセージ交換のための新 wire protocol (`AgentMessage` payload kind, ticket-authenticated Hello, topic-based send/subscribe/wait/inbox) と in-memory ring + TTL retention を実装する。Spec §4–§8。

**Architecture:** 既存 `pubsub` layer は不変条件 "ephemeral fan-out" を保持。agentboard は server 内の独立 component で、別 wire payload kind (`agent_message`) で routing。auth ticket は task ごとに server が発行し、`AssignTask` / `OpenExecRunnerRequest` で runner に渡す。agent CLI からの Hello は `(runner_id, task_id, ticket)` を server registry と照合してから受け入れる。

**Tech Stack:** Go 1.25.7、`brgen` / ebm2go (`.bgn` → Go codec、`make protoregen` で再生成)、既存 `objproto` / `peer` / `trsf` / `wire` / `runner/protocol`。Spec: `docs/superpowers/specs/2026-04-28-agent-comms-design.md`。

---

## Reference for implementers

### 既存コードの前提

このプランは "multi-task per runner & multi-roots" がマージ済の main branch から始まる前提。具体的には:
- `runner/protocol/message.bgn` に `RunnerHello` 内の `hostname`, `max_tasks`, `allowed_roots` フィールドが揃っている
- `runner.Config` に `AllowedRoots []string`, `MaxTasks int`, `Hostname string` がある
- `Session.tasks map[string]*taskEntry` で multi-task lifecycle を管理

### 用語

- **agentboard**: server 内の新 component (このプランで作る)
- **agent CLI**: P3 で作る `harness agent ...` subcommand 群。本プランでは "agent CLI からの hello / send" として Go test で再現する
- **ticket**: 16 byte random、task ごとに server が発行、runner が env 経由で claude に渡す

### .bgn → Go 再生成

`.bgn` を編集したら `make protoregen ARGS=path/to/file.bgn` で対応 `.go` を再生成する。`make protoregen ARGS=--all` で全再生成も可能。再生成後 git diff で内容確認すること。

### topic 名空間

- 既存 pubsub: `task.<id>.log` 等の "." 区切り
- agentboard: `task/<id>/dispatch`, `conv/<id>/messages` 等の "/" 区切り
- 両者は別 dispatch path、混ざらない

### 既存 server entry points

- `server/server.go`: WS 接続を受けて `pc.SetOnControl` で payload kind ごとに dispatch
- `server/dispatch.go`: TaskQueue + RunnerRegistry の交差点 (このプランでは ticket 登録/破棄 hook を追加)
- `server/task_handler.go`: `handleSubmit`, `handleOpenInteractive`、`handleGetTaskLog` 等の TaskControl 処理
- `server/runner_handler.go`: RunnerControl の Hello / TaskAccepted / TaskFinished 処理 (このプランで TaskFinished hook 追加)

---

## File structure

### Create

```
agentboard/
  agentboard.bgn          # AgentBridge wire schema (Hello, Send, Wait, Inbox, ...)
  agentboard.go           # generated from .bgn (do not edit)
  topic.go                # Topic struct: ring buffer + last-publish timestamp
  topic_test.go
  registry.go             # ticket registry: (runner_id, task_id) → ticket
  registry_test.go
  board.go                # Board: Send / Subscribe / Wait / Inbox APIs, TTL evict goroutine
  board_test.go
  conn.go                 # ConnState: per-connection client state (subscribed patterns, cursor)
  conn_test.go
  e2e_test.go             # Go-level e2e: 2 simulated agent CLIs talking through Board
```

### Modify

```
trsf/wire/stream.bgn                  # add `agent_message` to ApplicationPayloadKind
trsf/wire/stream.go                   # regenerated
runner/protocol/message.bgn           # add auth_ticket :[16]u8 to AssignTask, OpenExecRunnerRequest
runner/protocol/message.go            # regenerated
server/server.go                      # wire AgentBoard into Server struct + ApplicationPayloadKind dispatch
server/dispatch.go                    # ticket lifecycle: register on TryDispatch, revoke on TaskFinished hook
server/runner_handler.go              # TaskFinished hook: call agentboard.RevokeTicket
cmd/harness-server/main.go            # new flags: --agentboard-ring, --agentboard-ttl, --agentboard-max-topics, --agentboard-max-payload
```

---

## Tasks

### Task 1: Add `agent_message` to wire payload kind

**Files:**
- Modify: `trsf/wire/stream.bgn:62-74`
- Regen: `trsf/wire/stream.go`

- [ ] **Step 1: Edit `trsf/wire/stream.bgn` enum**

`enum ApplicationPayloadKind` の末尾 (`close` の後) に `agent_message` を追加:

```
enum ApplicationPayloadKind:
    :u8
    ping
    pong
    stream_data
    stream_cancel
    stream_ack
    stream_window_update
    pubsub
    task_control
    relay_control
    runner_control
    close
    agent_message     # NEW
```

- [ ] **Step 2: Regenerate Go**

```bash
make protoregen ARGS=trsf/wire/stream.bgn
```

Expected: `trsf/wire/stream.go` が更新され `ApplicationPayloadKind_AgentMessage ApplicationPayloadKind = 11` が追加される。

- [ ] **Step 3: Verify build**

```bash
go build ./...
```

Expected: クリーンに通る。

- [ ] **Step 4: Commit**

```bash
git add trsf/wire/stream.bgn trsf/wire/stream.go
git commit -m "wire: add agent_message ApplicationPayloadKind for agentboard"
```

---

### Task 2: Add `auth_ticket` field to AssignTask and OpenExecRunnerRequest

**Files:**
- Modify: `runner/protocol/message.bgn:37-53`
- Regen: `runner/protocol/message.go`

- [ ] **Step 1: Edit `runner/protocol/message.bgn`**

`AssignTask` と `OpenExecRunnerRequest` に `auth_ticket :[16]u8` を追加:

```
format AssignTask:
    task_id :TaskID
    auth_ticket :[16]u8
    repo_path_len :u16
    repo_path :[repo_path_len]u8
    prompt :[..]u8

format OpenExecRunnerRequest:
    task_id :TaskID
    auth_ticket :[16]u8
    repo_path_len :u16
    repo_path :[repo_path_len]u8
    stream_id :u64
```

ポジションは `task_id` の直後に統一する。

- [ ] **Step 2: Regenerate**

```bash
make protoregen ARGS=runner/protocol/message.bgn
```

Expected: `AssignTask.AuthTicket [16]uint8` フィールドが生成される。

- [ ] **Step 3: Build (will fail at use sites)**

```bash
go build ./...
```

Expected: `server/dispatch.go` 等で AssignTask 生成箇所が AuthTicket 未指定で warning なしで通る (Go zero value)。runner 側の deserialize は新フィールドを読むようになる。**この Step では既存 caller の修正は不要 — Task 8 でまとめて入れる。**

- [ ] **Step 4: Commit**

```bash
git add runner/protocol/message.bgn runner/protocol/message.go
git commit -m "protocol: add auth_ticket to AssignTask, OpenExecRunnerRequest"
```

---

### Task 3: Create `agentboard/agentboard.bgn` schema

**Files:**
- Create: `agentboard/agentboard.bgn`
- Generated: `agentboard/agentboard.go`

- [ ] **Step 1: Write the .bgn file**

```
config.go.package = "agentboard"

import "../runner/protocol/message.bgn"

enum AgentMessageKind:
    :u8
    hello
    hello_response
    send
    send_response
    subscribe
    subscribe_response
    unsubscribe
    wait
    wait_response
    inbox
    inbox_response
    deliver

format AgentBridgeHello:
    runner_id :RunnerID
    task_id :TaskID
    auth_ticket :[16]u8
    hostname_len :u8
    hostname :[hostname_len]u8

enum HelloStatus:
    :u8
    ok
    bad_ticket
    unknown_task
    runner_mismatch

format AgentBridgeHelloResponse:
    status :HelloStatus

format SendRequest:
    request_id :u32
    topic_len :u16
    topic :[topic_len]u8
    payload_len :u32
    payload :[payload_len]u8

enum SendStatus:
    :u8
    ok
    payload_too_large
    too_many_topics
    bad_frame

format SendResponse:
    request_id :u32
    status :SendStatus
    seq :u64

format SubscribeRequest:
    request_id :u32
    pattern_len :u16
    pattern :[pattern_len]u8

format UnsubscribeRequest:
    request_id :u32
    pattern_len :u16
    pattern :[pattern_len]u8

enum SubscribeStatus:
    :u8
    ok
    bad_pattern

format SubscribeResponse:
    request_id :u32
    status :SubscribeStatus

format DeliveredMessage:
    seq :u64
    topic_len :u16
    topic :[topic_len]u8
    payload_len :u32
    payload :[payload_len]u8

format WaitRequest:
    request_id :u32
    pattern_len :u16
    pattern :[pattern_len]u8
    since :u64
    timeout_ms :u32

format WaitResponse:
    request_id :u32
    timed_out :u8
    next_cursor :u64
    msgs_len :u16
    msgs :[msgs_len]DeliveredMessage

format InboxRequest:
    request_id :u32
    since :u64

format InboxResponse:
    request_id :u32
    next_cursor :u64
    msgs_len :u16
    msgs :[msgs_len]DeliveredMessage

format AgentMessage:
    kind :AgentMessageKind
    match kind:
        AgentMessageKind.hello => hello :AgentBridgeHello
        AgentMessageKind.hello_response => hello_response :AgentBridgeHelloResponse
        AgentMessageKind.send => send :SendRequest
        AgentMessageKind.send_response => send_response :SendResponse
        AgentMessageKind.subscribe => subscribe :SubscribeRequest
        AgentMessageKind.subscribe_response => subscribe_response :SubscribeResponse
        AgentMessageKind.unsubscribe => unsubscribe :UnsubscribeRequest
        AgentMessageKind.wait => wait :WaitRequest
        AgentMessageKind.wait_response => wait_response :WaitResponse
        AgentMessageKind.inbox => inbox :InboxRequest
        AgentMessageKind.inbox_response => inbox_response :InboxResponse
        AgentMessageKind.deliver => deliver :DeliveredMessage
        .. => error("Unexpected agent message kind")
```

- [ ] **Step 2: Verify import resolves**

`import "../runner/protocol/message.bgn"` が動くか確認。`runner/protocol/message.bgn` 内の `format RunnerID`, `format TaskID` を再利用するための import。動かなければ same-file copy で代替。

```bash
make protoregen ARGS=agentboard/agentboard.bgn
```

Expected: import エラーなく `agentboard/agentboard.go` が生成される。エラーが出たら、import を消して `RunnerID` と `TaskID` を agentboard.bgn 内で再定義 (runner protocol と同形式) して再 regen。

- [ ] **Step 3: Verify build**

```bash
go build ./agentboard/...
```

Expected: pure schema package、依存少なく通る。

- [ ] **Step 4: Commit**

```bash
git add agentboard/agentboard.bgn agentboard/agentboard.go
git commit -m "agentboard: add wire schema (Hello/Send/Subscribe/Wait/Inbox)"
```

---

### Task 4: Topic struct + ring buffer

**Files:**
- Create: `agentboard/topic.go`
- Test: `agentboard/topic_test.go`

- [ ] **Step 1: Write failing test `agentboard/topic_test.go`**

```go
package agentboard

import (
	"testing"
	"time"
)

func TestTopic_AppendInRing(t *testing.T) {
	topic := newTopic("conv/x/messages", 4)
	for i := 0; i < 6; i++ {
		topic.append(uint64(i+1), []byte{byte(i)})
	}
	got := topic.since(0)
	if len(got) != 4 {
		t.Fatalf("ring should hold last 4 only, got %d", len(got))
	}
	if got[0].Seq != 3 || got[3].Seq != 6 {
		t.Errorf("ring oldest/newest seq = %d/%d, want 3/6", got[0].Seq, got[3].Seq)
	}
}

func TestTopic_SinceFiltersByCursor(t *testing.T) {
	topic := newTopic("topic/foo", 8)
	for i := uint64(1); i <= 5; i++ {
		topic.append(i, []byte{byte(i)})
	}
	got := topic.since(2)
	if len(got) != 3 {
		t.Fatalf("since=2 should yield seq 3,4,5, got len=%d", len(got))
	}
	if got[0].Seq != 3 {
		t.Errorf("first seq = %d, want 3", got[0].Seq)
	}
}

func TestTopic_LastPublishedAtUpdates(t *testing.T) {
	topic := newTopic("status/x/y", 4)
	t0 := time.Now()
	topic.append(1, []byte("a"))
	if topic.lastPublishedAt.Before(t0) {
		t.Error("lastPublishedAt did not update after append")
	}
}
```

- [ ] **Step 2: Run test (fails, struct undefined)**

```bash
go test ./agentboard/ -run TestTopic -v
```

Expected: build error: `undefined: newTopic`.

- [ ] **Step 3: Implement `agentboard/topic.go`**

```go
package agentboard

import (
	"sync"
	"time"
)

// RetainedMessage is one entry in a topic ring buffer.
type RetainedMessage struct {
	Seq     uint64
	Topic   string
	Payload []byte
}

// topic holds a bounded ring of recent messages plus metadata used for TTL eviction.
type topic struct {
	mu              sync.Mutex
	name            string
	cap             int
	ring            []RetainedMessage // len(ring) <= cap; oldest first
	lastPublishedAt time.Time
}

func newTopic(name string, cap int) *topic {
	return &topic{name: name, cap: cap, ring: make([]RetainedMessage, 0, cap)}
}

func (t *topic) append(seq uint64, payload []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lastPublishedAt = time.Now()
	if len(t.ring) == t.cap {
		copy(t.ring, t.ring[1:])
		t.ring = t.ring[:t.cap-1]
	}
	t.ring = append(t.ring, RetainedMessage{
		Seq:     seq,
		Topic:   t.name,
		Payload: append([]byte(nil), payload...),
	})
}

// since returns retained messages with Seq > sinceSeq, in ascending order.
func (t *topic) since(sinceSeq uint64) []RetainedMessage {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]RetainedMessage, 0, len(t.ring))
	for _, m := range t.ring {
		if m.Seq > sinceSeq {
			out = append(out, m)
		}
	}
	return out
}
```

- [ ] **Step 4: Run test (passes)**

```bash
go test ./agentboard/ -run TestTopic -v
```

Expected: 3 tests pass.

- [ ] **Step 5: Commit**

```bash
git add agentboard/topic.go agentboard/topic_test.go
git commit -m "agentboard: topic with bounded ring buffer"
```

---

### Task 5: Ticket registry

**Files:**
- Create: `agentboard/registry.go`
- Test: `agentboard/registry_test.go`

- [ ] **Step 1: Write failing test `agentboard/registry_test.go`**

```go
package agentboard

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func mkID(b byte) protocol.RunnerID {
	return protocol.RunnerID{
		Transport: []byte("ws"),
		IpAddr:    []byte{127, 0, 0, 1},
		Port:      9000,
		UniqueNumber: uint16(b),
	}
}

func mkTask(b byte) protocol.TaskID {
	var t protocol.TaskID
	t.Id[0] = b
	return t
}

func TestRegistry_RegisterAndValidate(t *testing.T) {
	r := newRegistry()
	rid, tid := mkID(1), mkTask(1)
	var ticket [16]byte
	ticket[0] = 0xAA
	r.Register(rid, tid, ticket)

	if status := r.Validate(rid, tid, ticket); status != HelloStatusOk {
		t.Errorf("matching ticket → status=%v, want ok", status)
	}
	var bad [16]byte
	if status := r.Validate(rid, tid, bad); status != HelloStatusBadTicket {
		t.Errorf("wrong ticket → status=%v, want bad_ticket", status)
	}
}

func TestRegistry_UnknownTask(t *testing.T) {
	r := newRegistry()
	rid, tid := mkID(1), mkTask(2)
	var ticket [16]byte
	if status := r.Validate(rid, tid, ticket); status != HelloStatusUnknownTask {
		t.Errorf("unregistered → status=%v, want unknown_task", status)
	}
}

func TestRegistry_Revoke(t *testing.T) {
	r := newRegistry()
	rid, tid := mkID(1), mkTask(3)
	var ticket [16]byte
	ticket[0] = 0x55
	r.Register(rid, tid, ticket)
	r.Revoke(rid, tid)
	if status := r.Validate(rid, tid, ticket); status != HelloStatusUnknownTask {
		t.Errorf("after revoke → status=%v, want unknown_task", status)
	}
}
```

注: `HelloStatusOk` / `HelloStatusBadTicket` 等は agentboard.go の生成 enum (`HelloStatus_ok` 等) を package-internal alias で再エクスポートする。Step 3 で alias を追加。

- [ ] **Step 2: Run test (fails, undefined)**

```bash
go test ./agentboard/ -run TestRegistry -v
```

Expected: build error.

- [ ] **Step 3: Implement `agentboard/registry.go`**

```go
package agentboard

import (
	"crypto/subtle"
	"sync"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Aliases for shorter usage at call sites; map to brgen-generated HelloStatus_*.
const (
	HelloStatusOk             = HelloStatus_ok
	HelloStatusBadTicket      = HelloStatus_bad_ticket
	HelloStatusUnknownTask    = HelloStatus_unknown_task
	HelloStatusRunnerMismatch = HelloStatus_runner_mismatch
)

type ticketKey struct {
	runner string // RunnerID.String()
	task   string // hex(task_id)
}

type registry struct {
	mu      sync.Mutex
	tickets map[ticketKey][16]byte
}

func newRegistry() *registry {
	return &registry{tickets: make(map[ticketKey][16]byte)}
}

func keyOf(rid protocol.RunnerID, tid protocol.TaskID) ticketKey {
	return ticketKey{runner: runnerIDString(rid), task: hexTaskID(tid)}
}

func (r *registry) Register(rid protocol.RunnerID, tid protocol.TaskID, ticket [16]byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tickets[keyOf(rid, tid)] = ticket
}

func (r *registry) Revoke(rid protocol.RunnerID, tid protocol.TaskID) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.tickets, keyOf(rid, tid))
}

func (r *registry) Validate(rid protocol.RunnerID, tid protocol.TaskID, ticket [16]byte) HelloStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	want, ok := r.tickets[keyOf(rid, tid)]
	if !ok {
		return HelloStatusUnknownTask
	}
	if subtle.ConstantTimeCompare(want[:], ticket[:]) != 1 {
		return HelloStatusBadTicket
	}
	return HelloStatusOk
}
```

- [ ] **Step 4: Add `agentboard/ids.go` with helpers**

```go
package agentboard

import (
	"encoding/hex"
	"fmt"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func runnerIDString(r protocol.RunnerID) string {
	return fmt.Sprintf("%s:%s:%d-%d", string(r.Transport), formatIP(r.IpAddr), r.Port, r.UniqueNumber)
}

func formatIP(b []byte) string {
	switch len(b) {
	case 4:
		return fmt.Sprintf("%d.%d.%d.%d", b[0], b[1], b[2], b[3])
	case 16:
		return fmt.Sprintf("%x", b) // hex; good enough for keying
	default:
		return ""
	}
}

func hexTaskID(t protocol.TaskID) string {
	return hex.EncodeToString(t.Id[:])
}
```

- [ ] **Step 5: Run test (passes)**

```bash
go test ./agentboard/ -run TestRegistry -v
```

Expected: 3 tests pass.

- [ ] **Step 6: Commit**

```bash
git add agentboard/registry.go agentboard/registry_test.go agentboard/ids.go
git commit -m "agentboard: ticket registry with constant-time compare"
```

---

### Task 6: Board (Send / Subscribe / Wait / Inbox)

**Files:**
- Create: `agentboard/board.go`, `agentboard/conn.go`
- Test: `agentboard/board_test.go`

- [ ] **Step 1: Write failing test `agentboard/board_test.go`**

```go
package agentboard

import (
	"context"
	"testing"
	"time"
)

func TestBoard_SendThenInboxReturnsMessage(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()
	conn := b.Attach()
	defer b.Detach(conn)
	if err := b.Subscribe(conn, "topic/foo"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Send("topic/foo", []byte("hello")); err != nil {
		t.Fatal(err)
	}
	msgs, _ := b.Inbox(conn, 0)
	if len(msgs) != 1 || string(msgs[0].Payload) != "hello" {
		t.Fatalf("inbox = %+v, want one message 'hello'", msgs)
	}
}

func TestBoard_WaitBlocksUntilMessageArrives(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()
	conn := b.Attach()
	defer b.Detach(conn)
	_ = b.Subscribe(conn, "topic/bar")

	go func() {
		time.Sleep(20 * time.Millisecond)
		_, _ = b.Send("topic/bar", []byte("ping"))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	msgs, timedOut, _ := b.Wait(ctx, conn, "topic/bar", 0)
	if timedOut {
		t.Fatal("Wait timed out unexpectedly")
	}
	if len(msgs) != 1 || string(msgs[0].Payload) != "ping" {
		t.Fatalf("wait = %+v, want one message 'ping'", msgs)
	}
}

func TestBoard_WaitTimesOut(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()
	conn := b.Attach()
	defer b.Detach(conn)
	_ = b.Subscribe(conn, "topic/quiet")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, timedOut, _ := b.Wait(ctx, conn, "topic/quiet", 0)
	if !timedOut {
		t.Fatal("Wait should have timed out")
	}
}

func TestBoard_PayloadTooLargeRejected(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 4})
	defer b.Close()
	if _, err := b.Send("topic/big", []byte("toolong")); err == nil {
		t.Fatal("expected payload_too_large error")
	}
}
```

- [ ] **Step 2: Run test (fails)**

```bash
go test ./agentboard/ -run TestBoard -v
```

Expected: build error.

- [ ] **Step 3: Implement `agentboard/conn.go`**

```go
package agentboard

import (
	"sync"
)

// ConnState is per-attached-client subscription state.
type ConnState struct {
	mu       sync.Mutex
	patterns map[string]struct{} // exact topic strings (glob in future, exact-match in v1)
	notify   chan struct{}       // closed/pinged on relevant publish; lazy realloc per Wait
}

func newConnState() *ConnState {
	return &ConnState{patterns: make(map[string]struct{}), notify: make(chan struct{}, 1)}
}

func (c *ConnState) addPattern(p string) {
	c.mu.Lock()
	c.patterns[p] = struct{}{}
	c.mu.Unlock()
}

func (c *ConnState) removePattern(p string) {
	c.mu.Lock()
	delete(c.patterns, p)
	c.mu.Unlock()
}

func (c *ConnState) matches(topic string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.patterns[topic]
	return ok
}

func (c *ConnState) ping() {
	select {
	case c.notify <- struct{}{}:
	default:
	}
}
```

注: v1 では pattern は **exact match のみ**。glob (`task/*/result/*` 等) は v2。

- [ ] **Step 4: Implement `agentboard/board.go`**

```go
package agentboard

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	RingN      int
	TopicTTL   time.Duration
	MaxTopics  int
	MaxPayload int
}

type Board struct {
	cfg     Config
	mu      sync.Mutex
	topics  map[string]*topic
	conns   map[*ConnState]struct{}
	seq     atomic.Uint64
	reg     *registry
	stopCh  chan struct{}
	stopped bool
}

func New(cfg Config) *Board {
	b := &Board{
		cfg:    cfg,
		topics: make(map[string]*topic),
		conns:  make(map[*ConnState]struct{}),
		reg:    newRegistry(),
		stopCh: make(chan struct{}),
	}
	go b.evictLoop()
	return b
}

func (b *Board) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped {
		return
	}
	b.stopped = true
	close(b.stopCh)
}

// Registry returns the ticket registry for server lifecycle code to call.
func (b *Board) Registry() *registry { return b.reg }

func (b *Board) Attach() *ConnState {
	c := newConnState()
	b.mu.Lock()
	b.conns[c] = struct{}{}
	b.mu.Unlock()
	return c
}

func (b *Board) Detach(c *ConnState) {
	b.mu.Lock()
	delete(b.conns, c)
	b.mu.Unlock()
}

func (b *Board) Subscribe(c *ConnState, pattern string) error {
	if pattern == "" {
		return errors.New("empty pattern")
	}
	c.addPattern(pattern)
	return nil
}

func (b *Board) Unsubscribe(c *ConnState, pattern string) {
	c.removePattern(pattern)
}

var (
	ErrPayloadTooLarge = errors.New("agentboard: payload too large")
	ErrTooManyTopics   = errors.New("agentboard: too many topics")
)

func (b *Board) Send(topicName string, payload []byte) (uint64, error) {
	if len(payload) > b.cfg.MaxPayload {
		return 0, ErrPayloadTooLarge
	}
	b.mu.Lock()
	t, ok := b.topics[topicName]
	if !ok {
		if len(b.topics) >= b.cfg.MaxTopics {
			b.evictOldestTopicLocked()
			if len(b.topics) >= b.cfg.MaxTopics {
				b.mu.Unlock()
				return 0, ErrTooManyTopics
			}
		}
		t = newTopic(topicName, b.cfg.RingN)
		b.topics[topicName] = t
	}
	conns := make([]*ConnState, 0, len(b.conns))
	for c := range b.conns {
		conns = append(conns, c)
	}
	b.mu.Unlock()

	seq := b.seq.Add(1)
	t.append(seq, payload)

	for _, c := range conns {
		if c.matches(topicName) {
			c.ping()
		}
	}
	return seq, nil
}

// Inbox returns retained messages for all topics this conn is subscribed to,
// with Seq > since, plus the new cursor (max seq seen).
func (b *Board) Inbox(c *ConnState, since uint64) ([]RetainedMessage, uint64) {
	c.mu.Lock()
	patterns := make([]string, 0, len(c.patterns))
	for p := range c.patterns {
		patterns = append(patterns, p)
	}
	c.mu.Unlock()

	b.mu.Lock()
	all := make([]RetainedMessage, 0)
	for _, p := range patterns {
		if t, ok := b.topics[p]; ok {
			all = append(all, t.since(since)...)
		}
	}
	b.mu.Unlock()

	max := since
	for _, m := range all {
		if m.Seq > max {
			max = m.Seq
		}
	}
	return all, max
}

// Wait blocks until at least one message arrives on the given topic with seq > since,
// or until ctx is done. Returns (messages, timedOut, error).
func (b *Board) Wait(ctx context.Context, c *ConnState, topicName string, since uint64) ([]RetainedMessage, bool, error) {
	if !c.matches(topicName) {
		c.addPattern(topicName) // implicit subscribe for the wait window
	}
	for {
		b.mu.Lock()
		var msgs []RetainedMessage
		if t, ok := b.topics[topicName]; ok {
			msgs = t.since(since)
		}
		b.mu.Unlock()
		if len(msgs) > 0 {
			return msgs, false, nil
		}
		select {
		case <-c.notify:
			continue
		case <-ctx.Done():
			return nil, true, nil
		case <-b.stopCh:
			return nil, false, errors.New("board closed")
		}
	}
}

func (b *Board) evictLoop() {
	tick := time.NewTicker(b.cfg.TopicTTL / 6)
	defer tick.Stop()
	for {
		select {
		case <-b.stopCh:
			return
		case <-tick.C:
			b.evictExpiredTopics()
		}
	}
}

func (b *Board) evictExpiredTopics() {
	cutoff := time.Now().Add(-b.cfg.TopicTTL)
	b.mu.Lock()
	defer b.mu.Unlock()
	for name, t := range b.topics {
		if t.lastPublishedAt.Before(cutoff) {
			delete(b.topics, name)
		}
	}
}

func (b *Board) evictOldestTopicLocked() {
	var oldestName string
	var oldestT time.Time
	for n, t := range b.topics {
		if oldestName == "" || t.lastPublishedAt.Before(oldestT) {
			oldestName, oldestT = n, t.lastPublishedAt
		}
	}
	if oldestName != "" {
		delete(b.topics, oldestName)
	}
}
```

- [ ] **Step 5: Run test (passes)**

```bash
go test ./agentboard/ -run TestBoard -v
```

Expected: 4 tests pass.

- [ ] **Step 6: Commit**

```bash
git add agentboard/board.go agentboard/conn.go agentboard/board_test.go
git commit -m "agentboard: Board with Send/Subscribe/Wait/Inbox APIs"
```

---

### Task 7: Server wire dispatch routing

**Files:**
- Modify: `server/server.go`

- [ ] **Step 1: Read existing dispatch sketch**

```bash
grep -n "SetOnControl\|ApplicationPayloadKind\|TaskControl\|RunnerControl\|Pubsub" server/server.go
```

確認: 既存 Server には pc.SetOnControl で kind ごとに switch している部分があるはず。`agent_message` ケースを追加する。

- [ ] **Step 2: Add Board field to Server struct**

`server.go` 内 `type Server struct { ... }` に追加:

```go
type Server struct {
    // ... existing fields ...
    Board *agentboard.Board
}
```

- [ ] **Step 3: Add agent_message dispatch in OnControl handler**

`pc.SetOnControl(func(kind wire.ApplicationPayloadKind, payload []byte) { ... })` の switch に agent_message ケースを追加:

```go
case wire.ApplicationPayloadKind_AgentMessage:
    s.handleAgentMessage(pc, payload)
```

- [ ] **Step 4: Implement `handleAgentMessage` in `server/agent_handler.go` (new file)**

```go
package server

import (
    "context"
    "encoding/json"
    "log/slog"
    "time"

    "github.com/on-keyday/agent-harness/agentboard"
    "github.com/on-keyday/agent-harness/peer"
    "github.com/on-keyday/agent-harness/trsf/wire"
)

// agentConn keys per-peer state in the Server.
type agentConn struct {
    state    *agentboard.ConnState
    helloed  bool
}

func (s *Server) handleAgentMessage(pc *peer.Conn, payload []byte) {
    msg := &agentboard.AgentMessage{}
    if _, err := msg.Decode(payload); err != nil {
        slog.Warn("agent_message decode", "err", err)
        return
    }
    ac := s.getOrCreateAgentConn(pc)
    switch msg.Kind {
    case agentboard.AgentMessageKind_hello:
        s.agentHandleHello(pc, ac, msg.Hello())
    case agentboard.AgentMessageKind_send:
        s.agentHandleSend(pc, ac, msg.Send())
    case agentboard.AgentMessageKind_subscribe:
        s.agentHandleSubscribe(pc, ac, msg.Subscribe())
    case agentboard.AgentMessageKind_unsubscribe:
        s.agentHandleUnsubscribe(pc, ac, msg.Unsubscribe())
    case agentboard.AgentMessageKind_wait:
        go s.agentHandleWait(pc, ac, msg.Wait())
    case agentboard.AgentMessageKind_inbox:
        s.agentHandleInbox(pc, ac, msg.Inbox())
    }
}

func (s *Server) sendAgent(pc *peer.Conn, msg *agentboard.AgentMessage) {
    data := msg.MustAppend([]byte{byte(wire.ApplicationPayloadKind_AgentMessage)})
    _, _, _ = pc.Connection().SendMessage(data)
}

// hello / send / subscribe / unsubscribe / wait / inbox handler implementations
// follow; each constructs an agentboard.AgentMessage with the corresponding
// *Response kind and calls s.sendAgent.

func (s *Server) agentHandleHello(pc *peer.Conn, ac *agentConn, h *agentboard.AgentBridgeHello) {
    var ticket [16]byte
    copy(ticket[:], h.AuthTicket[:])
    status := s.Board.Registry().Validate(h.RunnerId, h.TaskId, ticket)
    resp := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_hello_response}
    resp.SetHelloResponse(agentboard.AgentBridgeHelloResponse{Status: status})
    s.sendAgent(pc, resp)
    if status == agentboard.HelloStatusOk {
        ac.helloed = true
        ac.state = s.Board.Attach()
    } else {
        // close the connection on failed hello
        _ = pc.Close()
    }
}

// (other handlers: implement against ac.state, calling s.Board.Send/Subscribe/Wait/Inbox.
//  Each builds the corresponding *Response message and calls s.sendAgent.)
```

注: 完全な handler 実装は次の step で。Wait は goroutine で long-poll を実行する。

- [ ] **Step 5: Implement remaining handlers (send / subscribe / wait / inbox)**

```go
func (s *Server) agentHandleSend(pc *peer.Conn, ac *agentConn, r *agentboard.SendRequest) {
    if !ac.helloed {
        return
    }
    seq, err := s.Board.Send(string(r.Topic), r.Payload)
    var status agentboard.SendStatus
    switch err {
    case nil:
        status = agentboard.SendStatus_ok
    case agentboard.ErrPayloadTooLarge:
        status = agentboard.SendStatus_payload_too_large
    case agentboard.ErrTooManyTopics:
        status = agentboard.SendStatus_too_many_topics
    default:
        status = agentboard.SendStatus_bad_frame
    }
    resp := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_send_response}
    resp.SetSendResponse(agentboard.SendResponse{RequestId: r.RequestId, Status: status, Seq: seq})
    s.sendAgent(pc, resp)
}

func (s *Server) agentHandleSubscribe(pc *peer.Conn, ac *agentConn, r *agentboard.SubscribeRequest) {
    if !ac.helloed {
        return
    }
    err := s.Board.Subscribe(ac.state, string(r.Pattern))
    status := agentboard.SubscribeStatus_ok
    if err != nil {
        status = agentboard.SubscribeStatus_bad_pattern
    }
    resp := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_subscribe_response}
    resp.SetSubscribeResponse(agentboard.SubscribeResponse{RequestId: r.RequestId, Status: status})
    s.sendAgent(pc, resp)
}

func (s *Server) agentHandleUnsubscribe(pc *peer.Conn, ac *agentConn, r *agentboard.UnsubscribeRequest) {
    if !ac.helloed {
        return
    }
    s.Board.Unsubscribe(ac.state, string(r.Pattern))
    resp := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_subscribe_response}
    resp.SetSubscribeResponse(agentboard.SubscribeResponse{RequestId: r.RequestId, Status: agentboard.SubscribeStatus_ok})
    s.sendAgent(pc, resp)
}

func (s *Server) agentHandleWait(pc *peer.Conn, ac *agentConn, r *agentboard.WaitRequest) {
    if !ac.helloed {
        return
    }
    ctx, cancel := context.WithTimeout(context.Background(), time.Duration(r.TimeoutMs)*time.Millisecond)
    defer cancel()
    msgs, timedOut, _ := s.Board.Wait(ctx, ac.state, string(r.Pattern), r.Since)
    next := r.Since
    delivered := make([]agentboard.DeliveredMessage, 0, len(msgs))
    for _, m := range msgs {
        if m.Seq > next {
            next = m.Seq
        }
        delivered = append(delivered, agentboard.DeliveredMessage{
            Seq:     m.Seq,
            Topic:   []byte(m.Topic),
            Payload: m.Payload,
        })
    }
    resp := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_wait_response}
    var to uint8
    if timedOut {
        to = 1
    }
    resp.SetWaitResponse(agentboard.WaitResponse{
        RequestId: r.RequestId, TimedOut: to, NextCursor: next, Msgs: delivered,
    })
    s.sendAgent(pc, resp)
}

func (s *Server) agentHandleInbox(pc *peer.Conn, ac *agentConn, r *agentboard.InboxRequest) {
    if !ac.helloed {
        return
    }
    msgs, next := s.Board.Inbox(ac.state, r.Since)
    delivered := make([]agentboard.DeliveredMessage, 0, len(msgs))
    for _, m := range msgs {
        delivered = append(delivered, agentboard.DeliveredMessage{
            Seq: m.Seq, Topic: []byte(m.Topic), Payload: m.Payload,
        })
    }
    resp := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_inbox_response}
    resp.SetInboxResponse(agentboard.InboxResponse{
        RequestId: r.RequestId, NextCursor: next, Msgs: delivered,
    })
    s.sendAgent(pc, resp)
}

func (s *Server) getOrCreateAgentConn(pc *peer.Conn) *agentConn {
    s.agentConnsMu.Lock()
    defer s.agentConnsMu.Unlock()
    if s.agentConns == nil {
        s.agentConns = make(map[*peer.Conn]*agentConn)
    }
    ac, ok := s.agentConns[pc]
    if !ok {
        ac = &agentConn{}
        s.agentConns[pc] = ac
    }
    return ac
}
```

`Server` struct に `agentConnsMu sync.Mutex` と `agentConns map[*peer.Conn]*agentConn` を追加。peer.Conn close hook で Detach + map から削除。

- [ ] **Step 6: Build**

```bash
go build ./...
```

Expected: クリーンに通る。

- [ ] **Step 7: Commit**

```bash
git add server/server.go server/agent_handler.go
git commit -m "server: agent_message dispatch + agentboard handlers (hello/send/sub/wait/inbox)"
```

---

### Task 8: Ticket lifecycle integration

**Files:**
- Modify: `server/dispatch.go`, `server/runner_handler.go`

- [ ] **Step 1: Generate ticket on TryDispatch**

`server/dispatch.go` の `TryDispatch` 内、`AssignTask` を構築する箇所に ticket 生成と registry 登録を追加:

```go
import "crypto/rand"

// inside TryDispatch, before sending AssignTask
var ticket [16]byte
if _, err := rand.Read(ticket[:]); err != nil {
    return false, fmt.Errorf("ticket gen: %w", err)
}
s.Board.Registry().Register(runner.ID, taskID, ticket)
assign := protocol.AssignTask{
    TaskId:     taskID,
    AuthTicket: ticket,
    RepoPath:   []byte(repoPath),
    Prompt:     []byte(prompt),
}
```

`OpenInteractiveRequest` 経路 (`server/task_handler.go` の `handleOpenInteractive`) でも同様に ticket を生成して `OpenExecRunnerRequest.AuthTicket` にセット。

- [ ] **Step 2: Revoke ticket on TaskFinished**

`server/runner_handler.go` の `handleTaskFinished` (or 等価関数) で:

```go
s.Board.Registry().Revoke(runnerID, taskID)
```

を追加。位置は taskstore.MarkFinished の直後で OK。

- [ ] **Step 3: Build**

```bash
go build ./...
```

Expected: クリーンに通る。

- [ ] **Step 4: Add unit test for TryDispatch ticket flow**

`server/dispatch_test.go` に追加:

```go
func TestTryDispatch_RegistersTicket(t *testing.T) {
    // ... existing fixture setup ...
    board := agentboard.New(agentboard.Config{RingN: 4, TopicTTL: time.Hour, MaxTopics: 4, MaxPayload: 1024})
    s := &Server{Board: board /* ... */}
    // simulate dispatch ...
    // assert: board.Registry().Validate(runnerID, taskID, ticketSeenInAssignTask) == HelloStatusOk
}
```

(既存 fixture に合わせて adapt — fakes_test.go 参照)

- [ ] **Step 5: Run test**

```bash
go test ./server/ -run TestTryDispatch_RegistersTicket -v
```

Expected: PASS。

- [ ] **Step 6: Commit**

```bash
git add server/dispatch.go server/dispatch_test.go server/runner_handler.go server/task_handler.go
git commit -m "server: ticket lifecycle — generate on dispatch, revoke on TaskFinished"
```

---

### Task 9: Server flag wiring

**Files:**
- Modify: `cmd/harness-server/main.go`

- [ ] **Step 1: Add flags**

```go
ringN := flag.Int("agentboard-ring", 64, "agentboard ring buffer entries per topic")
ttl := flag.Duration("agentboard-ttl", 30*time.Minute, "agentboard topic TTL after last publish")
maxTopics := flag.Int("agentboard-max-topics", 1024, "agentboard max active topics")
maxPayload := flag.Int("agentboard-max-payload", 64*1024, "agentboard max payload bytes per message")
```

- [ ] **Step 2: Construct Board with config**

`Server` 構築時:

```go
board := agentboard.New(agentboard.Config{
    RingN:      *ringN,
    TopicTTL:   *ttl,
    MaxTopics:  *maxTopics,
    MaxPayload: *maxPayload,
})
defer board.Close()
srv := &server.Server{Board: board /* ... */}
```

- [ ] **Step 3: Build + smoke test**

```bash
go build ./cmd/harness-server/
./harness-server --help 2>&1 | grep agentboard
```

Expected: 4 つの flag が表示される。

- [ ] **Step 4: Commit**

```bash
git add cmd/harness-server/main.go
git commit -m "harness-server: add --agentboard-ring/-ttl/-max-topics/-max-payload flags"
```

---

### Task 10: End-to-end Go test

**Files:**
- Create: `agentboard/e2e_test.go`

- [ ] **Step 1: Write E2E test that simulates 2 agent CLIs**

```go
package agentboard_test

import (
    "context"
    "net"
    "testing"
    "time"

    "github.com/on-keyday/agent-harness/agentboard"
    "github.com/on-keyday/agent-harness/runner/protocol"
    "github.com/on-keyday/agent-harness/server"
    "github.com/on-keyday/agent-harness/transport"
)

// TestAgentboardE2E_SendThenWait: agent A sends "hi" to topic/foo;
// agent B subscribes then waits → receives "hi".
func TestAgentboardE2E_SendThenWait(t *testing.T) {
    // Spin up an in-process server with agentboard
    board := agentboard.New(agentboard.Config{RingN: 16, TopicTTL: time.Minute, MaxTopics: 4, MaxPayload: 1024})
    defer board.Close()

    rid := protocol.RunnerID{Transport: []byte("ws"), IpAddr: []byte{127,0,0,1}, Port: 9000, UniqueNumber: 1}
    var tidA protocol.TaskID; tidA.Id[0] = 1
    var tidB protocol.TaskID; tidB.Id[0] = 2
    var ticketA, ticketB [16]byte; ticketA[0] = 0xA1; ticketB[0] = 0xB2
    board.Registry().Register(rid, tidA, ticketA)
    board.Registry().Register(rid, tidB, ticketB)

    // Bind a real loopback WS listener and run server.Server with Board on top
    ln, err := net.Listen("tcp", "127.0.0.1:0")
    if err != nil { t.Fatal(err) }
    defer ln.Close()
    // ... wire up transport.WebSocketEndpoint(ln, ...)  + server.Server{Board: board, ...}
    // ... start server goroutine

    // Two clients dial: each sends Hello with its (rid, tid, ticket)
    // Client A: Subscribe "topic/foo"
    // Client B: Send "topic/foo" "hi"
    // Client A: Wait "topic/foo" since=0 timeout=200ms → expect msg "hi"

    // (exact wiring follows the structure in transport/websocket.go and peer.Dial)
    // This is the smallest end-to-end flow: hello → subscribe → send → wait → assertion.
    ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
    defer cancel()
    _ = ctx
    // ... assertion
}
```

注: 完全な test wiring は本リポジトリの既存 server test fixture (`server/fakes_test.go`, `server_test.go`) を参考に組む。 

- [ ] **Step 2: Run test**

```bash
go test ./agentboard/ -run TestAgentboardE2E -v
```

Expected: PASS。失敗したら server.Server 構築 / Board 連結部分を debug。

- [ ] **Step 3: Commit**

```bash
git add agentboard/e2e_test.go
git commit -m "agentboard: e2e test — hello → subscribe → send → wait round-trip"
```

---

## Self-review checklist (実装者向け)

- [ ] `make protoregen` 後の差分は `git diff` で確認したか?
- [ ] `agentboard.bgn` の import が解決しなかった場合、`RunnerID` と `TaskID` を agentboard.bgn 内に独立して再定義し、Go 側で 2 つの型を変換する helper を `agentboard/ids.go` に追加することで対処した
- [ ] `subtle.ConstantTimeCompare` で ticket 比較していて、`==` で比較していないか?
- [ ] Board.Send が err を返すケース全てが `SendStatus_*` にマップされているか?
- [ ] Wait の goroutine リーク: `b.stopCh` で必ず終了するか?
- [ ] Server struct の `agentConns` map は connection close 時に Detach + delete されるか?
- [ ] Spec §8 の retention default (N=64, TTL=30m, max-topics=1024, max-payload=64K) が flag default に正しく入っているか?

---

## Done definition

- `go test ./agentboard/ ./server/ -v` 全 pass
- `go build ./...` クリーン
- E2E test (Task 10) で 2 つの simulated client が server 経由でメッセージ往復できる
- `harness-server --help` に 4 つの agentboard flag が表示される
- 既存 `task.<id>.log` 系の watch / GetTaskLog 動作に変化なし (回帰なし)
