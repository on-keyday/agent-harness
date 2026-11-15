# Task list ordering modes Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Switchable task-list ordering/grouping (created / activity / live / state / repo) in the TUI, the WebUI and `ls`.

**Architecture:** One pure Go function `cli.OrderTasks` decides order and groups. `ls --order` and the TUI tasks table call it directly; the WebUI calls it through a wasm bridge function over the snapshot the wasm last fetched.

**Tech Stack:** Go (cli, tui with bubbles table), wasm bridge (cmd/harness-webui-wasm), plain JS (webui/static).

**Spec:** docs/superpowers/specs/2026-11-16-task-list-order-design.md

## Global Constraints

- Mode names, exactly: `created`, `activity`, `live`, `state`, `repo`; cycle order in the TUI is that order.
- State group labels, exactly and in this order: `needs you`, `ready`, `working`, `no report`, `queued`, `finished`. Live mode labels: `live`, `finished`.
- Live statuses: Queued, Running, Detached, Held. Terminal: Succeeded, Failed, Cancelled.
- Defaults unchanged: TUI `created`, WebUI `activity`, `ls` server order without `--order`.
- Empty groups never render.
- The repo is public: no local identifiers in tests or docs (land.py checks).

## Review Focus

1. A task with BOTH a lifecycle report and a program report: the lifecycle decides its state group (the badge rule).
2. Two repositories with the same last path element: both groups must be distinguishable (full path as the label).
3. TUI cursor on a header row after a refresh that shrinks a group: selection must land on a task, and actions (d, Enter…) never act on a header.
4. WebUI with a filter that admits ids the wasm snapshot does not have (snapshot lag): those ids are not dropped; they go last, ungrouped.
5. `o` while the tree view is on: a message, no change.

---

### Task 1: cli.OrderTasks

**Files:** Create `cli/task_order.go`, `cli/task_order_test.go`.

**Interfaces — Produces:**
- `type TaskOrder string` with consts `OrderCreated="created"`, `OrderActivity="activity"`, `OrderLive="live"`, `OrderState="state"`, `OrderRepo="repo"`; `var TaskOrders = []TaskOrder{...}` in that order.
- `func ParseTaskOrder(s string) (TaskOrder, error)` — unknown → error naming the valid modes.
- `func (o TaskOrder) Next() TaskOrder` — cycles.
- `type TaskGroup struct { Label string; Tasks []protocol.TaskInfo }` — `Label == ""` means no header (created / activity).
- `func OrderTasks(ts []protocol.TaskInfo, o TaskOrder, now time.Time) []TaskGroup` — never returns an empty group; does not mutate `ts`.
- `func TaskActivityAt(t protocol.TaskInfo, now time.Time) int64` (unix ms): live output → `now - OutputIdleMs` when `LastOutputAt > 0`, else max(EndedAt, StartedAt, CreatedAt)/1e6.

- [ ] Tests first: one table test per mode (order + labels), plus Review Focus 1 (lifecycle wins over program) and 2 (same basename → full paths), empty-group omission, `ParseTaskOrder("bogus")` error, `Next()` wraps repo → created.
- [ ] Run `go test ./cli/ -run 'Order|TaskActivity'` → FAIL (undefined).
- [ ] Implement; state classification reuses the badge rule: lifecycle reported (`LifecycleAt != 0`) decides; otherwise `ProgramState` when `LiveTerminal()`.
- [ ] Tests pass; commit.

### Task 2: `ls --order`

**Files:** `cli/verb/table.go` (ls row: flag `order`, Type string, Field `Order`, Help naming the modes; `Exclusive` adds `{tree, order}`), `cli/list.go` (render), `cli/list_test.go`.

- [ ] Tests: `ls --order state` text output has `== needs you (1) ==` header lines in group order and rows under them; `--order created` has no headers; `--json --order repo` rows in order with a `"group"` field; without `--order` the JSON has no `group` key; `--tree --order live` refused by the verb's Exclusive rule.
- [ ] Implement: header line format `== <label> (<n>) ==`; JSON field `group` (`omitempty`).
- [ ] `go test ./cli/... ` pass; commit.

### Task 3: TUI

**Files:** `tui/tasks.go` (order field, header rows, cursor skip, `SetOrder`/`Order`), `tui/app.go` (`onOrder`, refresh passes rows unsorted-by-mode), `tui/keys.go` (`Order: "o"`, binding row, scope tasks), tests in `tui/tasks_order_test.go`.

**Interfaces — Consumes:** Task 1.

- [ ] Tests: SetRows in `state` mode inserts header rows `── ready (2) ──` (exact text) before each group; `SelectedID()` / `SelectedTask()` return ""/nil never for a header because the cursor never rests on one: moving down onto a header lands on the next task, moving up lands on the previous task, a refresh that leaves the cursor on a header moves it to the nearest task (Review Focus 3); `o` cycles and puts `order: <mode>` in the result pane; `o` in tree mode reports `order: not with the tree view (T)` and changes nothing (Review Focus 5); default mode is `created` and the flat order equals today's (CreatedAt desc).
- [ ] Implement: rows built from `cli.OrderTasks(ts, m.order, now)`; a header row is a row whose cells are empty except the ID column carries the header text; `m.rowTasks[i]` zero-value + `m.rowHeader[i]=true`; a single skip helper used after every cursor-moving key and after rebuild.
- [ ] `make test` (tui) pass; live check in a dummy harness TUI (o cycles, headers render, cursor skips); commit.

### Task 4: WebUI

**Files:** `cmd/harness-webui-wasm/main.go` (cache last snapshot's `[]protocol.TaskInfo`; `harness.orderTasks(mode, ids) -> [{group, ids}]`), `webui/index.html` (select `#task-order` beside the chips), `webui/static/main.js` (render groups + headers, localStorage `taskOrder`, default `activity`), `webui/static/style.css` (`.task-group-head`), test in `webui/static/cmd_test.mjs` for the pure JS part (`orderedRowsModel(groups, byId)` → rows with headers), wasm-check.

**Interfaces — Consumes:** Task 1.

- [ ] Tests: the JS model turns `[{group:"ready", ids:[a]}, {group:"", ids:[b]}]` into header + rows with no header for `""`; ids the bridge did not return are appended last without a header (Review Focus 4).
- [ ] Implement; the bridge returns ids missing from its cache in a trailing `{group:"", ids}`.
- [ ] `make js-test`, `make wasm-check`; live check with `dummy-harness up --known-psk` (select each mode; headers; the choice survives reload); commit.

### Task 5: Docs

- [ ] README TUI section: `o` and the modes; `ls --order` in the CLI section; WebUI select. Commit; land with `scripts/land.py`.
