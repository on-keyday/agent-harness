# WebUI Task List Redesign

**Date:** 2026-07-18
**Status:** Approved

## Problem

The WebUI Tasks tab renders every task the server returns as a flat,
one-line-per-task list in insertion order (oldest first). As task count
grows this becomes hard to scan:

1. **Live sessions are buried** — terminal tasks (Succeeded / Failed /
   Cancelled, e.g. piles of `runner_disconnected` failures after a fleet
   restart) dominate the list and hide the few Running / Detached sessions
   the user actually acts on.
2. **No way to find a specific task** — no search or filtering; locating a
   past task to resume means visually scanning rows.
3. **Rows are unreadable** — one line packs id, status, kind, full repo
   path, origin attrs, caps, and prompt; on a phone it overflows badly.
4. **Ordering is wrong for the workflow** — insertion order puts the most
   recently touched tasks at the bottom. The desired order is "most
   recently active first".

## Scope

Client-side only: `webui/index.html`, `webui/static/main.js`,
`webui/static/style.css`, plus ONE wasm-bridge mapping addition in
`cmd/harness-webui-wasm/main.go`: `TaskInfo.ErrorMessage` is already on
the wire (`toTaskInfo` calls `SetErrorMessage`) but is not mapped into
the JS task object; add `"errorMsg": string(t.ErrorMessage)` so failed
rows can show their error. No server or protocol changes.

- The List RPC's most-recent-100 cap (`server/task_handler.go`,
  `h.Tasks.List(100)`) is **intentionally untouched** — the live install
  currently holds ~25 tasks, and older terminal tasks are handled by
  `prune`. Filtering/search therefore only sees what the server returns;
  that matches the current list's visibility and is accepted.
- TUI/CLI are out of scope: this is a presentation-layer change, and the
  TUI's selection-based list has different mechanics. Activity-sort for
  the TUI can be a separate change if wanted later.

All other data needed is already delivered to JS by the wasm bridge
(`cmd/harness-webui-wasm/main.go` `harnessSnapshot`): `createdAt`,
`startedAt`, `endedAt`, `outputIdleMs`, `status`, `repoPath`, `prompt`,
`agentProfile`, `caps`, origin attrs. Wire timestamps are UnixNano
(`toTaskInfo` uses `UnixNano()`); JS converts to ms with `/1e6`.

## Design

### 1. Filter bar (static markup in `index.html`)

A control row above `#task-list`, **outside** the re-rendered container so
the 5s snapshot poll never steals input focus or resets chip state:

```
[Active (5)] [Finished (20)] [All (25)]   [filter……]
```

- **Status chips** — exactly one active at a time:
  - `Active` (default): every task whose status is NOT in
    `TERMINAL_STATES` (`Succeeded`, `Failed`, `Cancelled`) — i.e.
    Running, Detached, Pending, etc.
  - `Finished`: only terminal statuses.
  - `All`: everything.
  - Each chip shows a live count computed from the latest snapshot.
- **Text filter** — case-insensitive substring match over `id`,
  `repoPath`, `status`, `agentProfile`, and `prompt`. (In practice
  interactive tasks have empty prompts, so repo path and id are the
  effective search keys.) Applies on `input` against the cached snapshot;
  no server round-trip.
- Chip selection and filter text live in JS memory only — a page reload
  resets to Active/empty. No persistence.

### 2. Sort: last-activity descending

```js
activity(t) = t.outputIdleMs >= 0
  ? Date.now() - t.outputIdleMs        // live session: real last-output time
  : max(t.endedAt, t.startedAt, t.createdAt)  // finished / never-started
```

Busy live sessions sort to the top, then recently-ended tasks, then old
history. Wire timestamps (`createdAt`/`startedAt`/`endedAt`) must be
converted to the same unit as the `Date.now()`-derived value — verify the
wire unit (ns vs ms) at implementation time and normalize to ms. Zero
values (unset `startedAt`/`endedAt`) are treated as "absent", not as epoch.

Note: the live-session branch derives from the browser clock while the
finished branch uses server wire timestamps, so browser↔server clock skew
can shift relative order between a live and a finished task by the skew
amount. Skew on this LAN deployment is seconds at worst and only affects
adjacent ordering, never the Active/Finished partition — accepted.

### 3. Two-line card rows (no horizontal scroll at 390px)

```
● Running  my-repo          busy    claude
  5b65231c…  from=tui resumed_by=webui  caps=all
```

- **Line 1:** colored status dot + label (Running=green, Detached=yellow,
  Failed=red, Succeeded/Cancelled=muted gray), repo tail directory name
  (full path in `title` attr), activity badge (`busy` / `idle:Nm`), agent
  profile name.
- **Line 2:** small muted text: first 12 hex of id, `from=`/`by=`/
  `resumed_by=` attrs, caps label. If the task has an error message,
  append it in red.
- **Line 3 (only when prompt is non-empty):** prompt, wrapped, clamped to
  ~2 lines.
- Row click still toggles the task's inline action sheet; the existing
  `buildTaskSheet` content and single-open-sheet behavior are unchanged.
- Palette matches the existing dark theme (#1e1e1e background family);
  layout uses flex/wrap so 390px width shows no horizontal scroll.

### 4. Unchanged

- `buildTaskSheet` actions: Copy id, Preview, Reattach, idle-notify,
  Resume variants, Files, Cancel.
- Snapshot polling cadence and `refreshSnapshot` structure — only
  `renderTaskList` (and small helpers) change, plus the new filter-bar
  wiring.
- `renderFileTaskSelect` and every other consumer of `snap.tasks`.

### Error handling

- Missing/undefined new fields (older server): `outputIdleMs` already
  defaults to -1 sentinel; timestamp fallbacks land on `createdAt`, which
  is always set. No new failure modes.
- Empty filter result: render a `(no matching tasks)` placeholder in the
  existing `task-empty` style so the state is visibly "filtered", not
  "broken".

## Testing

Playwright (existing MCP setup) against the live WebUI:

- Desktop and 390px-wide viewport: no horizontal page scroll.
- Chip switching filters correctly and counts match the snapshot.
- Text filter narrows by repo substring and by id prefix.
- Sort order: a busy live session ranks above idle/finished tasks.
- Action sheet still opens per row; Files action still navigates; a
  resume from the sheet still works.
- Input round-trip on an attached session (regression guard for the
  shared-xterm path is not touched, but attach must still work).
