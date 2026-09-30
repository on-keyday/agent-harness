# Stream resume: replay the agent's own transcript to the operator

Date: 2026-09-30

**Status: IMPLEMENTED.** Every design point below is marked DECIDED,
including the one taken with the operator (§Decisions taken with the
operator). §Implementation notes at the end records where the code differs
in detail from the text above, and what was measured.

Builds on [`2026-08-20-event-stream-agent-design.md`](2026-08-20-event-stream-agent-design.md)
(the stream kind, its adapter, and the neutral protocol).

## Problem

A task is resumed by opening it again with `--resume-conversation`, and the
mode is chosen per open. So a conversation started as a PTY session can be
continued as an event-stream session, and a stream session can be resumed as
a stream session again. In both cases:

1. **The agent remembers and the operator does not see.** `--continue` does
   restore the agent's context under the framed stdin (measured in the
   event-stream spec, §Resume: a codeword was recalled and the session id
   stayed the same). But claude's stream-json output carries only events that
   happen *after* the resume. Every stream surface — the TUI chat, the WebUI
   chat, `session stream attach` — therefore starts from an empty transcript.
   The operator cannot see what was just said, which is the context they need
   in order to write the next turn.
2. **The PTY kind does not have this gap.** When claude's own TUI is started
   with `--continue` it redraws the earlier conversation from its session
   storage. So today, resuming as a stream is strictly worse than resuming as
   a PTY.
3. **Persisting the harness's own copy of the events cannot close it.** When
   the conversation started as a PTY session, no event stream ever existed —
   what the harness holds is terminal bytes. Only the agent's own record
   covers that route. (The event-stream spec's "Not in this design" excludes
   persisting the event stream for replay. This design does not reverse that:
   it reads the vendor's record and stores nothing new.)

## What was measured before designing this

**The record exists, and where it lives is documented.** The Agent SDK
sessions page (code.claude.com/docs/en/agent-sdk/sessions) says Claude Code
stores sessions under `~/.claude/projects/<encoded-cwd>/*.jsonl`, or under
`$CLAUDE_CONFIG_DIR/projects/` when that variable is set. `<encoded-cwd>` is
the absolute working directory with every non-alphanumeric character
replaced by `-`. A name longer than 200 characters is truncated and a hash is
appended, and `CLAUDE_CODE_PROJECT_DIR_NAME` overrides the name entirely. The
page also names the official readers, `getSessionMessages()` (TypeScript) and
`get_session_messages()` (Python), "to build … transcript viewers". The
page does **not** describe the line format itself as a stable contract.

**The line format, as observed** (2026-09-30). This is one transcript of an
interactive session, written by claude `2.1.280`: 498 lines, 1.4 MB. Only the
line structure was read, never the message text:

```
python3 -c 'import json,sys,collections
rows=[json.loads(l) for l in open(sys.argv[1]) if l.strip()]
print(collections.Counter((r.get("type"),r.get("subtype")) for r in rows))' <file>.jsonl
```

| `type` | lines | conversation? |
|---|---|---|
| `attachment` | 152 | no — injected context (hook output and similar) |
| `assistant` | 107 | yes — exactly ONE content block per line (`thinking` / `text` / `tool_use`) |
| `user` | 69 | yes — `tool_result` blocks, or a typed prompt whose `content` is a **string** (20 lines) |
| `mode`, `atis-latch`, `last-prompt`, `ai-title`, `file-history-snapshot`, `file-history-delta`, `system` (`turn_duration`, `stop_hook_summary`, `local_command`) | the rest | no — bookkeeping |

- The content blocks of `assistant` and `user` lines have the same shape as
  claude's live stream-json: `type` / `text` / `name` / `input` / `content` /
  `is_error`.
- Every row carries `uuid` and `parentUuid`. In this file the chain was
  perfectly linear: 375 rows pointed at the row before them and 0 did not,
  and all 186 conversation rows lay on the chain from the leaf to the root.
- `isSidechain` was true on 0 rows and `isMeta` on 2.

**What the existing decoder does with it** (read, `runner/agentlog/agentlog.go`):

- Bookkeeping `type`s fall through `claudeStreamJSON.Decode`'s
  `default: return nil`, so they are harmless without any pre-filtering.
- A `user` line whose `content` is a string does NOT decode:
  `claudeEnvelope.Message.Content` is a slice, so `json.Unmarshal` fails and
  the whole line comes out as a `raw` event — the vendor JSON, verbatim. The
  live stream never produces such a line, because the adapter does not pass
  `--replay-user-messages`, which is why this has not come up before.
- A `text` block decodes as `KindText` whether it came from `assistant` or
  `user`. The neutral `Event` has no field that says who spoke.

## Design

### 1. The decoder reads a user's turn — DECIDED

In `runner/agentlog`:

- `claudeEnvelope.Message.Content` becomes a type with its own
  `UnmarshalJSON` that accepts either a block array or a string. A string
  becomes one `text` block, and an empty string becomes no blocks.
- A new `KindUserText` is appended to `Kind`. In the `user` case a `text`
  block becomes `KindUserText`; in the `assistant` case it stays `KindText`.
- `Render` prints `KindUserText` as `you ▶ <text>`. That is the prefix the
  TUI chat (`tui/chat.go`) and the WebUI chat (`main.js`) already use when
  they echo a turn they sent, so a replayed turn and a freshly typed one read
  the same way.

This changes live decoding too, and on purpose. It fixes the raw-JSON leak
for any string-content line, on every path that uses the decoder (the oneshot
task log included). Codex's decoder is not touched.

### 2. The neutral protocol gains a speaker and a replay mark — DECIDED

In `runner/streamagent/proto.go`:

- `EventUserText EventKind = "user_text"`, mapped both ways in `toNeutral`
  and `ToAgentlog`.
- `Event.Replay bool` (`json:"replay,omitempty"`): true means "this happened
  before the agent process now running, and is being shown again". It is
  false on every live event.
- `RenderText` prefixes a replayed event with `↺ `. `RenderText` is the one
  renderer the runner's tap, `session stream attach` and the TUI chat share,
  so all three pick this up together.

**Versioning: additive, `ProtocolVersion` stays 1 — DECIDED. This departs
from the comment on `ProtocolVersion`**, which says the day the neutral event
grows a field is the day adapters must be able to fail loudly. The reason for
departing: `DecodeMsg` rejects any `v` other than its own, so a bump would
make every reader that has not been upgraded drop **every** line of every
stream — not only the new ones. With additive fields, a reader that has not
been upgraded shows a replayed event as if it were live, and a `user_text`
event as raw text. That is a cosmetic misreading during a version skew, where
a bump would mean total loss. The runner, the adapter and all clients are
built from one tree (`make build`), so the skew is short-lived. The comment
is rewritten to state the actual rule: bump the version when the MEANING of
an existing field changes, and add fields additively when an old reader's
default reading is a safe one. The rewritten comment has to record this as a
decision taken here, not as if it had always said so.

### 3. The adapter replays the transcript before the agent speaks — DECIDED

In `runner/streamagent/claude.go`, only when `ResumeConversation` is set:

1. **Locate the file before spawning the agent.**
   - The config dir is `CLAUDE_CONFIG_DIR`, or `<home>/.claude` when that is
     unset. The environment is the adapter's own, which is also the agent's
     (`cmd.Env = os.Environ()`).
   - The project dir name is `CLAUDE_CODE_PROJECT_DIR_NAME` if set.
     Otherwise it is the encoded absolute `Dir`: every byte outside
     `[A-Za-z0-9]` becomes `-`. An encoded name longer than 200 bytes is
     matched as a prefix of its first 200 bytes against the entries of
     `projects/`.
   - The file is the `*.jsonl` with the newest mtime in the matched
     directories. This is the rule `--continue` itself documents ("the most
     recent session in the current directory").
2. **Select the conversation.**
   - Parse each line loosely: `type`, `uuid`, `parentUuid`, `isMeta`,
     `isSidechain`.
   - The leaf is the last `user` / `assistant` row whose `isSidechain` is
     false. Walk `parentUuid` from the leaf over ALL rows that have a
     `uuid`, since a parent can be an `attachment` or `system` row. The walk
     stops where a parent is missing, which is where a compaction boundary
     ends the chain.
   - Keep the `user` / `assistant` rows on that chain, in file order, and
     drop those with `isMeta`.
   - The chain walk is what keeps an abandoned branch (a rewind) out of the
     replay. The measured file had no branch, so this rule is not yet
     exercised by real data; its test uses a synthetic fixture.
3. **Convert each kept row** by passing the raw line to the same
   `agentlog.NewDecoder("claude-stream-json")` the live path uses, then
   through `toNeutral`, and setting `Replay = true`. There is no second
   parser: the transcript line is a stream-json envelope plus fields the
   decoder ignores.
4. **Bound the replay.**
   - Each field is capped: `Args` and `Result` at 2 KiB, `Text` at 16 KiB,
     cut on a rune boundary.
   - Events are then kept from the END until either their serialized size
     reaches 256 KiB or 300 events are kept, whichever comes first.
   - Reasons for the numbers: the server's per-task ring is about 1 MiB and
     evicts from the front (`server/session_mux.go`), and both chat views
     keep 400 lines (`chatLineLimit`, `CHAT_LINE_LIMIT`). The measured file
     alone was 1.4 MB, so an unbounded replay could evict itself from the
     ring. The most recent part of the conversation is what the operator
     needs, which is why events are kept from the end.
5. **Emit.**
   - First `hello`, then a bracket line: an `EventRaw` event with
     `Replay: true` and text
     `── previous conversation: last <kept> of <total> messages ──`.
   - Then the replayed events, then a closing bracket line
     `── resumed ──`.
   - All of this is written synchronously BEFORE the stdout pump starts, so
     it cannot interleave with live events. The agent's own output waits in
     its pipe meanwhile.
6. **Cross-check the session.** When the live `system/init` event arrives,
   compare its `session_id` with the transcript's file name. If they differ,
   emit a warning event saying which session was replayed and which one the
   agent resumed. The discovery rule in step 1 is the adapter's reading of
   documented behaviour, and this makes a disagreement visible instead of
   silently showing the wrong history.
7. **Fail visibly, never fatally.** If there is no file, the file cannot be
   read, or no row is selected, emit ONE `EventError` warning with
   `Replay: true` saying why, and continue the resume. A missing replay must
   not cost the operator the session.

### 4. The runner's task log does not repeat the history — DECIDED

In `runner/streamtask.go`, `streamTap.onAdapterLine` does not write
`Replay` events to the task log, except `EventRaw` ones (the two bracket
lines). Reasons:

- When a stream task is resumed as a stream task, the log already holds that
  history from the first run.
- When a PTY task is resumed, the log is not the view the operator reads the
  conversation in.
- Up to 256 KiB per resume would also pile up in the durable log.

The bracket lines still record in the log that a replay happened.

### 5. Surfaces

| Surface | Change |
|---|---|
| runner task log (`runner/streamtask.go`) | §4 |
| `session stream attach` (`cli/streamattach_native.go`) | none beyond `RenderText` — it renders through it |
| TUI chat (`tui/chat.go`) | `eventStyle`: `user_text` → `OKStyle` (the style of its own `you ▶` echo); any `Replay` event → `MutedStyle` |
| WebUI chat (`webui/static/main.js` `chatRenderEvent`) | a `user_text` case using `c-you`, and the `↺ ` prefix plus `c-muted` for `replay` — the JS mirror of `RenderText`, which that function's comment already declares itself to be |
| wasm bridge (`cli/streamchat_wasm.go`, `cmd/harness-webui-wasm`) | none — it carries lines, it does not interpret events |
| `session snapshot --raw` | none — raw by definition |
| TUI grid pane / WebUI session preview | n/a — they render PTY screens, and a stream task has none |
| README §5b | one sentence: a resumed stream session starts with a replay of the agent's recorded conversation, bounded, and marked `↺` |

## Decided against

- **Reading through the Agent SDK's `getSessionMessages`.** It is the
  officially supported reader, but it would put a Node or Python runtime and
  the SDK on every runner host, next to an adapter that is otherwise one Go
  binary running on Windows and Linux alike. Parsing the line format directly
  is exposed to format changes, and §3.7 plus the unit fixtures are what make
  such a change visible rather than silent. If the format does change, moving
  to the SDK is the fallback.
- **The server persisting stream events** — see Problem 3. It cannot cover
  the PTY → stream route at all.
- **Bumping `ProtocolVersion`** — see §2.

## Not in this design

- **Sandboxed agents.** Under `agent-in-podman.sh` the adapter runs outside
  the container and claude inside it, so the transcript is written to the
  container's config dir. Unless that directory is mounted from the host,
  the adapter will not find it and §3.7's warning fires. Most tasks today run
  outside the sandbox. Wiring the mount is the sandbox wrapper's business
  (surface-parity S3), not this design's.
- **Codex and other vendors.** `user_text` and `replay` are neutral
  vocabulary that another adapter may emit. Nothing here reads a non-claude
  transcript.
- **Replay on a plain reattach.** A reattach to a live stream task already
  replays the server's ring. This design is about the process boundary of a
  resume, not about attach.
- **The PTY kind.** Claude's own TUI already redraws on `--continue`.

## Tests

- `runner/agentlog`:
  - A string-content `user` line decodes to one `KindUserText` event, not
    `raw`.
  - A `text` block in `user` becomes `KindUserText`, and in `assistant`
    becomes `KindText`.
  - The existing golden fixtures still pass unchanged, or any change to them
    is explained in the commit.
- `runner/streamagent`, all against synthetic transcripts in `t.TempDir()`
  with `CLAUDE_CONFIG_DIR` pointed at them. A test must never read the real
  home directory; the existing resume test gains the same override.
  - Name encoding, including the over-200 prefix match.
  - Newest-mtime selection.
  - The chain walk drops a rewound branch.
  - `isMeta` and `isSidechain` rows are dropped.
  - Bookkeeping rows produce nothing.
  - The budget keeps the tail, and the bracket counts are right.
  - Ordering: `hello`, then bracket, then replay, then bracket, then live
    events, against the existing fake agent.
  - A `session_id` mismatch produces a warning.
  - No transcript produces exactly one warning and the resume continues.
- `runner/streamtask`: `Replay` events other than bracket lines do not
  reach `LogSink`.
- **End to end, once, against the real binary:**
  - Start a claude session in a scratch directory and have it state a
    codeword.
  - Run `harness-stream-adapter --dir <scratch> --resume-conversation --
    claude` and confirm the replayed `user_text` and `text` lines appear
    before the resumed agent's first event.
  - Then do the PTY → stream route through `scripts/dummy-harness.sh` (open
    interactive, stop, resume with `--stream --resume-conversation`) and read
    it in `session stream attach` and in the TUI chat. Unit tests enter
    below both of these layers.

## Implementation notes

- **Bracket wording.** The opening bracket reads `── previous conversation:
  last <kept> of <total> events ──`. It says EVENTS rather than messages
  because an event is what is counted: one transcript row can decode to more
  than one event.
- **Where the code lives.**
  - `runner/streamagent/claude_transcript.go`: discovery, chain selection,
    bounds, and the emit step.
  - `runner/streamagent/claude.go`: the replay runs after `hello`, before
    the stdout pump starts, together with the §3.6 session check.
  - `runner/streamtask.go`: §4.
  - `ClaudeOpts.Getenv`: lets a test point discovery at a scratch config
    dir. The adapter binary passes nil, which means `os.Getenv`.
- **The WebUI chat also renders `raw` events by their text now.** It used to
  show the event object as JSON. The WebUI's text rendering is split out into
  `chatEventLine`, so a live event and a replayed one word themselves the same
  way. `raw` → text is what the Go renderer (`agentlog.Render`) already did.
- **A test that the comment on `ToAgentlog` claimed, but that did not
  exist.** "The round trip (agentlog → neutral → agentlog) is asserted in
  this package's tests" was not true of `runner/streamagent`. The assertion
  lives in `runner/streamtask_test.go`
  (`TestEventRoundTripsThroughTheNeutralType`), which now includes
  `KindUserText`.
- **Measured against the real binary (claude `2.1.284`, 2026-09-30).**
  - `claude -p "Remember the codeword PLUM-42. Reply only with: noted."` was
    run in a scratch directory, and it wrote a 153 KB transcript. So `-p`
    persists a session, as the SDK docs imply.
  - Then `harness-stream-adapter --dir <scratch> --resume-conversation --
    claude` was run with one user turn on stdin ("What was the codeword?").
    Its output, in order:
    1. `hello`
    2. the opening bracket (`last 3 of 3 events`)
    3. replayed `user_text` (the prompt), `thinking`, and `text` ("noted.")
    4. `── resumed ──`
    5. live `session_start` with the same session id as the transcript
       file, so no mismatch warning fired
    6. live `text` "PLUM-42"
    7. `finish`, then `exit 0`

## Amendment 2026-09-30b: one display decision for every chat, and what a live TUI run found

Asked for by the operator: drive the TUI for real, and share whatever can be
shared.

**What is now decided in one place.**

- `streamagent.DisplayOf(Msg)` returns a `Display`: the line to append, its
  `Tone` (text / you / muted / warn / err / raw), and whether it sets the
  status line or ends the busy state.
  - The TUI chat maps a Tone to a lipgloss style (`toneStyle`).
  - The WebUI chat maps it to a `c-<tone>` class, and it receives the
    `Display` from Go over the wasm pump (`displayForJS`).
  - The page's own event renderer (`chatRenderEvent` / `chatEventLine`) is
    deleted. That closes the JS-mirror omission recorded against
    surface-parity item 32 in the first walk.
- `cli.LineDisplay(StreamLine)` adds the one case `DisplayOf` cannot see: a
  line that is not the protocol. It displays as `cli.NotProtocolLine`, which
  escapes terminal-steering bytes with `cli.EscapeForTerminal`, the escaper
  `board read` already uses. The TUI chat, the WebUI chat and `session stream
  attach` all go through it.
- The rest:
  - `streamagent.RenderExit` is the one wording of an exit.
  - `streamagent.UserTurnLine` is the one echo of a sent turn; the WebUI gets
    it through the new `harness.streamUserTurnLine` bridge function.
  - `agentlog.UserTurnPrefix` is the one `you ▶ `.
  - `agentlog.TruncateBytes` replaced the transcript reader's own copy.
  - `streamagent.FromAgentlog` (formerly the unexported `toNeutral`) lives
    beside `ToAgentlog` in `render.go`. The round-trip test moved there from
    the runner package, where it had tested a hand-written copy of the mapper.
- `scripts/dummy-harness.py` builds its claude and bash profiles from
  `scripts/agent_presets.py` (`expand_agents_preset`, and the new
  `agent_profiles_json`) instead of hand-copied argv. The copy had lacked the
  stream adapter, so every stream task on a claude dummy failed.

**Two defects the live run found. Both predate this feature**; the first was
reproduced with a TUI built from `c0af1551`, before any of this landed.

1. **The server sent terminal bytes into an NDJSON stream.**
   - The session mux keeps a terminal model (the mode tracker and a `vtgrid`
     screen) for every session. Every attach replay ended with a synthesized
     mode preamble and a screen repaint, and it did this for the stream kind
     too. The repaint is a terminal program with no trailing newline, so the
     chat's line reader joined it to the stream's next line.
   - What the operator saw: on the first turn sent from the TUI chat, the
     joined line displayed as "(not the protocol)". The escapes in it
     (`ESC[?1049l` and others) took the TUI out of its alternate screen, and
     the first live event (`session_start`) was lost inside that line.
   - Fix: `NewSessionMux` takes the task's kind. For a kind that is not a
     PTY, it does not feed the terminal model and `screenRepaint` returns
     nothing. The attach path and the rebind path of a held session both pass
     the kind.
   - Pinned by `TestStreamSessionAttachCarriesNoSynthesisedFrames`, and its
     negative control went red with the gate forced on.
2. **The followers showed a non-protocol line verbatim.** That is a terminal
   injection from anyone who can write the stream, since `session send` puts
   raw bytes on it. Fixed by `cli.NotProtocolLine` above.

**Verified live** on a dummy harness:
- The PTY → stream route in the TUI chat, run under `script(1)`: the TUI
  wrote 0 NDJSON bytes and 0 "(not the protocol)" lines to its terminal.
  Replayed lines were `fg#8a8a8a`, the typed turn `fg#00d787`, `session_start`
  was present, and the answer `FIG-77` was shown.
- `session stream attach`: 0 ESC bytes in its output.
- The WebUI chat, including a turn sent from the page: its echo came through
  the bridge as `c-you`.

**Deploy note.** Fix 1 is in `server/`, so it reaches the fleet only when the
SERVER restarts. The runner and adapter halves are unaffected by the order.

## Decisions taken with the operator

1. **Replay on every resume, with no switch — DECIDED (operator,
   2026-09-30).** Whenever `--resume-conversation` is set, the replay
   happens. This matches what claude's own interactive TUI does: resuming the
   process redraws the earlier conversation, and nothing turns that off. So
   the stream kind should behave the same way. There is no flag, so no
   surface in items 1–9 of the surface-parity checklist changes.
2. **A replayed line keeps its live color — DECIDED (operator,
   2026-09-30).** This reverses the "any `Replay` event → `MutedStyle`" /
   `c-muted` rows of §5. Claude's own UI does not fade a resumed
   conversation, and the faded history read as a different thing from the
   same conversation. `streamagent.EventTone` no longer looks at `Replay`,
   so a replayed answer is `text` and a replayed turn is `you` in both chats.
   The `↺ ` prefix still marks a line as history, and a replayed event still
   never drives the status line or the busy state (`DisplayOf`).
