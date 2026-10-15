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
an open approval menu, a half-typed deny reason are all still there on the
next `i`.

Inside, the pane shows one input row whose meaning follows its state:

| Pane state | Input row | Keys |
| --- | --- | --- |
| normal | `▶ ` + draft | text editing; Enter sends `KindUser` and clears; Esc sends `KindInterrupt` |
| request pending (approval) | `1 allow  2 allow + <suggestion>…  N deny…` | a digit answers at once; ↑/↓ + Enter selects; Esc does nothing |
| deny reason | `deny reason ▶ ` + text | Enter sends the denial; Esc returns to the menu |
| request pending (question) | `⏎ open chat` | Enter closes the grid and opens the chat overlay |

`⏎` and `v` in navigation mode open the chat overlay for a stream pane (there
is no PTY to attach to), instead of `DoAttachSession`.

### 5. WebUI stream cell

The WebUI grid has no input mode: a focused cell receives keys. A stream cell
therefore holds a transcript (the chat's `chatApplyDisplay` rendering, bounded)
and, under it, a one-line input (Enter sends the turn) and — while a request is
pending — the chat's approval buttons. A question shows a button that opens the
chat. The header gets the same `‹stream›` / `⚑` markers.

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
| D5 | Approval is answered inside input mode with a numbered menu, not with modifier chords | this spec |
| D6 | `Ctrl+]` leaves pane state untouched (draft, menu, deny reason survive) | this spec |
| D7 | Esc in the normal row interrupts; Esc in the approval menu does nothing | this spec |
| D8 | Questions are not answered in a grid cell; the cell opens the chat | this spec |
| D9 | Stream panes use the PTY pane's 128 KiB replay cap; the first undecodable line after attach is dropped | this spec |
| D10 | `taskEntry.wakeWrite` is replaced by a per-path `wake func() error` | this spec |
| D11 | wasm stream attaches become keyed slots; write verbs route by task id | this spec |

## Not in this change

- A task-level pending count (`pending=N`) — the event-stream increment that
  would make D9's tail assumption unnecessary.
- Multi-line input in a grid cell.
- Answering a question from a grid cell (D8).

## Verification

- Unit: `gridLiveTasks` includes stream; `StreamPane` drops a leading fragment,
  tracks a pending request and its resolution, renders the tail; input-mode
  key handling per row state; `WakeStdin` on a stream task writes one decodable
  `KindUser` line; the interactive wake still writes text, then `\r`.
- Dummy harness (keystroke and click paths are not reachable from a Go test):
  open a TUI grid with one interactive and one stream session, `i` → type →
  Enter → `Ctrl+]` on the stream pane, answer one approval by digit; same in
  the WebUI grid; send an agentboard message to the stream task and see the
  wake turn appear and the agent take a turn.
