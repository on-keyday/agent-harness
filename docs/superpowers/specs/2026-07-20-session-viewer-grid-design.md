# Session Viewer Grid (tmux-style multi-pane monitoring) — Design

Date: 2026-07-20
Status: draft (brainstormed with user)
Builds on: `2026-07-18-webui-live-session-preview-design.md` (the live view-attach
pump, scale-to-fit math, and guard shapes are reused; the preview modal itself is
unchanged).

## Problem

Monitoring several parallel worker sessions (e.g. multiple agents mid-task)
means cycling through full-screen attaches or opening the WebUI preview one
session at a time. There is no way to see N sessions' screens at once.

## Goal

A live, read-only, tmux-lookalike grid of session panes in the TUI and WebUI.
Each pane is an `AttachViewer` observer of one interactive session. Selecting a
pane jumps to the existing full-screen attach. The grid never influences the
session: PTY size, input, and lifecycle are untouched.

## Non-goals

- Interactive input from a pane (that is what attach / cowrite are for).
- Splitting ONE session into multiple shells (harness's unit is the session;
  "split" = create another session and show it in another pane).
- Server-side rendering to pane size (needs a server VT grid — the known
  WONTFIX area; the server stays a byte ring).
- A CLI grid. The grid is inherently a full-screen display, i.e. the TUI
  binary's job. CLI keeps its one-session primitives (`session snapshot`,
  `session attach --view`).

## Window-size model (the core decision)

**The session PTY size is never touched by the grid.** The control client
remains the sole size authority (the existing model: cowriter winsize frames
are already dropped, `session_mux.go` `forwardCowriterFrames`). tmux-style
min-size aggregation was rejected: resizing a worker's PTY to a pane size
would re-render the worker's TUI tiny for its real consumer and pollute the
ring — the opposite of non-intrusive monitoring.

Panes therefore render the session's REAL grid locally:

- **WebUI**: xterm sized to the real rows×cols, CSS-scaled to fit the pane
  (the preview modal's scale-to-fit math, per pane).
- **TUI**: a `charmbracelet/x/vt` emulator per pane at the real size; the pane
  shows a **bottom-left crop** (activity in shells and claude concentrates at
  the bottom). Per-pane scrolling is a later upgrade, not v1.
- **No recorded size**: fall back to 80×24 and mark the pane, mirroring
  `cli/snapshot_native.go`'s fallback.

### Server fix: mid-stream winsize fan-out (prerequisite, standalone value)

Today an observer learns the PTY size exactly once, in the attach preamble
(`session_mux.go` `attachObserver` replays `lastWinSize` first). A mid-stream
resize by the control client — including a takeover attach from a
different-sized terminal — reaches only `lastWinSize` and the runner
(`forwardControlFrames`); live observers keep rendering at the stale size.

This is a LATENT BUG already shipped: `session attach --view` (CLI), the TUI
`v` view-attach, and the WebUI live preview are all long-lived observers that
survive resizes. The client side is already prepared (the exec stream parses
mid-stream `TerminalWindowSize` frames into `LastWindowSize()`; the preview
pump already polls it and fires `harness_previewResize`) — only the server
never sends them.

Fix: in `forwardControlFrames`, after recording `lastWinSize`, enqueue the
same verbatim frame to every observer channel (`m.viewers` holds viewers AND
cowriters), non-blocking under `m.mu` with the existing drop-if-full policy.
Ordering vs `runnerPump` output is not strictly serialized (two producers);
a few frames rendered at the old size self-heal on the next full repaint —
same race tmux tolerates.

## Mechanism

### Server

- The winsize fan-out fix above. No other server change: `AttachViewer`,
  per-viewer bounded queues (drop-if-slow), and the replay preamble are used
  as-is.

### WebUI

- New grid view (entry from the task list / a dedicated 🔲 control). Panes are
  the live-preview engine generalized from a singleton to N instances:
  `StartPreview`-equivalent per pane with its own generation guard, its own
  throwaway xterm, and the same open/write/resize/closed callback quartet,
  keyed by pane id. All panes share the ONE long-lived wasm client (the
  existing rule: no per-pane Dial).
- Pane population v1: all sessions with a live detachable PTY (Running /
  Detached), activity-sorted, capped (e.g. 9); per-pane ✕ to dismiss. An
  explicit picker is a later upgrade.
- Pane tap → the existing full-screen reattach route (grid closes first; the
  main terminal keeps its single-writer generation guard invariants).
- `harness_previewResize` per pane: `term.resize` + re-run scale-to-fit (now
  actually firing mid-stream thanks to the server fix).
- Stream death (session exit, takeover policy, drop-if-slow) → "(stream
  ended)" note in the pane + a per-pane reconnect ▶, reusing the preview's
  paused-state pattern. No auto-retry in v1.
- Dark theme #1e1e1e palette; ≤600px = 1-column stack; verify desktop + 390px
  in Playwright.

### TUI

- New grid screen (normal bubbletea view — NOT `tea.Exec`; the terminal stays
  under bubbletea control).
- Per pane: a goroutine owns one view-attach stream and one `vt.Emulator`
  sized from the preamble winsize; drains emulator query responses
  (`io.Copy(io.Discard, emu)`, the `snapshot_native.go` pattern); applies
  mid-stream winsize frames via emulator resize.
- Rendering: on a coalescing tick (~10 Hz), panes whose emulator changed are
  re-extracted (bottom-left crop of the cell grid, plain text v1 — styled
  cells are a later upgrade) and composed with lipgloss boxes. Byte arrival
  must NOT call `program.Send` per chunk (known unbuffered-Send blocking
  pitfall); goroutines set a dirty flag the tick collects.
- Keys: pane focus movement (hjkl/arrows), Enter = full attach (existing
  attach flow), `x` = dismiss pane, `q`/Esc = leave grid (detach all
  observers).
- Reuses the long-lived `*cli.Client` (`XWith(client)` helper shape).

### Call-site enumeration (the shared-op rule)

The winsize fan-out changes what long-lived observers receive. Consumers to
verify against the new mid-stream frames: CLI `attach --view` (raw
passthrough — frames must keep being consumed by the exec stream, not leak
into the terminal byte stream), TUI `v` view-attach, WebUI live preview,
cowrite, and both new grid surfaces.

## Error handling

- Pane attach failure → error text in the pane, others unaffected.
- Server drops a slow observer → that pane gets the stream-death path.
- Session ends → stream-death path; the pane offers reconnect, which reports
  the attach error if the session is really gone.
- Leaving the grid closes every observer stream immediately (no background
  viewers).

## Verification

a. Liveness: two sandbox sessions echo distinct markers; both appear in their
   panes with no manual action (WebUI via Playwright; TUI via nested run).
b. Size churn: while the grid watches a session, attach to it from a
   different-sized terminal (takeover) → the pane re-renders at the new grid
   without corruption. Repeat for the PRE-EXISTING surfaces (attach --view,
   TUI v, WebUI preview) to confirm the latent-bug fix.
c. Non-interference: with the grid open, the session's control client does an
   echo round-trip (input AND output intact); PTY size unchanged throughout.
d. Slow-viewer drop: artificially stall one pane; the server drops only that
   observer, other panes and the control client unaffected.
e. Scale/crop: a full-screen app (e.g. claude) in a pane is recognizable in
   WebUI (scaled) and shows its bottom region in TUI (crop).
f. WebUI 390px: 1-column stack renders and scrolls.
g. `make check` / wasm-check / vet / test.

## Amendment 2026-08-19 — which tasks a grid shows

Pane population above is "all live sessions, activity-sorted, capped". Once a
fleet holds several supervisors' worth of workers, that page answers the wrong
question: the operator wants one supervisor's crew, not everything running.

So "which tasks" becomes an explicit choice with four modes, decided once in
`cli.GridSet(tasks, mode, anchor, ids) -> (set, label, err)` and consumed by
every surface. `GridSet` returns the LABEL as well as the set, because two
surfaces formatting their own is how one choice ends up with two names.

| mode | set |
|---|---|
| `all` | every visible task (TUI `g`, the WebUI show button, bare `grid`) |
| `subtree` | an anchor's **working set** (TUI `z`, `grid --under`) |
| `descendants` | the same minus the anchor itself (TUI `Z`, `--descendants`) |
| `ids` | exactly the tasks named, in the order named (`grid <id>...`) |

**The anchor's working set is BOTH halves**: its subtree (every task it spawned,
transitively) *and* the tasks its own `TaskScope` names individually. Either
alone is a half-answer. The creator tree says what the task STARTED; a scope's
`ids:` names peers it was handed that are nobody's descendant — which is exactly
why they had to be named. A supervisor working with a task it did not spawn is
the case a subtree-only grid cannot show, and the first cut of this amendment
shipped that bug: the ids half was dropped along with the scope-grammar input,
which was a different decision entirely.

**There is still no scope grammar for an operator to type.** `--scope`'s
`subtree` / `ids:` are anchored at the holder task, and an operator has no
holder task; the ids are read from the task's stored scope
(`cli.TaskScopeIDs`), where those words already mean what they mean.

- **Membership**: `cli.TaskSubtree` reads `BuildTaskTree`'s pre-order rows (a
  node's descendants are the rows following it until the depth drops back)
  rather than walking creator links a second time — one definition of "who is
  whose child", shared with the tree view, the `ls --tree` gutter and the WebUI
  graph. An anchor or a named id the operator cannot see is an ERROR, not an
  empty set: those ids were typed, and a typo that silently shows nothing is
  the failure this repo keeps re-fixing. A legitimately empty set (the
  descendants of a task that has none) is not an error.
- **Tileability stays where it was**: `gridLiveTasks` (TUI) /
  `liveInteractiveTasks` (WebUI) still decide what a pane can show, and the
  WebUI's per-session グリッドに含める toggles still subtract. `GridSet` picks
  candidates, the existing predicates filter them, and the pane cap
  (24 TUI / 9 WebUI) applies to the result.
- **Entry points**: TUI keys `g` / `z` / `Z` on the tasks pane and a `grid`
  verb on its command line (`grid [<id>...]`, `grid --under <id>
  [--descendants]`, ids resolved from prefixes like every other TUI verb);
  WebUI task sheet `▦ この配下をグリッド` and `▦ 配下のみ (自分を除く)` —
  offered on EVERY task, not only live interactive ones, because the useful
  anchor is often a one-shot supervisor or a finished parent whose workers are
  still running — plus the same `grid` command (full 32-hex only, no prefix
  resolution, matching `prune`). Bare `grid <id...>` is an explicit list, never
  expanded into subtrees, mirroring `--scope ids:`.
- **An empty result opens nothing.** Both surfaces report
  `grid <label>: no live interactive session in this set (N task(s) in it)` on
  their result surface (TUI cmdresult / WebUI `appendCmdOutput`) instead of
  opening a full-screen overlay that says the same thing. The count matters: a
  set of four tasks none of which is watchable is a different situation from a
  set of none.
- **The scope is always stated**: TUI status bar `scope:<label>`, WebUI modal
  title `(scope: <label>)`, where label is `all` / `<id8>+desc` /
  `<id8>+desc+ids×N` / `<id8>/desc-only` / `ids×N`. The `+ids×N` suffix appears
  only when the scope half actually contributed, so the common case stays quiet
  and a pane that is nobody's child is never unexplained. A blank would read as
  "no narrowing in effect", which is what a narrowed grid missing its label
  also looks like.
- No wire, WAL or `TaskInfo` change: the choice is per-view UI state that dies
  with the overlay. The WebUI reaches the shared decision through a
  `harness.gridSet({mode, anchor, ids}) -> Promise<{ids, label}>` wasm export.

Verification for this amendment: (h) a three-level tree (parent → worker →
grandchild) plus an unrelated session — `z` on the parent tiles exactly its own
live descendants, `Z` drops the parent's own pane, the unrelated session appears
in neither; (i) a task granted `--scope subtree+ids:<unrelated>` shows that
unrelated session under `z` and says so in the label; (j) the same through the
WebUI buttons and every `grid` form, desktop and 390px; (k) an anchor with
nothing watchable under it reports instead of opening.

## Amendment 2026-11-01 — WebUI terminal cells can crop like the TUI's

The two grids shrink a session's screen in different ways. The TUI keeps the
glyph size and shows the bottom-left part of the screen that fits the pane
(`tui/pane_streamer.go` `PaneStreamer.Render`), panned with ⇧↑↓←→ and reset
with `0`. The WebUI lowers the xterm font size until all the session's columns
fit the cell (`fitPaneScale`, floor `PANE_FONT_MIN = 1`). On a 390px phone a
cell body is about 340px wide, so a 120-column session renders at about 5.7px
and a 200-column one at about 3.4px. That is not readable. The figures are
computed from `fitPaneScale`'s own measurement (13px font = 6.5px per column),
not measured on a device.

The WebUI grid now has both ways, and a header button switches between them.

| # | Decision | Decided-by |
|---|---|---|
| 1 | The WebUI grid's terminal cells get a crop mode with the TUI's semantics: the glyph size is unchanged, the cell shows the bottom-left of the screen, and the operator pans | operator, 2026-11-01 (author's proposal) |
| 2 | One button in the grid header switches every terminal cell between crop (`表示: 等倍`) and fit (`表示: 縮小`) | operator, 2026-11-01 |
| 3 | Before the operator presses it, each opening uses crop at ≤600px (the breakpoint where the grid becomes one column) and fit above that | operator, 2026-11-01 |
| 4 | After the operator presses it, that choice is used for every later opening until the page reloads. It is not stored in `localStorage` | author |
| 5 | Crop uses the font size the single preview starts at (`PANE_FONT_BASE`, 13px), about 52 columns on a 390px phone. The cell body is at least `40vh` (and at least 200px) tall and scrolls. It grows when the grid row is taller: on a desktop with one row of cells, the row stretches to the modal's height | author |
| 6 | The crop's bottom edge follows the TUI's rule (`cropBottomRow`): the last row with anything drawn on it, or the cursor's row if that is lower. The left edge is column 0 | author |
| 7 | The operator pans with the cell body's native scrolling. The xterm inside takes no pointer events, and a click on the body focuses the terminal | author |
| 8 | A pan stops the cell following new output, and a `⤓` button appears in the cell header. Pressing it returns the cell to the bottom-left and turns following back on. This combines the TUI's `↑N`/`→M` markers and its `0` key in one control | author |
| 9 | Event-stream cells are the same in both modes. They render a chat transcript at 12px and have no terminal to crop | author |
| 10 | The single preview modal still always fits. It is one session at full modal width, which is a different trade-off from a grid of cells | author — **reversed** by the operator on 2026-11-02 (amendment below) |

Why #7 avoids xterm's own pointer handling. The bundled xterm (6.x) handles
touch with its own gesture recognizer and calls `preventDefault`, and it scrolls
its own scrollback. With no scrollback (an alternate-screen app such as claude),
it turns a wheel event into arrow keys sent to the session. A pan that reached
the xterm would therefore either fail to move the cell or type into the agent.
The cost is that crop mode has no mouse text selection in a cell. Fit mode keeps
it.

What the TUI already has and the WebUI now matches: the "Per-pane scrolling is
a later upgrade" line under Window-size model was done in the TUI by its
shift-arrow pan. This amendment gives the WebUI the same thing.

Surfaces: TUI — already crops, no change. CLI — has no grid (Non-goals). WebUI
`grid` command and `▦` task-sheet buttons — they open the same modal, so they
get the same mode and button. No wire, wasm or server change.

Verification: (l) at 390px the grid opens in crop. A 160-column session that
printed 60 lines shows its last line at the cell's bottom with readable text.
A line ending past column 52 is cut on the right and can be reached by
scrolling. (m) A pan shows `⤓` and stops following (new output does not move
the cell). `⤓` returns the cell to the bottom-left. (n) Tapping a cropped cell
focuses it, and typed text reaches the session. (o) The header button switches
to fit and back. At 1280px the grid opens in fit. (p) `node --test` covers
`cropBottomRow`, `cropScrollTop` and `gridCropDefault`.

## Amendment 2026-11-02 — crop in the single preview, ⏸/▶ in grid cells

Two gaps between the WebUI's two session views. The single preview always
fitted (#10 above), so on a 390px phone a 160-column session rendered at about
4.7px (measured: `fitPaneScale` set 4.725px in Playwright). The grid had no ⏸/▶,
although the Mechanism section above planned "a per-pane reconnect ▶, reusing
the preview's paused-state pattern", and a cell whose stream ended already said
`(ストリーム終了 — ▶ で再接続)` with no ▶ to press.

| # | Decision | Decided-by |
|---|---|---|
| 11 | The single preview gets the grid's crop mode (#1, #5–#8): the same `表示` toggle in its header, and the same `⤓` in its header. This reverses #10 | operator, 2026-11-02 |
| 12 | Each grid terminal cell gets the single preview's ⏸/▶: ⏸ closes the cell's stream and keeps its frame, ▶ re-attaches and the replay jumps to the current screen. A stream that ends by itself turns the button to ▶ | operator, 2026-11-02 |
| 13 | The preview and the grid keep separate toggles. Both use #3's default (crop at ≤600px) and #4's lifetime. `gridCropDefault` is renamed `cropDefault` because both modals call it | author |
| 14 | ⏸/▶ is per cell, not one button for the whole grid. That is the preview's semantics, and it is what the stream-ended note points at | author |
| 15 | Event-stream cells get no ⏸/▶. They attach through `streamPaneStart`, not the preview pump, and #9 already keeps them separate | author |
| 16 | ▶ turns following back on and resets the pane's scroll to the top-left before re-attaching. An offset panned in the old frame means nothing in the new one. Without the reset, the browser clamps the offset when the body is emptied, and that scroll event would be read as the operator's pan | author (the clamp was found in Playwright) |

The single preview's body was already the scroll container (`.preview-body`
has `overflow: auto`), and the modal already bounds its height. So crop there
needs no fixed height, unlike a grid cell (#5). Only the spacer's clipping and
the xterm's pointer events change (`.preview-crop` in `style.css`). The cost is
#7's cost: in crop mode, the preview has no mouse text selection and the wheel
does not reach xterm's own scrollback. Fit mode keeps both.

Surfaces: the `preview` command and the task sheet's `🔍 プレビュー` open the
same modal. The `grid` command and the `▦` buttons open the same grid. TUI: it
has no single preview, and its grid has no pause. Nobody asked for a TUI pause,
so none is added. No wire, wasm or server change.

Verification (Playwright against `scripts/dummy-harness.sh`, a 50×160 bash
session that printed 60 lines of 120+ columns): (q) at 390px the preview opens
in crop at 13px, scrolled to the prompt at the bottom-left, with no horizontal
page overflow. A pan shows `⤓`, and `⤓` returns to the bottom-left. A tap
focuses the terminal, and typed keys reach the session (`echo ZQ9` echoed). (r)
The toggle switches to fit (4.725px) and back. (s) ⏸ then ▶ in the preview
follows again, with `⤓` hidden. Before #16's reset it came back at the top
with `⤓` shown. (t) In the grid, ⏸ on a cell keeps it from showing output
printed while paused. ▶ shows that output and follows. Killing a session turns
its cell's button to ▶, next to the stream-ended note. (u) At 1280px the
preview opens in fit and its toggle switches to crop.
