# WebUI Live Session Preview (pause/resume) — Design

Date: 2026-07-18
Status: approved (brainstormed with user)
Supersedes: the one-shot engine of `2026-07-18-webui-session-preview-design.md`
(the modal, entry points, gating, scale-to-fit, and dark/mobile requirements
from that spec stay in force; only the snapshot engine changes).

## Problem

The shipped session-preview modal shows a one-shot snapshot with a manual 🔄
refresh. To watch a session progress (e.g. an agent mid-task), the user must
mash refresh; each press is a full view-attach round-trip and a visible
re-render. The user wants the preview to just keep moving while the modal is
open.

## Goal

While the preview modal is open, hold a view-mode attach stream and feed its
bytes to the preview xterm continuously — a read-only live mini-terminal. Add
a pause/resume control (⏸/▶). Closing the modal disconnects immediately.

## Decisions taken

- **Pause = disconnect, resume = fresh view attach.** ⏸ closes the view
  stream and leaves the frozen frame on screen (zero server/link load while
  paused). ▶ disposes the term and re-attaches; the server's ring replay
  reconstructs the CURRENT screen, so resume jumps to now rather than
  replaying the missed interval. (The buffer-while-paused alternative was
  rejected: preserving VT-parser correctness requires keeping every byte, so
  a long pause grows an unbounded buffer for no preview-relevant benefit.)
- **Live replaces one-shot entirely.** The `harness.sessionPreview` one-shot
  bridge and the JS one-shot render path are REMOVED (the live engine with ⏸
  is a superset; YAGNI). Native `CollectRaw` / `harness-cli session snapshot`
  are unaffected — they stay on the shared `cli/snapshot_raw.go`.
- **Stream death while previewing** (session exit, runner death, takeover of
  the ring, connection drop) surfaces as a "(ストリーム終了)" note in the pane
  and drops the modal into the paused state (▶ visible, frame frozen). ▶
  simply tries a fresh attach; if the session is really gone, the attach
  error renders in the pane like today.
- **Preview stays read-only.** No stdin path; taking input is what ↪
  Reattach is for.

## Mechanism

### wasm side (cli + bridge)

1. **Preview session singleton**, separate from `activeInteractiveSession`
   (a preview must never disturb the main terminal). New js-tagged file
   `cli/preview_wasm.go`:
   - `StartPreview(ctx, c, taskIDHex)` — `attachSessionRPC(View)` →
     `agentexec.NewCommandExecutionStream` (available under js since the
     objtrsf exec_stream split) → store as the singleton (closing any
     previous preview stream first) with a generation counter mirroring the
     `interactiveGen` pattern, then start a recv pump goroutine.
   - `StopPreview()` — idempotent; bumps the generation and closes the
     stream.
   - Recv pump: reads `stream.Stdout()` in chunks. After the FIRST read,
     `LastWindowSize()` is consulted once (the size control frame precedes
     the ring bytes) and `harness_previewOpen(rows, cols, hasSize)` fires;
     then each chunk goes to `harness_previewWrite(Uint8Array)`. After every
     read the pump re-checks `LastWindowSize()` and fires
     `harness_previewResize(rows, cols)` when it changed. On read error/EOF
     the pump fires `harness_previewClosed()` and exits. EVERY callback is
     gated on the pump's generation still being current — a superseded pump
     exits silently.
2. **Bridge** (`cmd/harness-webui-wasm`):
   - `harness.previewStart(taskIDHex) -> Promise<taskIDHex>` (rejects with
     the usual `{message}` shape on attach failure).
   - `harness.previewStop()` — synchronous, idempotent.
   - REMOVE `harness.sessionPreview` and its `harnessSessionPreview`
     implementation.

### JS side (modal engine swap)

3. Entry points (task sheet / notify feed / cmdline `preview <id>`), gating,
   the dialog markup, backdrop/Esc close, the scale-to-fit math, and the ↪
   Reattach shortcut all stay as shipped. Changes:
   - `openSessionPreview(id)`: showModal → `previewStart(id)`; body shows
     "connecting…" until `harness_previewOpen` delivers the grid size, at
     which point the throwaway xterm is created (same options/scale as
     today) and `harness_previewWrite` chunks stream into it.
   - `harness_previewResize(rows, cols)`: `term.resize(cols, rows)` +
     re-run the measure/scale/spacer computation.
   - The 🔄 button becomes **⏸/▶**: ⏸ → `previewStop()`, keep the frozen
     term, flip label to ▶; ▶ → dispose term + `previewStart(id)` again
     (fresh grid via `harness_previewOpen`).
   - `harness_previewClosed`: append a "(ストリーム終了)" preview-note under
     the frozen term and flip the button to ▶.
   - Modal `close` event (✕ / backdrop / Esc / Reattach shortcut):
     `previewStop()` + dispose, immediately.
   - As-shipped guard shape: the wasm hooks check the modal-open +
     `sessionPreviewLive` flags (flipped BEFORE any stop, so a raced late
     hook after close/pause no-ops); `sessionPreviewEpoch` guards the async
     `previewStart` promise across close/reopen, and the success path
     reconciles — if a stream-death hook flipped `live` off during the
     connect window, the freshly-installed stream is stopped rather than
     left running unrendered behind a paused UI.

## Error handling

- `previewStart` rejection → message in the pane (unchanged pattern).
- Late/stale callbacks: double-guarded (wasm generation + JS epoch).
- Whether the server ends view streams on foreign takeover is not assumed
  either way: any server-side end of the view stream (session exit, runner
  death, takeover policy) lands in the same `harness_previewClosed` path —
  frozen frame + ▶. The implementation must not special-case takeover.

## Verification (sandbox server+runner + Playwright, as before)

a. Liveness: with the modal open, `echo MARKER` in the MAIN terminal; the
   marker appears in the preview with NO manual action.
b. Pause: ⏸, echo another marker — it must NOT appear; ▶ — the preview
   catches up to the current screen (both markers visible), then continues
   live.
c. Close = disconnect: after ✕, the server's connection/stream count drops
   (verify via a follow-up echo not reaching any preview callback — no JS
   console callback activity — and by `conns`/server log).
d. Non-interference: live preview open+closed while the same session is
   attached in the main terminal; echo round-trip still works (input AND
   output).
e. Stream-death path: kill the sandbox session (cancel/exit bash) with the
   modal open → "(ストリーム終了)" note + ▶ state; ▶ shows the attach error
   in the pane.
f. 390px layout still renders as the fullscreen sheet with the live term.
