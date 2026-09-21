# One board on the wire: fold the agent frame family into task control

The agentboard is described **twice** in the wire schemas. `runner/protocol`
carries an operator face (`BoardTopics` / `BoardRead` / `BoardSubscribers` /
`BoardPurge` / `BoardRetract`, `BoardStatus`, three row formats);
`agentboard/agentboard.bgn` carries an agent face for the same board with its
own envelope, its own status vocabulary and its own copies of `RunnerID` /
`TaskID`.

This folds the agent face into `TaskControlKind` and deletes the second
description. No operator-visible behaviour changes.

## Decisions taken

| # | Decision | Decided by |
|---|----------|------------|
| U1 | Agent verbs become `TaskControlKind` values: request in `TaskControlRequest`'s match, response in `TaskControlResponse`'s. **One kind per verb**, not one per direction. | operator, 2026-09-22 |
| U2 | A verb keyed to the caller's own subscriptions or authorship gets an `agent_*` kind. A verb that reaches topics the caller need not subscribe to keeps the `board_*` kind it already has. That split is not new naming — it is where the capability gates already sit. | author, from `agent_handler.go` (see Problem) |
| U3 | `list_topics` merges into `board_topics`; `purge` merges into `board_purge`. These are the only two agent verbs that are capability-gated, and the only two with operator twins. | operator, 2026-09-22 |
| U4 | On the merged `board_topics`, `retracted_count` is returned to **every** caller that passes the `board_observe` gate. No presence bit, no zero default. | operator, 2026-09-22 |
| U5 | `retract` stays its own kind (`agent_retract`) beside `board_retract`. The two differ in authority AND in what the operator view records (`by=author` vs `by=purge_cap:<id>`); one kind could not carry that distinction. | author |
| U6 | `read_seq` stays its own kind. It is one seq scoped to the caller's subscriptions; `board_read` is a whole topic behind `board_observe`. | author |
| U7 | Capability denials answer with `PermissionDenied`. `ListTopicsStatus.denied` and `PurgeStatus.denied` are deleted, with the two hardcoded capability-name strings in the CLI. | operator, 2026-09-22 |
| U8 | Refusals that are **not** capability answers keep their own enums: `ReadSeqStatus.not_found`, `RetractStatus.not_found`, `SendStatus`'s frame and size arms. Those merge two cases deliberately, to avoid an enumeration oracle; `PermissionDenied` does not express that. | author, from `agentboard.bgn:419` |
| U9 | The schema change lands whole, in one commit with the server and client migration. Per-verb staging would require both frame families alive at once. | author, from `feedback_no_split_schemas` |
| U10 | `board_send`, the agentboard send capability, is **out of scope here** and gets its own spec after this lands. Sequencing chosen because `PermissionDenied` then answers its denial with no new status values. | operator, 2026-09-22 |
| U11 | The reserved `deliver` slot is **not** carried into `TaskControlKind`. Its delivery half was superseded by the PTY wake a day after it was reserved; its remaining half is a dial-per-hook cost that an enum value does not address. A push path, if built, appends a kind then — reserving a slot in an append-safe enum buys nothing, and an unused value invites being read as debris, which is how this spec's first draft read it. Both halves are recorded in Problem so they survive the value. | author, from `2026-04-28-agent-comms-design.md:187`/`:468` and `2026-04-29-agent-wake-and-origin-design.md` |

## Problem

### The board is on the wire twice

| Concern | operator face (`runner/protocol/message.bgn`) | agent face (`agentboard/agentboard.bgn`) |
|---|---|---|
| envelope | `TaskControlRequest` / `TaskControlResponse`, one `kind` matched twice | `AgentMessage`, **one enum value per direction** (25 values for 12 verbs) |
| topic listing | `BoardTopicsRequest` → `BoardTopicRow` | `ListTopicsRequest` → `TopicSummary` |
| message read | `BoardReadRequest` → `BoardMessageRow` | `ReadSeqRequest` → `DeliveredMessage` |
| destruction | `BoardPurgeRequest` → `BoardStatus` | `PurgeRequest` → `PurgeStatus` |
| withdrawal | `BoardRetractRequest` → `BoardStatus` | `RetractRequest` → `RetractStatus` |
| denial | `PermissionDeniedResponse{requested_kind, required_cap}` | a `denied` value per response enum |
| identity | `protocol.RunnerID` / `protocol.TaskID` | its own `RunnerID` / `TaskID`, hand-copied |

### The duplication already diverged, and it is observable

`agent topics` and `board topics` read the same source
(`Board.ListTopics()` — `server/agent_handler.go:676`, `server/board_handler.go:17`),
clamp `msg_count` the same way, and are gated on the same bit
(`agent_handler.go:660`; `requiredCap[BoardTopics]` in `server/capabilities.go:44`).
Neither narrows the rows: both return the whole board, because no task scope
can bound a topic (`scope_percap_completeness_test.go` classifies
`board_observe` as `capNoTargetResolution`).

The operator row carries one field the agent row does not — `retracted_count`,
filled from `Board.RetractedCount` at `board_handler.go:31`.

**So the same principal, holding the same capability, receives different data
depending on which frame family it typed.** A task granted `board_observe`
gets `retracted_count` from `harness-cli board topics` and not from
`harness-cli agent topics`. That is drift between two descriptions of one
subsystem, not a disclosure policy: nothing gates the field on anything the
two callers differ in.

### The duplication has already cost maintenance

- `agentboard/ids.go` holds paired helpers for one identity in two types —
  `runnerIDStringProto` / `runnerIDStringBoard`, `hexTaskIDProto` /
  `hexTaskIDBoard` — and `registry.go:37` and `:65` use both to key **the same
  map** from the two copies.
- `formatIP` in that file has no caller anywhere in the repo. It is what
  survived when `RunnerID` stopped being an address and only the board-side
  copy was left holding the old shape.
- `HelloStatus` (`agentboard.bgn:48`) is referenced by no format. It is a
  Go-only enum duplicating the first four values of
  `protocol.ClientHelloStatus`, and `clientHelloStatusFromBoard`
  (`agent_handler.go:139`) exists to convert between them.

One thing in the same area is **not** on that list, and the distinction is
load-bearing. `AgentMessageKind.deliver` (value 9) is a slot reserved in the
2026-04-28 design for a server → agent push of new messages on a subscribed
topic, deferred out of v1 in favour of `wait`'s long-poll
(`2026-04-28-agent-comms-design.md:125`, `:187`). `git log -S
AgentMessageKind_Deliver`, excluding the generated file, returns no commit:
no hand-written line has ever read it.

It was reserved against two things, and only one of them is still open.

- **An idle agent not learning that a message arrived.** Solved the NEXT DAY
  by a different mechanism: `2026-04-29-agent-wake-and-origin-design.md`
  opens on exactly this ("No real-time delivery to idle agents") and answers
  it with the runner typing a synthetic prompt into the session's PTY. That
  spec never names `deliver`, so the slot was not retired — it was left
  behind.
- **A fresh dial per inbox hook** (`2026-04-28-agent-comms-design.md:468`,
  which named a long-lived connection carrying `deliver` as the v2 answer).
  Still open, and the wake did not narrow it: a wake fires
  `UserPromptSubmit`, which runs `harness-cli agent inbox`, which dials
  through `ConnectAgent` in `cli/agent/conn.go`. The one place a connection
  is held open is `wait` / `dispatch`, and those are bounded to scripts
  outside an agent turn by design.

So what the value reserves is a solved problem plus an unsolved cost, and
the unsolved cost is not a reason to keep an enum value.

### Half of this migration already happened

`c9fe691f feat(schema): ClientHello carries agent identity; remove
AgentBridgeHello` moved `hello` / `hello_response` out of `AgentMessageKind`
into `ClientHello`, and `AgentBridgeHello` is gone from the generated code as
well. Agent identity is already established once for both families on one
connection — `establishAgentIdentity` says so in its own comment. This spec
finishes what that commit started.

## Where the gates already sit — the rule U2 names

Two capability checks exist in the whole of `server/agent_handler.go`:

- `Capability_BoardObserve`, once, in `agentHandleListTopics` (`:660`)
- `Capability_Purge`, once, in `agentHandlePurge`

Every other agent verb — `send`, `subscribe`, `unsubscribe`, `wait`, `inbox`,
`inbox_advance`, `list_subscriptions`, `list_retained`, `read_seq`, `retract` —
is ungated, and each handler's comment gives the same reason: the verb is
keyed to a topic the caller already subscribes to, or to a message the caller
authored. `agentHandleListRetained` states it directly: *"a KEYED read of a
topic the caller must already name — not a discovery sweep (that is
list_topics, which board_observe gates)"*.

So the split is already in the code. The two gated verbs are exactly the two
with operator twins, and merging them is the whole of U3 — the rule predicts
the merge set rather than being fitted to it.

**IN / OUT for the `agent_*` prefix**, stated because a prefix is a label and
labels harden: `agent_*` means *reaches only topics the caller subscribes to,
or messages the caller published, and takes no capability*. `board_*` means
*reaches topics the caller need not subscribe to, and takes a capability*. A
future verb is named by that test, not by which CLI noun it sits under.

## Verb map

| agent verb today | becomes | capability | reaches |
|---|---|---|---|
| `send` | `agent_send` | none (until the `board_send` spec) | a topic it names, or the parent's sender |
| `subscribe` | `agent_subscribe` | none | own subscription set |
| `unsubscribe` | `agent_unsubscribe` | none | own subscription set |
| `wait` | `agent_wait` | none | own subscribed topics |
| `inbox` | `agent_inbox` | none | own subscribed topics |
| `inbox_advance` | `agent_inbox_advance` | none | own delivery mark |
| `list_subscriptions` | `agent_list_subscriptions` | none | own subscription set |
| `list_retained` | `agent_list_retained` | none | one named topic, metadata only |
| `read_seq` | `agent_read_seq` | none | one seq, inside own subscriptions |
| `retract` | `agent_retract` | none (authorship) | one message it published |
| `list_topics` | **`board_topics`** (merged) | `board_observe` | every topic |
| `purge` | **`board_purge`** (merged) | `purge` | any topic's ring |

Ten new kinds, two merges. `TaskControlKind` is a `:u8` holding 34 values
today, so the twelve fit with room left; they are **appended**, as
`list_conns` and the `inbox_advance` pair both were, because the enum is
positional and an insertion renumbers every later value.

### `purge` merges because the handlers are already identical

`agentHandlePurge` and `handleBoardPurge` branch the same way on the same
calls:

```
seq == 0 → Board.PurgeTopic(topic);   !found → not_found; else ok + clamp to 65535
seq  > 0 → Board.PurgeSeq(topic,seq); !found || !removed → not_found; else ok + purged=1
```

They differ in the status enum's name and in where the `purge` check is
written (inline vs the central `requiredCap`). Both are removed by the merge.

## What is deleted

- `appwire.AppKind_AgentMessage` (0x44) retires. The value is not reused.
- `agentboard.bgn`'s wire formats and the `AgentMessage` envelope.
- The reserved `deliver` slot, under U11 — with both halves of what it
  reserved recorded in Problem rather than carried as an unused enum value.
- `agentboard`'s `RunnerID` / `TaskID`, and with them `ids.go`'s paired
  helpers and the uncalled `formatIP`.
- `HelloStatus` and `clientHelloStatusFromBoard`; `Registry.Validate` returns
  `protocol.ClientHelloStatus`.
- `ListTopicsStatus` and `PurgeStatus` entirely — the `ok` arms are
  `BoardStatus`, the `denied` arms are `PermissionDenied`.
- The two capability names spelled as literals in the CLI:
  `cli/agent/purge.go:100` (`"purge"`) and `cli/agent/topics.go:74`
  (`"board_observe"`). After U7 the missing bit arrives as
  `PermissionDeniedResponse.required_cap`, so no consumer restates the cap
  vocabulary.
- `cli/agent/conn.go`'s `Conn` wrapper, `SendRaw` and the per-subcommand
  `SetOnControl` + request-id demux: replaced by `RoundTripTaskControl`, the
  helper `cli/board.go:313` already uses for the operator half of these very
  verbs.

## What is preserved, and how it is checked

`DeliveredMessage` and `RetainedMeta` move to `runner/protocol` **unchanged
in field set**. They are deliberately not merged with `BoardMessageRow`: the
operator row carries `shown_to`, the retracted trio and `reply_to_topic`, and
an agent must not receive those — a withdrawn message leaves every
agent-facing path, so surfacing it through a shared row would return it.

The observable failure if this is got wrong: a task calls `agent read <seq>`
and the record carries a `retracted` marker or another task's delivery
position. A test asserts the agent-facing record's field set explicitly rather
than round-tripping a shared struct.

`retracted_count` is the one field that crosses, under U4, and it crosses
because the gate it sits behind (`board_observe`) is the same gate the caller
already passed to receive any row at all.

## Long-poll and payload streams carry over unchanged

- `agent wait` blocks server-side (`go s.agentHandleWait`). The task-control
  family already delays a response the same way: `handleAwaitIdle` builds a
  `respond` closure and calls it when the watcher fires
  (`server/await_idle_handler.go:45`).
- Bodies ride trsf streams, announced by id in the response envelope.
  `board_read` already does this on the task-control family ("returns
  stream_id for content"), as do `open_file_transfer` and `get_task_log`.
  `openDeliveredPayloadStream` / `flushDeliveredPayloads` move with the
  handlers, not into them.

## Surfaces

| Surface | Change |
|---|---|
| `cli/agent/*.go` (19 files) | request construction moves to `TaskControlRequest`; response demux to `RoundTripTaskControl`. Flags, output and JSON shapes unchanged. |
| `cli/board.go` | unchanged — it already speaks this family |
| `cli/verb/table.go` | no new verb, no new flag. The `agent` verb paths keep their spelling |
| TUI / WebUI / wasm | no change: neither surface speaks the agent frame family today |
| `server/agent_handler.go` | handlers move to the task-control dispatch; the two inline capability checks are deleted in favour of `requiredCap` |
| `server/capabilities.go` | `requiredCap` gains `agent_*` entries only where a verb is gated — under U2 that is none of them today |
| `server/scope_percap_completeness_test.go` | unchanged: no new capability is introduced here |
| `runner/agentskills/harness-cli/SKILL.md` | the denial strings quoted in "stderr is not the error channel" change shape; mirror to `.claude/` and `.agents/` in the same commit |
| `docs/.../2026-04-28-agent-comms-design.md` | Amendment section: the `AgentMessage` kind it introduced is retired |

`harness-cli agent --help`, every subcommand's flags, and every JSON record
field keep their current spelling. An operator or agent cannot tell this
landed from any command's output — which is the completion test in the
Testing section.

## Testing

1. **Output equivalence, before and against after.** Capture
   `agent send / subscribe / subscriptions / inbox / retained / read / retract
   / topics / purge` JSON and text output against a dummy harness on the
   current build; re-run on the new build; diff. Any difference is a defect
   unless it is the denial line (U7).
2. **Denial shape.** A task spawned `--caps none` running `agent topics` and
   `agent purge` must report the missing bit by name, sourced from
   `required_cap` rather than a literal.
3. **`board topics` from a task principal holding `board_observe`** returns
   `retracted_count` — the same value the operator connection sees (U4).
4. **Agent-facing field sets.** Assert `agent read` / `agent retained`
   records carry no `shown_to`, no retracted marker.
5. **`cli/flagorder_test.go` and `cli/verb`'s invariant tests** must stay
   green: the verb declarations do not change, so a failure there means the
   migration reached the parser.
6. **`scripts/wire-skew-check.sh`**, unconditionally. It is the gate for any
   `.bgn` change.
7. **Dummy-harness run of each `agent` subcommand in the exact spelling the
   help text prints** — Pitfall 13: every test above enters below argv
   parsing.

## Risks

- **Wire break across the fleet.** Retiring an `AppKind` and appending twelve
  kinds is a decode change on a path `harness-cli` speaks from every runner
  host. The retry guard landed at `9f164ca1`, so a skewed peer is expected to
  reconnect rather than exit; `wire-skew-check.sh` is what proves that for
  this change rather than assuming it. Restart the server before the fleet.
- **Nineteen client files, no behaviour change to anchor review.** A dropped
  field in a request build produces a working command with a missing
  argument. Mitigation: item 28a's discipline — count the request
  constructions per subcommand and confirm each sets what its flags parsed.
- **The row-merge temptation returns later.** `DeliveredMessage` and
  `BoardMessageRow` will sit in one file describing one board and look
  redundant. The "What is preserved" section is written to be the answer a
  future reader finds; the field-set test is what fails if they merge anyway.
- **Scope contraction.** The Problem statement names three maintenance costs
  (`ids.go`'s paired helpers, the uncalled `formatIP`, `HelloStatus`). An
  implementation that moves the frames and leaves those in place has not
  finished; each is listed in "What is deleted" so the two sections cover the
  same scope.
- **Deleting a reservation as if it were debris.** `deliver` was described as
  dead in this spec's first draft and is not: it holds a design intent from
  2026-04-28 whose motivating cost — a fresh dial per inbox hook — is still
  present. U11 removes the value and keeps the intent in prose. The failure
  this guards against is the one `feedback_doc_fixed_to_match_code_erases_intent`
  names: an unused declaration is evidence about a plan, and deleting it
  silently destroys the only record.

## Completion

Done when: every `agent` subcommand's output matches the pre-change capture
except denials; `appwire.AppKind_AgentMessage` has no remaining sender or
receiver; `agentboard.bgn` declares no wire format; `grep -rn "AgentMessage"`
returns nothing outside git history; and `wire-skew-check.sh` is green.
