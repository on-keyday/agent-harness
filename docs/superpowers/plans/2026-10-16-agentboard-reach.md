# Agentboard reach — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A task publishes to the agentboard only within its base set (self, creator, direct children, `chat.operator`, contacts, its `tree.` namespaces) unless it holds `board_peer`, and reads another task's direct messages only with `board_observe`. Both are checked on every operation.

**Architecture:** One reach predicate lives in `agentboard` (`reach.go`), fed by a `ReachSource` the server implements over the `TaskStore` and installs in `SetBoard`. Every board entry that hands a task a message or a wake asks it, a go/ast guard test pins the list of entries, and each refused agent verb answers `not_permitted`.

**Tech Stack:** Go, `.bgn` schemas compiled by `make protoregen`.

**Spec:** `docs/superpowers/specs/2026-10-16-agentboard-reach-design.md` (read §1–§5, §7, §8 and the Amendment before Task 1).

## Global Constraints

- **Work in the checkout that holds this plan**: `.harness-worktrees/70fbad4a6eb6f1e992be8a669f1bcefd/`. A bare absolute path under `/home/kforfk/workspace/remote-agent-harness/<rel>` is the PARENT checkout (implementation-pitfalls, Pitfall 8). Check with `git rev-parse --abbrev-ref HEAD` before writing code.
- **Read `.claude/skills/implementation-pitfalls/SKILL.md` in full before writing code.**
- `board_peer = 0x80000`; `all = 0xfffff` (D12).
- A `tree.` topic is `tree.<32 lowercase hex>.<suffix>`; anything else under `tree.` is a free-form name.
- `chat.<id>.*` uses the 8-hex short id (`agentboard.SelfTopic`).
- Contacts: at most 64 per task, least recently added dropped first (§3).
- **The whole `.bgn` change is Task 1.** No later task adds a wire field.
- **No compat shims.** Single-user dogfood.
- Verify with make targets: `make check`, `make vet`, `make wasm-check`, `make test`, `make test-integration`. Never a bare `go build ./cmd/<x>/` (it drops a binary in the worktree).
- Commit after every task; messages end with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

1. **A worker answering its supervisor's message after the parent rotated out.** `--in-reply-to` fails with `unknown_in_reply_to` exactly as today. `--topic chat.<supervisor>` must still pass, because the creator is in the base set. Test: Task 3, `TestPublishableBaseSet` case "creator".
2. **A reply to a seq the replier never received.** Seqs are guessable. The resolved route is not in the replier's contacts, so it must be refused (D4). Test: Task 5, `TestAgentSendRefusesAGuessedReplyRoute`.
3. **`caps set` removing `board_observe` from a task already subscribed to a peer's topic.** The next inbox must return nothing from that topic and move no delivery mark. Re-granting must bring the rows back. Test: Task 4, `TestRevokedObserveHidesAndRestores`.
4. **`caps set-parent` moving a task out of a tree.** Its next read of `tree.<old>.*` must be refused with no resubscribe. Test: Task 2, `TestMembershipFollowsTheLiveParent`.
5. **An operator `board send` / `board wake` to a confined task's own topic.** It must still deliver and wake. The operator face is not reach-checked, and the recipient reads its own topic. Test: Task 4, `TestOperatorPublishStillReachesAConfinedTask`.

---

## File Structure

| File | Responsibility | Task |
| --- | --- | --- |
| `runner/protocol/message.bgn` (+ regen `message.go`) | `board_peer`, `all`, the four `not_permitted` statuses, `AgentWaitResponse.status`, comment rewrites | 1 |
| `runner/protocol/capability_test.go` | `all` = OR of bits | 1 |
| `cli/verb/caps.go` | grantable list and descriptions | 1 |
| `server/scope_percap_completeness_test.go` | classification of the new bit | 1 |
| `agentboard/reach.go` (new) | `ReachSource`, namespace parsing, `Readable`, `Publishable`, membership walk | 2, 3 |
| `agentboard/taskstate.go` | contacts | 3 |
| `agentboard/board.go` | the gated entries of spec §5, `ErrNotPermitted`, `NoteDelivered` | 4 |
| `agentboard/reach_test.go` (new) | predicate, membership, base set, contacts, entry gating | 2, 3, 4 |
| `agentboard/reach_callsites_test.go` (new) | go/ast guard over `matches` / `snapshotPatterns` callers | 4 |
| `server/taskstore_reach.go` (new) | `storeReach` over `TaskStore` | 5 |
| `server/server.go` | `SetBoard` installs it | 5 |
| `server/agent_taskcontrol.go` | statuses, the send check, `NoteDelivered` | 5 |
| `server/reach_handlers_test.go` (new) | handler statuses, D4 | 5 |
| `cli/agent/{send,dispatch,subscribe,wait,retained,thread}.go` | render `not_permitted` | 6 |
| `cli/agent/*_e2e_test.go` | real task records for agents; a reach e2e | 6 |
| `runner/agentskills/harness-cli/SKILL.md` + mirrors, `README.md` | the new rules for agents and operators | 7 |

---

### Task 1: Wire, bit and catalog

**Files:**
- Modify: `runner/protocol/message.bgn` (enum `Capability`; enums `SendStatus`, `SubscribeStatus`, `BoardStatus`; format `AgentWaitResponse`; new enum `WaitStatus`)
- Regenerate: `runner/protocol/message.go` (`make protoregen`)
- Modify: `runner/protocol/capability_test.go`, `cli/verb/caps.go`, `server/scope_percap_completeness_test.go`

**Interfaces:**
- Produces: `protocol.Capability_BoardPeer`, `protocol.SendStatus_NotPermitted`, `protocol.SubscribeStatus_NotPermitted`, `protocol.BoardStatus_NotPermitted`, `protocol.WaitStatus` with `WaitStatus_Ok` / `WaitStatus_NotPermitted`, and `AgentWaitResponse.Status protocol.WaitStatus`.

- [ ] **Step 1: Update the bit test first (it will not compile yet)**

In `runner/protocol/capability_test.go`, change the `Capability_All` check to `0xfffff` (message too) and add `| Capability_BoardPeer` to the OR.

- [ ] **Step 2: Edit `message.bgn`**

Before `# `all` is a LITERAL…` in `enum Capability`:

```
    # board_peer authorizes publishing to a topic outside the task's BASE SET:
    # its own chat.<id> and chat.<id>.*, its creator's and its direct
    # children's chat.<id>, chat.operator, the reply routes of messages it was
    # handed (contacts), and the tree.<id>.* namespaces it belongs to. Without
    # it a task can talk to its own lineage and to whoever talked to it, and
    # cannot reach a peer or a free-form topic. It opens no tree.<id>.* the
    # task is not a member of. Default-off, appended. See
    # docs/superpowers/specs/2026-10-16-agentboard-reach-design.md.
    board_peer     = 0x80000, "board_peer"
```

Change `all = 0x7ffff` to `all = 0xfffff`. Rewrite the `board_observe` comment's second paragraph to:

```
    # "observe" rather than "read": the bit is not needed to send, to read your
    # own inbox, or to read a group topic. It IS needed to read another task's
    # direct messages by any verb — subscribe, wait, inbox, read_seq,
    # list_retained on another task's chat.<id> / chat.<id>.*, on chat.operator,
    # or on a tree.<id>.* the reader does not belong to — because those carry
    # someone else's messages, which is what this bit always meant.
```

Append `not_permitted` as the LAST member of `SendStatus`, `SubscribeStatus` and `BoardStatus`, each with a one-line comment naming the bit that would have allowed it (`board_peer` for send, `board_observe` for the other two). Add after `AgentWaitRequest`:

```
enum WaitStatus:
    :u8
    ok
    # The topic carries another task's messages and the caller holds no
    # board_observe. Distinct from timed_out, which must keep meaning "nothing
    # arrived".
    not_permitted
```

and append to `AgentWaitResponse` after `stream_id :u64`:

```
    status :WaitStatus
```

Rewrite the comment above `format AgentListRetainedRequest`. Its argument ("No cap gate … a strict subset of what subscribing + inbox already returns uncapped") still holds for the caller's own and group topics. For another task's direct messages the subscribe it leans on is now gated too, so the same subset argument puts this read behind `board_observe` (spec §5). Say that, and keep the reason purge stays separate.

- [ ] **Step 3: Regenerate and build**

Run: `make protoregen && go build ./...`
Expected: success; `go test ./runner/protocol/ -run 'TestCapabilityBits|TestResponseRowsAreNotInline'` PASS.

- [ ] **Step 4: Catalog**

In `cli/verb/caps.go` `GrantableCaps`, after `protocol.Capability_BoardSend`:

```go
		// board_peer last among the board bits: it widens where an agent may
		// publish, where board_send changes whose name it publishes in.
		protocol.Capability_BoardPeer,
```

`CapDescription`:

```go
	case protocol.Capability_None:
		return "no capabilities; data-plane only (agentboard messaging within the task's base set — " +
			"self, creator, direct children, chat.operator, whoever messaged it — and own task logs/ls)"
	…
	case protocol.Capability_BoardObserve:
		return "list board topics, read a topic's retained messages, list its subscribers; " +
			"and read another task's direct messages (its chat.<id>, chat.operator, a tree.<id> you are not in) by any agent verb. " +
			"NOT required to send, to read your own inbox, or to read a group topic"
	…
	case protocol.Capability_BoardPeer:
		return "publish to topics outside the task's base set: peers' chat.<id>, free-form topics; " +
			"NOT required for its own lineage, chat.operator, or replying to a message it received; never opens a tree.<id> it is not in"
```

- [ ] **Step 5: Classify the bit**

In `server/scope_percap_completeness_test.go`, after the `Capability_BoardSend` entry:

```go
	protocol.Capability_BoardPeer: {
		kind:   capNoTargetResolution,
		reason: "board_peer names a board TOPIC, for the same reason as board_observe",
	},
```

- [ ] **Step 6: Run and commit**

Run: `go test ./runner/protocol/ ./cli/verb/ ./server/ -count=1`
Expected: PASS. If `server`'s cap completeness test names another table, add `board_peer` to it with the same reason.

```bash
git add runner/protocol/message.bgn runner/protocol/message.go runner/protocol/capability_test.go cli/verb/caps.go server/scope_percap_completeness_test.go
git commit -m "protocol: board_peer, not_permitted statuses, AgentWaitResponse.status"
```

---

### Task 2: The reach predicate (read side) and membership

**Files:**
- Create: `agentboard/reach.go`, `agentboard/reach_test.go`
- Modify: `agentboard/board.go` (one field and `SetReach`)

**Interfaces:**
- Consumes: Task 1's capability names.
- Produces:

```go
// ReachSource is what the board needs from the task store to decide reach.
type ReachSource interface {
	CreatorOf(tid protocol.TaskID) (creator protocol.TaskID, ok bool)
	CapsOf(tid protocol.TaskID) (protocol.Capability, bool)
	HasChild(parent protocol.TaskID, short string) bool
	Len() int
}
func (b *Board) SetReach(r ReachSource)
func (b *Board) Readable(c *ConnState, topic string) bool
func parseTreeTopic(topic string) (protocol.TaskID, bool)
func (b *Board) member(r ReachSource, caller, x protocol.TaskID) bool
var ErrNotPermitted = errors.New("agentboard: not permitted")
```

- [ ] **Step 1: Write the failing tests** (`agentboard/reach_test.go`)

```go
package agentboard

import (
	"encoding/hex"
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// fakeReach is a mutable task table: tests change a parent or a cap between
// calls to show the board reads it on every check, not at subscribe time.
type fakeReach struct {
	creator map[protocol.TaskID]protocol.TaskID
	caps    map[protocol.TaskID]protocol.Capability
}

func newFakeReach() *fakeReach {
	return &fakeReach{creator: map[protocol.TaskID]protocol.TaskID{}, caps: map[protocol.TaskID]protocol.Capability{}}
}
func (f *fakeReach) add(tid, creator protocol.TaskID, caps protocol.Capability) {
	f.creator[tid], f.caps[tid] = creator, caps
}
func (f *fakeReach) CreatorOf(t protocol.TaskID) (protocol.TaskID, bool) { c, ok := f.creator[t]; return c, ok }
func (f *fakeReach) CapsOf(t protocol.TaskID) (protocol.Capability, bool) { c, ok := f.caps[t]; return c, ok }
func (f *fakeReach) HasChild(p protocol.TaskID, short string) bool {
	for t, c := range f.creator {
		if c == p && hexTaskIDProto(t)[:8] == short {
			return true
		}
	}
	return false
}
func (f *fakeReach) Len() int { return len(f.creator) }

func tidN(b byte) protocol.TaskID { var t protocol.TaskID; t.Id[0], t.Id[15] = b, b; return t }
func treeOf(t protocol.TaskID) string { return "tree." + hex.EncodeToString(t.Id[:]) + ".x" }

func reachBoard(t *testing.T, r *fakeReach) *Board {
	t.Helper()
	b := New(Config{RingN: 8, TopicTTL: 3600e9, MaxTopics: 64, MaxPayload: 1024})
	t.Cleanup(func() { b.Close() })
	b.SetReach(r)
	return b
}

func TestReadableNamespaces(t *testing.T) {
	r := newFakeReach()
	root, me, peer := tidN(1), tidN(2), tidN(3)
	r.add(root, protocol.TaskID{}, 0)
	r.add(me, root, 0)
	r.add(peer, protocol.TaskID{}, 0)
	b := reachBoard(t, r)
	c := b.Attach(protocol.RunnerID{}, me, "h", "bash")

	for _, tc := range []struct {
		topic string
		want  bool
	}{
		{SelfTopic(me), true},
		{SelfTopic(me) + ".side", true},
		{SelfTopic(peer), false},
		{SelfTopic(peer) + ".side", false},
		{OperatorTopic, false},
		{treeOf(root), true}, // my ancestor
		{treeOf(me), true},   // myself
		{treeOf(peer), false},
		{"tree.notahex.x", true}, // not a tree namespace: free-form
		{"build.events", true},
	} {
		if got := b.Readable(c, tc.topic); got != tc.want {
			t.Errorf("Readable(%s) = %v, want %v", tc.topic, got, tc.want)
		}
	}
	r.caps[me] = protocol.Capability_BoardObserve
	for _, topic := range []string{SelfTopic(peer), OperatorTopic, treeOf(peer)} {
		if !b.Readable(c, topic) {
			t.Errorf("with board_observe, Readable(%s) = false", topic)
		}
	}
}

func TestMembershipFollowsTheLiveParent(t *testing.T) {
	r := newFakeReach()
	s, w := tidN(1), tidN(2)
	r.add(s, protocol.TaskID{}, 0)
	r.add(w, s, 0)
	b := reachBoard(t, r)
	cw := b.Attach(protocol.RunnerID{}, w, "h", "bash")
	if !b.Readable(cw, treeOf(s)) {
		t.Fatal("worker cannot read its supervisor's tree")
	}
	// set-parent --swap: W becomes S's parent.
	r.creator[w], r.creator[s] = protocol.TaskID{}, w
	if b.Readable(cw, treeOf(s)) {
		t.Error("worker still reads tree.S after leaving it")
	}
	cs := b.Attach(protocol.RunnerID{}, s, "h", "bash")
	if !b.Readable(cs, treeOf(w)) {
		t.Error("supervisor cannot read tree.W after joining it")
	}
}

func TestMembershipEndsAtAPruneHole(t *testing.T) {
	r := newFakeReach()
	root, mid, leaf := tidN(1), tidN(2), tidN(3)
	r.add(root, protocol.TaskID{}, 0)
	r.add(leaf, mid, 0) // mid is not in the table: pruned
	b := reachBoard(t, r)
	if b.Readable(b.Attach(protocol.RunnerID{}, leaf, "h", "bash"), treeOf(root)) {
		t.Error("membership crossed a pruned task (D9)")
	}
}

func TestMembershipWalkIsBounded(t *testing.T) {
	r := newFakeReach()
	a, c := tidN(1), tidN(2)
	r.add(a, c, 0)
	r.add(c, a, 0) // a cycle set_parent refuses; the walk must still stop
	b := reachBoard(t, r)
	if b.Readable(b.Attach(protocol.RunnerID{}, a, "h", "bash"), treeOf(tidN(9))) {
		t.Error("a cycle reported membership")
	}
}

func TestUnknownTaskFailsClosed(t *testing.T) {
	b := reachBoard(t, newFakeReach())
	c := b.Attach(protocol.RunnerID{}, tidN(7), "h", "bash")
	if b.Readable(c, SelfTopic(tidN(8))) {
		t.Error("a task missing from the store read a peer's topic")
	}
	if !b.Readable(c, SelfTopic(tidN(7))) {
		t.Error("a task missing from the store cannot read its own topic")
	}
}

func TestNilReachEnforcesNothing(t *testing.T) {
	b := New(Config{RingN: 8, TopicTTL: 3600e9, MaxTopics: 64, MaxPayload: 1024})
	defer b.Close()
	c := b.Attach(protocol.RunnerID{}, tidN(1), "h", "bash")
	if !b.Readable(c, SelfTopic(tidN(2))) {
		t.Error("a board with no reach source refused a read")
	}
}
```

- [ ] **Step 2: Run to see them fail**

Run: `go test ./agentboard/ -run 'Readable|Membership|UnknownTask|NilReach' -count=1`
Expected: FAIL to compile (`SetReach`, `Readable` undefined).

- [ ] **Step 3: Implement `agentboard/reach.go`**

```go
package agentboard

import (
	"encoding/hex"
	"errors"
	"strings"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// ErrNotPermitted is a board operation the caller's reach does not cover.
// See docs/superpowers/specs/2026-10-16-agentboard-reach-design.md.
var ErrNotPermitted = errors.New("agentboard: not permitted")

// ReachSource is what the board needs from the task store to decide reach.
// The server implements it over its TaskStore (server/taskstore_reach.go).
type ReachSource interface {
	CreatorOf(tid protocol.TaskID) (creator protocol.TaskID, ok bool)
	CapsOf(tid protocol.TaskID) (protocol.Capability, bool)
	// HasChild reports whether a live task whose creator is parent has a
	// task id beginning with short (8 hex).
	HasChild(parent protocol.TaskID, short string) bool
	// Len bounds the membership walk: no chain is longer than the store.
	Len() int
}

// SetReach installs the source every reach check reads. A board without one
// enforces nothing; the server installs one in SetBoard, and the ring and
// delivery tests construct bare boards.
func (b *Board) SetReach(r ReachSource) {
	b.mu.Lock()
	b.reach = r
	b.mu.Unlock()
}

func (b *Board) reachSource() ReachSource {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reach
}

const treeTopicPrefix = "tree."

// parseTreeTopic returns X for tree.<32 hex>.<suffix>. Anything else under
// tree. is a free-form name.
func parseTreeTopic(topic string) (protocol.TaskID, bool) {
	rest, ok := strings.CutPrefix(topic, treeTopicPrefix)
	if !ok || len(rest) < 34 || rest[32] != '.' {
		return protocol.TaskID{}, false
	}
	raw, err := hex.DecodeString(rest[:32])
	if err != nil {
		return protocol.TaskID{}, false
	}
	var x protocol.TaskID
	copy(x.Id[:], raw)
	return x, true
}

// isOwnChat reports whether topic is chat.<self short id> or chat.<short>.*.
func isOwnChat(self protocol.TaskID, topic string) bool {
	own := SelfTopic(self)
	return topic == own || strings.HasPrefix(topic, own+".")
}

// member is §4: x is caller, or x is on caller's creator chain. The walk stops
// at the root, at a task the store no longer has (D9), or after Len steps.
func (b *Board) member(r ReachSource, caller, x protocol.TaskID) bool {
	cur := caller
	for step := 0; step <= r.Len(); step++ {
		if cur == x {
			return true
		}
		next, ok := r.CreatorOf(cur)
		if !ok || next == (protocol.TaskID{}) {
			return false
		}
		cur = next
	}
	return false
}

func hasCap(r ReachSource, tid protocol.TaskID, c protocol.Capability) bool {
	caps, ok := r.CapsOf(tid)
	return ok && caps&c != 0
}

// Readable is §5: may c's task read topic? Called with no board or task lock
// held — it takes the task store's.
func (b *Board) Readable(c *ConnState, topic string) bool {
	r := b.reachSource()
	if r == nil {
		return true
	}
	if c == nil || c.task == nil {
		return false
	}
	_, self, _, _ := c.task.identity()
	return b.readableFor(r, self, topic)
}

func (b *Board) readableFor(r ReachSource, self protocol.TaskID, topic string) bool {
	switch {
	case strings.HasPrefix(topic, SelfTopicPrefix):
		if topic != OperatorTopic && isOwnChat(self, topic) {
			return true
		}
		return hasCap(r, self, protocol.Capability_BoardObserve)
	default:
		if x, ok := parseTreeTopic(topic); ok {
			return b.member(r, self, x) || hasCap(r, self, protocol.Capability_BoardObserve)
		}
		return true
	}
}
```

In `agentboard/board.go`, add to `type Board struct` (beside `onDeliver`):

```go
	reach ReachSource // nil = no reach enforcement; see SetReach
```

- [ ] **Step 4: Run and commit**

Run: `go test ./agentboard/ -count=1`
Expected: PASS (all old tests too: none installs a source).

```bash
git add agentboard/reach.go agentboard/reach_test.go agentboard/board.go
git commit -m "agentboard: reach predicate for reads, and tree membership by ancestor walk"
```

---

### Task 3: Publishing — base set and contacts

**Files:**
- Modify: `agentboard/reach.go`, `agentboard/taskstate.go`, `agentboard/reach_test.go`

**Interfaces:**
- Consumes: Task 2's `ReachSource`, `member`, `hasCap`, `isOwnChat`, `parseTreeTopic`.
- Produces:

```go
func (b *Board) Publishable(c *ConnState, topic string) bool
func (b *Board) NoteDelivered(c *ConnState, msgs []RetainedMessage)
func (t *taskState) addContact(topic string)
func (t *taskState) hasContact(topic string) bool
const maxContacts = 64
```

- [ ] **Step 1: Write the failing tests** (append to `agentboard/reach_test.go`)

```go
func TestPublishableBaseSet(t *testing.T) {
	r := newFakeReach()
	sup, me, child, peer := tidN(1), tidN(2), tidN(3), tidN(4)
	r.add(sup, protocol.TaskID{}, 0)
	r.add(me, sup, 0)
	r.add(child, me, 0)
	r.add(peer, protocol.TaskID{}, 0)
	b := reachBoard(t, r)
	c := b.Attach(protocol.RunnerID{}, me, "h", "bash")

	for _, tc := range []struct {
		name, topic string
		want        bool
	}{
		{"self", SelfTopic(me), true},
		{"self side channel", SelfTopic(me) + ".x", true},
		{"operator", OperatorTopic, true},
		{"creator", SelfTopic(sup), true},
		{"child", SelfTopic(child), true},
		{"peer", SelfTopic(peer), false},
		{"free-form", "build.events", false},
		{"own tree", treeOf(me), true},
		{"ancestor tree", treeOf(sup), true},
		{"foreign tree", treeOf(peer), false},
	} {
		if got := b.Publishable(c, tc.topic); got != tc.want {
			t.Errorf("%s: Publishable(%s) = %v, want %v", tc.name, tc.topic, got, tc.want)
		}
	}

	r.caps[me] = protocol.Capability_BoardPeer
	if !b.Publishable(c, SelfTopic(peer)) || !b.Publishable(c, "build.events") {
		t.Error("board_peer did not open a peer / free-form topic")
	}
	if b.Publishable(c, treeOf(peer)) {
		t.Error("board_peer opened a tree the task is not in (D7)")
	}
}

func TestContactsOpenTheReplyRoute(t *testing.T) {
	r := newFakeReach()
	me, peer := tidN(2), tidN(4)
	r.add(me, protocol.TaskID{}, 0)
	r.add(peer, protocol.TaskID{}, protocol.Capability_BoardPeer)
	b := reachBoard(t, r)
	c := b.Attach(protocol.RunnerID{}, me, "h", "bash")
	if b.Publishable(c, SelfTopic(peer)) {
		t.Fatal("peer reachable before it said anything")
	}
	b.NoteDelivered(c, []RetainedMessage{
		{FromTask: peer, SenderKind: protocol.SenderKind_Agent},
		{FromTask: peer, SenderKind: protocol.SenderKind_Agent, ReplyToTopic: "review.results"},
		{SenderKind: protocol.SenderKind_Operator}, // no route, nothing added
	})
	if !b.Publishable(c, SelfTopic(peer)) {
		t.Error("the sender's chat.<id> did not become a contact")
	}
	if !b.Publishable(c, "review.results") {
		t.Error("a declared reply_to did not become a contact")
	}
}

func TestContactsAreBounded(t *testing.T) {
	ts := newTaskState()
	for i := 0; i < maxContacts+1; i++ {
		ts.addContact(fmt.Sprintf("t.%d", i))
	}
	if ts.hasContact("t.0") {
		t.Error("the oldest contact survived past the bound")
	}
	if !ts.hasContact(fmt.Sprintf("t.%d", maxContacts)) {
		t.Error("the newest contact is missing")
	}
}
```

(add `"fmt"` to the test imports.)

- [ ] **Step 2: Run to see them fail**

Run: `go test ./agentboard/ -run 'Publishable|Contacts' -count=1`
Expected: FAIL to compile.

- [ ] **Step 3: Contacts in `agentboard/taskstate.go`**

Add fields to `taskState` (after `shown`):

```go
	// contacts are reply routes of messages handed to this task (spec §3): the
	// right to answer someone who spoke first, kept here rather than in the
	// ring so a rotated-out message does not take it along. Bounded, oldest
	// first out, because a sender picks ReplyToTopic freely.
	contacts     map[string]struct{}
	contactOrder []string
```

initialise `contacts: make(map[string]struct{})` in `newTaskState`, and add:

```go
const maxContacts = 64

func (t *taskState) addContact(topic string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.contacts[topic]; ok {
		return
	}
	if len(t.contactOrder) == maxContacts {
		delete(t.contacts, t.contactOrder[0])
		t.contactOrder = t.contactOrder[1:]
	}
	t.contacts[topic] = struct{}{}
	t.contactOrder = append(t.contactOrder, topic)
}

func (t *taskState) hasContact(topic string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	_, ok := t.contacts[topic]
	return ok
}
```

- [ ] **Step 4: `Publishable` and `NoteDelivered` in `agentboard/reach.go`**

```go
// Publishable is §2: may c's task publish to topic? The server asks it with
// the topic resolveReplyTarget returned, so a reply is checked where it will
// actually land (D4).
func (b *Board) Publishable(c *ConnState, topic string) bool {
	r := b.reachSource()
	if r == nil {
		return true
	}
	if c == nil || c.task == nil {
		return false
	}
	_, self, _, _ := c.task.identity()
	if x, ok := parseTreeTopic(topic); ok {
		return b.member(r, self, x) // no bit opens a tree the task is not in (D7)
	}
	if isOwnChat(self, topic) || topic == OperatorTopic {
		return true
	}
	if creator, ok := r.CreatorOf(self); ok && creator != (protocol.TaskID{}) && topic == SelfTopic(creator) {
		return true
	}
	if short, ok := strings.CutPrefix(topic, SelfTopicPrefix); ok && len(short) == selfTopicShortLen && r.HasChild(self, short) {
		return true
	}
	if c.task.hasContact(topic) {
		return true
	}
	return hasCap(r, self, protocol.Capability_BoardPeer)
}

// NoteDelivered records the reply route of every message handed to c's task
// (spec §3). The server calls it from the one place wait, inbox,
// inbox_advance and read_seq build their rows.
func (b *Board) NoteDelivered(c *ConnState, msgs []RetainedMessage) {
	if c == nil || c.task == nil {
		return
	}
	for _, m := range msgs {
		switch {
		case m.ReplyToTopic != "":
			c.task.addContact(m.ReplyToTopic)
		case m.SenderKind == protocol.SenderKind_Agent:
			c.task.addContact(SelfTopic(m.FromTask))
		}
	}
}
```

- [ ] **Step 5: Run and commit**

Run: `go test ./agentboard/ -count=1`
Expected: PASS.

```bash
git add agentboard/reach.go agentboard/taskstate.go agentboard/reach_test.go
git commit -m "agentboard: publish base set and contacts"
```

---

### Task 4: Gate every board entry (spec §5 table)

**Files:**
- Modify: `agentboard/board.go` — `Send` (fan-out, `:271-290`), `Wake` (`:337-348`), `Subscribe` (`:209`), `Subscribes` (`:751`), `Inbox` (`:353`), `InboxAdvance` (`:388`), `Wait` (`:425`), `ListSubscribers` (`:896`); delete `ConnState.matches` (`agentboard/conn.go:47`)
- Create: `agentboard/reach_callsites_test.go`
- Modify: `agentboard/reach_test.go`

**Interfaces:**
- Consumes: Task 2's `readableFor`, `reachSource`.
- Produces: `Board.Subscribe` and `Board.Wait` return `ErrNotPermitted`. The rest filter silently.

The rule for every entry: **collect under `b.mu` / `t.mu`, release, then ask the predicate** (it takes the task store's lock — spec §5, Lock order).

- [ ] **Step 1: Write the failing tests** (append to `agentboard/reach_test.go`)

```go
func TestSendFanoutSkipsAnUnreadableSubscriber(t *testing.T) {
	r := newFakeReach()
	owner, snoop := tidN(1), tidN(2)
	r.add(owner, protocol.TaskID{}, 0)
	r.add(snoop, protocol.TaskID{}, protocol.Capability_BoardObserve)
	b := reachBoard(t, r)
	var woke []protocol.TaskID
	b.SetOnDeliver(func(_ protocol.RunnerID, tid protocol.TaskID) { woke = append(woke, tid) })
	co := b.Attach(protocol.RunnerID{}, owner, "h", "bash")
	cs := b.Attach(protocol.RunnerID{}, snoop, "h", "bash")
	if err := b.Subscribe(co, SelfTopic(owner)); err != nil {
		t.Fatal(err)
	}
	if err := b.Subscribe(cs, SelfTopic(owner)); err != nil { // allowed: holds board_observe
		t.Fatal(err)
	}
	r.caps[snoop] = 0 // caps set takes the bit away
	if _, delivered, err := b.Send(SelfTopic(owner), []byte("x"), protocol.RunnerID{}, tidN(9), "h", "", 0); err != nil || delivered != 1 {
		t.Fatalf("delivered = %d, %v; want 1 (the owner only)", delivered, err)
	}
	for _, w := range woke {
		if w == snoop {
			t.Error("a task without board_observe was woken for another task's topic")
		}
	}
}

func TestRevokedObserveHidesAndRestores(t *testing.T) {
	r := newFakeReach()
	owner, snoop := tidN(1), tidN(2)
	r.add(owner, protocol.TaskID{}, 0)
	r.add(snoop, protocol.TaskID{}, protocol.Capability_BoardObserve)
	b := reachBoard(t, r)
	cs := b.Attach(protocol.RunnerID{}, snoop, "h", "bash")
	if err := b.Subscribe(cs, SelfTopic(owner)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.Send(SelfTopic(owner), []byte("x"), protocol.RunnerID{}, tidN(9), "h", "", 0, WithNoWake()); err != nil {
		t.Fatal(err)
	}
	r.caps[snoop] = 0
	if msgs, _ := b.Inbox(cs, 0); len(msgs) != 0 {
		t.Errorf("inbox after revoke returned %d rows (D11: already-retained must hide)", len(msgs))
	}
	if msgs := b.InboxAdvance(cs); len(msgs) != 0 {
		t.Errorf("advance after revoke returned %d rows", len(msgs))
	}
	r.caps[snoop] = protocol.Capability_BoardObserve
	if msgs := b.InboxAdvance(cs); len(msgs) != 1 {
		t.Errorf("advance after re-grant returned %d rows, want 1: the revoked read moved the mark", len(msgs))
	}
}

func TestSubscribeAndWaitRefuseAnUnreadableTopic(t *testing.T) {
	r := newFakeReach()
	me, peer := tidN(1), tidN(2)
	r.add(me, protocol.TaskID{}, 0)
	r.add(peer, protocol.TaskID{}, 0)
	b := reachBoard(t, r)
	c := b.Attach(protocol.RunnerID{}, me, "h", "bash")
	if err := b.Subscribe(c, SelfTopic(peer)); !errors.Is(err, ErrNotPermitted) {
		t.Errorf("subscribe to a peer's topic: %v, want ErrNotPermitted", err)
	}
	if err := b.Subscribe(c, OperatorTopic); !errors.Is(err, ErrReservedTopic) {
		t.Errorf("subscribe to chat.operator: %v, want ErrReservedTopic (unchanged)", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := b.Wait(ctx, c, OperatorTopic, 0, 0); !errors.Is(err, ErrNotPermitted) {
		t.Errorf("wait on chat.operator: %v, want ErrNotPermitted", err)
	}
	if err := b.Subscribe(c, "build.events"); err != nil {
		t.Errorf("subscribe to a group topic: %v, want nil", err)
	}
}

func TestOperatorPublishStillReachesAConfinedTask(t *testing.T) {
	r := newFakeReach()
	me := tidN(1)
	r.add(me, protocol.TaskID{}, 0)
	b := reachBoard(t, r)
	c := b.Attach(protocol.RunnerID{}, me, "h", "bash")
	if err := b.Subscribe(c, SelfTopic(me)); err != nil {
		t.Fatal(err)
	}
	_, delivered, err := b.Send(SelfTopic(me), []byte("op"), protocol.RunnerID{}, protocol.TaskID{}, "", "", 0,
		WithSenderKind(protocol.SenderKind_Operator))
	if err != nil || delivered != 1 {
		t.Fatalf("operator publish: delivered=%d err=%v, want 1", delivered, err)
	}
	if n := b.Wake(SelfTopic(me)); n != 1 {
		t.Errorf("operator wake reached %d tasks, want 1", n)
	}
}
```

(add `"context"`, `"errors"`, `"time"` to the test imports.)

- [ ] **Step 2: The call-site guard** (`agentboard/reach_callsites_test.go`)

```go
package agentboard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"testing"
)

// reachGated lists every Board method that reaches a task's subscription set,
// with whether it must ask the reach predicate (spec §5's table). A method
// that starts calling taskState.matches or snapshotPatterns fails this test
// until it is added here with a decision — the next reader is the gap
// otherwise.
var reachGated = map[string]bool{
	"Send": true, "Wake": true, "Subscribes": true, "Inbox": true,
	"InboxAdvance": true, "ListSubscribers": true,
	"anyTaskMatchesLocked": false, // cleanup: the raw set can only keep more
	"Revoke":               false, // cleanup loop over snapshotPatterns
	"ListSubscriptions":    false, // reports the set itself
}

func TestReachCallSitesAreListed(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "board.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Recv == nil || fn.Body == nil {
			continue
		}
		uses := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && (sel.Sel.Name == "matches" || sel.Sel.Name == "snapshotPatterns") {
				uses = true
			}
			return true
		})
		if !uses {
			continue
		}
		found = append(found, fn.Name.Name)
		gated, listed := reachGated[fn.Name.Name]
		if !listed {
			t.Errorf("Board.%s reaches the subscription set and is not in reachGated: decide whether it asks Readable", fn.Name.Name)
			continue
		}
		if gated {
			called := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if sel, ok := n.(*ast.SelectorExpr); ok && (sel.Sel.Name == "readableFor" || sel.Sel.Name == "Readable") {
					called = true
				}
				return true
			})
			if !called {
				t.Errorf("Board.%s is gated but never calls readableFor/Readable", fn.Name.Name)
			}
		}
	}
	sort.Strings(found)
	t.Logf("subscription-set readers: %v", found)
}
```

- [ ] **Step 3: Run to see the tests fail**

Run: `go test ./agentboard/ -run 'Fanout|Revoked|Refuse|OperatorPublish|CallSites' -count=1`
Expected: FAIL. The snoop is still delivered to and the mark moves; Subscribe and Wait return nil; the guard names `Send`, `Wake`, `Subscribes`, `Inbox`, `InboxAdvance` and `ListSubscribers` as never calling `readableFor`.

- [ ] **Step 4: Gate the entries**

Add to `agentboard/reach.go` the one filter every multi-target entry uses:

```go
// readableTargets keeps the tasks that may read topic. Called after b.mu is
// released: the predicate takes the task store's lock.
func (b *Board) readableTargets(targets []*taskState, topic string) []*taskState {
	r := b.reachSource()
	if r == nil {
		return targets
	}
	kept := targets[:0]
	for _, ts := range targets {
		_, tid, _, _ := ts.identity()
		if b.readableFor(r, tid, topic) {
			kept = append(kept, ts)
		}
	}
	return kept
}

func (b *Board) readableTopic(ts *taskState, topic string) bool {
	r := b.reachSource()
	if r == nil {
		return true
	}
	_, tid, _, _ := ts.identity()
	return b.readableFor(r, tid, topic)
}
```

Then, in `agentboard/board.go`:

- `Send`: after `b.mu.Unlock()` that ends the target loop (`:285`), insert `targets = b.readableTargets(targets, topicName)` before `deliver` and before the returned count is computed. Read the function through first: the `delivered` count must count only kept targets.
- `Wake`: the same, after `b.mu.Unlock()` (`:347`), `targets = b.readableTargets(targets, topicName)`.
- `Subscribe`: after the `OperatorTopic` check, `if !b.readableTopic(c.task, pattern) { return ErrNotPermitted }`.
- `Subscribes`: `return c.task.matches(topicName) && b.readableTopic(c.task, topicName)`.
- `Inbox` / `InboxAdvance`: filter `patterns` right after `snapshotPatterns()`, before taking `b.mu`:

```go
	patterns := c.task.snapshotPatterns()
	kept := patterns[:0]
	for _, p := range patterns {
		if b.readableTopic(c.task, p) {
			kept = append(kept, p)
		}
	}
	patterns = kept
```

  The skipped topic then never reaches `takeUnshown`, so its mark does not move (`TestRevokedObserveHidesAndRestores`).
- `Wait`: first statement after the nil check, `if !b.readableTopic(c.task, topicName) { return nil, false, ErrNotPermitted }`.
- `ListSubscribers`: collect `states` under `b.mu` as today, then filter. When `topic != ""` and `!b.readableTopic(ts, topic)`, `continue`, beside the existing `matches` check.
- Delete `ConnState.matches` in `agentboard/conn.go` (no caller outside tests). Fix any test that used it to call `b.Subscribes`.

- [ ] **Step 5: Run and commit**

Run: `go test ./agentboard/ -count=1 -race`
Expected: PASS, the guard included.

```bash
git add agentboard/
git commit -m "agentboard: every subscription-set reader asks the reach predicate"
```

---

### Task 5: Server — the store source, the send check, statuses

**Files:**
- Create: `server/taskstore_reach.go`, `server/reach_handlers_test.go`
- Modify: `server/server.go` (`SetBoard`, `:1555`), `server/agent_taskcontrol.go`

**Interfaces:**
- Consumes: `agentboard.ReachSource`, `Board.SetReach`, `Board.Publishable`, `Board.Readable`, `Board.NoteDelivered`, `agentboard.ErrNotPermitted`; Task 1's statuses.
- Produces: `type storeReach struct{ s *TaskStore }` implementing `agentboard.ReachSource`; `deliveredBody(b *agentboard.Board, st *agentboard.ConnState, msgs []agentboard.RetainedMessage) *protocol.DeliveredBody`.

- [ ] **Step 1: Write the failing tests** (`server/reach_handlers_test.go`)

```go
package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// reachHandler is newAgentTaskHandler with a real store behind the board's
// reach check, the way SetBoard wires it.
func reachHandler(t *testing.T, caps protocol.Capability) (*TaskHandler, *fakeConn, *agentboard.ConnState, protocol.TaskID) {
	t.Helper()
	h, conn := newBoardTestHandler(t)
	h.Board.SetReach(storeReach{h.Tasks})
	idHex := h.Tasks.Create("repo", "", protocol.TaskKind_Interactive, protocol.ClientKind_Agent,
		protocol.TaskID{}, "", protocol.RunnerSelector{}, nil, caps, Scope{}, "bash")
	tid := hexToTaskID(t, idHex)
	st := h.Board.Attach(protocol.RunnerID{}, tid, "h", "bash")
	h.BoardConnState = func(ConnHandle) *agentboard.ConnState { return st }
	return h, conn, st, tid
}

func TestStoreReach(t *testing.T) {
	s := NewTaskStore()
	parentHex := s.Create("r", "", protocol.TaskKind_Interactive, protocol.ClientKind_Agent, protocol.TaskID{}, "",
		protocol.RunnerSelector{}, nil, protocol.Capability_BoardPeer, Scope{}, "bash")
	parent := hexToTaskID(t, parentHex)
	childHex := s.Create("r", "", protocol.TaskKind_Interactive, protocol.ClientKind_Agent, parent, "",
		protocol.RunnerSelector{}, nil, 0, Scope{}, "bash")
	child := hexToTaskID(t, childHex)
	r := storeReach{s}
	if c, ok := r.CreatorOf(child); !ok || c != parent {
		t.Errorf("CreatorOf(child) = %x, %v", c.Id, ok)
	}
	if caps, ok := r.CapsOf(parent); !ok || caps != protocol.Capability_BoardPeer {
		t.Errorf("CapsOf(parent) = %v, %v", caps, ok)
	}
	if !r.HasChild(parent, childHex[:8]) || r.HasChild(child, parentHex[:8]) {
		t.Error("HasChild got the direction wrong")
	}
	if _, ok := r.CreatorOf(protocol.TaskID{Id: [16]byte{0xee}}); ok {
		t.Error("an unknown task resolved")
	}
}

func TestSetBoardWiresReach(t *testing.T) {
	s := newTestServer(t) // the package's existing Server constructor for tests
	b := newTestBoard(t)
	s.SetBoard(b)
	c := b.Attach(protocol.RunnerID{}, protocol.TaskID{Id: [16]byte{1}}, "h", "bash")
	if b.Readable(c, agentboard.SelfTopic(protocol.TaskID{Id: [16]byte{2}})) {
		t.Error("SetBoard did not install a reach source: an unknown task read a peer's topic")
	}
}

func TestAgentSubscribeAnswersNotPermitted(t *testing.T) {
	h, conn, _, _ := reachHandler(t, 0)
	h.handleAgentSubscribe(conn, 1, "chat.deadbeef", false)
	resp := lastTaskControlResponse(t, conn)
	if got := resp.AgentSubscribe(); got == nil || got.Status != protocol.SubscribeStatus_NotPermitted {
		t.Fatalf("status = %+v, want not_permitted", got)
	}
}

func TestAgentWaitAnswersNotPermitted(t *testing.T) {
	h, conn, _, _ := reachHandler(t, 0)
	r := &protocol.AgentWaitRequest{TimeoutMs: 50}
	r.SetPattern([]byte(agentboard.OperatorTopic))
	h.handleAgentWait(conn, 1, r)
	resp := lastTaskControlResponse(t, conn)
	got := resp.AgentWait()
	if got == nil || got.Status != protocol.WaitStatus_NotPermitted || got.TimedOut != 0 {
		t.Fatalf("wait = %+v, want not_permitted and not timed out", got)
	}
}

func TestAgentListRetainedAnswersNotPermitted(t *testing.T) {
	h, conn, _, _ := reachHandler(t, 0)
	h.Board.Send("chat.deadbeef", []byte("x"), protocol.RunnerID{}, protocol.TaskID{}, "h", "", 0)
	r := &protocol.AgentListRetainedRequest{}
	r.SetTopic([]byte("chat.deadbeef"))
	h.handleAgentListRetained(conn, 1, r)
	resp := lastTaskControlResponse(t, conn)
	if got := resp.AgentListRetained(); got == nil || got.Status != protocol.BoardStatus_NotPermitted || got.StreamId != 0 {
		t.Fatalf("list_retained = %+v, want not_permitted with no stream", got)
	}
}

// D4: the check runs on the topic the reply resolves to. A seq on someone
// else's topic, never handed to the replier, resolves to a route the replier
// has no contact for.
func TestAgentSendRefusesAGuessedReplyRoute(t *testing.T) {
	h, _, st, _ := reachHandler(t, 0)
	var stranger protocol.TaskID
	stranger.Id[0] = 0x77
	seq, _, err := h.Board.Send("chat.elsewhere", []byte("q"), protocol.RunnerID{}, stranger, "h", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	dest, status := resolveReplyTarget(h.Board, "", seq)
	if status != protocol.SendStatus_Ok || dest != agentboard.SelfTopic(stranger) {
		t.Fatalf("resolve = %q, %v", dest, status)
	}
	if h.Board.Publishable(st, dest) {
		t.Fatal("a reply to a never-delivered seq passed the reach check")
	}
	// Handed to the replier, the same route is a contact.
	h.Board.NoteDelivered(st, []agentboard.RetainedMessage{{FromTask: stranger, SenderKind: protocol.SenderKind_Agent}})
	if !h.Board.Publishable(st, dest) {
		t.Error("a reply to a delivered message was refused")
	}
}
```

If the package has no `newTestServer`, use the constructor `server_test.go` already uses for a `*Server` (grep `func newTestServer\|&Server{` in `server/*_test.go`) and name it here — do not build a second one.

- [ ] **Step 2: Run to see them fail**

Run: `go test ./server/ -run 'StoreReach|SetBoardWires|NotPermitted|GuessedReply' -count=1`
Expected: FAIL to compile (`storeReach` undefined).

- [ ] **Step 3: `server/taskstore_reach.go`**

```go
package server

import (
	"encoding/hex"
	"strings"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// storeReach is the agentboard's view of the task store: who created whom,
// and what each task holds. Each call takes the store's read lock once.
type storeReach struct{ s *TaskStore }

func (r storeReach) entry(tid protocol.TaskID) (*TaskEntry, bool) {
	e, ok := r.s.tasks[hex.EncodeToString(tid.Id[:])]
	return e, ok
}

func (r storeReach) CreatorOf(tid protocol.TaskID) (protocol.TaskID, bool) {
	r.s.mu.RLock()
	defer r.s.mu.RUnlock()
	if e, ok := r.entry(tid); ok {
		return e.CreatorTaskID, true
	}
	return protocol.TaskID{}, false
}

func (r storeReach) CapsOf(tid protocol.TaskID) (protocol.Capability, bool) {
	r.s.mu.RLock()
	defer r.s.mu.RUnlock()
	if e, ok := r.entry(tid); ok {
		return e.Capabilities, true
	}
	return protocol.Capability_None, false
}

// HasChild scans the store: about 27 ns a task, measured in
// server/reach_bench_test.go and inside the budget the reach spec §6 set.
func (r storeReach) HasChild(parent protocol.TaskID, short string) bool {
	r.s.mu.RLock()
	defer r.s.mu.RUnlock()
	for _, e := range r.s.tasks {
		if e.CreatorTaskID == parent && strings.HasPrefix(e.ID, short) {
			return true
		}
	}
	return false
}

func (r storeReach) Len() int {
	r.s.mu.RLock()
	defer r.s.mu.RUnlock()
	return len(r.s.tasks)
}
```

`Server.SetBoard` (`server/server.go:1555`), after `s.wireAgentBoardWake(b)`:

```go
	// Reach is decided from the task store on every board read and publish;
	// without this the board enforces nothing (agentboard.SetReach).
	b.SetReach(storeReach{s.tasks})
```

- [ ] **Step 4: Handlers in `server/agent_taskcontrol.go`**

- `handleAgentSend`: inside the goroutine the current line is `destTopic, st := resolveReplyTarget(h.Board, topic, inReplyTo)`, and it SHADOWS the outer `st` (the ConnState). Rename it:

```go
		destTopic, rst := resolveReplyTarget(h.Board, topic, inReplyTo)
		if rst != protocol.SendStatus_Ok {
			reply(rst, 0, 0)
			return
		}
		// D4: checked where the message will land, after the reply routing.
		if !h.Board.Publishable(st, destTopic) {
			reply(protocol.SendStatus_NotPermitted, 0, 0)
			return
		}
```

- `handleAgentSubscribe`: in the `default:` arm,

```go
		if err := h.Board.Subscribe(st, pattern); errors.Is(err, agentboard.ErrNotPermitted) {
			out.Status = protocol.SubscribeStatus_NotPermitted
		} else if err != nil {
			out.Status = protocol.SubscribeStatus_BadPattern
		}
```

- `handleAgentWait`: replace `msgs, timedOut, _ := h.Board.Wait(...)` with

```go
	msgs, timedOut, err := h.Board.Wait(ctx, st, string(r.Pattern), r.Since, r.InReplyTo)
	if errors.Is(err, agentboard.ErrNotPermitted) {
		out.Status = protocol.WaitStatus_NotPermitted
		respond(nil)
		return
	}
```

- `handleAgentListRetained`: before `h.Board.ListRetained`, resolve `st := h.boardState(conn)` and

```go
	if !h.Board.Readable(st, string(r.Topic)) {
		out.Status = protocol.BoardStatus_NotPermitted
		respond(0)
		return
	}
```

- Rewrite the comments that argue "no capability, because subscribe + inbox already expose it": the one above `handleAgentListRetained` in this file, and the orphaned `agentHandleListRetained` block at the end of `server/agent_handler.go`. Each states the new split in one sentence: open on your own and group topics, `board_observe` on another task's direct messages (spec §5).
- `deliveredBody`: change the signature to `deliveredBody(b *agentboard.Board, st *agentboard.ConnState, msgs []agentboard.RetainedMessage) *protocol.DeliveredBody` and make its first line `b.NoteDelivered(st, msgs)`. Update its four callers (wait, inbox, inbox_advance, read_seq) to pass `h.Board, st` (wait's nil-identity branch passes `nil, nil, nil` — guard: `if b != nil { b.NoteDelivered(st, msgs) }`).

- [ ] **Step 5: Run and commit**

Run: `go test ./server/ -count=1`
Expected: PASS. Existing server tests build boards without `SetBoard`, so they are unaffected. A test that fails here is one that goes through `SetBoard` with unregistered tasks. Give it real records, as Task 6 does for the e2e tests.

```bash
git add server/
git commit -m "server: reach from the task store; not_permitted on send, subscribe, wait, list_retained"
```

---

### Task 6: CLI — say what was refused; move e2e agents to real task records

**Files:**
- Modify: `cli/agent/send.go`, `dispatch.go`, `subscribe.go`, `wait.go`, `retained.go`, `thread.go`
- Modify: every `cli/agent/*_e2e_test.go` that calls `board.Registry().Register(` (18 files, about 60 sites — `grep -rln 'Registry().Register(' cli/agent/`)
- Create: `cli/agent/reach_e2e_test.go`

**Interfaces:**
- Consumes: Task 1's statuses; Task 5's server.
- Produces: `func sendRefused(topic string) error` and `func readRefused(topic string) error` in `cli/agent/send.go`; test helper `e2eAgent(t *testing.T, srv *server.Server, board *agentboard.Board, rid protocol.RunnerID, creator protocol.TaskID, caps protocol.Capability) (protocol.TaskID, [16]byte)`.

- [ ] **Step 1: The two messages** (`cli/agent/send.go`)

```go
// sendRefused is SendStatus.not_permitted: the topic is outside this task's
// reach. It names what IS reachable, because the agent reading it has to
// choose between a reachable route and asking for the bit.
func sendRefused(topic string) error {
	return fmt.Errorf("send refused: %s is outside this task's reach. Without board_peer a task publishes to "+
		"its own chat.<id> (and chat.<id>.*), its creator's and its direct children's chat.<id>, chat.operator, "+
		"the reply route of a message it received, and tree.<id> namespaces it belongs to. "+
		"Anything else needs the operator to grant board_peer (caps set <task> --caps …,board_peer)", topic)
}

// readRefused is not_permitted on a read: the topic carries another task's
// messages.
func readRefused(topic string) error {
	return fmt.Errorf("refused: %s carries another task's messages (its chat.<id>, chat.operator, or a tree.<id> "+
		"this task is not in); reading it needs board_observe", topic)
}
```

Wire them:
- `send.go`, before the `resp.Status != protocol.SendStatus_Ok` fallback: `if resp.Status == protocol.SendStatus_NotPermitted { return sendRefused(<the topic the user asked for, or "the reply route" when only --in-reply-to was given>) }`.
- `dispatch.go`, before `sent.Status != protocol.SendStatus_Ok`: the same with `*topic`. After the wait round trip, `if r.Status == protocol.WaitStatus_NotPermitted { return readRefused(waitOn) }`.
- `subscribe.go`, before `r.Status != protocol.SubscribeStatus_Ok`: `if r.Status == protocol.SubscribeStatus_NotPermitted { return readRefused(<pattern>) }`.
- `wait.go`, before reading the body: `if r.Status == protocol.WaitStatus_NotPermitted { return readRefused(*topic) }`.
- `retained.go`, a `case protocol.BoardStatus_NotPermitted: return readRefused(*topic)`.
- `thread.go` `retainedMetas`, a `case protocol.BoardStatus_NotPermitted:` that writes `agent thread: skipping <topic>: needs board_observe` to stderr and returns `nil, nil, nil`. A subscription the task can no longer read must not end the whole thread view.

- [ ] **Step 2: The e2e helper** (append to `cli/agent/agent_e2e_test.go`)

```go
// e2eAgent creates a real task record — creator, caps — and registers a
// ticket for it. The board's reach check reads the store, so an agent whose
// ticket exists without a record holds nothing and reaches only itself.
func e2eAgent(t *testing.T, srv *server.Server, board *agentboard.Board, rid protocol.RunnerID,
	creator protocol.TaskID, caps protocol.Capability) (protocol.TaskID, [16]byte) {
	t.Helper()
	idHex := srv.Tasks().Create("repo", "", protocol.TaskKind_Interactive, protocol.ClientKind_Agent,
		creator, "", protocol.RunnerSelector{}, nil, caps, server.Scope{}, "claude")
	raw, err := hex.DecodeString(idHex)
	if err != nil {
		t.Fatal(err)
	}
	var tid protocol.TaskID
	copy(tid.Id[:], raw)
	var ticket [16]byte
	if _, err := rand.Read(ticket[:]); err != nil {
		t.Fatal(err)
	}
	board.Registry().Register(rid, tid, ticket)
	return tid, ticket
}
```

Check `server.Scope` is exported under that name (`grep -n "^type Scope" server/*.go`). If it is not, the helper creates through whatever exported constructor `startServerE2E` already uses.

- [ ] **Step 3: Migrate the existing e2e tests**

For each file from `grep -rln 'Registry().Register(' cli/agent/`, replace each pair

```go
	tidA := mkTidE2E(…)
	…
	board.Registry().Register(ridA, tidA, ticketA)
```

with `tidA, ticketA := e2eAgent(t, srv, board, ridA, protocol.TaskID{}, protocol.Capability_BoardPeer)`. `board_peer` keeps each test's existing meaning, since they were written when every topic was reachable. Where the file names its server `_`, name it. A test that asserts a fixed task id or a fixed ticket (`TestAgentCLI_E2E_BadTicket`) keeps its own registration and gets a record via `srv.Tasks().Create` only if it needs one. Read each test before editing it.

Run after each file: `go test ./cli/agent/ -run '<that file's tests>' -count=1`.

- [ ] **Step 4: The reach e2e** (`cli/agent/reach_e2e_test.go`)

Using `startServerE2E` / `setAgentEnv` and the `Send` / `Dispatch` / `Subscribe` entries the neighbouring e2e tests already call (copy how `agent_e2e_test.go` invokes `Send` with an args slice and captures stdout), one test with four tasks:

```go
// sup (caps none), worker (creator sup, caps none), peer (caps none),
// helper (caps board_peer).
//  1. worker → chat.<sup>:      ok        (creator)
//  2. sup    → chat.<worker>:   ok        (child)
//  3. worker → chat.<peer>:     error naming board_peer
//  4. helper → chat.<worker>:   ok        (board_peer)
//  5. worker, after reading helper's message via `agent inbox`,
//     → `agent send --in-reply-to <that seq>` with no --topic: ok (contact)
//  6. worker `agent subscribe --topic chat.<peer>`: error naming board_observe
```

Each step asserts the exact outcome. For the errors, `strings.Contains(err.Error(), "board_peer")` / `"board_observe"`.

- [ ] **Step 5: Run and commit**

Run: `go test ./cli/... -count=1`
Expected: PASS.

```bash
git add cli/
git commit -m "cli: name the reach a refused send or read needed; e2e agents get real task records"
```

---

### Task 7: Agents' and operators' documentation, surface walk, verification

**Files:**
- Modify: `runner/agentskills/harness-cli/SKILL.md`, then copy it over `.claude/skills/harness-cli/SKILL.md` and `.agents/skills/harness-cli/SKILL.md` (`TestMirrorsMatchEmbeddedSkills` enforces it)
- Modify: `README.md` (`## Capabilities and scope`, `:837`)
- Modify: `.claude/skills/surface-parity-checklist/firing-log.md`
- Modify: the spec — an Amendment only if something here contradicted it

- [ ] **Step 1: The skill**

Read the whole `runner/agentskills/harness-cli/SKILL.md` first (1103 lines; Edit refuses a file you have not read in full). Add one section, beside the existing material about `chat.<short-id>`, titled `### Who you can reach, and who can read you`. It gives the base-set list from Task 6's `sendRefused` text, the confidential side channel convention (`chat.<your-short-id>.<suffix>`, D6), the `tree.<full-id>.<suffix>` team channel (members only, D7), and what `not_permitted` means on send vs on read. Wherever the skill currently tells an agent it can send to any topic, correct that sentence in place. Then copy over both mirrors.

Two sentences become false and must be rewritten where they stand (they argue "no capability, because subscribe + inbox already expose it uncapped", and the change ends that premise for other tasks' direct messages):
- the `agent retained` section (`### Choosing which message to purge`, today `:386-389`): "It takes **no capability** (like `inbox`/`wait`)…". It now needs none on your own topics and on group topics, and needs `board_observe` on another task's `chat.<id>`, on `chat.operator`, and on a `tree.<id>` you are not in.
- the `agent read` section (today `:219`): "**No capability is required**, for the same reason `retained` needs none". The same correction applies.

Also grep the skill for `no capability`, `any topic`, `uncapped` and `subscribe` + `peer`, and settle each hit against the new rules. The two above are the ones found while planning, not a census.

- [ ] **Step 2: README**

Read `## Capabilities and scope` in full. Add `board_peer` where the board bits are described. State that a task with no bits messages only its base set. Add one sentence on `board_observe` now covering another task's direct messages.

- [ ] **Step 3: The surface-parity walk**

Walk items 1–39 of `.claude/skills/surface-parity-checklist/SKILL.md` for "new capability `board_peer`" with a verdict per number. The expected `done` items: 1 (no flag change, the bit name parses through `ParseCaps`), 5/6/9 (TUI picker and WebUI chips enumerate `GrantableCaps` — confirm in a dummy-harness WebUI that a `board_peer` chip appears, and save the screenshot path), 13/15 (whoami and the `caps` catalog print it through `CapsLabel` / `CapsCatalog`), 22/23 (wasm prefill carries raw bits), 35/36/37. Record the walk in `firing-log.md`.

- [ ] **Step 4: Full verification**

Run: `make check && make vet && make wasm-check && make test && make test-integration && scripts/wire-skew-check.sh`
Expected: all pass. A flake in `make test-integration` is re-run once alone and named in the report with the load average.

Then on a dummy harness (`scripts/dummy-harness.sh up --detach --agent fake --name reach`), with the bash profile:
- a task submitted with `--caps none` runs `harness-cli agent send --topic chat.<another task's short id> x` → exit non-zero, stderr names `board_peer`;
- `harness-cli caps set <that task> --caps board_peer`, then the same command → ok, without restarting the task;
- the same task runs `agent subscribe --topic chat.operator` → refused (reserved, unchanged), and `agent wait --topic chat.operator --timeout 1s` → refused naming `board_observe`.

- [ ] **Step 5: Commit**

```bash
git add runner/agentskills .claude/skills .agents/skills README.md docs/
git commit -m "docs: who an agent can reach; surface walk for board_peer"
```

**Landing order** (wire change: `AgentWaitResponse` grew): restart the server first, then `make build` on every runner host, then operator clients. Before that, old clients against the new server fail to decode `wait`. Note it in the final report.
