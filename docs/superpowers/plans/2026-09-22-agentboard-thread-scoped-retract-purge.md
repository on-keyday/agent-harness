# Thread-scoped retract and purge — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let an operator withdraw, and then destroy, a whole agentboard
conversation with one command, on the same unit `board thread` already groups by.

**Architecture:** Client-side fan-out. `ThreadFilter` gains `Conversation`;
two new CLI verbs resolve a thread through the existing `CollectThreadsWith`
and then call the existing per-(topic, seq) `BoardRetract` / `BoardPurge` once
per row, on ONE dialed client. No server, wire or `.bgn` change.

**Tech Stack:** Go; `cli/verb` declarative verb table with code generation
(`go generate ./cli/verb`); bubbletea TUI; wasm bridge + vanilla JS WebUI.

**Spec:** `docs/superpowers/specs/2026-09-22-agentboard-thread-scoped-retract-purge-design.md` (commit `2dab1242`)

## Global Constraints

- **No server, wire or `.bgn` change.** Everything is client-side.
- **No new capability.** Both verbs are gated on the existing `purge` bit.
- **`cli/verb/actions_gen.go` is generated — never hand-edit it.** Adding a
  flag means adding a `Flag` with a `Field` to `cli/verb/table.go`, then
  `go generate ./cli/verb`.
- **`AtLeastOne` on both destructive verbs.** A selector-less invocation must
  be a parse error, not a board-wide destruction.
- **Verification uses make targets**, never bare `go build` / `go test ./...`:
  `make check`, `make test`, `make vet`, `make test-integration`.
- **One fan-out helper.** CLI, TUI and the wasm bridge all reach it. A second
  loop anywhere is the defect this plan's Task 3 test exists to catch.
- Commit message trailer on every commit:
  `Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>`

## Decision this plan settles (spec Amendment A, written in Task 9)

**A `--conversation` key naming no conversation in the visible set is an
ERROR, not an empty result** — parallel to `--seq`'s `SeqNotVisibleError`, not
to `--task`'s empty result.

Reason: the operator copies the key from a listing. On a destructive verb,
"selected nothing" rendered as success reads as *"already cleared"*, which is
precisely the state the operator is trying to reach — so the one wrong
conclusion the failure mode produces is the one they are looking for.
`--task` can stay an empty result because the operator mints task ids from
elsewhere and a typo there is not shaped like a completed job.

## File structure

| File | Responsibility | Task |
|---|---|---|
| `cli/boardthread.go` | `ThreadFilter.Conversation`, `selectChains` extraction, `ConversationNotVisibleError` | 1 |
| `cli/boardthread_conversation_test.go` (new) | `--conversation` ≠ `--seq`; unknown key errors | 1 |
| `cli/verb/table.go` | `--conversation` on `board thread`; two new `VerbSpec` rows | 2, 4 |
| `cli/threadfanout.go` (new) | the ONE fan-out helper + its result type | 3 |
| `cli/threadfanout_test.go` (new) | partitioning, partial failure, single-loop guard | 3 |
| `cli/cmd_board.go` | two `RunBoardAction` cases | 4 |
| `cli/board_thread_lifecycle_e2e_test.go` (new) | retract → still readable → purge → gone | 5 |
| `cli/caps.go` | `purge` description names the new forms | 6 |
| `README.md`, `runner/agentskills/harness-cli/SKILL.md` + 2 mirrors | docs | 6 |
| `tui/board.go` | chains list → detail; `w`/`X` on the highlighted conversation | 7 |
| `cmd/harness-webui-wasm/main.go` | two bridge functions | 8 |
| `webui/static/main.js` | two buttons on `.board-chain-conv` | 8 |
| the spec + `firing-log.md` | Amendment A, item 39 walk | 9 |

---

### Task 1: `ThreadFilter.Conversation`

**Files:**
- Modify: `cli/boardthread.go` (`ThreadFilter` ~line 200; `SelectThreads` ~line 217)
- Test: `cli/boardthread_conversation_test.go` (create)

**Interfaces:**
- Consumes: existing `BuildThreads`, `groupConversations`, `conversationKey`, `rowsOfChain`.
- Produces: `ThreadFilter.Conversation string`; `cli.ConversationNotVisibleError`
  with field `Key string`. `SelectThreads` keeps its signature
  `func(rows []ThreadRow, topicOf map[uint64]string, f ThreadFilter) ([]ThreadRow, error)`.

**Why the refactor:** `groupConversations` is what STAMPS `ThreadRow.Conversation`,
and today it runs at each of `SelectThreads`' three exit points. Filtering by a
key therefore cannot happen inside the existing branches — the key does not
exist yet. Extract the existing body as `selectChains` returning UNGROUPED rows,
group once at the end, then filter.

- [ ] **Step 1: Write the failing test**

Create `cli/boardthread_conversation_test.go`:

```go
package cli

import (
	"errors"
	"testing"
)

// threadFixture builds the shape conversationKey's own comment records as
// measured on a live board: a two-party exchange that is FOUR chains and ONE
// conversation. Each unanswered publish is its own chain; the reply pairs are
// chains of two. All of it keys to one conversation because the parties are
// the same.
//
// The rows go through BuildThreads (not hand-built) because SelectThreads is
// what stamps Conversation, and a hand-built ThreadRow is a shape production
// never produces -- the firing-log records a TUI test that failed for exactly
// that reason.
func threadFixture() ([]BoardMessage, map[uint64]string) {
	const a, b = "aaaaaaaa11111111aaaaaaaa11111111", "bbbbbbbb22222222bbbbbbbb22222222"
	msgs := []BoardMessage{
		{Seq: 10, FromTaskHex: a, ReceivedAtMs: 1000},              // chain 1 root
		{Seq: 11, InReplyTo: 10, FromTaskHex: b, ReceivedAtMs: 1100}, // chain 1
		{Seq: 12, FromTaskHex: b, ReceivedAtMs: 1200},              // chain 2 (unanswered)
		{Seq: 13, FromTaskHex: a, ReceivedAtMs: 1300},              // chain 3 root
		{Seq: 14, InReplyTo: 13, FromTaskHex: b, ReceivedAtMs: 1400}, // chain 3
		{Seq: 15, FromTaskHex: a, ReceivedAtMs: 1500},              // chain 4 (unanswered)
	}
	topicOf := map[uint64]string{
		10: "chat.bbbbbbbb", 11: "chat.aaaaaaaa", 12: "chat.aaaaaaaa",
		13: "chat.bbbbbbbb", 14: "chat.aaaaaaaa", 15: "chat.bbbbbbbb",
	}
	return msgs, topicOf
}

// TestConversationSelectsMoreThanSeq is the whole reason the field exists: on
// one fixture --seq and --conversation must return DIFFERENT row sets. A test
// where both pass does not exercise the gap that motivated the field.
func TestConversationSelectsMoreThanSeq(t *testing.T) {
	msgs, topicOf := threadFixture()
	rows := BuildThreads(msgs, topicOf)

	bySeq, err := SelectThreads(rows, topicOf, ThreadFilter{Seq: 10})
	if err != nil {
		t.Fatalf("SelectThreads(--seq 10): %v", err)
	}
	if len(bySeq) != 2 {
		t.Fatalf("--seq 10 selected %d rows, want 2 (the chain 10<-11)", len(bySeq))
	}

	key := bySeq[0].Conversation
	if key == "" {
		t.Fatal("selected rows carry no Conversation key; groupConversations did not stamp them")
	}

	byConv, err := SelectThreads(rows, topicOf, ThreadFilter{Conversation: key})
	if err != nil {
		t.Fatalf("SelectThreads(--conversation %q): %v", key, err)
	}
	if len(byConv) != len(msgs) {
		t.Fatalf("--conversation %q selected %d rows, want all %d: the four chains are one conversation",
			key, len(byConv), len(msgs))
	}
	if len(byConv) == len(bySeq) {
		t.Fatal("--conversation and --seq returned the same set; the fixture does not exercise the gap")
	}
}

// TestConversationUnknownKeyIsAnError: an empty result on a destructive verb
// reads as "already cleared", which is the state the operator is trying to
// reach -- so the wrong conclusion is the one they are looking for.
func TestConversationUnknownKeyIsAnError(t *testing.T) {
	msgs, topicOf := threadFixture()
	rows := BuildThreads(msgs, topicOf)

	got, err := SelectThreads(rows, topicOf, ThreadFilter{Conversation: "nosuch+key"})
	if got != nil {
		t.Errorf("rows = %d, want nil", len(got))
	}
	var want *ConversationNotVisibleError
	if !errors.As(err, &want) {
		t.Fatalf("err = %v (%T), want *ConversationNotVisibleError", err, err)
	}
	if want.Key != "nosuch+key" {
		t.Errorf("Key = %q, want %q", want.Key, "nosuch+key")
	}
}

// TestConversationComposesWithTask: the axes AND, matching --seq's rule.
func TestConversationComposesWithTask(t *testing.T) {
	msgs, topicOf := threadFixture()
	rows := BuildThreads(msgs, topicOf)
	all, err := SelectThreads(rows, topicOf, ThreadFilter{})
	if err != nil {
		t.Fatalf("SelectThreads(no filter): %v", err)
	}
	key := all[0].Conversation

	got, err := SelectThreads(rows, topicOf, ThreadFilter{
		Conversation: key,
		Tasks:        []string{"cccccccc33333333cccccccc33333333"},
	})
	if err != nil {
		t.Fatalf("SelectThreads: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("rows = %d, want 0: the conversation exists but names no such task", len(got))
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./cli/ -run 'TestConversation' -v`
Expected: FAIL — `ThreadFilter` has no field `Conversation`, `ConversationNotVisibleError` undefined (compile error).

- [ ] **Step 3: Implement**

In `cli/boardthread.go`, extend `ThreadFilter`:

```go
type ThreadFilter struct {
	// Tasks: keep a chain if ANY message in it was sent by a named task OR
	// sits on that task's own inbound topic (chat.<id8>). Repeating the flag
	// UNIONS — two ids give the pair's exchange plus anything either had with
	// a third party, because a filter that dropped the third party would hide
	// the fact that the conversation was not private.
	Tasks []string
	// Seq: keep only the chain containing it. 0 = no selection.
	Seq uint64
	// Conversation: keep only the rows groupConversations keyed to this
	// string. A conversation is coarser than a chain — measured, a two-party
	// exchange five minutes long was four chains and one conversation — so
	// this is the only selector that names the unit the views group by and the
	// export sheet contains. "" = no selection.
	//
	// Applied AFTER grouping, because grouping is what assigns the key.
	Conversation string
}

// ConversationNotVisibleError is a key that names no conversation in the
// visible set. It is an error and not an empty result on purpose, matching
// SeqNotVisibleError rather than --task: the key is copied from a listing, and
// on the destructive verbs an empty result reads as "already cleared" — the
// state the caller is trying to reach, so the one wrong conclusion the failure
// produces is the one they are looking for.
type ConversationNotVisibleError struct {
	Key string
}

func (e *ConversationNotVisibleError) Error() string {
	return fmt.Sprintf("board thread: conversation %q is not in the visible set (a conversation key is derived from its participants, so it changes when a party is a fresh task; re-read it from `board thread`)", e.Key)
}
```

Rename the existing `SelectThreads` body to `selectChains`, deleting the
`groupConversations` call from each of its three `return` statements so it
yields UNGROUPED rows, then add the new `SelectThreads` in front of it:

```go
// SelectThreads filters rows produced by BuildThreads to the chains the
// filter selects, groups them into conversation sections, and — when the
// filter names a conversation — keeps only that one.
//
// Grouping runs once, here, rather than at each of selectChains' exits,
// because grouping is what stamps ThreadRow.Conversation and a Conversation
// filter has nothing to match before it has run.
func SelectThreads(rows []ThreadRow, topicOf map[uint64]string, f ThreadFilter) ([]ThreadRow, error) {
	picked, err := selectChains(rows, topicOf, f)
	if err != nil {
		return nil, err
	}
	grouped := groupConversations(picked, topicOf)
	if f.Conversation == "" {
		return grouped, nil
	}
	kept := make([]ThreadRow, 0, len(grouped))
	for _, r := range grouped {
		if r.Conversation == f.Conversation {
			kept = append(kept, r)
		}
	}
	if len(kept) == 0 {
		// Distinguish "no such conversation" from "this conversation exists
		// but the other axes excluded it". Only the first is a bad argument;
		// the second is the AND of the axes, which --seq already answers with
		// an empty result.
		for _, r := range groupConversations(rows, topicOf) {
			if r.Conversation == f.Conversation {
				return nil, nil
			}
		}
		return nil, &ConversationNotVisibleError{Key: f.Conversation}
	}
	return kept, nil
}

func selectChains(rows []ThreadRow, topicOf map[uint64]string, f ThreadFilter) ([]ThreadRow, error) {
	// ... existing body, with groupConversations(...) stripped from its returns
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./cli/ -run 'TestConversation|TestSelectThreads|TestBuildThreads|TestGroupConv' -v`
Expected: PASS, including the pre-existing thread tests — the refactor must not
move a row.

- [ ] **Step 5: Commit**

```bash
git add cli/boardthread.go cli/boardthread_conversation_test.go
git commit -m "feat(cli): select a thread by conversation, not only by chain

--seq picks the chain containing a message and --task unions in third-party
exchanges; neither names a conversation, which is what the views group by and
what the export sheet contains. conversationKey's own comment records the gap
as measured: a two-party exchange five minutes long was four chains and one
conversation.

Grouping is what stamps the key, so it moves from selectChains' three exits to
one call in SelectThreads, and the new filter applies after it.

An unknown key is an error rather than an empty result, matching --seq: the key
is copied from a listing, and on the destructive verbs this feature adds next,
an empty result reads as 'already cleared' -- the state the caller wants, so
the wrong conclusion would be the one they are looking for.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 2: `--conversation` on `board thread`

**Files:**
- Modify: `cli/verb/table.go` (the `board thread` `VerbSpec`, ~line 1047)
- Modify: `cli/cmd_board.go` (`case verb.SubThread`, pass the field through)
- Regenerate: `cli/verb/actions_gen.go`

**Interfaces:**
- Consumes: Task 1's `ThreadFilter.Conversation`.
- Produces: `verb.BoardAction.Conversation string`, generated.

- [ ] **Step 1: Add the flag to the declaration**

In the `board thread` `VerbSpec`'s `Flags`, after the `task` entry:

```go
			{Name: "conversation", Type: FlagString, Default: "", Field: "Conversation",
				Help: "only this conversation (the key `board thread` prints in each section header); coarser than --seq, which is one chain"},
```

and add to that spec's `Examples`:

```go
			"board thread --conversation 60542da9+70fbad4a",
```

- [ ] **Step 2: Regenerate and confirm the field exists**

Run: `go generate ./cli/verb && grep -n 'Conversation' cli/verb/actions_gen.go`
Expected: a `Conversation string` field on `BoardAction` and an assignment
filling it. If `grep` prints nothing, the `Field:` name is wrong.

- [ ] **Step 3: Pass it through**

In `cli/cmd_board.go`, `case verb.SubThread`:

```go
		rows, serr := CollectThreads(ctx, cid, ThreadFilter{
			Tasks:        ba.Tasks,
			Seq:          ba.Seq,
			Conversation: ba.Conversation,
		})
```

- [ ] **Step 4: Run the verb invariants**

Run: `go test ./cli/verb/ -v`
Expected: PASS. `TestEveryDeclaredFlagIsReadByItsBuild` is the one that fails if
the `Field` is declared and never read.

- [ ] **Step 5: Commit**

```bash
git add cli/verb/table.go cli/verb/actions_gen.go cli/cmd_board.go
git commit -m "feat(cli): board thread --conversation

The read verb gets the selector first, so the expression that names what you
read is the expression the destructive verbs will take.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 3: The fan-out helper

**Files:**
- Create: `cli/threadfanout.go`
- Test: `cli/threadfanout_test.go`

**Interfaces:**
- Consumes: `CollectThreadsWith(ctx, *Client, ThreadFilter)`, `ConversationHeader(rows)`,
  `(*Client).BoardRetract(ctx, topic, seq) (bool, error)`,
  `(*Client).BoardPurge(ctx, topic, seq) (int, bool, error)`.
- Produces:

```go
type ThreadOp int
const (ThreadRetract ThreadOp = iota; ThreadPurge)

type ThreadRowResult struct {
	Seq     uint64 `json:"seq"`
	Topic   string `json:"topic"`
	Outcome string `json:"outcome"` // retracted|purged|already-withdrawn|not-found|failed|skipped
}

type ThreadFanoutResult struct {
	Header string
	Rows   []ThreadRowResult
	Err    error // the error that stopped the run, if any
}

func (r ThreadFanoutResult) Counts() map[string]int
func (r ThreadFanoutResult) Summary() string

func FanoutThread(ctx context.Context, c *Client, op ThreadOp, f ThreadFilter) (ThreadFanoutResult, error)
```

- [ ] **Step 1: Write the failing test**

Create `cli/threadfanout_test.go`:

```go
package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestFanoutDoesNotCallForAlreadyWithdrawn is the regression this helper
// exists for. The server collapses already-withdrawn into not_found so a reply
// cannot probe a topic, so a naive fan-out reports a half-withdrawn thread as
// "retracted N / not_found N" -- success rendered as failure. Measured on the
// exchange that prompted this feature: 11 of 22 messages were already
// auto-retracted, because a reply withdraws the message it answers.
//
// The client has just read every row and holds Retracted per message, so it
// partitions locally and asks the server nothing extra.
func TestFanoutDoesNotCallForAlreadyWithdrawn(t *testing.T) {
	rows := []ThreadRow{
		{Topic: "chat.aaaaaaaa", Msg: BoardMessage{Seq: 1}},
		{Topic: "chat.aaaaaaaa", Msg: BoardMessage{Seq: 2, Retracted: true}},
		{Topic: "chat.bbbbbbbb", Msg: BoardMessage{Seq: 3}},
		{Topic: "chat.bbbbbbbb", Msg: BoardMessage{Seq: 4, Retracted: true}},
	}
	calls := []uint64{}
	res := fanoutRows(rows, "hdr", func(topic string, seq uint64) (string, error) {
		calls = append(calls, seq)
		return "retracted", nil
	})

	if len(calls) != 2 || calls[0] != 1 || calls[1] != 3 {
		t.Fatalf("calls = %v, want [1 3]: a withdrawn row must not be called for", calls)
	}
	got := res.Counts()
	for k, want := range map[string]int{
		"retracted": 2, "already-withdrawn": 2, "not-found": 0, "failed": 0, "skipped": 0,
	} {
		if got[k] != want {
			t.Errorf("Counts()[%q] = %d, want %d", k, got[k], want)
		}
	}
}

// TestFanoutSummaryPrintsZeros: a zero is a measurement. Eliding it makes
// "nothing failed" indistinguishable from "failures were not counted".
func TestFanoutSummaryPrintsZeros(t *testing.T) {
	rows := []ThreadRow{{Topic: "chat.aaaaaaaa", Msg: BoardMessage{Seq: 1}}}
	res := fanoutRows(rows, "hdr", func(string, uint64) (string, error) {
		return "purged", nil
	})
	s := res.Summary()
	for _, want := range []string{"purged 1", "not-found 0", "failed 0", "skipped 0"} {
		if !strings.Contains(s, want) {
			t.Errorf("Summary() = %q, missing %q", s, want)
		}
	}
}

// TestFanoutStopsOnErrorAndCountsTheRest: a capability denial on the first
// call is a denial on every call, so the helper must not hammer the server
// with the remaining rows -- and the untried rows are "skipped", which is
// neither "we tried and it was gone" nor "it failed".
func TestFanoutStopsOnErrorAndCountsTheRest(t *testing.T) {
	rows := []ThreadRow{
		{Topic: "t", Msg: BoardMessage{Seq: 1}},
		{Topic: "t", Msg: BoardMessage{Seq: 2}},
		{Topic: "t", Msg: BoardMessage{Seq: 3}},
	}
	boom := errors.New("permission denied: BoardRetract requires capability purge")
	n := 0
	res := fanoutRows(rows, "hdr", func(string, uint64) (string, error) {
		n++
		if n == 2 {
			return "", boom
		}
		return "retracted", nil
	})
	if n != 2 {
		t.Fatalf("issued %d calls, want 2: the run must stop at the first error", n)
	}
	if !errors.Is(res.Err, boom) {
		t.Errorf("Err = %v, want the stopping error", res.Err)
	}
	got := res.Counts()
	if got["retracted"] != 1 || got["failed"] != 1 || got["skipped"] != 1 {
		t.Errorf("Counts() = %v, want retracted 1 / failed 1 / skipped 1", got)
	}
}

// TestFanoutNotFoundContinues: not_found is an outcome, not a fault.
func TestFanoutNotFoundContinues(t *testing.T) {
	rows := []ThreadRow{
		{Topic: "t", Msg: BoardMessage{Seq: 1}},
		{Topic: "t", Msg: BoardMessage{Seq: 2}},
	}
	n := 0
	res := fanoutRows(rows, "hdr", func(string, uint64) (string, error) {
		n++
		return "not-found", nil
	})
	if n != 2 {
		t.Fatalf("issued %d calls, want 2: not-found must not stop the run", n)
	}
	if res.Err != nil {
		t.Errorf("Err = %v, want nil", res.Err)
	}
}

func TestFanoutUsesTheRowsOwnTopic(t *testing.T) {
	rows := []ThreadRow{
		{Topic: "chat.aaaaaaaa", Msg: BoardMessage{Seq: 1}},
		{Topic: "chat.bbbbbbbb", Msg: BoardMessage{Seq: 2}},
	}
	seen := map[uint64]string{}
	fanoutRows(rows, "hdr", func(topic string, seq uint64) (string, error) {
		seen[seq] = topic
		return "retracted", nil
	})
	if seen[1] != "chat.aaaaaaaa" || seen[2] != "chat.bbbbbbbb" {
		t.Fatalf("topics = %v: a thread spans topics, so each row carries its own", seen)
	}
}

var _ = context.Background
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./cli/ -run TestFanout -v`
Expected: FAIL — `fanoutRows` undefined (compile error).

- [ ] **Step 3: Implement**

Create `cli/threadfanout.go`:

```go
package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// ThreadOp is which destructive verb the fan-out runs.
type ThreadOp int

const (
	// ThreadRetract withdraws each message from every agent-facing path,
	// leaving it readable to the operator marked RETRACTED.
	ThreadRetract ThreadOp = iota
	// ThreadPurge destroys the bytes, operator view included.
	ThreadPurge
)

func (o ThreadOp) verb() string {
	if o == ThreadPurge {
		return "purged"
	}
	return "retracted"
}

// ThreadRowResult is one message's outcome. Outcome takes exactly the values
// Summary counts, so the JSON form and the text form cannot disagree.
type ThreadRowResult struct {
	Seq     uint64 `json:"seq"`
	Topic   string `json:"topic"`
	Outcome string `json:"outcome"`
}

// ThreadFanoutResult is what one run did. Err is the error that STOPPED the
// run; rows after it carry "skipped".
type ThreadFanoutResult struct {
	Header string
	Rows   []ThreadRowResult
	Err    error
}

// categories is the fixed print order. Fixed rather than derived from the
// rows, because a category with no rows must still print its zero: gating on
// the value would make "nothing failed" and "failures were not counted" the
// same output.
func (r ThreadFanoutResult) categories() []string {
	seen := map[string]bool{}
	for _, row := range r.Rows {
		seen[row.Outcome] = true
	}
	out := []string{}
	for _, c := range []string{"retracted", "purged", "already-withdrawn"} {
		if seen[c] {
			out = append(out, c)
		}
	}
	// Always printed, whatever the counts.
	return append(out, "not-found", "failed", "skipped")
}

// Counts totals the outcomes, with a zero for every category Summary prints.
func (r ThreadFanoutResult) Counts() map[string]int {
	out := map[string]int{}
	for _, c := range r.categories() {
		out[c] = 0
	}
	for _, row := range r.Rows {
		out[row.Outcome]++
	}
	return out
}

// Summary is the operator-facing line: the target named, then every category
// with its count, zeros included.
func (r ThreadFanoutResult) Summary() string {
	counts := r.Counts()
	parts := make([]string, 0, len(counts))
	for _, c := range r.categories() {
		parts = append(parts, fmt.Sprintf("%s %d", c, counts[c]))
	}
	return "  " + strings.Join(parts, "   ")
}

// fanoutRows walks rows in order, calling do for each row that is not already
// withdrawn, and stops at the first error. Split from FanoutThread so the
// partitioning, the stop rule and the counting are testable without a server.
func fanoutRows(rows []ThreadRow, header string, do func(topic string, seq uint64) (string, error)) ThreadFanoutResult {
	res := ThreadFanoutResult{Header: header, Rows: make([]ThreadRowResult, 0, len(rows))}
	for i, r := range rows {
		rr := ThreadRowResult{Seq: r.Msg.Seq, Topic: r.Topic}
		switch {
		case res.Err != nil:
			rr.Outcome = "skipped"
		case r.Msg.Retracted:
			// Not asked about. The server would answer not_found -- it
			// collapses already-withdrawn into it so a reply cannot probe a
			// topic -- and that answer would render a handled message as a
			// failure.
			rr.Outcome = "already-withdrawn"
		default:
			outcome, err := do(r.Topic, r.Msg.Seq)
			if err != nil {
				// A capability denial on one call is a denial on all of them.
				// Stop, and mark the untried remainder skipped.
				res.Err = err
				rr.Outcome = "failed"
			} else {
				rr.Outcome = outcome
			}
		}
		res.Rows = append(res.Rows, rr)
		_ = i
	}
	return res
}

// FanoutThread resolves the filter to a thread and runs op over every message
// in it, on the client it is given. ONE client for the whole run: a thread is
// tens of messages and dialing per message would be tens of handshakes.
//
// This is the only place the per-seq destructive calls are made on a thread
// path. CLI, TUI and the wasm bridge all reach it; a second loop elsewhere is
// how one surface silently grows different semantics.
func FanoutThread(ctx context.Context, c *Client, op ThreadOp, f ThreadFilter) (ThreadFanoutResult, error) {
	rows, err := CollectThreadsWith(ctx, c, f)
	if err != nil {
		return ThreadFanoutResult{}, err
	}
	if len(rows) == 0 {
		return ThreadFanoutResult{}, fmt.Errorf("board: the filter selected no messages")
	}
	header := ConversationHeader(rows)
	// Rows arrive grouped; act in seq order so a partial run is a prefix an
	// operator can reason about rather than an arbitrary subset.
	ordered := append([]ThreadRow(nil), rows...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Msg.Seq < ordered[j].Msg.Seq })

	return fanoutRows(ordered, header, func(topic string, seq uint64) (string, error) {
		if op == ThreadPurge {
			_, found, err := c.BoardPurge(ctx, topic, seq)
			if err != nil {
				return "", err
			}
			if !found {
				return "not-found", nil
			}
			return "purged", nil
		}
		found, err := c.BoardRetract(ctx, topic, seq)
		if err != nil {
			return "", err
		}
		if !found {
			return "not-found", nil
		}
		return "retracted", nil
	}), nil
}
```

- [ ] **Step 4: Run tests**

Run: `go test ./cli/ -run TestFanout -v`
Expected: PASS (5 tests).

- [ ] **Step 5: Add the single-loop guard**

Append to `cli/threadfanout_test.go`:

```go
// TestOnlyOneThreadFanoutLoopExists: the failure class this guards is a
// surface growing its own loop over BoardRetract/BoardPurge and drifting from
// the partitioning above -- the shape implementation-pitfalls Pitfall 3
// records. Counting the call sites is cheap; discovering the drift by symptom
// is not.
func TestOnlyOneThreadFanoutLoopExists(t *testing.T) {
	roots := []string{"../cli", "../tui", "../cmd/harness-webui-wasm"}
	allowed := map[string]bool{
		"cli/threadfanout.go": true, // the helper itself
		"cli/cmd_board.go":    true, // board retract / board purge, per-seq verbs
		"cli/board.go":        true, // the client methods
	}
	// walk roots, grep for `.BoardRetract(` / `.BoardPurge(`, fail on a file
	// that is neither allowed nor a _test.go
	assertCallSites(t, roots, []string{".BoardRetract(", ".BoardPurge("}, allowed)
}
```

Implement `assertCallSites` in the same file using `filepath.WalkDir` +
`os.ReadFile` + `strings.Contains`, skipping `_test.go`.

- [ ] **Step 6: Run and commit**

Run: `go test ./cli/ -run 'TestFanout|TestOnlyOne' -v`
Expected: PASS.

```bash
git add cli/threadfanout.go cli/threadfanout_test.go
git commit -m "feat(cli): one fan-out for thread-scoped destruction

The server collapses already-withdrawn into not_found on purpose, so a reply
cannot probe a topic. A naive fan-out therefore renders success as failure: on
the exchange that prompted this feature, 11 of 22 messages were already
auto-retracted by the replies that answered them, and a straight loop reports
'retracted 11 / not_found 11'. The client has just read every row and holds
Retracted per message, so it partitions locally and asks the server nothing.

One client for the whole run, one helper for all three surfaces, and a test
that fails when a second loop appears.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 4: The two verbs

**Files:**
- Modify: `cli/verb/table.go` (two new `VerbSpec` rows after `board purge`)
- Modify: `cli/cmd_board.go` (two `RunBoardAction` cases)
- Regenerate: `cli/verb/actions_gen.go`

**Interfaces:**
- Consumes: Task 3's `FanoutThread`, `ThreadOp`; Task 1's `ThreadFilter.Conversation`.
- Produces: `verb.SubRetractThread = "retract-thread"`, `verb.SubPurgeThread = "purge-thread"`.

- [ ] **Step 1: Declare both verbs**

In `cli/verb/table.go`, immediately after the `board purge` spec:

```go
	{
		Path: []string{"board", "retract-thread"},
		Notes: []string{
			"withdraw every message in ONE conversation from every agent-facing path,",
			"across all the topics it spans. Leaves them readable to the operator marked",
			"RETRACTED, like `board retract` (cap: purge). No <topic> argument: a",
			"conversation spans topics by construction, so naming one cannot select it.",
			"A selector is REQUIRED -- see `board thread` with the same selector for what",
			"this will reach.",
		},
		CmdlineSurfaces: CLI,
		ModalSurfaces: []ModalSurface{
			// w on the highlighted conversation in the chains list.
			{Surface: TUI, At: "tui/board.go:BoardModal"},
			{Surface: WebUI, At: "webui/index.html#board-chains-view"},
		},
		Action: "BoardAction",
		Const:  map[string]string{"Sub": "retract-thread"},
		// board thread with no selector prints every chain on the board. A
		// destructive twin inheriting that default means "withdraw the board",
		// which is the --seq-left-at-zero shape recorded on `board purge`
		// above. Same answer prune gives: the widest form has to be asked for.
		AtLeastOne: []Rule{{Flags: []string{"seq", "task", "conversation"},
			Reason: "a bare retract-thread would withdraw every conversation on the board; say which"}},
		Flags: []Flag{
			{Name: "seq", Type: FlagUint64, Default: uint64(0), Field: "Seq",
				Help: "the chain containing this seq -- NARROWER than a conversation, so this can leave a thread half-withdrawn"},
			{Name: "task", Type: FlagString, Custom: argListValue, Field: "Tasks",
				Help: "conversations involving ANY named task (repeatable, 32-hex id); repeating UNIONS, so this also reaches what either task discussed with a third party"},
			{Name: "conversation", Type: FlagString, Default: "", Field: "Conversation",
				Help: "the key `board thread` prints in each section header -- the unit the views group by"},
			{Name: "json", Type: FlagBool, Default: false, Field: "JSON", Help: "JSON Lines instead of text"},
		},
		Examples: []string{
			"board thread --conversation 60542da9+70fbad4a",
			"board retract-thread --conversation 60542da9+70fbad4a",
			"board retract-thread --task aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	},
	{
		Path: []string{"board", "purge-thread"},
		Notes: []string{
			"destroy every message in ONE conversation, across all the topics it spans,",
			"operator view included (cap: purge). Reaches messages `retract-thread` already",
			"withdrew. No <topic> argument and a REQUIRED selector, for the same reasons as",
			"`board retract-thread`. This is irreversible and the board is the only copy --",
			"the WebUI chains view has an Export button, and `board thread` with the same",
			"selector prints what this removes.",
		},
		CmdlineSurfaces: CLI,
		ModalSurfaces: []ModalSurface{
			// X on the highlighted conversation in the chains list.
			{Surface: TUI, At: "tui/board.go:BoardModal"},
			{Surface: WebUI, At: "webui/index.html#board-chains-view"},
		},
		Action: "BoardAction",
		Const:  map[string]string{"Sub": "purge-thread"},
		AtLeastOne: []Rule{{Flags: []string{"seq", "task", "conversation"},
			Reason: "a bare purge-thread would destroy every conversation on the board; say which"}},
		Flags: []Flag{
			{Name: "seq", Type: FlagUint64, Default: uint64(0), Field: "Seq",
				Help: "the chain containing this seq -- NARROWER than a conversation, so this can leave a thread half-destroyed"},
			{Name: "task", Type: FlagString, Custom: argListValue, Field: "Tasks",
				Help: "conversations involving ANY named task (repeatable, 32-hex id); repeating UNIONS, so this also reaches what either task discussed with a third party"},
			{Name: "conversation", Type: FlagString, Default: "", Field: "Conversation",
				Help: "the key `board thread` prints in each section header -- the unit the views group by"},
			{Name: "json", Type: FlagBool, Default: false, Field: "JSON", Help: "JSON Lines instead of text"},
		},
		Examples: []string{
			"board thread --conversation 60542da9+70fbad4a",
			"board purge-thread --conversation 60542da9+70fbad4a",
		},
	},
```

- [ ] **Step 2: Regenerate and run the invariants**

Run: `go generate ./cli/verb && go test ./cli/verb/ -v`
Expected: PASS. Specifically `TestWidestFormIsNeverTheBareOne` must pass
because of `AtLeastOne`, and `TestDeclaredRulesRefuseWhatTheBuildsRefused`
must see the rule refuse the bare form.

- [ ] **Step 3: Wire the cases**

In `cli/cmd_board.go`, before `default:`:

```go
	case verb.SubRetractThread, verb.SubPurgeThread:
		// One dialed client for the whole run: a thread is tens of messages
		// and the package-level BoardRetract/BoardPurge helpers dial per call.
		c, derr := Dial(ctx, cid, protocol.ClientKind_Cli)
		if derr != nil {
			return derr
		}
		defer c.Close()

		op := ThreadRetract
		if ba.Sub == verb.SubPurgeThread {
			op = ThreadPurge
		}
		res, ferr := FanoutThread(ctx, c, op, ThreadFilter{
			Tasks:        ba.Tasks,
			Seq:          ba.Seq,
			Conversation: ba.Conversation,
		})
		if ferr != nil {
			return ferr
		}
		if ba.JSON {
			for _, row := range res.Rows {
				b, merr := json.Marshal(row)
				if merr != nil {
					return merr
				}
				fmt.Fprintf(out, "%s\n", b)
			}
		} else {
			fmt.Fprintf(out, "thread: %s\n%s\n", res.Header, res.Summary())
		}
		// The stopping error is reported after the partial result, so what
		// completed is visible before the reason it stopped.
		if res.Err != nil {
			return res.Err
		}
```

Add `encoding/json` and the `protocol` import if not already present.

- [ ] **Step 4: Verify the bare form is refused, by hand**

Run: `go run ./cmd/harness-cli board purge-thread`
Expected: a usage error naming the rule's Reason, NOT a destructive run.

Run: `go run ./cmd/harness-cli board retract-thread --help`
Expected: the Notes and all four flags.

- [ ] **Step 5: Commit**

```bash
git add cli/verb/table.go cli/verb/actions_gen.go cli/cmd_board.go
git commit -m "feat(cli): board retract-thread / purge-thread

Neither takes a <topic>: a conversation spans topics by construction, so
naming one cannot select it. Both require a selector via AtLeastOne, for the
reason board purge's --seq comment records -- a widest form reachable by
omission destroyed two messages on a live board -- and the reason prune already
answers the same way.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 5: Two-stage lifecycle e2e

**Files:**
- Create: `cli/board_thread_lifecycle_e2e_test.go` (model it on `cli/board_e2e_test.go`)

**Interfaces:** consumes Tasks 1–4.

**Why:** the whole feature rests on purge reaching what retract withdrew.
`agentboard/topic.go`'s `removeSeq` scans `ring` and then `retracted`, so it
does — but that is currently known by reading. A feature whose two stages are
ordered must check the order, not trust it.

- [ ] **Step 1: Write the failing test**

```go
// TestThreadLifecycleRetractThenPurge walks the operator's actual workflow:
// retract when the discussion ends, export, purge after. The middle step is
// the assertion that matters -- a retracted thread is GONE from the agent
// paths and STILL THERE for the operator, which is what makes "export later"
// possible at all.
func TestThreadLifecycleRetractThenPurge(t *testing.T) {
	// 1. Bring up the test server + two subscribed tasks (copy the harness
	//    from cli/board_e2e_test.go).
	// 2. Publish a cross-topic exchange: A -> chat.b, B replies --in-reply-to
	//    (which auto-retracts A's message), A -> chat.b again.
	// 3. key := the Conversation of any row from `CollectThreads`.
	// 4. FanoutThread(ctx, c, ThreadRetract, ThreadFilter{Conversation: key})
	//    - assert Counts()["already-withdrawn"] > 0   (the auto-retracted one)
	//    - assert Counts()["retracted"] > 0
	//    - assert Counts()["not-found"] == 0          (the whole point)
	// 5. assert BoardRead still returns every message, each with Retracted set
	// 6. assert the agent face does NOT: agent inbox / agent retained on those
	//    topics return none of them
	// 7. FanoutThread(ctx, c, ThreadPurge, ThreadFilter{Conversation: key})
	//    - assert Counts()["purged"] == total, ["not-found"] == 0
	//      <- this is the step that pins removeSeq scanning `retracted`
	// 8. assert BoardRead now returns nothing for those seqs
}
```

Write it out in full against the existing e2e harness; the comment above is the
assertion list, not a substitute for the code.

- [ ] **Step 2: Run it**

Run: `make test-integration` (or `go test ./cli/ -run TestThreadLifecycle -v`
if the e2e harness is in-package).
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add cli/board_thread_lifecycle_e2e_test.go
git commit -m "test(cli): pin that purge reaches what retract withdrew

withdrawLocked moves a message out of topic.ring into topic.retracted, and
removeSeq scans both -- so the operator's retract-then-purge order works. That
was known by reading the code. The feature's two stages are ordered, so the
order is now checked.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 6: Capability description and docs

**Files:**
- Modify: `cli/caps.go` (`CapDescription`, `Capability_Purge` arm, ~line 94)
- Modify: `README.md` (board section + TUI cmdline verb list)
- Modify: `runner/agentskills/harness-cli/SKILL.md` (go:embed source)
- Mirror: `.claude/skills/harness-cli/SKILL.md`, `.agents/skills/harness-cli/SKILL.md`

- [ ] **Step 1: The capability catalog**

The current arm enumerates the verbs the bit authorizes, so two more forms must
be named or the catalog understates what granting `purge` hands over:

```go
	case protocol.Capability_Purge:
		return "destroy an agentboard topic's retained-message buffer (agent purge / board purge) " +
			"or a whole conversation's messages across every topic it spans (board purge-thread), " +
			"or withdraw messages from every agent path while leaving them readable to the operator " +
			"(board retract, board retract-thread)"
```

- [ ] **Step 2: Run the capability tests**

Run: `go test ./cli/ ./server/ -run 'Cap' -v`
Expected: PASS. `server/cap_completeness_test.go` is the one that fails if a
capability grows a form nothing describes.

- [ ] **Step 3: README**

In the board/agentboard section, after the `board purge` description, add the
two verbs with the "no topic argument, selector required" sentence, and add
both paths to the TUI cmdline verb list.

- [ ] **Step 4: Agent-facing skill**

`runner/agentskills/harness-cli/SKILL.md` already has **"Somebody else can
withdraw your message"**. Extend that paragraph: two more forms can now do so
at conversation scope, so an agent seeing several of its messages vanish at
once is seeing one operator action, not a fault. Edit the embed source, then
copy it byte-for-byte to both mirrors.

- [ ] **Step 5: Verify the mirrors**

Run: `go test ./runner/agentskills/ -v`
Expected: PASS — `TestMirrorsMatchEmbeddedSkills` fails on a byte difference.

- [ ] **Step 6: Commit**

```bash
git add cli/caps.go README.md runner/agentskills/harness-cli/SKILL.md \
        .claude/skills/harness-cli/SKILL.md .agents/skills/harness-cli/SKILL.md
git commit -m "docs: name the thread-scoped forms wherever purge is described

CapDescription enumerates the verbs the purge bit authorizes, so two more
forms means the sentence understates what granting it hands over. The agent
skill gets the same note from the other side: several of your messages
vanishing at once is one operator action, not a fault.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 7: TUI — chains becomes list → detail

**Files:**
- Modify: `tui/board.go` (the `boardChains` mode, its update and its view)
- Test: `tui/board_test.go`

**Interfaces:** consumes `FanoutThread`, `ConversationHeader`, `ThreadFilter`.

**Why the restructure and not just a cursor:** the board modal is list → detail
everywhere else (topics → `Enter` → messages → `w`/`X` on a message). `chains`
is the one view that is a scrolled blob. Making it a list of conversations
supplies the selection the action needs AND removes the asymmetry, in one move.

- [ ] **Step 1: Add the two modes**

Split `boardChains` into `boardChainList` (conversations, one row each:
`ConversationHeader` + message count) and `boardChainDetail` (today's rendering,
filtered to the chosen conversation). `Enter` descends, `Esc` ascends.

- [ ] **Step 2: Add the actions**

In `boardChainList`: `w` → `DoBoardRetractThread(c, key)`, `X` →
`DoBoardPurgeThread(c, key)`. Both follow the existing `DoBoardRetract` shape —
**`c *cli.Client` threaded from `a.client`, never Dial+Close**, which is the
pattern every other `Do*` in this package uses and the one Pitfall 3 records
being broken. Footer becomes:
`↑/↓ select · Enter: open · w: retract thread  X: purge thread  c: refresh  Esc: back`

- [ ] **Step 3: Write the test**

```go
// TestChainsListActionsScopeToTheHighlightedConversation is the surface-parity
// invariant, not presentation: no surface may offer a wider destructive form
// than the CLI allows. The CLI requires a selector; this list requires a
// selection, and the action must carry the highlighted row's key and no other.
func TestChainsListActionsScopeToTheHighlightedConversation(t *testing.T) {
	// build a model with two conversations, move the cursor to the second,
	// send tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'X'}},
	// assert the emitted command carries conversation key #2 and not #1.
}
```

Build the fixture by running messages through `BuildThreads` + `SelectThreads`,
**not** by hand-constructing `ThreadRow` values — the firing-log records a TUI
test that failed because hand-built rows are a shape production never produces.

- [ ] **Step 4: Run and commit**

Run: `go test ./tui/ -v`

```bash
git add tui/board.go tui/board_test.go
git commit -m "feat(tui): the chains view becomes a list of conversations

Every other view in the board modal is list -> detail; chains was a scrolled
blob with no cursor, which is also why it had no way to act on one thread. One
move fixes both: the list supplies the selection the destructive actions need.

w/X keep the meanings the modal already established -- retract and purge, at
the scope the view is a view of.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 8: WebUI — buttons on the conversation header

**Files:**
- Modify: `cmd/harness-webui-wasm/main.go` (two bridge fns + registration ~line 146)
- Modify: `webui/static/main.js` (`renderBoardChains`, the `.board-chain-conv` block ~line 5645)
- Modify: `webui/index.html` (CSS hook only if the header needs a flex row)

**Interfaces:**
- Produces: `harness.boardRetractThread(key) -> Promise<{header, rows:[{seq,topic,outcome}]}>`
  and `harness.boardPurgeThread(key)` with the same shape.

- [ ] **Step 1: The bridge functions**

Model on `harnessBoardRetract` (`cmd/harness-webui-wasm/main.go:1840`). Each
takes the conversation key, calls `cli.FanoutThread` with
`ThreadFilter{Conversation: key}` on the existing client, and returns the rows.
**The browser passes back the key it was given and never constructs one** — if
a key ever has to be built in JS, export the Go serializer over the bridge
instead of mirroring it.

- [ ] **Step 2: The buttons**

In `renderBoardChains`, where the `.board-chain-conv` header div is created,
append two buttons carrying `currentConv`:

```js
      if (r.conversation !== currentConv) {
        currentConv = r.conversation;
        const h = document.createElement("div");
        h.className = "board-chain-conv";
        const label = document.createElement("span");
        label.textContent = headerOf.get(currentConv) || currentConv;
        h.appendChild(label);
        // Scoped to THIS conversation, never to what the view is showing:
        // harness.boardThread() takes no selector and returns every
        // conversation on the board, so a button acting on "what is displayed"
        // would be the bare form the CLI refuses to have.
        h.appendChild(convActionBtn("w Retract", currentConv, "boardRetractThread"));
        h.appendChild(convActionBtn("X Purge", currentConv, "boardPurgeThread"));
        boardChainsRowsEl.appendChild(h);
      }
```

`convActionBtn(label, key, fn)` creates the button, calls
`window.harness[fn](key)`, writes the result through `appendCmdOutput` (the
result surface — `setStatus` is the connection badge), and re-renders.

- [ ] **Step 3: Assert the dispatch map is unaffected**

Run: load the WebUI. `WEBUI_DISPATCH`'s startup assertion covers paths
`harness.pathsForSurface("webui")` returns; these verbs are `CmdlineSurfaces: CLI`,
so they are not in it and nothing should throw.

- [ ] **Step 4: Run and commit**

Run: `make js-test && make wasm-check`

```bash
git add cmd/harness-webui-wasm/main.go webui/static/main.js webui/index.html
git commit -m "feat(webui): retract and purge one conversation from its header

The header the chains view already draws per conversation is the only place on
this surface that names ONE thread. The Export modal does not: its content is
whatever is displayed, and harness.boardThread() takes no selector, so a button
there would destroy every conversation on the board -- the bare form the CLI
refuses to have.

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

### Task 9: Spec Amendment A, item 39, firing-log

**Files:**
- Modify: `docs/superpowers/specs/2026-09-22-agentboard-thread-scoped-retract-purge-design.md`
- Modify: `.claude/skills/surface-parity-checklist/firing-log.md`

- [ ] **Step 1: Amendment A**

Append to the spec:

```markdown
# Amendment A (2026-09-22) — an unknown conversation key is an error

The spec did not settle what a `--conversation` key naming nothing should do.
It is a `*cli.ConversationNotVisibleError`, matching `--seq`, not `--task`'s
empty result: the key is copied from a listing, and on a destructive verb an
empty result reads as "already cleared" — the state the caller is trying to
reach, so the one wrong conclusion the failure produces is the one they are
looking for. A key that names a real conversation which the OTHER axes then
exclude stays an empty result, because that is the AND of the axes and not a
bad argument.
```

- [ ] **Step 2: Walk item 39**

Open the spec's Surfaces matrix and check each row against the code, one row at
a time. A row decided against is struck through in the spec with its reason —
the failure this prevents is a table that keeps asserting something nobody has
looked at since it was written.

- [ ] **Step 3: firing-log entry**

Append one entry listing only the `done` / `omitted` items and the lesson,
following the format of the entries already there. Everything unlisted was
`n/a`.

- [ ] **Step 4: Full verification**

Run: `make check && make test && make vet && make test-integration`
Expected: all green.

- [ ] **Step 5: Commit**

```bash
git add docs/superpowers/specs/ .claude/skills/surface-parity-checklist/firing-log.md
git commit -m "docs: amendment A and the item-39 walk

Co-Authored-By: Claude Opus 5 <noreply@anthropic.com>"
```

---

## Self-review

**Spec coverage.** Every Completion box maps to a task: `ThreadFilter` + flag →
1, 2; verb rows + cases → 4; fan-out → 3; TUI → 7; WebUI + bridge → 8;
`CapDescription` → 6; README + skill → 6; tests 1–7 → 1, 3, 5, 7, 8; item 39 →
9. The spec's Risks need no task: the key-instability risk is mitigated by the
header echo (Task 3, `FanoutThread` returns `Header`) and by `not-found` being
reported (Task 3), both of which exist.

**Placeholders.** Task 5 and Task 7 Step 3 give assertion lists rather than
complete test bodies, because both need the existing e2e / bubbletea harness in
front of them to write against. Flagged rather than hidden: the executor writes
them out in full, and the listed assertions are the acceptance criteria.

**Type consistency.** `ThreadFilter.Conversation` (Task 1) is read by
`SelectThreads` (1), the `board thread` case (2), `FanoutThread` (3) and both
new cases (4) under the same name. `verb.BoardAction.Conversation` is generated
from `Field: "Conversation"` in both places it is declared. `FanoutThread`'s
signature in Task 3's Interfaces matches its call in Task 4. `ThreadOp` values
`ThreadRetract` / `ThreadPurge` are used under those names in Tasks 4, 7 and 8.
Outcome strings are the same six everywhere: `retracted`, `purged`,
`already-withdrawn`, `not-found`, `failed`, `skipped`.
