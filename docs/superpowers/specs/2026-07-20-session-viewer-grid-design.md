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

## Amendment 2026-08-19 — subtree scope

Pane population above is "all live sessions, activity-sorted, capped". Once a
fleet holds several supervisors' worth of workers, that page answers the wrong
question: the operator wants one supervisor's crew, not everything running.

**The narrowing is an ANCHOR, not a scope expression.** One task is named — the
row the cursor is on, the row whose sheet is open — and the grid shows that
task plus every task it spawned, transitively. There is deliberately no
scope-grammar input on this path: `--scope`'s `subtree` / `ids:` grammar is
anchored at the holder task, and an operator has no holder task, so reusing the
spelling would make the same string mean two different things depending on
which field it was typed into.

- **Membership**: `cli.TaskSubtree(tasks, anchorHex)`, which reads
  `BuildTaskTree`'s pre-order rows (a node's descendants are the rows following
  it until the depth drops back) rather than walking creator links a second
  time. One definition of "who is whose child", shared with the tree view, the
  `ls --tree` gutter and the WebUI graph. An anchor not in the visible set
  yields EMPTY — never the unfiltered set.
- **Tileability stays where it was**: `gridLiveTasks` (TUI) /
  `liveInteractiveTasks` (WebUI) still decide what a pane can show, and the
  WebUI's per-session グリッドに含める toggles still subtract. The subtree
  picks candidates, the existing predicates filter them, and the pane cap
  (24 TUI / 9 WebUI) applies to the result.
- **Entry points**: TUI `z` on the tasks pane (`g` remains the whole fleet);
  WebUI task sheet `▦ この配下をグリッド` — offered on EVERY task, not only
  live interactive ones, because the useful anchor is often a one-shot
  supervisor or a finished parent whose workers are still running; WebUI
  `grid --under <32-hex>` (full id only, no prefix resolution, matching
  `prune`). Bare `grid <id...>` keeps its meaning — an explicit list, never
  expanded into subtrees, mirroring `--scope ids:`.
- **An empty result opens nothing.** Both surfaces report
  `no live interactive session under <id> (self + N descendant(s))` on their
  result surface (TUI cmdresult / WebUI `appendCmdOutput`) instead of opening a
  full-screen overlay that says the same thing.
- **The scope is always stated**: TUI status bar `scope:all` or
  `scope:<id8>+desc`, WebUI modal title the same pair. A blank would read as
  "no narrowing in effect", which is exactly what a narrowed grid missing its
  label also looks like.
- No wire, WAL or `TaskInfo` change: the anchor is per-view UI state that dies
  with the overlay. The WebUI reaches the shared filter through a new
  `harness.taskSubtree(anchorId) -> Promise<string[]>` wasm export.

Verification for this amendment: (h) a three-level tree (parent → two workers →
one grandchild) plus an unrelated session — `z` on the parent shows exactly its
own live descendants, `z` on a worker shows only its own, the unrelated session
appears in neither; (i) the same through the WebUI button and `grid --under`,
desktop and 390px; (j) an anchor with no live descendants reports instead of
opening.
