# Server-side screen model and repaint on attach

**Status:** design, approved 2026-08-22. Implementation plan to follow.

## 1. Goal

Give `SessionMux` a `vtgrid.Terminal` per session, fed from the same frames it
already puts in the ring, and end an attach's replay with a REPAINT synthesised
from that grid instead of hoping the replayed bytes reconstruct the screen.

Today the replay is four stacked approximations of "send the screen", and the
measurements say they do not reach it. Feeding each corpus in
`vtgrid/testdata/vtcorpus` through a full-ring replay and comparing the result
to the screen the same bytes produce in full:

| corpus | rows reconstructed by the replay alone |
|---|---|
| htop | 5 / 40 |
| vim-split | 2 / 40 |
| win-cmd | 11 / 40 |
| codex-tui | 25 / 40 |

The repaint reconstructs 708/708 rows and 108,684/108,684 cells — attributes,
underline style, colours, hyperlinks and combining marks included — against
vtgrid and against `x/vt` as an independent implementation, in four observer
states; 708/708 rows with 18/18 cursor position and alt-screen state against the
xterm.js the WebUI ships; and 1416/1416 rows with 36/36 cursor position and
36/36 cursor visibility against a real Windows console read back with
`ReadConsoleOutputW`. `vtgrid/repaint_test.go` and
`vtgrid/repaint_console_windows_test.go` are those gates; `buildRepaint` in the
former is the sequence this design wires up.

## 2. Non-goals

- Replacing the ring. It stays, at its current size and meaning, because it is
  the scrollback. `vtgrid/terminal.go` says so from the other side: "there is no
  scrollback here — the harness's 1 MiB replay ring already is the scrollback".
- Making the server the authority for `session snapshot` or the TUI grid pane.
  Both keep their own emulator. That is a strictly larger change and it can only
  be built on top of this one.
- Sending cells instead of bytes (herdr's `SemanticFrame` shape). The repaint is
  ordinary terminal bytes, so every existing client renders it unchanged.
- Reflow on resize. `vtgrid` does not record which line breaks were soft and
  neither does the byte ring, so this is not a regression.
- Fixing the cursor VISIBILITY case an observer cannot be moved out of when it
  is already on the alternate buffer with no switch left to make. That is fixed
  in `buildRepaint` already (commit `670164b`); this design only carries it.

## 3. Existing architecture

`server/session_mux.go` splices the runner stream to a control client and to
observers, and keeps a ring of wire-encoded frames for replay.

```
runner ──frames──▶ runnerPump ──▶ ring (1 MiB of wire frames)
                        │         modeTracker.feed()   (DEC private modes)
                        ├──▶ control client (tui)
                        └──▶ observers (viewers / cowriters)
```

An attach replays, in order:

| # | bytes | where |
|---|---|---|
| 1 | `m.lastWinSize` — the PTY size as a `Control` frame | `attachObserver` only |
| 2 | `m.modes.preamble()` — DEC private modes, as a **Stdout** frame | both attach paths |
| 3 | `capReplayTail(m.replaySnapshot(), replayLimit)` — the ring | both |

and `replaySnapshot()` trims a finished alt-screen episode:

```go
if m.modes.onAltScreen() { return m.ring.Snapshot() }
return m.ring.SnapshotFrom(int(m.mainMark.Load()))
```

Every one of 1–3 plus the trim exists because the server has bytes and no
screen. `server/mode_tracker.go` says it outright: "Reconstructing alt-screen
content needs a full terminal-state model, which this is deliberately not."

Replay sizes that actually occur: `session attach` passes `replayLimit` 0, which
is the whole ring (`cli/attach_native.go`); a grid pane passes 128 KiB
(`cli/preview_wasm.go`); the ring defaults to 1 MiB
(`server/task_handler.go`).

## 4. Architecture

```
runner ──frames──▶ runnerPump ──▶ ring          (scrollback, unchanged)
                        ├──────▶ modeTracker    (input-affecting modes only, §7)
                        └──────▶ vtgrid.Terminal ◀── applyWinSizeFrame (Resize)
                                        │
                                   buildRepaint()
                                        │
attach replay:  winsize │ preamble │ ring │ REPAINT
                 Control    Synth    Stdout   Synth
```

### 4.1 Lifecycle — one grid per session, always

Created in `NewSessionMux` before `runnerPump` starts, alongside the ring, and
living as long as the mux.

Not lazily, and the reason is the whole point of having it: **the grid's value
is that it saw bytes the ring has already evicted.** A grid created on first
attach could only be seeded from the ring, which is truncated — that is the ring
with extra steps, and it would reproduce exactly the blindness this design
exists to remove.

Cost, measured (`vtgrid/bench_test.go`, `TestFootprint`): **382.5 kiB per
screen** at 150x40, and it does not grow with traffic — 382.5 kiB empty against
382.7 kiB after 2,000 lines, because the model keeps no scrollback. That is
0.37× the ring already paid per session. Throughput is 27–59 MB/s with zero
allocations per operation, against a PTY that peaks in the tens of KB/s.

### 4.2 Initial size

`m.lastWinSize` is empty until someone sends a size, and a detached session may
have none at all. The grid is created at the size on `OpenExecRunnerRequest`
when it carries one, else at **80x24**, and resized on every size change (§4.4).
It is never created late, so it never misses a byte.

80x24 rather than `session snapshot`'s 40x120 fallback because the two answer
different questions: the snapshot flag is "render this for me at a size I chose"
and defaults to something a TUI can be read at, while this is "what size does
the session believe it is" before anyone has said. The conventional terminal
default is the honest answer to that, and it is wrong for at most the first
resize — after which the grid holds only what the app drew at the real size.

### 4.3 Feed point

`runnerPump`, beside the existing `m.modes.feed(...)`, on `FrameType_Stdout` and
`FrameType_Stderr` only — the same switch that already decides what counts as
display output.

### 4.4 Resize — one funnel

Size changes have two entry points and one exit:

```
tuiPump           → forwardControlFrames   ─┐
                                            ├→ applyWinSizeFrame
observerInputPump → forwardObserverFrames  ─┘
  → applyObserverWinSize (only while the control seat is EMPTY)
```

The control client owns the size whenever it holds the seat; `exec_resize` lets
an observer stand in when nobody does (`applyObserverWinSize`). Both reach
`applyWinSizeFrame`, so the grid's `Resize` goes there and picks up both paths.

The check-then-apply in `applyObserverWinSize` races a concurrent control
attach, deliberately and benignly — a control client sends its own size on
attach and wins by arriving second. That race decides WHICH size wins, not
whether the grid sees a torn state, because the funnel serialises.

### 4.5 Locking

The grid gets **its own mutex**, as `modeTracker` already has, not `m.mu`.

`runnerPump` deliberately does not take `m.mu` to feed the tracker; putting the
grid under `m.mu` would make the feed hot path contend with fan-out and attach.
Feed (`runnerPump`) and resize (`applyWinSizeFrame`) run on different goroutines
and `vtgrid.Terminal` has no internal locking, so its own mutex serialises them.
A resize landing between two feeds is what a real terminal does.

The repaint is generated under the same grid mutex, and the replay block is
assembled under `m.mu` exactly as today (`attachObserver` already snapshots
replay state under the lock that runnerPump's fan-out takes).

## 5. Replay composition

```
winsize   (Control frame, unchanged)
preamble  (Synth frame)
ring      (Stdout frames, capped by replayLimit as today)
REPAINT   (Synth frame)
```

`replayLimit`'s NUMBERS do not change: 0 still means the whole ring, a grid pane
still asks for 128 KiB, and no caller is edited. What changes is what the knob
MEANS. Today a small limit buys a broken screen — which is why `capReplayTail`'s
ground-state scan, the mode preamble and the alt trim all exist. With the screen
carried separately, the limit is simply how much history the observer wants, and
`replayLimit: 0` on a monitoring pane becomes a correct screen with no history
instead of a broken one.

`capReplayTail`'s ground-state scan stays. Its job changes from "do not corrupt
the screen" to "do not corrupt the scrollback", which is still a job.

## 6. The `Synth` frame type

The repaint and the preamble are bytes the SERVER synthesised. Today the
preamble is sent as a `Stdout` frame, so it is indistinguishable from bytes the
PTY actually emitted, and `session snapshot --raw` — whose entire purpose is
"show me the actual bytes" — cannot tell them apart. Adding a 3–9 KB screen
paint to that makes the ambiguity worse.

**`FrameType_Synth = 4`** in `objtrsf`'s `exec/frame/frame.bgn`, and one line in
`exec/exec_stream.go`:

```go
case frame.FrameType_Stdout, frame.FrameType_Synth:
    → stdoutPipe
```

That is the whole upstream change. Everything else follows:

- **Ordering is the frame order**, because it is one stream. This is why a
  separate trsf stream was rejected: trsf streams have no ordering relationship
  to each other, and a repaint that may arrive before the ring it must overwrite
  is not a design, it is a race. Synthesised bytes here are not sideband — they
  must land at an exact position in a byte sequence — so the repo's "sideband
  goes in a separate trsf stream" rule does not apply to them.
- **A separate pipe was rejected for the same reason**: two `io.Pipe`s have no
  ordering relationship either. The distinction has to survive as frames, not as
  destinations.
- **Old clients degrade, they do not break.** `frame.bgn`'s `Frame` matches
  `.. => data`, so an unknown type parses as opaque data, and
  `exec/exec_stream.go`'s dispatch ends in `default: // ignore unknown frame
  types`. An old client silently drops Synth: no repaint, and no preamble
  either — behaviour it has today, which it loses. That is a real if transient
  regression in a mixed-version deployment, and it is why the server is replaced
  along with its clients.
  `ControlType` could NOT have been used for this: its match ends in
  `.. => error(...)`, so extending it would hard-fail old clients.
- **`winsize` stays a `Control` frame.** It is already distinguishable, and
  `CommandExecutionStream` consumes it (`w.winSize.Store(...)`) to size the
  snapshot renderer. Moving it would break that.

### 6.1 `--raw` reads frames

`cli/snapshot_raw.go`'s `CollectRaw` currently wraps the stream in
`CommandExecutionStream` and reads `stream.Stdout()`, which merges Stdout and
Synth into one pipe and loses the distinction again. It instead reads frames
directly with the generated `frame.Frame.Read` — already exported, already what
`exec_stream.go` itself uses — keeping `(type, payload)` in order:

```go
f := &frame.Frame{}
err := f.Read(stream)
f.Header.Type      // provenance
f.Data()           // payload
```

`LastWindowSize()`'s equivalent comes from reading the `Control` frame in the
same loop, as `session_mux.go` already does server-side.

How `--raw` PRESENTS synthesised bytes — omit them, emit them, annotate the
boundary, or summarise them — is deliberately left to implementation. The point
of keeping provenance rather than filtering it at the source is that the choice
stays open; a "drop or merge" switch would have decided it now.

`server/session_mux.go`'s hand-rolled `readOneFrame` stays. It reads frames to
put the VERBATIM wire bytes in the ring, which `frame.Frame.Read` cannot do
without a parse-and-re-encode round trip. That is a different requirement, not
duplication to remove.

## 7. `modeTracker`'s role shrinks

`modeTracker` exists because "any mode whose controlling sequence has already
scrolled out of the ring window would otherwise be lost". A grid fed from the
first byte has not lost them, so the overlap moves:

| | before | after |
|---|---|---|
| screen content, alt buffer | nobody (the stated gap) | grid |
| modes vtgrid models (6, 7, 25, 47, 1047, 1048, 1049) | tracker | grid, via the repaint |
| input-affecting modes (bracketed paste 2004, mouse 1000/1002/1003/1006, application cursor keys 1, …) | tracker | **tracker** — vtgrid neither models nor exposes them |

The tracker is not deleted. Its preamble keeps carrying the modes the grid does
not model, and it keeps the leave/DECTCEM/enter ordering landed in `670164b` for
as long as it emits DECTCEM at all. Whether DECTCEM should move entirely to the
repaint once both are in the same replay is an implementation-time
simplification, not a design question — both orderings are already gated by
tests on a real console.

## 8. The alt-screen trim becomes conditional

`replaySnapshot()` trims a finished alt episode because its absolute-cursor
fragments would paint the PRIMARY screen. With the repaint fixing the final
screen, the only remaining stake is **scrollback**, and the trim costs the
history from before the episode.

The trim is needed exactly when **the ring starts INSIDE an alt episode**:

- entry `ESC[?1049h` still in the ring → the client enters the alt buffer,
  the episode paints THAT buffer (touching neither the primary screen nor
  scrollback), leaves, and the rest paints primary. Scrollback intact, **trim
  not needed**.
- entry evicted, ring starts mid-episode → fragments paint the primary screen
  and scroll into scrollback. The repaint fixes the screen, not the scrollback
  it already polluted. **Trim needed.**

Testing "is the most recent entry still in the ring" is UNSOUND. With two
episodes E1(a1…x1), E2(a2…x2), a ring starting between a1 and x1 leaves a2
present while E1 straddles the start, and E1's tail pollutes.

The sound predicate is "was the session on the alternate screen at the ring's
OLDEST surviving frame". `runnerPump` sees every transition as it appends, so it
keeps a small ordered list of transition indices, pruned as the ring evicts; the
state at the oldest surviving frame is the direction of the most recently pruned
transition, or primary if none has been pruned.

```go
if m.modes.onAltScreen()     { return m.ring.Snapshot() }  // live app: unchanged
if !m.altAtOldestSurviving() { return m.ring.Snapshot() }  // ring starts on primary
return m.ring.SnapshotFrom(int(m.mainMark.Load()))          // straddles: trim as today
```

`RingBuffer` gains an accessor for its oldest surviving index — the value
`SnapshotFrom` already computes internally.

Net effect: full scrollback in the ordinary case, today's behaviour in the
straddling case.

## 9. Component changes per layer

| file | change |
|---|---|
| `objtrsf` `exec/frame/frame.bgn` | `FrameType_Synth = 4`; regenerate |
| `objtrsf` `exec/exec_stream.go` | `case FrameType_Stdout, FrameType_Synth:` |
| harness `go.mod` | bump objtrsf after it publishes |
| `server/session_mux.go` | grid field + own mutex; create in `NewSessionMux`; feed in `runnerPump`; `Resize` in `applyWinSizeFrame`; repaint appended in `Attach` and `attachObserver`; preamble and repaint sent as Synth; `replaySnapshot` conditional trim; transition list |
| `server/ring_buffer.go` | oldest-surviving-index accessor |
| `server/mode_tracker.go` | unchanged in behaviour; doc updated for the narrowed role |
| `cli/snapshot_raw.go` | `CollectRaw` reads frames directly, keeping provenance |
| `vtgrid/ansi.go` (or a sibling) | `buildRepaint` becomes the exported `(*Terminal).Repaint()` |
| `vtgrid/repaint_test.go` | calls `term.Repaint()` instead of the local helper |

`buildRepaint` currently lives in `vtgrid/repaint_test.go` and is reachable only
from tests. It becomes **`(*vtgrid.Terminal).Repaint() []byte`**, exported from
`vtgrid` itself.

That is its natural home rather than a concession to the server: it is the
inverse of `ANSI()`, and `ANSI()`'s own doc marks the gap it fills — "There is
no cursor positioning and no screen clear: the result is a block of text, not a
program that repaints a terminal." `Repaint()` is that program. Putting it in
`server/` instead would invert the dependency, since `vtgrid`'s own gates must
keep calling it — and they must, because the Windows leg's whole value is that
it exercises the real sequence rather than a copy.

## 10. Edge cases

- **No size ever set.** Grid created at the default (§4.2). A full-screen app
  paints nothing at 0x0 anyway, so there is little to get wrong, and the first
  real size resizes the grid before anything interesting arrives.
- **Session on the alternate screen at attach.** The repaint's leading
  `ESC[?1049l` forces a real switch so DECTCEM rides across it; that is landed
  and gated on conhost.
- **Observer resize while the control seat is empty.** Reaches the grid through
  the same funnel as a control resize, by construction.
- **Runner stream dies.** `runnerPump` returns and calls `Stop`; the grid dies
  with the mux, like the ring.
- **Server restart.** Unchanged: Detached survivors are Cancelled
  (`server/server.go`), and the grid is in-memory like the `SessionMux` it
  belongs to. Persisting it is not in scope.
- **Old client, new server.** Loses the preamble and the repaint, keeps
  everything else. Named in §6.

## 11. Testing strategy

- **Unit, `server/`**: the grid is fed from `runnerPump` (bytes in, screen out);
  `applyWinSizeFrame` resizes it from BOTH entry points, including the observer
  path with `allowResize`; the replay block is `winsize │ preamble │ ring │
  repaint` with the right frame types; `replaySnapshot`'s conditional trim,
  including the two-episode straddle that makes the naive predicate wrong.
- **Existing gates**: `vtgrid/repaint_test.go` and
  `vtgrid/repaint_console_windows_test.go` keep testing `buildRepaint` after it
  moves, unchanged.
- **Integration**: an attach after an alt-screen app has exited returns the
  screen AND the pre-episode scrollback; a reattach mid-episode still trims.
- **`--raw`**: synthesised frames are distinguishable from PTY frames in its
  output, whatever presentation is chosen.
- **Live**: the thing no test covers — watch a real attach, on a real terminal,
  and see whether the forced buffer switch reads as a flash. §12.

## 12. Deferred, with reasons

- **Whether the forced `ESC[?1049l` is a perceptible flash.** The sequence
  arrives as one contiguous write, so the terminal may never present the primary
  buffer, but nobody has watched it on a live attach. If it does flash, the fix
  is to condition the leave on the visibility actually needing to change — which
  requires knowing the observer's state, which the server does not.
- **Server as the authority for `session snapshot` / the TUI grid pane.**
  Buildable on this, not with it.
- **Persisting the grid across a server restart.** Detached survivors are
  Cancelled today; changing that is the separate question of whether an
  interactive task should outlive the server at all, and it has its own
  answer already recorded.
