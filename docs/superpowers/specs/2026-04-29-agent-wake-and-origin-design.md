# Agent wake-up and message origin

Date: 2026-04-29
Status: Proposed

## Problem

The agentboard sideband currently has two related UX gaps that surface
when LLM-driven agents talk to each other through it:

1. **No real-time delivery to idle agents.** The runner spawns claude
   with a `.claude/settings.json` that wires a UserPromptSubmit hook
   (fires on user input) and a Stop hook (fires at turn end). If a
   message arrives while claude is *idle* (already finished a turn,
   nothing scheduled to fire), neither hook is triggered. The message
   sits in the inbox until either the human types something or claude
   independently happens to call `agent inbox`. Empirically reproduced
   in this codebase's own dogfood session.

2. **No protocol-level sender info.** `DeliveredMessage` carries
   `seq / topic / payload` only. To tell who sent a message, agents have
   to bake a `from` field into the payload by convention. That
   convention has to be re-imposed on every fresh LLM session — see
   `feedback_protocol_explicit_over_convention.md`. There is also no
   way to enumerate topics on the board or list one's own subscriptions.

This spec addresses both.

## Goals

- Idle agents wake up within ~1–2s of an inbound agentboard message.
- The wake path is layered cleanly: runner protocol carries the wake
  hint, agentboard carries the message.
- `agent inbox` / `agent wait` / `agent topics` outputs identify the
  sender at the protocol level — no payload conventions required.
- `harness-cli agent topics` and `harness-cli agent subscriptions`
  expose board / per-agent enumeration so tooling can discover state
  without poking implementation details.

## Non-goals

- True pre-turn-boundary preemption of an in-flight claude turn. The
  wake bytes that arrive mid-turn queue at the OS pipe and are consumed
  at the next prompt boundary; we do not interrupt a running turn.
- Filtering topic listings by subscribed-only / published-by-me.
  `MaxTopics=32` makes whole-board enumeration cheap; clients can grep.
- Per-subscription cursors. Cursor is per-task-global today and stays
  that way.
- Glob / wildcard topic patterns. Subscribe is exact-match in v1; this
  spec keeps the field name `pattern` for future flexibility but does
  not introduce new matching semantics.

## Architecture

Two independent slices that share the agentboard schema as their
common ground.

### Slice A — Wake-up via runner protocol

```
                       agentboard.Board.Send appends to topic
                                       │
                                       ▼
                       resolve subscribers (rid,tid) for topic
                                       │
                                       ▼
                       lookup runner conn that hosts (rid,tid)
                                       │
                                       ▼
        server ──→ runner: RunnerRequest{kind=task_wake, task_id=tid}
                                       │
                                       ▼
                       runner sessions[tid].WakeStdin()  (debounced 1.5s)
                                       │
                                       ▼
            pipeIn ←── "<harness:agentboard-wake> new agentboard
                       message(s) — review and act as appropriate\n"
                                       │
                                       ▼
                       claude: next turn fires UserPromptSubmit hook
                       which runs `agent inbox --since-last --json`
                       and dumps the buffered messages into context
```

### Slice B — Sender info + listing API

`agentboard.Board.Send` already authenticates the caller (it has the
sending taskState). It captures `(from_runner_id, from_task_id,
from_hostname)` at append time and attaches them to the
`RetainedMessage`. Every downstream `DeliveredMessage`, whether
delivered live or pulled via `inbox` / `wait`, carries these fields.

Two new agentboard ops, `list_topics` and `list_subscriptions`, expose
read-only enumeration. They are separate kinds (not subcommand sugar)
so they round-trip the existing AgentMessage envelope cleanly.

## Schema diff (single source of truth)

This is the *complete* set of `.bgn` changes. Per
`feedback_no_split_schemas.md`, no follow-up tasks add fields here.

### `agentboard/agentboard.bgn`

```bgn
# 1. DeliveredMessage gains 3 sender fields
format DeliveredMessage:
    seq :u64
    topic_len :u16
    topic :[topic_len]u8
    payload_len :u32
    payload :[payload_len]u8
    from_runner_id :RunnerID
    from_task_id :TaskID
    from_hostname_len :u8
    from_hostname :[from_hostname_len]u8

# 2. AgentMessageKind gains 4 enum values
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
    list_topics                          # NEW
    list_topics_response                 # NEW
    list_subscriptions                   # NEW
    list_subscriptions_response          # NEW

# 3. Six new formats
format ListTopicsRequest:
    request_id :u32

format TopicSummary:
    name_len :u16
    name :[name_len]u8
    last_seq :u64
    last_published_at_unix_ms :u64
    msg_count :u16

format ListTopicsResponse:
    request_id :u32
    topics_len :u16
    topics :[topics_len]TopicSummary

format ListSubscriptionsRequest:
    request_id :u32

format SubscriptionSummary:
    pattern_len :u16
    pattern :[pattern_len]u8

format ListSubscriptionsResponse:
    request_id :u32
    subscriptions_len :u16
    subscriptions :[subscriptions_len]SubscriptionSummary

# 4. AgentMessage match: 4 new arms
format AgentMessage:
    kind :AgentMessageKind
    match kind:
        ... existing arms ...
        AgentMessageKind.list_topics                => list_topics                :ListTopicsRequest
        AgentMessageKind.list_topics_response       => list_topics_response       :ListTopicsResponse
        AgentMessageKind.list_subscriptions         => list_subscriptions         :ListSubscriptionsRequest
        AgentMessageKind.list_subscriptions_response=> list_subscriptions_response:ListSubscriptionsResponse
        .. => error("Unexpected agent message kind")
```

### `runner/protocol/message.bgn`

```bgn
enum RunnerRequestType:
    :u8
    assign_task
    cancel_task
    open_exec
    runner_hello_response
    task_wake                            # NEW

format TaskWakeRequest:
    task_id :TaskID

# RunnerRequest match: 1 new arm
format RunnerRequest:
    kind :RunnerRequestType
    match kind:
        ... existing arms ...
        RunnerRequestType.task_wake => task_wake :TaskWakeRequest
```

## CLI surface

```
harness-cli agent topics
   # JSON Lines, one per board topic:
   # {"name":"chat/demo","last_seq":6,"last_published_at":"2026-04-29T12:36:55Z","msg_count":6}

harness-cli agent subscriptions
   # JSON Lines, one per registered subscription on this (rid, tid):
   # {"pattern":"chat/demo"}
```

`inbox` / `wait` / `dispatch` JSON output gains a nested `from` block:

```json
{
  "seq": 1,
  "topic": "chat/demo",
  "payload_b64": "...",
  "payload": {...},
  "from": {
    "runner_id": "ws:1.2.3.4:1234-1",
    "task_id": "57b2dbd3...",
    "hostname": "gmkhost"
  }
}
```

`from` is nested rather than flat so future fields can be added without
crowding the top-level namespace.

## Wake string

```
<harness:agentboard-wake> new agentboard message(s) — review and act as appropriate\n
```

Single line, contains both a machine-detectable tag (for future hook
post-processing) and an action-agnostic human-readable instruction.
"act as appropriate" deliberately covers reply / continue work /
acknowledge / no-op so the LLM is not forced into a reply-shaped
response when the message does not warrant one.

## Coalescing

Per-task on the runner side: at most 1 wake write per 1.5s window.
Implementation: `lastWakeAt time.Time` guarded by a mutex on the
session. Subsequent deliveries arriving within the window are dropped
on the runner; they are not lost — the agent's next inbox call uses
`--since-last` and reads everything since the cursor.

## Layering / where each piece lives

| Concern                          | Layer                       |
|----------------------------------|------------------------------|
| Sender attestation               | `agentboard/board.go::Send` (server side, from taskState) |
| Sender bytes on the wire         | `agentboard.bgn::DeliveredMessage` |
| Topic / subscription listing     | `agentboard/board.go` (new methods), `cli/agent/topics.go`, `cli/agent/subscriptions.go` |
| Wake decision                    | server: hook into `Board.Send` to enumerate target (rid, tid) → resolve runner conn |
| Wake transport                   | `runner/protocol::RunnerRequest{task_wake}` |
| Wake delivery to claude          | `runner/session.go`: per-task `WakeStdin()` writing to `pipeIn` (the same writer exec.go uses for TUI stdin frames). Debounced. |
| stdout / hook → context          | unchanged (existing UserPromptSubmit hook with `--since-last`) |

The wake path does **not** depend on the agent ever calling
`harness-cli agent ...` itself — it is purely server → runner →
claude. The schema-listing path runs through the existing agentboard
client.

## Error / edge cases

- **No live session for a wake**: runner receives `task_wake` for a
  task it does not have in `sessions` (just finished, was cancelled,
  etc.). Log at debug level and drop. Server is already designed to
  send wakes for whatever subscribers exist; the race is benign.
- **Mid-turn wake**: wake bytes queue at the OS pipe. They are
  consumed when claude reaches its next prompt boundary. UserPromptSubmit
  fires once with the (now-larger) inbox dump.
- **Multiple wakes mid-turn**: after coalescing, at most one queued.
- **Server side missing subscriber index**: today `Board` resolves
  subscribers per topic from `topics[topicName].subscribers`. The wake
  enumeration reuses that path, then resolves rid→runner via the
  existing runner registry. No new registry needed.
- **RunnerID hard assertion**: `protocol.RunnerID` requires
  `IpAddrLen ∈ {4, 16}`. The taskState's stored runner_id was already
  registered with a valid IP (otherwise hello would have failed), so
  using it verbatim is safe (`project_runnerid_constraint.md`).
- **Cross-OS deployment**: wake string is plain ASCII + UTF-8; pipeIn
  on Windows ConPTY accepts the same bytes (`project_deployment_topology.md`).

## Testing

- **Unit**: `Board.Send` attaches sender from caller taskState (not
  from arbitrary bytes in the request). Decode/encode round-trip for
  the new formats.
- **Unit**: runner-side `WakeStdin` writes the expected bytes; second
  invocation within debounce window is a no-op; invocation after window
  succeeds.
- **E2E** (in-process server, two synthetic agents): A subscribes,
  B sends, A's `inbox` JSON contains B's `from.task_id` and
  `from.hostname`. Add a topic-not-yet-published case for `agent
  topics` and a multi-subscription case for `agent subscriptions`.
- **E2E for wake**: spawn a real `agent-runner` against an in-process
  server with a fake-claude that echoes its stdin to a file; agent A
  sends to a topic A's task subscribes to; assert the fake's stdin file
  contains the wake string within ~2s.

## Out of scope but observed

The Stop-hook path shipped earlier (commit `4dbcf25`) covers
"message arrived during a turn"; this spec covers "message arrived
while idle". They are complementary, not redundant — the Stop hook
remains the right cover for the in-flight case because the runner
cannot know that claude is mid-turn from outside.

## Security / threat model

The harness has no external users (`feedback_individual_dogfood.md`).
Sender attestation matters mainly for distinguishing your own running
LLMs by hostname during dogfood. Server-side attestation prevents an
agent from spoofing `from_runner_id` in payload — a useful
defense-in-depth even within trusted scope.
