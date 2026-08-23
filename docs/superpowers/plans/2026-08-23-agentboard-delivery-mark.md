# Agentboard delivery mark + wait/dispatch repair — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move the agentboard's delivery position from a client-side cursor file into per-topic server state, stop a synchronous wait from waking its own PTY, scope a wait's subscription to the wait, and make `dispatch` recognise the reply to the message it sent.

**Architecture:** Three per-topic facts about a task — is it synchronously waiting, is it subscribed, how far has the automatic injection path reached — all land in `agentboard.taskState`, which already holds the subscription set and shares its lifetime with the task's ticket. `Board.Send` consults the first to skip the wake it would otherwise cause for a waiter's own message. A new wire kind (`inbox_advance`) makes "read what has not been injected, and mark it injected" one server-side operation under one lock, replacing `~/.cache/harness/agent-cursor-<task>` and its `--since-last` / `--commit` flag pair. `WaitRequest` gains an `in_reply_to` filter so `dispatch` can wait for the answer to its own publish on the topic the server actually routes replies to.

**Tech Stack:** Go 1.x, `.bgn` schemas compiled by `make protoregen` (brgen), Bubble Tea TUI, Go/WASM + vanilla JS WebUI.

**Spec:** `docs/superpowers/specs/2026-08-23-agentboard-delivery-mark-design.md`

## Global Constraints

- **Work in ONE checkout, and it is the one holding this plan.** The spec and this plan are committed on `harness/70fbad4a6eb6f1e992be8a669f1bcefd` in `.harness-worktrees/70fbad4a6eb6f1e992be8a669f1bcefd/`, so that worktree is where the code goes too. Verify with `git rev-parse --abbrev-ref HEAD` before writing code, and anchor every tool call to that directory: a bare absolute path under `/home/kforfk/workspace/remote-agent-harness/<rel>` resolves to the PARENT checkout, which would put edits and commits in different places (Pitfall 8). Landing to trunk happens afterwards via the `landing-to-main` skill, not by writing to the parent.
  - **If this plan is executed by dispatched subagents instead**, the constraint inverts: subagents cannot reliably anchor to a worktree, so they work in the parent repo and the plan must be moved there first. Pick one before starting.
- **Read `.claude/skills/implementation-pitfalls/SKILL.md` in full before writing code.**
- **Verify with make targets, not ad-hoc `go build`**: `make check`, `make vet`, `make test`, `make wasm-check`. `./...` hides pattern breaks that the make targets catch.
- **Build hygiene**: compile-check with `go build ./...` (writes no binary) or `go vet ./cmd/<x>`. Never bare `go build ./cmd/<x>/` — it drops an executable into the worktree root. The working tree must be exactly as clean after checks as before.
- **No compat shims, no deprecation periods.** Single-user dogfood deployment, no external users. Removed flags are removed.
- **Schema is the single source of truth.** Every byte on the wire is described in the `.bgn`. No "by convention this byte means X".
- **Both `.bgn` changes land in Task 5, together.** No follow-up task adds a field.
- **Commit after every task.** Commit messages end with:
  `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>`

---

## File Structure

| File | Responsibility | Tasks |
| --- | --- | --- |
| `agentboard/taskstate.go` | per-(runner,task) state: subscriptions, wait refcounts, delivery marks | 1, 4 |
| `agentboard/conn.go` | per-connection state; close signal | 2 |
| `agentboard/board.go` | `Wait`, `Send`, `Inbox`, new `InboxAdvance`, `ListSubscribers` | 1–4, 11 |
| `agentboard/agentboard.bgn` | agent↔server wire: `in_reply_to` filter, `inbox_advance` | 5 |
| `runner/protocol/message.bgn` | operator wire: per-pattern `shown` / `pending` | 5 |
| `server/agent_handler.go` | agent message handlers | 6 |
| `server/board_handler.go` | operator `board subscribers` handler | 11 |
| `cli/agent/inbox.go` | plain read vs advancing read | 7 |
| `cli/agent/wait.go` | `--in-reply-to`, no cursor | 8 |
| `cli/agent/dispatch.go` | send + correlated reply wait | 9 |
| `cli/agent/cursor.go` | **deleted** | 7 |
| `cli/agent/stop_hook.go` | **deleted** | 7 |
| `runner/settings.go` | injected hook command line | 10 |
| `cli/board.go`, `cli/cmd_board.go` | operator row type + CLI rendering | 11 |
| `tui/board.go` | TUI subscribers view | 11 |
| `cmd/harness-webui-wasm/main.go`, `webui/static/main.js` | WASM bridge + WebUI rendering | 11 |
| `.claude/`, `.agents/`, `runner/agentskills/` skills | agent-facing docs | 12 |

---

### Task 1: Wait-scoped subscription in `taskState`

A wait makes its topic subscribed for exactly the duration of the wait, and never removes a subscription the task already held. One refcount map serves this and (in Task 3) the wake suppression, because the two have the same extent.

**Files:**
- Modify: `agentboard/taskstate.go:16-50`
- Modify: `agentboard/board.go:323-348` (`Board.Wait`)
- Test: `agentboard/taskstate_test.go` (create)

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces: `(*taskState).beginWait(topic string)`, `(*taskState).endWait(topic string)`, `(*taskState).isWaiting(topic string) bool`. `matches` and `snapshotPatterns` keep their existing signatures and start returning the union of persistent patterns and topics with a live wait.

- [ ] **Step 1: Write the failing test**

Create `agentboard/taskstate_test.go`:

```go
package agentboard

import "testing"

func TestTaskState_WaitScopedSubscription(t *testing.T) {
	ts := newTaskState()
	if ts.matches("topic/a") {
		t.Fatal("fresh taskState must not match topic/a")
	}
	ts.beginWait("topic/a")
	if !ts.matches("topic/a") {
		t.Fatal("a live wait must make its topic match")
	}
	ts.endWait("topic/a")
	if ts.matches("topic/a") {
		t.Fatal("the wait ended, so topic/a must stop matching")
	}
}

func TestTaskState_WaitDoesNotRemoveExistingSubscription(t *testing.T) {
	ts := newTaskState()
	ts.addPattern("topic/b")
	ts.beginWait("topic/b")
	ts.endWait("topic/b")
	if !ts.matches("topic/b") {
		t.Fatal("a wait must not remove a subscription the task already held")
	}
}

func TestTaskState_ConcurrentWaitsRefcount(t *testing.T) {
	ts := newTaskState()
	ts.beginWait("topic/c")
	ts.beginWait("topic/c")
	ts.endWait("topic/c")
	if !ts.matches("topic/c") {
		t.Fatal("one of two waits ended; topic/c must still match")
	}
	ts.endWait("topic/c")
	if ts.matches("topic/c") {
		t.Fatal("both waits ended; topic/c must stop matching")
	}
}

func TestTaskState_SnapshotPatternsIncludesLiveWait(t *testing.T) {
	ts := newTaskState()
	ts.addPattern("topic/persistent")
	ts.beginWait("topic/waited")
	got := map[string]bool{}
	for _, p := range ts.snapshotPatterns() {
		got[p] = true
	}
	if !got["topic/persistent"] || !got["topic/waited"] {
		t.Fatalf("snapshotPatterns = %v, want both the persistent and the waited topic", got)
	}
	ts.endWait("topic/waited")
	got = map[string]bool{}
	for _, p := range ts.snapshotPatterns() {
		got[p] = true
	}
	if got["topic/waited"] {
		t.Fatal("the wait ended; topic/waited must leave snapshotPatterns")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./agentboard/ -run TestTaskState_ -v`
Expected: FAIL — `ts.beginWait undefined`, `ts.endWait undefined`.

- [ ] **Step 3: Add the refcount map and its methods**

In `agentboard/taskstate.go`, add the field to the struct (keep the existing fields and their order):

```go
type taskState struct {
	mu       sync.Mutex
	patterns map[string]struct{}
	// waiting counts the live synchronous Board.Wait calls per topic for
	// this task. It does two jobs, and they have the same extent: a topic
	// with a live wait is subscribed for the duration of that wait, and a
	// publish to it must not also wake this task's PTY (the waiter is
	// already being handed the message). Refcounted because two waits on
	// one topic may overlap; the entry is deleted at zero so `matches`
	// needs no zero check.
	waiting map[string]int
	conns   map[*ConnState]struct{}
	rid     protocol.RunnerID
	tid     protocol.TaskID
	host    string
	profile string
}
```

Initialise it in `newTaskState`:

```go
func newTaskState() *taskState {
	return &taskState{
		patterns: make(map[string]struct{}),
		waiting:  make(map[string]int),
		conns:    make(map[*ConnState]struct{}),
	}
}
```

Add the three methods and widen the two readers:

```go
// beginWait registers a live synchronous wait on topic. Paired with endWait.
func (t *taskState) beginWait(topic string) {
	t.mu.Lock()
	t.waiting[topic]++
	t.mu.Unlock()
}

// endWait retires one live wait on topic. Deleting at zero keeps `matches`
// a plain map lookup rather than a lookup plus a count comparison.
func (t *taskState) endWait(topic string) {
	t.mu.Lock()
	if n := t.waiting[topic] - 1; n > 0 {
		t.waiting[topic] = n
	} else {
		delete(t.waiting, topic)
	}
	t.mu.Unlock()
}

// isWaiting reports whether this task has a live synchronous wait on topic.
func (t *taskState) isWaiting(topic string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.waiting[topic]
	return ok
}

func (t *taskState) matches(topic string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.patterns[topic]; ok {
		return true
	}
	_, ok := t.waiting[topic]
	return ok
}

func (t *taskState) snapshotPatterns() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.patterns)+len(t.waiting))
	for p := range t.patterns {
		out = append(out, p)
	}
	for p := range t.waiting {
		if _, dup := t.patterns[p]; !dup {
			out = append(out, p)
		}
	}
	return out
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./agentboard/ -run TestTaskState_ -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Make `Board.Wait` use it instead of `addPattern`**

In `agentboard/board.go`, replace the permanent-subscribe block at the top of `Wait` and update the doc comment:

```go
// Wait blocks until at least one message arrives on topicName with seq > since,
// or until ctx is done. Returns (messages, timedOut, error).
//
// For the duration of the call the topic is subscribed: beginWait/endWait
// refcount it, and taskState.matches is the union of the persistent pattern
// set and the topics with a live wait. A wait therefore leaves no
// subscription behind, and does not remove one the task already held.
func (b *Board) Wait(ctx context.Context, c *ConnState, topicName string, since uint64) ([]RetainedMessage, bool, error) {
	if c == nil || c.task == nil {
		return nil, false, errors.New("not attached")
	}
	c.task.beginWait(topicName)
	defer c.task.endWait(topicName)
	for {
		// ... loop body unchanged
	}
}
```

- [ ] **Step 6: Write the board-level test**

Append to `agentboard/board_test.go`:

```go
func TestBoard_WaitLeavesNoSubscriptionBehind(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()
	conn := b.Attach(RunnerID{}, TaskID{}, "test-host", "")
	defer b.Detach(conn)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, timedOut, _ := b.Wait(ctx, conn, "topic/transient", 0)
	if !timedOut {
		t.Fatal("expected the wait to time out")
	}
	if b.Subscribes(conn, "topic/transient") {
		t.Fatal("the wait ended; topic/transient must not remain subscribed")
	}
}
```

- [ ] **Step 7: Run the agentboard suite**

Run: `go test ./agentboard/ -v`
Expected: PASS. Existing tests that call `b.Wait(ctx, conn, topic, 0)` still compile — the signature is unchanged in this task.

- [ ] **Step 8: Commit**

```bash
git add agentboard/taskstate.go agentboard/taskstate_test.go agentboard/board.go agentboard/board_test.go
git commit -m "fix(agentboard): a wait's subscription lasts exactly as long as the wait

Board.Wait called addPattern and never removed it, so one wait on a topic left
a permanent subscription on the task. The topic is now refcounted in
taskState.waiting for the duration of the call, and matches/snapshotPatterns
return the union — a wait on an already-subscribed topic leaves that
subscription intact.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 2: A blocked `Wait` ends when its connection closes

Task 3 makes the waiter count decide wake suppression. A CLI process killed mid-wait would otherwise hold the count above zero — and the agent's PTY silent — until the server-side timeout expires, up to the five-minute default.

**Files:**
- Modify: `agentboard/conn.go:9-19`
- Modify: `agentboard/board.go:134-142` (`Board.Detach`), `agentboard/board.go` (`Wait` select)
- Test: `agentboard/board_test.go`

**Interfaces:**
- Consumes: Task 1's `beginWait` / `endWait`.
- Produces: `ConnState.done` (unexported `chan struct{}`), closed exactly once by `Board.Detach`.

- [ ] **Step 1: Write the failing test**

Append to `agentboard/board_test.go`:

```go
func TestBoard_WaitEndsWhenConnectionDetaches(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()
	conn := b.Attach(RunnerID{}, TaskID{}, "test-host", "")

	done := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _, _ = b.Wait(ctx, conn, "topic/abandoned", 0)
		close(done)
	}()

	time.Sleep(20 * time.Millisecond)
	b.Detach(conn)

	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Wait did not return after its connection was detached")
	}
}

func TestBoard_DetachTwiceIsSafe(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()
	conn := b.Attach(RunnerID{}, TaskID{}, "test-host", "")
	b.Detach(conn)
	b.Detach(conn) // must not panic on a second close
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./agentboard/ -run 'TestBoard_WaitEndsWhenConnectionDetaches|TestBoard_DetachTwiceIsSafe' -v`
Expected: FAIL — the first test times out after 500ms because `Wait` keeps blocking.

- [ ] **Step 3: Add the close signal to `ConnState`**

In `agentboard/conn.go`:

```go
package agentboard

import (
	"sync"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// ConnState is per-attached-client transient state. The persistent piece —
// subscription pattern set — lives in the shared *taskState (one per
// (runner_id, task_id)) so it survives across the short-lived per-subcommand
// harness-cli connections.
type ConnState struct {
	notify chan struct{} // pinged when a relevant publish happens
	task   *taskState

	// done is closed by Board.Detach when the agent's connection goes away.
	// A blocked Board.Wait selects on it: the server's wait context carries
	// only the client's timeout (server/agent_handler.go), so without this a
	// killed CLI would leave the wait — and the wake suppression keyed on it
	// — alive for the rest of that timeout.
	closeOnce sync.Once
	done      chan struct{}
}

func newConnState(task *taskState) *ConnState {
	return &ConnState{
		notify: make(chan struct{}, 1),
		task:   task,
		done:   make(chan struct{}),
	}
}

// close marks the connection gone. Idempotent: Board.Detach may be reached
// more than once for one ConnState.
func (c *ConnState) close() {
	c.closeOnce.Do(func() { close(c.done) })
}
```

Keep `ping`, `matches` and `Identity` exactly as they are.

- [ ] **Step 4: Close it from `Board.Detach`**

In `agentboard/board.go`, inside `Detach`, before or after the existing `c.task.detachConn(c)`:

```go
func (b *Board) Detach(c *ConnState) {
	if c == nil || c.task == nil {
		return
	}
	c.close()
	c.task.detachConn(c)
}
```

- [ ] **Step 5: Select on it in `Wait`**

In `Board.Wait`'s inner `select`, add the case:

```go
		select {
		case <-c.notify:
			continue
		case <-c.done:
			// The agent's connection is gone; nobody is left to receive
			// these messages. Not a timeout: the caller is not there to
			// distinguish the two.
			return nil, false, nil
		case <-ctx.Done():
			return nil, true, nil
		case <-b.stopCh:
			return nil, false, errors.New("board closed")
		}
```

- [ ] **Step 6: Run tests**

Run: `go test ./agentboard/ -v`
Expected: PASS, including both new tests.

- [ ] **Step 7: Commit**

```bash
git add agentboard/conn.go agentboard/board.go agentboard/board_test.go
git commit -m "fix(agentboard): a blocked Wait ends when its connection detaches

The server builds the wait context from context.Background() plus the client's
timeout, so a killed CLI left the server-side Wait running to the full timeout.
That is about to matter: the live-waiter count decides whether a publish also
wakes the task's PTY.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 3: A publish does not wake the task that is waiting for it

**Files:**
- Modify: `agentboard/board.go:277-289` (the per-target loop in `Send`)
- Test: `agentboard/board_test.go`

**Interfaces:**
- Consumes: Task 1's `isWaiting`.
- Produces: no new exported surface. `Board.Send`'s signature is unchanged.

- [ ] **Step 1: Write the failing test**

Append to `agentboard/board_test.go`:

```go
// taskIDFromByte builds a distinct TaskID so two taskStates can coexist.
func taskIDFromByte(b byte) TaskID {
	var t TaskID
	t.Id[0] = b
	return t
}

func TestBoard_SendSkipsWakeForTheTaskWaitingOnThatTopic(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()

	waiter := b.Attach(RunnerID{}, taskIDFromByte(1), "host-a", "")
	defer b.Detach(waiter)
	bystander := b.Attach(RunnerID{}, taskIDFromByte(2), "host-b", "")
	defer b.Detach(bystander)
	_ = b.Subscribe(bystander, "topic/shared")

	var mu sync.Mutex
	woke := map[byte]int{}
	b.SetOnDeliver(func(_ protocol.RunnerID, tid protocol.TaskID) {
		mu.Lock()
		woke[tid.Id[0]]++
		mu.Unlock()
	})

	ready := make(chan struct{})
	go func() {
		close(ready)
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _, _ = b.Wait(ctx, waiter, "topic/shared", 0)
	}()
	<-ready
	time.Sleep(30 * time.Millisecond) // let the wait register

	if _, _, err := b.Send("topic/shared", []byte("x"), testRid, testTid, "h", "", 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if woke[1] != 0 {
		t.Errorf("the waiting task was woken %d times, want 0", woke[1])
	}
	if woke[2] != 1 {
		t.Errorf("the bystander subscriber was woken %d times, want 1", woke[2])
	}
}

func TestBoard_WaitingTaskIsStillWokenForItsOtherTopics(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()

	conn := b.Attach(RunnerID{}, taskIDFromByte(3), "host-c", "")
	defer b.Detach(conn)
	_ = b.Subscribe(conn, "topic/other")

	var mu sync.Mutex
	wakes := 0
	b.SetOnDeliver(func(_ protocol.RunnerID, _ protocol.TaskID) {
		mu.Lock()
		wakes++
		mu.Unlock()
	})

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _, _ = b.Wait(ctx, conn, "topic/awaited", 0)
	}()
	time.Sleep(30 * time.Millisecond)

	if _, _, err := b.Send("topic/other", []byte("y"), testRid, testTid, "h", "", 0); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if wakes != 1 {
		t.Errorf("wakes = %d, want 1 — a wait on one topic must not silence the others", wakes)
	}
}
```

Add `"sync"` to the test file's imports if it is not already there.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./agentboard/ -run 'TestBoard_SendSkipsWake|TestBoard_WaitingTaskIsStill' -v`
Expected: FAIL — `the waiting task was woken 1 times, want 0`.

- [ ] **Step 3: Skip `onDeliver` for a target waiting on this topic**

In `agentboard/board.go`, in `Send`'s per-target loop, replace the `if fn != nil` block:

```go
	for _, ts := range targets {
		for _, c := range ts.snapshotConns() {
			c.ping()
		}
		// A task synchronously waiting on THIS topic is already being handed
		// the message by that wait; waking its PTY on top of it types a
		// <harness:agentboard-wake> prompt into an agent about a message it
		// did not ask for and cannot act on. The check is per (task, topic):
		// another task subscribed to the same topic still gets its wake, and
		// this task still gets woken for its other topics.
		if fn != nil && !ts.isWaiting(topicName) {
			rid, tid, _, _ := ts.identity()
			fn(rid, tid)
		}
	}
```

- [ ] **Step 4: Run tests**

Run: `go test ./agentboard/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add agentboard/board.go agentboard/board_test.go
git commit -m "fix(agentboard): a publish does not wake the task waiting for it

Send pinged the waiter and called onDeliver in the same loop, so a script
blocked in agent wait got its message AND the interactive agent sharing that
task id got a wake marker typed into its PTY about it. The skip is keyed on
(task, topic): other subscribers keep their wake, and the waiting task keeps
wakes for its other topics.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 4: The delivery mark lives in `taskState`

**Files:**
- Modify: `agentboard/taskstate.go`
- Modify: `agentboard/board.go` (add `InboxAdvance`)
- Test: `agentboard/board_test.go`

**Interfaces:**
- Consumes: Task 1's union `snapshotPatterns`.
- Produces: `func (b *Board) InboxAdvance(c *ConnState) []RetainedMessage`. `Board.Inbox(c, since)` keeps its signature and does not touch the mark.

- [ ] **Step 1: Write the failing test**

Append to `agentboard/board_test.go`:

```go
func TestBoard_InboxAdvanceReturnsEachMessageOnce(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()
	conn := b.Attach(RunnerID{}, TaskID{}, "test-host", "")
	defer b.Detach(conn)
	_ = b.Subscribe(conn, "topic/mark")

	if _, _, err := b.Send("topic/mark", []byte("one"), testRid, testTid, "h", "", 0); err != nil {
		t.Fatal(err)
	}
	first := b.InboxAdvance(conn)
	if len(first) != 1 || string(first[0].Payload) != "one" {
		t.Fatalf("first advance = %+v, want one message 'one'", first)
	}
	if second := b.InboxAdvance(conn); len(second) != 0 {
		t.Fatalf("second advance = %+v, want empty", second)
	}
	if _, _, err := b.Send("topic/mark", []byte("two"), testRid, testTid, "h", "", 0); err != nil {
		t.Fatal(err)
	}
	third := b.InboxAdvance(conn)
	if len(third) != 1 || string(third[0].Payload) != "two" {
		t.Fatalf("third advance = %+v, want one message 'two'", third)
	}
}

// This is the regression the whole change exists for. Under the old
// client-side cursor, taking a message on one topic moved a single global
// watermark past an unread message on another.
func TestBoard_InboxAdvanceIsPerTopic(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()
	conn := b.Attach(RunnerID{}, TaskID{}, "test-host", "")
	defer b.Detach(conn)
	_ = b.Subscribe(conn, "topic/quiet")
	_ = b.Subscribe(conn, "topic/busy")

	// seq N   -> topic/quiet, never read
	// seq N+1 -> topic/busy, taken by a wait-like reader
	if _, _, err := b.Send("topic/quiet", []byte("unread"), testRid, testTid, "h", "", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Send("topic/busy", []byte("taken"), testRid, testTid, "h", "", 0); err != nil {
		t.Fatal(err)
	}

	got := map[string]bool{}
	for _, m := range b.InboxAdvance(conn) {
		got[string(m.Payload)] = true
	}
	if !got["unread"] {
		t.Fatal("the message on the other topic was skipped — the mark is not per topic")
	}
	if !got["taken"] {
		t.Fatal("the busy topic's message was not returned")
	}
}

func TestBoard_InboxDoesNotMoveTheMark(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()
	conn := b.Attach(RunnerID{}, TaskID{}, "test-host", "")
	defer b.Detach(conn)
	_ = b.Subscribe(conn, "topic/plain")

	if _, _, err := b.Send("topic/plain", []byte("hello"), testRid, testTid, "h", "", 0); err != nil {
		t.Fatal(err)
	}
	if msgs, _ := b.Inbox(conn, 0); len(msgs) != 1 {
		t.Fatalf("Inbox = %+v, want one message", msgs)
	}
	if msgs, _ := b.Inbox(conn, 0); len(msgs) != 1 {
		t.Fatal("Inbox must be idempotent")
	}
	if adv := b.InboxAdvance(conn); len(adv) != 1 {
		t.Fatal("Inbox must not have advanced the mark")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./agentboard/ -run TestBoard_Inbox -v`
Expected: FAIL — `b.InboxAdvance undefined`.

- [ ] **Step 3: Add the mark to `taskState`**

In `agentboard/taskstate.go`, add the field (after `waiting`) and initialise it in `newTaskState`:

```go
	// shown is the highest seq per topic that the automatic injection path
	// has already handed to this task. Only Board.InboxAdvance writes it.
	// Per topic rather than one scalar per task: a reader that covers one
	// topic must not be able to claim progress on the others, which is the
	// defect the client-side cursor had.
	shown map[string]uint64
```

```go
		shown:    make(map[string]uint64),
```

Add the read-modify-write helper:

```go
// takeUnshown records msgs as shown for topic and returns the ones that were
// above the mark. Collecting and marking happen under one acquisition of
// t.mu, so two concurrent advances cannot both return the same message.
func (t *taskState) takeUnshown(topic string, msgs []RetainedMessage) []RetainedMessage {
	t.mu.Lock()
	defer t.mu.Unlock()
	mark := t.shown[topic]
	out := make([]RetainedMessage, 0, len(msgs))
	for _, m := range msgs {
		if m.Seq > mark {
			out = append(out, m)
			if m.Seq > t.shown[topic] {
				t.shown[topic] = m.Seq
			}
		}
	}
	return out
}

// shownSnapshot returns a copy of the per-topic marks, for the operator view.
func (t *taskState) shownSnapshot() map[string]uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make(map[string]uint64, len(t.shown))
	for k, v := range t.shown {
		out[k] = v
	}
	return out
}
```

- [ ] **Step 4: Add `Board.InboxAdvance`**

In `agentboard/board.go`, next to `Inbox`:

```go
// InboxAdvance returns the retained messages on this task's subscribed topics
// that the automatic injection path has not been given yet, and records them
// as given. It is the only reader that moves taskState.shown; Inbox never
// touches it.
//
// Lock discipline: the board lock is taken and released to snapshot the topic
// pointers, and taskState.mu is taken afterwards. It never holds t.mu while
// acquiring b.mu, so the b.mu -> t.mu order documented on Revoke holds.
// topic.since takes the topic's own lock, so the scan needs neither.
func (b *Board) InboxAdvance(c *ConnState) []RetainedMessage {
	if c == nil || c.task == nil {
		return nil
	}
	patterns := c.task.snapshotPatterns()

	b.mu.Lock()
	found := make(map[string]*topic, len(patterns))
	for _, p := range patterns {
		if t, ok := b.topics[p]; ok {
			found[p] = t
		}
	}
	b.mu.Unlock()

	out := make([]RetainedMessage, 0)
	for p, t := range found {
		out = append(out, c.task.takeUnshown(p, t.snapshot())...)
	}
	return out
}
```

`topic.snapshot()` already exists (`agentboard/board.go:438-446` uses it via `ListRetained`); it returns the whole ring, and `takeUnshown` applies the mark.

- [ ] **Step 5: Run tests**

Run: `go test ./agentboard/ -v`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add agentboard/taskstate.go agentboard/board.go agentboard/board_test.go
git commit -m "feat(agentboard): the delivery mark is per-topic server state

InboxAdvance returns what the automatic injection path has not yet been given
and marks it given, under one acquisition of the task's lock. The mark is per
topic, so a reader covering one topic cannot claim progress on the others --
the defect the single client-side cursor scalar had. Inbox is unchanged and
still moves nothing.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 5: Both wire schemas, together

Everything that changes on either wire lands here. No later task adds a field.

**Files:**
- Modify: `agentboard/agentboard.bgn:15-37` (enum), `:164-169` (`WaitRequest`), `:363-387` (union), plus two new formats
- Modify: `runner/protocol/message.bgn:800-802` (`SubscriptionPattern`)
- Regenerate: `agentboard/agentboard.go`, `runner/protocol/message.go`

**Interfaces:**
- Produces, for later tasks:
  - `agentboard.WaitRequest.InReplyTo uint64`
  - `agentboard.AgentMessageKind_InboxAdvance`, `agentboard.AgentMessageKind_InboxAdvanceResponse`
  - `agentboard.InboxAdvanceRequest{RequestId uint32}`
  - `agentboard.InboxAdvanceResponse{RequestId uint32, MsgsLen uint16, Msgs []DeliveredMessage}` with `SetMsgs`
  - `(*AgentMessage).SetInboxAdvance`, `.InboxAdvance()`, `.SetInboxAdvanceResponse()`, `.InboxAdvanceResponse()`
  - `protocol.SubscriptionPattern.Shown uint64`, `.Pending uint32`

- [ ] **Step 1: Append the two enum values at the END of `AgentMessageKind`**

In `agentboard/agentboard.bgn`, after `retract_response`:

```
    inbox_advance
    inbox_advance_response
```

Appended rather than placed next to `inbox` / `inbox_response`: the enum is a positional `:u8`, so inserting mid-list renumbers every later value and a server and runner at different versions would decode `purge` as `list_retained` instead of failing.

- [ ] **Step 2: Add the `in_reply_to` filter to `WaitRequest`**

```
format WaitRequest:
    request_id :u32
    pattern_len :u16
    pattern :[pattern_len]u8
    since :u64
    timeout_ms :u32
    # in_reply_to, when non-zero, filters the wait: only a message whose
    # in_reply_to equals this satisfies it, and a non-matching publish on the
    # topic does not end the wait. Zero means no filter. `dispatch` sets it to
    # the seq its own publish returned, which is how a reply is told apart
    # from unrelated traffic on the same topic.
    in_reply_to :u64
```

`InboxRequest` and `InboxResponse` are **not** touched: `since` and `next_cursor` stay for callers that poll (`codex`, `bash`, `cmd.exe` tasks get no `UserPromptSubmit` hook and read `inbox` themselves).

- [ ] **Step 3: Add the two new formats**

Next to `InboxRequest` / `InboxResponse`:

```
# InboxAdvanceRequest asks for the messages on this task's subscribed topics
# that the automatic injection path has not yet been given, and records them
# as given. The position is the server's (taskState.shown), never the
# client's — the request carries no cursor because a caller has no position
# to assert. This is what the UserPromptSubmit hook sends; a plain read uses
# InboxRequest and moves nothing.
format InboxAdvanceRequest:
    request_id :u32

format InboxAdvanceResponse:
    request_id :u32
    msgs_len :u16
    msgs :[msgs_len]DeliveredMessage
```

- [ ] **Step 4: Add the two union arms**

At the end of the `AgentMessage` match block, before the `..` error arm:

```
        AgentMessageKind.inbox_advance => inbox_advance :InboxAdvanceRequest
        AgentMessageKind.inbox_advance_response => inbox_advance_response :InboxAdvanceResponse
```

- [ ] **Step 5: Add the per-pattern mark to the operator schema**

In `runner/protocol/message.bgn`:

```
format SubscriptionPattern:
    name_len :u16
    name :[name_len]u8
    # shown is the highest seq the automatic injection path has given this
    # task for this topic; 0 when nothing has been advanced past. pending is
    # how many retained messages sit above it. Together they make the
    # delivery position readable by an operator — it used to be a file on the
    # runner host that no surface showed.
    shown :u64
    pending :u32
```

- [ ] **Step 6: Regenerate**

Run:

```bash
make protoregen ARGS='agentboard/agentboard.bgn runner/protocol/message.bgn'
```

Expected: `agentboard/agentboard.go` and `runner/protocol/message.go` are rewritten.

- [ ] **Step 7: Confirm the generated names**

Run:

```bash
grep -n 'InboxAdvanceRequest\|InboxAdvanceResponse\|AgentMessageKind_InboxAdvance' agentboard/agentboard.go | head
grep -n 'InReplyTo' agentboard/agentboard.go | grep -i wait
grep -n 'type SubscriptionPattern' -A 8 runner/protocol/message.go
```

Expected: the union accessors `InboxAdvance()` / `SetInboxAdvance(...)` exist, `WaitRequest` has an `InReplyTo` field, and `SubscriptionPattern` has `Shown` and `Pending`. If a generated method name collides with an interface method the type also needs (the `.bgn` arm-name collision this project has hit before), rename the arm and regenerate.

- [ ] **Step 8: Compile**

Run: `go build ./...`
Expected: builds. Nothing consumes the new fields yet.

- [ ] **Step 9: Run the wire-skew check**

Run: `scripts/wire-skew-check.sh`
Expected: PASS — a new runner against an old server is rejected and retries rather than dying.

- [ ] **Step 10: Commit**

```bash
git add agentboard/agentboard.bgn agentboard/agentboard.go runner/protocol/message.bgn runner/protocol/message.go
git commit -m "feat(schema): inbox_advance, WaitRequest.in_reply_to, per-pattern shown/pending

Three additions, one commit, no follow-ups:
- AgentMessageKind gains inbox_advance / inbox_advance_response, appended at
  the END of the enum so no existing positional value moves
- WaitRequest.in_reply_to filters a wait to the answer to one seq
- protocol.SubscriptionPattern carries shown/pending so the delivery position
  is readable on the operator surfaces

InboxRequest/InboxResponse are deliberately untouched: since/next_cursor stay
for runtimes that poll inbox because they get no UserPromptSubmit hook.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 6: Server handlers for the filter and the advancing read

**Files:**
- Modify: `server/agent_handler.go:60-100` (dispatch switch), `:484-530` (`agentHandleWait`), and add `agentHandleInboxAdvance`
- Modify: `agentboard/board.go` (`Wait` signature gains `inReplyTo`)
- Test: `agentboard/board_test.go`

**Interfaces:**
- Consumes: Task 4's `Board.InboxAdvance`, Task 5's generated types.
- Produces: `Board.Wait(ctx, c, topicName string, since, inReplyTo uint64)`. Every existing caller — including tests written before this task — must pass `0` for `inReplyTo`.

- [ ] **Step 1: Write the failing test**

Append to `agentboard/board_test.go`:

```go
func TestBoard_WaitFiltersByInReplyTo(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()
	conn := b.Attach(RunnerID{}, TaskID{}, "test-host", "")
	defer b.Detach(conn)
	_ = b.Subscribe(conn, "topic/replies")

	// A non-reply and a reply to the wrong seq must not satisfy the wait.
	if _, _, err := b.Send("topic/replies", []byte("noise"), testRid, testTid, "h", "", 0); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Send("topic/replies", []byte("wrong"), testRid, testTid, "h", "", 999); err != nil {
		t.Fatal(err)
	}

	go func() {
		time.Sleep(30 * time.Millisecond)
		_, _, _ = b.Send("topic/replies", []byte("right"), testRid, testTid, "h", "", 42)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	msgs, timedOut, _ := b.Wait(ctx, conn, "topic/replies", 0, 42)
	if timedOut {
		t.Fatal("the wait timed out instead of matching the reply to seq 42")
	}
	if len(msgs) != 1 || string(msgs[0].Payload) != "right" {
		t.Fatalf("wait = %+v, want only the reply to seq 42", msgs)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./agentboard/ -run TestBoard_WaitFiltersByInReplyTo -v`
Expected: FAIL — too many arguments to `b.Wait`.

- [ ] **Step 3: Widen `Board.Wait` and filter in the scan**

```go
func (b *Board) Wait(ctx context.Context, c *ConnState, topicName string, since, inReplyTo uint64) ([]RetainedMessage, bool, error) {
	if c == nil || c.task == nil {
		return nil, false, errors.New("not attached")
	}
	c.task.beginWait(topicName)
	defer c.task.endWait(topicName)
	for {
		b.mu.Lock()
		var msgs []RetainedMessage
		if t, ok := b.topics[topicName]; ok {
			msgs = t.since(since)
		}
		b.mu.Unlock()
		if inReplyTo != 0 {
			kept := msgs[:0]
			for _, m := range msgs {
				if m.InReplyTo == inReplyTo {
					kept = append(kept, m)
				}
			}
			msgs = kept
		}
		if len(msgs) > 0 {
			return msgs, false, nil
		}
		select {
		case <-c.notify:
			continue
		case <-c.done:
			return nil, false, nil
		case <-ctx.Done():
			return nil, true, nil
		case <-b.stopCh:
			return nil, false, errors.New("board closed")
		}
	}
}
```

- [ ] **Step 4: Update every existing `Board.Wait` caller**

Run: `grep -rn 'b\.Wait(\|Board\.Wait(\|\.Wait(ctx' --include=*.go . | grep -v SessionMux`
Add a trailing `0` to each call written before this task, in `agentboard/board_test.go` and `server/agent_handler.go`.

- [ ] **Step 5: Pass the filter through the handler**

In `server/agent_handler.go`, in `agentHandleWait`:

```go
	msgs, timedOut, _ := s.Board.Wait(ctx, ac.state, string(r.Pattern), r.Since, r.InReplyTo)
```

- [ ] **Step 6: Add the advancing-read handler**

In `server/agent_handler.go`, next to `agentHandleInbox`:

```go
// agentHandleInboxAdvance serves the read that moves the task's delivery mark.
// Only the runner-injected UserPromptSubmit hook sends it. The body is
// agentHandleInbox's, minus the client-supplied cursor and the next_cursor in
// the reply: the position is the server's, so the client neither sends nor
// receives one.
func (s *Server) agentHandleInboxAdvance(conn ConnHandle, ac *agentConn, r *agentboard.InboxAdvanceRequest) {
	if !ac.helloed || r == nil {
		return
	}
	msgs := s.Board.InboxAdvance(ac.state)
	delivered := make([]agentboard.DeliveredMessage, 0, len(msgs))
	pending := make([]pendingPayload, 0, len(msgs))
	for _, m := range msgs {
		stream, streamID, werr := openDeliveredPayloadStream(conn)
		if werr != nil {
			slog.Warn("agent_handler: inbox_advance deliver stream", "seq", m.Seq, "err", werr)
			continue
		}
		pending = append(pending, pendingPayload{stream: stream, payload: m.Payload})
		dm := agentboard.DeliveredMessage{
			Seq:             m.Seq,
			InReplyTo:       m.InReplyTo,
			PayloadStreamId: streamID,
			FromRunnerId:    protoToAgentboardRunnerID(m),
			FromTaskId:      protoToAgentboardTaskID(m),
		}
		dm.SetTopic([]byte(m.Topic))
		dm.SetFromHostname([]byte(m.FromHostname))
		dm.SetFromAgentProfile([]byte(m.FromAgentProfile))
		delivered = append(delivered, dm)
	}
	out := agentboard.InboxAdvanceResponse{RequestId: r.RequestId}
	out.SetMsgs(delivered)
	resp := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_InboxAdvanceResponse}
	resp.SetInboxAdvanceResponse(out)
	s.sendAgent(conn, resp)
	go flushDeliveredPayloads(pending)
}
```

- [ ] **Step 7: Wire it into the dispatch switch**

In `handleAgentMessage`, alongside `case agentboard.AgentMessageKind_Inbox:`:

```go
	case agentboard.AgentMessageKind_InboxAdvance:
		s.agentHandleInboxAdvance(conn, ac, msg.InboxAdvance())
```

- [ ] **Step 8: Verify**

Run: `make check && go test ./agentboard/ ./server/ -count=1`
Expected: PASS. `server`'s `TestOpenInteractive*` SessionMux test is known to be flaky at roughly one run in four and is unrelated to this diff — re-run before investigating.

- [ ] **Step 9: Commit**

```bash
git add agentboard/board.go agentboard/board_test.go server/agent_handler.go
git commit -m "feat(server): serve the in_reply_to wait filter and the advancing read

Board.Wait takes inReplyTo and skips publishes that do not answer that seq, so
a caller can wait for the reply to its own message rather than for whatever
lands next. agentHandleInboxAdvance serves the read that moves the task's
delivery mark; it carries no cursor in either direction.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 7: `agent inbox` — delete the cursor file, the peek/commit pair and `--stop-hook`

**Files:**
- Delete: `cli/agent/cursor.go`, `cli/agent/cursor_test.go`, `cli/agent/stop_hook.go`, `cli/agent/stop_hook_test.go`, `cli/agent/inbox_peek_commit_e2e_test.go`, `cli/agent/inbox_stop_hook_e2e_test.go`
- Modify: `cli/agent/inbox.go`, `cli/agent/json_emit.go:38` (comment only), `cli/agent/inbox_prompt_hook_e2e_test.go:129`
- Modify: `cmd/harness-cli/main.go:1098` (usage line)

**Interfaces:**
- Consumes: Task 5's `InboxAdvanceRequest` / `InboxAdvanceResponse`, Task 6's handler.
- Produces: `agent.Inbox(ctx, args, stdout)` keeps its signature. Flags after this task: `--json`, `--since N`, `--in-reply-to SEQ`, `--user-prompt-submit-hook`.

- [ ] **Step 1: Write the failing test**

Create `cli/agent/inbox_advance_e2e_test.go`, following the harness used by the existing `cli/agent` E2E tests (copy the server/attach setup from `cli/agent/inbox_prompt_hook_e2e_test.go`):

```go
package agent_test

import (
	"context"
	"strings"
	"testing"

	"github.com/on-keyday/agent-harness/cli/agent"
)

// The hook mode advances; a plain read does not. Two plain reads return the
// same thing, and the hook read that follows still sees the message.
func TestAgentCLI_E2E_PlainInboxDoesNotAdvance(t *testing.T) {
	ctx, taskB := setupTwoTaskBoard(t) // helper from inbox_prompt_hook_e2e_test.go
	publishTo(t, taskB.SelfTopic(), `{"msg":"hi"}`)

	var first, second strings.Builder
	if err := agent.Inbox(ctx, []string{"--json"}, &first); err != nil {
		t.Fatal(err)
	}
	if err := agent.Inbox(ctx, []string{"--json"}, &second); err != nil {
		t.Fatal(err)
	}
	if first.String() != second.String() {
		t.Fatalf("plain inbox is not idempotent:\nfirst=%q\nsecond=%q", first.String(), second.String())
	}
	if !strings.Contains(first.String(), `"msg":"hi"`) {
		t.Fatalf("plain inbox = %q, want the published message", first.String())
	}

	var hook strings.Builder
	if err := agent.Inbox(ctx, []string{"--json", "--user-prompt-submit-hook"}, &hook); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hook.String(), `"msg":"hi"`) {
		t.Fatalf("hook read = %q, want the message the plain reads did not consume", hook.String())
	}

	var again strings.Builder
	if err := agent.Inbox(ctx, []string{"--json", "--user-prompt-submit-hook"}, &again); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(again.String(), `"msg":"hi"`) {
		t.Fatalf("second hook read = %q, want the message gone", again.String())
	}
}

func TestAgentCLI_E2E_StopHookFlagIsGone(t *testing.T) {
	ctx, _ := setupTwoTaskBoard(t)
	var out strings.Builder
	if err := agent.Inbox(ctx, []string{"--stop-hook"}, &out); err == nil {
		t.Fatal("--stop-hook must no longer parse")
	}
}
```

If `setupTwoTaskBoard` / `publishTo` do not exist under those names, extract them from `inbox_prompt_hook_e2e_test.go`'s existing setup into shared helpers in that same file and use them from both.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cli/agent/ -run 'TestAgentCLI_E2E_PlainInbox|TestAgentCLI_E2E_StopHookFlagIsGone' -v`
Expected: FAIL — `--stop-hook` still parses, and the hook read still uses the cursor file.

- [ ] **Step 3: Delete the cursor and stop-hook files**

```bash
git rm cli/agent/cursor.go cli/agent/cursor_test.go \
       cli/agent/stop_hook.go cli/agent/stop_hook_test.go \
       cli/agent/inbox_peek_commit_e2e_test.go cli/agent/inbox_stop_hook_e2e_test.go
```

- [ ] **Step 4: Rewrite `Inbox`**

Replace the doc comment and the flag/request section of `cli/agent/inbox.go`:

```go
// Inbox returns the JSON-Lines dump of messages on subscribed topics.
//
// Two reads exist, and the flags pick between them:
//
//   - plain (the default): every retained message above --since (0 = the whole
//     ring). Idempotent — it moves nothing. This is what an agent runs by hand,
//     and what a runtime with no UserPromptSubmit hook polls.
//   - advancing (--user-prompt-submit-hook): the messages the automatic
//     injection path has not yet been given, marked as given by the server in
//     the same operation. Only the runner-injected hook sends it
//     (runner/settings.go). There is no flag that advances without also
//     producing the hook envelope, so advancing by hand is not a thing a
//     caller can do by mistake.
//
// --in-reply-to filters the emitted records to replies to that seq. It is
// presentational only: the advancing read still marks every message the server
// returned, so a filtered run does not re-deliver what it hid.
func Inbox(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("agent inbox", flag.ContinueOnError)
	serverCID := fs.String("server-cid", "", "")
	since := fs.Uint64("since", 0, "return messages above this seq (0 = the whole ring)")
	asJSON := fs.Bool("json", false, "output JSON Lines (current default; flag accepted for forward compat)")
	promptHook := fs.Bool("user-prompt-submit-hook", false, "advancing read, wrapped as Claude Code UserPromptSubmit additionalContext")
	inReplyTo := fs.Uint64("in-reply-to", 0, "only show messages replying to this seq (client-side filter)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	_ = asJSON // currently always JSON Lines

	conn, err := ConnectAgent(ctx, Flags{ServerCID: *serverCID})
	if err != nil {
		return err
	}
	defer conn.Close()

	reqID := rand.Uint32()
	respCh := make(chan []agentboard.DeliveredMessage, 1)
	conn.SetOnControl(func(kind appwire.AppKind, p []byte) {
		if kind != appwire.AppKind_AgentMessage {
			return
		}
		msg := &agentboard.AgentMessage{}
		if _, err := msg.Decode(p); err != nil {
			return
		}
		switch msg.Kind {
		case agentboard.AgentMessageKind_InboxResponse:
			if r := msg.InboxResponse(); r != nil && r.RequestId == reqID {
				select {
				case respCh <- r.Msgs:
				default:
				}
			}
		case agentboard.AgentMessageKind_InboxAdvanceResponse:
			if r := msg.InboxAdvanceResponse(); r != nil && r.RequestId == reqID {
				select {
				case respCh <- r.Msgs:
				default:
				}
			}
		}
	})

	var msg *agentboard.AgentMessage
	if *promptHook {
		msg = &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_InboxAdvance}
		msg.SetInboxAdvance(agentboard.InboxAdvanceRequest{RequestId: reqID})
	} else {
		msg = &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_Inbox}
		msg.SetInbox(agentboard.InboxRequest{RequestId: reqID, Since: *since})
	}
	if err := conn.SendRaw(msg); err != nil {
		return err
	}
	// ... the existing receive/emit block follows, reading from respCh and
	//     using *promptHook where it used to branch on *stopHook || *promptHook
```

In the receive/emit block: delete the `case *stopHook: emitStopHookOutput(...)` branch and the `SaveCursor` call, and keep the `emitUserPromptSubmitHookOutput` branch under `if *promptHook`.

- [ ] **Step 5: Drop the mutual-exclusion test case**

In `cli/agent/inbox_prompt_hook_e2e_test.go`, delete the block at `:129` that asserts `--stop-hook --user-prompt-submit-hook` is rejected, and any cursor-file assertions in that file.

- [ ] **Step 6: Fix the usage line**

`cmd/harness-cli/main.go:1098`:

```go
	fmt.Fprintln(os.Stderr, "  inbox [--since N] [--in-reply-to SEQ]     idempotent dump of subscribed topics; --since 0 (default) is the whole ring")
```

- [ ] **Step 7: Verify**

Run: `make check && go test ./cli/agent/ -count=1`
Expected: PASS, including the two new tests.

- [ ] **Step 8: Confirm nothing still references the cursor**

Run: `grep -rn 'LoadCursor\|SaveCursor\|since-last\|agent-cursor\|stop-hook' --include=*.go .`
Expected: no hits outside `docs/`.

- [ ] **Step 9: Commit**

```bash
git add -A cli/agent cmd/harness-cli/main.go
git commit -m "feat(cli): agent inbox reads idempotently; only the hook advances

Deletes ~/.cache/harness/agent-cursor-<task>, --since-last, --commit, the
prev/live two-line format and the peek it existed for. The advancing read is
selected by --user-prompt-submit-hook alone, so 'never pass --commit by hand'
stops being an instruction and starts being the shape of the CLI. --since
stays: it names a bound for one read and writes nothing.

--stop-hook goes with them. runner/settings.go retired the Stop hook
deliberately and pruneStaleHarnessHooks removes any an older runner left, so
nothing installs the flag.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 8: `agent wait` — `--in-reply-to`, no cursor

**Files:**
- Modify: `cli/agent/wait.go`
- Modify: `cmd/harness-cli/main.go:1096-1097` (usage)
- Test: `cli/agent/wait_in_reply_to_e2e_test.go` (create)

**Interfaces:**
- Consumes: Task 5's `WaitRequest.InReplyTo`, Task 6's handler.
- Produces: `agent.Wait(ctx, args, stdout)` keeps its signature. Flags: `--topic`, `--since`, `--timeout`, `--in-reply-to`.

- [ ] **Step 1: Write the failing test**

Create `cli/agent/wait_in_reply_to_e2e_test.go`, using the same setup helpers as Task 7:

```go
package agent_test

import (
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli/agent"
)

func TestAgentCLI_E2E_WaitIgnoresNonMatchingReply(t *testing.T) {
	ctx, taskB := setupTwoTaskBoard(t)
	topic := taskB.SelfTopic()

	// Noise first: a plain publish and a reply to a different seq.
	publishTo(t, topic, `{"msg":"noise"}`)
	publishReplyTo(t, topic, 999, `{"msg":"wrong"}`)

	go func() {
		time.Sleep(100 * time.Millisecond)
		publishReplyTo(t, topic, 42, `{"msg":"right"}`)
	}()

	var out strings.Builder
	err := agent.Wait(ctx, []string{
		"--topic", topic, "--in-reply-to", "42", "--timeout", "3s",
	}, &out)
	if err != nil {
		t.Fatalf("wait returned %v, want the reply to seq 42", err)
	}
	if strings.Contains(out.String(), "noise") || strings.Contains(out.String(), "wrong") {
		t.Fatalf("wait = %q, want only the reply to seq 42", out.String())
	}
	if !strings.Contains(out.String(), "right") {
		t.Fatalf("wait = %q, want the reply to seq 42", out.String())
	}
}
```

`publishReplyTo(t, topic, parentSeq, body)` publishes with `--in-reply-to`; add it next to `publishTo` in the shared helper file.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cli/agent/ -run TestAgentCLI_E2E_WaitIgnoresNonMatchingReply -v`
Expected: FAIL — `flag provided but not defined: -in-reply-to`.

- [ ] **Step 3: Rewrite the flag and request block**

In `cli/agent/wait.go`, replace the flag set, the cursor block and the request:

```go
// Wait blocks until a message arrives on the given topic, or until --timeout.
// Output: JSON Lines on stdout, one line per delivered message.
//
// This is a shell-level tool for scripting OUTSIDE an agent's turn loop. An
// agent must not call it from inside a turn: it holds the process for the
// whole timeout, and replies arrive through the inbox hook anyway.
//
// --since is the caller's own resume position (WaitResponse.next_cursor from
// the previous call). Nothing is persisted: the position a script keeps is the
// script's, and the delivery mark the hook advances is the server's.
func Wait(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("agent wait", flag.ContinueOnError)
	serverCID := fs.String("server-cid", "", "server ConnectionID (env: HARNESS_SERVER_CID)")
	topic := fs.String("topic", "", "topic to wait on")
	since := fs.Uint64("since", 0, "return messages above this seq")
	inReplyTo := fs.Uint64("in-reply-to", 0, "only accept a message replying to this seq (0 = any)")
	timeout := fs.Duration("timeout", 5*time.Minute, "max block duration")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *topic == "" {
		return errors.New("--topic required")
	}

	conn, err := ConnectAgent(ctx, Flags{ServerCID: *serverCID})
	if err != nil {
		return err
	}
	defer conn.Close()
```

Then in the request:

```go
	wr := agentboard.WaitRequest{
		RequestId: reqID,
		Since:     *since,
		TimeoutMs: uint32(timeout.Milliseconds()),
		InReplyTo: *inReplyTo,
	}
	wr.SetPattern([]byte(*topic))
```

And in the response handling, delete the `if *sinceLast { SaveCursor(...) }` block. Keep the timeout error.

- [ ] **Step 4: Fix the usage lines**

`cmd/harness-cli/main.go:1096-1097`:

```go
	fmt.Fprintln(os.Stderr, "  wait --topic T [--since N] [--in-reply-to SEQ] [--timeout DUR]")
	fmt.Fprintln(os.Stderr, "                                       block until a matching message arrives (scripting; not from an agent turn)")
```

- [ ] **Step 5: Verify**

Run: `make check && go test ./cli/agent/ -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add cli/agent/wait.go cli/agent/wait_in_reply_to_e2e_test.go cmd/harness-cli/main.go
git commit -m "feat(cli): agent wait gains --in-reply-to and stops writing a cursor

--since-last wrote the hook's shared watermark using the max seq of ONE topic,
so taking a message on one topic skipped unread messages on the others. The
flag is gone; --since remains as the caller's own resume position.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 9: `dispatch` waits for the answer to its own publish

**Files:**
- Modify: `cli/agent/dispatch.go`
- Modify: `cmd/harness-cli/main.go:1103-1104` (usage)
- Test: `cli/agent/dispatch_correlation_e2e_test.go` (create)

**Interfaces:**
- Consumes: Task 5's `WaitRequest.InReplyTo`, Task 6's handler, `agentboard.SelfTopic`.
- Produces: `agent.Dispatch(ctx, args, stdin, stdout)` keeps its signature. Flags: `--topic`, `--data`, `--timeout`. `--reply-topic` is removed.

- [ ] **Step 1: Write the failing test**

Create `cli/agent/dispatch_correlation_e2e_test.go`:

```go
package agent_test

import (
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli/agent"
)

// Unrelated traffic on the dispatcher's own topic must not satisfy a dispatch.
func TestAgentCLI_E2E_DispatchIgnoresUnrelatedTraffic(t *testing.T) {
	ctx, taskB := setupTwoTaskBoard(t)

	publishTo(t, selfTopicOfTaskA(t), `{"msg":"unrelated"}`)

	go func() {
		time.Sleep(150 * time.Millisecond)
		seq := lastSeqOn(t, taskB.SelfTopic())
		publishReplyTo(t, "", seq, `{"msg":"the answer"}`)
	}()

	var out strings.Builder
	err := agent.Dispatch(ctx, []string{
		"--topic", taskB.SelfTopic(), "--data", "question", "--timeout", "3s",
	}, strings.NewReader(""), &out)
	if err != nil {
		t.Fatalf("dispatch returned %v, want the correlated reply", err)
	}
	if strings.Contains(out.String(), "unrelated") {
		t.Fatalf("dispatch = %q, want only the reply to its own publish", out.String())
	}
	if !strings.Contains(out.String(), "the answer") {
		t.Fatalf("dispatch = %q, want the correlated reply", out.String())
	}
}

func TestAgentCLI_E2E_DispatchRejectsReplyTopicFlag(t *testing.T) {
	ctx, taskB := setupTwoTaskBoard(t)
	var out strings.Builder
	err := agent.Dispatch(ctx, []string{
		"--topic", taskB.SelfTopic(), "--reply-topic", "whatever", "--data", "x",
	}, strings.NewReader(""), &out)
	if err == nil {
		t.Fatal("--reply-topic must no longer parse")
	}
}
```

`selfTopicOfTaskA` and `lastSeqOn` go next to the other helpers; `lastSeqOn` reads the topic's retained ring and returns the highest seq, which is the seq the dispatch just published.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cli/agent/ -run TestAgentCLI_E2E_Dispatch -v`
Expected: FAIL — `--reply-topic` still parses and the wait uses `Since: 0` with no filter.

- [ ] **Step 3: Rewrite `Dispatch`**

In `cli/agent/dispatch.go`, replace the doc comment, the flags, and the wait half:

```go
// Dispatch publishes to --topic and blocks until a message answering that
// publish arrives, all over one Hello'd connection. JSON-Lines output.
//
// There is no --reply-topic. A reply carrying --in-reply-to and no topic is
// routed by the server to the ORIGINAL SENDER's own chat.<short-id>
// (server/agent_handler.go resolveReplyTarget), so the reply topic is not a
// caller's choice: a supplied one could only disagree with where the reply
// actually lands. The wait is bounded below by the seq this call published and
// filtered to replies to it, so retained traffic already on the topic — and
// answers to somebody else's message — do not satisfy it.
//
// This is a shell-level tool for scripting OUTSIDE an agent's turn loop, for
// the same reason agent wait is.
func Dispatch(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	fs := flag.NewFlagSet("agent dispatch", flag.ContinueOnError)
	serverCID := fs.String("server-cid", "", "")
	topic := fs.String("topic", "", "topic to send to")
	data := fs.String("data", "-", `payload string or "-" for stdin`)
	timeout := fs.Duration("timeout", 5*time.Minute, "max wait")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *topic == "" {
		return errors.New("--topic required")
	}
```

Keep the payload read, the `refuseIfOwnTicket` call, the connect, the two response channels and the send half exactly as they are. Capture the published seq from the send response and use it for the wait:

```go
	var publishedSeq uint64
	select {
	case r := <-sendCh:
		if r.Status != agentboard.SendStatus_Ok {
			return fmt.Errorf("send failed: %v", r.Status)
		}
		publishedSeq = r.Seq
	case <-ctx.Done():
		return ctx.Err()
	}

	// Wait for the reply on OUR own topic, above our own publish, filtered to
	// answers to it.
	wr := agentboard.WaitRequest{
		RequestId: waitID,
		Since:     publishedSeq,
		TimeoutMs: uint32(timeout.Milliseconds()),
		InReplyTo: publishedSeq,
	}
	wr.SetPattern([]byte(agentboard.SelfTopic(conn.TaskID())))
```

The rest of the wait half is unchanged.

- [ ] **Step 4: Fix the usage lines**

`cmd/harness-cli/main.go:1103-1104`:

```go
	fmt.Fprintln(os.Stderr, "  dispatch --topic T --data D|- [--timeout DUR]")
	fmt.Fprintln(os.Stderr, "                                       send, then block for the reply to THAT message (scripting; not from an agent turn)")
```

- [ ] **Step 5: Verify**

Run: `make check && go test ./cli/agent/ -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add cli/agent/dispatch.go cli/agent/dispatch_correlation_e2e_test.go cmd/harness-cli/main.go
git commit -m "fix(cli): dispatch waits for the answer to its own publish

It waited on a caller-named topic with Since:0 and no correlation, so anything
already retained there satisfied it. It now waits on its own chat.<short-id> --
where resolveReplyTarget actually routes a reply -- above the seq it published
and filtered to replies to that seq. --reply-topic is removed: a caller-chosen
reply topic could only disagree with where the reply lands.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 10: The injected hook command line

**Files:**
- Modify: `runner/settings.go:37-39` (`harnessHookEntries`), `:86-95` (doc comment)
- Modify: `.claude/settings.json:7`
- Test: `runner/settings_test.go`

**Interfaces:**
- Consumes: Task 7's flag set.
- Produces: the exact string `harness-cli agent inbox --json --user-prompt-submit-hook`, which `pruneStaleHarnessHooks` uses as the current-entry key.

- [ ] **Step 1: Write the failing test**

Append to `runner/settings_test.go`:

```go
func TestWriteAgentSettings_PrunesTheOldCursorHookLine(t *testing.T) {
	dir := t.TempDir()
	seed := map[string]any{
		"hooks": map[string]any{
			"UserPromptSubmit": []any{
				map[string]any{
					"hooks": []any{
						map[string]any{
							"type":    "command",
							"command": "harness-cli agent inbox --since-last --commit --json --user-prompt-submit-hook",
						},
					},
				},
			},
		},
	}
	writeSettingsJSON(t, dir, seed) // existing helper in this file

	if err := WriteAgentSettings(dir); err != nil {
		t.Fatal(err)
	}
	hooks := readSettingsHooks(t, dir) // existing helper in this file
	groups, _ := hooks["UserPromptSubmit"].([]any)
	if groupCommandSearch(groups, "harness-cli agent inbox --since-last --commit --json --user-prompt-submit-hook") {
		t.Error("the old cursor-based hook line must be pruned")
	}
	if !groupCommandSearch(groups, "harness-cli agent inbox --json --user-prompt-submit-hook") {
		t.Error("the current hook line must be present")
	}
}
```

If `writeSettingsJSON` / `readSettingsHooks` do not exist under those names, use whatever the neighbouring tests in `runner/settings_test.go` already use to seed and read a settings file — do not add a second helper doing the same job.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./runner/ -run TestWriteAgentSettings_PrunesTheOldCursorHookLine -v`
Expected: FAIL — the old line survives because it is still the current entry.

- [ ] **Step 3: Change the entry**

`runner/settings.go`:

```go
var harnessHookEntries = []struct {
	Event   string
	Command string
}{
	{"UserPromptSubmit", "harness-cli agent inbox --json --user-prompt-submit-hook"},
}
```

- [ ] **Step 4: Rewrite the stale doc comment**

Replace the paragraph at `runner/settings.go:86-95` (the one describing the `--since-last` cursor and `--commit`) with:

```go
// The delivery mark lives on the SERVER, per (task, topic), in
// agentboard.taskState. --user-prompt-submit-hook is the only caller that
// advances it: the hook asks for what has not been injected and the server
// marks it injected in the same operation. A manual `harness-cli agent inbox`
// is a plain read of the subscribed topics' rings and moves nothing, so an
// agent can re-read freely. See agentboard.Board.InboxAdvance.
```

- [ ] **Step 5: Update the checked-in settings file**

`.claude/settings.json`:

```json
            "command": "harness-cli agent inbox --json --user-prompt-submit-hook",
```

- [ ] **Step 6: Verify**

Run: `go test ./runner/ -count=1`
Expected: PASS. The existing tests asserting the Stop event is pruned still pass — that behaviour is unchanged.

- [ ] **Step 7: Commit**

```bash
git add runner/settings.go runner/settings_test.go .claude/settings.json
git commit -m "feat(runner): the injected hook drops --since-last --commit

pruneStaleHarnessHooks removes the old line from worktrees on the next
WriteAgentSettings, the same way it removes the retired Stop hook.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 11: The delivery mark on the operator surfaces

The position used to be a file on a runner host that no surface showed. All three surfaces already have a board-subscribers view, so all three get the new columns.

**Files:**
- Modify: `agentboard/board.go:637-679` (`SubscriberRow`, `ListSubscribers`)
- Modify: `server/board_handler.go:54-73`
- Modify: `cli/board.go:55-60`, `cli/cmd_board.go:184-202`
- Modify: `tui/board.go:548-556`
- Modify: `cmd/harness-webui-wasm/main.go:1137-1151`, `webui/static/main.js:5097-5106`
- Test: `agentboard/board_test.go`

**Interfaces:**
- Consumes: Task 4's `shownSnapshot`, Task 5's `protocol.SubscriptionPattern.Shown` / `.Pending`.
- Produces: `agentboard.SubscriberPattern{Name string; Shown uint64; Pending uint32}` and `SubscriberRow.Patterns []SubscriberPattern`; `cli.BoardSubscriberRow.Patterns []cli.BoardSubscriberPattern` with the same three fields.

- [ ] **Step 1: Write the failing test**

Append to `agentboard/board_test.go`:

```go
func TestBoard_ListSubscribersCarriesShownAndPending(t *testing.T) {
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	defer b.Close()
	conn := b.Attach(RunnerID{}, TaskID{}, "test-host", "")
	defer b.Detach(conn)
	_ = b.Subscribe(conn, "topic/watched")

	for _, body := range []string{"a", "b", "c"} {
		if _, _, err := b.Send("topic/watched", []byte(body), testRid, testTid, "h", "", 0); err != nil {
			t.Fatal(err)
		}
	}
	b.InboxAdvance(conn) // shows all three
	if _, _, err := b.Send("topic/watched", []byte("d"), testRid, testTid, "h", "", 0); err != nil {
		t.Fatal(err)
	}

	rows := b.ListSubscribers("")
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	var got SubscriberPattern
	for _, p := range rows[0].Patterns {
		if p.Name == "topic/watched" {
			got = p
		}
	}
	if got.Name == "" {
		t.Fatal("topic/watched missing from the row's patterns")
	}
	if got.Shown == 0 {
		t.Error("Shown = 0, want the seq of the third message")
	}
	if got.Pending != 1 {
		t.Errorf("Pending = %d, want 1 (the message published after the advance)", got.Pending)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./agentboard/ -run TestBoard_ListSubscribersCarries -v`
Expected: FAIL — `SubscriberPattern` undefined; `Patterns` is `[]string`.

- [ ] **Step 3: Widen the board row type**

In `agentboard/board.go`:

```go
// SubscriberPattern is one subscribed topic with this task's delivery position
// on it. Shown is the highest seq the automatic injection path has given the
// task; Pending is how many retained messages sit above it. A topic with a
// live synchronous wait appears here like any other subscription, because for
// the duration of that wait the task really is a subscriber.
type SubscriberPattern struct {
	Name    string
	Shown   uint64
	Pending uint32
}

type SubscriberRow struct {
	Task         protocol.TaskID
	Hostname     string
	AgentProfile string
	Patterns     []SubscriberPattern
}
```

And in `ListSubscribers`, replace the `Patterns: ts.snapshotPatterns()` line:

```go
		marks := ts.shownSnapshot()
		names := ts.snapshotPatterns()
		pats := make([]SubscriberPattern, 0, len(names))
		for _, n := range names {
			shown := marks[n]
			var pendingN uint32
			b.mu.Lock()
			tp, ok := b.topics[n]
			b.mu.Unlock()
			if ok {
				for _, m := range tp.snapshot() {
					if m.Seq > shown {
						pendingN++
					}
				}
			}
			pats = append(pats, SubscriberPattern{Name: n, Shown: shown, Pending: pendingN})
		}
		out = append(out, SubscriberRow{
			Task:         tid,
			Hostname:     host,
			AgentProfile: profile,
			Patterns:     pats,
		})
```

- [ ] **Step 4: Carry it through the server handler**

`server/board_handler.go`, in `handleBoardSubscribers`:

```go
		patterns := make([]protocol.SubscriptionPattern, 0, len(r.Patterns))
		for _, p := range r.Patterns {
			var sp protocol.SubscriptionPattern
			sp.SetName([]byte(p.Name))
			sp.Shown = p.Shown
			sp.Pending = p.Pending
			patterns = append(patterns, sp)
		}
		row.SetPatterns(patterns)
```

- [ ] **Step 5: Carry it through the client type**

`cli/board.go`:

```go
// BoardSubscriberPattern is one subscribed topic plus this task's delivery
// position on it — see agentboard.SubscriberPattern.
type BoardSubscriberPattern struct {
	Name    string
	Shown   uint64
	Pending uint32
}

type BoardSubscriberRow struct {
	TaskHex      string
	Hostname     string
	AgentProfile string
	Patterns     []BoardSubscriberPattern
}
```

Fill the three fields where the function currently builds `Patterns` from the response.

- [ ] **Step 6: CLI rendering**

`cli/cmd_board.go`, in `case "subscribers"`:

```go
		for _, r := range rows {
			pats := "-"
			if len(r.Patterns) > 0 {
				parts := make([]string, 0, len(r.Patterns))
				for _, p := range r.Patterns {
					parts = append(parts, fmt.Sprintf("%s(shown=%d pending=%d)", p.Name, p.Shown, p.Pending))
				}
				pats = strings.Join(parts, ",")
			}
			fmt.Fprintf(out, "%s host=%s agent=%s topics=%s\n",
				r.TaskHex, boardHostOrDash(r.Hostname), boardAgentOrDash(r.AgentProfile), pats)
		}
```

`board topics` also consumes `BoardSubscribers("")` to count subscribers per name (`cli/cmd_board.go:59-66`); change that loop to `subs[pat.Name]++`.

- [ ] **Step 7: TUI rendering**

`tui/board.go`, in the subscribers view where `pats` is built, and in `DoBoardTopics`'s subscriber counting (`tui/board.go:66`):

```go
			parts := make([]string, 0, len(r.Patterns))
			for _, p := range r.Patterns {
				parts = append(parts, fmt.Sprintf("%s(shown=%d pending=%d)", p.Name, p.Shown, p.Pending))
			}
			pats := "-"
			if len(parts) > 0 {
				pats = strings.Join(parts, ",")
			}
```

- [ ] **Step 8: WASM bridge**

`cmd/harness-webui-wasm/main.go`, in `harnessBoardSubscribers`:

```go
			for _, pt := range r.Patterns {
				pats = append(pats, map[string]any{
					"name":    pt.Name,
					"shown":   float64(pt.Shown),
					"pending": float64(pt.Pending),
				})
			}
```

`patterns` becomes an array of objects rather than strings. Check every other place in this file that reads `r.Patterns` (the `boardTopics` subscriber count near `:1098`) and use `pt.Name` there.

- [ ] **Step 9: WebUI rendering**

`webui/static/main.js`, in `renderBoardSubscribers`:

```js
        const pats = (r.patterns && r.patterns.length)
          ? r.patterns.map((p) => `${p.name}(shown=${p.shown} pending=${p.pending})`).join(",")
          : "-";
```

And in `renderBoardTopics`, where it counts subscribers per pattern from `boardSubscribers("")` (near `:4874`), read `p.name` instead of the bare string.

- [ ] **Step 10: Walk the surface-parity checklist**

Invoke the `surface-parity-checklist` skill and walk items 1–37 plus S1–S6, recording a verdict per number. `board subscribers` has no TUI cmdline or WebUI command-input entry point today; record that as an existing shape, not as a new omission.

- [ ] **Step 11: Verify**

Run: `make check && make wasm-check && make vet && go test ./... -count=1`
Expected: PASS.

- [ ] **Step 12: Commit**

```bash
git add agentboard/ server/board_handler.go cli/board.go cli/cmd_board.go tui/board.go cmd/harness-webui-wasm/main.go webui/static/main.js
git commit -m "feat: board subscribers shows each task's delivery position

Per subscribed topic: shown (the highest seq the injection path has given the
task) and pending (how many retained messages sit above it). The position used
to be a file on the runner host that no operator surface displayed, which is
why the skill told agents to guess at a desync and re-read everything. CLI, TUI
and WebUI all carry it.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 12: Documentation

**Files:**
- Modify: `runner/agentskills/harness-cli/SKILL.md` (the `go:embed` source), then copy to `.claude/skills/harness-cli/SKILL.md` and `.agents/skills/harness-cli/SKILL.md`
- Modify: `agentboard/board.go:18-28` (`SeqSeed` comment)

**Interfaces:**
- Consumes: the finished behaviour from Tasks 7–11.
- Produces: no code surface.

- [ ] **Step 1: Read the skill in full**

Read `runner/agentskills/harness-cli/SKILL.md` end to end before editing. An addition is a claim that the content is not already there, and a windowed read cannot support it.

- [ ] **Step 2: Delete the desync section**

Remove the whole **"Known issue — `--since-last` can desync (interactive sessions)"** block (`:149-161` in the current file). The mechanism it describes is gone: no client-side cursor, and the only advancing reader is the hook.

- [ ] **Step 3: Rewrite the hook description**

Replace the paragraphs describing `--since-last`, the read-only peek and **"Never pass `--commit` by hand"** with:

```markdown
- `UserPromptSubmit` runs
  `harness-cli agent inbox --json --user-prompt-submit-hook`
  (delivers any pending messages at the start of a turn).

The delivery position lives on the SERVER, per (task, topic). The hook is the
only caller that advances it: it asks for what has not been injected, and the
server marks it injected in the same operation. `harness-cli agent inbox` run
by hand is a plain, idempotent read of your subscribed topics' rings — it
moves nothing, so re-read as often as you like. `--since N` bounds a read from
below when you are polling and want only what is new to you; it writes nothing.

An operator can see where you are: `harness-cli board subscribers` shows
`shown=` and `pending=` per topic.
```

- [ ] **Step 4: Update the wait/dispatch description**

In the "Async by default" section, keep every sentence of the rule — it is unchanged. Update only the mechanics: `wait` takes `--topic`, `--since`, `--in-reply-to`, `--timeout`; `dispatch` takes `--topic`, `--data`, `--timeout` and waits for the reply to its own publish on your own `chat.<short-id>`. Delete every mention of `--since-last`, `--commit` and `--reply-topic`.

- [ ] **Step 5: Sync the two copies**

```bash
cp runner/agentskills/harness-cli/SKILL.md .claude/skills/harness-cli/SKILL.md
cp runner/agentskills/harness-cli/SKILL.md .agents/skills/harness-cli/SKILL.md
diff -q runner/agentskills/harness-cli/SKILL.md .claude/skills/harness-cli/SKILL.md
diff -q runner/agentskills/harness-cli/SKILL.md .agents/skills/harness-cli/SKILL.md
```

Expected: both `diff` calls silent.

- [ ] **Step 6: Rewrite the `SeqSeed` comment**

`agentboard/board.go:18-28`. The old text explains that on-disk cursors survive a restart while `b.seq` does not, and that the seed out-runs them. That reason is gone with the cursor file. Replace with:

```go
	// SeqSeed is the starting value for the board-global publish sequence
	// counter (b.seq). The server seeds it with a strictly-increasing boot
	// epoch (wall-clock ms << 20) so seq keeps rising across restarts. The
	// delivery mark no longer depends on this — it lives in taskState and
	// dies with the board it indexes. What monotonicity still buys is
	// readability: a seq named in a log line, a `retract <seq>` or an
	// `agent read <seq>` cannot silently mean a different message from an
	// earlier boot. Zero (the default, used by tests) preserves the legacy
	// seq=1,2,3… behavior.
```

- [ ] **Step 7: Confirm no stale references remain**

```bash
grep -rn 'since-last\|--commit\|agent-cursor\|reply-topic\|stop-hook' \
  runner/agentskills/ .claude/skills/ .agents/skills/ CLAUDE.md
```

Expected: no hits.

- [ ] **Step 8: Verify the embed still builds**

Run: `make check && go test ./runner/ -count=1`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add runner/agentskills/harness-cli/SKILL.md .claude/skills/harness-cli/SKILL.md .agents/skills/harness-cli/SKILL.md agentboard/board.go
git commit -m "docs: the delivery position is server-side, and the desync section goes

Deletes 'Known issue -- --since-last can desync' and the 'Never pass --commit
by hand' instruction: neither has a mechanism left. Adds board subscribers'
shown/pending as the way to SEE the position instead of guessing at it. The
rule that an agent must not call wait/dispatch inside a turn is unchanged --
the wake misfire was never its reason.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

## Rollout (after Task 12)

- [ ] **Restart the SERVER first.** The runner fleet is on other hosts and stays on the old binary until restarted. The reverse order puts a new runner against an old server.
- [ ] `make build` on the runner host, then `RUN scripts/build_and_restart_all.py`.
- [ ] Already-running tasks keep the old hook line (`--since-last --commit`) until their next task, so their hooks exit non-zero on flag parse until then. This is accepted, not shimmed: no external users, one task lifetime, and a parse failure is loud.
- [ ] Live check: `harness-cli board subscribers` shows `shown=` / `pending=` per topic, and a `send` to a peer's `chat.<short-id>` moves that peer's `pending` by one.
- [ ] Live check: with an interactive session idle, run `harness-cli agent wait --topic <its self topic> --timeout 30s` from a second shell, publish to that topic, and confirm the session's screen is unchanged (`harness-cli session snapshot`) while the wait returns the message.

## Self-Review Notes

Spec coverage, section by section:

| Spec item | Task |
| --- | --- |
| Problem 1 — sync wait wakes its own PTY | 1, 2, 3 |
| Problem 2 — `wait --since-last` skips other topics | 4, 7, 8 |
| Problem 3 — `Board.Wait` subscribes permanently | 1 |
| Problem 4 — `dispatch` uncorrelated | 5, 6, 9 |
| Problem 5 — `--stop-hook` has no installer | 7 |
| Problem 6 — position on the wrong side | 4, 5, 6, 7, 10 |
| Problem 7 — no design record for peek/commit | 7 (peek deleted), 12 |
| Design §1 storage | 1, 4 |
| Design §2 wire schema | 5 |
| Design §3 server | 3, 4, 6 |
| Design §4 agent CLI | 7, 8, 9 |
| Design §5 operator surfaces | 11 |
| Design §6 documentation | 12 |
| Rollout | Rollout section |
| Testing | folded into each task, plus the Rollout live checks |
