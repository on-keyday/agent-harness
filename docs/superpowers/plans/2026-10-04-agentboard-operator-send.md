# Operator publishes to the agentboard — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the operator publish to the agentboard under its own name (`board send`), optionally without waking recipients, and wake a topic's subscribers without publishing (`board wake`), on CLI, TUI and WebUI.

**Architecture:** One `.bgn` change adds a `SenderKind` to every message row, two operator-face TaskControl kinds, a capability bit and a `no_reply_route` status. `agentboard` gains send options (`WithSenderKind`, `WithNoWake`), a `Wake` method, and the reserved topic `chat.operator`. The server adds two handlers next to `handleBoardRetract`. The client gets a shared payload-stream helper (extracted from `agent send`). Then each surface wires the verbs and renders the sender kind.

**Tech Stack:** Go, `.bgn` → ebm2go (`make protoregen`), `cli/verb` codegen (`go generate ./cli/verb`), bubbletea TUI, Go→wasm bridge + plain JS WebUI.

**Spec:** `docs/superpowers/specs/2026-10-04-agentboard-operator-send-design.md`

## Global Constraints

- Work in this worktree: `/home/kforfk/workspace/remote-agent-harness/.harness-worktrees/70fbad4a6eb6f1e992be8a669f1bcefd`. Every absolute path you pass to a tool must start with it; a bare `/home/kforfk/workspace/remote-agent-harness/<rel>` writes the PARENT checkout.
- Read `.claude/skills/implementation-pitfalls/SKILL.md` in full before writing code.
- `agent send`, `agent dispatch` and `AgentSendRequest` keep their wire format and CLI surface. `--no-wake` must not appear on any `agent` verb.
- No change to the runner, `TaskWakeRequest`, or `wakeMarker`.
- `TaskControlKind` is a positional `:u8`: append new kinds after `await_idle_kill`, never insert.
- The new capability is `board_send = 0x20000`; `all` becomes `0x3ffff`.
- The reserved topic name is exactly `chat.operator`.
- Verify with make targets (`make test`, `make build`), not ad-hoc `go build`. Compile-check with `go build ./...` or `go vet`; never a bare `go build ./cmd/<x>` (it drops a binary into the worktree).
- Commits end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.
- Do not land (push) as part of this plan; landing is the controller's step after review.

## Review Focus

1. **Reply to an operator message with `--in-reply-to` alone.** Expected: `no_reply_route`, and an error that says the message came from the operator and has no reply destination. It must not be a silent publish to `chat.00000000`. Pinned in Task 3 and Task 5.
2. **An operator reply from TUI/WebUI, then the agent answers it with `--in-reply-to` alone.** Expected: the answer lands on `chat.operator` and wakes nobody. Pinned in Task 5 (e2e) and verified by hand in Task 9.
3. **`--no-wake` message, then `board wake`.** Expected: the message is retained and appears in the next inbox read; the send emits no wake; the wake emits one wake per subscribed task; `board wake` on a topic nobody subscribes reports `woken: 0`, not an error. Pinned in Task 2 and Task 5.
4. **An agent without `board_send` calls `BoardSend`/`BoardWake`.** Expected: an ordinary capability denial naming `board_send`. Pinned in Task 3.
5. **Display of a zero `from_task` as a sender.** Expected: `operator` (or `server`) wherever a task short-id appears today. `00000000` must not appear in `board read`, `board thread`, the conversation key, the TUI modal, or the WebUI. Pinned in Task 6, Task 7 and Task 8.

---

### Task 1: Wire schema (the whole `.bgn` change, in one place)

**Files:**
- Modify: `runner/protocol/message.bgn`
- Regenerate: `runner/protocol/message.go` (via `make protoregen`)
- Modify: `runner/protocol/capability_test.go`
- Modify: `server/capabilities.go` (requiredCap)
- Modify: `server/cap_completeness_test.go`, `server/scope_completeness_test.go`, `server/scope_percap_completeness_test.go`
- Modify: `cli/verb/caps.go` (`GrantableCaps`, `CapDescription`)

**Interfaces:**
- Produces:
  - `protocol.SenderKind` with values `SenderKind_Agent` (0), `SenderKind_Operator`, `SenderKind_Server`.
  - Field `SenderKind protocol.SenderKind` on `DeliveredMessage`, `RetainedMeta`, `BoardMessageRow`.
  - `protocol.SendStatus_NoReplyRoute`.
  - `protocol.TaskControlKind_BoardSend`, `protocol.TaskControlKind_BoardWake`.
  - `protocol.BoardSendRequest` with `InReplyTo uint64`, `Topic []byte` (`SetTopic`), `PayloadStreamId uint64`, `NoRetireOnReply()/SetNoRetireOnReply(bool)`, `NoWake()/SetNoWake(bool)`, `ReplyToTopic []byte` (`SetReplyToTopic`).
  - Response arm `board_send` typed `AgentSendResponse`, accessors `resp.BoardSend()` / `resp.SetBoardSend(...)`.
  - `protocol.BoardWakeRequest{Topic}` with `SetTopic`, and `protocol.BoardWakeResponse{RequestId uint32, Woken uint16}`, accessors `BoardWake()/SetBoardWake`.
  - `protocol.Capability_BoardSend` = 0x20000.

- [ ] **Step 1: Add `SenderKind` and use it on the three row formats**

In `runner/protocol/message.bgn`, directly above `format BoardMessageRow:` (line ~1166), add:

```
# SenderKind says WHO published a message, which the from_* identity fields
# cannot: an operator and a server-originated publish both carry a zero (or
# borrowed) from_task. Before this field the server's await-idle publish was
# recognisable only by from_hostname "server", a convention; the kind carries
# it. agent is 0 so a row nobody stamped means what every row meant before.
enum SenderKind:
    :u8
    agent
    operator
    server
```

Append as the LAST field of each of `format BoardMessageRow`, `format DeliveredMessage` (~1405) and `format RetainedMeta` (~1525):

```
    # Who published it; see SenderKind. from_task is the caller's principal
    # task for an operator publish (zero for a real operator connection).
    sender_kind :SenderKind
```

Rewrite the `from_agent_profile` comment in `DeliveredMessage` that says a server-originated publish "carries from_hostname \"server\"" to say it carries `sender_kind = server` (the hostname is still `"server"`, for older readers).

- [ ] **Step 2: Add `no_reply_route` to `SendStatus`**

At `enum SendStatus` (~1356), append after `unknown_in_reply_to`:

```
    # The parent of a reply was published by the operator or the server and
    # declared no reply_to_topic, and the reply named no topic of its own.
    # The fallback (the author's chat.<short-id>) would be the chat topic of
    # a zero task id, so the server refuses instead of guessing.
    no_reply_route
```

- [ ] **Step 3: Append the two kinds to `TaskControlKind`**

After `await_idle_kill` (~541) and its comment, append:

```

    # --- agentboard, operator face: publishing ---
    #
    # Gated on board_send. These publish (or wake) IN THE OPERATOR'S NAME: the
    # message is stamped sender_kind=operator whoever the caller is, with the
    # caller's principal task as from_task. That is why they are not agent_
    # kinds: agent_send is keyed to the caller's own identity and takes no bit.
    board_send              # publish; may suppress the wake (no_wake)
    board_wake              # wake a topic's subscribers; publishes nothing
```

- [ ] **Step 4: Add the request/response formats**

Directly after `format AgentSendResponse` and its fields, add:

```
# BoardSendRequest is agent_send's request on the operator face: the same
# body transport and reply routing, plus no_wake. Its own format rather than a
# bit on AgentSendRequest so that the agent face cannot express no_wake at
# all -- an agent that suppressed its own wake by misunderstanding would
# report the message as undelivered.
format BoardSendRequest:
    request_id :u32
    # See AgentSendRequest.in_reply_to.
    in_reply_to :u64
    topic_len :u16
    topic :[topic_len]u8
    topic_len != 0 || in_reply_to != 0
    # See AgentSendRequest.payload_stream_id.
    payload_stream_id :u64
    # See AgentSendRequest.no_retire_on_reply.
    no_retire_on_reply :u1
    # no_wake publishes and retains the message but emits no task_wake to any
    # subscriber. The message reaches them on their next inbox read (the
    # UserPromptSubmit hook), or when a board_wake on the topic wakes them.
    no_wake :u1
    reserved :u6
    # See AgentSendRequest.reply_to_topic. An operator message without one has
    # no reply route: a reply to it that names no topic is refused with
    # SendStatus.no_reply_route.
    reply_to_topic_len :u16
    reply_to_topic :[reply_to_topic_len]u8

# BoardWakeRequest wakes every task subscribed to topic, exactly as a publish
# there would, and publishes nothing. Meant for after one or more no_wake
# sends; it does not check whether anything is unread.
format BoardWakeRequest:
    request_id :u32
    topic_len :u16
    topic :[topic_len]u8

format BoardWakeResponse:
    request_id :u32
    # How many tasks a task_wake was emitted for. A task waiting on the topic
    # is not counted (the wait already hands it the messages). Clamped to u16.
    woken :u16
```

Copy the exact length/assertion syntax from `AgentSendRequest` (lines ~1300-1350) if anything above disagrees with the file's conventions.

- [ ] **Step 5: Add the union arms**

In `format TaskControlRequest` (~1900), after the `await_idle_kill` arm and before `.. => error(...)`:

```
        TaskControlKind.board_send => board_send :BoardSendRequest
        TaskControlKind.board_wake => board_wake :BoardWakeRequest
```

In `format TaskControlResponse` (~1967), same position:

```
        TaskControlKind.board_send => board_send :AgentSendResponse
        TaskControlKind.board_wake => board_wake :BoardWakeResponse
```

- [ ] **Step 6: Add the capability bit**

In `enum Capability` (~3108), after `forward_tap = 0x10000`, add:

```
    # board_send authorizes publishing to the agentboard IN THE OPERATOR'S
    # NAME (board send, board wake): the message is stamped
    # sender_kind=operator. Operators hold it through all; for a task it is
    # the permission to speak as the operator. Default-off, appended.
    board_send     = 0x20000, "board_send"
```

and change `all = 0x1ffff` to `all = 0x3ffff`.

- [ ] **Step 7: Regenerate**

Run: `make protoregen`
Expected: only `runner/protocol/message.go` changes (`git status --short`).

- [ ] **Step 8: Fix the bit pin**

In `runner/protocol/capability_test.go`, change `0x1ffff` to `0x3ffff` (both the comparison and the message), and add `| Capability_BoardSend` to the OR list.

- [ ] **Step 9: Gate the kinds and classify them**

`server/capabilities.go` requiredCap map, after the BoardSubscribers line:

```go
	protocol.TaskControlKind_BoardSend:        protocol.Capability_BoardSend,
	protocol.TaskControlKind_BoardWake:        protocol.Capability_BoardSend,
```

`server/cap_completeness_test.go` `kindCapClass`: add both kinds as `capInMap`. Change the loop bound in `TestEveryTaskControlKindHasACapVerdict` from `TaskControlKind_AwaitIdleKill` to `TaskControlKind_BoardWake`.

`server/scope_completeness_test.go` `kindTargetClass`: add both as `noTarget`, next to the board kinds (a topic, not a task). Raise the loop bound in `TestEveryTaskControlKindIsClassified` to `TaskControlKind_BoardWake`. Rename `TestAwaitIdleKillIsStillTheLastKind` to `TestBoardWakeIsStillTheLastKind`, point it at `TaskControlKind_BoardWake`, update its message, and add "and board_send / board_wake" to the list of appends in its comment.

`server/scope_percap_completeness_test.go` `capTargetClasses`: add

```go
	protocol.Capability_BoardSend: {
		kind:   capNoTargetResolution,
		reason: "board_send names a board TOPIC, for the same reason as board_observe",
	},
```

- [ ] **Step 10: Catalog the bit**

`cli/verb/caps.go`: append `protocol.Capability_BoardSend` to `GrantableCaps()` after `Capability_ForwardTap` (keep the existing ordering rule the function documents). Add to `CapDescription`:

```go
	case protocol.Capability_BoardSend:
		return "publish to the agentboard in the operator's name (board send, board wake); " +
			"NOT required for agent send"
```

- [ ] **Step 11: Run the suites that pin all of the above**

Run: `go test ./runner/protocol/ ./server/ ./cli/... 2>&1 | tail -30`
Expected: PASS. `TestCapClassAgreesWithRequiredCap`, `TestGrantableCapsCoversEveryBitOfAll`, `TestEveryCapabilityDeclaresHowItsTargetIsResolved` must all be green. Before this step the new kinds dispatch to nothing; that is expected.

- [ ] **Step 12: Commit**

```bash
git add runner/protocol/ server/capabilities.go server/*_completeness_test.go cli/verb/caps.go
git commit -m "protocol: SenderKind, board_send/board_wake kinds, board_send cap, no_reply_route"
```

---

### Task 2: agentboard — sender kind, no-wake, Wake, reserved topic

**Files:**
- Modify: `agentboard/topic.go` (RetainedMessage, append)
- Modify: `agentboard/board.go` (sendConfig, options, Send, Wake, Subscribe, errors)
- Modify: `agentboard/ids.go` (OperatorTopic)
- Test: `agentboard/operator_send_test.go` (new)

**Interfaces:**
- Consumes: `protocol.SenderKind` (Task 1).
- Produces:
  - `RetainedMessage.SenderKind protocol.SenderKind`
  - `func WithSenderKind(k protocol.SenderKind) SendOption`
  - `func WithNoWake() SendOption`
  - `func (b *Board) Wake(topicName string) int`
  - `const OperatorTopic = "chat.operator"`
  - `var ErrReservedTopic` returned by `Subscribe(c, OperatorTopic)`

- [ ] **Step 1: Write the failing tests**

Create `agentboard/operator_send_test.go`. Reuse the board constructor and `SetOnDeliver` pattern from `TestBoard_SendFiresOnDeliverForPublisherToo` (board_test.go:381) and the attach/subscribe helpers used in `wait_scope_test.go:104`. Read those two tests first and use the same helpers by name.

```go
package agentboard

import (
	"errors"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestBoard_SendStampsAgentKindByDefault(t *testing.T) {
	b := newTestBoardForOperator(t)
	seq, _, err := b.Send("chat.k", []byte("x"), protocol.RunnerID{}, protocol.TaskID{}, "h", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	m, ok := b.Retained(seq)
	if !ok || m.SenderKind != protocol.SenderKind_Agent {
		t.Fatalf("SenderKind = %v, want agent", m.SenderKind)
	}
}

func TestBoard_WithSenderKindIsRecorded(t *testing.T) {
	b := newTestBoardForOperator(t)
	seq, _, _ := b.Send("chat.k", []byte("x"), protocol.RunnerID{}, protocol.TaskID{}, "", "", 0,
		WithSenderKind(protocol.SenderKind_Operator))
	m, _ := b.Retained(seq)
	if m.SenderKind != protocol.SenderKind_Operator {
		t.Fatalf("SenderKind = %v, want operator", m.SenderKind)
	}
}

func TestBoard_NoWakeRetainsAndDeliversButDoesNotWake(t *testing.T) {
	b, sub, woken := subscribedBoard(t, "chat.nw") // attaches one task subscribed to chat.nw; woken counts onDeliver calls
	_, delivered, err := b.Send("chat.nw", []byte("quiet"), protocol.RunnerID{}, protocol.TaskID{}, "", "", 0, WithNoWake())
	if err != nil {
		t.Fatal(err)
	}
	if delivered != 1 {
		t.Errorf("deliveredTo = %d, want 1 (no-wake still counts subscribers)", delivered)
	}
	if *woken != 0 {
		t.Errorf("onDeliver called %d times, want 0", *woken)
	}
	msgs, _ := b.Inbox(sub, 0)
	if len(msgs) != 1 || string(msgs[0].Payload) != "quiet" {
		t.Fatalf("inbox = %+v, want the quiet message", msgs)
	}
}

func TestBoard_WakeWakesSubscribersAndPublishesNothing(t *testing.T) {
	b, _, woken := subscribedBoard(t, "chat.wk")
	if n := b.Wake("chat.wk"); n != 1 {
		t.Errorf("Wake = %d, want 1", n)
	}
	if *woken != 1 {
		t.Errorf("onDeliver called %d times, want 1", *woken)
	}
	if _, found := b.Read("chat.wk"); found {
		t.Errorf("Wake created the topic; it must publish nothing")
	}
	if n := b.Wake("chat.nobody"); n != 0 {
		t.Errorf("Wake on an unsubscribed topic = %d, want 0", n)
	}
}

func TestBoard_SubscribeRefusesOperatorTopic(t *testing.T) {
	b, sub, _ := subscribedBoard(t, "chat.other")
	if err := b.Subscribe(sub, OperatorTopic); !errors.Is(err, ErrReservedTopic) {
		t.Fatalf("Subscribe(%q) err = %v, want ErrReservedTopic", OperatorTopic, err)
	}
}
```

Implement the two helpers at the bottom of the file using the existing helpers you found: `newTestBoardForOperator(t) *Board` (a board with test config) and `subscribedBoard(t, topic) (*Board, *ConnState, *int)` (one attached task subscribed to `topic`, and `SetOnDeliver` incrementing the returned counter). If `Board.Read` has a different name, use the method `board read` uses on the server (`server/board_handler.go:handleBoardRead`) and keep the "found" check.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./agentboard/ -run 'SenderKind|NoWake|Wake|OperatorTopic' -v`
Expected: compile failure (`WithSenderKind`, `WithNoWake`, `Wake`, `OperatorTopic`, `ErrReservedTopic` undefined).

- [ ] **Step 3: Implement**

`agentboard/ids.go`, below `SelfTopicPrefix`:

```go
// OperatorTopic is where replies to the operator go. The TUI and WebUI prefill
// it as --reply-to when the operator replies, so an agent answering with
// --in-reply-to alone lands here. It is reserved: no agent may subscribe to it
// (Board.Subscribe), so a publish here wakes nobody and reaches no inbox. It
// cannot collide with a task's chat.<8-hex>, because "operator" is not hex.
const OperatorTopic = SelfTopicPrefix + "operator"
```

`agentboard/board.go`, in the `var (...)` with `ErrPayloadTooLarge`:

```go
	ErrReservedTopic   = errors.New("agentboard: topic is reserved and cannot be subscribed")
```

`Subscribe`, after the empty-pattern check:

```go
	// Wait is deliberately NOT refused the same way: a wait already reads any
	// named topic's ring (knowing the name is the price of entry), and a task
	// waiting on a topic is skipped by the wake loop. Subscription is what
	// would put chat.operator into an inbox and wake a task on every reply.
	if pattern == OperatorTopic {
		return ErrReservedTopic
	}
```

`sendConfig` gains `senderKind protocol.SenderKind` and `noWake bool`. Add after `WithReplyTo`:

```go
// WithSenderKind records who published the message. The zero value is agent,
// which is what every caller passing no options means.
func WithSenderKind(k protocol.SenderKind) SendOption {
	return func(c *sendConfig) { c.senderKind = k }
}

// WithNoWake publishes and retains the message without emitting a wake for
// any subscriber. Subscribers still get their conn ping (a live wait or
// inbox read sees it at once) and still count in deliveredTo; only the
// task_wake, which types a prompt into the agent's PTY, is suppressed.
func WithNoWake() SendOption {
	return func(c *sendConfig) { c.noWake = true }
}
```

In `topic.append`, set `SenderKind: cfg.senderKind` in the `RetainedMessage` literal. In `RetainedMessage` (topic.go), add after `FromAgentProfile`:

```go
	// SenderKind is who published it (agent / operator / server). from_task
	// alone cannot say: an operator and a server publish can both carry a
	// zero task id.
	SenderKind protocol.SenderKind
```

In `Send`, extract the delivery loop (board.go ~296-311) into a method, and call it:

```go
	b.deliver(targets, topicName, !cfg.noWake)
	return seq, len(targets), nil
}

// deliver pings every target's connections and, when wake is set, emits
// onDeliver for each target that is not already waiting on this topic. Shared
// by Send and Wake so the two cannot disagree about who a publish reaches.
func (b *Board) deliver(targets []*taskState, topicName string, wake bool) int {
	b.mu.Lock()
	fn := b.onDeliver
	b.mu.Unlock()
	woken := 0
	for _, ts := range targets {
		for _, c := range ts.snapshotConns() {
			c.ping()
		}
		// (keep the existing comment block about isWaiting here, verbatim)
		if wake && fn != nil && !ts.isWaiting(topicName) {
			rid, tid, _, _ := ts.identity()
			fn(rid, tid)
			woken++
		}
	}
	return woken
}
```

Move the existing comment that sits above the loop ("Subscription is the opt-in: ...") onto `deliver` unchanged. Then add `Wake`:

```go
// Wake emits a wake to every task subscribed to topicName, exactly as a
// publish there would, and publishes nothing: no topic is created and no seq
// is consumed. It returns how many tasks a wake was emitted for. The caller is
// the operator's board_wake, run after one or more no-wake sends.
func (b *Board) Wake(topicName string) int {
	b.mu.Lock()
	targets := make([]*taskState, 0)
	for _, ts := range b.tasks {
		if ts.matches(topicName) {
			targets = append(targets, ts)
		}
	}
	b.mu.Unlock()
	return b.deliver(targets, topicName, true)
}
```

Wake does NOT ping conns differently from Send: `deliver` pings either way. A ping with nothing new is harmless (a wait re-reads and blocks again).

- [ ] **Step 4: Run the package tests**

Run: `go test ./agentboard/`
Expected: PASS, including the existing `wait_scope_test.go` and `board_test.go` onDeliver tests (the refactor must not change them).

- [ ] **Step 5: Commit**

```bash
git add agentboard/
git commit -m "agentboard: sender kind, no-wake publish, Wake, reserved chat.operator"
```

---

### Task 3: Server — stamp, route, and the two handlers

**Files:**
- Modify: `server/agent_taskcontrol.go` (deliveredRows ~199-212, handleAgentListRetained ~343-353)
- Modify: `server/board_handler.go` (BoardMessageRow fill ~126-135; new handlers)
- Modify: `server/await_idle_handler.go` (fireIdleBoard)
- Modify: `server/agent_handler.go` (resolveReplyTarget)
- Modify: `server/task_handler.go` (dispatch cases)
- Modify: `server/agent_handler_reply_test.go`, `server/reply_target_test.go` (signature change)
- Test: `server/board_send_test.go` (new)

**Interfaces:**
- Consumes: Task 1 types; `agentboard.WithSenderKind`, `WithNoWake`, `Board.Wake` (Task 2).
- Produces:
  - `func resolveReplyTarget(b *agentboard.Board, topic string, inReplyTo uint64) (string, protocol.SendStatus)`. It returns `SendStatus_Ok` on success, `SendStatus_UnknownInReplyTo` or `SendStatus_NoReplyRoute` otherwise.
  - `func (h *TaskHandler) boardPublish(by protocol.TaskID, topic string, payload []byte, inReplyTo uint64, replyTo string, noRetire, noWake bool) (seq uint64, deliveredTo int, status protocol.SendStatus)`
  - `func (h *TaskHandler) handleBoardSend(conn ConnHandle, requestID uint32, r *protocol.BoardSendRequest, by protocol.TaskID)`
  - `func (h *TaskHandler) handleBoardWake(conn ConnHandle, requestID uint32, topic string)`

- [ ] **Step 1: Write the failing tests**

Create `server/board_send_test.go`. Use `newBoardTestHandler` and `lastTaskControlResponse` from `board_handler_test.go`:

```go
package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestBoardPublish_StampsOperatorAndPrincipal(t *testing.T) {
	h, _ := newBoardTestHandler(t)
	var by protocol.TaskID
	by.Id[0] = 5
	seq, _, st := h.boardPublish(by, "chat.op1", []byte("hi"), 0, "", false, false)
	if st != protocol.SendStatus_Ok {
		t.Fatalf("status = %v", st)
	}
	m, _ := h.Board.Retained(seq)
	if m.SenderKind != protocol.SenderKind_Operator || m.FromTask != by {
		t.Fatalf("kind=%v from=%x, want operator/%x", m.SenderKind, m.FromTask.Id, by.Id)
	}
}

func TestBoardPublish_RepliesToAgentWithoutTopic(t *testing.T) {
	h, _ := newBoardTestHandler(t)
	var agentTid protocol.TaskID
	agentTid.Id[0] = 0xab
	parent, _, _ := h.Board.Send("shared", []byte("q"), protocol.RunnerID{}, agentTid, "h", "claude", 0)
	seq, _, st := h.boardPublish(protocol.TaskID{}, "", []byte("a"), parent, agentboard.OperatorTopic, false, false)
	if st != protocol.SendStatus_Ok {
		t.Fatalf("status = %v", st)
	}
	m, _ := h.Board.Retained(seq)
	if m.Topic != agentboard.SelfTopic(agentTid) {
		t.Errorf("reply landed on %q, want the author's chat topic", m.Topic)
	}
	if m.ReplyToTopic != agentboard.OperatorTopic {
		t.Errorf("ReplyToTopic = %q, want %q", m.ReplyToTopic, agentboard.OperatorTopic)
	}
}

func TestResolveReplyTarget_OperatorParentWithoutRouteIsRefused(t *testing.T) {
	h, _ := newBoardTestHandler(t)
	parent, _, _ := h.Board.Send("chat.abababab", []byte("from op"), protocol.RunnerID{}, protocol.TaskID{}, "", "", 0,
		agentboard.WithSenderKind(protocol.SenderKind_Operator))
	if _, st := resolveReplyTarget(h.Board, "", parent); st != protocol.SendStatus_NoReplyRoute {
		t.Fatalf("status = %v, want no_reply_route", st)
	}
	// An explicit topic is unchanged.
	if dest, st := resolveReplyTarget(h.Board, "elsewhere", parent); st != protocol.SendStatus_Ok || dest != "elsewhere" {
		t.Fatalf("explicit topic = %q/%v", dest, st)
	}
}

func TestResolveReplyTarget_OperatorParentWithRouteGoesThere(t *testing.T) {
	h, _ := newBoardTestHandler(t)
	parent, _, _ := h.Board.Send("chat.abababab", []byte("from op"), protocol.RunnerID{}, protocol.TaskID{}, "", "", 0,
		agentboard.WithSenderKind(protocol.SenderKind_Operator), agentboard.WithReplyTo(agentboard.OperatorTopic))
	dest, st := resolveReplyTarget(h.Board, "", parent)
	if st != protocol.SendStatus_Ok || dest != agentboard.OperatorTopic {
		t.Fatalf("dest = %q/%v, want chat.operator/ok", dest, st)
	}
}

func TestResolveReplyTarget_ServerParentWithoutRouteIsRefused(t *testing.T) {
	h, _ := newBoardTestHandler(t)
	var requester protocol.TaskID
	requester.Id[0] = 3
	parent, _, _ := h.Board.Send("chat.03000000", []byte(`{"kind":"session_idle"}`), protocol.RunnerID{}, requester, "server", "", 0,
		agentboard.WithSenderKind(protocol.SenderKind_Server))
	if _, st := resolveReplyTarget(h.Board, "", parent); st != protocol.SendStatus_NoReplyRoute {
		t.Fatalf("status = %v, want no_reply_route", st)
	}
}

func TestHandleBoardWake_ReportsWoken(t *testing.T) {
	h, conn := newBoardTestHandler(t)
	h.handleBoardWake(conn, 7, "chat.none")
	resp := lastTaskControlResponse(t, conn)
	w := resp.BoardWake()
	if resp.Kind != protocol.TaskControlKind_BoardWake || w == nil || w.Woken != 0 {
		t.Fatalf("resp = %v %+v, want board_wake woken=0", resp.Kind, w)
	}
}

func TestBoardSendAndWakeNeedBoardSend(t *testing.T) {
	for _, k := range []protocol.TaskControlKind{protocol.TaskControlKind_BoardSend, protocol.TaskControlKind_BoardWake} {
		if got := requiredCap[k]; got != protocol.Capability_BoardSend {
			t.Errorf("%v required cap = %v, want board_send", k, got)
		}
	}
}

func TestFireIdleBoardStampsServer(t *testing.T) {
	h, _ := newBoardTestHandler(t)
	h.fireIdleBoard("chat.idle0000", "00", protocol.TaskID{}, false, 0)
	rows, _ := h.Board.Read("chat.idle0000")
	if len(rows) != 1 || rows[0].SenderKind != protocol.SenderKind_Server {
		t.Fatalf("rows = %+v, want one server-kind message", rows)
	}
}
```

Use the board read method `handleBoardRead` calls if `Board.Read` has another name. Also add one assertion to the existing `TestHandleBoardRead_CarriesReplyToTopic` (board_handler_test.go:183): seed one message with `WithSenderKind(SenderKind_Operator)` and assert `br.Msgs[i].SenderKind == protocol.SenderKind_Operator`.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test ./server/ -run 'BoardPublish|ResolveReplyTarget_|HandleBoardWake|BoardSendAndWake|FireIdleBoardStampsServer' -v`
Expected: compile failure (`boardPublish`, `handleBoardWake` undefined; `resolveReplyTarget` returns bool).

- [ ] **Step 3: Change `resolveReplyTarget`**

`server/agent_handler.go:110`:

```go
func resolveReplyTarget(b *agentboard.Board, topic string, inReplyTo uint64) (string, protocol.SendStatus) {
	if inReplyTo == 0 {
		return topic, protocol.SendStatus_Ok
	}
	parent, ok := b.Retained(inReplyTo)
	if !ok {
		return "", protocol.SendStatus_UnknownInReplyTo
	}
	if topic != "" {
		return topic, protocol.SendStatus_Ok
	}
	if parent.ReplyToTopic != "" {
		return parent.ReplyToTopic, protocol.SendStatus_Ok
	}
	// The author's chat topic is a destination only when the author is an
	// agent. An operator or server message can carry a zero from_task, and
	// SelfTopic of that is chat.00000000 -- a topic nobody owns.
	if parent.SenderKind != protocol.SenderKind_Agent {
		return "", protocol.SendStatus_NoReplyRoute
	}
	return agentboard.SelfTopic(parent.FromTask), protocol.SendStatus_Ok
}
```

Keep the function's doc comment and add a sentence on the new arm. Update the caller in `handleAgentSend` (~135):

```go
		destTopic, st := resolveReplyTarget(h.Board, topic, inReplyTo)
		if st != protocol.SendStatus_Ok {
			reply(st, 0, 0)
			return
		}
```

Update the test callers in `server/agent_handler_reply_test.go` (36, 55, 66, 73) and `server/reply_target_test.go` (51): `ok` becomes `st == protocol.SendStatus_Ok`.

- [ ] **Step 4: Stamp the kind on every row**

- `deliveredRows` (agent_taskcontrol.go ~199-212): set `SenderKind: m.SenderKind` in the `DeliveredMessage` literal.
- `handleAgentListRetained` (~343-353): set `SenderKind: m.SenderKind` on `RetainedMeta`.
- `handleBoardRead` row build (board_handler.go ~126-135): set `SenderKind: m.SenderKind` on `BoardMessageRow`.
- `fireIdleBoard` (await_idle_handler.go:220): append `agentboard.WithSenderKind(protocol.SenderKind_Server)` to the `Send` call. Rewrite the comment above it: the kind now identifies the publish, and the `"server"` hostname stays for older readers. While there, fix the doc comment's claim of a "placeholder RunnerID": the code passes `protocol.RunnerID{}`.

- [ ] **Step 5: Implement `boardPublish`, `handleBoardSend`, `handleBoardWake`**

Append to `server/board_handler.go`:

```go
// boardPublish is board_send after the body has been read: the operator-face
// publish. It is split from handleBoardSend so the routing and stamping are
// testable without a payload stream.
//
// The message is stamped sender_kind=operator whoever the caller is; by is the
// caller's principal task (zero for a real operator connection), recorded as
// from_task so a task granted board_send stays attributable. There is no
// reply-retire: that rule fires only when the parent sits on the replier's own
// chat.<short-id>, and the operator has none.
func (h *TaskHandler) boardPublish(by protocol.TaskID, topic string, payload []byte, inReplyTo uint64, replyTo string, noRetire, noWake bool) (uint64, int, protocol.SendStatus) {
	dest, st := resolveReplyTarget(h.Board, topic, inReplyTo)
	if st != protocol.SendStatus_Ok {
		return 0, 0, st
	}
	opts := []agentboard.SendOption{agentboard.WithSenderKind(protocol.SenderKind_Operator)}
	if noRetire {
		opts = append(opts, agentboard.NoRetireOnReply())
	}
	if replyTo != "" {
		opts = append(opts, agentboard.WithReplyTo(replyTo))
	}
	if noWake {
		opts = append(opts, agentboard.WithNoWake())
	}
	// A zero RunnerID, as fireIdleBoard passes for a publish no runner made.
	// RunnerID is 16 opaque bytes (message.bgn `format RunnerID`), so zero
	// encodes; the "panics the encoder" note in fireIdleBoard's comment
	// predates RunnerID becoming opaque.
	seq, delivered, err := h.Board.Send(dest, payload, protocol.RunnerID{}, by, "", "", inReplyTo, opts...)
	switch err {
	case nil:
		return seq, delivered, protocol.SendStatus_Ok
	case agentboard.ErrPayloadTooLarge:
		return 0, 0, protocol.SendStatus_PayloadTooLarge
	case agentboard.ErrTooManyTopics:
		return 0, 0, protocol.SendStatus_TooManyTopics
	default:
		return 0, 0, protocol.SendStatus_BadFrame
	}
}

// handleBoardSend reads the body off the client-initiated stream the request
// names, then publishes it through boardPublish. The read runs on its own
// goroutine for the reason handleAgentSend gives.
func (h *TaskHandler) handleBoardSend(conn ConnHandle, requestID uint32, r *protocol.BoardSendRequest, by protocol.TaskID) {
	reply := func(status protocol.SendStatus, seq uint64, deliveredTo int) {
		if deliveredTo > 65535 {
			deliveredTo = 65535
		}
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_BoardSend, RequestId: requestID}
		resp.SetBoardSend(protocol.AgentSendResponse{
			RequestId: requestID, Status: status, Seq: seq, DeliveredTo: uint16(deliveredTo),
		})
		respondAgent(conn, resp)
	}
	if h.Board == nil {
		reply(protocol.SendStatus_BadFrame, 0, 0)
		return
	}
	topic := string(r.Topic)
	inReplyTo := r.InReplyTo
	replyTo := string(r.ReplyToTopic)
	noRetire := r.NoRetireOnReply()
	noWake := r.NoWake()
	streamID := r.PayloadStreamId
	go func() {
		payload, err := readAgentPayloadStream(conn, streamID, h.Board.MaxPayload())
		if err != nil {
			slog.Warn("board_send: read payload stream failed", "request_id", requestID, "err", err)
			status := protocol.SendStatus_BadFrame
			if errors.Is(err, errPayloadTooLarge) {
				status = protocol.SendStatus_PayloadTooLarge
			}
			reply(status, 0, 0)
			return
		}
		seq, delivered, st := h.boardPublish(by, topic, payload, inReplyTo, replyTo, noRetire, noWake)
		reply(st, seq, delivered)
	}()
}

// handleBoardWake wakes topic's subscribers and publishes nothing.
func (h *TaskHandler) handleBoardWake(conn ConnHandle, requestID uint32, topic string) {
	woken := 0
	if h.Board != nil {
		woken = h.Board.Wake(topic)
	}
	if woken > 65535 {
		woken = 65535
	}
	resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_BoardWake, RequestId: requestID}
	resp.SetBoardWake(protocol.BoardWakeResponse{RequestId: requestID, Woken: uint16(woken)})
	conn.SendMessage(resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})) //nolint:errcheck
}
```

Add the imports (`errors`, `log/slog`) that `board_handler.go` lacks. In `server/task_handler.go`, after the `BoardSubscribers` case (~755), add (copy the nil-check style of the BoardRetract case at 743-753):

```go
	case protocol.TaskControlKind_BoardSend:
		bs := req.BoardSend()
		if bs == nil {
			slog.Error("TaskHandler: BoardSend variant is nil")
			return
		}
		h.handleBoardSend(conn, req.RequestId, bs, h.lookupPrincipal(cid))

	case protocol.TaskControlKind_BoardWake:
		bw := req.BoardWake()
		if bw == nil {
			slog.Error("TaskHandler: BoardWake variant is nil")
			return
		}
		h.handleBoardWake(conn, req.RequestId, string(bw.Topic))
```

The requiredCap gate at task_handler.go:349 already runs before this switch (Task 1 Step 9).

- [ ] **Step 6: Run the server suite**

Run: `go test ./server/`
Expected: PASS. Run it twice; memory records a ~1/6 package-level flake family in `server` (`TestOpenInteractive*` / sessionmux). A failure there that does not reproduce is that, not this change. Report it either way.

- [ ] **Step 7: Commit**

```bash
git add server/
git commit -m "server: board_send/board_wake handlers, sender kind on every row, no_reply_route"
```

---

### Task 4: Client — shared payload-stream helper, client methods, CLI verbs

**Files:**
- Create: `cli/payload.go` (moved from `cli/agent/payload.go` logic)
- Modify: `cli/agent/payload.go` (thin wrappers)
- Create: `cli/payload_stream.go`
- Modify: `cli/agent/send.go` (use the helper; `no_reply_route` arm in `sendResult`)
- Modify: `cli/board.go` (client methods + fresh-dial wrappers)
- Modify: `cli/verb/table.go` (two rows)
- Regenerate: `cli/verb/actions_gen.go` (`go generate ./cli/verb`)
- Modify: `cmd/harness-cli/dispatch.go`
- Modify: `cli/cmd_board.go` (`wake` case; `RunBoardSend`)
- Test: `cli/board_send_e2e_test.go` (new)

**Interfaces:**
- Consumes: Task 1 wire types; the Task 3 server.
- Produces:
  - `func ResolvePayload(dataSet bool, data, positional string, stdin io.Reader) ([]byte, string, error)` and consts `PayloadSourceData`, `PayloadSourcePositional`, `PayloadSourceStdin` in package `cli`.
  - `func (c *Client) TaskControlWithPayload(ctx context.Context, build func(streamID uint64) *protocol.TaskControlRequest, payload []byte) (TaskControlResult, error)`
  - `type BoardSendParams struct { Topic string; InReplyTo uint64; ReplyTo string; NoRetireOnReply, NoWake bool }`
  - `type BoardSendResult struct { Seq uint64; DeliveredTo uint16 }`
  - `func (c *Client) BoardSend(ctx context.Context, p BoardSendParams, payload []byte) (BoardSendResult, error)`
  - `func (c *Client) BoardWake(ctx context.Context, topic string) (int, error)`
  - `func BoardSend(ctx, peerCID, p, payload)` and `func BoardWake(ctx, peerCID, topic)` (fresh-dial wrappers)
  - `type NoReplyRouteError struct{ InReplyTo uint64 }` (an `error`)
  - verb `board send` → `verb.BoardSendAction` (fields `Topic, DataSet, Data, ReplyTo, InReplyTo, NoRetireOnReply, NoWake, Positional`)
  - verb `board wake` → `verb.BoardAction` with `Sub == verb.SubWake`
  - `func RunBoardSend(ctx context.Context, cid objproto.ConnectionID, a verb.BoardSendAction, stdin io.Reader, out io.Writer) error`

- [ ] **Step 1: Move payload resolution into `cli`**

Create `cli/payload.go` with the body of `resolvePayloadFrom` and the three source consts, exported as `ResolvePayload` / `PayloadSourceData` / `PayloadSourcePositional` / `PayloadSourceStdin`. Keep the comments; they explain why each case exists. In `cli/agent/payload.go`, keep `resolvePayload` (it reads a FlagSet) and make `resolvePayloadFrom` one line: `return cli.ResolvePayload(dataSet, data, positional, stdin)`. Keep the agent consts as aliases (`sourceData = cli.PayloadSourceData`, …) so `payload_test.go` still compiles.

Run: `go test ./cli/agent/ -run Payload -v`
Expected: PASS (behaviour unchanged).

- [ ] **Step 2: Extract the stream round trip**

Create `cli/payload_stream.go`. Move the body of `agent.SendWith` from `stream := c.Transport().CreateSendStream()` through the final `select` (send.go ~76-153) into this method, and keep its comments (they record the deadlock ordering):

```go
// payloadErrGrace bounds how long a failed payload write waits for the
// server's explanation before reporting the local error instead.
const payloadErrGrace = 2 * time.Second

// TaskControlWithPayload sends a task-control request whose body travels on a
// client-initiated stream, and returns the server's response. build receives
// the stream id and returns the request naming it.
//
// (Carry over the ordering comment from agent.SendWith verbatim: the request
// goes out BEFORE the body, and why.)
func (c *Client) TaskControlWithPayload(ctx context.Context, build func(streamID uint64) *protocol.TaskControlRequest, payload []byte) (TaskControlResult, error) {
	stream := c.Transport().CreateSendStream()
	if stream == nil {
		return TaskControlResult{}, errors.New("failed to allocate payload stream")
	}
	respCh, err := c.BeginTaskControl(build(uint64(stream.ID())))
	if err != nil {
		return TaskControlResult{}, err
	}
	writeErr := stream.AppendDataContext(ctx, false, payload)
	if writeErr == nil {
		writeErr = stream.AppendDataContext(ctx, true)
	}
	if writeErr != nil {
		select {
		case r := <-respCh:
			return r, nil
		case <-time.After(payloadErrGrace):
			return TaskControlResult{}, fmt.Errorf("payload stream write: %w", writeErr)
		case <-ctx.Done():
			return TaskControlResult{}, fmt.Errorf("payload stream write: %w", writeErr)
		}
	}
	select {
	case r := <-respCh:
		return r, nil
	case <-ctx.Done():
		return TaskControlResult{}, ctx.Err()
	}
}
```

Rewrite `agent.SendWith` to build its `AgentSendRequest` inside a `build` closure and call `c.TaskControlWithPayload`. Then pass the result to `sendResult` exactly as before; on a returned error, prefix `agent: ` as the old messages did. Delete `payloadErrGrace` from `cli/agent/send.go` if nothing else there uses it (grep `cli/agent` first; `dispatch.go` may).

Run: `go test ./cli/agent/ -run 'E2E_Send|Sender|ReplyTo|PayloadLimit|SendSize' -v`
Expected: PASS. These e2e tests are the guard that the extraction preserved behaviour, including the oversized-body path (`agent_payload_limit_e2e_test.go`).

- [ ] **Step 3: `no_reply_route` in `sendResult`**

Add to `cli/board.go` (package `cli`, so both the agent and the operator verbs can return it):

```go
// NoReplyRouteError is SendStatus.no_reply_route: the message being answered
// came from the operator or the server and declared no reply destination.
type NoReplyRouteError struct{ InReplyTo uint64 }

func (e *NoReplyRouteError) Error() string {
	return fmt.Sprintf("send rejected: message %d came from the operator (or the server) "+
		"and declared no reply destination; answer in your own conversation, "+
		"or name a topic with --topic", e.InReplyTo)
}
```

In `agent.sendResult`, after the `UnknownInReplyTo` arm:

```go
	if resp.Status == protocol.SendStatus_NoReplyRoute {
		return &cli.NoReplyRouteError{InReplyTo: inReplyTo}
	}
```

Do the same in `cli/agent/dispatch.go:170` before its generic `!= Ok` arm.

- [ ] **Step 4: Client methods**

Append to `cli/board.go`, after `BoardRetract`:

```go
// BoardSendParams is board send's request, minus the body.
type BoardSendParams struct {
	Topic           string // may be empty only with InReplyTo
	InReplyTo       uint64
	ReplyTo         string
	NoRetireOnReply bool
	NoWake          bool
}

// BoardSendResult is what an accepted board send reports.
type BoardSendResult struct {
	Seq         uint64
	DeliveredTo uint16
}

// BoardSend publishes payload in the operator's name. The server stamps
// sender_kind=operator and needs Capability_BoardSend.
func (c *Client) BoardSend(ctx context.Context, p BoardSendParams, payload []byte) (BoardSendResult, error) {
	if p.Topic == "" && p.InReplyTo == 0 {
		return BoardSendResult{}, errors.New("board send: --topic required (or --in-reply-to)")
	}
	r, err := c.TaskControlWithPayload(ctx, func(streamID uint64) *protocol.TaskControlRequest {
		br := protocol.BoardSendRequest{PayloadStreamId: streamID, InReplyTo: p.InReplyTo}
		br.SetTopic([]byte(p.Topic))
		if p.ReplyTo != "" {
			br.SetReplyToTopic([]byte(p.ReplyTo))
		}
		br.SetNoRetireOnReply(p.NoRetireOnReply)
		br.SetNoWake(p.NoWake)
		req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_BoardSend}
		req.SetBoardSend(br)
		return req
	}, payload)
	if err != nil {
		return BoardSendResult{}, err
	}
	if r.Err != nil {
		return BoardSendResult{}, r.Err
	}
	resp := r.Resp.BoardSend()
	if resp == nil || r.Resp.Kind != protocol.TaskControlKind_BoardSend {
		return BoardSendResult{}, fmt.Errorf("BoardSend: unexpected response kind=%v", r.Resp.Kind)
	}
	switch resp.Status {
	case protocol.SendStatus_Ok:
		return BoardSendResult{Seq: resp.Seq, DeliveredTo: resp.DeliveredTo}, nil
	case protocol.SendStatus_NoReplyRoute:
		return BoardSendResult{}, &NoReplyRouteError{InReplyTo: p.InReplyTo}
	case protocol.SendStatus_UnknownInReplyTo:
		return BoardSendResult{}, fmt.Errorf("board send: --in-reply-to %d is not on the board", p.InReplyTo)
	default:
		return BoardSendResult{}, fmt.Errorf("board send rejected: %v (%d bytes)", resp.Status, len(payload))
	}
}

// BoardWake wakes every task subscribed to topic and publishes nothing.
// Returns how many tasks a wake was emitted for.
func (c *Client) BoardWake(ctx context.Context, topic string) (int, error) {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_BoardWake}
	var bw protocol.BoardWakeRequest
	bw.SetTopic([]byte(topic))
	req.SetBoardWake(bw)
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return 0, err
	}
	w := resp.BoardWake()
	if w == nil || resp.Kind != protocol.TaskControlKind_BoardWake {
		return 0, fmt.Errorf("BoardWake: unexpected response kind=%v", resp.Kind)
	}
	return int(w.Woken), nil
}
```

Add the two fresh-dial wrappers `BoardSend(ctx, peerCID, p, payload)` and `BoardWake(ctx, peerCID, topic)` beside `BoardRetract`'s wrapper, in the same shape (Dial `ClientKind_Cli`, defer Close). If a `Set*` name above differs from what Task 1 generated, use the generated name (`grep -n 'func (t \*BoardSendRequest)' runner/protocol/message.go`).

- [ ] **Step 5: Declare the verbs**

In `cli/verb/table.go`, after the `board purge-thread` row (~1240), add:

```go
	{
		Path: []string{"board", "send"},
		ModalSurfaces: []ModalSurface{
			// m composes on the open topic, a replies to the selected message.
			{Surface: TUI, At: "tui/board.go:BoardModal"},
			{Surface: WebUI, At: "webui/index.html#board-send-btn"},
		},
		Notes: []string{
			"publish in the OPERATOR's name: the message is stamped sender=operator (cap: board_send).",
			"--reply-to chat.operator lets the recipient answer with --in-reply-to alone; without a reply destination its reply is refused (no_reply_route).",
			"--no-wake retains the message without waking anyone; `board wake <topic>` wakes them later.",
			"The body is the trailing words, or --data STRING, or --data - to read stdin.",
		},
		CmdlineSurfaces: CLI,
		Action:          "BoardSendAction",
		Trailing: &Trailing{Name: "text", Field: "Positional",
			Reason: "the message body is free-form; --data or stdin are the alternatives"},
		Flags: []Flag{
			{Name: "topic", Type: FlagString, Default: "", Field: "Topic", Help: "agentboard topic (may be omitted with --in-reply-to)"},
			{Name: "data", Type: FlagString, Default: "-", Field: "Data", PresenceField: "DataSet",
				Help: `payload string, or "-" to read stdin`},
			{Name: "in-reply-to", Type: FlagUint64, Default: uint64(0), Field: "InReplyTo",
				Help: "seq of the message being replied to; with it, --topic may be omitted"},
			{Name: "reply-to", Type: FlagString, Default: "", Field: "ReplyTo",
				Help: "where replies to THIS message go (chat.operator to receive them on the operator surfaces)"},
			{Name: "no-retire-on-reply", Type: FlagBool, Default: false, Field: "NoRetireOnReply",
				Help: "keep this message on the board even after its recipient replies"},
			{Name: "no-wake", Type: FlagBool, Default: false, Field: "NoWake",
				Help: "retain without waking subscribers; see board wake"},
		},
		Examples: []string{"board send --topic chat.abcd1234 --reply-to chat.operator please look at the failing test"},
	},
	{
		Path: []string{"board", "wake"},
		ModalSurfaces: []ModalSurface{
			{Surface: TUI, At: "tui/board.go:BoardModal"},
			{Surface: WebUI, At: "webui/index.html#board-wake-btn"},
		},
		Notes: []string{
			"wake every task subscribed to <topic>, exactly as a publish there would; publishes nothing (cap: board_send).",
			"For after --no-wake sends. It does not check whether anything is unread.",
		},
		CmdlineSurfaces: CLI,
		Action:          "BoardAction",
		Const:           map[string]string{"Sub": "wake"},
		Args:            []Arg{{Name: "topic", Type: ArgTopic, Field: "Topic"}},
		Examples:        []string{"board wake chat.abcd1234"},
	},
```

`ModalSurfaces.At` for WebUI must name a real element id, which `TestModalSurfaceEntryPointsExist` checks. So add the two buttons to `webui/index.html` in this step; their handlers come in Task 8. Put them inside the board detail header (index.html ~323-330), beside `board-purge-topic-btn`:

```html
<button id="board-send-btn" type="button" title="Send a message to this topic as the operator">✉ Send</button>
<button id="board-wake-btn" type="button" title="Wake this topic's subscribers without publishing">⏰ Wake</button>
```

Run: `go generate ./cli/verb && go test ./cli/verb/`
Expected: PASS: `TestGeneratedFileIsCurrent`, `TestEveryVerbHasASurfaceVerdict`, `TestModalSurfaceEntryPointsExist`, `notecompleteness`, `help`, `wiring`, `TestExamplesParse`, `TestTrailingVerbsKeepTextLiteral`. If `wiring_test` complains that a flag is not read by its Build, compare the `agent send` row; `BoardSendAction` gets the same generated Build.

- [ ] **Step 6: Dispatch and run**

`cmd/harness-cli/dispatch.go`: next to the board methods (~525-536):

```go
func (h cliVerbs) BoardWake(a verb.BoardAction) error { return h.board(a) }
func (h cliVerbs) BoardSend(a verb.BoardSendAction) error {
	return cli.RunBoardSend(h.ctx, h.cid(), a, os.Stdin, os.Stdout)
}
```

(Use the method names the generated `CLIDispatch` interface demands; the compiler names them.)

`cli/cmd_board.go`, in `RunBoardAction`'s switch:

```go
	case verb.SubWake:
		n, err := BoardWake(ctx, cid, ba.Topic)
		if err != nil {
			return err
		}
		out2, _ := json.Marshal(map[string]any{"topic": ba.Topic, "status": "ok", "woken": n})
		fmt.Fprintln(out, string(out2))
		return nil
```

and a new function:

```go
// RunBoardSend is `board send`: publish in the operator's name. The ok line
// mirrors agent send's (seq, delivered_to, bytes, source) because the same
// two mistakes -- a body that went out wrong, a topic nobody holds -- are
// what it exists to show.
func RunBoardSend(ctx context.Context, cid objproto.ConnectionID, a verb.BoardSendAction, stdin io.Reader, out io.Writer) error {
	payload, source, err := ResolvePayload(a.DataSet, a.Data, a.Positional, stdin)
	if err != nil {
		return err
	}
	res, err := BoardSend(ctx, cid, BoardSendParams{
		Topic: a.Topic, InReplyTo: a.InReplyTo, ReplyTo: a.ReplyTo,
		NoRetireOnReply: a.NoRetireOnReply, NoWake: a.NoWake,
	}, payload)
	if err != nil {
		return err
	}
	line, _ := json.Marshal(map[string]any{
		"seq": res.Seq, "status": "ok", "delivered_to": res.DeliveredTo,
		"woke": !a.NoWake, "bytes": len(payload), "source": source,
	})
	fmt.Fprintln(out, string(line))
	return nil
}
```

`woke` is the request (`!NoWake`), not a count: the wake count for a send is `delivered_to` minus waiting tasks, which the response does not carry. Say so in a comment on that key.

- [ ] **Step 7: Write the e2e test**

Create `cli/board_send_e2e_test.go`, using `startOperatorServerE2E` (cli/board_e2e_test.go:41) for the operator. For an agent subscriber, use the board directly (`(*operatorE2E).Board()` + `Registry().Register` + attach), or the agent env helpers if they are reachable from package `cli` tests (they live in `cli/agent`; if not reachable, subscribe through `Board()` directly):

```go
func TestBoardSend_E2E_OperatorStampAndReplyRoute(t *testing.T) {
	e, cid := startOperatorServerE2E(t)
	ctx := context.Background()
	b := e.Board()

	// An agent message the operator answers.
	var agentTid protocol.TaskID
	agentTid.Id[0] = 0xcd
	parent, _, _ := b.Send("shared", []byte("q"), protocol.RunnerID{}, agentTid, "h", "claude", 0)

	res, err := BoardSend(ctx, cid, BoardSendParams{InReplyTo: parent, ReplyTo: agentboard.OperatorTopic}, []byte("answer"))
	if err != nil {
		t.Fatal(err)
	}
	m, _ := b.Retained(res.Seq)
	if m.SenderKind != protocol.SenderKind_Operator || m.Topic != agentboard.SelfTopic(agentTid) {
		t.Fatalf("kind=%v topic=%q", m.SenderKind, m.Topic)
	}

	// No route: an operator message with no --reply-to cannot be answered by seq alone.
	bare, err := BoardSend(ctx, cid, BoardSendParams{Topic: "chat.cd000000"}, []byte("fyi"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = BoardSend(ctx, cid, BoardSendParams{InReplyTo: bare.Seq}, []byte("re"))
	var nr *NoReplyRouteError
	if !errors.As(err, &nr) {
		t.Fatalf("err = %v, want NoReplyRouteError", err)
	}

	// board wake on a topic nobody subscribes.
	if n, err := BoardWake(ctx, cid, "chat.nobody0"); err != nil || n != 0 {
		t.Fatalf("wake = %d, %v; want 0, nil", n, err)
	}
}
```

Also add a test that `board send --help` text (`verb.Lookup("board","send")` usage) contains `--no-wake`, and that `verb.Lookup("agent","send")` usage does NOT (Review Focus / spec testing bullet).

Run: `go test ./cli/ -run 'BoardSend' -v && go test ./cli/... ./cmd/...`
Expected: PASS.

- [ ] **Step 8: Commit**

```bash
git add cli/ cmd/harness-cli/ webui/index.html
git commit -m "cli: board send / board wake verbs; shared payload-stream helper; no_reply_route error"
```

---

### Task 5: Agent-side e2e for routing and wake

**Files:**
- Test: `cli/agent/operator_reply_e2e_test.go` (new)

**Interfaces:**
- Consumes: Tasks 2-4.

- [ ] **Step 1: Write the tests**

Model on `TestAgentCLI_E2E_ReplyToRoutesAwayFromTheAskersInbox` (cli/agent/reply_to_e2e_test.go:56) and its helpers `lastSeqOnTopic` / `topicPayloads`; register the agent with `setAgentEnv` (agent_e2e_test.go:101). Cover:

1. The operator publishes (`board.Send` with `WithSenderKind(Operator)`, `WithReplyTo(agentboard.OperatorTopic)`) to the agent's chat topic. The agent runs `Send` with `--in-reply-to <seq>` and no `--topic`. The reply lands on `chat.operator`; `topicPayloads(chat.operator)` contains it.
2. Same without `WithReplyTo`: the agent's `Send` returns an error that `errors.As` matches `*cli.NoReplyRouteError`, and nothing is published (`lastSeqOnTopic` unchanged on every topic involved).
3. The agent subscribing to `chat.operator` (`agent subscribe --topic chat.operator`) fails with the `bad_pattern` error text.

(`sender_kind` in `agent inbox --json` is Task 6's.)

- [ ] **Step 2: Run**

Run: `go test ./cli/agent/ -run 'Operator' -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add cli/agent/operator_reply_e2e_test.go
git commit -m "test(agent): replies to operator messages route to chat.operator or are refused"
```

---

### Task 6: CLI rendering of the sender kind and the conversation party

**Files:**
- Modify: `cli/board.go` (`BoardMessage` + `BoardRead` fill ~249-253)
- Modify: `cli/agent/thread.go` (~180-186, `hexTask` use)
- Modify: `cli/agent/json_emit.go` (`emitMessageRecord` ~65-70)
- Modify: `cli/agent/retained.go` (`retainedLine` ~119-128, fill ~95-104)
- Modify: `cli/cmd_board.go` (board read text ~281-283, `emitBoardMessageJSON` ~72-76, `RenderThreads` row ~553-557, `emitThreadRowJSON` ~603-607, `boardTaskShort` ~568)
- Modify: `cli/boardthread.go` (`conversationKey` ~450-455, `ConversationHeader` ~600-607)
- Modify: `examples/board-render/board_render.py` (`_header` ~55-68) + its test
- Test: extend `cli/boardthread_test.go` (or the file holding `conversationKey` tests), `cli/agent/json_emit_test.go`

**Interfaces:**
- Consumes: `SenderKind` on the three wire rows (Task 1/3).
- Produces:
  - `BoardMessage.SenderKind string` (`"agent"|"operator"|"server"`, from `protocol.SenderKind.String()`).
  - `func SenderParty(kind, fromTaskHex string) string` in package `cli`. It returns `"operator"` for kind `operator`, `"server"` for `server`, else the 8-hex prefix. Every surface uses this for the short sender label.

- [ ] **Step 1: Write the failing tests**

In the `conversationKey` test file:

```go
func TestConversationKey_OperatorIsAPartyByName(t *testing.T) {
	agent := "cd000000" + strings.Repeat("0", 24)
	chain := []ThreadRow{
		{Topic: "chat.cd000000", Msg: BoardMessage{Seq: 1, FromTaskHex: strings.Repeat("0", 32), SenderKind: "operator"}},
		{Topic: "chat.operator", Msg: BoardMessage{Seq: 2, InReplyTo: 1, FromTaskHex: agent, SenderKind: "agent"}},
	}
	if got := conversationKey(chain, nil); got != "cd000000+operator" {
		t.Fatalf("key = %q, want cd000000+operator", got)
	}
}

func TestSenderParty(t *testing.T) {
	zero := strings.Repeat("0", 32)
	for _, c := range []struct{ kind, hex, want string }{
		{"operator", zero, "operator"},
		{"server", zero, "server"},
		{"agent", "abcdef12" + strings.Repeat("0", 24), "abcdef12"},
	} {
		if got := SenderParty(c.kind, c.hex); got != c.want {
			t.Errorf("SenderParty(%q) = %q, want %q", c.kind, got, c.want)
		}
	}
}
```

Adjust the `ThreadRow` / `BoardMessage` literal field names to what the file defines. In `cli/agent/json_emit_test.go`, add a case asserting `emitMessageRecord` on a `DeliveredMessage` with `SenderKind: protocol.SenderKind_Operator` writes `"sender_kind":"operator"` at the top level of the record (beside `"from"`, not inside it, so a reader that ignores unknown keys is unaffected).

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./cli/ ./cli/agent/ -run 'ConversationKey_Operator|SenderParty|SenderKind' -v`
Expected: FAIL / compile error.

- [ ] **Step 3: Implement**

- `SenderParty` in `cli/boardthread.go`, beside `conversationKey`:

```go
// SenderParty is the short name a sender goes by on every surface: the 8-hex
// task prefix for an agent, and the kind itself for the operator and the
// server, whose from_task may be zero -- "00000000" would name a party that
// does not exist. It is also the conversation party, so an exchange with the
// operator keys as <8-hex>+operator, matching the chat.operator topic suffix.
func SenderParty(kind, fromTaskHex string) string {
	switch kind {
	case "operator", "server":
		return kind
	}
	if len(fromTaskHex) > 8 {
		return fromTaskHex[:8]
	}
	return fromTaskHex
}
```

- `conversationKey` (~450-455) and `ConversationHeader` (~600-607): replace the `h := r.Msg.FromTaskHex; if len(h) > 8 {…}` blocks with `SenderParty(r.Msg.SenderKind, r.Msg.FromTaskHex)`. Keep the `agentOf` map keyed on the party.
- `BoardMessage` gains `SenderKind string`. Fill it in `BoardRead` from `row.SenderKind.String()` and in `cli/agent/thread.go` from `m.SenderKind.String()`. Check that `String()` yields the bare lowercase names (`agent`, `operator`, `server`); if the generator yields something else, add a small `senderKindName(protocol.SenderKind) string` in `cli/board.go` and use it everywhere instead.
- `emitMessageRecord`: add `"sender_kind": <name>` at the top level.
- `retainedLine`: add `SenderKind string \`json:"sender_kind"\`` and fill it.
- `board read` text line (~281-283): print `from=<SenderParty>` instead of the raw short task. Add `sender=<kind>` immediately after it (do not elide `agent`; checklist item 31). JSON (`emitBoardMessageJSON`): add `"sender_kind"` beside `"from"`.
- `board thread` text row (~553-557): use `SenderParty` where `boardTaskShort` is used for the sender. JSON (`emitThreadRowJSON`): add `"sender_kind"`.
- `examples/board-render/board_render.py` `_header`: when `rec.get("sender_kind") in ("operator","server")`, render `from=<kind>` instead of `from=?@server`. Update its fixture test (test_board_render.py:61-64) to expect `from=server` for a record carrying `"sender_kind":"server"`, and keep a case without the key (older server) rendering as before.

- [ ] **Step 4: Run**

Run: `go test ./cli/... && python3 -m pytest examples/board-render -q`
Expected: PASS. If `pytest` is not installed, run `python3 examples/board-render/test_board_render.py` the way its header documents; do NOT install packages.

- [ ] **Step 5: Commit**

```bash
git add cli/ examples/board-render/
git commit -m "cli: render sender kind; operator/server are named parties, never 00000000"
```

---

### Task 7: TUI — compose, reply, wake, sender column

**Files:**
- Modify: `tui/board.go` (Do* commands, msgs, compose sub-mode, footers, sender rendering ~641-675 and ~767-779)
- Modify: `tui/keys.go` (`modalKeyMap` + `modalKeys`)
- Modify: `tui/overlays.go` (`inBoardModal`)
- Modify: `tui/app.go` (result handling next to `case BoardRetractMsg:` ~826-839)
- Test: `tui/board_test.go`

**Interfaces:**
- Consumes: `(*cli.Client).BoardSend`, `(*cli.Client).BoardWake`, `cli.BoardSendParams`, `cli.SenderParty`, `agentboard.OperatorTopic`.
- Produces:
  - `func DoBoardSend(c *cli.Client, p cli.BoardSendParams, body string) tea.Cmd` → `BoardSendMsg{Topic string; Seq uint64; DeliveredTo uint16; NoWake bool; Err error}`
  - `func DoBoardWake(c *cli.Client, topic string) tea.Cmd` → `BoardWakeMsg{Topic string; Woken int; Err error}`
  - keys `modalKeys.BoardCompose = "m"`, `modalKeys.BoardReply = "a"`, `modalKeys.BoardWake = "p"`

- [ ] **Step 1: Write the failing tests**

In `tui/board_test.go`, following the existing footer test at :337 and the modal-driving tests there:

```go
func TestBoardModal_MessagesFooterNamesComposeReplyWake(t *testing.T) {
	m := newBoardModalWithMessages(t) // reuse the helper the :337 test uses to reach boardMessages mode
	foot := m.View()
	for _, want := range []string{
		modalKeys.BoardCompose + ": send", modalKeys.BoardReply + ": reply", modalKeys.BoardWake + ": wake",
	} {
		if !strings.Contains(foot, want) {
			t.Errorf("footer missing %q", want)
		}
	}
}

func TestBoardModal_ReplyPrefillsOperatorTopic(t *testing.T) {
	m := newBoardModalWithMessages(t)
	m.BeginReply(42)
	p := m.ComposeParams()
	if p.InReplyTo != 42 || p.ReplyTo != agentboard.OperatorTopic || p.Topic != "" {
		t.Fatalf("params = %+v, want in_reply_to=42 reply_to=chat.operator topic empty", p)
	}
}

func TestBoardModal_ComposeTogglesWake(t *testing.T) {
	m := newBoardModalWithMessages(t)
	m.BeginCompose()
	if m.ComposeParams().NoWake {
		t.Fatal("compose starts with NoWake set; the default must wake")
	}
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyTab})
	if !m.ComposeParams().NoWake {
		t.Fatal("tab did not toggle no-wake")
	}
}

func TestBoardModal_OperatorSenderIsNamed(t *testing.T) {
	m := NewBoardModal()
	m.Open()
	m.ApplyMessages("chat.x", []cli.BoardMessage{{Seq: 1, FromTaskHex: strings.Repeat("0", 32), SenderKind: "operator"}}, nil, true)
	if v := m.View(); !strings.Contains(v, "from=operator") || strings.Contains(v, "00000000") {
		t.Fatalf("view does not name the operator:\n%s", v)
	}
}
```

Also extend `tui/git_keys_test.go`'s disjointness check if it enumerates `modalKeys` fields by hand (it should catch `m`/`a`/`p` automatically if it reflects; check).

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./tui/ -run 'BoardModal_' -v`
Expected: compile errors.

- [ ] **Step 3: Implement**

- `tui/keys.go`: add `BoardCompose`, `BoardReply`, `BoardWake` to `modalKeyMap`, with values `"m"`, `"a"`, `"p"` and a comment in the style of the neighbours: `m` = message, `a` = answer, `p` = poke (its own letter, because a wake is not a stronger form of sending).
- `tui/board.go`: add a compose sub-state to `BoardModal`. Copy the shape of `tui/chat.go:552 enterDenyReason` / `cancelSubMode`: a `textinput.Model` focused with a Prompt, plus a status hint `enter sends · tab: wake on/off · esc cancels`. State fields: `composing bool`, `compose textinput.Model`, `composeParams cli.BoardSendParams`. Methods:
  - `BeginCompose()`: `composeParams = {Topic: CurTopic()}`.
  - `BeginReply(seq uint64)`: `composeParams = {InReplyTo: seq, ReplyTo: agentboard.OperatorTopic}`.
  - `ComposeParams() cli.BoardSendParams`
  - `Composing() bool`
  - `ComposeBody() string`
  - `EndCompose()`

  In `Update`, while composing: `tab` toggles `composeParams.NoWake` and redraws the hint (`wake: on` / `wake: off`); all other keys go to the textinput. The prompt shows the target (`→ chat.x` or `↩ #42 (replies to chat.operator)`).
- `DoBoardSend` / `DoBoardWake`: copy `DoBoardRetract` (board.go:168) exactly: same 15 s timeout, same `c *cli.Client` threading (Pitfall 3: never Dial here).
- `tui/overlays.go` `inBoardModal`:
  - At the top, before the Esc handling: if `a.boardModal.Composing()`, then Esc → `EndCompose()`. Enter → `p := ComposeParams(); body := ComposeBody(); EndCompose(); return DoBoardSend(a.client, p, body)` (ignore Enter on an empty body). Otherwise forward to `a.boardModal.Update`.
  - In boardMessages mode: `BoardCompose` → `BeginCompose()`; `BoardReply` → `BeginReply(SelectedMsgSeq())` when non-zero; `BoardWake` → `DoBoardWake(a.client, CurTopic())`.
  - In boardTopics mode: `BoardWake` → `DoBoardWake(a.client, SelectedTopicName())`.
- `tui/app.go`: `case BoardSendMsg:` and `case BoardWakeMsg:`, next to `BoardRetractMsg`, using the same `SetStatus` / `SetStatusAfterRefresh` + `DoBoardRead` refresh pattern. Status text names the target and the change (checklist 29): `sent #<seq> to <topic> (delivered_to=N, wake on|off)` and `woke N task(s) on <topic>`. On a `*cli.NoReplyRouteError`, show its message.
- Footers: messages-mode footer (board.go ~794) gains `m: send  a: reply  p: wake`; topics footer (~754) gains `p: wake`.
- Sender rendering: in `updateContentFromCursor` (~641-649) and `View` (~767-779), replace the from-short computation with `cli.SenderParty(m.SenderKind, m.FromTaskHex)`.

- [ ] **Step 4: Run**

Run: `go test ./tui/`
Expected: PASS (including the existing footer test at :337 and `TestModalKeysDoNotShadowScrolling`).

- [ ] **Step 5: Commit**

```bash
git add tui/
git commit -m "tui: board modal sends, replies (to chat.operator) and wakes; names operator senders"
```

---

### Task 8: WebUI — send dialog, reply button, wake button, sender display

**Files:**
- Modify: `cmd/harness-webui-wasm/main.go` (bridge registration ~120-174; new funcs beside `harnessBoardRetract` :1874; sender keys in `harnessBoardRead` ~1635-1637 and `harnessBoardThread` ~1749-1753)
- Modify: `webui/index.html` (a `<dialog>` for compose; buttons added in Task 4)
- Modify: `webui/static/main.js` (`openBoardTopic` :5478 card loop; `renderBoardChains` ~5886; button wiring near :5715)
- Test: `webui/static/cmd_test.mjs` or a new `board_send_test.mjs` run by `make js-test`

**Interfaces:**
- Consumes: `(*cli.Client).BoardSend`, `BoardWake`, `cli.SenderParty`.
- Produces:
  - `window.harness.boardSend({topic, inReplyTo, replyTo, noWake, body})` → Promise of `{seq: string, deliveredTo: number}`. `seq` is a decimal string; board seqs exceed 2^53 (see `harnessBoardRetract`).
  - `window.harness.boardWake(topic)` → Promise of `{woken: number}`
  - `senderKind` and `senderParty` keys on every `boardRead` row and every `boardThread` `from` object.

- [ ] **Step 1: Bridge functions**

Copy `harnessBoardRetract` (:1874) for shape: Promise executor, goroutine, `currentClient()` (never Dial; Pitfall 3), `rejectErr`. `inReplyTo` arrives as a decimal string and goes through `strconv.ParseUint`. Return `seq` as `strconv.FormatUint(res.Seq, 10)`. Register `"boardSend"` and `"boardWake"` next to `"boardRetract"` (:153). In `harnessBoardRead` add `"senderKind": m.SenderKind, "senderParty": cli.SenderParty(m.SenderKind, m.FromTaskHex)`; in `harnessBoardThread`'s `from` object add the same two keys.

- [ ] **Step 2: Compose dialog**

In `index.html`, beside the other `<dialog class="picker-modal …">` elements (~383-560), add:

```html
<dialog id="board-send-dialog" class="picker-modal">
  <form method="dialog">
    <h3 id="board-send-title">Send as operator</h3>
    <textarea id="board-send-body" rows="6" required></textarea>
    <label><input type="checkbox" id="board-send-wake" checked> wake recipients</label>
    <label>replies go to <input type="text" id="board-send-reply-to"></label>
    <menu>
      <button value="cancel" type="button" id="board-send-cancel">Cancel</button>
      <button value="send" id="board-send-submit">Send</button>
    </menu>
  </form>
</dialog>
```

`replies go to` is a text input because it is a topic name, the same kind of value as the topic field elsewhere; it is prefilled, not typed from nothing (checklist 34a).

- [ ] **Step 3: Wire main.js**

- An `openBoardSendDialog({topic, inReplyTo})` function. The title is `Send to <topic>` or `Reply to #<seq>`. The reply-to input is prefilled with `chat.operator` for a reply and left empty for a new message. Wake is checked. On submit, call `window.harness.boardSend({topic, inReplyTo, replyTo, noWake: !wake.checked, body})`. Write the result with `appendCmdOutput` (checklist 30, not `setStatus`): `sent #<seq> to <topic> (delivered_to=N, wake on|off)`. On error, write `appendCmdOutput(err.message)`. Then `openBoardTopic(currentTopic)` to refresh.
- `board-send-btn` → `openBoardSendDialog({topic: currentTopic})`.
- `board-wake-btn` → `window.harness.boardWake(currentTopic)` → `appendCmdOutput("woke N task(s) on <topic>")`.
- In the `openBoardTopic` card loop (~5592-5631, beside the retract `⊘` button), add a `↩` button titled `Reply as operator`. It calls `openBoardSendDialog({inReplyTo: m.seq})`. Show it on retracted messages too (replying is not destruction).
- Sender display: `board-msg-from` (~5542) shows `m.senderParty`; `renderBoardChains` (~5886) uses `r.from.senderParty`.

- [ ] **Step 4: JS test**

Add a test using `webui/static/harness_env.mjs` (the real bridge). After `boardRead` against a stub or seeded board, every row carries `senderKind` and `senderParty`, and an operator row's `senderParty` is `"operator"`. If `harness_env.mjs` cannot seed a board, test only that `harness.boardSend` and `harness.boardWake` exist and reject cleanly with no client. Write down that limitation in the test file.

Run: `make webui-build && make js-test`
Expected: PASS.

- [ ] **Step 5: Drive it in a real browser**

Per memory "Verify INPUT, not just the render": start the dummy harness (`.claude/skills/dummy-harness/SKILL.md`, `scripts/dummy-harness.sh`). The dummy server serves EMBEDDED assets, so `make build` and restart it, or run it with `--webui-dir`. Use Playwright to:
1. Open a topic and click `✉ Send`, type a body, uncheck wake, and send. The output line says `wake off`, and the card appears with `from=operator`.
2. Click `↩` on that card. The reply-to field reads `chat.operator`.
3. Click `⏰ Wake`. The output line says `woke 0 task(s)` (no subscribers on the dummy).

Save screenshots under the scratchpad and report their paths. Do not delete them.

- [ ] **Step 6: Commit**

```bash
git add cmd/harness-webui-wasm/ webui/
git commit -m "webui: send/reply/wake as operator from the board view; names operator senders"
```

---

### Task 9: Docs, skills, surface walk, end-to-end check

**Files:**
- Modify: `README.md` (board section ~913-995, caps prose ~900)
- Modify: `runner/agentskills/harness-cli/SKILL.md`, then mirror to `.claude/skills/harness-cli/SKILL.md` and `.agents/skills/harness-cli/SKILL.md`
- Modify: `.claude/skills/surface-parity-checklist/firing-log.md`
- Modify: `docs/superpowers/specs/2026-10-04-agentboard-operator-send-design.md` (only if something shipped differently: Amendment section)

- [ ] **Step 1: README**

In the operator board section, document `board send` (`--topic`, `--in-reply-to`, `--reply-to chat.operator`, `--no-wake`) and `board wake`. Cover `chat.operator` (reserved; replies to the operator land there; it ages out with the ordinary topic TTL) and the `no_reply_route` refusal. In the caps prose, add `board_send`.

- [ ] **Step 2: Agent-facing skill**

In `runner/agentskills/harness-cli/SKILL.md`, read the whole file first (prose edit rule). Then add only what an agent acts on:
- A message whose `sender_kind` is `operator` came from the human. If it has no `reply_to_topic`, `--in-reply-to` alone is refused (`no_reply_route`); answer in your own conversation.
- `chat.operator` cannot be subscribed to.
- `sender_kind: server` marks server notices (await-idle).

Do not mention `--no-wake` or `board send`; they are not agent verbs. Copy the file byte-for-byte to both mirrors.

Run: `go test ./runner/agentskills/`
Expected: PASS (`TestMirrorsMatchEmbeddedSkills`).

- [ ] **Step 3: Walk the surface-parity checklist**

Walk items 1-39 of `.claude/skills/surface-parity-checklist/SKILL.md` for this feature, with a verdict per number. Item 15 (`caps` catalog) and item 39 (the spec's own Surfaces table) are the two most likely to fire. Append the walk to `firing-log.md` in its format.

- [ ] **Step 4: Full verification**

Run, in order:

```bash
make test
make build
scripts/wire-skew-check.sh
```

Expected: all pass. `wire-skew-check.sh` asserts a NEW runner × OLD server failure is recoverable; it must report PASS, not "setup error".

Then, against `scripts/dummy-harness.sh` (eval its env; bring it down after), run the exact spellings the help text prints:

```bash
harness-cli board send --topic chat.abcd1234 --reply-to chat.operator please look at the failing test
harness-cli board send --topic chat.abcd1234 --no-wake --data - <<<'queued'
harness-cli board wake chat.abcd1234
harness-cli board read chat.abcd1234
harness-cli board thread
```

Expected:
- Each send prints an ok line with `seq`, `delivered_to`, `woke`, `bytes`, `source`.
- `board wake` prints `woken`.
- `board read` shows `from=operator sender=operator` and never `00000000`.
- `board thread` keys the conversation with `operator` as a party.

Use the worktree's `bin/harness-cli` from `make build`, not a bare `harness-cli` on PATH: memory records that a bare binary in a worktree runs the PARENT build.

- [ ] **Step 5: Commit**

```bash
git add README.md runner/agentskills/ .claude/skills/ .agents/skills/ docs/superpowers/
git commit -m "docs: board send / board wake, chat.operator, sender_kind for agents"
```
