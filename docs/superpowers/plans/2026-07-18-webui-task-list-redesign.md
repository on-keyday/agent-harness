# WebUI Task List Redesign Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the WebUI Tasks tab scannable at high task counts: status filter chips (default Active), text filter, last-activity-descending sort, and two-line card rows that fit a 390px phone.

**Architecture:** Pure presentation-layer change. `renderTaskList` in `webui/static/main.js` gains a sort+filter pre-pass fed by a static filter bar in `index.html`; rows become structured two/three-line card DOM. One wasm-bridge mapping line exposes the already-on-the-wire `ErrorMessage`. No server or protocol changes.

**Tech Stack:** Vanilla JS (no framework, no build step for JS/CSS/HTML), Go wasm bridge (`cmd/harness-webui-wasm`), Playwright MCP for verification.

**Spec:** `docs/superpowers/specs/2026-07-18-webui-task-list-redesign-design.md` — read its Problem section before starting; the diff must address all four numbered pain points.

## Global Constraints

- **Work in THIS harness worktree**: `/home/kforfk/workspace/remote-agent-harness/.harness-worktrees/5b65231c141da46b2239912efd108196/`. All tool calls use absolute paths under this directory. Verify `git rev-parse --abbrev-ref HEAD` prints `harness/5b65231c141da46b2239912efd108196` before the first edit. Paths under `/home/kforfk/workspace/remote-agent-harness/<rel>` (without `.harness-worktrees/...`) route to the PARENT checkout — never use them.
- **Dark theme only**: palette is `#1e1e1e`/`#111` backgrounds, `#d4d4d4` text, `#888` muted, `#2d5` green accent (active state = `background:#2d5; color:#000`, see `.tab-btn.is-active`). No UA-default light styles.
- **Mobile from the first cut**: no horizontal page scroll at 390px width.
- **Build hygiene**: compile-check Go with `make wasm-check` (writes no artifact). `make webui-build` writes only the gitignored `webui/static/main.wasm` + `wasm_exec.js`. Never bare `go build ./cmd/<x>/` (drops a binary into the worktree root).
- **`TERMINAL_STATES`** (`main.js:383`) = `Succeeded | Failed | Cancelled` is the single source of the Active/Finished partition. Do not duplicate the set.
- Existing behavior that must survive unchanged: `buildTaskSheet` contents, single-open-sheet toggling, `renderFileTaskSelect`, snapshot poll cadence.

---

### Task 1: Expose `errorMsg` through the wasm bridge

**Files:**
- Modify: `cmd/harness-webui-wasm/main.go` (~line 433 doc comment, ~line 499 task map)

**Interfaces:**
- Consumes: `protocol.TaskInfo.ErrorMessage` (`[]byte` field, already populated by `server/task_handler.go` `toTaskInfo` via `SetErrorMessage`; see `tui/detail.go:113` for an existing consumer).
- Produces: JS task objects from `harness.snapshot()` gain `errorMsg: string` (empty string when no error). Task 3 renders it.

- [ ] **Step 1: Add the mapping**

In `harnessSnapshot`'s task map (directly under the `"agentProfile"` entry at ~line 499), add:

```go
					// Terminal-failure reason (e.g. "runner_disconnected"); empty
					// for non-failed tasks. Rendered in red on the task card.
					"errorMsg": string(t.ErrorMessage),
```

- [ ] **Step 2: Update the doc comment**

The `harness.snapshot()` contract comment at ~line 433 lists task fields. Extend the list:

```go
//	  tasks:   [{id, status, kind, repoPath, prompt, assignedTo, exitCode,
//	             createdAt, startedAt, endedAt, agentProfile, errorMsg}],
```

- [ ] **Step 3: Compile-check**

Run: `make wasm-check`
Expected: exits 0, no output files created (`git status --short` shows only `main.go` modified).

- [ ] **Step 4: Commit**

```bash
git add cmd/harness-webui-wasm/main.go
git commit -m "feat(wasm): map TaskInfo.ErrorMessage to snapshot task errorMsg"
```

---

### Task 2: Filter bar — status chips, text filter, activity sort

**Files:**
- Modify: `webui/index.html` (Tasks section, ~line 41)
- Modify: `webui/static/main.js` (element refs near line 319; `renderTaskList` at line 2200; helpers near `activityBadge` at line 2194)
- Modify: `webui/static/style.css` (after the `.task-*` block ending ~line 292)

**Interfaces:**
- Consumes: task objects from `harness.snapshot()` (`status`, `id`, `repoPath`, `agentProfile`, `prompt`, `outputIdleMs`, `createdAt`/`startedAt`/`endedAt` in unix **nanoseconds**), `TERMINAL_STATES` (`main.js:383`).
- Produces: module-level `let lastTasks = []` (latest snapshot task array; Task 3 does not use it but later code may), functions `taskActivityMs(t) -> number(ms)` and `taskMatchesFilter(t, needle) -> bool`, DOM ids `#task-filter-input`, `#task-chip-active`, `#task-chip-finished`, `#task-chip-all`. `renderTaskList(tasks)` keeps its signature.

- [ ] **Step 1: Add the filter bar markup**

In `webui/index.html`, inside the Tasks section (between `<h2>Tasks</h2>` and `<div id="task-list" ...>`), add — deliberately OUTSIDE `#task-list` so the 5s poll re-render never rebuilds it (input focus and chip state survive):

```html
      <div class="task-filter-bar">
        <button type="button" id="task-chip-active" class="task-chip is-active">Active</button>
        <button type="button" id="task-chip-finished" class="task-chip">Finished</button>
        <button type="button" id="task-chip-all" class="task-chip">All</button>
        <input id="task-filter-input" type="search" placeholder="filter: repo / id / status">
      </div>
```

- [ ] **Step 2: Wire state + handlers in main.js**

Next to the existing element refs (`const taskList = document.getElementById("task-list");`, line ~319), add:

```js
  // Task-list filter bar. Lives OUTSIDE #task-list so the snapshot poll's
  // re-render never steals input focus or resets chip selection.
  const taskFilterInput = document.getElementById("task-filter-input");
  const taskChips = {
    active:   document.getElementById("task-chip-active"),
    finished: document.getElementById("task-chip-finished"),
    all:      document.getElementById("task-chip-all"),
  };
  let taskStatusFilter = "active"; // "active" | "finished" | "all"
  let lastTasks = [];              // latest snapshot; re-render source for filter events
  for (const [key, btn] of Object.entries(taskChips)) {
    btn.addEventListener("click", () => {
      taskStatusFilter = key;
      for (const b of Object.values(taskChips)) b.classList.toggle("is-active", b === btn);
      renderTaskList(lastTasks);
    });
  }
  taskFilterInput.addEventListener("input", () => renderTaskList(lastTasks));
```

(`renderTaskList` is a hoisted function declaration, so calling it from handlers registered earlier in the file is safe — same pattern the file already relies on, see comment at line 2190.)

- [ ] **Step 3: Add the helpers**

Next to `activityBadge` (line ~2194), add:

```js
  // taskActivityMs returns the task's last-activity time in unix-ms, for
  // most-recently-active-first sorting. Live sessions (outputIdleMs >= 0)
  // derive from the server-computed idle age; finished / never-started tasks
  // fall back to wire timestamps, which are unix NANOseconds (toTaskInfo uses
  // UnixNano) — divide by 1e6. Zero wire values mean "unset" and lose to any
  // set value inside max().
  function taskActivityMs(t) {
    if (t.outputIdleMs >= 0) return Date.now() - t.outputIdleMs;
    return Math.max(t.endedAt || 0, t.startedAt || 0, t.createdAt || 0) / 1e6;
  }

  // taskMatchesFilter applies the status chip + lowercased needle. Search
  // keys: id, repoPath, status, agentProfile, prompt (prompt is usually
  // empty on interactive tasks — repo and id are the effective keys).
  function taskMatchesFilter(t, needle) {
    if (taskStatusFilter === "active" && TERMINAL_STATES.has(t.status)) return false;
    if (taskStatusFilter === "finished" && !TERMINAL_STATES.has(t.status)) return false;
    if (!needle) return true;
    return [t.id, t.repoPath, t.status, t.agentProfile, t.prompt]
      .some((v) => (v || "").toLowerCase().includes(needle));
  }
```

- [ ] **Step 4: Sort + filter inside renderTaskList**

Replace the head of `renderTaskList` (currently lines 2200-2208):

```js
  function renderTaskList(tasks) {
    lastTasks = tasks || [];
    const finished = lastTasks.filter((t) => TERMINAL_STATES.has(t.status)).length;
    taskChips.active.textContent   = `Active (${lastTasks.length - finished})`;
    taskChips.finished.textContent = `Finished (${finished})`;
    taskChips.all.textContent      = `All (${lastTasks.length})`;
    const needle = taskFilterInput.value.trim().toLowerCase();
    const visible = lastTasks
      .filter((t) => taskMatchesFilter(t, needle))
      .sort((a, b) => taskActivityMs(b) - taskActivityMs(a));
    taskList.innerHTML = "";
    if (visible.length === 0) {
      const empty = document.createElement("div");
      empty.className = "task-empty";
      empty.textContent = lastTasks.length === 0 ? "(none)" : "(no matching tasks)";
      taskList.appendChild(empty);
      return;
    }
    for (const t of visible) {
```

The body of the `for` loop (row/sheet construction) stays as-is in this task; only the iterated variable source changes from `tasks` to `visible`.

- [ ] **Step 5: Chip + input CSS**

Append to the task-list block in `webui/static/style.css` (after `.task-agent-select`, ~line 288):

```css
/* Task-list filter bar (chips + text filter). Selected chip uses the same
   green-accent active treatment as .tab-btn.is-active. */
.task-filter-bar { display:flex; flex-wrap:wrap; gap:0.4rem; align-items:center; margin-bottom:0.4rem; }
.task-chip { background:#2a2a2a; color:#ccc; border:1px solid #555; border-radius:12px; padding:0.3rem 0.7rem; font-size:0.85rem; cursor:pointer; }
.task-chip.is-active { background:#2d5; color:#000; border-color:#2d5; font-weight:bold; }
#task-filter-input { flex:1 1 8rem; min-width:6rem; background:#111; color:#d4d4d4; border:1px solid #555; border-radius:4px; padding:0.35rem 0.5rem; font-size:0.85rem; }
```

- [ ] **Step 6: Sanity-check in a browser**

```bash
make build   # webui-build (wasm w/ Task 1's errorMsg) + ./bin/* binaries
mkdir -p "$SCRATCH/devstack"
./bin/harness-server --listen 127.0.0.1:8599 --webui-dir webui --data-dir "$SCRATCH/devstack/data"   # run_in_background
```

(`$SCRATCH` = the session scratchpad directory. `--webui-dir webui` serves JS/CSS/HTML/wasm from the worktree disk, so later JS-only edits need just a browser refresh — no rebuild, no restart.)

Open `http://127.0.0.1:8599/` via Playwright: the filter bar renders, chips show `(0)` counts, task list shows `(none)`. No JS console errors (`browser_console_messages`).

- [ ] **Step 7: Commit**

```bash
git add webui/index.html webui/static/main.js webui/static/style.css
git commit -m "feat(webui): task-list filter chips + text filter + activity-desc sort"
```

---

### Task 3: Two-line card rows

**Files:**
- Modify: `webui/static/main.js` (`renderTaskList` for-loop body, currently lines 2209-2238)
- Modify: `webui/static/style.css` (`.task-row` block, ~line 274, + new card styles)

**Interfaces:**
- Consumes: task objects incl. `errorMsg` (Task 1), `activityBadge(idleMs)` (`main.js:2194`, unchanged), `buildTaskSheet(sheet, t)` (unchanged).
- Produces: card DOM inside `.task-row` — classes `.task-row-line1`, `.task-status-dot`, `.task-status-label`, `.task-repo`, `.task-act`, `.task-agent`, `.task-row-meta`, `.task-err`, `.task-prompt`. Helper `repoTail(p) -> string`.

- [ ] **Step 1: Add repoTail helper + status palette**

Next to `taskActivityMs`:

```js
  // repoTail returns the last path segment for display ("/a/b/repo" -> "repo",
  // "C:/x/y" -> "y"); full path stays available via the row's title attr.
  function repoTail(p) {
    const parts = (p || "").split(/[\\/]/).filter(Boolean);
    return parts.length ? parts[parts.length - 1] : (p || "-");
  }

  // Status dot/label colors. Terminal states are muted so live rows pop;
  // unknown (Pending/Assigned/...) falls back to blue.
  const TASK_STATUS_COLORS = {
    Running: "#2d5", Detached: "#e5c07b",
    Failed: "#f14c4c", Succeeded: "#888", Cancelled: "#888",
  };
```

- [ ] **Step 2: Replace the row construction**

Inside the `for (const t of visible) {` loop, replace everything from `const row = document.createElement("div");` through `row.title = t.id;` (the old one-line `row.textContent` build, lines ~2211-2224) with:

```js
      const row = document.createElement("div");
      row.className = "task-row";
      row.title = `${t.id}\n${t.repoPath}`; // full id + path on hover; sheet has Copy id

      const line1 = document.createElement("div");
      line1.className = "task-row-line1";
      const dot = document.createElement("span");
      dot.className = "task-status-dot";
      dot.style.background = TASK_STATUS_COLORS[t.status] || "#61afef";
      const statusEl = document.createElement("span");
      statusEl.className = "task-status-label";
      statusEl.style.color = TASK_STATUS_COLORS[t.status] || "#61afef";
      statusEl.textContent = t.status;
      const repoEl = document.createElement("span");
      repoEl.className = "task-repo";
      repoEl.textContent = repoTail(t.repoPath);
      line1.append(dot, statusEl, repoEl);
      if (t.outputIdleMs >= 0) {
        const act = document.createElement("span");
        act.className = "task-act";
        act.textContent = activityBadge(t.outputIdleMs);
        line1.appendChild(act);
      }
      if (t.agentProfile) {
        const ag = document.createElement("span");
        ag.className = "task-agent";
        ag.textContent = t.agentProfile;
        line1.appendChild(ag);
      }

      const meta = document.createElement("div");
      meta.className = "task-row-meta";
      let metaText = `${t.id.slice(0, 12)}…  ${t.kind}  from=${t.origin || "-"}`;
      if (t.createdBy) metaText += `  by=${t.createdBy}`;
      if (t.resumedBy) metaText += `  resumed_by=${t.resumedBy}`;
      if (t.caps) metaText += `  caps=${t.caps}`;
      meta.textContent = metaText;
      if (t.errorMsg) {
        const err = document.createElement("span");
        err.className = "task-err";
        err.textContent = `  err=${t.errorMsg}`;
        meta.appendChild(err);
      }

      row.append(line1, meta);
      if (t.prompt) {
        const promptEl = document.createElement("div");
        promptEl.className = "task-prompt";
        promptEl.textContent = t.prompt;
        row.appendChild(promptEl);
      }
```

Everything after (sheet creation, click handler, `wrap.appendChild`) stays byte-identical. Note the old `promptShort`/`attr` locals disappear — nothing else referenced them.

- [ ] **Step 3: Card CSS**

In `webui/static/style.css`, the `.task-row` rule (~line 274) keeps monospace/padding/cursor but drops `white-space:pre-wrap` (the card children control their own wrapping; meta uses default whitespace collapsing — its double spaces become single, which is fine). Replace the rule and add card styles:

```css
.task-row { font-family: monospace; padding:0.45rem 0.5rem; border-radius:3px; cursor:pointer; word-break:break-all; }
.task-row-line1 { display:flex; align-items:center; gap:0.5rem; flex-wrap:wrap; }
.task-status-dot { width:0.6rem; height:0.6rem; border-radius:50%; flex:0 0 auto; }
.task-status-label { font-size:0.85rem; }
.task-repo { font-weight:bold; color:#d4d4d4; }
.task-act { font-size:0.75rem; color:#aaa; border:1px solid #444; border-radius:8px; padding:0 0.4rem; }
.task-agent { font-size:0.75rem; color:#9cdcfe; }
.task-row-meta { font-size:0.75rem; color:#888; margin-top:0.15rem; }
.task-err { color:#f14c4c; }
.task-prompt { font-size:0.8rem; color:#aaa; margin-top:0.15rem; overflow:hidden; display:-webkit-box; -webkit-line-clamp:2; -webkit-box-orient:vertical; }
```

- [ ] **Step 4: Browser sanity check**

With the Task 2 dev server still up: refresh `http://127.0.0.1:8599/` in Playwright, confirm the empty list still renders `(none)`, no console errors. (Real-data rendering is Task 4.)

- [ ] **Step 5: Commit**

```bash
git add webui/static/main.js webui/static/style.css
git commit -m "feat(webui): two-line task cards — status color, repo tail, muted meta, error text"
```

---

### Task 4: E2E verification (local stack + Playwright)

**Files:** none created in-repo (throwaway stack lives in the scratchpad; fixes discovered here are committed to the files above).

**Interfaces:**
- Consumes: `./bin/harness-server`, `./bin/agent-runner`, `./bin/harness-cli` (from `make build` in the worktree), Playwright MCP tools, dev server from Task 2 Step 6.

**Why a local stack:** the deployed server runs on another host and serves ITS OWN assets — pointing Playwright at it would test old code. A loopback server+runner pair with `--webui-dir` serves this worktree's files and generates real task data. This throwaway runner deliberately does NOT go through `scripts/runner.sh` and must NOT touch `bin/.run/` — it belongs to no fleet slot, connects only to the loopback server, and is killed at the end (the runner.sh rule in memory governs production fleet slots).

- [ ] **Step 1: Seed a scratch repo + runner + task variety**

```bash
mkdir -p "$SCRATCH/devstack/repo" && cd "$SCRATCH/devstack/repo" && git init -q && git commit -q --allow-empty -m init && cd -
./bin/agent-runner --server-cid 'ws:127.0.0.1:8599-*' --roots "$SCRATCH/devstack/repo" \
  --agent-bin bash --agent-oneshot-argv '{args} -c {prompt}' --max-tasks 4   # run_in_background
CID='ws:127.0.0.1:8599-*'
./bin/harness-cli --server-cid "$CID" submit --repo "$SCRATCH/devstack/repo" --task 'echo done'   # -> Succeeded
./bin/harness-cli --server-cid "$CID" submit --repo "$SCRATCH/devstack/repo" --task 'exit 1'      # -> Failed
./bin/harness-cli --server-cid "$CID" submit --repo "$SCRATCH/devstack/repo" --task 'sleep 600'   # -> Running
./bin/harness-cli --server-cid "$CID" ls    # confirm the three statuses before driving the browser
```

- [ ] **Step 2: Desktop checks (Playwright at default viewport)**

Navigate to `http://127.0.0.1:8599/`, then via `browser_snapshot` / `browser_evaluate` assert:

1. Default chip is `Active` and only the `sleep 600` task (Running) is listed; counts read `Active (1)`, `Finished (2)`, `All (3)`.
2. Click `All`: three rows; the Running row is FIRST (live activity outranks the finished ones); each row shows colored status label, repo tail `repo`, meta line with 12-hex id prefix, and the prompt text line.
3. The `exit 1` row shows `err=` text in red (`.task-err` present) — this proves Task 1's `errorMsg` mapping end-to-end.
4. Type `sleep` in the filter box: only the sleep task remains, without waiting for the next poll; type `zzz`: `(no matching tasks)`; clear it.
5. Click a row: action sheet opens (Copy id visible); click another row: first sheet closes (single-open preserved).
6. From the `echo done` row's sheet, click `📁 ファイル`: the Files tab opens with that task selected (sheet→Files navigation intact).
7. Cancel the sleep task from its sheet (`browser_handle_dialog` accept). After the next poll it leaves `Active` and appears under `Finished` as Cancelled.
8. From the Cancelled task's sheet, click `▶ Resume any runner`: a session opens (terminal tab activates, `attached:` label set) — resume-from-sheet intact. Detach/close it after.

- [ ] **Step 3: Mobile checks (390px)**

`browser_resize` to 390x844, re-run checks 1-2 visually, and assert no horizontal page scroll:

```js
// browser_evaluate
() => document.documentElement.scrollWidth <= window.innerWidth
```

Expected: `true`. Screenshot both widths for the user.

- [ ] **Step 4: Interactive input regression**

From Compose, Open an interactive session on the scratch repo (agent bin is `bash`). In the terminal tab type `echo hi<Enter>` via `browser_type`/`browser_press_key` and assert `hi` appears in the xterm DOM (input round-trip, per the verify-interactive-INPUT rule). Detach afterwards.

- [ ] **Step 5: Teardown + commit any fixes**

Kill the background server and runner processes (they were started via run_in_background; stop their task ids), `rm -rf "$SCRATCH/devstack"`. Run `make check` (compile-level full check). If steps 2-4 forced code changes, commit them:

```bash
git add -u && git commit -m "fix(webui): task-list verification fixes"
```

---

## Landing

After all tasks pass: land via the `landing-to-main` skill (Mode A FF-push, rebase-first), then `make build` in the main checkout per `feedback_build_after_landing`. The deployed server picks the change up on its own host per its usual asset flow — WebUI/wasm changes do not need a server restart (`feedback_webui_hot_reload_no_server_restart`); coordinate the live-host refresh with the user.
