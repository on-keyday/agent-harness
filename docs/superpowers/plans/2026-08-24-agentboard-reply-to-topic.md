# Sender-declared reply destination (`reply_to_topic`) — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a sender declare, on the wire, where a reply to its message is routed, so a script driving a peer on an agent's behalf can keep the answer out of that agent's inbox.

**Architecture:** One field on `SendRequest`, frozen onto the retained message the way `FromAgentProfile` already is, and read by `resolveReplyTarget` as a new middle arm between an explicit `--topic` on the reply and today's fallback to the parent sender's `chat.<short-id>`. The replier is unchanged: `--in-reply-to <seq>` alone reaches the declared topic, because the routing already resolves the parent to decide where a reply goes.

**Tech Stack:** Go, `.bgn` schemas compiled by `make protoregen` (brgen).

**Spec:** `docs/superpowers/specs/2026-08-24-agentboard-reply-to-topic-design.md`

## Global Constraints

- **Work in the checkout that holds this plan**, `.harness-worktrees/70fbad4a6eb6f1e992be8a669f1bcefd/`. A bare absolute path under `/home/kforfk/workspace/remote-agent-harness/<rel>` resolves to the PARENT checkout, which would split edits from commits (Pitfall 8). Verify with `git rev-parse --abbrev-ref HEAD` before writing code.
- **Read `.claude/skills/implementation-pitfalls/SKILL.md` in full before writing code.**
- **Verify with make targets**: `make check`, `make wasm-check`, `make vet`, `make test`. `go build ./...` misses the integration-tagged callers `make vet` catches.
- **Build hygiene**: never bare `go build ./cmd/<x>/` — it drops an executable into the worktree. Use `go build ./...` or `-o /dev/null`.
- **The whole `.bgn` change lands in Task 1.** No later task adds a wire field.
- **No compat shims.** Single-user dogfood; a removed or changed flag is changed.
- **Commit after every task**, messages ending with:
  `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>`
- **Priority is fixed** and every task must preserve it: explicit `--topic` on the reply > the parent's `reply_to_topic` > `SelfTopic(parent.FromTask)`.

---

## File Structure

| File | Responsibility | Task |
| --- | --- | --- |
| `agentboard/agentboard.bgn` | the wire field | 1 |
| `agentboard/topic.go` | `RetainedMessage.ReplyToTopic`, frozen at append | 2 |
| `agentboard/board.go` | `WithReplyTo` SendOption + `sendConfig` | 2 |
| `server/agent_handler.go` | `resolveReplyTarget` middle arm; `agentHandleSend` passes the option | 3 |
| `cli/agent/send.go` | `--reply-to` | 4 |
| `cli/agent/dispatch.go` | `--reply-to`, declared AND waited on | 5 |
| `cmd/harness-cli/main.go` | usage lines | 4, 5 |
| skills (`runner/agentskills/harness-cli` + 2 mirrors) | replace the payload convention | 6 |

---

### Task 1: The wire field

**Files:**
- Modify: `agentboard/agentboard.bgn` (`format SendRequest`, ends at the `reserved :u7` line)
- Regenerate: `agentboard/agentboard.go`

**Interfaces:**
- Produces: `agentboard.SendRequest.ReplyToTopicLen uint16`, `.ReplyToTopic []uint8`, and `(*SendRequest).SetReplyToTopic([]uint8) bool` — the same shape the generator already produces for `topic`.

- [ ] **Step 1: Add the field at the END of SendRequest**

In `agentboard/agentboard.bgn`, after the existing `reserved :u7` line that closes `format SendRequest`:

```
    # reply_to_topic is where a reply to THIS message is routed. Empty = the
    # sender's own chat.<short-id>, which is what every message did before this
    # field existed and what one still does when it says nothing.
    #
    # It is the SENDER declaring a destination, not the replier choosing one: a
    # peer answers with --in-reply-to alone and never has to know the topic
    # exists. The alternative was a `reply_topic` field in the PAYLOAD, which
    # only works on a peer that reads the convention — and the convention is
    # documented in a skill file such a peer has by definition not been given.
    #
    # Appended, not inserted: this is a REQUEST, so the skew that matters is a
    # new client against an old server. The old server stops before these bytes
    # and routes replies the way it always did, which is a degrade rather than
    # a decode failure.
    reply_to_topic_len :u16
    reply_to_topic :[reply_to_topic_len]u8
```

- [ ] **Step 2: Regenerate**

Run:

```bash
make protoregen ARGS='agentboard/agentboard.bgn'
```

Expected: `agentboard/agentboard.go` is rewritten.

- [ ] **Step 3: Confirm the generated names**

Run:

```bash
grep -n 'ReplyToTopic' agentboard/agentboard.go | head
```

Expected: a `ReplyToTopicLen uint16` and `ReplyToTopic []uint8` on `SendRequest`, plus `SetReplyToTopic`. If the generated setter collides with an interface method the type also needs, rename the field and regenerate — this project has hit `.bgn` arm/field name collisions before.

- [ ] **Step 4: Compile**

Run: `go build ./...`
Expected: builds. Nothing reads the field yet.

- [ ] **Step 5: Wire-skew check**

Run: `scripts/wire-skew-check.sh`
Expected: PASS — the skew degrades recoverably.

- [ ] **Step 6: Commit**

```bash
git add agentboard/agentboard.bgn agentboard/agentboard.go
git commit -m "feat(schema): SendRequest.reply_to_topic — the sender declares where a reply goes

Appended at the END of the format: SendRequest is a REQUEST, so the skew is a
new client against an old server, which stops before the new bytes and keeps
today's routing. That is a degrade, not a decode failure.

Nothing reads it yet.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 2: The board records it per message

**Files:**
- Modify: `agentboard/topic.go` (`RetainedMessage` struct; `(*topic).append` at `:70-91`)
- Modify: `agentboard/board.go` (`sendConfig` at `:525-527`, next to `NoRetireOnReply` at `:534`)
- Test: `agentboard/reply_to_test.go` (create)

**Interfaces:**
- Consumes: nothing from Task 1 (this layer is wire-agnostic).
- Produces: `agentboard.RetainedMessage.ReplyToTopic string`, and
  `func WithReplyTo(topic string) SendOption`.

- [ ] **Step 1: Write the failing test**

Create `agentboard/reply_to_test.go`:

```go
package agentboard

import (
	"testing"
	"time"
)

func newReplyToBoard(t *testing.T) *Board {
	t.Helper()
	b := New(Config{RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024})
	t.Cleanup(b.Close)
	return b
}

// The destination is frozen onto the message, not looked up later: the ring
// outlives the connection that published, and a reply can arrive long after.
func TestBoard_SendRecordsReplyToTopic(t *testing.T) {
	b := newReplyToBoard(t)
	seq, _, err := b.Send("t.ask", []byte("q"), testRid, testTid, "h", "", 0, WithReplyTo("rr.task-1"))
	if err != nil {
		t.Fatal(err)
	}
	m, ok := b.Retained(seq)
	if !ok {
		t.Fatalf("seq %d not retained", seq)
	}
	if m.ReplyToTopic != "rr.task-1" {
		t.Errorf("ReplyToTopic = %q, want rr.task-1", m.ReplyToTopic)
	}
}

// Saying nothing must stay the default, and must be distinguishable from
// saying "": both are empty, which is why the ABSENT case is asserted rather
// than assumed.
func TestBoard_SendWithoutReplyToLeavesItEmpty(t *testing.T) {
	b := newReplyToBoard(t)
	seq, _, err := b.Send("t.ask", []byte("q"), testRid, testTid, "h", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	m, _ := b.Retained(seq)
	if m.ReplyToTopic != "" {
		t.Errorf("ReplyToTopic = %q, want empty", m.ReplyToTopic)
	}
}

// One message's destination must not leak onto the next: append writes it per
// entry, and a shared sendConfig would be a silent cross-message bug.
func TestBoard_ReplyToIsPerMessage(t *testing.T) {
	b := newReplyToBoard(t)
	withSeq, _, _ := b.Send("t.ask", []byte("a"), testRid, testTid, "h", "", 0, WithReplyTo("rr.one"))
	plainSeq, _, _ := b.Send("t.ask", []byte("b"), testRid, testTid, "h", "", 0)
	other, _, _ := b.Send("t.ask", []byte("c"), testRid, testTid, "h", "", 0, WithReplyTo("rr.two"))

	for _, tc := range []struct {
		seq  uint64
		want string
	}{{withSeq, "rr.one"}, {plainSeq, ""}, {other, "rr.two"}} {
		m, ok := b.Retained(tc.seq)
		if !ok {
			t.Fatalf("seq %d not retained", tc.seq)
		}
		if m.ReplyToTopic != tc.want {
			t.Errorf("seq %d: ReplyToTopic = %q, want %q", tc.seq, m.ReplyToTopic, tc.want)
		}
	}
}

// WithReplyTo composes with the other option rather than replacing it — they
// set different fields of the same sendConfig.
func TestBoard_ReplyToComposesWithNoRetireOnReply(t *testing.T) {
	b := newReplyToBoard(t)
	seq, _, _ := b.Send("t.ask", []byte("q"), testRid, testTid, "h", "", 0,
		WithReplyTo("rr.task-1"), NoRetireOnReply())
	m, _ := b.Retained(seq)
	if m.ReplyToTopic != "rr.task-1" {
		t.Errorf("ReplyToTopic = %q, want rr.task-1", m.ReplyToTopic)
	}
	if !m.NoRetireOnReply {
		t.Error("NoRetireOnReply was lost when combined with WithReplyTo")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./agentboard/ -run 'TestBoard_SendRecordsReplyToTopic|TestBoard_SendWithoutReplyTo|TestBoard_ReplyToIsPerMessage|TestBoard_ReplyToComposes' -v`
Expected: FAIL — `undefined: WithReplyTo`.

- [ ] **Step 3: Add the field to RetainedMessage**

In `agentboard/topic.go`, in the `RetainedMessage` struct, after `FromAgentProfile`:

```go
	// ReplyToTopic is where the SENDER asked replies to this message to go.
	// Empty = the sender's own chat.<short-id>, which is what resolveReplyTarget
	// falls back to. Frozen here with the message for the same reason
	// FromAgentProfile is: the ring outlives the connection that produced the
	// entry, and a reply may arrive long after that connection is gone.
	ReplyToTopic string
```

- [ ] **Step 4: Carry it through append**

In `(*topic).append`, add to the `RetainedMessage{...}` literal, beside `NoRetireOnReply`:

```go
		ReplyToTopic:     cfg.replyToTopic,
```

`append` already takes `cfg sendConfig`, so no signature changes.

- [ ] **Step 5: Add the option**

In `agentboard/board.go`, extend `sendConfig`:

```go
type sendConfig struct {
	noRetireOnReply bool
	replyToTopic    string
}
```

and add, next to `NoRetireOnReply`:

```go
// WithReplyTo records where replies to this message should be routed, for
// resolveReplyTarget to read off the retained entry. Empty (the zero value, and
// what a caller passing no options gets) means the sender's own
// chat.<short-id> — the behaviour every message had before this existed.
//
// An option rather than an eighth positional parameter, for the reason stated
// on SendOption: Send already takes seven and is called from ~50 places that
// have no opinion about this.
func WithReplyTo(topic string) SendOption {
	return func(c *sendConfig) { c.replyToTopic = topic }
}
```

- [ ] **Step 6: Run tests**

Run: `go test ./agentboard/ -count=1`
Expected: PASS, including the four new tests.

- [ ] **Step 7: Commit**

```bash
git add agentboard/topic.go agentboard/board.go agentboard/reply_to_test.go
git commit -m "feat(agentboard): record a message's declared reply destination

RetainedMessage.ReplyToTopic is frozen at append for the same reason
FromAgentProfile is: the ring outlives the connection that published, and a
reply can arrive long after. WithReplyTo is a SendOption beside
NoRetireOnReply, because Send already takes seven positional parameters and
says so.

Nothing routes on it yet.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 3: The server routes on it

**Files:**
- Modify: `server/agent_handler.go` (`resolveReplyTarget` at `:156-168`; `agentHandleSend`'s option list at `:245-249`)
- Test: `server/reply_target_test.go` (create)

**Interfaces:**
- Consumes: Task 1's `SendRequest.ReplyToTopic`, Task 2's `RetainedMessage.ReplyToTopic` and `WithReplyTo`.
- Produces: `resolveReplyTarget` keeps its signature `(b *agentboard.Board, topic string, inReplyTo uint64) (string, bool)`.

- [ ] **Step 1: Write the failing test**

Create `server/reply_target_test.go`:

```go
package server

import (
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// resolveReplyTarget has three arms and the ORDER is the contract: an explicit
// --topic on the reply wins over the asker's declaration, which wins over the
// historical fallback. Each arm is asserted separately, because a wrong order
// still satisfies any single one of them.
func TestResolveReplyTarget_Priority(t *testing.T) {
	b := agentboard.New(agentboard.Config{
		RingN: 64, TopicTTL: time.Hour, MaxTopics: 16, MaxPayload: 1024,
	})
	defer b.Close()

	var rid protocol.RunnerID
	rid.SetTransport([]byte("ws"))
	rid.SetIpAddr([]byte{1, 2, 3, 4})
	var tid protocol.TaskID
	tid.Id[0] = 0x5A

	declared, _, err := b.Send("t.ask", []byte("q"), rid, tid, "h", "", 0,
		agentboard.WithReplyTo("rr.declared"))
	if err != nil {
		t.Fatal(err)
	}
	plain, _, err := b.Send("t.ask", []byte("q"), rid, tid, "h", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	self := agentboard.SelfTopic(tid)

	for _, tc := range []struct {
		name      string
		topic     string
		inReplyTo uint64
		want      string
		wantOK    bool
	}{
		{"not a reply passes the topic through", "t.somewhere", 0, "t.somewhere", true},
		{"explicit topic beats the declaration", "t.override", declared, "t.override", true},
		{"declaration beats the self fallback", "", declared, "rr.declared", true},
		{"no declaration falls back to self", "", plain, self, true},
		{"unknown parent is refused", "", 99999, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := resolveReplyTarget(b, tc.topic, tc.inReplyTo)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("resolveReplyTarget(%q, %d) = %q,%v want %q,%v",
					tc.topic, tc.inReplyTo, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./server/ -run TestResolveReplyTarget_Priority -v`
Expected: FAIL on "declaration beats the self fallback" — it returns the self topic.

- [ ] **Step 3: Add the middle arm**

Replace `resolveReplyTarget` in `server/agent_handler.go`:

```go
// resolveReplyTarget maps a send request's (topic, in_reply_to) to the topic
// actually published to. inReplyTo == 0 passes the requested topic through. A
// non-zero inReplyTo must resolve to a message still on the board.
//
// Three arms, in this order:
//
//   - an explicit --topic on the REPLY wins. The replier is answering and may
//     know something the asker did not.
//   - the parent's reply_to_topic, which the ASKER declared when it published.
//     This is what lets a caller keep an answer out of its own inbox without
//     the replier knowing anything: a peer sends --in-reply-to alone and the
//     destination comes off the parent, server-side.
//   - the parent sender's own chat.<short-id>, which is what every reply did
//     before reply_to_topic existed and what one still does when the asker
//     declared nothing.
//
// The parent is read whole (Retained) rather than as (topic, sender)
// (LookupSeq), because the destination now lives on the message. Both are full
// ring scans, so this costs nothing extra.
func resolveReplyTarget(b *agentboard.Board, topic string, inReplyTo uint64) (string, bool) {
	if inReplyTo == 0 {
		return topic, true
	}
	parent, ok := b.Retained(inReplyTo)
	if !ok {
		return "", false
	}
	if topic != "" {
		return topic, true
	}
	if parent.ReplyToTopic != "" {
		return parent.ReplyToTopic, true
	}
	return agentboard.SelfTopic(parent.FromTask), true
}
```

- [ ] **Step 4: Pass the field through on publish**

In `agentHandleSend`, where `sendOpts` is built:

```go
		var sendOpts []agentboard.SendOption
		if r.NoRetireOnReply() {
			sendOpts = append(sendOpts, agentboard.NoRetireOnReply())
		}
		if len(r.ReplyToTopic) > 0 {
			sendOpts = append(sendOpts, agentboard.WithReplyTo(string(r.ReplyToTopic)))
		}
```

- [ ] **Step 5: Run tests**

Run: `go test ./server/ ./agentboard/ -count=1`
Expected: PASS. `server`'s `TestOpenInteractive*` SessionMux test is known-flaky at roughly one run in four and predates this change — re-run before investigating it.

- [ ] **Step 6: Commit**

```bash
git add server/agent_handler.go server/reply_target_test.go
git commit -m "feat(server): route a reply to the destination its parent declared

resolveReplyTarget gains a middle arm: an explicit --topic on the reply still
wins, then the parent's reply_to_topic, then the historical fallback to the
parent sender's chat.<short-id>. The order is the contract and the test asserts
each arm separately, since a wrong order still satisfies any one of them.

The parent is now read whole rather than as (topic, sender): the destination
lives on the message. Same full ring scan either way.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 4: `agent send --reply-to`

**Files:**
- Modify: `cli/agent/send.go` (flag set; the `SendRequest` build)
- Modify: `cmd/harness-cli/main.go` (the `send` usage lines in `agentUsage`)
- Test: `cli/agent/reply_to_e2e_test.go` (create)

**Interfaces:**
- Consumes: Tasks 1–3.
- Produces: `agent.Send` keeps its signature; the flag is `--reply-to`.

- [ ] **Step 1: Write the failing test**

Create `cli/agent/reply_to_e2e_test.go`. It uses the helpers already in
`cli/agent/agent_e2e_test.go` (`freePortE2E`, `startServerE2E`, `mkTidE2E`,
`mkRidE2E`, `setAgentEnv`):

```go
package agent_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli/agent"
)

// The property the whole change exists for: the asker declares a destination,
// the peer answers with --in-reply-to ALONE, and the answer lands there —
// never on the asker's own chat.<short-id>. The negative half is asserted too,
// because "it arrived at R" and "it did not also arrive in my inbox" are
// different claims and only the second one is the goal.
func TestAgentCLI_E2E_ReplyToRoutesAwayFromTheAskersInbox(t *testing.T) {
	addr := freePortE2E(t)
	board, _ := startServerE2E(t, addr)

	const (
		ridStrA = "ws:1.2.3.4:9501-51" // asker
		ridStrB = "ws:5.6.7.8:9502-52" // peer
	)
	var ticketA, ticketB [16]byte
	ticketA[0] = 0xC1
	ticketB[0] = 0xC2
	tidA := mkTidE2E(0x51)
	tidB := mkTidE2E(0x52)
	ridA := mkRidE2E([4]byte{1, 2, 3, 4}, 9501, 51)
	ridB := mkRidE2E([4]byte{5, 6, 7, 8}, 9502, 52)
	board.Registry().Register(ridA, tidA, ticketA)
	board.Registry().Register(ridB, tidB, ticketB)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	selfA := agent.SelfTopic(tidA)
	selfB := agent.SelfTopic(tidB)
	const replyTo = "rr.task-1"

	// The asker subscribes to its declared destination and publishes, naming it.
	restoreA := setAgentEnv(addr, ridStrA, tidA, ticketA)
	var subOut, sendOut bytes.Buffer
	if err := agent.Subscribe(ctx, []string{"--topic", replyTo}, &subOut); err != nil {
		restoreA()
		t.Fatalf("Subscribe: %v", err)
	}
	if err := agent.Send(ctx,
		[]string{"--topic", selfB, "--reply-to", replyTo, "--data", `{"q":"question"}`},
		nil, &sendOut); err != nil {
		restoreA()
		t.Fatalf("Send: %v", err)
	}
	restoreA()

	// The peer replies with --in-reply-to ONLY. It never names replyTo.
	parent := lastSeqOnTopic(t, board, selfB)
	restoreB := setAgentEnv(addr, ridStrB, tidB, ticketB)
	var replyOut bytes.Buffer
	if err := agent.Send(ctx,
		[]string{"--in-reply-to", itoa(parent), "--data", `{"a":"answer-here"}`},
		nil, &replyOut); err != nil {
		restoreB()
		t.Fatalf("peer reply: %v", err)
	}
	restoreB()

	if got := topicPayloads(t, board, replyTo); !strings.Contains(got, "answer-here") {
		t.Errorf("declared topic %s holds %q, want the answer", replyTo, got)
	}
	if got := topicPayloads(t, board, selfA); strings.Contains(got, "answer-here") {
		t.Errorf("the asker's own topic holds %q — the answer was supposed to go elsewhere", got)
	}
}

// Saying nothing keeps today's behaviour, which is the default arm and the one
// every existing caller relies on.
func TestAgentCLI_E2E_WithoutReplyToTheAnswerComesHome(t *testing.T) {
	addr := freePortE2E(t)
	board, _ := startServerE2E(t, addr)

	const (
		ridStrA = "ws:1.2.3.4:9503-53"
		ridStrB = "ws:5.6.7.8:9504-54"
	)
	var ticketA, ticketB [16]byte
	ticketA[0] = 0xC3
	ticketB[0] = 0xC4
	tidA := mkTidE2E(0x53)
	tidB := mkTidE2E(0x54)
	ridA := mkRidE2E([4]byte{1, 2, 3, 4}, 9503, 53)
	ridB := mkRidE2E([4]byte{5, 6, 7, 8}, 9504, 54)
	board.Registry().Register(ridA, tidA, ticketA)
	board.Registry().Register(ridB, tidB, ticketB)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	selfA := agent.SelfTopic(tidA)
	selfB := agent.SelfTopic(tidB)

	restoreA := setAgentEnv(addr, ridStrA, tidA, ticketA)
	var sendOut bytes.Buffer
	if err := agent.Send(ctx,
		[]string{"--topic", selfB, "--data", `{"q":"question"}`}, nil, &sendOut); err != nil {
		restoreA()
		t.Fatalf("Send: %v", err)
	}
	restoreA()

	parent := lastSeqOnTopic(t, board, selfB)
	restoreB := setAgentEnv(addr, ridStrB, tidB, ticketB)
	var replyOut bytes.Buffer
	if err := agent.Send(ctx,
		[]string{"--in-reply-to", itoa(parent), "--data", `{"a":"came-home"}`},
		nil, &replyOut); err != nil {
		restoreB()
		t.Fatalf("peer reply: %v", err)
	}
	restoreB()

	if got := topicPayloads(t, board, selfA); !strings.Contains(got, "came-home") {
		t.Errorf("asker's own topic holds %q, want the answer (the default arm)", got)
	}
}
```

Add these THREE helpers to the same file. `itoa` does not exist anywhere in
`cli/agent` — an earlier draft of this plan said it did, which would have been
a compile error on first run; verified with `grep -rn itoa cli/agent/`:

```go
// itoa renders a board seq for a --in-reply-to argument. strconv.FormatUint,
// not fmt.Sprint: a seq exceeds what %d on an untyped constant would keep.
func itoa(n uint64) string { return strconv.FormatUint(n, 10) }

// lastSeqOnTopic reads the highest retained seq on a topic straight from the
// in-process board, which is how a test learns the seq a peer must reply to
// without driving a second CLI identity (setAgentEnv is process-global, so two
// concurrent identities race).
func lastSeqOnTopic(t *testing.T, b *agentboard.Board, topic string) uint64 {
	t.Helper()
	msgs, found := b.ListRetained(topic)
	if !found || len(msgs) == 0 {
		t.Fatalf("topic %s holds nothing to reply to", topic)
	}
	var max uint64
	for _, m := range msgs {
		if m.Seq > max {
			max = m.Seq
		}
	}
	return max
}

// topicPayloads concatenates a topic's retained payloads, for asserting that
// something is or is NOT there.
func topicPayloads(t *testing.T, b *agentboard.Board, topic string) string {
	t.Helper()
	msgs, _ := b.ListRetained(topic)
	var sb strings.Builder
	for _, m := range msgs {
		sb.Write(m.Payload)
		sb.WriteByte('\n')
	}
	return sb.String()
}
```

with `"github.com/on-keyday/agent-harness/agentboard"` and `"strconv"` added to
the imports.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cli/agent/ -run 'TestAgentCLI_E2E_ReplyToRoutes|TestAgentCLI_E2E_WithoutReplyTo' -v`
Expected: FAIL — `flag provided but not defined: -reply-to`.

- [ ] **Step 3: Add the flag**

In `cli/agent/send.go`, beside the existing flags:

```go
	replyTo := fs.String("reply-to", "", "route replies to THIS message to this topic instead of your own chat.<short-id>; the peer needs no knowledge of it and answers with --in-reply-to alone")
```

and where the `SendRequest` is built, after `sr.SetTopic(...)`:

```go
	if *replyTo != "" {
		if !sr.SetReplyToTopic([]byte(*replyTo)) {
			return errors.New("agent: --reply-to too long")
		}
	}
```

- [ ] **Step 4: Update the usage text**

In `cmd/harness-cli/main.go`'s `agentUsage`, after the two existing `send` lines:

```go
	fmt.Fprintln(os.Stderr, "  send --reply-to R ...               route replies to THIS message to R instead of your own")
	fmt.Fprintln(os.Stderr, "                                       chat.<short-id>; the peer answers with --in-reply-to alone")
```

- [ ] **Step 5: Run tests**

Run: `make check && go test ./cli/agent/ -count=1`
Expected: PASS.

- [ ] **Step 6: Commit**

```bash
git add cli/agent/send.go cli/agent/reply_to_e2e_test.go cmd/harness-cli/main.go
git commit -m "feat(cli): agent send --reply-to declares where the answer goes

The peer needs no change and no knowledge: it answers with --in-reply-to alone
and the server routes off the parent. The E2E asserts BOTH halves — the answer
reaches the declared topic AND does not reach the asker's own chat.<short-id>,
which is the actual goal and the half a positive-only test would miss.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 5: `agent dispatch --reply-to`

**Files:**
- Modify: `cli/agent/dispatch.go` (flag set; the `SendRequest` build; the wait pattern)
- Modify: `cmd/harness-cli/main.go` (the `dispatch` usage lines)
- Test: `cli/agent/dispatch_correlation_e2e_test.go` (append)

**Interfaces:**
- Consumes: Tasks 1–3, and Task 4's flag name (`--reply-to`, same spelling).
- Produces: `agent.Dispatch` keeps its signature.

- [ ] **Step 1: Write the failing test**

Append to `cli/agent/dispatch_correlation_e2e_test.go`:

```go
// dispatch --reply-to declares the destination AND waits there. The peer still
// answers with --in-reply-to alone, so this is the whole feature end to end:
// the script gets its answer and the asking task's own inbox never sees it.
func TestAgentCLI_E2E_DispatchReplyToWaitsWhereItDeclared(t *testing.T) {
	addr := freePortE2E(t)
	board, _ := startServerE2E(t, addr)

	const ridStrA = "ws:1.2.3.4:9505-55"
	var ticketA, ticketB [16]byte
	ticketA[0] = 0xC5
	ticketB[0] = 0xC6
	tidA := mkTidE2E(0x55)
	tidB := mkTidE2E(0x56)
	ridA := mkRidE2E([4]byte{1, 2, 3, 4}, 9505, 55)
	ridB := mkRidE2E([4]byte{5, 6, 7, 8}, 9506, 56)
	board.Registry().Register(ridA, tidA, ticketA)
	board.Registry().Register(ridB, tidB, ticketB)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	selfA := agent.SelfTopic(tidA)
	selfB := agent.SelfTopic(tidB)
	const replyTo = "rr.dispatch-1"

	// The peer answers with --in-reply-to only, driven through the in-process
	// board: setAgentEnv is process-global, so a second concurrent CLI identity
	// would race the dispatcher's own.
	go func() {
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			msgs, found := board.ListRetained(selfB)
			if found && len(msgs) > 0 {
				// Same resolution the server performs for a reply with no topic.
				_, _, _ = board.Send(replyTo, []byte(`{"a":"dispatched-answer"}`),
					ridB, tidB, "peer-host", "", msgs[0].Seq)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	restoreA := setAgentEnv(addr, ridStrA, tidA, ticketA)
	defer restoreA()

	var out bytes.Buffer
	if err := agent.Dispatch(ctx,
		[]string{"--topic", selfB, "--reply-to", replyTo, "--data", `{"q":"q"}`, "--timeout", "15s"},
		strings.NewReader(""), &out); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if !strings.Contains(out.String(), "dispatched-answer") {
		t.Fatalf("dispatch = %q, want the answer from the declared topic", out.String())
	}
	if got := topicPayloads(t, board, selfA); strings.Contains(got, "dispatched-answer") {
		t.Errorf("the asking task's own topic holds %q — it was supposed to stay clean", got)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./cli/agent/ -run TestAgentCLI_E2E_DispatchReplyToWaits -v`
Expected: FAIL — `flag provided but not defined: -reply-to`.

- [ ] **Step 3: Add the flag and use it on both halves**

In `cli/agent/dispatch.go`, beside the existing flags:

```go
	replyTo := fs.String("reply-to", "", "declare THIS topic as where the reply goes, and wait there; default is your own chat.<short-id>")
```

On the send half, after `sr.SetTopic(...)`:

```go
	if *replyTo != "" {
		if !sr.SetReplyToTopic([]byte(*replyTo)) {
			return errors.New("agent: --reply-to too long")
		}
	}
```

On the wait half, replacing the `waitOn` derivation:

```go
	// Wait where we told the peer's reply to go. The two must be the same
	// topic, which is why one flag sets both: the old --reply-topic set only
	// the wait and the reply went somewhere else entirely.
	waitOn := agentboard.SelfTopic(conn.TaskID())
	if *replyTo != "" {
		waitOn = *replyTo
	}
	wr.SetPattern([]byte(waitOn))
```

Leave `Since` and `InReplyTo` as `publishedSeq` on both paths — the correlation is unchanged.

- [ ] **Step 4: Update the doc comment**

Replace the `--reply-topic` paragraph in `Dispatch`'s doc comment with:

```go
// WHERE it waits: its own chat.<short-id> by default. --reply-to changes BOTH
// halves at once — it rides on the publish as reply_to_topic, so the server
// routes the reply there, and it is what this call waits on. One flag sets both
// deliberately: the removed --reply-topic set only the wait, so a peer replying
// with --in-reply-to had its answer routed to the asker's own inbox while this
// call waited out its timeout somewhere else.
```

- [ ] **Step 5: Update the usage text**

In `agentUsage`, replace the `dispatch` block with:

```go
	fmt.Fprintln(os.Stderr, "  dispatch --topic T --data D|- [--reply-to R] [--timeout DUR]")
	fmt.Fprintln(os.Stderr, "                                       send, then block for the reply to THAT message. --reply-to R")
	fmt.Fprintln(os.Stderr, "                                       declares R as the destination AND waits there; default is your own")
	fmt.Fprintln(os.Stderr, "                                       chat.<short-id>. --timeout bounds the WHOLE call, publish ack")
	fmt.Fprintln(os.Stderr, "                                       included (scripting; NOT from an agent turn)")
```

- [ ] **Step 6: Run tests**

Run: `make check && make vet && go test ./... -count=1`
Expected: PASS, including the pre-existing dispatch tests (default path unchanged).

- [ ] **Step 7: Commit**

```bash
git add cli/agent/dispatch.go cli/agent/dispatch_correlation_e2e_test.go cmd/harness-cli/main.go
git commit -m "feat(cli): agent dispatch --reply-to declares AND waits there

One flag sets both halves on purpose. The removed --reply-topic set only the
wait, so a peer answering with --in-reply-to had its reply routed to the
asker's own inbox while the call waited out its timeout elsewhere -- the exact
failure this feature exists to remove.

Correlation is unchanged on both paths: Since and InReplyTo stay the published
seq.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 6: Documentation

**Files:**
- Modify: `runner/agentskills/harness-cli/SKILL.md` (the `go:embed` source), then copy to `.claude/skills/harness-cli/SKILL.md` and `.agents/skills/harness-cli/SKILL.md`

**Interfaces:**
- Consumes: the finished behaviour from Tasks 1–5.
- Produces: no code surface.

- [ ] **Step 1: Read the skill in full**

Read `runner/agentskills/harness-cli/SKILL.md` end to end before editing. An
addition is a claim the content is not already there, and a windowed read
cannot support one.

- [ ] **Step 2: Rewrite "Naming inbound channels"**

Replace the section body (it currently tells every agent to announce
`reply_topic` in every message and calls it "the fallback, not the mechanism")
with:

```markdown
Your inbound topic is `chat.<first-8-chars-of-task-id>`, and the server
subscribes you to it when it assigns your task. You do not have to announce it:
a reply carrying `--in-reply-to` is routed to the sender of the parent message,
resolved server-side from the retained entry.

**When you want the answer somewhere ELSE, say so when you ASK:**

```bash
harness-cli agent send --topic chat.<peer> --reply-to rr.dec-019 --data '...'
```

The server records `rr.dec-019` on your message and routes replies to it. The
peer needs no knowledge of it — it answers with `--in-reply-to <seq>` as
always. Subscribe to the topic first if you intend to receive there.

The `reply_topic` field some older payloads carry is legacy. No code reads it:
it was a convention asking the RECIPIENT to parse the payload and honour it,
which only ever worked on a peer that had read this skill — and the peers it
named as its audience are the ones without it.
```

- [ ] **Step 3: Rewrite "Per-subject reply topics (fallback)"**

Retitle it `### Per-subject reply topics` and replace the opening paragraph
(currently "When a peer cannot be relied on to set `in_reply_to`, give each
subject its own reply topic … tell the peer to reply there") with:

```markdown
Give a subject its own topic and name it with `--reply-to` when you ask, so the
answers to that subject land together and away from your `chat.<short-id>`:

```bash
harness-cli agent subscribe --topic rr.dec-019
harness-cli agent send --topic chat.<peer> --reply-to rr.dec-019 --data '...'
```

A script driving a peer on an agent's behalf wants exactly this: the answer
goes to the subject topic, the script decides what (if anything) to forward to
the agent's own `chat.<short-id>`.
```

Keep the existing limits list below it unchanged (exact-match topics, 64
messages per ring, 30-minute TTL, the 1024-topic cap, the 1 MiB payload cap,
the 64 KiB inline cap) — all of it still holds.

- [ ] **Step 4: Sync the mirrors**

```bash
cp runner/agentskills/harness-cli/SKILL.md .claude/skills/harness-cli/SKILL.md
cp runner/agentskills/harness-cli/SKILL.md .agents/skills/harness-cli/SKILL.md
diff -q runner/agentskills/harness-cli/SKILL.md .claude/skills/harness-cli/SKILL.md
diff -q runner/agentskills/harness-cli/SKILL.md .agents/skills/harness-cli/SKILL.md
```

Expected: both `diff` calls silent.

- [ ] **Step 5: Confirm no stale guidance remains**

```bash
grep -n 'reply_topic' runner/agentskills/harness-cli/SKILL.md
```

Expected: only the sentence that names it as legacy.

- [ ] **Step 6: Verify and commit**

Run: `make check && go test ./runner/ -count=1`
Expected: PASS, `TestMirrorsMatchEmbeddedSkills` included.

```bash
git add runner/agentskills/harness-cli/SKILL.md .claude/skills/harness-cli/SKILL.md .agents/skills/harness-cli/SKILL.md
git commit -m "docs: --reply-to replaces the reply_topic payload convention

The convention asked the RECIPIENT to parse a payload field and honour it. No
code reads it, and the peers the skill named as its audience -- the ones
without skill injection -- are exactly the ones that cannot. --reply-to is the
same intent as a wire field the server enforces, with nothing required of the
peer.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

## Rollout (after Task 6)

- [ ] **Restart the SERVER first**, then `make build` on the runner host and
      `scripts/build_and_restart_all.py`. A new client against an old server
      loses the field silently and gets today's routing; server-first avoids
      even that.
- [ ] `make build` in the main checkout, per the landing rule.
- [ ] **Live check, in the shape the feature was asked for**: spawn a bash peer
      that waits on its own topic and answers with `--in-reply-to` alone;
      `dispatch --topic chat.<peer> --reply-to rr.live-1` from a script; confirm
      the answer arrives on `rr.live-1`, and that `board subscribers` shows the
      asking task's `chat.<short-id>` with `pending` unchanged — the answer
      never entered the agent's inbox. Prune the peer afterwards.

## Self-Review Notes

| Spec item | Task |
| --- | --- |
| Problem: sender cannot declare a destination | 1, 2, 3 |
| Problem: script cannot keep the answer out of the agent's inbox | 5 (+ its negative assertion) |
| Problem: the payload convention nothing implements | 6 |
| Problem: `--reply-topic` removed and would not have helped | 5 (doc comment records why) |
| Goal 1 (declared on the wire, server-enforced) | 1, 2, 3 |
| Goal 2 (replier unchanged) | 4, 5 — both tests reply with `--in-reply-to` alone |
| Goal 3 (`send` and `dispatch`) | 4, 5 |
| Goal 4 (today's behaviour is the default) | 3 (third arm), 4 (`WithoutReplyTo` test) |
| Design §1 wire | 1 |
| Design §2 board | 2 |
| Design §3 server | 3 |
| Design §4 CLI | 4, 5 |
| Design §5 correlation costs nothing | 5 (correlation left unchanged; no test needed — nothing changed) |
| Design §6 documentation | 6 |
| Design §7 residual (wait-side subscription) | Not implemented by design; recorded in the spec |
| Error handling: nonexistent topic accepted | 4 (the test's `rr.task-1` does not exist until subscribed) |
| Error handling: reply that itself declares | No task — the field is per-message with no inheritance, which falls out of Task 2's per-message test |
| Error handling: over-long `--reply-to` | 4, 5 (`SetReplyToTopic` returning false) |
| Rollout | Rollout section |
