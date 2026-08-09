# `board subscribers` — who is listening to a topic

Date: 2026-08-09

## Problem

- **The board shows what was said, never who is listening.** `board topics`
  returns name, last seq, last publish time and message count
  (`BoardTopicRow`, `runner/protocol/message.bgn:656-661`); `board read`
  returns each message's sender (`BoardMessageRow`, `:673-686`). Nothing
  reports subscribers.

- **An agent can see only its own subscriptions.**
  `agentHandleListSubscriptions` reads `ac.state`
  (`server/agent_handler.go:408-423`), the caller's own
  `(runner_id, task_id)`, and `ListSubscriptionsRequest`
  (`agentboard/agentboard.bgn:150-151`) carries no target field. There is
  no form of the call that names another task.

- **The data is already in memory and already walked.** `Board.tasks` maps
  each `(runner_id, task_id)` to a `taskState` holding its pattern set and
  its identity (`agentboard/taskstate.go:16-26`), and
  `anyTaskMatchesLocked` (`agentboard/board.go:166-175`) scans exactly that
  to answer "does anyone subscribe to this topic". It returns a bool and is
  used only by `Revoke`. Nothing exposes it.

- **This is the missing half of "my message never arrived".** The
  possibilities are: the peer has not replied yet; the peer replied to a
  different topic; or the reply was published to a topic nobody subscribes
  to, in which case it sits retained on the board and reaches no inbox.
  The third is invisible today unless the topic name can be guessed and
  passed to `board read`. Per-subject reply topics — the documented
  fallback for peers that do not set `in_reply_to`
  (`docs/superpowers/specs/2026-08-09-agentboard-reply-linkage-design.md`)
  — are operated entirely blind to whether anything is listening.

## Goal

Report, for each task known to the board, the topics it subscribes to;
optionally narrowed to the tasks that would receive a given topic.

## Design

### 1. Board API

```go
// SubscriberRow is one task's subscription set with the identity captured
// on its taskState.
type SubscriberRow struct {
    Task         protocol.TaskID
    Hostname     string
    AgentProfile string
    Patterns     []string
}

// ListSubscribers returns one row per task known to the board. A non-empty
// topic narrows the result to the tasks that would receive a publish to
// that topic; Patterns still holds each row's full set. Order is
// unspecified.
func (b *Board) ListSubscribers(topic string) []SubscriberRow
```

The filter calls `taskState.matches(topic)` (`agentboard/taskstate.go:45-49`)
— the same predicate `Board.Send` uses to pick delivery targets
(`agentboard/board.go:224-228`). Reusing the function rather than
reimplementing exact-match comparison is what keeps the view from
diverging from actual delivery if matching ever gains wildcards.

### 2. Wire schema

`runner/protocol/message.bgn`, appended to the end of `TaskControlKind` so
existing ordinals stay stable — the convention `list_conns` records at
`:255-257`:

```
    board_subscribers       # list each task's agentboard subscription set,
                            # optionally narrowed to one topic. Requires
                            # info_global.

format BoardSubscribersRequest:
    request_id :u32
    topic_len :u16
    topic :[topic_len]u8

format BoardSubscriberRow:
    task :TaskID
    hostname_len :u8
    hostname :[hostname_len]u8
    agent_profile_len :u8
    agent_profile :[agent_profile_len]u8
    patterns_len :u16
    patterns :[patterns_len]SubscriptionPattern

format SubscriptionPattern:
    name_len :u16
    name :[name_len]u8

format BoardSubscribersResponse:
    request_id :u32
    rows_len :u16
    rows :[rows_len]BoardSubscriberRow
```

An empty `topic` means "no filter". A variable-length array nested inside a
variable-length array is already used on this wire — `WaitResponse.msgs` is
`[msgs_len]DeliveredMessage` and `DeliveredMessage` is itself variable
length (`agentboard/agentboard.bgn:118-123, 86-113`).

The row carries `task`, `hostname` and `agent_profile`, matching what
`BoardMessageRow` reports about a sender. `runner_id` is omitted: `task` is
already unique, and the remaining two fields are the human-readable part.

**No connection count.** `taskState.conns` is populated by `Attach` and
`Detach` (`agentboard/board.go:111-143`), and `harness-cli` is a
short-lived process per subcommand — a perfectly healthy agent has zero
attached connections almost all of the time. Publishing that number would
read as "nobody is connected" and mislead exactly the diagnosis this
surface exists for. Subscription state, which persists across those
reconnects by design (`agentboard/taskstate.go:9-15`), is the honest
signal.

### 3. Server

`handleBoardSubscribers` in `server/board_handler.go`, dispatched beside
its siblings in `server/task_handler.go:481-495`, gated on
`Capability_InfoGlobal` in the `server/capabilities.go:23-36` table —
matching `BoardTopics` and `BoardRead` (`:32-33`). This is a board-wide
sweep across every task, so it belongs with the other global-info reads.

Unlike `list_conns`, there is no own-subtree narrowing for confined
callers: `board_topics` does not narrow either, and a partial subscriber
list would answer "is anyone listening" wrongly rather than incompletely.
Callers without the capability get the existing `permission_denied`.

**Row lifetime.** A `taskState` is created by `RegisterTask` when the task
is assigned to a runner and destroyed by `Revoke` on `TaskFinished`
(`agentboard/board.go:81-97, 148-167`). `RegisterTask` seeds
`chat.<short-id>` and sets hostname to the empty string; `Attach` fills
hostname in when the task first runs a `harness-cli` command
(`board.go:111-135`). A row with an empty hostname is therefore a task
that is registered and subscribed but has not yet spoken — a real state,
documented so it is not read as missing data.

### 4. CLI

```bash
harness-cli board subscribers            # every task and its patterns
harness-cli board subscribers chat.a1b2  # only tasks that would receive it
```

A new verb in `RunBoardSubcmd` (`cli/cmd_board.go:33-44`), alongside
`topics` / `read` / `purge`, plus `Client.BoardSubscribers` and the
standalone dial form in `cli/board.go` following `BoardRead`
(`cli/board.go:69, 171`) so both the long-lived-client and one-shot paths
exist.

Output is sorted by task id before printing: `ListSubscribers` iterates a
map, and the existing `ListTopics` contract already declares order
unspecified (`agentboard/board.go:431-433`). Sorting in the CLI keeps
output and tests deterministic without constraining the board.

`board_subscribers` needs no new `--caps` value; `info_global` already
gates it, and the `harness-cli` skill's capability section gains it in the
list of what `info_global` unlocks.

### 5. TUI and WebUI

Both board views gain a subscribers pane for the selected topic, per the
rule that a feature is reachable from all three UIs unless functionally
impossible:

- TUI: a key on the topic list opens the subscriber rows, following the
  existing `boardMessages` mode transition (`tui/board.go:27, 58-64, 179`).
- WebUI: a subscribers section in the board panel, beside the existing
  `boardRead` call (`webui/static/main.js:3429`).

Both show task id, hostname, agent profile and pattern set — the same
columns as the CLI.

## Testing

- `agentboard`: `ListSubscribers` with no filter returns every registered
  task including one that has only been `RegisterTask`ed (empty hostname);
  with a filter returns exactly the tasks `Send` would deliver to for that
  topic; a `Revoke`d task disappears; `Patterns` holds the full set even
  when the row was selected by filter.
- `server`: `info_global` required, `permission_denied` without it; empty
  request topic means no filter.
- `cli`: output sorted by task id; both the client and one-shot dial paths.
- `tui` / `webui`: the subscriber rows render for a selected topic.

Verification runs through the existing `make` targets, with the surface
driven against a locally started dummy server and runner before it is
called done.

## Relationship to `in_reply_to`

Independent. `docs/superpowers/specs/2026-08-09-agentboard-reply-linkage-design.md`
makes a reply say what it answers and lets the server derive its
destination; this makes the board say who would receive a publish. They
share no schema. Reply-destination derivation removes the common case of
publishing where nobody listens, but not the explicit-`--topic` path nor
peers that never set `in_reply_to`, which is what this surface covers.
