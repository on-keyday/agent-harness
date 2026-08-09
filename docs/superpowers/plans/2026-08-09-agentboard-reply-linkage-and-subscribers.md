# Agentboard reply linkage + board subscribers — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make "which message is this a reply to" a server-vouched wire field on the agentboard, and expose which tasks subscribe to which topics.

**Architecture:** Two independent features sharing no schema. (A) `in_reply_to :u64` carries the parent message's `seq` through `SendRequest` → retained ring → `DeliveredMessage` / `RetainedMeta` / `BoardMessageRow`; the server resolves it against the rings and rejects unresolvable links, and derives the destination from the parent's authenticated sender when the topic is omitted. (B) A new `board_subscribers` TaskControl RPC returns each task's subscription set, optionally narrowed to the tasks a publish to one topic would reach.

**Tech Stack:** Go, brgen (`.bgn`) generated wire codecs, bubbletea TUI, vanilla-JS WebUI over a GOOS=js WASM bridge.

**Specs:**
- `docs/superpowers/specs/2026-08-09-agentboard-reply-linkage-design.md`
- `docs/superpowers/specs/2026-08-09-board-subscribers-design.md`

## Global Constraints

- **Read the spec Problem section, not just its Design section.** Anything the Problem states is in scope even if the Design section is silent (project Pitfall 1).
- **`.bgn` files are never hand-edited into Go.** Regenerate with `make protoregen ARGS='<path>.bgn'`. First run downloads `~/.cache/brgen-kit` (~20 MB, one-time, needs network).
- **`.bgn` changes require `scripts/wire-skew-check.sh` before landing** (project Pitfall 10). It is a no-op when no `.bgn` changed, so run it unconditionally at the end.
- **Rollout is SERVER FIRST.** The server runs on a different host from the runners. Restart the server before refreshing runner-side binaries.
- **Operator surface matrix** (project Pitfall 9). For every operator-visible field, each of these is "implemented", "not applicable", or "intentionally omitted because …": CLI binary, TUI keybindings, TUI cmdline, TUI popups, WebUI buttons/forms, WebUI command input, WASM bridge, shared `cli/` + `server/`.
- **Board `seq` exceeds JavaScript's 2^53 safe integer.** Every u64 seq crossing the WASM bridge is a **decimal string**, never a `float64` (existing precedent: `harnessBoardRead` in `cmd/harness-webui-wasm/main.go:656-700`). `in_reply_to` is a seq and obeys this.
- **Verification uses `make` targets**, not ad-hoc `go build ./...`: `make check`, `make wasm-check`, `make vet`, `make test`.
- **Agent skills have two copies.** `runner/agentskills/` is the `go:embed` source of truth; `.claude/skills/` is a synced copy. Edit the embed source, sync the copy, rebuild.
- Commit after each task. Never commit `webui/static/main.wasm` (build artifact).

## File Structure

**Feature A — reply linkage**

| File | Responsibility |
| --- | --- |
| `agentboard/agentboard.bgn` | `SendRequest.in_reply_to`, `SendStatus.unknown_in_reply_to`, `DeliveredMessage.in_reply_to`, `RetainedMeta.in_reply_to` |
| `agentboard/agentboard.go` | generated — never hand-edited |
| `agentboard/topic.go` | `RetainedMessage.InReplyTo`; `topic.append` carries it |
| `agentboard/board.go` | `Board.Send` carries it; new `Board.LookupSeq` |
| `server/agent_handler.go` | validation, rejection, destination derivation, field propagation |
| `runner/protocol/message.bgn` | `BoardMessageRow.in_reply_to` |
| `server/board_handler.go` | fills the new row field |
| `cli/board.go` | `BoardMessage.InReplyTo` |
| `cli/cmd_board.go` | `board read` column + `--in-reply-to` filter |
| `cli/agent/send.go` | `--in-reply-to`, `--topic` optional on a reply |
| `cli/agent/json_emit.go` | `in_reply_to` in every JSON-Lines record |
| `cli/agent/inbox.go` | `--in-reply-to` client-side filter |
| `tui/board.go` | parent seq in the message list |
| `webui/static/main.js` | parent seq in the board panel |
| `cmd/harness-webui-wasm/main.go` | `inReplyTo` as a decimal string |
| `runner/agentskills/harness-cli/SKILL.md` + `.claude/skills/harness-cli/SKILL.md` | replying uses `--in-reply-to` |

**Feature B — subscribers**

| File | Responsibility |
| --- | --- |
| `agentboard/board.go` | `SubscriberRow`, `Board.ListSubscribers` |
| `runner/protocol/message.bgn` | `board_subscribers` kind + 4 formats |
| `server/board_handler.go` | `handleBoardSubscribers` |
| `server/task_handler.go` | dispatch |
| `server/capabilities.go` | `info_global` gate |
| `cli/board.go` | `BoardSubscriberRow`, `Client.BoardSubscribers`, package-level `BoardSubscribers` |
| `cli/cmd_board.go` | `subscribers` verb |
| `tui/board.go` | subscribers mode |
| `webui/static/main.js`, `cmd/harness-webui-wasm/main.go` | subscribers panel + bridge |

---

# Feature A — reply linkage

### Task A1: Wire schema + board storage

**Files:**
- Modify: `agentboard/agentboard.bgn:43-53` (SendRequest), `:55-60` (SendStatus), `:86-87` (DeliveredMessage), `:203-204` (RetainedMeta)
- Modify: `runner/protocol/message.bgn:673-686` (BoardMessageRow)
- Modify: `agentboard/topic.go:11-25` (RetainedMessage), `:41-62` (append)
- Modify: `agentboard/board.go:206-259` (Send)
- Test: `agentboard/topic_test.go`, `agentboard/board_test.go`

**Interfaces:**
- Produces: `RetainedMessage.InReplyTo uint64`; `Board.Send(topicName string, payload []byte, fromRid protocol.RunnerID, fromTid protocol.TaskID, fromHost, fromProfile string, inReplyTo uint64) (uint64, error)`; generated `agentboard.SendRequest.InReplyTo`, `agentboard.SendStatus_UnknownInReplyTo`, `agentboard.DeliveredMessage.InReplyTo`, `agentboard.RetainedMeta.InReplyTo`, `protocol.BoardMessageRow.InReplyTo`.

- [ ] **Step 1: Edit `agentboard/agentboard.bgn`**

`SendRequest` becomes:

```
format SendRequest:
    request_id :u32
    # in_reply_to is the seq of the message this one answers; 0 = not a reply.
    # The server resolves it against the retained rings at publish time and
    # rejects the send when it cannot (SendStatus.unknown_in_reply_to), so a
    # non-zero value on a delivered message always denotes a real message.
    in_reply_to :u64
    topic_len :u16
    topic :[topic_len]u8
    # An empty topic is meaningful only on a reply: the server then derives the
    # destination from the parent's authenticated sender (its chat.<short-id>).
    topic_len != 0 || in_reply_to != 0
    # payload_stream_id references a client-initiated trsf send-stream
    # carrying the message payload; the server reads from the matching
    # receive stream until EOF and decodes the bytes as the publish body.
    # Streamed instead of inline so multi-KB payloads don't blow path MTU
    # on UDP. See docs/superpowers/specs/2026-05-09-udp-dualstack-design.md
    # §12.1.
    payload_stream_id :u64
```

Append to `SendStatus` (keeps existing ordinals stable):

```
enum SendStatus:
    :u8
    ok
    payload_too_large
    too_many_topics
    bad_frame
    unknown_in_reply_to
```

In `DeliveredMessage`, insert after `seq :u64`:

```
    # in_reply_to is the seq of the message this one answers, 0 when it is not
    # a reply. Server-validated at publish time — see SendRequest.in_reply_to.
    in_reply_to :u64
```

In `RetainedMeta`, insert the same two lines after `seq :u64`.

- [ ] **Step 2: Edit `runner/protocol/message.bgn`**

In `BoardMessageRow`, insert after `seq :u64`:

```
    # See agentboard/agentboard.bgn SendRequest.in_reply_to — same value,
    # 0 when the message is not a reply.
    in_reply_to :u64
```

- [ ] **Step 3: Regenerate**

```bash
make protoregen ARGS='agentboard/agentboard.bgn'
make protoregen ARGS='runner/protocol/message.bgn'
```

Expected: `agentboard/agentboard.go` and `runner/protocol/message.go` change; `git diff --stat` shows only those two generated files.

**If brgen rejects the `topic_len != 0 || in_reply_to != 0` assertion** (it is the first cross-field assertion in this schema; `RunnerID:7` only constrains the field immediately above it): delete that one line, regenerate, and instead enforce it in `agentHandleSend` in Task A3 as the first check, returning `SendStatus_BadFrame`. Record the fallback in the `SendRequest` comment by replacing the assertion line with `# INVARIANT (enforced in server/agent_handler.go, not expressible here): topic_len != 0 || in_reply_to != 0`. Do not invent a third option.

- [ ] **Step 4: Write the failing storage tests**

Append to `agentboard/topic_test.go`:

```go
func TestTopic_Append_CarriesInReplyTo(t *testing.T) {
	tp := newTopic("t", 4)
	tp.append(1, []byte("parent"), zeroRid, zeroTid, "h", "", 0)
	tp.append(2, []byte("child"), zeroRid, zeroTid, "h", "", 1)
	got := tp.snapshot()
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].InReplyTo != 0 {
		t.Errorf("parent InReplyTo = %d, want 0", got[0].InReplyTo)
	}
	if got[1].InReplyTo != 1 {
		t.Errorf("child InReplyTo = %d, want 1", got[1].InReplyTo)
	}
}
```

Append to `agentboard/board_test.go`:

```go
func TestBoard_Send_RetainsInReplyTo(t *testing.T) {
	b := New(Config{RingN: 8, TopicTTL: time.Hour, MaxTopics: 8, MaxPayload: 1024})
	defer b.Close()
	var rid protocol.RunnerID
	rid.SetTransport([]byte("ws"))
	rid.SetIpAddr([]byte{1, 2, 3, 4})
	var tid protocol.TaskID
	tid.Id[0] = 1

	parent, err := b.Send("t", []byte("q"), rid, tid, "h", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Send("t", []byte("a"), rid, tid, "h", "", parent); err != nil {
		t.Fatal(err)
	}
	msgs, found := b.ListRetained("t")
	if !found || len(msgs) != 2 {
		t.Fatalf("ListRetained = %v %d, want true 2", found, len(msgs))
	}
	if msgs[1].InReplyTo != parent {
		t.Errorf("InReplyTo = %d, want %d", msgs[1].InReplyTo, parent)
	}
}
```

- [ ] **Step 5: Run the tests and verify they fail**

Run: `go test ./agentboard/ -run 'InReplyTo' -v`
Expected: compile failure — `too many arguments in call to tp.append` / `b.Send`.

- [ ] **Step 6: Thread the field through storage**

`agentboard/topic.go` — add to `RetainedMessage` after `Seq`:

```go
	// InReplyTo is the seq of the message this one answers, 0 when it is not a
	// reply. Validated by the server at publish time, so a non-zero value here
	// referred to a real message when it was accepted — the parent may since
	// have been evicted from its ring.
	InReplyTo uint64
```

Change `append` to take it as the last parameter and store it:

```go
func (t *topic) append(seq uint64, payload []byte, fromRid protocol.RunnerID, fromTid protocol.TaskID, fromHost, fromProfile string, inReplyTo uint64) {
```

and inside the `RetainedMessage{...}` literal add `InReplyTo: inReplyTo,`.

`agentboard/board.go` — change `Send` to take `inReplyTo uint64` as the last parameter and pass it to `t.append(...)`. Extend the doc comment with:

```go
// inReplyTo is the parent message's seq, or 0. Send does NOT validate it —
// resolution is the caller's job (server/agent_handler.go, via LookupSeq),
// because rejecting a send is a protocol-level decision and the board is the
// storage layer.
```

- [ ] **Step 7: Fix every call site the compiler names**

Run: `go build ./... && go vet ./...`
Add `, 0` to each reported `Board.Send` / `topic.append` call. Non-test call sites are `server/agent_handler.go:161` and `server/await_idle_handler.go:168` — both pass `0` for now (A3 replaces the first). Test call sites live in `agentboard/board_test.go`, `agentboard/board_listings_test.go`, `agentboard/topic_test.go`, `agentboard/e2e_test.go`, `server/board_handler_test.go`, `server/capabilities_test.go`, `cli/board_e2e_test.go`. Let the compiler enumerate them rather than grepping — `.Send(` also matches unrelated types.

- [ ] **Step 8: Run the tests and verify they pass**

Run: `go test ./agentboard/ ./server/ ./cli/... -count=1`
Expected: PASS.

- [ ] **Step 9: Commit**

```bash
git add agentboard/ runner/protocol/ server/ cli/
git commit -m "feat(agentboard): carry in_reply_to through the retained ring

Schema-level parent link: SendRequest gains in_reply_to (the parent's
seq, 0 = not a reply) plus an assertion that an empty topic is only
meaningful on a reply, SendStatus gains unknown_in_reply_to, and
DeliveredMessage / RetainedMeta / BoardMessageRow carry the field out.

Board.Send takes it and stores it verbatim; validation is deliberately
not here — rejecting a publish is a protocol decision and the board is
the storage layer."
```

---

### Task A2: `Board.LookupSeq`

**Files:**
- Modify: `agentboard/board.go` (add after `ListRetained`, `:401-410`)
- Test: `agentboard/board_test.go`

**Interfaces:**
- Consumes: `RetainedMessage.InReplyTo` (A1).
- Produces: `func (b *Board) LookupSeq(seq uint64) (topic string, fromTid protocol.TaskID, ok bool)`.

- [ ] **Step 1: Write the failing tests**

Append to `agentboard/board_test.go`:

```go
func TestBoard_LookupSeq_AcrossTopics(t *testing.T) {
	b := New(Config{RingN: 8, TopicTTL: time.Hour, MaxTopics: 8, MaxPayload: 1024})
	defer b.Close()
	var rid protocol.RunnerID
	rid.SetTransport([]byte("ws"))
	rid.SetIpAddr([]byte{1, 2, 3, 4})
	var tid protocol.TaskID
	tid.Id[0] = 7

	if _, err := b.Send("a", []byte("1"), rid, tid, "h", "", 0); err != nil {
		t.Fatal(err)
	}
	seqB, err := b.Send("b", []byte("2"), rid, tid, "h", "", 0)
	if err != nil {
		t.Fatal(err)
	}

	topic, gotTid, ok := b.LookupSeq(seqB)
	if !ok {
		t.Fatal("LookupSeq ok = false, want true")
	}
	if topic != "b" {
		t.Errorf("topic = %q, want %q", topic, "b")
	}
	if gotTid.Id != tid.Id {
		t.Errorf("task = %x, want %x", gotTid.Id, tid.Id)
	}
}

func TestBoard_LookupSeq_Unknown(t *testing.T) {
	b := New(Config{RingN: 8, TopicTTL: time.Hour, MaxTopics: 8, MaxPayload: 1024})
	defer b.Close()
	if _, _, ok := b.LookupSeq(0); ok {
		t.Error("LookupSeq(0) ok = true, want false")
	}
	if _, _, ok := b.LookupSeq(999999); ok {
		t.Error("LookupSeq(999999) ok = true, want false")
	}
}

func TestBoard_LookupSeq_GoneAfterEviction(t *testing.T) {
	// RingN 2: the third send pushes the first out of the ring.
	b := New(Config{RingN: 2, TopicTTL: time.Hour, MaxTopics: 8, MaxPayload: 1024})
	defer b.Close()
	var rid protocol.RunnerID
	rid.SetTransport([]byte("ws"))
	rid.SetIpAddr([]byte{1, 2, 3, 4})
	var tid protocol.TaskID
	tid.Id[0] = 7

	first, err := b.Send("a", []byte("1"), rid, tid, "h", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := b.Send("a", []byte("x"), rid, tid, "h", "", 0); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, ok := b.LookupSeq(first); ok {
		t.Error("ring-evicted seq still resolves")
	}
}

func TestBoard_LookupSeq_GoneAfterPurge(t *testing.T) {
	b := New(Config{RingN: 8, TopicTTL: time.Hour, MaxTopics: 8, MaxPayload: 1024})
	defer b.Close()
	var rid protocol.RunnerID
	rid.SetTransport([]byte("ws"))
	rid.SetIpAddr([]byte{1, 2, 3, 4})
	var tid protocol.TaskID
	tid.Id[0] = 7

	seq, err := b.Send("a", []byte("1"), rid, tid, "h", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if removed, found := b.PurgeSeq("a", seq); !removed || !found {
		t.Fatalf("PurgeSeq = %v %v, want true true", removed, found)
	}
	if _, _, ok := b.LookupSeq(seq); ok {
		t.Error("purged seq still resolves")
	}

	seq2, err := b.Send("a", []byte("2"), rid, tid, "h", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if purged, found := b.PurgeTopic("a"); !found || purged != 1 {
		t.Fatalf("PurgeTopic = %d %v, want 1 true", purged, found)
	}
	if _, _, ok := b.LookupSeq(seq2); ok {
		t.Error("seq in a purged topic still resolves")
	}
}
```

- [ ] **Step 2: Run the tests and verify they fail**

Run: `go test ./agentboard/ -run LookupSeq -v`
Expected: FAIL — `b.LookupSeq undefined`.

- [ ] **Step 3: Implement `LookupSeq`**

Add to `agentboard/board.go` after `ListRetained`:

```go
// LookupSeq resolves a published seq to the topic whose ring still retains it
// and the task that published it. ok is false for seq 0, for a seq that was
// never published, and for one whose entry has since left its ring (count
// overflow, TTL eviction, purge, or Revoke) — the three are indistinguishable
// and are treated alike: the message is not on the board.
//
// This is a full scan of every ring, deliberately: the alternative is a
// seq -> topic index that must be invalidated in six places (ring overflow in
// topic.append, removeSeq, PurgeTopic, PurgeSeq, both evict paths, and the
// topic deletion in Revoke), and a missed one desynchronizes it from the rings
// intermittently. The scan holds no derived state so it cannot desynchronize.
// Cost is bounded by MaxTopics * RingN — 1024 * 64 with the shipped defaults —
// against a publish rate driven by agent turns. Raising either bound by an
// order of magnitude is the trigger to reconsider the index.
//
// A message evicted between the snapshot and its ring's scan is missed,
// yielding a spurious "not found". That is the same approximate-read tradeoff
// already accepted in evictExpiredTopics.
func (b *Board) LookupSeq(seq uint64) (string, protocol.TaskID, bool) {
	if seq == 0 {
		return "", protocol.TaskID{}, false
	}
	b.mu.Lock()
	names := make([]string, 0, len(b.topics))
	tps := make([]*topic, 0, len(b.topics))
	for n, t := range b.topics {
		names = append(names, n)
		tps = append(tps, t)
	}
	b.mu.Unlock()

	for i, t := range tps {
		for _, m := range t.snapshot() {
			if m.Seq == seq {
				return names[i], m.FromTask, true
			}
		}
	}
	return "", protocol.TaskID{}, false
}
```

- [ ] **Step 4: Run the tests and verify they pass**

Run: `go test ./agentboard/ -run LookupSeq -v`
Expected: PASS (4 tests).

- [ ] **Step 5: Commit**

```bash
git add agentboard/board.go agentboard/board_test.go
git commit -m "feat(agentboard): Board.LookupSeq resolves a seq to its topic and sender

Full scan of the rings rather than a seq->topic index. The index would
need invalidating in six places and desyncs intermittently when one is
missed; the scan holds no derived state and is bounded by
MaxTopics * RingN (1024*64 shipped) against an agent-turn publish rate."
```

---

### Task A3: Server validation and destination derivation

**Files:**
- Modify: `server/agent_handler.go:145-200` (`agentHandleSend`)
- Test: `server/agent_handler_reply_test.go` (create)

**Interfaces:**
- Consumes: `Board.LookupSeq` (A2), `Board.Send(..., inReplyTo)` (A1), `agentboard.SelfTopic` (`agentboard/ids.go:44-50`), `agentboard.SendStatus_UnknownInReplyTo` (A1).
- Produces: the wire behaviour Task A4's CLI depends on.

- [ ] **Step 1: Read the existing handler**

Run: `sed -n '145,200p' server/agent_handler.go`
Note how it builds and sends `SendResponse`; the new failure path reuses that shape with `Seq: 0`.

- [ ] **Step 2: Write the failing tests**

Create `server/agent_handler_reply_test.go`. Model the harness on the existing `server/board_handler_test.go` — read it first and reuse its board/server construction rather than inventing one.

```go
package server

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// resolveReplyTarget is the pure part of agentHandleSend's new logic: it maps
// (requested topic, in_reply_to) to the topic that will actually be published
// to, or reports that the parent could not be resolved.
func TestResolveReplyTarget_DerivesParentSenderTopic(t *testing.T) {
	b := agentboard.New(agentboard.Config{RingN: 8, TopicTTL: time.Hour, MaxTopics: 8, MaxPayload: 1024})
	defer b.Close()

	var rid protocol.RunnerID
	rid.SetTransport([]byte("ws"))
	rid.SetIpAddr([]byte{1, 2, 3, 4})
	var parentTid protocol.TaskID
	for i := range parentTid.Id {
		parentTid.Id[i] = byte(i + 1)
	}

	parent, err := b.Send("chat.deadbeef", []byte("q"), rid, parentTid, "h", "", 0)
	if err != nil {
		t.Fatal(err)
	}

	topic, ok := resolveReplyTarget(b, "", parent)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if want := agentboard.SelfTopic(parentTid); topic != want {
		t.Errorf("topic = %q, want %q", topic, want)
	}
}

func TestResolveReplyTarget_ExplicitTopicWins(t *testing.T) {
	b := agentboard.New(agentboard.Config{RingN: 8, TopicTTL: time.Hour, MaxTopics: 8, MaxPayload: 1024})
	defer b.Close()

	var rid protocol.RunnerID
	rid.SetTransport([]byte("ws"))
	rid.SetIpAddr([]byte{1, 2, 3, 4})
	var parentTid protocol.TaskID
	parentTid.Id[0] = 9

	parent, err := b.Send("chat.deadbeef", []byte("q"), rid, parentTid, "h", "", 0)
	if err != nil {
		t.Fatal(err)
	}

	topic, ok := resolveReplyTarget(b, "rr.dec-019", parent)
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if topic != "rr.dec-019" {
		t.Errorf("topic = %q, want %q", topic, "rr.dec-019")
	}
}

func TestResolveReplyTarget_UnknownParent(t *testing.T) {
	b := agentboard.New(agentboard.Config{RingN: 8, TopicTTL: time.Hour, MaxTopics: 8, MaxPayload: 1024})
	defer b.Close()
	if _, ok := resolveReplyTarget(b, "chat.aaaa", 424242); ok {
		t.Error("ok = true for an unpublished parent, want false")
	}
}

func TestResolveReplyTarget_NotAReply(t *testing.T) {
	b := agentboard.New(agentboard.Config{RingN: 8, TopicTTL: time.Hour, MaxTopics: 8, MaxPayload: 1024})
	defer b.Close()
	topic, ok := resolveReplyTarget(b, "plain", 0)
	if !ok {
		t.Fatal("ok = false for a non-reply, want true")
	}
	if topic != "plain" {
		t.Errorf("topic = %q, want %q", topic, "plain")
	}
}
```

- [ ] **Step 3: Run the tests and verify they fail**

Run: `go test ./server/ -run ResolveReplyTarget -v`
Expected: FAIL — `undefined: resolveReplyTarget`.

- [ ] **Step 4: Implement `resolveReplyTarget` and wire it into the handler**

Add to `server/agent_handler.go`:

```go
// resolveReplyTarget maps a send request's (topic, in_reply_to) to the topic
// actually published to. inReplyTo == 0 passes the requested topic through. A
// non-zero inReplyTo must resolve to a message still on the board; when it
// does and the request named no topic, the destination is the parent sender's
// own inbound topic — taken from the retained entry, so it is the server's
// attested sender and not something the requester supplied.
func resolveReplyTarget(b *agentboard.Board, topic string, inReplyTo uint64) (string, bool) {
	if inReplyTo == 0 {
		return topic, true
	}
	_, parentTid, ok := b.LookupSeq(inReplyTo)
	if !ok {
		return "", false
	}
	if topic == "" {
		return agentboard.SelfTopic(parentTid), true
	}
	return topic, true
}
```

In `agentHandleSend`, before the publish (`server/agent_handler.go:161`), replace the `Board.Send` call site with:

```go
		fromRid, fromTid, fromHost, fromProfile := ac.state.Identity()
		destTopic, ok := resolveReplyTarget(s.Board, string(r.Topic), r.InReplyTo)
		if !ok {
			resp := &agentboard.AgentMessage{Kind: agentboard.AgentMessageKind_SendResponse}
			resp.SetSendResponse(agentboard.SendResponse{
				RequestId: r.RequestId,
				Status:    agentboard.SendStatus_UnknownInReplyTo,
				Seq:       0,
			})
			s.sendAgent(conn, resp)
			return
		}
		seq, sendErr := s.Board.Send(destTopic, payload, fromRid, fromTid, fromHost, fromProfile, r.InReplyTo)
```

Keep the existing status mapping for `sendErr` below it unchanged.

Also propagate the field where delivered/retained messages are built: search for the constructions of `agentboard.DeliveredMessage` and `agentboard.RetainedMeta` in `server/agent_handler.go` and set `InReplyTo: m.InReplyTo` from the `RetainedMessage`. Do not guess the line numbers — run `grep -n 'DeliveredMessage{\|RetainedMeta{' server/agent_handler.go` and wire **every** hit; the inbox, wait, deliver and list-retained paths each build one, and a partially-wired interceptor is a known trap on this project.

- [ ] **Step 5: Run the tests and verify they pass**

Run: `go test ./server/ -run ResolveReplyTarget -v && go test ./server/ -count=1`
Expected: PASS. Note `TestOpenInteractive*` SessionMux flakiness is pre-existing (~1 in 4 runs) — re-run once before blaming this change.

- [ ] **Step 6: Commit**

```bash
git add server/agent_handler.go server/agent_handler_reply_test.go
git commit -m "feat(server): validate in_reply_to and derive the reply destination

A non-zero in_reply_to must resolve to a message still on the board or
the publish is rejected with unknown_in_reply_to — a link the server
cannot vouch for is worse than no link, because a collector would then
have to distrust every link.

When a reply names no topic the destination comes from the parent's
retained entry, i.e. the server's attested sender, never from the
requester."
```

---

### Task A4: Agent CLI — send, emit, filter

**Files:**
- Modify: `cli/agent/send.go:18-60` (flags), `:100-110` (request build)
- Modify: `cli/agent/json_emit.go:24-41`
- Modify: `cli/agent/inbox.go:34-50` (flags), `:100-120` (emit loop)
- Modify: `cli/agent/wait.go`, `cli/agent/dispatch.go`, `cli/agent/retained.go` — `emitMessageLine` call sites
- Test: `cli/agent/send_test.go` (create), `cli/agent/json_emit_test.go` (create)

**Interfaces:**
- Consumes: `agentboard.SendRequest.InReplyTo`, `agentboard.SendStatus_UnknownInReplyTo` (A1); server behaviour (A3).
- Produces: `emitMessageLine(w io.Writer, seq uint64, topic string, payload []byte, fromRid agentboard.RunnerID, fromTid agentboard.TaskID, fromHost, fromAgent string, inReplyTo uint64)`.

- [ ] **Step 1: Write the failing tests**

Create `cli/agent/json_emit_test.go`:

```go
package agent

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/on-keyday/agent-harness/agentboard"
)

func TestEmitMessageLine_InReplyToAlwaysPresent(t *testing.T) {
	var rid agentboard.RunnerID
	rid.SetTransport([]byte("ws"))
	rid.SetIpAddr([]byte{1, 2, 3, 4})
	var tid agentboard.TaskID

	for _, tc := range []struct {
		name string
		in   uint64
	}{{"not a reply", 0}, {"reply", 42}} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			emitMessageLine(&buf, 7, "t", []byte("hi"), rid, tid, "h", "claude", tc.in)
			var rec map[string]any
			if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
				t.Fatal(err)
			}
			v, ok := rec["in_reply_to"]
			if !ok {
				t.Fatal("in_reply_to absent; it must be emitted unconditionally")
			}
			if uint64(v.(float64)) != tc.in {
				t.Errorf("in_reply_to = %v, want %d", v, tc.in)
			}
		})
	}
}
```

Create `cli/agent/send_test.go`:

```go
package agent

import "testing"

func TestSendTargetArgs_RejectsNeitherTopicNorReply(t *testing.T) {
	if _, err := sendTargetArgs("", 0); err == nil {
		t.Error("err = nil, want an error when neither --topic nor --in-reply-to is given")
	}
}

func TestSendTargetArgs_TopicOnly(t *testing.T) {
	got, err := sendTargetArgs("chat.abcd1234", 0)
	if err != nil {
		t.Fatal(err)
	}
	if got != "chat.abcd1234" {
		t.Errorf("topic = %q, want %q", got, "chat.abcd1234")
	}
}

func TestSendTargetArgs_ReplyOnlyLeavesTopicEmpty(t *testing.T) {
	got, err := sendTargetArgs("", 99)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Errorf("topic = %q, want empty so the server derives it", got)
	}
}
```

- [ ] **Step 2: Run the tests and verify they fail**

Run: `go test ./cli/agent/ -run 'InReplyTo|SendTargetArgs' -v`
Expected: FAIL — `undefined: sendTargetArgs`, and `emitMessageLine` arity mismatch.

- [ ] **Step 3: Implement**

`cli/agent/json_emit.go` — add the parameter and the field. Extend the doc comment with:

```go
// in_reply_to is emitted on every record, 0 when the message is not a reply,
// for the same reason the from block is unconditional: a consumer can address
// the field without probing for it.
```

and in the `rec` literal add `"in_reply_to": inReplyTo,`.

`cli/agent/send.go` — add the flag beside `--topic`:

```go
	inReplyTo := fs.Uint64("in-reply-to", 0, "seq of the message being replied to; with it, --topic may be omitted and the server routes to the parent's sender")
```

Replace the `if *topic == ""` guard with a call to a new helper:

```go
// sendTargetArgs validates the destination pair and returns the topic to put on
// the wire. An empty topic is legal only alongside a non-zero in-reply-to, in
// which case the server derives the destination from the parent's authenticated
// sender; the schema encodes the same rule, this is the early, legible error.
func sendTargetArgs(topic string, inReplyTo uint64) (string, error) {
	if topic == "" && inReplyTo == 0 {
		return "", errors.New("--topic required (or --in-reply-to, to reply to the sender of that message)")
	}
	return topic, nil
}
```

wired as:

```go
	wireTopic, err := sendTargetArgs(*topic, *inReplyTo)
	if err != nil {
		return err
	}
```

Set the field on the request and keep `SetTopic` unconditional (an empty slice is the wire's "derive it"):

```go
	req := agentboard.SendRequest{RequestId: reqID, PayloadStreamId: uint64(stream.ID()), InReplyTo: *inReplyTo}
	req.SetTopic([]byte(wireTopic))
```

Give the rejection a specific message in the response switch:

```go
		if resp.Status == agentboard.SendStatus_UnknownInReplyTo {
			return fmt.Errorf("send rejected: --in-reply-to %d is not on the board (evicted past the %s ring or TTL, or purged). Drop --in-reply-to to send it as an ordinary message", *inReplyTo, "per-topic")
		}
		if resp.Status != agentboard.SendStatus_Ok {
			return fmt.Errorf("send rejected: %v", resp.Status)
		}
```

`cli/agent/inbox.go` — add the filter flag:

```go
	inReplyTo := fs.Uint64("in-reply-to", 0, "only show messages replying to this seq (client-side filter)")
```

and skip non-matching records in both the `--stop-hook` and plain emit loops:

```go
			if *inReplyTo != 0 && m.InReplyTo != *inReplyTo {
				continue
			}
```

Update every `emitMessageLine` call site to pass `m.InReplyTo`: `cli/agent/inbox.go`, `cli/agent/wait.go`, `cli/agent/dispatch.go`, and `cli/agent/retained.go` if it uses that emitter. Let the compiler find them: `go build ./cli/...`.

- [ ] **Step 4: Run the tests and verify they pass**

Run: `go test ./cli/... -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cli/agent/
git commit -m "feat(cli): agent send --in-reply-to, with --topic optional on a reply

A linked reply is now less work than an unlinked one: the responder
passes one number and needs neither the reply topic nor a reply_topic
key parsed out of the request body. That is the only property that gives
the flag a chance of being used, since instructions about message form
are demonstrably not followed.

inbox/wait/dispatch emit in_reply_to on every record (0 when absent, for
the same reason the from block is unconditional), and inbox gains an
--in-reply-to filter."
```

---

### Task A5: Operator surfaces — CLI, TUI, WebUI, WASM

**Files:**
- Modify: `server/board_handler.go:46-107` (fill the row field)
- Modify: `cli/board.go:23-35` (`BoardMessage`), `:69-120` (`Client.BoardRead`), `:171-...` (package-level `BoardRead`)
- Modify: `cli/cmd_board.go:45-71` (`read` verb)
- Modify: `tui/board.go:181-200` (`ApplyMessages` / row rendering)
- Modify: `cmd/harness-webui-wasm/main.go:656-700` (`harnessBoardRead`)
- Modify: `webui/static/main.js:3429` (board panel rendering)
- Test: `server/board_handler_test.go`, `cli/board_e2e_test.go`, `tui/board_test.go`

**Interfaces:**
- Consumes: `protocol.BoardMessageRow.InReplyTo` (A1), `RetainedMessage.InReplyTo` (A1).
- Produces: `cli.BoardMessage.InReplyTo uint64`.

- [ ] **Step 1: Fill the surface matrix**

Write the eight-row matrix from Global Constraints into the commit message body, one verdict per row. Expected verdicts for this task: CLI implemented; TUI keybindings implemented; TUI cmdline **not applicable** (it has no board verbs — verify with `grep -n 'board' tui/cmdline.go`); TUI popups implemented (the board modal is the popup); WebUI buttons/forms implemented; WebUI command input — check `runCmd` in `webui/static/main.js` for a `board` verb and implement it if present, otherwise "not applicable"; WASM bridge implemented; shared cli/server implemented.

- [ ] **Step 2: Write the failing test**

Append to `server/board_handler_test.go` (reuse its existing server/board fixture):

```go
func TestHandleBoardRead_CarriesInReplyTo(t *testing.T) {
	// Publish a parent and a reply, read the topic back, and assert the row
	// carries the link. Uses the same fixture as the other tests in this file.
	// ... construct board + handler exactly as the neighbouring tests do ...
	// parent := board.Send("t", []byte("q"), rid, tid, "h", "", 0)
	// board.Send("t", []byte("a"), rid, tid, "h", "", parent)
	// rows := <read "t" through the handler>
	// if rows[1].InReplyTo != parent { t.Errorf(...) }
}
```

Fill the body from the neighbouring tests' fixture rather than inventing one — read `server/board_handler_test.go` first and copy its setup verbatim.

- [ ] **Step 3: Run it and verify it fails**

Run: `go test ./server/ -run BoardRead_CarriesInReplyTo -v`
Expected: FAIL.

- [ ] **Step 4: Implement, one surface at a time**

`server/board_handler.go` — in the loop that builds `BoardMessageRow`, add `row.InReplyTo = m.InReplyTo`.

`cli/board.go` — add to `BoardMessage`:

```go
	// InReplyTo is the seq of the message this one answers, 0 when it is not a
	// reply. See agentboard/agentboard.bgn SendRequest.in_reply_to.
	InReplyTo uint64
```

and populate it in **both** `Client.BoardRead` and the package-level `BoardRead` — the two paths build the struct independently.

`cli/cmd_board.go` — in the `read` verb, print `in_reply_to` when non-zero, and add a filter:

```go
		inReplyTo := fs.Uint64("in-reply-to", 0, "only show messages replying to this seq")
```

skipping rows where `*inReplyTo != 0 && m.InReplyTo != *inReplyTo`.

`tui/board.go` — render the parent seq in the message row; keep it out of the way when 0.

`cmd/harness-webui-wasm/main.go` — in `harnessBoardRead`'s per-message object add:

```go
			"inReplyTo": strconv.FormatUint(m.InReplyTo, 10),
```

as a **decimal string**, matching how `seq` is already returned there (board seq exceeds JS's 2^53 safe-integer range).

`webui/static/main.js` — show the parent seq in the board message list when it is not `"0"`. Compare as a string; do not `Number()` it.

- [ ] **Step 5: Run the checks**

Run: `make check && make wasm-check && make vet && go test ./... -count=1`
Expected: all green.

- [ ] **Step 6: Commit**

```bash
git add server/board_handler.go cli/ tui/ cmd/harness-webui-wasm/ webui/static/main.js
git commit -m "feat(cli,tui,webui): the board view shows what a message replies to

Surface matrix: CLI implemented; TUI keybindings implemented; TUI
cmdline not applicable (no board verbs); TUI popups implemented (the
board modal); WebUI forms implemented; WebUI command input <verdict>;
WASM bridge implemented; shared cli/server implemented.

in_reply_to crosses the WASM bridge as a decimal string, like seq: board
seq is boot-epoch seeded and exceeds JS's 2^53 safe integer."
```

---

### Task A6: Skill documentation

**Files:**
- Modify: `runner/agentskills/harness-cli/SKILL.md` (embed source of truth)
- Modify: `.claude/skills/harness-cli/SKILL.md` (synced copy)

**Interfaces:**
- Consumes: the CLI surface from A4.

- [ ] **Step 1: Locate the sections to edit**

Run: `grep -n 'reply_topic\|## Sending\|### Handshake flow\|### Naming inbound channels' runner/agentskills/harness-cli/SKILL.md`

- [ ] **Step 2: Edit the embed source**

In the sending section, add:

```markdown
### Replying — `--in-reply-to`

Reply with the parent message's `seq`, which every inbox record carries:

```bash
harness-cli agent send --in-reply-to <seq> "…"
```

`--topic` is not needed: the server routes the reply to the sender of the
parent message, resolved from its retained entry, not from anything in the
payload. The delivered reply carries `in_reply_to`, so the receiver can
correlate it without parsing your text.

The server validates the link at publish time. If the parent has fallen out
of its topic ring (64 entries) or its TTL (30 minutes), the send is rejected
with `unknown_in_reply_to`; drop the flag to send the same body as an
ordinary message.

Collect replies to one message with:

```bash
harness-cli agent inbox --json --in-reply-to <seq>
```
```

In "Naming inbound channels", after the `reply_topic` guidance, add:

```markdown
`reply_topic` remains for peers that do not set `in_reply_to` — `agent=bash`,
or any peer without skill injection. Prefer `--in-reply-to`: it survives the
reply landing on the wrong topic, which `reply_topic` does not.
```

In the conventions section, add the fallback with its limits:

```markdown
### Per-subject reply topics (fallback)

When a peer cannot be relied on to set `in_reply_to`, give each subject its
own reply topic (`rr.dec-019`) and bucket by the row's `topic`. Limits worth
knowing: `subscribe` is exact-match only (no wildcards), each topic retains
64 messages, a topic's ring is dropped 30 minutes after its last publish
whether or not anything read it, and past 1024 topics the board evicts the
least recently published one.
```

- [ ] **Step 3: Sync the copy and rebuild**

```bash
cp runner/agentskills/harness-cli/SKILL.md .claude/skills/harness-cli/SKILL.md
make build
harness-cli skill | grep -c 'in-reply-to'
```

Expected: the count is non-zero, proving the embedded copy was rebuilt.

- [ ] **Step 4: Commit**

```bash
git add runner/agentskills/ .claude/skills/
git commit -m "docs(skill): replying uses --in-reply-to, no reply topic needed

Keeps reply_topic and per-subject reply topics documented as the fallback
for peers that never set in_reply_to, with the ring/TTL/topic-count
limits that make those fallbacks lossy."
```

---

# Feature B — board subscribers

### Task B1: `Board.ListSubscribers`

**Files:**
- Modify: `agentboard/board.go` (add after `ListSubscriptions`, `:422-429`)
- Test: `agentboard/board_listings_test.go`

**Interfaces:**
- Produces: `type SubscriberRow struct { Task protocol.TaskID; Hostname, AgentProfile string; Patterns []string }` and `func (b *Board) ListSubscribers(topic string) []SubscriberRow`.

- [ ] **Step 1: Write the failing tests**

Append to `agentboard/board_listings_test.go`:

```go
func TestBoard_ListSubscribers_NoFilter(t *testing.T) {
	b := New(Config{RingN: 8, TopicTTL: time.Hour, MaxTopics: 8, MaxPayload: 1024})
	defer b.Close()

	var rid protocol.RunnerID
	rid.SetTransport([]byte("ws"))
	rid.SetIpAddr([]byte{1, 2, 3, 4})
	var registered protocol.TaskID
	registered.Id[0] = 1

	// RegisterTask only: subscribed to its own chat topic, never attached, so
	// hostname is still empty. This is a real state, not missing data.
	b.RegisterTask(rid, registered, [16]byte{9}, "codex")

	var attached protocol.TaskID
	attached.Id[0] = 2
	c := b.Attach(toAgentboardRunnerID(rid), toAgentboardTaskID(attached), "host-A", "claude")
	if err := b.Subscribe(c, "rr.dec-019"); err != nil {
		t.Fatal(err)
	}

	rows := b.ListSubscribers("")
	if len(rows) != 2 {
		t.Fatalf("len = %d, want 2", len(rows))
	}
	byTask := map[protocol.TaskID]SubscriberRow{}
	for _, r := range rows {
		byTask[r.Task] = r
	}
	if got := byTask[registered].Hostname; got != "" {
		t.Errorf("registered-only hostname = %q, want empty", got)
	}
	if got := byTask[registered].AgentProfile; got != "codex" {
		t.Errorf("registered-only profile = %q, want codex", got)
	}
	if got := byTask[attached].Hostname; got != "host-A" {
		t.Errorf("attached hostname = %q, want host-A", got)
	}
}

func TestBoard_ListSubscribers_FilterMatchesDelivery(t *testing.T) {
	b := New(Config{RingN: 8, TopicTTL: time.Hour, MaxTopics: 8, MaxPayload: 1024})
	defer b.Close()

	var rid protocol.RunnerID
	rid.SetTransport([]byte("ws"))
	rid.SetIpAddr([]byte{1, 2, 3, 4})
	var listener protocol.TaskID
	listener.Id[0] = 1
	var bystander protocol.TaskID
	bystander.Id[0] = 2

	c := b.Attach(toAgentboardRunnerID(rid), toAgentboardTaskID(listener), "host-A", "claude")
	if err := b.Subscribe(c, "rr.dec-019"); err != nil {
		t.Fatal(err)
	}
	if err := b.Subscribe(c, "other"); err != nil {
		t.Fatal(err)
	}
	b.Attach(toAgentboardRunnerID(rid), toAgentboardTaskID(bystander), "host-B", "claude")

	rows := b.ListSubscribers("rr.dec-019")
	if len(rows) != 1 {
		t.Fatalf("len = %d, want 1", len(rows))
	}
	if rows[0].Task != listener {
		t.Errorf("task = %x, want %x", rows[0].Task, listener)
	}
	// A filtered row still reports the task's FULL pattern set.
	if len(rows[0].Patterns) != 2 {
		t.Errorf("patterns = %v, want both of the task's subscriptions", rows[0].Patterns)
	}
}

func TestBoard_ListSubscribers_GoneAfterRevoke(t *testing.T) {
	b := New(Config{RingN: 8, TopicTTL: time.Hour, MaxTopics: 8, MaxPayload: 1024})
	defer b.Close()

	var rid protocol.RunnerID
	rid.SetTransport([]byte("ws"))
	rid.SetIpAddr([]byte{1, 2, 3, 4})
	var tid protocol.TaskID
	tid.Id[0] = 1

	b.RegisterTask(rid, tid, [16]byte{9}, "claude")
	if len(b.ListSubscribers("")) != 1 {
		t.Fatal("registered task missing from ListSubscribers")
	}
	b.Revoke(rid, tid)
	if got := len(b.ListSubscribers("")); got != 0 {
		t.Errorf("len after Revoke = %d, want 0", got)
	}
}
```

- [ ] **Step 2: Run the tests and verify they fail**

Run: `go test ./agentboard/ -run ListSubscribers -v`
Expected: FAIL — `b.ListSubscribers undefined`.

- [ ] **Step 3: Implement**

Add to `agentboard/board.go` after `ListSubscriptions`:

```go
// SubscriberRow is one task's subscription set together with the identity
// captured on its taskState. Hostname is empty for a task that has been
// registered (and so has its chat.<short-id> seeded) but has not yet run a
// harness-cli command — Attach is what fills it in. That is a real state, not
// missing data.
type SubscriberRow struct {
	Task         protocol.TaskID
	Hostname     string
	AgentProfile string
	Patterns     []string
}

// ListSubscribers returns one row per task known to the board. A non-empty
// topic narrows the result to the tasks a publish to that topic would reach;
// Patterns still holds each returned row's full set. Order is unspecified.
//
// The filter calls taskState.matches — the same predicate Board.Send uses to
// pick delivery targets — rather than reimplementing the comparison, so this
// view cannot claim a different set of recipients than delivery actually uses
// if matching ever gains wildcards.
//
// Deliberately absent: the attached-connection count. harness-cli is a
// short-lived process per subcommand, so a healthy agent has zero attached
// connections almost all of the time; reporting that number would read as
// "nobody is connected" and mislead exactly the diagnosis this exists for.
func (b *Board) ListSubscribers(topic string) []SubscriberRow {
	b.mu.Lock()
	states := make([]*taskState, 0, len(b.tasks))
	for _, ts := range b.tasks {
		states = append(states, ts)
	}
	b.mu.Unlock()

	out := make([]SubscriberRow, 0, len(states))
	for _, ts := range states {
		if topic != "" && !ts.matches(topic) {
			continue
		}
		_, tid, host, profile := ts.identity()
		out = append(out, SubscriberRow{
			Task:         tid,
			Hostname:     host,
			AgentProfile: profile,
			Patterns:     ts.snapshotPatterns(),
		})
	}
	return out
}
```

- [ ] **Step 4: Run the tests and verify they pass**

Run: `go test ./agentboard/ -run ListSubscribers -v`
Expected: PASS (3 tests).

- [ ] **Step 5: Commit**

```bash
git add agentboard/board.go agentboard/board_listings_test.go
git commit -m "feat(agentboard): Board.ListSubscribers reports who subscribes to what

The data was already in b.tasks and already walked by
anyTaskMatchesLocked, which returns a bool for Revoke and is exposed
nowhere. The topic filter calls taskState.matches, the same predicate
Send uses to pick targets, so the view cannot diverge from delivery.

Excludes the attached-connection count on purpose: harness-cli is a
short-lived process per subcommand, so a healthy agent shows zero
connections almost always."
```

---

### Task B2: Wire schema, server handler, capability gate

**Files:**
- Modify: `runner/protocol/message.bgn` (end of `TaskControlKind`; new formats near the other Board ones at `:656-700`; both union arms at `:889-891` and `:926-928`)
- Modify: `server/board_handler.go`
- Modify: `server/task_handler.go:481-495` (dispatch)
- Modify: `server/capabilities.go:23-36` (gate)
- Test: `server/board_handler_test.go`, `server/capabilities_test.go`

**Interfaces:**
- Consumes: `Board.ListSubscribers` (B1).
- Produces: `protocol.TaskControlKind_BoardSubscribers`, `protocol.BoardSubscribersRequest{RequestId, Topic}`, `protocol.BoardSubscriberRow{Task, Hostname, AgentProfile, Patterns}`, `protocol.SubscriptionPattern{Name}`, `protocol.BoardSubscribersResponse{RequestId, Rows}`.

- [ ] **Step 1: Edit `runner/protocol/message.bgn`**

Append to the very end of `enum TaskControlKind` (keeps existing ordinals stable — the convention `list_conns` records at `:255-257`):

```
    board_subscribers       # list each task's agentboard subscription set,
                            # optionally narrowed to the tasks a publish to one
                            # topic would reach. Requires info_global.
```

Add the formats next to `BoardReadResponse`:

```
format BoardSubscribersRequest:
    request_id :u32
    # Empty topic = no filter (every task known to the board).
    topic_len :u16
    topic :[topic_len]u8

format SubscriptionPattern:
    name_len :u16
    name :[name_len]u8

format BoardSubscriberRow:
    task :TaskID
    # hostname is empty for a task registered but not yet attached — see
    # agentboard.SubscriberRow.
    hostname_len :u8
    hostname :[hostname_len]u8
    agent_profile_len :u8
    agent_profile :[agent_profile_len]u8
    patterns_len :u16
    patterns :[patterns_len]SubscriptionPattern

format BoardSubscribersResponse:
    request_id :u32
    rows_len :u16
    rows :[rows_len]BoardSubscriberRow
```

Add both union arms:

```
        TaskControlKind.board_subscribers => board_subscribers :BoardSubscribersRequest
```

```
        TaskControlKind.board_subscribers => board_subscribers :BoardSubscribersResponse
```

- [ ] **Step 2: Regenerate and confirm it compiles**

```bash
make protoregen ARGS='runner/protocol/message.bgn'
go build ./...
```

- [ ] **Step 3: Write the failing tests**

Append to `server/capabilities_test.go`:

```go
func TestRequiredCap_BoardSubscribers(t *testing.T) {
	got, ok := requiredCap[protocol.TaskControlKind_BoardSubscribers]
	if !ok {
		t.Fatal("board_subscribers missing from requiredCap; an ungated kind is reachable by any helloed agent")
	}
	if got != protocol.Capability_InfoGlobal {
		t.Errorf("cap = %v, want InfoGlobal (matching board_topics / board_read)", got)
	}
}
```

Append to `server/board_handler_test.go` a test that publishes nothing, registers two tasks with distinct subscriptions through the fixture's board, calls `handleBoardSubscribers`, and asserts: no filter returns both rows; a filter returns only the matching task; the returned row's patterns hold the full set. Copy the fixture setup from the neighbouring tests in that file verbatim.

- [ ] **Step 4: Run the tests and verify they fail**

Run: `go test ./server/ -run 'BoardSubscribers' -v`
Expected: FAIL.

- [ ] **Step 5: Implement the handler, dispatch and gate**

`server/board_handler.go`:

```go
// handleBoardSubscribers reports each task's agentboard subscription set,
// optionally narrowed to the tasks a publish to one topic would reach. Gated on
// info_global like its board_topics / board_read siblings: it is a sweep across
// every task on the board.
//
// Unlike list_conns there is no own-subtree narrowing for confined callers.
// board_topics does not narrow either, and a partial subscriber list would
// answer "is anyone listening" wrongly rather than incompletely.
func (h *TaskHandler) handleBoardSubscribers(conn ConnHandle, requestID uint32, topic string) {
	out := protocol.BoardSubscribersResponse{RequestId: requestID}
	if h.Board != nil {
		for _, r := range h.Board.ListSubscribers(topic) {
			row := protocol.BoardSubscriberRow{Task: <protocol.TaskID from r.Task>}
			row.SetHostname([]byte(r.Hostname))
			row.SetAgentProfile([]byte(r.AgentProfile))
			for _, p := range r.Patterns {
				var sp protocol.SubscriptionPattern
				sp.SetName([]byte(p))
				row.Patterns = append(row.Patterns, sp)
			}
			row.PatternsLen = uint16(len(row.Patterns))
			out.Rows = append(out.Rows, row)
		}
	}
	out.RowsLen = uint16(len(out.Rows))
	resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_BoardSubscribers, RequestId: requestID}
	resp.SetBoardSubscribers(out)
	<send resp exactly as handleBoardTopics does — copy its send path>
}
```

Read `handleBoardTopics` (`server/board_handler.go:13-45`) first and mirror its field-setting and response-send idiom exactly; the generated setters' names and the `*Len` bookkeeping follow that file's existing usage.

`server/task_handler.go` — add beside the other board kinds:

```go
	case protocol.TaskControlKind_BoardSubscribers:
		h.handleBoardSubscribers(conn, req.RequestId, string(req.BoardSubscribers().Topic))
```

`server/capabilities.go` — add to `requiredCap`:

```go
	protocol.TaskControlKind_BoardSubscribers: protocol.Capability_InfoGlobal,
```

- [ ] **Step 6: Run the tests and verify they pass**

Run: `go test ./server/ -count=1`
Expected: PASS (re-run once if the pre-existing SessionMux flake trips).

- [ ] **Step 7: Commit**

```bash
git add runner/protocol/ server/
git commit -m "feat(server): board_subscribers RPC behind info_global

Reports task id, hostname, agent profile and pattern set per task,
optionally narrowed to the tasks a publish to one topic would reach.
Gated with board_topics / board_read; no own-subtree narrowing, because
a partial subscriber list answers 'is anyone listening' wrongly rather
than incompletely."
```

---

### Task B3: CLI client and `board subscribers` verb

**Files:**
- Modify: `cli/board.go` (add beside `BoardRead`, `:69` and `:171`)
- Modify: `cli/cmd_board.go:33-44` (verb switch)
- Test: `cli/board_e2e_test.go`

**Interfaces:**
- Consumes: `protocol.BoardSubscribers*` (B2).
- Produces: `type BoardSubscriberRow struct { TaskHex, Hostname, AgentProfile string; Patterns []string }`, `func (c *Client) BoardSubscribers(ctx context.Context, topic string) ([]BoardSubscriberRow, error)`, `func BoardSubscribers(ctx context.Context, peerCID objproto.ConnectionID, topic string) ([]BoardSubscriberRow, error)`.

- [ ] **Step 1: Write the failing test**

Append to `cli/board_e2e_test.go` a test using the existing `operatorE2E` fixture: seed the board via `e.Board()` with two attached tasks holding different subscriptions, call `BoardSubscribers` with and without a topic, and assert the rows and their sort order (ascending `TaskHex`). Read the file's existing tests first and copy their fixture construction.

- [ ] **Step 2: Run it and verify it fails**

Run: `go test ./cli/ -run BoardSubscribers -v`
Expected: FAIL — undefined.

- [ ] **Step 3: Implement**

`cli/board.go` — mirror `Client.BoardRead` (`:69`) and the package-level `BoardRead` (`:171`); both build the result struct independently, so implement both. Sort before returning:

```go
	sort.Slice(out, func(i, j int) bool { return out[i].TaskHex < out[j].TaskHex })
```

with the reason recorded once:

```go
// Rows are sorted by task id here rather than on the board: Board.ListSubscribers
// iterates a map and declares its order unspecified, and stable output is a
// property the CLI and its tests want, not one the board should have to promise.
```

`cli/cmd_board.go` — add the verb:

```go
	case "subscribers":
		topic := ""
		if len(rest) > 0 {
			topic = rest[0]
		}
		rows, err := BoardSubscribers(ctx, cid, topic)
		if err != nil {
			return err
		}
		for _, r := range rows {
			fmt.Fprintf(out, "%s\t%s\t%s\t%s\n", r.TaskHex, r.Hostname, boardAgentOrDash(r.AgentProfile), strings.Join(r.Patterns, ","))
		}
		return nil
```

Match the surrounding verbs' output idiom — read `topics` and `read` in the same switch and use `boardAgentOrDash` (`cli/cmd_board.go:22-31`) for the profile column as they do.

- [ ] **Step 4: Run the tests and verify they pass**

Run: `go test ./cli/... -count=1`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add cli/
git commit -m "feat(cli): harness-cli board subscribers [topic]

Both the long-lived-client and one-shot dial paths, since they build the
result independently. Sorted by task id in the CLI, not the board:
ListSubscribers iterates a map and declares its order unspecified."
```

---

### Task B4: TUI, WebUI and the WASM bridge

**Files:**
- Modify: `tui/board.go:82-98` (modes), `:132-210` (modal state)
- Modify: `tui/keys.go:332` (modal help line)
- Modify: `cmd/harness-webui-wasm/main.go:105-115` (export table), and a new `harnessBoardSubscribers` beside `harnessBoardRead` at `:656`
- Modify: `webui/static/main.js:3429` (board panel)
- Test: `tui/board_test.go`

**Interfaces:**
- Consumes: `cli.BoardSubscriberRow`, `cli.Client.BoardSubscribers` (B3).

- [ ] **Step 1: Fill the surface matrix**

Same eight rows as Task A5 Step 1; record each verdict in the commit body.

- [ ] **Step 2: Write the failing TUI test**

Append to `tui/board_test.go` a test that constructs a `BoardModal`, applies subscriber rows, and asserts the mode switched to the subscribers mode and the rows render. Copy the fixture idiom from the existing `ApplyMessages` tests in that file.

- [ ] **Step 3: Run it and verify it fails**

Run: `go test ./tui/ -run Subscribers -v`
Expected: FAIL.

- [ ] **Step 4: Implement**

`tui/board.go` — add `boardSubscribers` to the `boardMode` iota **after** `boardMessages`, a `BoardSubscribersMsg`, a `DoBoardSubscribers(c *cli.Client, topic string) tea.Cmd` mirroring `DoBoardRead` (`:59-68`), an `ApplySubscribers` mirroring `ApplyMessages` (`:181-199`), and Esc handling that pops back to `boardTopics` like `PopToTopics` (`:151-155`).

Bind a key in the board modal for "subscribers of the selected topic" and add it to the modal help line in `tui/keys.go:332` — that line is the only place the board modal's keys are documented, so a key added without it is undiscoverable.

`cmd/harness-webui-wasm/main.go` — register `"boardSubscribers": js.FuncOf(harnessBoardSubscribers)` in the export table and implement it beside `harnessBoardRead`, mirroring its promise construction. No u64 crosses this call, so no decimal-string handling is needed here.

`webui/static/main.js` — add a subscribers section to the board panel calling `window.harness.boardSubscribers(topic)`, rendering task id, hostname, agent profile and patterns. Check `runCmd` for a `board` verb; if one exists, add the `subscribers` sub-verb there too and update its help text.

- [ ] **Step 5: Run the checks**

Run: `make check && make wasm-check && make vet && go test ./... -count=1`
Expected: all green.

- [ ] **Step 6: Commit**

```bash
git add tui/ cmd/harness-webui-wasm/ webui/static/main.js
git commit -m "feat(tui,webui): board view shows a topic's subscribers

Surface matrix: CLI implemented (B3); TUI keybindings implemented; TUI
cmdline not applicable (no board verbs); TUI popups implemented (the
board modal); WebUI forms implemented; WebUI command input <verdict>;
WASM bridge implemented; shared cli/server implemented (B1/B2)."
```

---

### Task B5: Skill documentation, wire-skew check, end-to-end verification

**Files:**
- Modify: `runner/agentskills/harness-cli/SKILL.md`, `.claude/skills/harness-cli/SKILL.md`

- [ ] **Step 1: Document the capability and the command**

In the skill's capability section, add `board subscribers` to what `info_global` unlocks. In the diagnosis guidance, add:

```markdown
If a message you published never reached anyone, `harness-cli board
subscribers <topic>` (needs `info_global`) lists the tasks that would have
received it. An empty result means the message is retained on the board and
nobody is listening — distinct from "the peer has not replied yet".
```

Sync the copy and rebuild:

```bash
cp runner/agentskills/harness-cli/SKILL.md .claude/skills/harness-cli/SKILL.md
make build
```

- [ ] **Step 2: Run the wire-skew check**

```bash
scripts/wire-skew-check.sh
```

Expected: it exercises NEW-runner × OLD-server and asserts the failure is recoverable. Exit 2 means a setup error (e.g. missing `webui/static/main.wasm`), not a pass — investigate rather than proceeding.

- [ ] **Step 3: Full local verification**

```bash
make check && make wasm-check && make vet && make test
```

- [ ] **Step 4: Drive both features against a local dummy server and runner**

Start a local `harness-server` and a bash-agent runner from this checkout's own `bin/`, then, as a real agent:

1. publish a request, capture its `seq` from the `agent send` JSON output;
2. reply with `agent send --in-reply-to <seq>` and **no** `--topic`; confirm it lands on the requester's `chat.<short-id>`;
3. `agent inbox --json --in-reply-to <seq>` returns exactly that reply;
4. `agent send --in-reply-to 1` (a seq the board never issued) is rejected with the `unknown_in_reply_to` message;
5. `harness-cli board subscribers` lists both tasks; `board subscribers chat.<short-id>` lists only the owner;
6. `board read <topic>` shows the parent seq on the reply row.

Pipe every id from command output — never retype a hex id or a 19-digit seq by hand.

- [ ] **Step 5: Commit**

```bash
git add runner/agentskills/ .claude/skills/
git commit -m "docs(skill): board subscribers as the 'nobody is listening' check

Distinguishes 'retained but unsubscribed' from 'peer has not replied',
which was previously indistinguishable without guessing the topic name."
```

---

## Landing

Follow the repo landing policy (Mode A, local trunk authoritative): merge into local `main`, fast-forward push to `origin/main`, never cherry-pick. Then `make build` in the main checkout.

Deploy **server first** — it runs on a different host, and a runner-side binary refresh alone leaves the server unable to decode the new `SendRequest`, which the handler drops with a warning. Runner-side `harness-cli` is a short-lived process per subcommand, so refreshing `bin/` is sufficient; running agent sessions do not need restarting.

## Self-Review

**Spec coverage — reply linkage:** schema §1 → A1; server validation and derivation §2 → A2 (LookupSeq) + A3 (handler); agent CLI §3 → A4; operator surfaces §4 → A5; skill docs §5 → A6; error handling → A4 Step 3; rollout → Landing + B5 Step 2; testing → tests in A1–A5 plus B5 Step 4.

**Spec coverage — subscribers:** board API §1 → B1; wire schema §2 → B2; server §3 → B2; CLI §4 → B3; TUI/WebUI §5 → B4; testing → B1–B4 plus B5 Step 4.

**Known gaps, stated rather than hidden:**
- `server/board_handler_test.go`, `cli/board_e2e_test.go` and `tui/board_test.go` test bodies say "copy the neighbouring fixture" instead of inlining it. That is deliberate: those fixtures are 30–60 lines of server/board construction that would go stale in this document, and the instruction names the exact file to copy from. Every *assertion* is spelled out.
- The `handleBoardSubscribers` body has two `<...>` placeholders (the `protocol.TaskID` conversion and the response-send path) because the generated setter names come out of `make protoregen` in B2 Step 2 and the send idiom is one line copied from `handleBoardTopics`. The step names both sources explicitly.
- The WebUI command input verdict is left as `<verdict>` in two commit messages because whether `runCmd` has a `board` verb is a fact to check, not to assume — the step says to check it.
