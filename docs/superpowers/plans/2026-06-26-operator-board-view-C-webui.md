# Operator Board View — Plan C (WebUI) Implementation Plan

> Single cohesive deliverable (wasm exports + JS/HTML/CSS panel), one reviewed task. Spec: `docs/superpowers/specs/2026-06-26-operator-board-inbox-view-design.md`. Builds on Plan A's `cli.Client` board methods (landed; compile for GOOS=js — confirmed).

**Goal:** Add a WebUI "board" tab: list agentboard topics → click a topic → see its messages WITH content → purge buttons (whole topic + per-message). Dark theme (#1e1e1e/#d4d4d4), works at ≤390px.

**Architecture:** wasm (Go) exposes 3 JS-callable Promise functions on `window.harness` wrapping the `*cli.Client` board methods; plain JS/HTML/CSS renders the panel and calls them, mirroring the existing tasks panel. No new transport — same `currentClient()` + Promise pattern as `harness.cancel`/`harness.snapshot`.

## Global Constraints
- wasm exports mirror `harnessCancel` (cmd/harness-webui-wasm/main.go:477 — Promise wrapper, goroutine, `currentClient()`, resolve/reject) and `harnessSnapshot` (main.go:390 — building a `js.ValueOf` object tree for list returns).
- Board methods (already exist, cli/board.go, no `//go:build` exclusion): `c.BoardTopics(rootCtx) ([]cli.BoardTopicRow, error)`, `c.BoardRead(rootCtx, topic) ([]cli.BoardMessage, bool, error)`, `c.BoardPurge(rootCtx, topic, seq uint64) (purged int, found bool, err error)`. `cli.BoardMessage.Payload []byte` — decode with `string(payload)` for the UI (agentboard payloads are UTF-8 JSON). `seq==0` = whole topic.
- JS panel mirrors the tasks panel: tab button + `data-tabgroup` section (index.html:19-25 nav, sections by `data-tabgroup`), `setActiveTab` (main.js:1023), `renderTaskList` (main.js:1443) for list rows + click, the Cancel button pattern (main.js:1579) for mutating actions (confirm → `await window.harness.X()` → refresh).
- Dark palette tokens (style.css:1-14): bg #1e1e1e, fg #d4d4d4, input bg #2a2a2a, secondary surface #252526, accent #2d5. Mobile: the `@media (max-width:600px)` tab system (style.css:250-269) hides non-active `data-tabgroup` sections — add `"board"` to that selector group so the board tab works on mobile. Must be usable at 390px.
- Build: `make wasm-check` (GOOS=js compile check) for the Go; wasm hot-reloads (`make webui-build` + browser refresh — per memory, NO server restart needed to serve new wasm in this dev setup; verify via Playwright).
- Work in THIS worktree (…/.harness-worktrees/0f0d4dd6…), branch `harness/0f0d4dd6b7d3b64354cf4ff249b87403`. NEVER bare parent paths.

## Task: WebUI board panel (single reviewed task)

**Files:**
- Modify `cmd/harness-webui-wasm/main.go`: add 3 exports to the `window.harness` map (mirror `harnessCancel`):
  - `harnessBoardTopics(this, args)` → `currentClient()` → `c.BoardTopics(rootCtx)` → resolve a JS array of `{name, lastSeq, lastPublishedAtMs, msgCount}` (use `js.ValueOf([]any{...})` like harnessSnapshot builds its arrays).
  - `harnessBoardRead(this, args)` → topic=`args[0].String()` → `c.BoardRead(rootCtx, topic)` → resolve `{found: bool, msgs: [{seq, fromTask, fromHostname, receivedAtMs, payload: string(m.Payload)}]}`.
  - `harnessBoardPurge(this, args)` → topic=`args[0].String()`, seq=`uint64(args[1].Int())` → `c.BoardPurge(rootCtx, topic, seq)` → resolve `{purged, found}`.
  Register them under keys `"boardTopics"`, `"boardRead"`, `"boardPurge"` in the harness object literal (main.go:61-88 area).
- Modify `webui/index.html`: add `<button class="tab-btn" data-tab="board">Board</button>` to `<nav id="tabbar">`; add a `<section data-tabgroup="board">` to `<main>` containing a `<div id="board-topics">` (topic list) and a `<div id="board-detail" hidden>` (drill view: a back link, the topic name, a "purge topic" button, and a `<div id="board-messages">`).
- Modify `webui/static/main.js`:
  - `renderBoardTopics()`: `const topics = await window.harness.boardTopics()`; clear `#board-topics`; per topic make a clickable `<div class="board-topic-row">` showing name + `msgs=N` + last time (ms→`new Date(ms).toISOString()`); click → `openBoardTopic(name)`. Optionally annotate a `chat.<8hex>` topic with the matching task label from the last snapshot's task list (best-effort; raw name if no match).
  - `openBoardTopic(topic)`: show `#board-detail`, hide `#board-topics`; `const r = await window.harness.boardRead(topic)`; render each `r.msgs[]` as a `<div class="board-msg">` with a header (`#seq from=… host=… at=…`) and a `<pre>` of the payload (pretty-print if JSON.parse succeeds, else raw), plus a per-message "✕" purge button → confirm → `await window.harness.boardPurge(topic, seq)` → re-`openBoardTopic(topic)`. A "purge whole topic" button → confirm → `boardPurge(topic, 0)` → back to `renderBoardTopics()`.
  - Wire `board` into `setActiveTab` (load `renderBoardTopics()` when the board tab activates) and add a back handler from `#board-detail` to the topic list.
- Modify `webui/static/style.css`: add `"board"` to the mobile show/hide selector group (style.css:262-268); add styles for `.board-topic-row` (clickable, hover, palette), `.board-msg` (card on #252526), `.board-msg pre` (bg #111, wrap), the purge buttons (danger accent), and ensure the layout is single-column / readable at ≤390px.

**Steps:**
- [ ] Read mirror targets: main.go `harnessCancel`(477)+`harnessSnapshot`(390)+the harness object literal(61); main.js `renderTaskList`(1443)+`buildTaskSheet` cancel(1579)+`setActiveTab`(1023); index.html nav(19)+a section; style.css palette(1-14)+mobile(250-269).
- [ ] Add the 3 wasm exports; `make wasm-check` (GOOS=js build) green.
- [ ] Add the HTML tab + sections.
- [ ] Add the JS render/drill/purge logic.
- [ ] Add CSS (dark + ≤390px).
- [ ] `make webui-build` (rebuild main.wasm) + `go build ./...` + `make vet` green.
- [ ] Commit: `feat(webui): agentboard board panel (topics → messages w/ content → purge)`.

## Verification (controller, post-implement)
Playwright (per memory project_playwright_webui_visual_check): open the WebUI (URL from HARNESS_SERVER_CID), click the Board tab → topic list renders; click a topic → messages with content; click a purge button → topic/message removed. Verify at desktop AND 390px (`browser_resize`). Seed a board topic first via `harness-cli agent send`. Land via Mode A FF + `make build`.
