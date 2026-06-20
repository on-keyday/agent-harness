# Show task capabilities in ls / task-info Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development. Steps use `- [ ]`.

**Goal:** Surface each task's capability set in `ls`, the TUI detail popup, and the WebUI task rows (you can set caps but currently can't see them).

**Architecture:** Add `capabilities :Capability` to the `TaskInfo` wire type; `toTaskInfo` copies it from the persisted `TaskEntry.Capabilities`. A shared `cli.CapsLabel(c)` renders the bitmask as `all` / `none` / comma-joined snake_case names (single source = `Capability.String()`). ls / TUI-detail / WebUI all read `TaskInfo.Capabilities` and render via that label.

**Tech Stack:** Go, brgen (`.bgn`), cli, Bubble Tea (TUI), Go/wasm + JS (WebUI).

**Spec/decision (approved in chat):** display-only; legacy pre-feature tasks replay as `none` (unchanged — operator re-grants via resume override); no change to enforcement.

## Global Constraints
- Display-only; no enforcement / no semantics change. `TaskInfo` is read-only output.
- One source for cap names: `Capability.String()` via `cli.CapsLabel` / `cli.GrantableCaps`. No duplicate literal maps.
- Generated code not hand-edited (`message.go`); `.bgn` + `make protoregen`. Build hygiene: `make check`/`wasm-check` + focused `go test`. NEVER bare `go build ./cmd/<x>/`. Commit only intended files (no `git add -A`; ignore pre-existing untracked noise).
- WebUI matches dark palette + ≤600px; Playwright visual is DEFERRED (embed assets need a server rebuild).

---

## File Structure
- `runner/protocol/message.bgn` → `capabilities :Capability` on `TaskInfo` (regenerates message.go).
- `server/task_handler.go` → `toTaskInfo` copies `Capabilities`.
- `cli/caps.go` → `CapsLabel(c protocol.Capability) string` (all/none/comma).
- `cli/list.go` → render `caps=<label>` in the task row.
- `tui/detail.go` → `caps:` line in the detail popup.
- `cmd/harness-webui-wasm/main.go` → snapshot task map gains `"caps": cli.CapsLabel(...)`.
- `webui/static/main.js` → render `task.caps` in the task row.

---

## Task 1: TaskInfo.capabilities + toTaskInfo + cli.CapsLabel

**Files:** Modify `runner/protocol/message.bgn` (TaskInfo `:397`), regenerate `message.go`; `server/task_handler.go` (`toTaskInfo` `:1101`); `cli/caps.go`. Test: `runner/protocol/capability_test.go`, `cli/caps_test.go`, `server/task_handler_test.go` (or capabilities_test.go).

**Interfaces:** Produces `TaskInfo.Capabilities` (type `Capability`); `cli.CapsLabel(protocol.Capability) string`.

- [ ] **Step 1:** In `runner/protocol/message.bgn` `format TaskInfo:` add `capabilities :Capability` (place it after `creator_task_id`, with a comment "the task's server-enforced capability set"). Regenerate: `make protoregen ARGS='runner/protocol/message.bgn'`. (If protoregen fails for env reasons, STOP/BLOCKED.)

- [ ] **Step 2:** In `server/task_handler.go` `toTaskInfo` (`:1101`), add `Capabilities: t.Capabilities,` to the returned `protocol.TaskInfo{...}` literal.

- [ ] **Step 3: Add `cli.CapsLabel`** to `cli/caps.go`:

```go
// CapsLabel renders a capability bitmask as "all", "none", or a comma-joined
// list of the set granular cap names (from Capability.String()). Single source
// of names — no literal map.
func CapsLabel(c protocol.Capability) string {
	if c == protocol.Capability_All {
		return "all"
	}
	if c == protocol.Capability_None {
		return "none"
	}
	var names []string
	for _, g := range GrantableCaps() {
		if g == protocol.Capability_None || g == protocol.Capability_All {
			continue
		}
		if c&g == g {
			names = append(names, g.String())
		}
	}
	return strings.Join(names, ",")
}
```

- [ ] **Step 4: Tests.**
  - `cli/caps_test.go` `TestCapsLabel`: `All`→"all", `None`→"none", `Spawn|FileRead`→"spawn,file_read".
  - `runner/protocol/capability_test.go`: extend a TaskInfo round-trip to set `Capabilities` and assert it survives encode/decode.
  - `server` test: assert `toTaskInfo(TaskEntry{Capabilities: X}).Capabilities == X`.

```go
func TestCapsLabel(t *testing.T) {
	if got := CapsLabel(protocol.Capability_All); got != "all" { t.Fatalf("all=%q", got) }
	if got := CapsLabel(protocol.Capability_None); got != "none" { t.Fatalf("none=%q", got) }
	if got := CapsLabel(protocol.Capability_Spawn | protocol.Capability_FileRead); got != "spawn,file_read" {
		t.Fatalf("got %q", got)
	}
}
```

- [ ] **Step 5:** `go test ./runner/protocol/ ./cli/ ./server/ -run 'TestCapsLabel|TaskInfo|toTaskInfo|Capab' -v && make check` → PASS.

- [ ] **Step 6:** Commit. `git add runner/protocol/message.bgn runner/protocol/message.go server/task_handler.go cli/caps.go cli/caps_test.go runner/protocol/capability_test.go server/*_test.go && git commit -m "feat(protocol): expose task capabilities in TaskInfo + cli.CapsLabel"`

---

## Task 2: render caps in CLI ls + TUI detail

**Files:** Modify `cli/list.go` (`:129`-area), `tui/detail.go` (`formatTaskDetail`). Test: `cli/list_test.go` or `tui/detail_test.go` if present (else a focused render assertion).

**Interfaces:** Consumes `TaskInfo.Capabilities` (Task 1), `cli.CapsLabel`.

- [ ] **Step 1:** In `cli/list.go`, after the `createdBy` block (`:129-131`), add a `caps` segment and include it in the `Fprintf`:

```go
		caps := "  caps=" + CapsLabel(t.Capabilities)
```

Append `caps` to the row format string + args (next to `createdBy`). (Use the package-local `CapsLabel` — list.go is in package `cli`.)

- [ ] **Step 2:** In `tui/detail.go` `formatTaskDetail`, add a line after the `created by:`/`resumed by:` lines:

```go
	fmt.Fprintf(&sb, "caps:          %s\n", cli.CapsLabel(t.Capabilities))
```

(Import `cli` if not already; `t` is the `protocol.TaskInfo`.) If TUI already has a local `capsLabel` (from session-default-caps, for `sessionCaps`), leave it — but the detail line uses `cli.CapsLabel` on the task's caps (the single shared renderer); optionally refactor the local `capsLabel` to delegate to `cli.CapsLabel` (DRY) if trivial.

- [ ] **Step 3:** Tests — if `cli/list_test.go` exists, assert the rendered row contains `caps=`; if `tui` has a detail test, assert the `caps:` line. Otherwise add a minimal one. Then `go test ./cli/ ./tui/ && make check` → PASS.

- [ ] **Step 4:** Commit. `git add cli/list.go tui/detail.go cli/*_test.go tui/*_test.go && git commit -m "feat(cli,tui): show task caps in ls and detail popup"`

---

## Task 3: render caps in WebUI task rows

**Files:** Modify `cmd/harness-webui-wasm/main.go` (snapshot task map), `webui/static/main.js` (task row render). Test: Playwright (deferred).

**Interfaces:** Consumes `TaskInfo.Capabilities`, `cli.CapsLabel`.

- [ ] **Step 1:** In `cmd/harness-webui-wasm/main.go` `harnessSnapshot` (the loop that builds the per-task JS object — the same place `createdBy`/`from`/`resumedBy` are set), add `"caps": cli.CapsLabel(t.Capabilities),` to the task map (`t` is the decoded `protocol.TaskInfo`).

- [ ] **Step 2:** In `webui/static/main.js`, where a task row is rendered (the same code that appends `from`/`by`/`resumed_by` — search for `createdBy`/`from`), append the caps to the row text, e.g. `  caps=${t.caps}` (guard for undefined/empty → show nothing or `caps=none`). Match the existing attribution-string style. Keep dark palette; no layout change beyond the appended text.

- [ ] **Step 3:** `make wasm-check && make check && make webui-build`. Playwright (best-effort; defer to server rebuild if assets aren't live): a task row shows `caps=...`. If not live (embed), report the deferral.

- [ ] **Step 4:** Commit. `git add cmd/harness-webui-wasm/main.go webui/static/main.js && git commit -m "feat(webui): show task caps in task rows"`

---

## Self-Review
**Spec coverage:** TaskInfo.capabilities + toTaskInfo → T1; cli.CapsLabel (single name source) → T1; ls + TUI detail → T2; WebUI rows → T3. Display-only, no enforcement change. Legacy → none (unchanged), per the approved decision. ✓
**Type consistency:** `TaskInfo.Capabilities` (protocol), `cli.CapsLabel(protocol.Capability) string` used by ls/TUI/WebUI uniformly; names from `Capability.String()` via `GrantableCaps`. ✓
**Placeholder scan:** test-presence is conditional ("if a test file exists") — anchored, not a TODO. No TBD.
