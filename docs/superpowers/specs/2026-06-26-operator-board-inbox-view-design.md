# Operator-facing agentboard inbox view — design

Date: 2026-06-26
Status: approved design, pre-implementation

## Problem

The operator (the human running TUI / WebUI / `harness-cli`) has **no way to see
what is sitting in the agentboard** — neither the list of topics nor the
messages retained on any of them. The only board readers are in-task **agents**:
`inbox`/`wait` deliver a caller's own subscribed topics, and the `agent
retained`/`agent purge` verbs added on 2026-06-26 are **agent-plane** (kind=Agent
+ per-task auth ticket), so the operator's own shell — which has no ticket —
cannot call them, and the TUI/WebUI (operator/kind=Client connections) have no
agentboard surface at all.

Concretely the operator wants to: (a) see each agent's inbox contents to
understand what's flowing, and (b) drop a poisoned/unwanted payload from the
server buffer — including the case where the payload itself trips a moderation
gate the instant it enters an LLM agent's context, so a human must inspect it
out-of-band and purge it.

This is structural, not a missing button: agentboard identity is the **task**
(`(runner_id, task_id)` proven by ticket; see
`2026-04-28-agent-comms-design.md`), and the operator is not a task. So operator
board access must go through the **operator plane (TaskControl)**, where the
server already holds a `*agentboard.Board` reference (`TaskHandler.Board`) and
can call board methods directly without a ticket.

## Goals (MVP)

- Operator can **list agentboard topics** (name, message count, last activity).
- Operator can **drill into one topic and read its retained messages WITH
  content** (a human reads them in a UI; no LLM classifier is involved).
- Operator can **purge** a topic's buffer — whole topic or a single `seq`.
- Available from **all three surfaces** — `harness-cli`, TUI, WebUI — over one
  shared server-side RPC family (per the all-3-UIs project norm).
- Topic rows are **associated back to a task** client-side: a `chat.<first-8-hex>`
  topic is matched against the task list the client already holds and shown with
  that task's label (prompt/repo/host).

## Non-goals (MVP — explicitly deferred)

- Live message-flow streaming / who→whom timeline (separate, heavier feature).
- True per-agent inbox (the *union* of a task's subscribed topic rings):
  navigation is **topic-keyed**; per-agent union is deferred. The `chat.<id>`
  convention makes topic-keyed read as per-agent for the common 1-topic case.
- Read/unread state: the consumer cursor lives client-side on each agent host,
  so the server cannot compute "pending vs already-delivered" — not shown.
- Sending messages from the operator UI.

## Architecture

One new **operator-plane RPC family** (TaskControl), three thin clients.

```
WebUI panel ─┐
TUI view    ─┼─> cli.Client (operator, kind=Client) ─> TaskControl RPC ─> TaskHandler ─> s.Board
harness-cli ─┘                                                                            (ListTopics / ListRetained / PurgeTopic / PurgeSeq)
```

The server services every verb by calling the methods already on
`*agentboard.Board` (no ticket, no `helloed` — that gate is agent-plane only).
The three clients are thin: each renders the same response.

### Verbs (Approach A — granular)

Three `TaskControlKind` verbs, each single-purpose, mirroring the existing
granular operator verbs (`list`, `list_conns`):

- `board_topics` — cheap topic overview. cap: `info_global`.
- `board_read {topic}` — one topic's ring **with payloads**. cap: `info_global`.
- `board_purge {topic, seq}` — whole (`seq==0`) or single-seq purge. cap: `purge`.

`board_topics` and `board_read` are reads of *global* board state (topics are
not task-scoped), so they gate on `info_global` exactly like `agentHandleListTopics`
— operator (zero principal) and any `all`-holder pass. `board_purge` reuses the
destructive `purge` capability (added 2026-06-26). All three deny with a
`denied` status rather than a silent empty.

### Bulk transport for `board_read`

Payloads can be up to 64 KiB each × up to 64 ring entries (~4 MiB) — too large
to inline in a control message. Mirror the established `get_task_log` pattern:
`BoardReadResponse` inlines **per-message metadata** (seq / from_task /
hostname / size / time) and a `stream_id`; the server writes each message's
payload **concatenated in `msgs` order** into a unidirectional send-stream
(EOF-closed). The client reads `msgs[i].size` bytes per row from the receive
stream (`trsf.Transport.GetReceiveStream`). `board_topics`/`board_purge`
responses are small and stay inline.

## Wire schema (authoritative — `runner/protocol/message.bgn`)

Append to `TaskControlKind` (keeps existing ordinals stable):

```
    board_topics
    board_read
    board_purge
```

New types:

```
enum BoardStatus:
    :u8
    ok
    not_found
    denied

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
    status :BoardStatus           # ok | denied
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
    size :u32                     # payload byte length; payload arrives on the response stream

format BoardReadResponse:
    request_id :u32
    status :BoardStatus           # ok | not_found | denied
    stream_id :u64                # 0 unless status==ok with >=1 msg; else a server
                                  # send-stream carrying each msg's payload concatenated
                                  # in `msgs` order. Client reads msgs[i].size bytes per row.
    msgs_len :u16
    msgs :[msgs_len]BoardMessageRow

format BoardPurgeRequest:
    request_id :u32
    topic_len :u16
    topic :[topic_len]u8
    seq :u64                      # 0 = whole topic; >0 = single seq

format BoardPurgeResponse:
    request_id :u32
    status :BoardStatus           # ok | not_found | denied
    purged :u16
```

Match arms in both `TaskControlRequest` and `TaskControlResponse`:

```
TaskControlKind.board_topics => board_topics :BoardTopicsRequest   # / BoardTopicsResponse
TaskControlKind.board_read   => board_read   :BoardReadRequest     # / BoardReadResponse
TaskControlKind.board_purge  => board_purge  :BoardPurgeRequest    # / BoardPurgeResponse
```

`TaskID` / `RunnerID` are the existing `runner/protocol` types (same file). After
editing, regenerate via `make protoregen ARGS='runner/protocol/message.bgn'`.

## Server (`server/task_handler.go`)

Three handlers dispatched from the TaskControl switch, each calling the existing
`h.Board` methods (already present for ticket lifecycle):

- `handleBoardTopics`: `info_global` gate (else `denied`); `h.Board.ListTopics()`
  → `BoardTopicRow[]`.
- `handleBoardRead`: `info_global` gate; `h.Board.ListRetained(topic)` →
  metadata rows; if `>=1` msg, `conn.CreateSendStream()`, respond with its id,
  then async-write payloads concatenated in row order, close on EOF (mirror
  `handleGetTaskLog`). `not_found` when the topic doesn't exist.
- `handleBoardPurge`: `purge` gate; `seq==0` → `h.Board.PurgeTopic`, else
  `h.Board.PurgeSeq`; respond `ok`+count / `not_found` / `denied`.

Caps wiring: `board_purge` → `requiredCap[...] = Capability_Purge`. The two reads
gate on `Capability_InfoGlobal` inline in their handlers (like
`agentHandleListTopics`), since `requiredCap` is for non-info kinds.

## Clients (thin — one shared `cli.Client`)

New `cli/board.go` exposing `BoardTopics(client)`, `BoardRead(client, topic)`,
`BoardPurge(client, topic, seq)` plus `*With(client)` variants (per the
long-lived-client norm). Each surface calls the `*With` form against its existing
client.

- **CLI** — `cmd/harness-cli`: `harness-cli board topics | read <topic> | purge
  <topic> [--seq N]`. `board read` prints each message with its decoded payload
  (UTF-8; pretty-printed if valid JSON) and a header (`seq`, resolved task label,
  host, size, time).
- **TUI** — new view: topic list → select → message list with content; a key
  binds purge (whole + per-row seq). Threads `a.client`.
- **WebUI** — new panel matching the dark palette (`#1e1e1e`/`#d4d4d4`) with a
  `<=390px` layout from the first cut: topic list → click → message cards with
  content + per-message and whole-topic purge buttons. Uses `currentClient()`.

**Topic→task association (client-side, no server change):** for a topic named
`chat.<8hex>`, match the prefix against the task list the client already holds
and show that task's label; non-`chat.` topics show the raw name.

**Refresh:** snapshot fetch on open + a manual refresh control; the WebUI may
piggyback its existing snapshot poll cadence. No live streaming in MVP.

## Testing

- **Server**: handler tests for all three verbs — `info_global` present/absent
  (reads → `denied` empty), `purge` present/absent (`board_purge` denied),
  `board_read` stream carries exactly the payloads in row order, `not_found` for
  an unknown topic. Mirror `server/*_test.go` + the `agentboard` test helpers.
- **CLI e2e**: `board topics/read/purge` against an in-process server (mirror the
  `cli/agent` e2e harness, operator-plane variant): seed a topic via a board
  `Send`, assert list/read/purge round-trips and the read payload content.
- **WebUI**: Playwright — panel lists topics, drill-down shows decoded content,
  purge button drops it; verify desktop **and** 390px.

## Decisions taken (no implementer choices left)

1. **Approach A** (3 granular verbs), not a single overloaded `inspect` verb, and
   not opening agentboard handlers to operator callers (that would break the
   agent-plane=ticket / operator≠board-agent model).
2. **`board_read` streams payloads** via a `stream_id` (mirror `get_task_log`),
   never inline-bulk and never truncated.
3. **Topic-keyed navigation** with **client-side** `chat.<8hex>`→task mapping;
   per-agent multi-topic union is a non-goal.
4. **Snapshot + manual refresh** for MVP; no live message stream.
5. Caps: `board_topics`/`board_read` → `info_global`; `board_purge` → `purge`.
6. Content shown as UTF-8 / pretty-JSON; raw bytes are a human's to read (no
   classifier on a UI), which is the whole reason this lives on the operator
   plane and not as another LLM-facing agent verb.
```
