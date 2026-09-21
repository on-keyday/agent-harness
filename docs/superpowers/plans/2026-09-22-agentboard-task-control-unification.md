# Agentboard → task-control unification Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Delete the second description of the agentboard from the wire — fold
the twelve `AgentMessageKind` verbs into `TaskControlKind`, merge the two that
have operator twins, and retire `AppKind_AgentMessage`.

**Architecture:** The schema lands first and complete: every kind, every format,
nothing dispatching them. Server and client then migrate one verb group per
task with both frame families alive inside the branch, each task leaving its
verbs wired at every call site. The old family is deleted last, in one task.

**Tech Stack:** Go, `.bgn` schemas via ebm2go, trsf/objproto transport.

**Spec:** `docs/superpowers/specs/2026-09-22-agentboard-task-control-unification-design.md`

## Global Constraints

These bind every task. They are stated once here, not re-derived per verb
(`feedback_carry_invariants_across_surfaces`: a plan with N surfaces over one
primitive states the primitive's rules once).

**C1 — the schema is written ONCE, in Task 1, complete.** No later task edits
`runner/protocol/message.bgn` or any `.bgn`. If a later task finds a missing
field, go back and fix Task 1, never patch forward
(`feedback_no_split_schemas`).

**C2 — the server-side verb shape.** Every migrated agent verb becomes a case
in `TaskHandler.HandleTaskControl`'s switch (`server/task_handler.go`), and
every one of them needs the caller's board identity, which lives on the
`Server`, not the `TaskHandler`. Reach it through ONE new hook field, wired
once in Task 2 and used unchanged by every later task:

```go
// BoardConnState resolves the agentboard ConnState for a connection, or nil
// when that connection never completed an agent ClientHello. Wired by
// Server.New to s.getOrCreateAgentConn(conn).state. The TaskHandler holds no
// *Server by design -- every other cross-boundary need on this struct is a
// function field (OnAgentHello, ConnListFn, DropConnsForPrincipal), and this
// follows them.
BoardConnState func(conn ConnHandle) *agentboard.ConnState
```

A handler whose `BoardConnState(conn)` returns nil answers the verb's own
"nothing here" status, never a panic and never a capability error: the caller
is a connection that is not an agent, and telling it which capability it lacks
would be false.

**C3 — the client-side verb shape.** Every migrated subcommand in `cli/agent/`
builds a `protocol.TaskControlRequest`, calls `c.RoundTripTaskControl(ctx,
req)`, and checks `resp.Kind` before reading the variant — the pattern
`cli/board.go:307-325` already uses:

```go
req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AgentSubscribe}
sr := protocol.AgentSubscribeRequest{}
sr.SetPattern([]byte(pattern))
req.SetAgentSubscribe(sr)

resp, err := c.RoundTripTaskControl(ctx, req)
if err != nil {
    return err
}
if resp.Kind == protocol.TaskControlKind_PermissionDenied {
    return permissionDeniedError(resp.PermissionDenied())
}
out := resp.AgentSubscribe()
if out == nil || resp.Kind != protocol.TaskControlKind_AgentSubscribe {
    return fmt.Errorf("AgentSubscribe: unexpected response kind=%v", resp.Kind)
}
```

**C4 — no verb is half-wired at any commit.** Before committing a task, grep
every construction site of the request you touched and confirm each sets what
its flags parsed (`feedback_enumerate_all_callsites_when_intercepting`: a
global interceptor with partial per-site state is the tell;
surface-parity item 28a). `grep -rn 'AgentSendRequest{' cli/` and diff the
count against the subcommands that build one.

**C5 — output is frozen.** Every subcommand's stdout, JSON field names and
exit codes stay byte-identical, except a capability denial (which gains the
bit's name from `required_cap`). Task 2 captures the baseline; every later
task diffs against it.

**C6 — build hygiene.** Compile-check with `go build ./...` or `go vet ./...`.
Never a bare `go build ./cmd/<x>/` — it drops a binary in the worktree.
Verification uses `make` targets, not ad-hoc `go build`
(`feedback_verify_with_make_targets_not_adhoc`).

**C7 — regen churn is not review surface.** Regenerating
`runner/protocol/message.go` moves tens of thousands of generated lines.
Review the `.bgn` diff and the hand-written call sites; verify the generated
half by build/vet/test and by checking the new symbols exist
(`feedback_bgn_is_defined_over_handwritten_switch`).

---

### Task 1: The schema, whole

**Files:**
- Modify: `runner/protocol/message.bgn`
- Regenerate: `runner/protocol/message.go`

**Interfaces:**
- Produces: twelve `protocol.TaskControlKind_Agent*` / `Board*` values and the
  request/response formats named below. Every later task consumes these.

- [ ] **Step 1: Append twelve values to `TaskControlKind`**

The enum is a positional `:u8` with 34 values. Append at the END — an
insertion renumbers every later value and a skewed peer decodes a different
verb, silently.

```
    # --- agentboard, agent face (folded in from AgentMessageKind) ---
    # Each verb below is keyed to the CALLER's own subscriptions or to a
    # message the caller published, and takes no capability. A board verb that
    # reaches topics the caller need not subscribe to is a board_* kind and
    # takes one; the two agent verbs that did (list_topics, purge) are gone,
    # merged into board_topics and board_purge.
    agent_send
    agent_subscribe
    agent_unsubscribe
    agent_list_subscriptions
    agent_wait
    agent_inbox
    agent_inbox_advance
    agent_list_retained
    agent_read_seq
    agent_retract
```

- [ ] **Step 2: Move the formats, verbatim except as listed**

Copy each body from `agentboard/agentboard.bgn` unchanged, including its
comments, renaming the format and retyping the two identity fields. The bodies
are long and load-bearing (the `no_retire_on_reply` negative-bit rationale, the
`delivered_to` enumeration argument, the `reply_to_topic` precedence note);
they move as written.

| from `agentboard.bgn` | to `message.bgn` | edits |
|---|---|---|
| `SendRequest` | `AgentSendRequest` | none |
| `SendStatus` | `SendStatus` | none |
| `SendResponse` | `AgentSendResponse` | none |
| `SubscribeRequest` | `AgentSubscribeRequest` | none |
| `UnsubscribeRequest` | `AgentUnsubscribeRequest` | none |
| `SubscribeStatus` | `SubscribeStatus` | none |
| `SubscribeResponse` | `AgentSubscribeResponse` | none |
| `DeliveredMessage` | `DeliveredMessage` | `from_runner_id :RunnerID` and `from_task_id :TaskID` now resolve to protocol's own — no text change, the local copies are deleted |
| `WaitRequest` | `AgentWaitRequest` | none |
| `WaitResponse` | `AgentWaitResponse` | none |
| `InboxRequest` | `AgentInboxRequest` | none |
| `InboxResponse` | `AgentInboxResponse` | none |
| `InboxAdvanceRequest` | `AgentInboxAdvanceRequest` | none |
| `InboxAdvanceResponse` | `AgentInboxAdvanceResponse` | none |
| `ListSubscriptionsRequest` | `AgentListSubscriptionsRequest` | none |
| `SubscriptionSummary` | `AgentSubscriptionSummary` | none |
| `ListSubscriptionsResponse` | `AgentListSubscriptionsResponse` | none |
| `ListRetainedRequest` | `AgentListRetainedRequest` | none |
| `RetainedMeta` | `RetainedMeta` | `from_runner :RunnerID`, `from_task :TaskID` as above |
| `ListRetainedResponse` | `AgentListRetainedResponse` | **`status :PurgeStatus` → `status :BoardStatus`** — `PurgeStatus` is deleted and `BoardStatus` already carries exactly `ok` / `not_found` |
| `ReadSeqRequest` | `AgentReadSeqRequest` | none |
| `ReadSeqStatus` | `AgentReadSeqStatus` | none |
| `ReadSeqResponse` | `AgentReadSeqResponse` | none |
| `RetractRequest` | `AgentRetractRequest` | none |
| `RetractStatus` | `AgentRetractStatus` | none |
| `RetractResponse` | `AgentRetractResponse` | none |

**NOT moved — deleted with the old schema in Task 11:** `RunnerID`, `TaskID`
(protocol's are used), `HelloStatus` (`protocol.ClientHelloStatus` is used),
`AgentMessageKind`, `AgentMessage`, `ListTopicsRequest`, `TopicSummary`,
`ListTopicsStatus`, `ListTopicsResponse`, `PurgeRequest`, `PurgeStatus`,
`PurgeResponse`.

- [ ] **Step 3: Add the request match arms**

In `format TaskControlRequest`'s `match kind:`, before the `.. => error(...)`
line:

```
        TaskControlKind.agent_send               => agent_send               :AgentSendRequest
        TaskControlKind.agent_subscribe          => agent_subscribe          :AgentSubscribeRequest
        TaskControlKind.agent_unsubscribe        => agent_unsubscribe        :AgentUnsubscribeRequest
        TaskControlKind.agent_list_subscriptions => agent_list_subscriptions :AgentListSubscriptionsRequest
        TaskControlKind.agent_wait               => agent_wait               :AgentWaitRequest
        TaskControlKind.agent_inbox              => agent_inbox              :AgentInboxRequest
        TaskControlKind.agent_inbox_advance      => agent_inbox_advance      :AgentInboxAdvanceRequest
        TaskControlKind.agent_list_retained      => agent_list_retained      :AgentListRetainedRequest
        TaskControlKind.agent_read_seq           => agent_read_seq           :AgentReadSeqRequest
        TaskControlKind.agent_retract            => agent_retract            :AgentRetractRequest
```

- [ ] **Step 4: Add the response match arms**

In `format TaskControlResponse`'s `match kind:`:

```
        TaskControlKind.agent_send               => agent_send               :AgentSendResponse
        TaskControlKind.agent_subscribe          => agent_subscribe          :AgentSubscribeResponse
        TaskControlKind.agent_unsubscribe        => agent_unsubscribe        :AgentSubscribeResponse
        TaskControlKind.agent_list_subscriptions => agent_list_subscriptions :AgentListSubscriptionsResponse
        TaskControlKind.agent_wait               => agent_wait               :AgentWaitResponse
        TaskControlKind.agent_inbox              => agent_inbox              :AgentInboxResponse
        TaskControlKind.agent_inbox_advance      => agent_inbox_advance      :AgentInboxAdvanceResponse
        TaskControlKind.agent_list_retained      => agent_list_retained      :AgentListRetainedResponse
        TaskControlKind.agent_read_seq           => agent_read_seq           :AgentReadSeqResponse
        TaskControlKind.agent_retract            => agent_retract            :AgentRetractResponse
```

`agent_unsubscribe` answers with `AgentSubscribeResponse` because the two
verbs share one status enum today (`SubscribeResponse` serves both), and
inventing a second identical format would be a format to keep in step for no
reader's benefit.

- [ ] **Step 5: Regenerate and build**

Run: `make gen` (or the repo's generator target for `runner/protocol`), then
`go build ./...`
Expected: green. Nothing dispatches the new kinds yet, so no behaviour changes.

- [ ] **Step 6: Assert the new symbols exist**

Run: `go doc ./runner/protocol AgentSendRequest` and
`go doc ./runner/protocol TaskControlKind`
Expected: `AgentSendRequest` is a struct; the kind list shows the ten new
values at the end with ordinals 34-43.

- [ ] **Step 7: Commit**

```bash
git add runner/protocol/message.bgn runner/protocol/message.go
git commit -m "feat(schema): task-control kinds and formats for the agent board face"
```

---

### Task 2: The identity hook, and the baseline capture

**Files:**
- Modify: `server/task_handler.go` (TaskHandler struct), `server/server.go` (wiring)
- Create: `docs/superpowers/plans/.baseline/` is NOT used — the capture is a
  scratch file outside the repo (see Step 1)

**Interfaces:**
- Produces: `TaskHandler.BoardConnState` (signature in C2), consumed by Tasks 3-10.

- [ ] **Step 1: Capture the frozen output baseline (C5)**

Bring up a dummy harness and record every agent subcommand's output to the
scratchpad — not the repo.

```bash
scripts/dummy-harness.sh up
# from inside a task on that instance, for each verb:
harness-cli agent subscribe --topic chat.deadbeef
harness-cli agent subscriptions
harness-cli agent send --topic chat.deadbeef --data 'hello'
harness-cli agent inbox --json
harness-cli agent retained --topic chat.deadbeef
harness-cli agent read <seq>
harness-cli agent thread --json
harness-cli agent retract <seq>
harness-cli agent topics
harness-cli agent purge --topic chat.deadbeef
harness-cli agent unsubscribe --topic chat.deadbeef
```

Redirect each to `$SCRATCH/baseline/<verb>.txt` with stderr kept
(`feedback_never_suppress_stderr_when_driving`) and the exit code appended.

- [ ] **Step 2: Add the hook field**

In `TaskHandler`'s struct, beside `OnAgentHello` (its closest sibling — also
an agent-identity hook), add the field exactly as written in C2.

- [ ] **Step 3: Wire it in `Server.New`**

In the `s.taskHandler = &TaskHandler{...}` literal (`server/server.go:254`),
beside the existing `OnAgentHello` closure:

```go
BoardConnState: func(conn ConnHandle) *agentboard.ConnState {
    ac := s.getOrCreateAgentConn(conn)
    if ac == nil || !ac.helloed {
        return nil
    }
    return ac.state
},
```

- [ ] **Step 4: Build**

Run: `go build ./... && go vet ./server/`
Expected: green. The field is unused so far; Go permits an unused struct field.

- [ ] **Step 5: Commit**

```bash
git add server/task_handler.go server/server.go
git commit -m "feat(server): reach the board ConnState from the task-control handler"
```

---

### Task 3: subscribe / unsubscribe / list_subscriptions

The simplest group: no payload streams, no capability, no long-poll. It sets
the shape every later task follows.

**Files:**
- Modify: `server/task_handler.go` (dispatch switch), `server/agent_handler.go`
  (the three handlers gain task-control entry points)
- Modify: `cli/agent/subscribe.go`, `cli/agent/subscriptions.go`
- Test: `server/agent_taskcontrol_test.go` (create)

- [ ] **Step 1: Write the failing test**

```go
// TestAgentSubscribeOverTaskControl asserts the task-control path registers
// the same subscription the AgentMessage path does. Both are alive during the
// migration, so the test asserts on the BOARD, not on which frame was used.
func TestAgentSubscribeOverTaskControl(t *testing.T) {
    h, conn, tid := newAgentTaskHandler(t) // helper: handler + helloed agent conn
    req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AgentSubscribe, RequestId: 7}
    sr := protocol.AgentSubscribeRequest{}
    sr.SetPattern([]byte("chat.abc"))
    req.SetAgentSubscribe(sr)

    resp := roundTrip(t, h, conn, req)
    if resp.Kind != protocol.TaskControlKind_AgentSubscribe {
        t.Fatalf("kind = %v, want agent_subscribe", resp.Kind)
    }
    if got := resp.AgentSubscribe(); got == nil || got.Status != protocol.SubscribeStatus_Ok {
        t.Fatalf("status = %+v, want ok", got)
    }
    if !h.Board.Subscribes(stateFor(h, conn), "chat.abc") {
        t.Error("the board does not hold the subscription the request asked for")
    }
    _ = tid
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./server/ -run TestAgentSubscribeOverTaskControl -v`
Expected: FAIL — the dispatch switch has no `agent_subscribe` case, so the
request is answered with nothing and `roundTrip` times out or returns a zero
response.

- [ ] **Step 3: Add the three dispatch cases**

In `TaskHandler.HandleTaskControl`'s switch, beside the `board_*` cases:

```go
case protocol.TaskControlKind_AgentSubscribe:
    r := req.AgentSubscribe()
    if r == nil {
        slog.Error("TaskHandler: AgentSubscribe variant is nil")
        return
    }
    h.handleAgentSubscribe(conn, req.RequestId, string(r.Pattern), false)

case protocol.TaskControlKind_AgentUnsubscribe:
    r := req.AgentUnsubscribe()
    if r == nil {
        slog.Error("TaskHandler: AgentUnsubscribe variant is nil")
        return
    }
    h.handleAgentSubscribe(conn, req.RequestId, string(r.Pattern), true)

case protocol.TaskControlKind_AgentListSubscriptions:
    if req.AgentListSubscriptions() == nil {
        slog.Error("TaskHandler: AgentListSubscriptions variant is nil")
        return
    }
    h.handleAgentListSubscriptions(conn, req.RequestId)
```

- [ ] **Step 4: Write the handlers**

New file `server/agent_taskcontrol.go`, so the migrated handlers sit together
and the file being emptied stays readable as it shrinks:

```go
// handleAgentSubscribe registers or removes one subscription for the calling
// agent. remove picks which; the two verbs differ in nothing else and share a
// response format.
//
// A caller with no board identity gets bad_pattern rather than a capability
// error: it holds no wrong capability, it is simply not an agent.
func (h *TaskHandler) handleAgentSubscribe(conn ConnHandle, requestID uint32, pattern string, remove bool) {
    out := protocol.AgentSubscribeResponse{RequestId: requestID, Status: protocol.SubscribeStatus_Ok}
    st := h.boardState(conn)
    if st == nil || pattern == "" {
        out.Status = protocol.SubscribeStatus_BadPattern
    } else if remove {
        h.Board.Unsubscribe(st, pattern)
    } else {
        h.Board.Subscribe(st, pattern)
    }
    kind := protocol.TaskControlKind_AgentSubscribe
    if remove {
        kind = protocol.TaskControlKind_AgentUnsubscribe
    }
    resp := protocol.TaskControlResponse{Kind: kind, RequestId: requestID}
    resp.SetAgentSubscribe(out)
    conn.SendMessage(resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})) //nolint:errcheck
}

// boardState is the nil-safe reader for the C2 hook. Every migrated handler
// goes through it so "the hook is not wired" has one answer, not ten.
func (h *TaskHandler) boardState(conn ConnHandle) *agentboard.ConnState {
    if h.BoardConnState == nil {
        return nil
    }
    return h.BoardConnState(conn)
}
```

`handleAgentListSubscriptions` follows the same shape, reading
`h.Board.Subscriptions(st)` into `AgentSubscriptionSummary` rows and answering
an empty list when `st` is nil.

- [ ] **Step 5: Run the test**

Run: `go test ./server/ -run TestAgentSubscribe -v`
Expected: PASS

- [ ] **Step 6: Switch the two client subcommands**

`cli/agent/subscribe.go` and `cli/agent/subscriptions.go` build a
`TaskControlRequest` per C3 against the long-lived client. Delete their
`SetOnControl` callbacks and their `agentboard.AgentMessage` construction. The
flags, the JSON emitted and the exit codes do not change.

- [ ] **Step 7: Verify output against the baseline (C5)**

Run the three subcommands against the dummy harness and `diff` each against
`$SCRATCH/baseline/`.
Expected: no differences.

- [ ] **Step 8: Check for half-wiring (C4)**

Run: `grep -rn 'AgentSubscribeRequest{\|AgentListSubscriptionsRequest{' cli/`
Expected: exactly the sites the two subcommands own, each setting `Pattern`
where its flag parsed one.

- [ ] **Step 9: Commit**

```bash
git add server/agent_taskcontrol.go server/task_handler.go cli/agent/subscribe.go cli/agent/subscriptions.go server/agent_taskcontrol_test.go
git commit -m "feat(board): subscribe/unsubscribe/list_subscriptions over task control"
```

---

### Task 4: send

The first verb with a payload stream. The body arrives on a client-initiated
trsf send-stream whose id rides the request; the server reads it to EOF before
publishing.

**Files:**
- Modify: `server/agent_taskcontrol.go`, `server/task_handler.go`, `cli/agent/send.go`
- Test: `server/agent_taskcontrol_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestAgentSendOverTaskControlPublishes(t *testing.T) {
    h, conn, _ := newAgentTaskHandler(t)
    sub := subscribeVia(t, h, conn, "chat.abc")
    _ = sub

    body := []byte(`{"kind":"hello"}`)
    sid := writeClientStream(t, conn, body) // helper: client-initiated stream + EOF

    req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_AgentSend, RequestId: 9}
    sr := protocol.AgentSendRequest{PayloadStreamId: sid}
    sr.SetTopic([]byte("chat.abc"))
    req.SetAgentSend(sr)

    resp := roundTrip(t, h, conn, req)
    got := resp.AgentSend()
    if got == nil || got.Status != protocol.SendStatus_Ok {
        t.Fatalf("status = %+v, want ok", got)
    }
    if got.DeliveredTo != 1 {
        t.Errorf("delivered_to = %d, want 1 (the publisher subscribes to the target)", got.DeliveredTo)
    }
    if m, ok := h.Board.Retained(got.Seq); !ok || string(m.Payload) != string(body) {
        t.Errorf("retained payload = %q, want %q", m.Payload, body)
    }
}
```

- [ ] **Step 2: Run it and watch it fail**

Run: `go test ./server/ -run TestAgentSendOverTaskControl -v`
Expected: FAIL — no `agent_send` case in the dispatch switch.

- [ ] **Step 3: Move the handler body**

`handleAgentSend` is `agentHandleSend` with four substitutions and nothing
else: `ac.state.Identity()` → `h.boardState(conn).Identity()`, the
`agentboard.SendRequest` fields → `protocol.AgentSendRequest`'s (identical
names), the response envelope → `TaskControlResponse` + `SetAgentSend`, and
`s.Board` → `h.Board`. Keep the goroutine that reads the payload stream: it is
what stops a peer that stalls mid-body from blocking the receive loop. Keep
`readAgentPayloadStream` and its `errPayloadTooLarge` mapping unchanged;
move `resolveReplyTarget` and `retireRepliedParent` with it.

- [ ] **Step 4: Run the test**

Run: `go test ./server/ -run TestAgentSendOverTaskControl -v`
Expected: PASS

- [ ] **Step 5: Add the reply-path test**

```go
func TestAgentSendInReplyToRoutesToParentSender(t *testing.T) {
    // publish a parent from task A on A's own chat topic, then reply from B
    // with --in-reply-to and no --topic; assert the reply landed on A's
    // chat.<short-id> and that the parent was auto-retired.
}
```

Run: `go test ./server/ -run TestAgentSendInReplyTo -v`
Expected: PASS — `resolveReplyTarget` moved unchanged, so this is a
regression guard on the move, not new behaviour.

- [ ] **Step 6: Switch the client**

`cli/agent/send.go` keeps its stream allocation
(`conn.PC().Transport().CreateSendStream()`) and its `resolvePayloadFrom` /
`refuseIfOwnTicket` calls verbatim. Only the envelope changes: build
`AgentSendRequest`, round-trip it, read `resp.AgentSend()`. The printed JSON
(`bytes` / `delivered_to` / `seq` / `source` / `status`) is unchanged.

- [ ] **Step 7: Verify against the baseline and check wiring**

Run the `send` cases from Task 2's capture; `diff`. Then
`grep -rn 'AgentSendRequest{' cli/` and confirm every site sets `InReplyTo`,
`Topic`, `PayloadStreamId`, `ReplyToTopic` and `NoRetireOnReply` where its
flag parsed one — `send` and `dispatch` both build this request.
Expected: no output differences; both sites complete.

- [ ] **Step 8: Commit**

```bash
git add server/agent_taskcontrol.go server/task_handler.go cli/agent/send.go server/agent_taskcontrol_test.go
git commit -m "feat(board): send over task control"
```

---

### Task 5: inbox / inbox_advance

Server-initiated payload streams, several per response. The announce-then-write
ordering is load-bearing and must survive the move.

**Files:**
- Modify: `server/agent_taskcontrol.go`, `server/task_handler.go`,
  `cli/agent/inbox.go`, `cli/agent/prompt_hook.go`
- Test: `server/agent_taskcontrol_test.go`

- [ ] **Step 1: Write the failing test**

```go
func TestAgentInboxDeliversEveryPayload(t *testing.T) {
    // publish three bodies to a subscribed topic, call agent_inbox, assert
    // three msgs come back in seq order and each stream carries its own body.
}

func TestAgentInboxAdvanceMovesTheMarkAndInboxDoesNot(t *testing.T) {
    // agent_inbox twice returns the same batch twice; agent_inbox_advance
    // returns it once and the second call returns nothing.
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./server/ -run 'TestAgentInbox' -v`
Expected: FAIL — no cases in the switch.

- [ ] **Step 3: Move the handlers**

Substitutions as in Task 4. **Keep the two-phase write**:
`openDeliveredPayloadStream` allocates and announces every stream id in the
response, and `flushDeliveredPayloads` writes the bodies only after the
response has been sent, on its own goroutine. Reversing that order makes a
body land in a window nobody is draining, bounded by the peer's receive
window — the reason the split exists is in
`openDeliveredPayloadStream`'s comment and it moves with the function.

- [ ] **Step 4: Run the tests**

Run: `go test ./server/ -run 'TestAgentInbox' -v`
Expected: PASS

- [ ] **Step 5: Switch the clients**

`cli/agent/inbox.go` and `cli/agent/prompt_hook.go`. The hook envelope's
shape, the 64 KiB `payload_omitted` threshold and the `read_with` string it
prints are unchanged.

- [ ] **Step 6: Verify against the baseline**

Run, against the dummy harness from Task 2:

```bash
harness-cli agent inbox --json > $SCRATCH/now/inbox.txt 2>&1; echo "rc=$?" >> $SCRATCH/now/inbox.txt
diff $SCRATCH/baseline/inbox.txt $SCRATCH/now/inbox.txt
```

Expected: no output from `diff`.

- [ ] **Step 7: Check for half-wiring (C4)**

Run: `grep -rn 'AgentInboxRequest{\|AgentInboxAdvanceRequest{' cli/`
Expected: `inbox.go` builds the first, `prompt_hook.go` the second, and
nothing else — `inbox_advance` has exactly one caller by design, the
`--user-prompt-submit-hook` path.

- [ ] **Step 8: Commit**

```bash
git add server/agent_taskcontrol.go server/task_handler.go cli/agent/inbox.go cli/agent/prompt_hook.go server/agent_taskcontrol_test.go
git commit -m "feat(board): inbox and inbox_advance over task control"
```

---

### Task 6: wait

The long-poll. `handleAwaitIdle` is the sibling shape: build a `respond`
closure and call it when the watcher fires.

**Files:**
- Modify: `server/agent_taskcontrol.go`, `server/task_handler.go`, `cli/agent/wait.go`, `cli/agent/dispatch.go`
- Test: `server/agent_taskcontrol_test.go`

- [ ] **Step 1: Write the failing tests**

```go
func TestAgentWaitReturnsRetainedAboveSinceWithoutBlocking(t *testing.T) {
    // two messages already retained, since=0 -> both come back at once.
    // This is the property a hand-written wait gets wrong: wait is "take
    // everything after the cursor, block only if there is nothing".
}

func TestAgentWaitBlocksThenWakesOnPublish(t *testing.T) {
    // since = last seq, no messages -> the response does not arrive until a
    // publish lands, and then carries exactly it.
}

func TestAgentWaitTimesOut(t *testing.T) {
    // timeout_ms small, nothing published -> timed_out = 1, msgs empty.
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./server/ -run TestAgentWait -v`
Expected: FAIL

- [ ] **Step 3: Move the handler, keeping it off the receive loop**

`agentHandleWait` is dispatched with `go` today. Keep that: the handler blocks
for the caller's timeout, and running it inline would stall every other
request on the connection.

- [ ] **Step 4: Run the tests**

Run: `go test ./server/ -run TestAgentWait -v`
Expected: PASS

- [ ] **Step 5: Switch the clients**

`cli/agent/wait.go` and `cli/agent/dispatch.go` per C3. Two properties must
survive the move, both of which have cost this repo a wrong diagnosis before:
`dispatch` prints its publish line (`agent dispatch: published N bytes from
<source> as seq S, delivered_to N`) to **stderr** before it starts waiting, so
an empty body shows up there rather than as a timeout minutes later; and it
sets `--since` to the seq it just published and filters on `in_reply_to`, so
nothing retained beforehand can satisfy it.

- [ ] **Step 6: Verify the stderr line survives**

Run: `harness-cli agent dispatch --topic chat.deadbeef 'ping' 2>&1 >/dev/null`
Expected: the `agent dispatch: published ...` line appears on stderr alone.

- [ ] **Step 7: Verify against the baseline and check wiring**

`diff` the `wait` capture. Then `grep -rn 'AgentWaitRequest{' cli/` — both
`wait.go` and `dispatch.go` build one, and `dispatch.go`'s must set `Since`
and `InReplyTo`.

- [ ] **Step 8: Commit**

```bash
git add server/agent_taskcontrol.go server/task_handler.go cli/agent/wait.go cli/agent/dispatch.go server/agent_taskcontrol_test.go
git commit -m "feat(board): wait over task control"
```

---

### Task 7: list_retained / read_seq

**Files:**
- Modify: `server/agent_taskcontrol.go`, `server/task_handler.go`, `cli/agent/retained.go`, `cli/agent/read.go`
- Test: `server/agent_taskcontrol_test.go`

- [ ] **Step 1: Write the failing tests**

```go
func TestAgentListRetainedIsMetadataOnly(t *testing.T) {
    // assert no field of AgentListRetainedResponse carries payload bytes:
    // build a message with a distinctive body, list it, and assert the body
    // appears nowhere in the encoded response.
}

func TestAgentReadSeqRefusesASeqOutsideMySubscriptions(t *testing.T) {
    // a seq on a topic this task does not subscribe to answers not_found --
    // the same not_found as a seq that never existed.
}

// TestAgentFacingRecordsCarryNoOperatorFields is the guard the spec's "What
// is preserved" section asks for. DeliveredMessage and RetainedMeta now live
// in the same file as BoardMessageRow, which carries shown_to, the retracted
// trio and reply_to_topic; an agent must not receive the first two, because a
// withdrawn message leaves every agent-facing path and a shared row would
// hand it back. Asserting on the FIELD SET rather than round-tripping a
// struct is the point -- a later merge of the two rows must fail here.
func TestAgentFacingRecordsCarryNoOperatorFields(t *testing.T) {
    forbidden := []string{"ShownTo", "Retracted", "RetractedAt", "RetractedBy"}
    for _, ty := range []reflect.Type{
        reflect.TypeOf(protocol.DeliveredMessage{}),
        reflect.TypeOf(protocol.RetainedMeta{}),
    } {
        for _, name := range forbidden {
            if _, ok := ty.FieldByName(name); ok {
                t.Errorf("%s carries %s -- operator-only disclosure reached an agent-facing row", ty.Name(), name)
            }
        }
    }
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./server/ -run 'TestAgentListRetained|TestAgentReadSeq' -v`
Expected: FAIL

- [ ] **Step 3: Move the handlers**

`AgentListRetainedResponse.status` is now `BoardStatus` (Task 1), so
`PurgeStatus_NotFound` becomes `BoardStatus_NotFound` and `PurgeStatus_Ok`
becomes `BoardStatus_Ok`. `handleAgentReadSeq` keeps its
`h.Board.Subscribes(st, m.Topic)` check — it is the whole of that verb's
scoping, and without it one request per integer reads the entire board.

- [ ] **Step 4: Run the tests**

Run: `go test ./server/ -run 'TestAgentListRetained|TestAgentReadSeq|TestAgentFacingRecords' -v`
Expected: PASS

- [ ] **Step 5: Switch the clients**

`cli/agent/retained.go` and `cli/agent/read.go` per C3. `read` keeps fetching
the body from the announced stream and keeps never truncating.

- [ ] **Step 6: Verify against the baseline and check wiring**

`diff` the `retained` and `read` captures. Then
`grep -rn 'AgentListRetainedRequest{\|AgentReadSeqRequest{' cli/` — `retained`
must set `Topic` (including via `--self`), `read` must set `Seq`.

- [ ] **Step 7: Commit**

```bash
git add server/agent_taskcontrol.go server/task_handler.go cli/agent/retained.go cli/agent/read.go server/agent_taskcontrol_test.go
git commit -m "feat(board): list_retained and read_seq over task control"
```

---

### Task 8: retract

**Files:**
- Modify: `server/agent_taskcontrol.go`, `server/task_handler.go`, `cli/agent/retract.go`
- Test: `server/agent_taskcontrol_test.go`

- [ ] **Step 1: Write the failing tests**

```go
func TestAgentRetractWithdrawsOnlyMyOwnMessage(t *testing.T) {
    // A publishes; B retracts -> not_found, and the message is still live.
    // A retracts its own -> ok, and it leaves inbox/wait/read/retained.
}

func TestAgentRetractLeavesItReadableToTheOperator(t *testing.T) {
    // after a successful agent_retract, board_read still shows the message
    // marked retracted with by=author.
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./server/ -run TestAgentRetract -v`
Expected: FAIL

- [ ] **Step 3: Move the handler**

No capability gate — the gate is authorship, inside `Board.RetractSeq`, which
compares the stored `FromTask` against the caller's authenticated id. Both
failure modes stay one `not_found`: a distinguishable "not yours" confirms
that any guessed seq exists somewhere on the board.

- [ ] **Step 4: Run the tests**

Run: `go test ./server/ -run TestAgentRetract -v`
Expected: PASS

- [ ] **Step 5: Switch the client**

`cli/agent/retract.go` per C3. Its output stays `{"status":"ok","seq":N}` /
`{"status":"not_found",...}` with exit 0 in both cases — a no-op is not an
error here, because "not yours" and "already gone" are deliberately one
answer.

- [ ] **Step 6: Verify against the baseline and check wiring**

`diff` the `retract` capture. Then `grep -rn 'AgentRetractRequest{' cli/` —
one site, setting `Seq`.

- [ ] **Step 7: Commit**

```bash
git add server/agent_taskcontrol.go server/task_handler.go cli/agent/retract.go server/agent_taskcontrol_test.go
git commit -m "feat(board): retract over task control"
```

---

### Task 9: the two merges — topics and purge

**Files:**
- Modify: `cli/agent/topics.go`, `cli/agent/purge.go`, `server/agent_handler.go`
- Modify: `cli/verb/caps.go` (two descriptions), `cli/board.go` (a shared denial helper)
- Test: `server/board_handler_test.go`

- [ ] **Step 1: Write the failing tests**

```go
func TestBoardTopicsReturnsRetractedCountToATaskPrincipal(t *testing.T) {
    // a task holding board_observe (not the operator connection) calls
    // board_topics on a topic with one withdrawn message; retracted_count is 1.
    // This is the disclosure U4 decided: the gate is the capability, not the
    // principal, and the same caller could already read it by typing
    // `board topics` instead of `agent topics`.
}

func TestAgentTopicsDeniedNamesTheCapability(t *testing.T) {
    // --caps none -> PermissionDenied with required_cap == board_observe,
    // and the CLI prints that name without holding a literal.
}
```

- [ ] **Step 2: Run them and watch them fail**

Run: `go test ./server/ -run 'TestBoardTopicsReturnsRetracted|TestAgentTopicsDenied' -v`
Expected: the first FAILs only if the handler filtered by principal (it does
not — it is a regression guard); the second FAILs because the CLI still
carries the literal.

- [ ] **Step 3: Point the two subcommands at the board kinds**

`cli/agent/topics.go` builds `TaskControlKind_BoardTopics`;
`cli/agent/purge.go` builds `TaskControlKind_BoardPurge` (its `--self`
shorthand still derives `chat.<short-id>` client-side). Their text and JSON
output is unchanged except that `topics` now has a `retracted_count` to print
— add it to the row, never eliding a zero (surface-parity item 31: gate on
existence, not value).

- [ ] **Step 4: Replace the two literals with one helper**

In `cli/`, one function both call and every later denial uses:

```go
// permissionDeniedError renders a PermissionDenied response as an error that
// names the missing capability from the wire, so no caller restates the cap
// vocabulary. Replaces the literals in cli/agent/purge.go and topics.go.
func permissionDeniedError(pd *protocol.PermissionDeniedResponse) error {
    if pd == nil {
        return errors.New("permission denied")
    }
    return fmt.Errorf("%s denied: requires capability %q", pd.RequestedKind, pd.RequiredCap)
}
```

- [ ] **Step 5: Delete the agent-side handlers and their status enums**

Remove `agentHandleListTopics` and `agentHandlePurge` from
`server/agent_handler.go`. Their two inline capability checks go with them —
`requiredCap` already gates the board kinds.

- [ ] **Step 6: Update the two stale capability descriptions**

In `cli/verb/caps.go`:
- `CapDescription(Capability_None)` says "no capabilities; data-plane only
  (agentboard messaging, own task logs/ls)" — still true, and it stays true
  until the `board_send` spec lands. Leave it.
- `CapDescription(Capability_BoardObserve)` says the bit is "NOT required to
  send, subscribe, or read your own inbox". Still true. Leave it.

Both descriptions survive this change unchanged; they are listed here so the
next reader knows they were checked rather than missed.

- [ ] **Step 7: Run the tests, verify against the baseline, commit**

```bash
git add cli/agent/topics.go cli/agent/purge.go cli/board.go server/agent_handler.go server/board_handler_test.go
git commit -m "feat(board): agent topics and purge use the board kinds"
```

---

### Task 10: `agent thread`, and any remaining reader

**Files:**
- Modify: `cli/agent/thread.go`, `cli/agent/json_emit.go`, `cli/agent/util.go`,
  `cli/agent/parse.go`, `cli/agent/payload.go`, `cli/agent/ticket_guard.go`

- [ ] **Step 1: Enumerate what still names the old family**

Run: `grep -rln 'agentboard\.' cli/ cmd/ server/`
Expected: a list. Every entry is either migrated here or is a server file
Task 11 deletes.

- [ ] **Step 2: Migrate each remaining reader**

`agent thread` builds its rows from `agent retained` + `agent read`, both of
which moved in Task 7, so it changes only in the types it names.

- [ ] **Step 3: Build, verify the full baseline, commit**

Run: `go build ./... && go test ./cli/...`
Then re-run every command in Task 2's capture and `diff` the whole directory.
Expected: no differences.

```bash
git add cli/agent/
git commit -m "refactor(cli): the agent subcommands no longer name the agentboard schema"
```

---

### Task 11: delete the second description

**Files:**
- Delete: `agentboard/agentboard.bgn` wire formats, `agentboard/agentboard.go` (regenerated away)
- Modify: `appwire/app.bgn`, `server/agent_handler.go`, `agentboard/ids.go`,
  `agentboard/registry.go`, `agentboard/board.go`, `agentboard/conn.go`,
  `agentboard/taskstate.go`, `agentboard/topic.go`

- [ ] **Step 1: Remove `handleAgentMessage` and its dispatch**

Delete the `case appwire.AppKind_AgentMessage:` arm and the function. Every
verb it dispatched now has a task-control case.

- [ ] **Step 2: Retire the AppKind**

In `appwire/app.bgn`, remove `agent_message = 0x44`. Do not reuse the value —
a peer built before this change still sends it, and an unrelated meaning on
0x44 would decode as that unrelated thing rather than failing.

- [ ] **Step 3: Collapse the identity types**

Delete `format RunnerID` / `format TaskID` from `agentboard.bgn` and switch
the Go package to `protocol.RunnerID` / `protocol.TaskID` throughout. Then
`agentboard/ids.go` loses `runnerIDStringBoard`, `hexTaskIDBoard` and the
uncalled `formatIP`; `registry.go:65`'s board-typed lookup becomes the same
call as `:37`'s.

- [ ] **Step 4: Delete `HelloStatus`**

`Registry.Validate` returns `protocol.ClientHelloStatus`;
`clientHelloStatusFromBoard` in `server/agent_handler.go` goes with it.

- [ ] **Step 5: Regenerate and build**

Run: `make gen && go build ./... && go vet ./...`
Expected: green.

- [ ] **Step 6: Assert the family is gone**

Run: `grep -rn 'AgentMessage\|AppKind_AgentMessage' --include=*.go . | grep -v _test`
Expected: no output.

- [ ] **Step 7: Run the whole suite**

Run: `make test` (or the repo's unit-test target, which CI runs under `-race`)
Expected: green.

- [ ] **Step 8: Commit**

```bash
git add -A agentboard/ appwire/ server/ runner/protocol/
git commit -m "refactor(board): delete the agent frame family"
```

---

### Task 12: documentation surfaces

**Files:**
- Modify: `runner/agentskills/harness-cli/SKILL.md` (the embed source of truth)
- Mirror: `.claude/skills/harness-cli/SKILL.md`, `.agents/skills/harness-cli/SKILL.md`
- Modify: `docs/superpowers/specs/2026-04-28-agent-comms-design.md` (Amendment)

- [ ] **Step 1: Update the skill's denial paragraph**

The "stderr is not the error channel" section quotes two denial strings that
differed by surface (`topics denied: requires capability "board_observe"` vs
`permission denied: BoardTopics requires capability board_observe`) and warns
that matching on the string breaks on one of them. After this change there is
one shape. Rewrite the paragraph to say so; keep the rule that the exit code,
not stderr, is the error channel.

- [ ] **Step 2: Mirror to both copies**

Run: `go test ./runner/agentskills/ -run 'TestMirrors|TestAgentsMirror'`
Expected: PASS — the mirrors are test-enforced, so a byte difference fails.

- [ ] **Step 3: Amend the 2026-04-28 spec**

Append an Amendment section recording that the `AgentMessage` kind it
introduced is retired, that the board's agent face now rides task control, and
that `Deliver` was never built (the wake solved its delivery half; its dial
half is a process-lifetime cost no push removes).

- [ ] **Step 4: Commit**

```bash
git add runner/agentskills/ .claude/skills/ .agents/skills/ docs/superpowers/specs/
git commit -m "docs: the agent board face rides task control"
```

---

### Task 13: verification

- [ ] **Step 1: Wire skew**

Run: `scripts/wire-skew-check.sh`
Expected: PASS, having exercised a real rejection. Per the spec's Risks, the
subject here is an OLD peer sending `AppKind` 0x44 at a server that no longer
routes it — confirm the run shows that peer failing recoverably, not merely
that the new pair works.

- [ ] **Step 2: Dummy-harness E2E, every verb in the spelling the help prints**

Bring up `scripts/dummy-harness.sh`, spawn two tasks, and run the full
handshake both ways: `send` → wake → `inbox` → reply with `--in-reply-to` →
confirm the parent was auto-retired → `retract` → `board read` shows it
withdrawn with `by=author`.

- [ ] **Step 3: Edge cases, each run once**

| case | expected |
|---|---|
| `send --topic` nobody subscribes | `status ok`, `delivered_to 0` |
| `send --data -` with empty stdin | `status ok`, `bytes 0`, `source stdin` |
| `send` a body over `--agentboard-max-payload` | `PayloadTooLarge`, not truncation |
| `send --in-reply-to <rotated seq>` | `unknown_in_reply_to` |
| `wait` with no `--since` on a topic holding messages | returns immediately with the retained batch |
| `read <seq>` on a topic not subscribed | `not_found` |
| `retract <seq>` published by another task | `not_found`, message still live |
| `agent topics` / `agent purge` from a `--caps none` task | denied, naming the capability from `required_cap` |
| `board topics` from a task holding `board_observe` | rows carry `retracted_count` |
| a >64 KiB body through the inbox hook | `payload_omitted`, with a `read_with` that works |

- [ ] **Step 4: Report**

Record each row's observed result with the command that produced it
(`feedback_numbers_carry_their_command`). A row that cannot be produced is
reported as such, not skipped silently.

---

### Task 14: land

- [ ] **Step 1: Sync onto the current trunk and land per the repo's policy**

REQUIRED SUB-SKILL: `landing-to-main`. Mode A (local-trunk FF-push), never
force, never cherry-pick to the remote. The landing UNIT is this whole feature
set, not one commit at a time.

- [ ] **Step 2: Build in the main checkout**

Run: `make build` in the parent checkout, unprompted — it is part of landing
(`feedback_build_after_landing`), and `go build` does not refresh `bin/`.

- [ ] **Step 3: Restart the fleet, server first**

Pitfall 10's ordering. Then confirm a live agent can still reach the board:
`harness-cli agent subscriptions` from a running task.
