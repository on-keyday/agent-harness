# Operator Board View — Plan A (server RPC + CLI) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the operator a board-inspection RPC family (list topics / read one topic's messages with content / purge) on the TaskControl plane, plus the `harness-cli board` subcommand that drives it.

**Architecture:** Three new TaskControl verbs handled by the server calling the existing `TaskHandler.Board` methods (`ListTopics`/`ListRetained`/`PurgeTopic`/`PurgeSeq`, all already present). `board_read` streams payloads via a `stream_id` exactly like `get_task_log`. Caps gate centrally via the `requiredCap` map. The CLI is a thin `cli.Client` consumer.

**Tech Stack:** Go; brgen `.bgn` codegen (`make protoregen`); objtrsf/trsf streams; existing `cli.Client.RoundTripTaskControl`.

## Global Constraints

- Spec: `docs/superpowers/specs/2026-06-26-operator-board-inbox-view-design.md` (authoritative wire schema lives there AND in `runner/protocol/message.bgn`).
- Regenerate generated Go ONLY via `make protoregen ARGS='runner/protocol/message.bgn'`; never hand-edit `runner/protocol/message.go`.
- Build/verify with make targets: `go build ./...`, `make vet`, `make test` (NOT bare `go build ./cmd/x` — that drops a binary in the worktree).
- Caps gating is central (`requiredCap` map, task_handler.go:215 → `PermissionDenied`). No per-handler cap checks, no `denied` board status.
- Board has no topic ownership; `board_read`/`board_topics` are global reads gated by `info_global`; `board_purge` by `purge`.
- Work in this worktree on branch `harness/<id>`; land via Mode A FF + `make build` (per landing-to-main + the build-after-landing norm). This plan does NOT land — execution stops at green tests.

---

### Task 1: Wire schema for the three board verbs

**Files:**
- Modify: `runner/protocol/message.bgn` (TaskControlKind enum, request/response match blocks, new formats)
- Regenerate: `runner/protocol/message.go` (via `make protoregen`)
- Test: `runner/protocol/board_view_test.go` (create)

**Interfaces:**
- Produces (generated Go used by every later task): `protocol.TaskControlKind_BoardTopics|BoardRead|BoardPurge`; `protocol.BoardStatus_Ok|BoardStatus_NotFound`; structs `BoardTopicsRequest{RequestId}`, `BoardTopicRow{Name,LastSeq,LastPublishedAtUnixMs,MsgCount}`, `BoardTopicsResponse{RequestId,Topics}`, `BoardReadRequest{RequestId,Topic}`, `BoardMessageRow{Seq,FromTask,FromHostname,ReceivedAtUnixMs,Size}`, `BoardReadResponse{RequestId,Status,StreamId,Msgs}`, `BoardPurgeRequest{RequestId,Topic,Seq}`, `BoardPurgeResponse{RequestId,Status,Purged}`; union accessors `TaskControlRequest.BoardTopics()/SetBoardTopics(...)` etc. and the matching `TaskControlResponse` ones.

- [ ] **Step 1: Add the enum members + formats + match arms to `runner/protocol/message.bgn`**

Append to the `TaskControlKind` enum (after its last member, keeps ordinals stable):

```
    board_topics
    board_read
    board_purge
```

Add these formats near the other TaskControl payload formats (e.g. just after `GetTaskLogResponse`):

```
enum BoardStatus:
    :u8
    ok
    not_found

format BoardTopicsRequest:
    request_id :u32

format BoardTopicRow:
    name_len :u16
    name :[name_len]u8
    last_seq :u64
    last_published_at_unix_ms :u64
    msg_count :u16

format BoardTopicsResponse:
    request_id :u32
    topics_len :u16
    topics :[topics_len]BoardTopicRow

format BoardReadRequest:
    request_id :u32
    topic_len :u16
    topic :[topic_len]u8

format BoardMessageRow:
    seq :u64
    from_task :TaskID
    from_hostname_len :u8
    from_hostname :[from_hostname_len]u8
    received_at_unix_ms :u64
    size :u32

format BoardReadResponse:
    request_id :u32
    status :BoardStatus
    stream_id :u64
    msgs_len :u16
    msgs :[msgs_len]BoardMessageRow

format BoardPurgeRequest:
    request_id :u32
    topic_len :u16
    topic :[topic_len]u8
    seq :u64

format BoardPurgeResponse:
    request_id :u32
    status :BoardStatus
    purged :u16
```

Add to the `TaskControlRequest` match block (alongside the other `TaskControlKind.* => ...` arms):

```
        TaskControlKind.board_topics => board_topics :BoardTopicsRequest
        TaskControlKind.board_read   => board_read   :BoardReadRequest
        TaskControlKind.board_purge  => board_purge  :BoardPurgeRequest
```

Add to the `TaskControlResponse` match block:

```
        TaskControlKind.board_topics => board_topics :BoardTopicsResponse
        TaskControlKind.board_read   => board_read   :BoardReadResponse
        TaskControlKind.board_purge  => board_purge  :BoardPurgeResponse
```

- [ ] **Step 2: Regenerate the Go**

Run: `make protoregen ARGS='runner/protocol/message.bgn'`
Expected: `==> Done. Regenerated: runner/protocol/message.bgn`

- [ ] **Step 3: Verify the new symbols exist**

Run: `grep -nE 'TaskControlKind_BoardRead|BoardReadResponse struct|BoardStatus_NotFound|func .*TaskControlRequest. BoardRead\b' runner/protocol/message.go | head`
Expected: matches for the kind constant, the struct, the status const, and the union accessor.

- [ ] **Step 4: Write a round-trip test**

Create `runner/protocol/board_view_test.go`:

```go
package protocol

import "testing"

func TestBoardReadResponseRoundTrip(t *testing.T) {
	in := BoardReadResponse{RequestId: 7, Status: BoardStatus_Ok, StreamId: 42}
	var ft TaskID
	ft.Id[0] = 0xAB
	row := BoardMessageRow{Seq: 3, FromTask: ft, ReceivedAtUnixMs: 1700, Size: 5}
	row.SetFromHostname([]byte("gmkhost"))
	in.Msgs = append(in.Msgs, row)
	in.MsgsLen = uint16(len(in.Msgs))

	b := in.MustAppend(nil)
	var out BoardReadResponse
	if err := out.DecodeExact(b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.RequestId != 7 || out.Status != BoardStatus_Ok || out.StreamId != 42 || out.MsgsLen != 1 {
		t.Fatalf("header round-trip mismatch: %+v", out)
	}
	if out.Msgs[0].Seq != 3 || out.Msgs[0].Size != 5 || string(out.Msgs[0].FromHostname) != "gmkhost" || out.Msgs[0].FromTask.Id[0] != 0xAB {
		t.Fatalf("row round-trip mismatch: %+v", out.Msgs[0])
	}
}

func TestBoardPurgeRequestRoundTrip(t *testing.T) {
	in := BoardPurgeRequest{RequestId: 1, Seq: 9}
	in.SetTopic([]byte("chat.deadbeef"))
	b := in.MustAppend(nil)
	var out BoardPurgeRequest
	if err := out.DecodeExact(b); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.RequestId != 1 || out.Seq != 9 || string(out.Topic) != "chat.deadbeef" {
		t.Fatalf("round-trip mismatch: %+v", out)
	}
}
```

- [ ] **Step 5: Run the test**

Run: `go test ./runner/protocol/ -run 'TestBoardReadResponseRoundTrip|TestBoardPurgeRequestRoundTrip' -v`
Expected: both PASS.

- [ ] **Step 6: Commit**

```bash
git add runner/protocol/message.bgn runner/protocol/message.go runner/protocol/board_view_test.go
git commit -m "feat(protocol): board_topics/board_read/board_purge TaskControl verbs"
```

---

### Task 2: Server handlers for board_topics + board_purge (inline) + central caps + dispatch

**Files:**
- Create: `server/board_handler.go`
- Modify: `server/capabilities.go` (add 3 entries to `requiredCap`)
- Modify: `server/task_handler.go` (3 dispatch cases in the TaskControl switch)
- Test: `server/board_handler_test.go` (create)

**Interfaces:**
- Consumes: `TaskHandler.Board` (`*agentboard.Board`) — methods `ListTopics() []agentboard.BoardTopicSummary`, `PurgeTopic(name) (purged int, found bool)`, `PurgeSeq(name, seq) (removed, found bool)`; `protocol.*` board types from Task 1; `h.callerCaps`/`hasCap` (server/capabilities.go).
- Produces: `func (h *TaskHandler) handleBoardTopics(conn ConnHandle, requestID uint32)`, `func (h *TaskHandler) handleBoardPurge(conn ConnHandle, requestID uint32, topic string, seq uint64)` (Task 3 adds `handleBoardRead`).

- [ ] **Step 1: Write the failing handler test**

Create `server/board_handler_test.go`. It uses the existing server test harness helpers (`newTestTaskHandlerWithBoard` if present; otherwise mirror `server/capabilities_test.go` which builds a `TaskHandler` with a Board + an operator caller). Use a fake `ConnHandle` that records sent messages (see `server/*_test.go` `recordingConn`/`fakeConn` — reuse whichever exists).

```go
package server

import (
	"testing"

	"github.com/on-keyday/agent-harness/agentboard"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestHandleBoardTopics_ListsTopics(t *testing.T) {
	h, conn := newBoardTestHandler(t) // helper: TaskHandler w/ Board + recording ConnHandle, operator caller
	// seed two topics via the board
	h.Board.Send("chat.aaa", []byte("x"), protocol.RunnerID{}, protocol.TaskID{}, "h")
	h.Board.Send("chat.bbb", []byte("y"), protocol.RunnerID{}, protocol.TaskID{}, "h")

	h.handleBoardTopics(conn, 1)

	resp := conn.lastTaskControlResponse(t)
	if resp.Kind != protocol.TaskControlKind_BoardTopics {
		t.Fatalf("kind = %v", resp.Kind)
	}
	bt := resp.BoardTopics()
	if bt == nil || bt.TopicsLen != 2 {
		t.Fatalf("topics = %+v, want 2", bt)
	}
}

func TestHandleBoardPurge_WholeAndSeq(t *testing.T) {
	h, conn := newBoardTestHandler(t)
	s1, _ := h.Board.Send("chat.p", []byte("a"), protocol.RunnerID{}, protocol.TaskID{}, "h")
	h.Board.Send("chat.p", []byte("b"), protocol.RunnerID{}, protocol.TaskID{}, "h")

	// seq purge drops exactly one
	h.handleBoardPurge(conn, 2, "chat.p", s1)
	r := conn.lastTaskControlResponse(t).BoardPurge()
	if r.Status != protocol.BoardStatus_Ok || r.Purged != 1 {
		t.Fatalf("seq purge = %+v, want ok/1", r)
	}
	// whole purge drops the remainder
	h.handleBoardPurge(conn, 3, "chat.p", 0)
	r = conn.lastTaskControlResponse(t).BoardPurge()
	if r.Status != protocol.BoardStatus_Ok || r.Purged != 1 {
		t.Fatalf("whole purge = %+v, want ok/1", r)
	}
	// unknown topic → not_found
	h.handleBoardPurge(conn, 4, "nope", 0)
	r = conn.lastTaskControlResponse(t).BoardPurge()
	if r.Status != protocol.BoardStatus_NotFound {
		t.Fatalf("unknown purge = %+v, want not_found", r)
	}
	_ = agentboard.RetainedMessage{} // keep import if unused otherwise
}
```

NOTE for the implementer: if no `newBoardTestHandler` / recording-conn helper exists, add a small one in this test file modeled on `server/capabilities_test.go` (which already constructs a `TaskHandler` with caps + a Board) and on the fake `ConnHandle` used there. `lastTaskControlResponse` decodes the last `conn.SendMessage` payload (strip the leading `appwire.AppKind_TaskControl` byte) into a `protocol.TaskControlResponse`.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./server/ -run 'TestHandleBoardTopics_ListsTopics|TestHandleBoardPurge_WholeAndSeq' -v`
Expected: FAIL — `h.handleBoardTopics`/`handleBoardPurge` undefined.

- [ ] **Step 3: Implement the handlers**

Create `server/board_handler.go`:

```go
package server

import (
	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// handleBoardTopics returns the agentboard topic overview (metadata only).
// Cap (info_global) is enforced centrally via requiredCap before dispatch.
func (h *TaskHandler) handleBoardTopics(conn ConnHandle, requestID uint32) {
	out := protocol.BoardTopicsResponse{RequestId: requestID}
	for _, r := range h.Board.ListTopics() {
		row := protocol.BoardTopicRow{
			LastSeq:               r.LastSeq,
			LastPublishedAtUnixMs: uint64(r.LastPublishedAt.UnixMilli()),
		}
		row.SetName([]byte(r.Name))
		if r.MsgCount > 65535 {
			row.MsgCount = 65535
		} else {
			row.MsgCount = uint16(r.MsgCount)
		}
		out.Topics = append(out.Topics, row)
	}
	out.TopicsLen = uint16(len(out.Topics))
	resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_BoardTopics, RequestId: requestID}
	resp.SetBoardTopics(out)
	conn.SendMessage(resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})) //nolint:errcheck
}

// handleBoardPurge drops a topic's ring (seq==0) or a single seq. Cap (purge)
// enforced centrally.
func (h *TaskHandler) handleBoardPurge(conn ConnHandle, requestID uint32, topic string, seq uint64) {
	var status protocol.BoardStatus
	var purged uint16
	if seq == 0 {
		n, found := h.Board.PurgeTopic(topic)
		if !found {
			status = protocol.BoardStatus_NotFound
		} else {
			status = protocol.BoardStatus_Ok
			if n > 65535 {
				purged = 65535
			} else {
				purged = uint16(n)
			}
		}
	} else {
		removed, found := h.Board.PurgeSeq(topic, seq)
		if !found || !removed {
			status = protocol.BoardStatus_NotFound
		} else {
			status = protocol.BoardStatus_Ok
			purged = 1
		}
	}
	out := protocol.BoardPurgeResponse{RequestId: requestID, Status: status, Purged: purged}
	resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_BoardPurge, RequestId: requestID}
	resp.SetBoardPurge(out)
	conn.SendMessage(resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})) //nolint:errcheck
}
```

- [ ] **Step 4: Wire central caps in `server/capabilities.go`**

Add to the `requiredCap` map literal:

```go
	protocol.TaskControlKind_BoardTopics: protocol.Capability_InfoGlobal,
	protocol.TaskControlKind_BoardRead:   protocol.Capability_InfoGlobal,
	protocol.TaskControlKind_BoardPurge:  protocol.Capability_Purge,
```

- [ ] **Step 5: Wire dispatch in `server/task_handler.go`**

In the `switch req.Kind` block (alongside `case protocol.TaskControlKind_GetTaskLog:` etc.), add:

```go
	case protocol.TaskControlKind_BoardTopics:
		h.handleBoardTopics(conn, req.RequestId)
	case protocol.TaskControlKind_BoardPurge:
		if r := req.BoardPurge(); r != nil {
			h.handleBoardPurge(conn, req.RequestId, string(r.Topic), r.Seq)
		}
```

(The `BoardRead` case is added in Task 3.)

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./server/ -run 'TestHandleBoardTopics_ListsTopics|TestHandleBoardPurge_WholeAndSeq' -v`
Expected: PASS.

- [ ] **Step 7: Build + commit**

```bash
go build ./... && go test ./server/ ./runner/protocol/
git add server/board_handler.go server/capabilities.go server/task_handler.go server/board_handler_test.go
git commit -m "feat(server): board_topics + board_purge operator handlers"
```

---

### Task 3: Server board_read handler (streamed payloads)

**Files:**
- Modify: `server/board_handler.go` (add `handleBoardRead`)
- Modify: `server/task_handler.go` (add the `BoardRead` dispatch case)
- Test: `server/board_handler_test.go` (add a streaming test)

**Interfaces:**
- Consumes: `h.Board.ListRetained(topic) ([]agentboard.RetainedMessage, bool)`; `conn.CreateSendStream()` (see `handleGetTaskLog`).
- Produces: `func (h *TaskHandler) handleBoardRead(conn ConnHandle, requestID uint32, topic string)`.

- [ ] **Step 1: Write the failing test**

Add to `server/board_handler_test.go`:

```go
func TestHandleBoardRead_StreamsPayloadsInOrder(t *testing.T) {
	h, conn := newBoardTestHandler(t)
	h.Board.Send("chat.r", []byte("alpha"), protocol.RunnerID{}, protocol.TaskID{}, "h")
	h.Board.Send("chat.r", []byte("bravo"), protocol.RunnerID{}, protocol.TaskID{}, "h")

	h.handleBoardRead(conn, 1, "chat.r")

	resp := conn.lastTaskControlResponse(t)
	br := resp.BoardRead()
	if br == nil || br.Status != protocol.BoardStatus_Ok || br.MsgsLen != 2 {
		t.Fatalf("board_read resp = %+v, want ok/2", br)
	}
	if br.Msgs[0].Size != 5 || br.Msgs[1].Size != 5 {
		t.Fatalf("sizes = %d,%d want 5,5", br.Msgs[0].Size, br.Msgs[1].Size)
	}
	// The recording conn captures the send-stream bytes; concatenation is row order.
	got := conn.sendStreamBytes(t, br.StreamId)
	if string(got) != "alphabravo" {
		t.Fatalf("stream payload = %q, want alphabravo", got)
	}

	// unknown topic → not_found, stream_id 0
	h.handleBoardRead(conn, 2, "nope")
	br = conn.lastTaskControlResponse(t).BoardRead()
	if br.Status != protocol.BoardStatus_NotFound || br.StreamId != 0 {
		t.Fatalf("unknown read = %+v, want not_found/0", br)
	}
}
```

NOTE: extend the test's fake `ConnHandle` so `CreateSendStream()` returns a fake stream that buffers writes, and add `sendStreamBytes(streamID)` to read them back. Model the fake on how `handleGetTaskLog`'s stream is exercised in existing server tests if such a test exists; otherwise a minimal in-memory `trsf`-like buffer suffices (the handler only needs `ID()`, `Write`/`WriteDirect`, and `Close`).

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./server/ -run TestHandleBoardRead_StreamsPayloadsInOrder -v`
Expected: FAIL — `handleBoardRead` undefined.

- [ ] **Step 3: Implement `handleBoardRead`**

Add to `server/board_handler.go` (mirror `handleGetTaskLog`'s respond-then-stream shape):

```go
func (h *TaskHandler) handleBoardRead(conn ConnHandle, requestID uint32, topic string) {
	respond := func(status protocol.BoardStatus, streamID uint64, rows []protocol.BoardMessageRow) {
		out := protocol.BoardReadResponse{RequestId: requestID, Status: status, StreamId: streamID}
		out.Msgs = rows
		out.MsgsLen = uint16(len(rows))
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_BoardRead, RequestId: requestID}
		resp.SetBoardRead(out)
		conn.SendMessage(resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})) //nolint:errcheck
	}

	msgs, found := h.Board.ListRetained(topic)
	if !found {
		respond(protocol.BoardStatus_NotFound, 0, nil)
		return
	}
	rows := make([]protocol.BoardMessageRow, 0, len(msgs))
	payloads := make([][]byte, 0, len(msgs))
	for _, m := range msgs {
		size := len(m.Payload)
		if size > 0xffffffff {
			size = 0xffffffff
		}
		row := protocol.BoardMessageRow{
			Seq:              m.Seq,
			ReceivedAtUnixMs: uint64(m.ReceivedAt.UnixMilli()),
			Size:             uint32(size),
		}
		copy(row.FromTask.Id[:], m.FromTask.Id[:])
		row.SetFromHostname([]byte(m.FromHostname))
		rows = append(rows, row)
		payloads = append(payloads, m.Payload)
	}
	if len(rows) == 0 {
		respond(protocol.BoardStatus_Ok, 0, rows)
		return
	}
	stream := conn.CreateSendStream()
	if stream == nil {
		// Non-streaming connection (test/degraded): metadata only, no stream.
		respond(protocol.BoardStatus_Ok, 0, rows)
		return
	}
	respond(protocol.BoardStatus_Ok, uint64(stream.ID()), rows)
	go func() {
		defer stream.Close()
		for _, p := range payloads {
			if len(p) > 0 {
				_ = writeStreamAll(stream, p) // helper mirroring handleGetTaskLog's write loop
			}
		}
	}()
}
```

If a `writeStreamAll(stream, []byte) error` helper does not already exist next to `handleGetTaskLog`, add it there (extract the existing write loop) and reuse it here — do not duplicate the loop.

- [ ] **Step 4: Wire the dispatch case in `server/task_handler.go`**

```go
	case protocol.TaskControlKind_BoardRead:
		if r := req.BoardRead(); r != nil {
			h.handleBoardRead(conn, req.RequestId, string(r.Topic))
		}
```

- [ ] **Step 5: Run to verify it passes**

Run: `go test ./server/ -run TestHandleBoardRead_StreamsPayloadsInOrder -v`
Expected: PASS.

- [ ] **Step 6: Build + commit**

```bash
go build ./... && go test ./server/
git add server/board_handler.go server/task_handler.go server/board_handler_test.go cli/get_log.go
git commit -m "feat(server): board_read streams retained payloads (get_task_log pattern)"
```

---

### Task 4: CLI client helpers (`cli/board.go`) + e2e

**Files:**
- Create: `cli/board.go`
- Test: `cli/board_e2e_test.go` (create)

**Interfaces:**
- Consumes: `(*Client).RoundTripTaskControl`, `c.Transport()`, `waitForReceiveStream` (cli/get_log.go), `trsf.StreamID`, the protocol board types.
- Produces:
  - `type BoardTopicRow struct { Name string; LastSeq uint64; LastPublishedAtMs uint64; MsgCount int }`
  - `type BoardMessage struct { Seq uint64; FromTaskHex string; FromHostname string; ReceivedAtMs uint64; Payload []byte }`
  - `func (c *Client) BoardTopics(ctx) ([]BoardTopicRow, error)`
  - `func (c *Client) BoardRead(ctx, topic string) ([]BoardMessage, bool, error)` — bool=found
  - `func (c *Client) BoardPurge(ctx, topic string, seq uint64) (purged int, found bool, err error)`
  - package-level `BoardTopics/BoardRead/BoardPurge(ctx, peerCID, ...)` fresh-dial wrappers (mirror cli/cancel.go).

- [ ] **Step 1: Write the failing e2e test**

Create `cli/board_e2e_test.go`. Reuse the operator-side e2e harness used by other `cli` tests that need a live server (look for `startServerE2E`/`freePortE2E` patterns under `cli/` or `cli/agent/`; the `cli` operator tests dial with `protocol.ClientKind_Cli`). Seed topics by calling `board.Send` directly on the test server's Board (as `cli/agent/purge_e2e_test.go` does), then exercise the operator helpers.

```go
package cli_test

import (
	"context"
	"testing"
	"time"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestClientBoard_TopicsReadPurge(t *testing.T) {
	srv, peerCID := startOperatorServerE2E(t) // helper: in-proc server + a Board, returns dial CID
	srv.Board().Send("chat.x", []byte("hello"), protocol.RunnerID{}, protocol.TaskID{}, "h")
	srv.Board().Send("chat.x", []byte("world"), protocol.RunnerID{}, protocol.TaskID{}, "h")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := cli.Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil { t.Fatal(err) }
	defer c.Close()

	topics, err := c.BoardTopics(ctx)
	if err != nil { t.Fatal(err) }
	if len(topics) != 1 || topics[0].Name != "chat.x" || topics[0].MsgCount != 2 {
		t.Fatalf("topics = %+v", topics)
	}

	msgs, found, err := c.BoardRead(ctx, "chat.x")
	if err != nil || !found || len(msgs) != 2 {
		t.Fatalf("read = (%d msgs, found=%v, err=%v)", len(msgs), found, err)
	}
	if string(msgs[0].Payload) != "hello" || string(msgs[1].Payload) != "world" {
		t.Fatalf("payloads = %q,%q", msgs[0].Payload, msgs[1].Payload)
	}

	purged, found, err := c.BoardPurge(ctx, "chat.x", msgs[0].Seq)
	if err != nil || !found || purged != 1 {
		t.Fatalf("seq purge = (%d, found=%v, err=%v)", purged, found, err)
	}
	purged, found, _ = c.BoardPurge(ctx, "chat.x", 0)
	if !found || purged != 1 {
		t.Fatalf("whole purge = (%d, found=%v)", purged, found)
	}
}
```

NOTE: if no operator-side in-proc server e2e helper exists under `cli/`, add `startOperatorServerE2E` modeled on `cli/agent`'s `startServerE2E` but exposing the server's `*agentboard.Board` (add a `Board()` accessor on the test server if needed) and returning a CID dialable as `ClientKind_Cli`.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./cli/ -run TestClientBoard_TopicsReadPurge -v`
Expected: FAIL — `c.BoardTopics` undefined.

- [ ] **Step 3: Implement `cli/board.go`**

```go
package cli

import (
	"context"
	"encoding/hex"
	"fmt"

	"github.com/on-keyday/objtrsf/objproto"
	"github.com/on-keyday/objtrsf/trsf"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

type BoardTopicRow struct {
	Name              string
	LastSeq           uint64
	LastPublishedAtMs uint64
	MsgCount          int
}

type BoardMessage struct {
	Seq          uint64
	FromTaskHex  string
	FromHostname string
	ReceivedAtMs uint64
	Payload      []byte
}

func (c *Client) BoardTopics(ctx context.Context) ([]BoardTopicRow, error) {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_BoardTopics}
	req.SetBoardTopics(protocol.BoardTopicsRequest{})
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, err
	}
	bt := resp.BoardTopics()
	if bt == nil || resp.Kind != protocol.TaskControlKind_BoardTopics {
		return nil, fmt.Errorf("unexpected response kind=%v", resp.Kind)
	}
	out := make([]BoardTopicRow, 0, len(bt.Topics))
	for _, r := range bt.Topics {
		out = append(out, BoardTopicRow{
			Name:              string(r.Name),
			LastSeq:           r.LastSeq,
			LastPublishedAtMs: r.LastPublishedAtUnixMs,
			MsgCount:          int(r.MsgCount),
		})
	}
	return out, nil
}

func (c *Client) BoardRead(ctx context.Context, topic string) ([]BoardMessage, bool, error) {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_BoardRead}
	rr := protocol.BoardReadRequest{}
	rr.SetTopic([]byte(topic))
	req.SetBoardRead(rr)
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, false, err
	}
	br := resp.BoardRead()
	if br == nil || resp.Kind != protocol.TaskControlKind_BoardRead {
		return nil, false, fmt.Errorf("unexpected response kind=%v", resp.Kind)
	}
	if br.Status == protocol.BoardStatus_NotFound {
		return nil, false, nil
	}
	rows := make([]BoardMessage, len(br.Msgs))
	total := 0
	for i, m := range br.Msgs {
		rows[i] = BoardMessage{
			Seq:          m.Seq,
			FromTaskHex:  hex.EncodeToString(m.FromTask.Id[:]),
			FromHostname: string(m.FromHostname),
			ReceivedAtMs: m.ReceivedAtUnixMs,
		}
		total += int(m.Size)
	}
	if br.StreamId != 0 && total > 0 {
		st := waitForReceiveStream(ctx, c.Transport(), trsf.StreamID(br.StreamId))
		if st == nil {
			return nil, true, fmt.Errorf("board_read stream %d not visible", br.StreamId)
		}
		buf := make([]byte, 0, total)
		for {
			select {
			case <-ctx.Done():
				return nil, true, ctx.Err()
			default:
			}
			data, eof, err := st.ReadDirect(64 * 1024)
			if err != nil {
				return nil, true, err
			}
			buf = append(buf, data...)
			if eof {
				break
			}
		}
		// Slice the concatenated stream by each row's size, in order.
		off := 0
		for i := range rows {
			n := int(br.Msgs[i].Size)
			if off+n > len(buf) {
				n = len(buf) - off
			}
			rows[i].Payload = append([]byte(nil), buf[off:off+n]...)
			off += n
		}
	}
	return rows, true, nil
}

func (c *Client) BoardPurge(ctx context.Context, topic string, seq uint64) (int, bool, error) {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_BoardPurge}
	pr := protocol.BoardPurgeRequest{Seq: seq}
	pr.SetTopic([]byte(topic))
	req.SetBoardPurge(pr)
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return 0, false, err
	}
	bp := resp.BoardPurge()
	if bp == nil || resp.Kind != protocol.TaskControlKind_BoardPurge {
		return 0, false, fmt.Errorf("unexpected response kind=%v", resp.Kind)
	}
	if bp.Status == protocol.BoardStatus_NotFound {
		return 0, false, nil
	}
	return int(bp.Purged), true, nil
}

// Package-level fresh-dial wrappers (mirror cli/cancel.go) for harness-cli.
func BoardTopics(ctx context.Context, peerCID objproto.ConnectionID) ([]BoardTopicRow, error) {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil { return nil, err }
	defer c.Close()
	return c.BoardTopics(ctx)
}
func BoardRead(ctx context.Context, peerCID objproto.ConnectionID, topic string) ([]BoardMessage, bool, error) {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil { return nil, false, err }
	defer c.Close()
	return c.BoardRead(ctx, topic)
}
func BoardPurge(ctx context.Context, peerCID objproto.ConnectionID, topic string, seq uint64) (int, bool, error) {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil { return 0, false, err }
	defer c.Close()
	return c.BoardPurge(ctx, topic, seq)
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./cli/ -run TestClientBoard_TopicsReadPurge -v`
Expected: PASS.

- [ ] **Step 5: Build + commit**

```bash
go build ./... && go test ./cli/
git add cli/board.go cli/board_e2e_test.go
git commit -m "feat(cli): Board topics/read/purge client helpers"
```

---

### Task 5: `harness-cli board` subcommand

**Files:**
- Create: `cli/cmd_board.go` (subcommand handlers; keep `cmd/harness-cli/main.go` thin) OR add directly under `cmd/harness-cli/` following how other subcommands are organized — match the existing layout.
- Modify: `cmd/harness-cli/main.go` (add the `board` top-level case + usage line)
- Test: covered by Task 4's e2e (the helpers); add a thin output-format unit test if a formatting helper is extracted.

**Interfaces:**
- Consumes: `cli.BoardTopics/BoardRead/BoardPurge` (package-level wrappers), the server CID resolver used by other subcommands (e.g. `resolveServerCID()` / the same env path `ls`/`cancel` use).
- Produces: `harness-cli board topics|read <topic>|purge <topic> [--seq N]`.

- [ ] **Step 1: Add the dispatch + handlers**

In `cmd/harness-cli/main.go`, add a top-level `case "board":` mirroring how `case "agent":` / `case "file":` parse a sub-subcommand. Resolve the server CID the same way sibling operator subcommands (`ls`, `cancel`) do. Implement:

- `board topics` → `cli.BoardTopics(ctx, cid)`; print one line per topic: `name  msgs=<n>  last_seq=<s>  last=<rfc3339 from ms>`.
- `board read <topic>` → `cli.BoardRead(ctx, cid, topic)`; if `!found` print nothing (exit 0); else per message print a header line (`#<seq> from=<FromTaskHex> host=<h> size=<n> at=<rfc3339>`) followed by the payload decoded as UTF-8, pretty-printed if it is valid JSON (`json.Indent`).
- `board purge <topic> [--seq N]` → `cli.BoardPurge(ctx, cid, topic, seq)`; print `{"status":"ok|not_found","topic":..,"purged":N}`.

Use a `flag.FlagSet` for `--seq` (default 0) on the `purge` sub-subcommand, matching how `agent purge` parses it.

- [ ] **Step 2: Add the usage line**

In the top-level usage printer, add:

```
  board topics|read <topic>|purge <topic> [--seq N]   inspect/purge the agentboard (cap: info_global; purge: purge)
```

- [ ] **Step 3: Build and smoke-run against a live server**

Run: `go build ./... && ./... ` then manually (with a live server) `harness-cli board topics` and `harness-cli board read <topic>`.
Expected: compiles; `board topics` lists topics; `board read` prints decoded content.
(Automated coverage is Task 4's e2e on the helpers; the subcommand is a thin shell over them.)

- [ ] **Step 4: Commit**

```bash
go build ./... && make vet
git add cmd/harness-cli/main.go cli/cmd_board.go
git commit -m "feat(cli): harness-cli board topics/read/purge subcommand"
```

---

## Self-Review

**Spec coverage:**
- Topic list → Task 1 (schema) + Task 2 (`handleBoardTopics`) + Task 4/5 (CLI). ✓
- Read content (streamed) → Task 1 + Task 3 + Task 4 (stream slice) + Task 5. ✓
- Purge (whole + seq) → Task 1 + Task 2 + Task 4/5. ✓
- Caps central (info_global/purge) → Task 2 Step 4. ✓
- `get_task_log` streaming pattern → Task 3. ✓
- All-3-UIs: CLI is this plan; **TUI and WebUI are deliberately separate follow-on plans** (Plan B / Plan C), each building on these RPCs. Noted as a gap to be filled by those plans, not this one.
- Topic→task client mapping: deferred to the TUI/WebUI plans (the CLI prints `FromTaskHex` raw; label enrichment needs the task list, which the UIs already hold). Documented in Task 5.

**Placeholder scan:** No TBD/TODO. The two NOTEs (test-harness helpers in Task 2/3/4) point the implementer at existing sibling helpers to reuse or mirror — they are explicit, not vague. Exact code is given for all production files.

**Type consistency:** `BoardMessage`/`BoardTopicRow` (cli) vs `BoardMessageRow`/`BoardTopicRow` (protocol) are distinct names by design (wire vs domain). `BoardStatus_Ok|NotFound`, `Purged`, `StreamId`, `SetBoardTopics/BoardRead/BoardPurge` used consistently across tasks.

## Follow-on plans (not this plan)

- **Plan B — TUI board view:** new view listing topics → drill into messages with content + purge keybinding; threads `a.client`; topic→task label via the task list the TUI already polls. Requires exploring the TUI view/model structure first.
- **Plan C — WebUI board panel:** dark-themed, `<=390px`, topic list → message cards with content + purge buttons; uses `currentClient()`; Playwright verification (desktop + 390px). Requires exploring the WebUI panel/wasm structure first.
