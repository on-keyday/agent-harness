# TUI Active Port-Forward List Modal

**Date:** 2026-07-22
**Status:** Approved (brainstorming)

## Problem

When several port forwards are open from the TUI, there is no way to see them
all at once. The only existing view is the transient stop-picker (`P`/`B`),
which is scoped to a *single task + single direction* and only appears when more
than one forward matches. The user loses track of which forwards are currently
live.

Verbatim: *"I want something in the TUI where I can see a list of open port
forwards... I lose track of which are open and which are closed."*

## Decisions (settled during brainstorming)

1. **Scope: this TUI's own forwards only.** No server change. The list shows
   exactly what this TUI process opened. Forwards opened by a separate
   `harness-cli forward` invocation or another TUI/WebUI are out of scope.
2. **Live-only.** The list shows currently-live forwards. Stopped/failed
   forwards are not retained with a "closed" marker — they simply leave the
   list, matching the existing removal-on-exit behavior.
3. **Modal overlay.** A full-screen overlay opened by a single key and closed
   with Esc, mirroring the existing Connections modal (`C`). Not an
   always-visible panel (avoids compressing the layout when zero forwards).
4. **TUI-only feature.** `activeForwards` is process-local client state that
   does not exist in the CLI or WebUI. A CLI/WebUI equivalent would require a
   different (server-side) data source and is explicitly not built here.

## Data source (no new state)

`App.activeForwards map[int]*PortForwardSession` (`tui/app.go:86`) is the single
source of truth. Entries are:

- **added** on `PortForwardStartedMsg` (`tui/app.go:651`), and
- **removed** on `PortForwardStoppedMsg` (`tui/app.go:656`), which fires for
  every exit path — normal stop, `-R` bind failure, and remote-side close.

Therefore the map already equals the set of live forwards. `PortForwardSession`
carries `ID`, `TaskID`, `Direction` (`ForwardLocal`/`ForwardRemote`), `Spec`,
`Cancel`. No new fields, no new messages, no server RPC.

## Components

### `ForwardsModal` (new, in `tui/portforward.go`)

A bubbles `table.Model` wrapper modeled on `ConnsModal` (`tui/conns.go`):

- **Columns:** `task` (short id, 12 chars via `pfShortID`) · `dir` (`-L`/`-R`
  via `ForwardDirection.flag()`) · `spec` (remaining width).
- **Methods:**
  - `Open()` / `Close()` / `IsOpen() bool`
  - `SetSize(w, h int)` — reserve 4 rows for border+header+footer (as
    `ConnsModal.SetSize`).
  - `SetSessions(sessions []*PortForwardSession)` — rebuild rows from a
    pre-sorted slice. Called by App on open and on any Started/Stopped while the
    modal is open.
  - `Update(msg tea.Msg) (ForwardsModal, tea.Cmd)` — forward keys to the table
    (up/down scroll) while open.
  - `View() string` — header `active port forwards (N)`, the table, footer
    `Esc: close · P/B stop from tasks pane`. When `N == 0`, render a
    `no active forwards` line instead of an empty table.
- **Read-only.** No stop action from the modal; stopping stays on the tasks
  pane's `P`/`B` keys.

### Sort order

Stable ordering by `(TaskID, Direction, ID)`. Add a helper (e.g.
`sortedForwards(m map[int]*PortForwardSession) []*PortForwardSession`) next to
the existing `selectForwards`, which only filters one task+direction; the new
helper spans all tasks.

### App integration (`tui/app.go`)

- **New field** `forwardsModal ForwardsModal`, constructed in the App
  initializer.
- **Key `f`** (free: `F` = file picker, `C` = connections): guarded by
  `a.focus != focusCmdline && !logsEditing`, placed next to the `C` branch
  (~`tui/app.go:902`). On press: build the sorted slice, call
  `SetSessions` → `SetSize(a.width, a.height)` → `Open()`.
- **While open:** route table-nav keys to `forwardsModal.Update`, and add the
  modal to the Esc-close chain (mirroring the Connections handling block at
  ~`tui/app.go:690`).
- **Live update:** in the `PortForwardStartedMsg` / `PortForwardStoppedMsg`
  handlers, if `a.forwardsModal.IsOpen()`, call `SetSessions` with the freshly
  sorted slice so an open list reflects add/remove immediately.
- **Render:** in `App.View`, when `a.forwardsModal.IsOpen()`, render it via the
  same overlay path used for the Connections modal.

## Out of scope (YAGNI)

- Server changes / querying the `-R` `remoteForwardRegistry`.
- Global cross-client view (`-L` forwards are inherently client-local and cannot
  be enumerated globally).
- Retaining closed/failed forwards with a status column.
- Stopping a forward from within the modal.
- Live-probing whether a listener socket is still bound (a forward in
  `activeForwards` is by construction still running).

## Testing (`tui/portforward_test.go`)

- `SetSessions` builds N rows in `(task, dir, ID)` order.
- `-L` / `-R` flag rendering in the `dir` column.
- Empty state: `SetSessions(nil)` → header count 0, `no active forwards`.
- `Open`/`Close` toggles `IsOpen`.
- If feasible with the existing App test harness: pressing `f` opens the modal
  and Esc closes it (style per `conns_test.go` / `portforward_test.go`).
