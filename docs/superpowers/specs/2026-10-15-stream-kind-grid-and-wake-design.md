# Stream-kind sessions in the grid, and agentboard wake for them

Status: DECIDED (operator, 2026-10-15) — rows marked `this spec` in the
decision table were chosen while writing and are the ones worth a second look.

## Problem

An event-stream (`stream` kind) session is unusable in a grid-centred workflow,
for two independent reasons:

1. **The grid does not show it.** Both grids select tiles with `IsPTYKind`
   (`tui/grid.go` `gridLiveTasks`, WebUI `liveInteractiveTasks` /
   `liveInteractiveIds`), so a live stream session is silently absent. The grid
   is the operator's main monitoring surface, and its standard move —
   `i`, type, `Ctrl+]` back out — has no stream-kind equivalent anywhere.
2. **An agentboard message does not wake it.** The server emits `TaskWake`
   for any assigned task (`server/agent_wake.go`), but the runner only has a
   writer for PTY sessions: `taskEntry.wakeWrite` is set by the
   `OnStdinWriter` callback on the interactive path, and the stream path
   (`StreamTask.Run`) passes no such callback. `WakeStdin` then logs
   `wake skipped … not an interactive session` and drops it. A stream agent
   only sees a message when somebody else starts a turn.

Either alone keeps the operator on the interactive kind.

## Design

### 1. Wake (runner)

A wake on a stream task is a **user turn whose text is `wakeMarker`**, written
on the adapter's stdin as one neutral-protocol line:

```json
{"v":1,"kind":"user","user":{"text":"<harness:agentboard-wake> …"}}
```

This needs no vendor knowledge: `KindUser` is the runner↔adapter protocol's own
message, and each adapter already turns it into its vendor's user message
(`streamagent/claude.go` `sendUserTurn`, which also echoes it as
`EventUserText`, so every attached client sees the wake in the transcript —
the PTY kind shows it too, as the prompt the agent's own UI paints).

Mechanism:

- `taskEntry.wakeWrite func([]byte)` becomes `wake func() error` — the
  delivery itself, built by the path that owns the stdin:
  - interactive: today's split write (marker text, `wakeSubmitDelay`, `\r`),
    moved into the closure unchanged
  - stream: one write of `EncodeStreamMsg`-equivalent bytes (one line,
    `\n`-terminated, from `streamagent`)
- `StreamTask` gains `OnStdinWriter`, passed through to agentexec's
  `ExecuteOption`.
- `WakeStdin` keeps its debounce and its log lines; it calls `e.wake()`.

The stream write shares the adapter's stdin pipe with client `Stdin` frames.
A single `write(2)` of ≤ `PIPE_BUF` bytes is atomic on a pipe, and the wake
line is ~250 bytes, so it cannot be torn by — or tear — a client line written
in one call. A client line split across two frames could still be interleaved
with a wake; the clients in this repo write one message per `Stdin().Write`
(`StreamSession.Send`), so this is not reachable from them.

### 2. Grid selection (all surfaces)

Every grid predicate moves from `IsPTYKind` to `IsSessionKind` (JS:
`isSessionKind`, added beside `isPTYKind`), and every message that says
"interactive session" about the grid's set says "session". Sites:

- TUI: `gridLiveTasks`, the empty-grid message in `GridModel.View`, any entry
  point that refuses an empty set
- WebUI: `liveInteractiveTasks`, `liveInteractiveIds`, `openGridSet`'s empty
  message, the task sheet's grid include/exclude toggle

The comments that say the stream kind is "intentionally omitted" are rewritten,
not left beside code that now includes it.

### 3. TUI stream pane

`GridModel.panes` becomes a slice of an interface implemented by
`*PaneStreamer` (unchanged) and a new `*StreamPane`. The interface is what the
grid already calls: `TaskID`, `Render`, `Err`, `Stop`, scroll methods,
`DiagLine`, and one input method, `Key(tea.KeyMsg)` — the PTY pane encodes it
with `keyToBytes` and calls `SendInput` exactly as the grid does today.

`StreamPane`:

- **Attach**: cowrite, through a `StreamSession` opened with a replay limit
  (`OpenStreamSessionWithReplayLimit`, the stream twin of
  `AttachSessionWithReplayLimit`), limit `gridReplayLimit` (128 KiB) like a PTY
  pane, staggered by the same `startDelay`.
- **Replay cut**: the capped replay is frame-aligned, not line-aligned, so its
  first line may be a fragment. The first line read after attach is dropped if
  it does not decode as the protocol.
- **Pending requests under the cap**: a pending request stops the agent, so the
  request line is at or near the tail and survives a 128 KiB cap. A task-level
  pending count (`pending=N`) would remove the assumption and is a separate
  increment.
- **Transcript**: each line goes through `cli.LineDisplay` (the chat's
  classifier), into a bounded ring; `Render(w,h)` wraps with `chatRows` and
  shows the tail. Shift+↑/↓ pin the view like a PTY pane; Shift+←/→ are no-ops
  (lines wrap, nothing is cut off).
- **Header**: `<id8> ‹stream›`, plus `⚑` while a request is pending.

### 4. TUI input mode on a stream pane

The interaction is the PTY pane's: **`i` enters, every key goes to the pane,
`Ctrl+]` leaves.** Outside input mode a stream pane consumes no key; digits and
letters stay grid navigation. `Ctrl+]` changes nothing in the pane — a draft,
a half-built question answer, a half-typed deny reason are all still there on
the next `i`.

Inside, the keys are **the stream chat's own**, the way a PTY pane's keys are
the agent UI's own. The chat already answers both kinds of request in one
line of input (`tui/chat.go` `onKey`): while a request is pending, letters are
decisions, not text.

| Pane state | Keys (all from the chat) |
| --- | --- |
| normal | text editing; Enter sends `KindUser`; Esc (or Ctrl+X) interrupts |
| approval pending | `a` allow · `d` deny reason · `1`–`9` allow + accept suggestion N · Esc interrupts |
| question pending | Tab / Shift+Tab question · `1`–`9` pick · `o` free text · `r` reply · `d` deny · Enter sends · Esc interrupts |
| a sub-editor (deny reason, free text, reply) | Enter keeps / sends; Esc returns to the request |

To make that one implementation rather than two, the chat's interaction state —
input, sub-mode, pending request, question answer, and the key handler that
turns keys into protocol messages — moves out of `ChatModel` into a component
both `ChatModel` and `StreamPane` hold.

**Leaving is `Ctrl+]` on every surface, and Esc is never "leave".** Today the
chat overlay is the one TUI surface where Esc leaves: a PTY attach detaches on
`Ctrl+]` (`tui/app.go`), the grid leaves input mode on `Ctrl+]`, and in an
agent's own UI Esc interrupts. An operator moving between them presses `Ctrl+]`
in the chat and gets nothing, and presses Esc in the chat meaning to interrupt
and loses the view instead. So, in the chat overlay as well as in a pane:

- `Ctrl+]` leaves — the chat closes its overlay (the task keeps running, as
  today); in a pane the grid handles it before the pane sees it, as today.
- Esc outside a sub-editor sends `KindInterrupt`. Ctrl+X stays as a second
  spelling of the same.

The key hints the chat prints (`questionHint`, the approval row, the status
line) change with them.

Esc while a request is pending interrupts rather than answering. What the
adapter does with an interrupt while it holds an unanswered request is checked
during implementation; if the request stays pending, the pane keeps showing it.

The pending block (approval input, question options) is rendered at the pane's
bottom, clipped like the chat's — the key row never scrolls away.

`⏎` and `v` in navigation mode open the chat overlay for a stream pane (there
is no PTY to attach to), instead of `DoAttachSession`.

### 5. WebUI stream cell

The WebUI grid has no input mode: a focused cell receives keys. A stream cell
therefore holds a transcript (the chat's `chatApplyDisplay` rendering, bounded)
and, under it, a one-line input (Enter sends the turn) and — while a request is
pending — the chat's approval or question form. `chatShowApproval` /
`chatShowQuestion` take the element to render into instead of writing
`#chat-approval`, so the cell gets the chat's form, not a second one. The
header gets the same `‹stream›` / `⚑` markers.

Esc in a cell's input — and in the main chat's input — sends the interrupt and
calls `preventDefault`: the grid is a `<dialog>`, whose default Esc action is
to close it, which is the same accident §4 removes from the TUI chat.

The wasm side today allows exactly one stream attach (`cli/streamchat_wasm.go`,
"there is exactly ONE chat"). It becomes a keyed slot map like
`preview_wasm.go`'s: the chat uses key `"chat"`, a grid cell `"grid:<id>"`.

- Hooks for grid cells carry the key: `harness_streamPaneOpen/Line/Closed`.
  The chat's existing hooks keep their signatures.
- `harness.streamPaneStart(key, taskId)` / `streamPaneStop(key)` open and close
  a cell's attach, with the replay limit and the first-fragment drop of §3.
- The write verbs (`streamTurn`, `streamApprove`, …) keep `(taskId, …)` and
  send on any open slot attached to that task. Every slot is a cowrite attach
  on the same task, so which one carries the write does not matter, and the
  existing check — refuse a task no slot is attached to — is kept.

## Decisions

| # | Decision | Decided by |
| --- | --- | --- |
| D1 | Stream sessions are shown in the grid, TUI and WebUI | operator |
| D2 | Grid input on a stream pane is `i` → operate → `Ctrl+]`, matching the PTY pane | operator |
| D3 | Wake on a stream task is a user turn carrying the wake marker text | operator |
| D4 | WebUI is in the same change | operator |
| D5 | Inside input mode a stream pane takes the stream chat's keys, through one interaction component shared with `ChatModel` | this spec — REPLACES a first draft's numbered approval menu, a second key scheme the chat already made unnecessary |
| D6 | `Ctrl+]` leaves pane state untouched (draft, answer in progress, deny reason survive) | this spec |
| D7 | Esc outside a sub-editor interrupts, in a pane and in the chat alike | operator (chat) / this spec (WebUI inputs) |
| D12 | The TUI chat overlay leaves on `Ctrl+]`, not Esc, like a PTY attach and the grid's input mode | operator |
| D8 | Questions ARE answered in a grid cell, with the chat's keys / form | operator — REVERSES a first draft that sent questions to the chat overlay: a PTY pane answers its agent's question UI in the same cell, so cell size was no reason |
| D9 | Stream panes use the PTY pane's 128 KiB replay cap; the first undecodable line after attach is dropped | this spec |
| D10 | `taskEntry.wakeWrite` is replaced by a per-path `wake func() error` | this spec |
| D11 | wasm stream attaches become keyed slots; write verbs route by task id | this spec |

## Not in this change

- A task-level pending count (`pending=N`) — the event-stream increment that
  would make D9's tail assumption unnecessary.
- Multi-line input in a grid cell.

## Verification

- Unit: `gridLiveTasks` includes stream; `StreamPane` drops a leading fragment,
  tracks a pending request and its resolution, renders the tail; the shared
  interaction component produces the same messages from `ChatModel` and from a
  pane (existing chat tests keep passing unchanged); `WakeStdin` on a stream
  task writes one decodable `KindUser` line; the interactive wake still writes
  text, then `\r`.
- Dummy harness (keystroke and click paths are not reachable from a Go test):
  open a TUI grid with one interactive and one stream session, `i` → type →
  Enter → `Ctrl+]` on the stream pane, answer one approval and one question
  from the pane; same in
  the WebUI grid; send an agentboard message to the stream task and see the
  wake turn appear and the agent take a turn.
