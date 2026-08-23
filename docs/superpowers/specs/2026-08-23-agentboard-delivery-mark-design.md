# The delivery mark moves to the server, and a synchronous wait stops waking its own PTY

Date: 2026-08-23

## Problem

- **A synchronous wait's own delivery also types into the agent's terminal.**
  `Board.Send` builds the target list from pattern matches and then, in one
  loop, both pings every attached connection and calls `onDeliver` per target
  (`agentboard/board.go:255-285`). `onDeliver` is the server's wake hook
  (`server/agent_wake.go:66-69`) — it emits `TaskWake`, the runner receives it
  (`runner/connect.go:517`) and `Session.WakeStdin` writes the marker text and
  a lone `\r` into the agent's PTY (`runner/session.go:937-970`). Nothing in
  that path asks whether this task is the one that asked for the message. So a
  script blocked in `harness-cli agent wait --topic build.done` gets its JSON
  line *and* the interactive agent sharing that task id gets a
  `<harness:agentboard-wake>` prompt typed into it, about a message that is not
  addressed to it and that the script has already taken.

- **`wait --since-last` advances the hook's cursor past other topics'
  undelivered messages.** The cursor at `~/.cache/harness/agent-cursor-<task>`
  is one board-global seq watermark (`cli/agent/cursor.go:13-27`).
  `Board.Inbox` filters *every* subscribed topic by it
  (`agentboard/board.go:294-316`), but `WaitResponse.NextCursor` is the maximum
  seq of the **one** topic the wait returned
  (`server/agent_handler.go:515-520`). `wait --since-last` writes that value
  into the live cursor unconditionally (`cli/agent/wait.go:96-98`). A message
  on another subscribed topic whose seq sits between the old and new cursor is
  then skipped by the next `inbox --since-last --commit` — the hook reads from
  the live position (`cli/agent/inbox.go:76-77`), so the message never reaches
  the agent's prompt context. It remains visible to a manual peek from the prev
  snapshot, which is not an automatic path.

- **`Board.Wait` subscribes permanently as a side effect.** Waiting adds the
  topic to the task's persistent pattern set and never removes it
  (`agentboard/board.go:325-327`). One `wait` on another task's
  `chat.<short-id>` leaves that subscription in place for the rest of the
  task's life, and `Unsubscribe` is the only way back.

- **`dispatch` cannot tell a reply from any other traffic.** It sends, then
  waits on `--reply-topic` with `Since: 0` (`cli/agent/dispatch.go:118-123`).
  Every message already retained on that topic satisfies the wait, and nothing
  ties the returned message to the seq that was just published. The
  `in_reply_to` linkage the board already has
  (`agentboard/agentboard.bgn:136-140`) is not used, and neither is the
  server-side reply routing that sends a reply to the original sender's own
  topic (`server/agent_handler.go:156-168`).

- **`agent inbox --stop-hook` is a mode with no installer.** The runner injects
  exactly one hook (`runner/settings.go:37-39`), and a Stop hook is not it:
  `runner/settings.go:64-71` records the retirement and its reason — a
  Stop-hook re-entry holds Claude Code's stdin while the turn continues, so the
  `WakeStdin` marker is deferred until the agent exits and messages get
  processed in an autonomous chain instead of a user turn. `pruneStaleHarnessHooks`
  (`runner/settings.go:138`) deletes any `harness-cli ` hook not in the current
  list, so a Stop hook installed by an older runner is removed from the worktree
  (`runner/settings_test.go:225-226,303`). The flag, `emitStopHookOutput`, the
  mutual-exclusion check and two test files are all that remain of it.

- **Underneath all of the above: the delivery position is on the wrong side.** The
  board keeps topics, rings and subscriptions in memory only — `Config.SeqSeed`
  exists precisely because "b.seq lives only in memory, so a bare restart would
  reset it to 0" while the cursor file does not
  (`agentboard/board.go:18-28`). The mark that indexes into the ring therefore
  outlives the ring it indexes. It is also a single scalar covering many
  topics, written by `os.WriteFile` with no locking from any process holding
  the task's ticket (`cli/agent/cursor.go:67-73`), so two writers can move it
  backwards or past each other's work.

  The result is documented in the shipped skill as **"Known issue —
  `--since-last` can desync"**, whose stated remedy is to run
  `harness-cli agent inbox --json` with no cursor at all
  (`.claude/skills/harness-cli/SKILL.md:149-161`). The wake marker itself tells
  agents to read that same way (`runner/session.go:78`). The cursor's only
  production writer is one hook line (`runner/settings.go:38`); every other
  documented path routes around it.

- **No design record survives for the peek/commit split.** The original spec's
  cursor section is four bullets of mechanism with no rationale
  (`docs/superpowers/specs/2026-04-28-agent-comms-design.md` §9.2), and at that
  point `--commit` did not exist — the hook was `inbox --since-last --json`
  (§10.4). `prev`/`live` and `--commit` arrived the next day in
  `0e751b1`, a bug-fix commit. Searching `docs/` for `peek`, `prev-cursor` and
  `prev cursor` returns only unrelated WebUI-preview and relay hits, so the
  split has no spec section to preserve. That commit's entire change to
  `wait.go` was widening the `SaveCursor` call to the new three-argument form.
  `inbox` gained "peek by default, `--commit` to advance"; `wait` kept
  advancing unconditionally. The second problem bullet above is that omission.

## Goal

Every bullet above is addressed by this change. Specifically:

1. A task blocked in a synchronous wait on topic T stops receiving `TaskWake`
   for publishes to T. Other tasks subscribed to T keep receiving it, and the
   waiting task keeps receiving wakes for its other topics.
2. The delivery mark moves into `taskState` on the server, keyed per topic. The
   client-side cursor file, `--since-last`, `--commit`, and the `prev`/`live`
   two-line format are deleted. `--since N` stays on both `inbox` and `wait`:
   it is a caller-supplied query bound, writes nothing, and touches no shared
   position.
3. A wait's subscription lasts exactly as long as the wait, and never removes a
   subscription the task already held.
4. `dispatch` waits for a message that answers the seq it published, on the
   topic the server actually routes replies to.
5. `--stop-hook` and its emitter are deleted, leaving `agent inbox` with one
   hook mode — the one the runner installs. The skill files need no edit for
   this: none of the three mentions `--stop-hook`.

Out of scope, decided rather than deferred:

- **Topic ownership is not added.** Measured on the live board: six tasks, each
  subscribed to exactly its own `chat.<short-id>` and nothing else. Zero
  cross-subscription, zero topics with more than one subscriber. The
  name-derived ownership of `chat.<short-id>` holds without enforcement, and
  the collateral damage `purge --topic` could do to another subscriber has no
  live instance to point at.
- **`SeqSeed` stays.** Its original justification disappears with the cursor
  file, but a seq that stays monotonic across server restarts keeps
  `retract <seq>` / `read <seq>` / log correlation readable. The comment at
  `agentboard/board.go:18-28` is rewritten to say that instead.
- **Commit-on-read is not changed.** The mark advances when the hook emits, not
  when the agent acts on the message. A hook whose output is discarded still
  loses that batch from the automatic path. Moving the mark server-side makes a
  "delivered but unacknowledged" state expressible later; this change does not
  express it.

## Design

### 1. Storage: `taskState` gains two maps

```go
type taskState struct {
	mu        sync.Mutex
	patterns  map[string]struct{} // persistent subscriptions
	waiting   map[string]int      // topic -> live synchronous waiters
	shown     map[string]uint64   // topic -> highest seq already advanced past
	conns     map[*ConnState]struct{}
	rid       protocol.RunnerID
	tid       protocol.TaskID
	host      string
	profile   string
}
```

`waiting` serves both the wake suppression and the wait-scoped subscription,
because they have the same extent:

- `matches(topic)` returns `patterns[topic] != nil || waiting[topic] > 0`.
- Wake suppression for this task on this topic is `waiting[topic] > 0`.

A wait therefore needs no bookkeeping about whether it added the subscription.
If the task was already subscribed, `patterns` still holds it after the wait
returns; if it was not, `matches` goes false again when the count drops to
zero. `snapshotPatterns()` returns the union, so during a wait the topic shows
up in `agent subscriptions` and `board subscribers` — that is accurate: for the
duration of the wait the task really is a subscriber.

`ConnState` gains `done chan struct{}`, closed by `Board.Detach`
(`agentboard/board.go:137-142`), which the server already calls from the
deferred per-connection cleanup (`server/agent_handler.go:48-60`).

### 2. Wire schema

All of it, in one place. `agentboard/agentboard.bgn`.

**`AgentMessageKind` — two values appended at the END of the enum**, after
`retract_response`:

```
    inbox_advance
    inbox_advance_response
```

Appended rather than placed next to `inbox`/`inbox_response`: the enum is a
positional `:u8`, so inserting mid-list renumbers every later value, and a
server and runner at different versions would then decode `purge` as
`list_retained` instead of failing. Appending keeps every existing value where
it is.

**`WaitRequest` — one field appended:**

```
format WaitRequest:
    request_id :u32
    pattern_len :u16
    pattern :[pattern_len]u8
    since :u64
    timeout_ms :u32
    # in_reply_to, when non-zero, filters the wait: only a message whose
    # in_reply_to equals this satisfies it. Non-matching publishes on the
    # topic do not end the wait. Zero means no filter.
    in_reply_to :u64
```

**`InboxRequest` and `InboxResponse` — unchanged.** `since` and `next_cursor`
stay: a runtime with no `UserPromptSubmit` hook (the board currently carries
`codex`, `bash` and `cmd.exe` tasks) polls `inbox` itself, passes the previous
response's `next_cursor` as the next `since`, and never touches a shared
position. What is deleted is the client-side *persistence* of that value, not
the ability to name one.

**Two new formats:**

```
# InboxAdvanceRequest asks for the messages on this task's subscribed topics
# that the automatic injection path has not yet been given, and records them
# as given. The position is the server's (taskState.shown), never the
# client's — the request carries no cursor because a caller has no position
# to assert.
format InboxAdvanceRequest:
    request_id :u32

format InboxAdvanceResponse:
    request_id :u32
    msgs_len :u16
    msgs :[msgs_len]DeliveredMessage
```

**`AgentMessage` union — two arms appended:**

```
        AgentMessageKind.inbox_advance => inbox_advance :InboxAdvanceRequest
        AgentMessageKind.inbox_advance_response => inbox_advance_response :InboxAdvanceResponse
```

`WaitResponse.next_cursor` **stays**: `wait --since N` remains, and the highest
seq returned is the value a looping script resumes from.

### 3. Server

**`Board.Wait`** (`agentboard/board.go:323`) — signature gains the reply filter
and the connection's close signal is now a wake-up case:

```go
func (b *Board) Wait(ctx context.Context, c *ConnState, topicName string,
                     since, inReplyTo uint64) ([]RetainedMessage, bool, error)
```

- On entry, `waiting[topicName]++`; `defer` decrements it.
- The scan filters `m.Seq > since` and, when `inReplyTo != 0`, also
  `m.InReplyTo == inReplyTo`. A non-matching publish pings the connection, the
  loop re-scans, finds nothing, and blocks again.
- The `select` gains `case <-c.done:` returning `(nil, false, nil)`.

`agentHandleWait` (`server/agent_handler.go:484`) currently derives its context
from `context.Background()` with only the client's timeout. That is why
`c.done` is needed: with the waiter count deciding wake suppression, a killed
CLI process would otherwise keep the count above zero — and the agent's PTY
silent — for the remainder of the timeout, up to the five-minute default.

**`Board.Send`** (`agentboard/board.go:277-285`) — inside the existing
per-target loop, `c.ping()` is unchanged and `fn(rid, tid)` is skipped when
that target's `waiting[topicName] > 0`. The check reads the target's own
`taskState`, so a second task subscribed to the same topic is unaffected.

**`Board.Inbox`** is unchanged: it keeps its `since` parameter and returns
every retained message above it on the task's subscribed topics. It does not
read or write `shown`.

**`Board.InboxAdvance(c *ConnState) []RetainedMessage`** is new. Under
`b.mu` then `t.mu` (the existing order, stated at `agentboard/board.go:168`),
for each subscribed pattern it collects `t.since(shown[pattern])` and sets
`shown[pattern]` to the highest seq collected for that pattern. Collecting and
marking happen under the same acquisition, so no window exists in which
messages were returned but the mark did not move.

**`resolveReplyTarget`** is unchanged; `dispatch` now relies on it.

### 4. Agent CLI

**`agent inbox`** — flags after this change: `--json` (accepted, output is
always JSON Lines), `--since N`, `--in-reply-to SEQ` (client-side display
filter), `--user-prompt-submit-hook`. Removed: `--since-last`, `--commit`,
`--stop-hook`. With `--since` omitted the read starts at 0, which is the whole
ring — today's behaviour and what the wake marker instructs
(`runner/session.go:78`).

The advancing read is used **exactly when `--user-prompt-submit-hook` is
given**; otherwise the plain read is used. This is what replaces the
"`Never pass --commit by hand`" instruction in the skill: advancing now
requires claiming to be the one hook the runner installs, and that flag's
output is a `UserPromptSubmit` envelope, not a usable human-facing dump.

`--stop-hook` is deleted along with `cli/agent/stop_hook.go`,
`emitStopHookOutput`, and the now-unnecessary mutual-exclusion check at
`cli/agent/inbox.go:57-59`. `emitMessageLineForHook`
(`cli/agent/json_emit.go:38`) keeps serving the surviving hook mode. Deleting
rather than leaving it: with the Stop hook deliberately retired and actively
pruned from worktrees, no path installs the flag, and carrying it forward would
mean specifying advancing-read behaviour for a caller that the runner removes
on sight.

**`agent wait`** — `--topic`, `--since`, `--timeout`, and new
`--in-reply-to SEQ`. Removed: `--since-last`. `cli/agent/cursor.go` and
`cli/agent/cursor_test.go` are deleted.

**`agent dispatch`** — `--topic`, `--data`, `--timeout`. Removed:
`--reply-topic`. The flow becomes:

1. Publish, and read `seq` from `SendResponse`
   (`agentboard/agentboard.bgn:95-98`).
2. Wait on `agentboard.SelfTopic(own task id)` with `since = seq` and
   `in_reply_to = seq`.

`--reply-topic` is removed rather than kept optional: a reply sent with
`--in-reply-to` and no `--topic` is routed to the original sender's own topic
by the server, so a caller-supplied reply topic can only disagree with where
the reply will actually arrive.

**Hook command line** — `runner/settings.go:38` and the checked-in
`.claude/settings.json` both become:

```
harness-cli agent inbox --json --user-prompt-submit-hook
```

### 5. Operator surfaces

`board subscribers` gains, per task row and per topic, the shown mark and how
many retained messages sit above it: `chat.ab12cd34 shown=1740 pending=2`. This
is the point of moving the mark — the desync the skill currently tells agents
to guess at becomes something an operator can read.

Where the same information is displayed is decided by where board
subscriptions are displayed today: the new columns go on every surface that
already shows them, and a surface that does not show board subscriptions at all
gains nothing here. The `surface-parity-checklist` skill is walked item by item
during implementation, with a verdict recorded per number, including S1–S6.

### 6. Documentation

Three copies of the harness-cli skill (`.claude/skills/harness-cli/SKILL.md`,
`.agents/skills/harness-cli/SKILL.md`,
`runner/agentskills/harness-cli/SKILL.md` — the last is the `go:embed` source)
lose the "Known issue — `--since-last` can desync" section, the `--since-last`
/ `--commit` / peek description, and the `Never pass --commit by hand`
instruction. They gain a description of `agent inbox` as an idempotent read of
the subscribed topics' rings.

The policy that an agent must not call `wait` / `dispatch` from inside a turn is
**unchanged**. Removing the wake misfire does not remove the reason for the
rule: a blocked wait still occupies the agent's bash process for the whole
timeout, during which it cannot reason or send to anyone else. The skill's
"Async by default" section keeps its current wording.

`agentboard/board.go:18-28` (`SeqSeed`) is rewritten: the justification is now
readable seq correlation across restarts, not out-running stale on-disk cursors.

## Error handling

- `wait --in-reply-to` naming a seq that does not exist, or that no one ever
  answers, is not rejected. The wait finds no match and ends in the existing
  timeout path. Validating the parent seq would cost a lookup and produce the
  same outcome — for `dispatch`, the seq is one the same process published
  moments earlier.
- `InboxAdvance` from a connection with no `taskState` returns an empty list,
  matching the existing `c == nil || c.task == nil` guards.
- A topic that is purged or TTL-evicted and later recreated leaves a stale entry
  in `shown` pointing at a seq that no longer exists. Since seq is monotonic,
  every message in the recreated topic has a higher seq, so the entry filters
  nothing. Entries are dropped with the whole `taskState` at `Board.Revoke`.

## Rollout

1. `scripts/wire-skew-check.sh` — `.bgn` changed, so it runs for real rather
   than exiting 0.
2. **Restart the server first.** The runner fleet is on other hosts and stays on
   the old binary until restarted; the reverse order puts a new runner against
   an old server (Pitfall 10).
3. `make build`, then restart runners.
4. The hook command line changes, and `runner/settings.go` writes it at task
   setup, so already-running tasks keep the old line until their next task. The
   old line's `--since-last --commit` flags are removed from `agent inbox`, so
   those tasks' hooks exit non-zero on flag parse until then. This is accepted
   rather than shimmed: there are no external users, the affected window is one
   task lifetime, and a flag-parse failure is loud.

## Testing

Unit, `agentboard/`:

- `Wait` on an unsubscribed topic makes `matches` true for the duration and
  false after; on an already-subscribed topic, `matches` stays true after.
- Two concurrent waits on one topic: `matches` stays true until both return.
- `Send` skips `onDeliver` for a task waiting on that topic, and still calls it
  for a second task subscribed to the same topic.
- `Send` calls `onDeliver` for a waiting task's *other* topics.
- Closing a `ConnState` ends a blocked `Wait` and drops the waiter count.
- `InboxAdvance` returns each message once, per topic, and is empty on the
  second call with no new publishes.
- The regression this change exists for: subscribe to A and B, publish to B,
  publish to A, `Wait` on A, then `InboxAdvance` — the B message is returned.
- `Wait` with `in_reply_to` set ignores a non-reply publish and a reply to a
  different seq.

E2E, `cli/agent/`:

- `dispatch` against a peer that replies with `--in-reply-to` returns that
  reply; a peer that publishes an unrelated message to the same topic first does
  not satisfy it.
- `agent inbox` twice with no publishes between returns the same lines both
  times.

Deleted: `cli/agent/cursor_test.go`, `cli/agent/inbox_peek_commit_e2e_test.go`,
`cli/agent/stop_hook_test.go`, `cli/agent/inbox_stop_hook_e2e_test.go`.
`cli/agent/inbox_prompt_hook_e2e_test.go` is updated to assert the advancing
read rather than cursor-file contents, and loses its
`--stop-hook --user-prompt-submit-hook` rejection case (`:129`).

Live check after rollout: `harness-cli board subscribers` shows a `shown=` /
`pending=` pair for each task, and a `send` to a peer's `chat.<short-id>` moves
that peer's `pending` by one.

## Decisions taken

- **DECIDED (operator, 2026-08-23)** — the policy that agents do not call
  `wait` / `dispatch` inside a turn is kept. This change fixes implementations,
  not the rule.
- **DECIDED (operator, 2026-08-23)** — wake suppression is scoped to
  (task, topic), not to the topic and not to the task.
- **DECIDED (operator, 2026-08-23)** — the delivery mark lives in `taskState`
  on the server, not on the client's disk and not in `topic`. A mark in `topic`
  would leave one entry per finished task in every topic, needing a sweep at
  `Revoke`; `taskState` shares its lifetime with the subscription set it is
  paired with.
- **DECIDED (operator, 2026-08-23)** — `SeqSeed` stays, with a rewritten
  rationale.
- **DECIDED (operator, 2026-08-23)** — topic ownership is not added.
- **DECIDED (author)** — the mark is per topic rather than one scalar per task.
  With one subscribed topic per task, as measured, the two behave identically
  today. Per-topic is chosen because the defect being fixed is a partial reader
  advancing a shared position, and per-topic removes the shape of that defect
  rather than restating the rule that only one caller may advance it.
- **DECIDED (author)** — peek is dropped along with `prev`. Its purpose was
  reading without moving the live position, which the plain read now does for
  every caller.
- **DECIDED (operator, 2026-08-23)** — `--since N` stays on `inbox` as well as
  on `wait`, and `InboxRequest.since` / `InboxResponse.next_cursor` stay on the
  wire. The defects are in the *persisted, shared, unlocked* position, not in a
  caller naming a bound for one read. A polling runtime with no
  `UserPromptSubmit` hook needs it, and removing it from `inbox` while keeping
  it on `wait` would have been inconsistent for no gain.
- **DECIDED (author)** — the advancing read is a separate wire kind and is
  selected by `--user-prompt-submit-hook`, not by a flag of its own.
- **DECIDED (operator, 2026-08-23)** — `--stop-hook` and its emitter are
  deleted rather than carried forward. The Stop hook was retired deliberately
  (`runner/settings.go:64-71`) and worktrees are actively pruned of it
  (`runner/settings.go:138`), so the flag has no installer.
- **DECIDED (author)** — `dispatch --reply-topic` is removed, not deprecated.
- **DECIDED (author)** — `taskState` keeps its `ticketKey{runner, task}`
  keying. A task that finishes loses its `taskState` at `Board.Revoke`, and
  with one subscriber per topic that also destroys the topic and its ring, so a
  resumed task on a different runner has an empty ring and nothing for a
  carried-over mark to filter.
