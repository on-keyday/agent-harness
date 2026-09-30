# Stream sessions: show that the agent is working, while it is working

Date: 2026-09-30

**Status: IMPLEMENTED.** Every design point below is marked DECIDED. See
§Implementation notes at the end for what was measured. It is sequenced BEFORE
[`2026-09-30-stream-ask-user-question-design.md`](2026-09-30-stream-ask-user-question-design.md)
(operator, 2026-09-30).

Builds on [`2026-08-20-event-stream-agent-design.md`](2026-08-20-event-stream-agent-design.md).

## Problem

A stream-kind chat shows nothing while the agent generates. Nothing
distinguishes a long thinking phase, or a long answer being written, from a
hung process. The operator observed it on 2026-09-30, after moving the same
work between the TUI chat and claude's own TUI:

- claude's TUI shows generation as it happens;
- the chat shows nothing until a whole block is done.

The operator called this the main weakness of the stream kind.

The chat's `working Ns` ticker counts time since the operator's own send. It
says nothing about whether the agent is producing anything.

## What was measured

Against claude `2.1.284` / Opus 5.5 on 2026-09-30, with
`claude -p "<a prompt that needs thought>" --output-format stream-json
--verbose --include-partial-messages`. Only the event types were counted:

```
system/init, hook_*, status          (startup)
stream_event message_start
stream_event content_block_start     thinking
system/thinking_tokens         ×2    ← arrives DURING thinking
stream_event content_block_delta     thinking_delta ×2, signature_delta ×1
assistant                            (the complete thinking block)
stream_event content_block_stop
stream_event content_block_start     text
stream_event content_block_delta     text_delta ×16
assistant                            (the complete text block)
stream_event content_block_stop / message_delta / message_stop
result/success
```

From that run:

- **`--include-partial-messages` works with `-p` / stream-json.** The Agent
  SDK docs name it as the SDK's `include_partial_messages`; here it was the
  CLI flag, and it produced the events above. With it, a block's start and
  its deltas arrive as they are generated.
  - The docs (code.claude.com/docs/en/agent-sdk/streaming-output) add that
    tool input streams as `input_json_delta`.
  - The same docs note that deltas are the main session's only: a subagent's
    token deltas are not forwarded.
- **`system/thinking_tokens` already arrives during thinking**, flag or not.
  It carries an estimated token count, and the base spec's neutrality table
  already assigned it to `claude.thinking_tokens`.
  - The adapter drops it: `addClaudeExtras` only decorates events the
    decoder produced, and the decoder produces none for a `system` line
    that is not `init`.
  - So a liveness signal claude sends today never reaches the chat.
- Thinking deltas carry no readable text on Opus 5.5, consistent with the
  earlier `-p` measurement. Their arrival is still a signal.

That is one run. It proves the events exist; it does not measure their rate
on a long turn. The E2E below does that.

## Design

### 1. A neutral `progress` message — DECIDED

In `runner/streamagent/proto.go`:

- A new message kind, `KindProgress MsgKind = "progress"`, carrying
  `Progress *Progress`:

  ```
  Progress { Phase string; Tool string; Tokens int; Chars int }
  ```

- `Phase` is one of:
  - `thinking`
  - `text` (the answer being written)
  - `tool_input` (a tool call's arguments being written; `Tool` names the
    tool)
- `Tokens` is the latest estimate claude gave for the phase: `thinking_tokens`
  during thinking, 0 when claude gave none.
- `Chars` is how much the phase has produced so far, counted from the deltas
  in runes. Thinking produces none on Opus 5.5, so its count stays 0 and
  `Tokens` carries the phase.

**A message kind, not an event kind — DECIDED.** An old reader must ignore
it, and the two options behave differently:

- An unknown `EventKind` renders through `ToAgentlog` as a raw event with
  empty text. An old TUI chat would append one blank line per progress
  message, which is a flood.
- An unknown `MsgKind` has no `RenderText` and no `DisplayOf` in an old
  build, so an old chat, an old `session stream attach` and an old runner
  tap all skip it.

That makes it additive under the rule on `ProtocolVersion`: an old reader's
default reading (ignore) is safe, so the version stays 1.

### 2. The adapter turns deltas into progress, at a bounded rate — DECIDED

In `runner/streamagent/claude.go`:

- **Flag.** `--include-partial-messages` joins `vendorFlags`. It does not join
  `conflicting`: a duplicate boolean flag disables nothing, which is the
  list's test.
- **Parsing.** `stream_event` lines are consumed by the adapter, the way
  `control_request` already is, and never reach the agentlog decoder. The
  decoder's `default: return nil` already ignores them, so this is about
  intent, not a fix. The adapter tracks the current content block:
  - `content_block_start` sets the phase (and the tool name for `tool_use`);
  - `content_block_delta` adds its text's rune count to `Chars`. The deltas
    are `text_delta`, `thinking_delta` and `input_json_delta`;
    `signature_delta` counts nothing;
  - `content_block_stop` ends the phase.
- **Thinking tokens.** `system/thinking_tokens` updates `Tokens` for the
  thinking phase. This is where that line stops being dropped.
- **Rate.**
  - Emit one `progress` at a block's start.
  - After that, emit at most one per second while the phase is still
    producing, carrying the running totals.
  - Emit nothing on stop. The complete `assistant` message that follows
    produces the ordinary event, and that is what the chats act on.
  - A phase that produces nothing emits nothing more. This keeps
    "produced nothing for 30 s" visible as silence, which is the honest
    reading.
- **Why that rate.**
  - The ring holds about 1 MiB, and a progress line is about 120 bytes.
    Ten minutes of continuous generation is at most about 600 lines, about
    72 KB.
  - One update per second is the resolution the status line needs.
  - Nothing is lost by throttling: the complete block still arrives
    verbatim.

### 3. Surfaces — DECIDED

| Surface | Change |
|---|---|
| `streamagent.DisplayOf` | `KindProgress` → `Display{SetStatus: true, Status: <from ProgressStatus>}`, with no `Text`: it updates the status line and adds no transcript line. A replayed ring's progress is superseded by whatever follows it, because a `finish` resets the status as it does today. |
| `streamagent.ProgressStatus(Progress)` | The one wording, used by both chats: `thinking… 1.2k tokens`, `writing… 340 chars`, `→ Bash: writing input… 1.1k chars`. It shows the numbers even at zero, because `thinking… 0 tokens` is still a measurement (surface-parity item 31). |
| TUI chat | Shows the status as it already shows `thinking…` / `running X…`, and the elapsed ticker stays beside it. The status now changes while the agent works, which is the whole point. |
| WebUI chat | `chatSetStatus` from the same Display, through the existing `displayForJS`. The page adds no code beyond what `chatApplyDisplay` already does. |
| runner task log | Not written. `RenderText` has no case for `KindProgress`, so the tap skips it, like hello. The log is a record, and progress is a heartbeat. |
| `session stream attach` (CLI) | **omitted**. A line per second would flood a follow view that is also a log you can pipe. The CLI follower shows what completes, as now. |
| await-idle / `act=busy` | No change of code. Progress lines are stream output, so the mux's quiescence detector now sees a thinking agent as busy. That improves accuracy: a stream task that is thinking used to read as idle. |

### 4. What this does not do

- **Stream the answer's text into the transcript.** That is claude-TUI
  parity: a line that grows as it is written. It needs a transcript line
  that is replaced in place on both chats, and it is a separate design
  (operator, 2026-09-30: this spec first, with the scope as proposed).
- **Subagent progress.** The vendor does not forward a subagent's deltas.

## Tests

- **Adapter**, against the fake agent: a scripted sequence of
  `content_block_start` / `delta` / `stop` plus `system/thinking_tokens`.
  - Progress is emitted at block start with the right phase and tool.
  - The totals accumulate.
  - The one-per-second rate holds. The test uses a fake clock.
  - Nothing is emitted for `signature_delta` alone, or after stop.
  - A `stream_event` never reaches the decoder, so it never becomes a raw
    event.
  - `thinking_tokens` is no longer dropped.
- **`DisplayOf` / `ProgressStatus`**: each phase's wording, the zero case,
  and no transcript text.
- **TUI**: a progress line updates the status and adds no line. A following
  `finish` resets it.
- **Old-reader safety**: a `progress` line fed to the previous release's
  decode path is ignored. The test pins that `DisplayOf` and `RenderText` of
  an unknown kind stay `ok=false`, because a future change there would
  re-open the flood.
- **End to end, against real claude**:
  - Give a stream session a prompt that thinks for a while.
  - Sample the TUI chat's status line through `session snapshot` every
    couple of seconds, under `script(1)`, and confirm it changes during
    thinking and during writing.
  - Count the progress lines the ring took for the turn, and check that
    against the one-per-second bound.
  - Confirm the WebUI chat's status changes the same way.

## Implementation notes

- **Where the code lives.**
  - `runner/streamagent/proto.go`: `KindProgress` and `Progress`.
  - `runner/streamagent/claude.go`: the flag, `handleStreamEvent`,
    `thinkingTokens`, and the rate (`progressInterval`).
  - `runner/streamagent/display.go`: `ProgressStatus` and the `DisplayOf`
    case.
  - Tests: `runner/streamagent/progress_test.go`.
  - No change to the TUI or the WebUI: both already apply `Display.SetStatus`.
- **Measured live** (dummy harness, Opus 5.5, 2026-09-30):
  - **TUI chat.** A turn sent from the chat was sampled every 2 s through
    `session snapshot`. The status line read `thinking… 0 tokens` →
    `→ Bash: writing input… 0 chars` → `running Bash…` → done. That turn's
    ring held **5** progress lines: thinking start, thinking at 274 tokens,
    Bash input start, Bash input at 264 chars, answer start.
  - **WebUI chat.** A turn sent from the page was sampled every 0.5 s. The
    status climbed `thinking… 0 tokens` → `100` → `200` … `1.3k tokens` at
    about one step per second, then showed `writing… 0 chars` →
    `writing… 158 chars`.
  - **Thinking deltas.** On Opus 5.5 they carried no text, so the thinking
    phase is measured by `thinking_tokens` alone. That is the signal the
    adapter used to drop.
