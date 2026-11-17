# Overview — design

Status: approved by the operator in conversation, 2026-11-17.

## Intent

On a phone the WebUI's タスク tab opens on the Runners section, one tall card
per runner slot, so the operator scrolls through every slot before reaching
Tasks; there each task row carries a long meta line (`id kind from by caps
scope delegate … execs`) above what the task is doing, so finding "which task
was it" is a second search (operator, 2026-11-17).

The operator wants one phone screen that summarises the whole harness: which
tasks are running, which need them, which hosts are in the network, which ports
are forwarded, and how the agentboard is moving. A summary, not everything —
each part may be truncated, and the detail stays where it is today.

## Goals

- One screen at phone width (390 px) answers, without scrolling much: what
  needs me, what is running, which hosts, which forwards, what is on the board.
- Each section leads into the existing detail surface for that thing.
- The same summary on the WebUI, the TUI and the CLI, computed once.

## Non-goals

- Replacing any existing tab, table or the session grid.
- Showing every task / runner / forward / topic — each section is capped.
- Changing the TUI's default: it still opens on its tables (the operator does
  not want looks changed unasked; see `2026-11-16-task-list-order-design.md`,
  Decisions). The WebUI's one change is below (remembered tab).
- Any wire / schema change.

## Anti-goals

- A second definition of "needs you", of runner status words, or of a task's
  "now" text. If the overview and the task list can disagree about whether a
  task needs the operator, the overview has failed.
- Agent-written text (board payloads, progress, titles) reaching a terminal
  unescaped.

## Sections

In this order (operator's order, 2026-11-17). `K` is a per-section row cap,
a named constant in the Go package.

| section | count line | rows (≤ K) | tap / drill-down |
|---|---|---|---|
| **介入待ち / needs you** | number of live tasks in `cli.stateGroup` = `needs you` | those tasks, most recent activity first: lifecycle/program badge, repo tail, now-text | a row opens that task (WebUI: the task sheet / id-open dialog; TUI: select it in the Tasks table); the header opens the task list in its current order — the operator's saved order is never changed by a tap |
| **タスク / tasks** | live count per state group: `ready / working / no report / queued`, plus finished count | live tasks not in needs you, most recent activity first: badge, repo tail, now-text | as above; header → task list |
| **ホスト / hosts** | number of distinct runner hostnames, runner slots total, operator clients connected (conns whose role is cli / tui / webui; agent conns are left out because they track the task count) | one row per hostname: goos, slots by status (the `runnerSummary` vocabulary: idle / busy / offline), active tasks / max tasks summed | header → Runners (WebUI: タスク tab scrolled to Runners; TUI: Runners table) |
| **ポート / forwards** | number of forwards | most recent activity first: direction, bind → target, owning task (short id + repo tail) | header → 接続 tab / TUI forwards modal |
| **Board** | number of topics, total messages | topics by last publish, newest first: name, message count, age of last publish, and the head of the newest message | a row opens that topic in the Board tab / TUI board |

Empty sections show their count line with a zero (never elided —
operator-output rule) and no rows.

### Definitions reused, not restated

- needs you / ready / working / no report / queued / finished:
  `cli.stateGroup` (`cli/task_order.go:223`). Export or wrap it; do not copy it.
- Activity order: `cli.TaskActivityAt` (`cli/task_order.go`).
- A task's now-text: progress with age, else `(title)` and the live
  program title, else the prompt cell — the TUI's `aboutCell`
  (`tui/tasks.go:752`). Move it into `cli` so the overview and the TUI table
  call one function; the WebUI's JS copy (`paneAboutText` in
  `webui/static/task_models.js`) is out of scope here, but the overview tab
  must take its text from the Go result, not from that copy.
  - Today only the progress and title branches go through
    `vtgrid.SanitizeTitle`; the prompt branch (`renderPromptCell` →
    `truncatePrompt`, `tui/tasks.go:898-941`) only maps `\n \r \t` to spaces,
    so ESC and C1 bytes from an agent-written prompt pass through. The moved
    function sanitises the prompt branch with `SanitizeTitle` too. This also
    fixes the TUI table's About column.
  - Moving it makes `cli` import `vtgrid` outside `//go:build !js`
    (`cli/snapshot_native.go:1`); it compiles for js/wasm (checked by the
    design reviewer).
- Runner status words: `runnerStatusStr` / `runnerSummary`
  (`tui/runners.go:339`). Move the counting into `cli` so the TUI strip and
  the overview share it.

### Board head

- Data: `Client.BoardTopics` (`cli/board.go:297`) gives name, `MsgCount`,
  `LastPublishedAtMs` for every topic. For the first K topics by
  `LastPublishedAtMs` only, `Client.BoardReadPage(ctx, topic, 0, 1)` fetches
  the newest message.
- Head = the payload's first line, cut to a fixed rune budget (a named
  constant, ~60), then passed through `cli.EscapeForTerminal`
  (`cli/payloadescape.go:54`). Computed once in Go; every surface prints it
  as given.
- A newest message that is withdrawn (`Retracted`) shows `(withdrawn)`, not
  its body.
- A topic whose read fails or returns not-found (it vanished between the two
  calls) shows its row without a head.

## Approaches

Considered for getting the board head (operator picked A, 2026-11-17):

- **A. Read the newest message of the top K topics** with the existing
  `BoardReadPage`. No wire change; the board read keeps its current cap
  gating. Costs K extra round trips per refresh, only while the overview is
  showing. **Picked.**
- **B. Add the newest message's head to the server's topic listing.** One
  round trip, every topic gets a head. Turns `board topics` from a listing of
  counts and times into a verb that returns message content, so its cap class
  changes; a wire change with the restart-order and size checks that brings.
  Rejected as too large for this.

Not considered further: computing the summary in the WebUI's JS (a second
implementation of the rules — the anti-goal above), and a server-side
`overview` RPC (a wire change that duplicates the snapshot the clients already
fetch).

## Architecture

- **Inputs come from four calls**, each already existing:
  - `Client.Snapshot` (`cli/list.go:84`) → runners and tasks only
    (`protocol.ListResultBody`, `runner/protocol/message.go:18740`);
  - `ConnListWith` (`cli/conns.go`) → conns, for the Hosts client count;
  - `PortForwardListWith` (`cli/port_forward_list.go:40`) → forwards;
  - `BoardTopics` + the K `BoardReadPage` head reads → Board.
- **One Go function** in `cli` builds the summary from those inputs and
  `now`. Pure and table-tested.
- **One fetch helper** on `*cli.Client` makes the four calls and calls the
  function. The CLI uses it as is.
- **Partial failure**: each section carries its own error. Conns, forwards
  and the board each have their own server-side cap check, so a caller whose
  caps do not cover one (an agent running `harness-cli overview`) gets every
  other section and that section naming the error.
- **wasm**: the bridge already fetches conns and forwards on every snapshot
  poll but converts them straight to JS (`cmd/harness-webui-wasm/main.go`,
  `harnessSnapshot`); keep them in Go beside `lastListing`
  (`main.go:1136`, tasks only today) so `harness.overview()` reads runners,
  tasks, conns and forwards from the last poll and fetches only the board
  part itself — the way `harness.orderTasks` reads `lastListing`
  (`main.go:1634`). `lastListing` holds tasks only, so runners need keeping
  too.
- **TUI**: tasks and runners come from its `SnapshotMsg`
  (`tui/client.go:496`), conns from the state it already holds
  (`tui/conns.go:29`). Forwards and the board are fetched by the overlay:
  once on open, then every 5 s while it is open (the WebUI's poll period),
  stopped on close. The TUI's only periodic tick, `actAgeTick`
  (`tui/app.go:473`), is cosmetic and makes no RPC; do not hang fetches on
  it.
- **Verb**: an `overview` entry in `cli/verb/table.go` with
  `CmdlineSurfaces: CLI | TUI | WebUI` and `ModalSurfaces` for the TUI overlay
  and the WebUI tab, like `grid` (`cli/verb/table.go:1999`). The CLI prints
  the sections as text; `--json` emits the structure.
- **WebUI**: a new tab button `概要` at the left end of `#tabbar`
  (`webui/index.html:34`), a `data-tabgroup="overview"` section, rendered
  from `harness.overview()` with `textContent` only. Recomputed on every
  snapshot poll; the board part is fetched only while the tab is active.
  Laid out for 390 px first (one column, no horizontal scroll); wider screens
  may use two columns.
- **WebUI remembers the last tab** (operator, 2026-11-17: a reload always
  returning to タスク was already a nuisance). `setActiveTab`
  (`webui/static/main.js`) stores the tab name in `localStorage`; page load
  restores it, falling back to タスク when the stored name is absent, unknown
  or storage throws. This applies to every tab, not only 概要. Per browser
  only; nothing server-side.
- **TUI overlay**: full-screen like `GridModel` (`tui/grid.go`), opened by
  a key and by the `overview` cmdline verb, closed by Esc. Rows are
  selectable; Enter does the drill-down in the table above.

## Constraints

- Agent-written strings (board payload, progress, title, prompt) are
  untrusted: the head goes through `EscapeForTerminal`; the TUI renders the
  other strings the way its task table already does; the WebUI uses
  `textContent`.
- The TUI requires ≥ 80×24 (`tui/app.go` `View`); the overlay must fit that.
- `surface-parity-checklist` applies (a new display surface and a new verb on
  all three UIs): walk it item by item.
- Verify per the repo's rules: `make` targets, the dummy harness for the live
  WebUI and TUI, Playwright screenshots at 390 and 1280 px (kept as
  deliverables), and the TUI in a dummy PTY.

## Decisions made by the designer (not by the operator)

- The section definitions in the table above, including what counts as "needs
  you" (= `stateGroup`, so Failed tasks are NOT in it; they are counted under
  finished).
- Hosts are grouped by runner hostname; connected clients appear only as a
  count, and only operator surfaces (cli / tui / webui) are counted.
- A section header tap never changes the saved task order or the TUI tree
  view.
- Two terminal-safety rules, each the one its source already uses elsewhere:
  task text (progress, title, prompt) through `vtgrid.SanitizeTitle`, board
  payload heads through `cli.EscapeForTerminal`.
- The TUI overlay refreshes forwards and the board every 5 s while open.
- The WebUI remembers the last tab (this one is the operator's, 2026-11-17).
- Forwards come from the snapshot; execs and await-idle watchers are not
  shown.
- The overview is a new tab / overlay, not a default landing (the remembered
  tab makes it the landing for whoever leaves it open).
- The head is computed and escaped in Go.

## Left to the builder

- The value of K per section and the head rune budget (fit one 390 px
  screen with all five sections populated).
- The TUI key (pick one that is free; the cmdline verb exists regardless).
- Exact visual styling, reusing the existing badge colours
  (`stateBadgeColor`) and the TUI's styles.
- JSON field names for `overview --json`.

## Amendment 2026-11-17 — as built

Decided while building (designer's calls, recorded in the plan's ledger):

- **K = 3** (`cli.OverviewRowCap`) for every section, hosts included; the head
  budget is 60 runes (`cli.OverviewHeadRunes`). The TUI key is `V`.
- **The TUI overlay fetches conns itself**, with forwards and the board, on
  open and every 5 s. The Architecture section above said "conns from the
  state it already holds"; that state is filled only once the conns view has
  been opened, so the overlay would have shown 0 clients until then.
- **The state badge** is `cli.TaskStateBadge`: the lifecycle badge, or the
  program's when the hooks never reported — the rule the TUI Life column
  already used, moved into `cli` and called from both.
- **A repo is named as the task list's `repo` order names it** (`repoLabels`:
  the last path element, or the full path when two repos share it).
- **A topic's age is a string computed in Go** (`OverviewTopic.Age`), so the
  WebUI prints it rather than re-deriving "2m ago".
- **Empty sections serialise as `[]`**, not `null`, in `overview --json` and
  for the WebUI.
- **The WebUI tab bar changed at phone width.** An eighth tab made the
  existing labels break inside a word (タ/ス/ク). At ≤ 600 px a label no longer
  breaks, the gaps and side padding are smaller (all eight fit at 390 px), and
  the bar scrolls sideways if they still do not fit. Wider screens are
  unchanged.
- **The `board_observe` description** (`cli/verb/caps.go`) names `overview`,
  whose board section reads topics and each one's newest message.
