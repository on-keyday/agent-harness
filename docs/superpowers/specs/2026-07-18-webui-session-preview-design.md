# WebUI Session Preview — Design

Date: 2026-07-18
Status: shipped; snapshot engine SUPERSEDED by
`2026-07-18-webui-live-session-preview-design.md` (live stream + ⏸/▶ —
the modal, entry points, gating, and scale-to-fit here remain accurate)

## Problem

In the WebUI task list, interactive sessions are identified only by task id /
kind / status. To find "the session I want", the user must reattach (or 👁
View) each candidate, which switches the main terminal away. There is no way
to peek at a session's current screen before committing to it. This hurts most
on phones, where switching back and forth is slow.

## Goal

A tap-to-open **preview pane**: from a task row, open a small read-only
snapshot of that session's current screen, without disturbing the main
terminal or the session's controlling client. One-shot snapshot with a manual
refresh button (no live streaming, no auto-poll).

## Non-goals

- Live/streaming preview (view stream held open) — rejected for lifecycle
  complexity; the one-shot model covers the "which session was this?" use.
- Per-row always-on thumbnails — rejected for view-attach load.
- TUI parity in this cycle. CLI parity already exists (`harness-cli session
  snapshot`). TUI can add a preview later cheaply (native builds already have
  `cli.SessionSnapshot`); that is a separate cycle.
- Server/wire changes. None are needed: `AttachMode_View` + size replay are
  already deployed.

## Mechanism (approved approach)

Reuse the view-attach byte-collection path that `harness-cli session
snapshot` uses, but render in the browser with xterm.js instead of the Go VT
emulator (which is `!js`-tagged and stays that way).

### Go / wasm side

1. **Move `collectRaw`** from `cli/snapshot_native.go` (build tag `!js`) into
   a new shared file `cli/snapshot_raw.go` with **no build tag**, exported as
   `CollectRaw` (the wasm bridge calls it cross-package). Same collection
   logic; `collectScreen` / `SessionSnapshot*` stay in the `!js` file.

   As-shipped amendment: an untagged file was impossible against the objtrsf
   version this branch started from — the whole `objtrsf/exec` package
   (including the client-side `CommandExecutionStream`) carried a file-level
   `//go:build !js`. objtrsf therefore split the client wrapper into an
   untagged `exec/exec_stream.go` (upstream commit dfc8b85, consumed here by
   bumping go.mod to v0.0.0-20260718082007-dfc8b85c3762). `CollectRaw` also
   wraps `attachSessionRPC` + `NewCommandExecutionStream` directly instead of
   calling `AttachSession`, because the native and js builds define
   `AttachSession` with different signatures (the js variant installs the
   browser-xterm singleton — the wrong tool for a peek).
2. **New wasm bridge function** in `cmd/harness-webui-wasm`:

   ```
   harness.sessionPreview(taskIDHex, settleMs?) ->
       Promise<{bytes: Uint8Array, rows, cols, hasSize}>
   ```

   Implementation: `currentClient()` → `c.CollectRaw(ctx, id, settle)` →
   resolve with the captured bytes + replayed terminal size. `settleMs`
   defaults to the same settle value the CLI uses. It uses an independent
   view-mode exec stream and must NOT touch the `activeInteractiveSession`
   singleton — the user's live attached session keeps working during a
   preview.

### JS / UI side

3. **Entry points** (operator-surface parity within the WebUI — the task
   sheet is not the only route to a session):
   - Task sheet: rows that offer ↪ Reattach / 👁 View (interactive kind,
     Running/Detached — same gating) additionally get a "🔍 プレビュー"
     button. Terminal-status and non-interactive rows never show it.
   - Notification feed: actionable entries that offer Reattach/View get the
     same プレビュー action.
   - WebUI cmdline: `preview <task-id>` opens the same modal (+ help text).
4. **Preview pane**: modal-style pane. On open, create a throwaway read-only
   xterm.Terminal sized to the session's real `rows×cols` (80×24 fallback
   when `hasSize` is false), write the bytes into it, and fit the pane with
   CSS `transform: scale()`. Pane header: shortened task id, an "↪ Reattach"
   shortcut (closes the pane and calls the existing `reattachTo(id, false)`),
   🔄 refresh (dispose + recreate xterm, re-run `sessionPreview`), ✕ close
   (dispose xterm).
5. **Style**: match the existing dark theme (#1e1e1e / #d4d4d4). At ≤600px
   the pane becomes a full-width bottom-sheet-style panel. Verify desktop and
   390px layouts.

## Error handling

- `sessionPreview` rejections (session gone, attach refused, not connected)
  render as a message inside the pane using the existing `{message}` error
  shape. Failures never affect the task list or its 5s snapshot poll.
- Preview while the same session is attached in the main terminal must be
  harmless (view mode is non-takeover by design) — covered in verification.

## Verification

- `make check` and the wasm build target (Makefile) pass.
- Playwright against the live WebUI (wasm hot-reloads; no server restart):
  resume a cheap bash-runner session for a real PTY, then verify:
  a. the preview pane shows the session's actual screen text (xterm is
     DOM-rendered, so snapshot-readable);
  b. opening a preview while a live main-terminal attachment is active does
     not corrupt the main terminal (echo round-trip still works);
  c. the 390px-wide layout renders as a bottom sheet and is usable.
