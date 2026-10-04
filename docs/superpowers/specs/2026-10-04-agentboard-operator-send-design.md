# The operator publishes to the agentboard under its own name

Today only an agent connection can publish. `handleAgentSend`
(`server/agent_taskcontrol.go`) answers `bad_frame` when `boardState(conn)` is
nil, and `BoardConnState` (`server/server.go`) returns nil for any connection
that never completed an agent ClientHello. The refusal is principled — a
publish needs an authenticated sender to stamp — but it leaves the operator
with no way onto the board except borrowing a task's identity.

That borrowing is already in use. `examples/memory-viewer/memviewer.py` runs
`agent send` with the env of whichever task launched it, so every message it
sends is stamped as that task — usually the recipient itself. Its `TOOL_NOTE`
comment records the cost: the recipient cannot tell from the envelope that a
human sent it or that there is nowhere to reply, and the first recipient
(2026-09-11) spent four tool calls deriving that. The fix in the viewer is a
fixed sentence in the body. For an operator who sends directly, that sentence
becomes a field.

## Decisions taken

| Decision | Decided by |
|---|---|
| The operator gets its own publish path; `agent send` is not changed | operator, 2026-10-04 |
| `--no-wake` exists on the operator path only; the agent face cannot express it | operator, 2026-10-04 |
| Replies: possible only when the sender gives `--reply-to`; otherwise the message has no reply route | operator, 2026-10-04 |
| A new capability bit gates it, because it publishes in the operator's name | operator, 2026-10-04 |
| A wake-only verb, for "queue with `--no-wake`, then wake once"; runner and wake text unchanged | operator, 2026-10-04 |
| Wake-only does not check for unread messages | operator, 2026-10-04 |
| The operator verb takes `--in-reply-to`: replying to an agent is a common use | operator, 2026-10-04 |
| `SenderKind` includes `server`, replacing the `from_hostname == "server"` convention | operator, 2026-10-04 |
| CLI, TUI and WebUI all get both verbs | operator, 2026-10-04 |
| memviewer stays as it is | operator, 2026-10-04 |
| Replies to the operator go to the reserved topic `chat.operator`; agents may not subscribe to it | operator, 2026-10-04 |
| `chat.operator` keeps the ordinary topic TTL | operator, 2026-10-04 |

Why no-wake is kept off the agent face: an agent that uses it by
misunderstanding gets a message that is retained but not acted on, and reports
it as undelivered. A flag present in `agent send --help` would be found and
used; absence from the request format is the only form of "agents don't use
this" that does not depend on the agent reading a note.

## Scope

**In:** two TaskControl kinds and their verbs (`board send`, `board wake`) on
CLI, TUI and WebUI; one capability bit; a sender-kind field on the retained
message and on every format that echoes a sender, with `fireIdleBoard`
restamped as `server`; a new `SendStatus` for a reply with no route; the
reserved topic `chat.operator`; the inbox renderers that show the sender.

**Out:** any change to `agent send` / `agent dispatch` / `AgentSendRequest`. Any
change to the runner, `TaskWakeRequest`, or `wakeMarker`. A wake that carries
its own text (case (ii) in the 2026-10-04 discussion; `session send` /
`session stream turn` already type free text into one task). Unread detection.
memviewer, which keeps sending through the launching task's env and keeps its
`TOOL_NOTE`.

## Wire

### Sender kind

```
enum SenderKind:
    :u8
    agent
    operator
    server
```

Added to `agentboard.RetainedMessage` and to each format that echoes a sender:
`DeliveredMessage`, `RetainedMeta`, and `BoardMessageRow` (`board read`).
`handleAgentSend` stamps `agent`; `fireIdleBoard` stamps `server`.

The board is in memory only (`agentboard/` has no disk path; a server restart
drops it), so this is a wire change with no disk axis. It is decoded by
clients and agents, not by runners: `scripts/wire-skew-check.sh` still runs,
but no runner restart order follows from it.

`from_task` on an operator message is the caller's principal task: zero for a
real operator connection, the task id when an agent holding the new bit calls
the operator verb. This is the attribution `fireIdleBoard`
(`server/await_idle_handler.go`) already uses for the requester. `from_runner`
gets the same placeholder that function uses (a zero RunnerID panics the
encoder), and `from_hostname` is empty.

`fireIdleBoard`'s publish is identified today only by `from_hostname ==
"server"`, a convention stated in its comment and in `DeliveredMessage`'s.
With `server` stamped it is carried by the field; the hostname stays
`"server"` for older readers, and both comments are rewritten to point at the
kind.

### `BoardSendRequest`

A new TaskControl kind. Its fields are those of `AgentSendRequest`
(`topic`, `payload_stream_id`, `in_reply_to`, `reply_to_topic`,
`no_retire_on_reply`), plus:

- `no_wake :u1` — publish and retain, but do not call `onDeliver`, so no
  `task_wake` is emitted for any subscriber.

`in_reply_to` routes through `resolveReplyTarget` as an agent reply does: to
the parent's `reply_to_topic`, else the parent author's `chat.<short-id>`.
Answering an agent's message therefore needs no `--topic`.

Reply-retire (`retireRepliedParent`) does not fire for an operator reply:
one of its conditions is that the parent sits on the replier's own
`chat.<short-id>`, and the operator has none. An agent's message to the
operator stays on the board until retracted or aged out, which matches the
operator surfaces' audit window.

The handler reuses `handleAgentSend`'s payload-stream read and size limit. It
does not require `boardState(conn)`; the sender identity comes from
`lookupPrincipal`, the source `callerCaps` uses.

### `BoardWakeRequest`

A new TaskControl kind carrying `topic`. It emits `task_wake` to every task
subscribed to `topic`, using the same per-task loop as `Board.Send`, including
its `isWaiting` skip. The response reports the number of tasks woken, as
`delivered_to` does for a send. Nothing is published and nothing is retained.
The runner writes the existing `wakeMarker` ("new message(s) — read via
`harness-cli agent inbox --json` …"), which is accurate in the intended use
because the queued `--no-wake` messages are unread.

### Capability

```
board_send = 0x…, "board_send"
```

It gates both kinds in `requiredCap`. Operator connections hold
`Capability_All` (`callerCaps`), so in practice the bit decides which agents
may publish in the operator's name. It is default-off and appended, so no
existing grant changes meaning.

Wake-only rides the same bit, not a separate one: it writes a prompt into
other tasks' PTYs, and a holder of `board_send` can already do that by sending
without `--no-wake`.

### Replies to an operator or server message

`resolveReplyTarget` (`server/agent_handler.go`) falls back to
`SelfTopic(parent.FromTask)` when the parent has no `reply_to_topic`. That
fallback is only meaningful when the parent's author is an agent. For an
operator message `from_task` may be zero, and so may a `server` message whose
await-idle requester was an operator; the fallback would route the reply to
the chat topic of task id zero. Instead, when the parent's kind is not
`agent`:

- no `reply_to_topic` on the parent, and the reply gives no `--topic`: refuse
  with a new status `no_reply_route`;
- `reply_to_topic` on the parent: route there, unchanged;
- a reply that names its own `--topic`: unchanged.

The CLI error for `no_reply_route` says the message came from the operator (or
the server) with no reply destination and that the answer belongs in the
agent's own conversation — the sentence `TOOL_NOTE` carries today.

The `server` case is a behaviour change for await-idle fired by an agent: today
a reply to it resolves to the requester's own chat topic. Nothing should be
replying to a `session_idle` notice, so this is expected to be invisible.

### `chat.operator`

When the operator replies to an agent without `--reply-to`, the agent's answer
to that reply is refused with `no_reply_route`. To keep a conversation going,
the TUI and WebUI reply actions prefill `--reply-to chat.operator`. The CLI
does not add it implicitly; `board send --help` names it.

The parent's own topic would not work as that destination: it is the agent's
`chat.<short-id>` or a topic the agent subscribes to, so the agent's answer
would wake the agent itself (`Board.Send` delivers to the publisher's own
subscriptions) and reach no one who reads for the operator.

`chat.operator`:

- is a constant beside `SelfTopicPrefix` in `agentboard/ids.go`; it cannot
  collide with a task's `chat.<8-hex>`;
- refuses subscription from an agent connection (`handleAgentSubscribe`
  answers `bad_pattern`), so nothing is woken by a publish there and no agent
  reads the operator's replies through its inbox. It is the first reserved
  name on the board;
- keeps the ordinary topic TTL. A reply the operator does not read before the
  topic ages out is lost.

An agent can already publish to `chat.operator` unprompted, and the WebUI
board view already shows it; this spec adds nothing for that case.

`conversationKey` (`cli/boardthread.go`) takes the suffix after `chat.` as a
party, so an exchange with the operator keys as `<8-hex>+operator` with no
change there. Its other input, the sender's `FromTaskHex`, would add a
`00000000` party for an operator message with zero `from_task`. For a sender of
kind `operator`, the party is `operator` instead.

## Surfaces

| Surface | `board send` | `board wake` |
|---|---|---|
| CLI | `board send [--topic T] [--in-reply-to N] [--reply-to T] [--no-wake] [--data -] text…` | `board wake <topic>` |
| TUI | compose from the board modal (`tui/board.go` `BoardModal`): new message on the open topic, reply on the selected message | action on the open topic |
| WebUI | compose on the board topic view (`openBoardTopic`, `webui/static/main.js`), and a reply action on each `board-msg` card | button on the topic view |

The topic is a flag, as on `agent send`, and may be omitted only with
`--in-reply-to`: a positional topic in front of free-form text would be
ambiguous exactly when it is omitted. Both verbs are one declaration each in
`cli/verb/table.go`. Like the rest of the `board` family they are CLI-command
verbs; the TUI and WebUI reach them through the board panes, not a command
line.

Sender rendering: `agent inbox` / `agent read` JSON and text, `board read`,
`board thread`, the TUI board modal and the WebUI `board-msg-from` span show
the sender kind. An `operator` or `server` message is labelled with its kind
where the task short-id is shown today, and the agent-facing text says
whether it has a reply route.

## Testing

- An operator connection publishes; a subscriber's inbox shows sender kind
  `operator` and zero `from_task`.
- An agent without `board_send` calling `BoardSend` / `BoardWake` is denied
  through the ordinary `requiredCap` path; with the bit, it publishes stamped
  `operator` with its own `from_task`.
- `--no-wake`: the message is retained and appears in the next inbox read;
  `onDeliver` is not called.
- `board wake`: `onDeliver` is called once per subscribed task, a task in
  `Wait` on the topic is skipped, and nothing is retained.
- A reply to an operator or server message: `no_reply_route` without a
  route, routed with the parent's `reply_to_topic`, unchanged with an explicit
  `--topic`.
- An operator `--in-reply-to` an agent message lands on the parent's
  `reply_to_topic` or the author's `chat.<short-id>`, and does not retire the
  parent.
- `fireIdleBoard` messages carry kind `server`.
- An agent subscribing to `chat.operator` gets `bad_pattern`; an agent
  answering an operator reply prefilled with `--reply-to chat.operator`
  lands there and wakes nobody.
- `board thread` keys an agent ↔ operator exchange as `<8-hex>+operator`.
- `agent send --help` does not list `--no-wake`.
- The TUI and WebUI paths are driven with real input (Playwright for the
  WebUI), not only rendered.
