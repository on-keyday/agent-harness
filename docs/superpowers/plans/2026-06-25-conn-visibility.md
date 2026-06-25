# Connection Visibility Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Surface the server's live objproto connection set (count + per-conn detail + live connect/identify/disconnect events) across CLI, TUI, and WebUI, capability-gated like `ls`.

**Architecture:** Two data paths reusing existing machinery — (a) a streamed snapshot RPC mirroring `list`/`ListResultBody`, and (c) pubsub status events mirroring `tasks.status`/`runners.status`. Server joins `activeConns` × client-identity map × runner registry into `ConnInfo`. CLI/TUI consume both paths; WebUI consumes only the snapshot (poll + diff) and renders a radial hub-and-spoke SVG topology that degrades to a grouped list on mobile.

**Tech Stack:** Go (server/cli/tui), brgen `.bgn` schema → generated Go, Go→wasm + vanilla JS/DOM/SVG (WebUI), Playwright for WebUI visual verification.

**Spec:** `docs/superpowers/specs/2026-06-25-conn-visibility-design.md` — read its **Problem statement** AND its component sections before each task.

## Global Constraints

- **Work in the harness worktree** `/home/kforfk/workspace/remote-agent-harness/.harness-worktrees/0f0d4dd6b7d3b64354cf4ff249b87403/` (branch `harness/0f0d4dd6...`). Do **NOT** use parent-repo absolute paths `/home/kforfk/workspace/remote-agent-harness/<rel>` — they route to the parent checkout and silently diverge (Pitfall 8). Verify with `git rev-parse --abbrev-ref HEAD` before committing.
- **Read `.claude/skills/implementation-pitfalls/SKILL.md` in full before writing code** (every task).
- **Schema lives in ONE place**: all `.bgn` additions in Task 1 only. No "also add a field in Task N" (`feedback_no_split_schemas`, `feedback_no_schema_invisible_bytes`).
- **Verify with make targets, not ad-hoc `go build ./...`**: `make check`, `make wasm-check`, `make vet`, `make test` (`feedback_verify_with_make_targets_not_adhoc`). Compile-check individual packages with `go build -o /dev/null ./cmd/<x>` or `go vet ./<pkg>/...` — never bare `go build ./cmd/<x>/` (drops a binary in the worktree).
- **Regen command**: `make protoregen ARGS='runner/protocol/message.bgn'` (regenerates `runner/protocol/message.go`).
- **TUI/WebUI reuse the long-lived `*cli.Client`**: every new cli helper ships as both `X(ctx, serverCID)` (dial+close) and `XWith(ctx, c *cli.Client)` (reuse). TUI/WebUI call the `*With` variant against their existing client — never dial+close (`feedback_reuse_long_lived_client`, Pitfall 3).
- **Sibling-grep before extending a layer**: before adding to CLI dispatch / TUI views / server handlers / WebUI, grep how the nearest existing sibling (`list`/`watch`/`notify-watch`, the task-status TUI view, `harnessSnapshot`) does it and match that pattern (`feedback_check_existing_patterns_before_extend`, Pitfall 3).
- **WebUI**: dark `#1e1e1e`/`#d4d4d4` palette + `<=600px` layout from the first cut; verify desktop + 390px in Playwright (`feedback_webui_dark_theme_and_mobile`, `project_playwright_webui_visual_check`). WebUI/wasm changes hot-reload — build + browser refresh, no server restart (`feedback_webui_hot_reload_no_server_restart`).
- **`peer.Conn.Close()` ≠ `pc.Connection().Close()`** if you touch conn teardown — the former sends a wire Close (Pitfall 5). This plan only stamps/reads `streamingConn`, but do not alter close semantics.

---

### Task 1: Schema — all wire types (one place)

**Files:**
- Modify: `runner/protocol/message.bgn` (append-only to enums for ordinal stability)
- Regenerate: `runner/protocol/message.go` (via `make protoregen`)

**Interfaces (Produces — exact generated Go names later tasks rely on):**
- `protocol.ConnRole` (u8 enum) with `protocol.ConnRole_Unspecified|Cli|Tui|Webui|Agent|Runner`
- `protocol.ConnInfo{ Cid []u8/string accessor, Role, RemoteAddr, PrincipalTask protocol.TaskID, ConnectedAt uint64, Identified (u1) }`
- `protocol.ConnListQuery`, `protocol.ConnListResult{ StreamId uint64 }`, `protocol.ConnListResultBody{ Conns []ConnInfo }`
- `protocol.ConnStatusEvent{ Kind protocol.StatusEventKind, Ts uint64, Info protocol.ConnInfo }`
- `protocol.StatusEventKind_ConnOpened|ConnIdentified|ConnClosed`
- `protocol.TaskControlKind_ListConns`, with request union `list_conns:ConnListQuery` and response union `list_conns:ConnListResult`

- [ ] **Step 1:** Read the spec's "Schema" section verbatim. Read the existing `enum StatusEventKind`, `enum TaskControlKind`, `enum ClientKind`, `format ListResult`/`ListResultBody`, `format TaskStatusEvent` in `runner/protocol/message.bgn` to match style (length-prefix conventions, `u1`+`reserved` bitfields, the streamed-list pattern, the `TaskControlKind` request/response union match arms).

- [ ] **Step 2:** Add the new types from the spec's Schema block. Exactly:
  - new `enum ConnRole : u8` = `unspecified cli tui webui agent runner`
  - new `format ConnInfo` (cid len+bytes, role, remote_addr len+bytes, principal_task `:TaskID`, connected_at `:u64`, identified `:u1`, reserved `:u7`)
  - new `format ConnListQuery` (`reserved :u8`)
  - new `format ConnListResult` (`stream_id :u64`)
  - new `format ConnListResultBody` (`conns_len :u16`, `conns :[conns_len]ConnInfo`)
  - new `format ConnStatusEvent` (`kind :StatusEventKind`, `ts :u64`, `info :ConnInfo`)
  - **append** `list_conns` to `enum TaskControlKind` (after `permission_denied`)
  - **append** `conn_opened`, `conn_identified`, `conn_closed` to `enum StatusEventKind` (after `task_pruned`)
  - add match arms in the TaskControl request union (`TaskControlKind.list_conns => list_conns :ConnListQuery`) and response union (`TaskControlKind.list_conns => list_conns :ConnListResult`)

- [ ] **Step 3:** Regenerate. Run: `make protoregen ARGS='runner/protocol/message.bgn'`. Expected: `runner/protocol/message.go` updated, no error.

- [ ] **Step 4:** Compile-check generated code. Run: `go vet ./runner/protocol/...`. Expected: clean. Confirm the symbols above exist: `grep -nE "ConnRole_Runner|ConnListResultBody|ConnStatusEvent|StatusEventKind_ConnOpened|TaskControlKind_ListConns" runner/protocol/message.go` returns hits.

- [ ] **Step 5:** Commit. `git add runner/protocol/message.bgn runner/protocol/message.go && git commit -m "feat(proto): conn-visibility wire types (ConnInfo/ConnList*/ConnStatusEvent)"`

**Reviewer focus:** every byte added is in the schema (no Go-side struct invented outside `.bgn`); enum appends did not reorder existing ordinals; request/response unions both have the new arm.

---

### Task 2: Server — snapshot (`connectedSince` + `ConnList` join + `list_conns` handler + gating)

**Files:**
- Modify: `server/server.go` (`streamingConn` struct ~line 674; register site ~line 765; add `ConnList`; add the `list_conns` TaskControl case)
- Modify: `server/capabilities.go` (reuse the `info_global` + subtree filter that gates `ls`)
- Test: `server/conn_list_test.go` (new)

**Interfaces:**
- Consumes: Task 1 types.
- Produces: `func (s *Server) ConnList(viewerTaskID protocol.TaskID, hasInfoGlobal bool) []protocol.ConnInfo` (signature the handler + tests use). Server-side only.

- [ ] **Step 1:** Read the spec "Server" + "Capability gating" sections. **Sibling-grep**: read the existing `list` handler in `server/` (how it allocates a send-stream, encodes `ListResultBody`, returns `ListResult{stream_id}`) and how `ListResultBody` is currently subtree-filtered when the caller lacks `info_global` (grep `info_global`, `Capability_`, the existing list-visibility filter). Your `list_conns` handler MUST copy that exact send-stream + gating shape.

- [ ] **Step 2 (failing test):** In `server/conn_list_test.go`, write `TestConnList_JoinAndRoles`: construct a server with a faked `activeConns` containing (i) a cli conn recorded via the identity map, (ii) an agent conn with a principal task, (iii) a registered runner conn, (iv) a conn with no identity yet. Assert `ConnList(zeroTask, true)` returns 4 entries with roles `Cli, Agent, Runner, Unspecified`; the agent entry's `PrincipalTask` is set and `Identified==1`; the unidentified entry has `Identified==0` and zero principal. Add `TestConnList_SubtreeGating`: a viewer task with `hasInfoGlobal=false` sees only its own conn + a descendant agent conn, not unrelated conns.

- [ ] **Step 3:** Run: `go test ./server/ -run TestConnList -v`. Expected: FAIL (ConnList undefined).

- [ ] **Step 4 (implement):**
  - Add `connectedSince time.Time` to `streamingConn`; set it at the `activeConns[...] = wrapped` register site.
  - Implement `ConnList`: lock `activeConnsMu`, iterate `activeConns`; for each CID derive `remote_addr` from the CID's `netip.AddrPort`; look up the identity map (`task_handler.go` client-identity store) for `ClientKind`→`ConnRole` + principal task; check the runner registry (`GetByConnectionID`) → role `Runner`; if neither known → `Unspecified`, `Identified=0`. When `!hasInfoGlobal`, apply the same subtree filter `list` uses (only `viewerTaskID`'s own conn + descendant agent conns).
  - Add the `TaskControlKind.list_conns` case to the TaskControl dispatch: gate on caps exactly like `list`, call `ConnList`, stream-encode `ConnListResultBody`, return `ConnListResult{stream_id}`.

- [ ] **Step 5:** Run: `go test ./server/ -run TestConnList -v`. Expected: PASS. Then `go vet ./server/...`. Expected: clean.

- [ ] **Step 6:** Commit. `git add server/server.go server/capabilities.go server/conn_list_test.go && git commit -m "feat(server): ConnList snapshot + list_conns RPC (info_global-gated)"`

**Reviewer focus:** Problem-statement coverage — operator client conns AND unidentified/probe conns appear in the list. Gating reuses the `ls` subtree filter (no second rule). Handler matches the `list` send-stream shape.

---

### Task 3: Server — live events on `conns.status`

**Files:**
- Modify: `server/server.go` (add a `publishConnEvent` closure near `publishTaskEvent`/`publishRunnerEvent` ~lines 233-263; emit at register, at identity, at teardown)
- Modify: `server/task_handler.go` (emit `conn_identified` where `RecordClientIdentity` sets the kind) and the RunnerHello/registry-register path (emit `conn_identified` for runner conns alongside the existing `RunnerRegistered`)
- Test: `server/conn_event_test.go` (new)

**Interfaces:**
- Consumes: Task 1 types; Task 2's `ConnInfo`-building helper (refactor the per-CID `ConnInfo` builder out of `ConnList` into `func (s *Server) connInfoFor(cid) protocol.ConnInfo` so both the snapshot and events use it — DRY).
- Produces: events on topic `conns.status` (and the build helper above).

- [ ] **Step 1:** Read the spec "Server" event section. **Sibling-grep**: read `publishTaskEvent`/`publishRunnerEvent` (server.go:233-263) and the `tasks.status`/`runners.status` topic strings + how they encode+publish. Copy that shape for `publishConnEvent` → `conns.status`.

- [ ] **Step 2 (failing test):** In `server/conn_event_test.go`, write `TestConnEvents_OpenIdentifyClose`: subscribe a fake subscriber to `conns.status`; register a conn → assert a `conn_opened` event with `Identified==0`; record its client identity → assert `conn_identified` with the right `ConnRole`; tear it down → assert `conn_closed`. All three carry the same `Cid` (correlation key).

- [ ] **Step 3:** Run: `go test ./server/ -run TestConnEvents -v`. Expected: FAIL.

- [ ] **Step 4 (implement):**
  - Refactor the per-CID `ConnInfo` builder into `connInfoFor` (used by Task 2's `ConnList` too).
  - Add `publishConnEvent(kind, ConnInfo)` publishing to `conns.status`.
  - Emit `conn_opened` immediately after the `activeConns` register (role `Unspecified`, `Identified=0`).
  - Emit `conn_identified` (a) where `RecordClientIdentity` sets `ClientKind`, (b) at runner register.
  - Emit `conn_closed` in the existing teardown `defer` (before/at the `delete`).
  - Gate the `conns.status` subscription to subtree-only for subscribers lacking `info_global` (mirror how task/runner status subscription visibility is gated, if any; otherwise filter in the publish-fanout the same way `ConnList` filters).

- [ ] **Step 5:** Run: `go test ./server/ -run TestConnEvents -v`. Expected: PASS. `go vet ./server/...`. Expected: clean.

- [ ] **Step 6:** Commit. `git add server/server.go server/task_handler.go server/conn_event_test.go && git commit -m "feat(server): conn_opened/identified/closed events on conns.status"`

**Reviewer focus:** `conn_opened` truly fires at raw register (probe visibility — the spec's decision #1), not after auth. `connInfoFor` is shared (no duplicated join logic). Same `Cid` across the 3 events.

---

### Task 4: CLI — `conns` snapshot subcommand

**Files:**
- Create: `cli/conns.go`
- Modify: the `harness-cli` subcommand dispatch (grep for where `list`/`ls` is registered, e.g. `cmd/harness-cli/main.go`)
- Test: `cli/conns_test.go` (new) — table-format + JSON-line formatters

**Interfaces:**
- Consumes: Task 1 + Task 2 (`TaskControlKind_ListConns`, `ConnListResultBody`).
- Produces: `func (c *Client) ConnListWith(ctx) ([]protocol.ConnInfo, error)` and package-level `func ConnList(ctx, serverCID) ([]protocol.ConnInfo, error)`; line formatters `connInfoTextLine(*protocol.ConnInfo) string`, `connInfoJSONLine(*protocol.ConnInfo) string`.

- [ ] **Step 1:** Read spec "CLI". **Sibling-grep**: read `cli/list.go` (`Snapshot`/`List` — the `TaskControlKind_List` round-trip + read-stream-until-EOF + decode) and copy it for `list_conns`. Read `cli/notify_watch.go` for the line-formatter convention.

- [ ] **Step 2 (failing test):** In `cli/conns_test.go`, write `TestConnInfoTextLine` and `TestConnInfoJSONLine`: given a `ConnInfo{Role:Agent, RemoteAddr:"203.0.113.5:5",  PrincipalTask:..., ConnectedAt:..., Identified:1}` assert the text line contains the addr, `agent`, a short principal id, an age; the unidentified case shows an `unident` marker; the JSON line is valid JSON with those fields.

- [ ] **Step 3:** Run: `go test ./cli/ -run TestConnInfo -v`. Expected: FAIL.

- [ ] **Step 4 (implement):** Implement `ConnListWith`/`ConnList` (mirror `cli/list.go` `Snapshot`), the two formatters, and a `conns` subcommand: `harness-cli conns` prints the table; `--json` prints JSON lines. Register it in the subcommand dispatch next to `ls`.

- [ ] **Step 5:** Run: `go test ./cli/ -run TestConnInfo -v`. Expected: PASS. `go build -o /dev/null ./cmd/harness-cli`. Expected: clean (no binary dropped).

- [ ] **Step 6:** Commit. `git add cli/conns.go cli/conns_test.go cmd/harness-cli/ && git commit -m "feat(cli): harness-cli conns snapshot subcommand"`

**Reviewer focus:** uses `ConnListWith(c)` internally (not a fresh dial in the helper body); table + JSON both covered; registered in dispatch.

---

### Task 5: CLI — `conns -f` live follow

**Files:**
- Modify: `cli/conns.go` (add watch); the subcommand (add `-f`/`--follow` flag)
- Test: covered by reuse of Task 4 formatters (the event carries `ConnInfo`); add `TestConnStatusEventLine` if a distinct event-line formatter is added.

**Interfaces:**
- Consumes: Task 3 (`conns.status`, `ConnStatusEvent`).
- Produces: `func (c *Client) WatchConnsWith(ctx, out io.Writer) error` + `func WatchConns(ctx, serverCID, out) error`.

- [ ] **Step 1:** **Sibling-grep**: read `cli/notify_watch.go` `watchNotifications` (`c.Peer().JoinAndGetStream(ctx, label, topic)` loop). Copy it for topic `conns.status`, decoding `ConnStatusEvent` and printing `kind` + `connInfoTextLine(&ev.Info)` (or JSON).

- [ ] **Step 2 (failing test):** `TestConnStatusEventLine`: a `conn_opened` event for an unidentified conn formats to a line containing `opened` + `unident`; a `conn_closed` contains `closed`.

- [ ] **Step 3:** Run: `go test ./cli/ -run TestConnStatusEventLine -v`. Expected: FAIL.

- [ ] **Step 4 (implement):** Implement `WatchConnsWith`/`WatchConns` (mirror `watchNotifications`) and wire `conns -f` to call it; `conns -f --json` uses JSON lines.

- [ ] **Step 5:** Run: `go test ./cli/ -run TestConnStatusEventLine -v`. Expected: PASS. `go build -o /dev/null ./cmd/harness-cli`. Expected: clean.

- [ ] **Step 6:** Commit. `git add cli/conns.go cmd/harness-cli/ && git commit -m "feat(cli): harness-cli conns -f live follow (conns.status)"`

---

### Task 6: TUI — connections view

**Files:**
- Modify: `tui/` (new view + key binding; grep the existing task/runner status view to find the files)
- Test: a render-snapshot unit test if the existing TUI views have them; otherwise a `ConnList`-mapping unit test.

**Interfaces:**
- Consumes: Task 4 (`ConnListWith`) + Task 5 (`WatchConnsWith`) against `a.client`.

- [ ] **Step 1:** **Sibling-grep (mandatory, Pitfall 3):** find the existing TUI view that shows task/runner status and how its `Do*`/update path threads `a.client` and consumes the `tasks.status`/`runners.status` watch. The new connections view MUST follow that exact pattern: `ConnListWith(a.client)` for the snapshot and `WatchConnsWith(a.client)` for live updates — NOT `cli.ConnList(serverCID)` (dial+close).

- [ ] **Step 2:** Add a connections view: a key binding to open it, initial population via `ConnListWith(a.client)`, live row add/update/remove driven by `WatchConnsWith`. Columns mirror the CLI: remote-addr, role, principal (short), age, `unident` marker. Match the existing view's bubbletea message/update idiom.

- [ ] **Step 3:** Run: `go vet ./tui/...` and any existing `go test ./tui/...`. Expected: clean/pass. Build-check: `go build -o /dev/null ./cmd/harness-tui` (adjust to the actual TUI cmd path). Expected: clean.

- [ ] **Step 4:** Commit. `git add tui/ && git commit -m "feat(tui): connections view (live conns.status)"`

**Reviewer focus:** `*With(a.client)` used, not dial+close (Pitfall 3); update idiom matches the sibling status view.

---

### Task 7: WebUI — radial topology + mobile list + Playwright verify

**Files:**
- Modify: `cmd/harness-webui-wasm/main.go` (extend `harnessSnapshot` to also call `list_conns` and merge `{conns:[...]}`; or add a `connsSnapshot` export called from the same poll)
- Modify: `webui/static/main.js` (`refreshSnapshot` merge + new `renderConnTopology(snap.conns)` desktop SVG + `renderConnList` mobile fallback + diff-based in/out animation)
- Modify: `webui/static/style.css` (topology + mobile-list styles, dark palette)
- Verify: Playwright (desktop + 390px)

**Interfaces:**
- Consumes: Task 2 (`list_conns` snapshot) via `ConnListWith` from wasm.

- [ ] **Step 1:** **Sibling-grep:** read `cmd/harness-webui-wasm/main.go` `harnessSnapshot` (how it calls `c.Snapshot` and reshapes to JS) and `webui/static/main.js` `refreshSnapshot` (the `setInterval` poll + `renderRunners`/`renderTaskList` fan-out). Extend, don't replace. wasm uses the existing long-lived client (`ConnListWith`), not a fresh dial.

- [ ] **Step 2:** wasm: in the snapshot reshaper, also fetch `ConnListWith` and add `conns:[{cid,role,remoteAddr,principalTask,connectedAt,identified}]` to the returned JS object. Build-check: `make wasm-check`. Expected: clean.

- [ ] **Step 3:** JS desktop render — `renderConnTopology(conns)`: a `<svg>` with a center server node; group conns by the IP portion of `remoteAddr`; one cluster node per IP positioned radially; each conn a leaf node on a spoke; color by `role`, shade by age; `unident` conns visually distinct. Drive in/out animation by diffing the previous vs current conn set (keyed by `cid`).

- [ ] **Step 4:** JS mobile (`<=600px`, CSS media query): `renderConnList(conns)` — one card per IP listing its conns with a connector line to a server node. Toggle topology/list by viewport width. Dark `#1e1e1e`/`#d4d4d4` palette throughout.

- [ ] **Step 5:** Wire both into `refreshSnapshot` (rides the existing ~5s poll; no new timer, no `conns.status` subscription in wasm — spec decision #3).

- [ ] **Step 6 (verify — no server restart needed, wasm hot-reloads):** Build wasm (`make wasm-check` then the webui build via `make webui-build`), refresh the browser. With Playwright (`project_playwright_webui_visual_check`): navigate to the WebUI conns view; screenshot at desktop and at 390px; assert dark theme; open a new connection (e.g. resume a bash-runner task or run `harness-cli conns` from another session to create a conn) and assert a node/row animates in, then disconnect and assert it animates out. Save screenshots for the review.

- [ ] **Step 7:** Commit. `git add cmd/harness-webui-wasm/ webui/ && git commit -m "feat(webui): radial connection topology + mobile list (poll-diff live)"`

**Reviewer focus:** dark theme + 390px from the first cut (not retrofitted); `ConnListWith` reuse in wasm; diff-based animation keyed by `cid`; Playwright evidence attached.

---

## Self-Review (controller, against the spec)

- **Spec coverage:** (a) snapshot → Tasks 2,4,6,7; (c) events → Tasks 3,5,6; CLI → 4,5; TUI → 6; WebUI → 7; gating → 2,3; schema-one-place → 1; failed-handshake visibility → 2 (list shows `Identified=0`) + 3 (`conn_opened` at register). All spec sections map to a task.
- **Type consistency:** `ConnInfo`/`ConnRole`/`ConnListResultBody`/`ConnStatusEvent`/`TaskControlKind_ListConns` defined in Task 1, consumed by the exact same names downstream. `ConnList(viewerTaskID, hasInfoGlobal)` and `connInfoFor(cid)` (Task 2/3), `ConnListWith`/`WatchConnsWith` (Task 4/5) referenced consistently in 6/7.
- **No placeholders:** every task has exact paths, regen/build/test commands, test oracles, and sibling-grep targets.
- **Decisions:** all from the spec's "Decisions taken" — none punted to the implementer.
