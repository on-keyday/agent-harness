# Task Reparenting Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An operator can re-point a live task's parent link (`creator_task_id`) or atomically swap a task with its parent, from CLI, TUI and WebUI, without touching caps/scope or conversation history.

**Architecture:** New operator-only `TaskControlKind_SetParent` mirroring `set_caps` (gate = no principal task). `TaskStore.SetParent` / `SwapWithParent` mutate the link under one lock and append `task_parent_changed` WAL events. All display surfaces (`by=` etc.) follow the mutated field with zero display-code change.

**Tech Stack:** Go, `.bgn` schema (regen via `make protoregen`), bubbletea TUI, vanilla-JS WebUI + Go wasm bridge.

**Spec:** `docs/superpowers/specs/2026-08-14-task-reparent-design.md`

## Global Constraints

- Wire change ⇒ run `scripts/wire-skew-check.sh` before landing; restart order at deploy is SERVER FIRST (implementation-pitfalls Pitfall 10).
- Verify with `make check`, `make wasm-check`, `make vet`, `make test` — never bare `go build ./cmd/x` (drops a binary into the worktree).
- TUI/WebUI/wasm call `cli.SetParentWith` against their long-lived client; only the CLI binary dials (Pitfall 3).
- No caps/scope mutation, no connection teardown, no descendant list in responses (spec Decisions table).
- Result messages name the target and the change (checklist item 29); WebUI results go to `appendCmdOutput`, TUI to `a.cmdresult` (item 30).

---

### Task 1: Wire schema + codegen

**Files:**
- Modify: `runner/protocol/message.bgn` (TaskControlKind after `set_caps` ~:281; new formats after `SetCapsResponse` ~:1045; both union arms)
- Generated: `runner/protocol/message.go` via `make protoregen ARGS='runner/protocol/message.bgn'`

**Interfaces:**
- Produces: `protocol.TaskControlKind_SetParent`, `protocol.SetParentStatus_{Ok,NotFound,ParentNotFound,WouldCycle,NoParent,SwapTakesNoParent,NotOperator,InternalError}`, `protocol.SetParentRequest{TaskId, ParentId TaskID; Swap()/SetSwap(bool)}`, `protocol.SetParentResponse{Status; OldParent, NewParent, SwappedId TaskID}`, `TaskControlRequest.SetParent()/SetSetParent()`, `TaskControlResponse.SetParent()/SetSetParent()`.

- [ ] **Step 1: Add the schema members** — copy the four blocks verbatim from the spec's "Wire schema" section: the `set_parent` TaskControlKind member (after `set_caps`), `enum SetParentStatus`, `format SetParentRequest`, `format SetParentResponse` (place all three after `SetCapsResponse`), plus the two union arms:

```
        TaskControlKind.set_parent            => set_parent            :SetParentRequest
```
```
        TaskControlKind.set_parent            => set_parent            :SetParentResponse
```

- [ ] **Step 2: Regenerate** — Run: `make protoregen ARGS='runner/protocol/message.bgn'`
- [ ] **Step 3: Verify** — Run: `go build ./runner/... && grep -c "SetParentRequest" runner/protocol/message.go` — expect build OK, count > 0.
- [ ] **Step 4: Commit** — `feat(protocol): set_parent wire schema — operator reparent + swap`

### Task 2: TaskStore.SetParent / SwapWithParent + WAL

**Files:**
- Modify: `server/taskstore.go` (new methods after `SetCaps` ~:249; WAL replay switch ~:769; comment corrections at :315 and :746)
- Test: `server/taskstore_test.go`

**Interfaces:**
- Produces:
  - `func (s *TaskStore) SetParent(id string, parent protocol.TaskID) (TaskEntry, bool)`
  - `func (s *TaskStore) SwapWithParent(id string) (target, former TaskEntry, err error)` with sentinel errors `ErrSwapNotFound`, `ErrSwapNoParent`, `ErrSwapParentMissing`
  - WAL event type `"task_parent_changed"` (fields `task_id`, `creator_task_id`; absent `creator_task_id` = set to zero — the event type carries the intent, unlike `task_created` where absence means "never set")

- [ ] **Step 1: Write failing tests** in `server/taskstore_test.go`:

```go
func TestSetParentRepoints(t *testing.T) {
	s := NewTaskStore(nil)
	a := mustCreate(t, s, "repo", "a", protocol.TaskID{})           // helper per existing tests
	b := mustCreate(t, s, "repo", "b", taskIDOf(a))
	// detach
	got, ok := s.SetParent(b, protocol.TaskID{})
	if !ok || got.CreatorTaskID.Id != ([16]byte{}) {
		t.Fatalf("detach: ok=%v creator=%x", ok, got.CreatorTaskID.Id)
	}
	// re-point
	got, ok = s.SetParent(a, taskIDOf(b))
	if !ok || got.CreatorTaskID != taskIDOf(b) {
		t.Fatalf("repoint: %x", got.CreatorTaskID.Id)
	}
	if _, ok := s.SetParent("00000000000000000000000000000000", protocol.TaskID{}); ok {
		t.Fatal("missing task should return ok=false")
	}
}

func TestSwapWithParent(t *testing.T) {
	s := NewTaskStore(nil)
	p := mustCreate(t, s, "repo", "p", protocol.TaskID{})
	a := mustCreate(t, s, "repo", "a", taskIDOf(p))
	b := mustCreate(t, s, "repo", "b", taskIDOf(a))
	c := mustCreate(t, s, "repo", "c", taskIDOf(a)) // sibling stays under a
	target, former, err := s.SwapWithParent(b)
	if err != nil { t.Fatal(err) }
	if target.CreatorTaskID != taskIDOf(p) { t.Fatalf("b's parent = %x, want p", target.CreatorTaskID.Id) }
	if former.CreatorTaskID != taskIDOf(b) { t.Fatalf("a's parent = %x, want b", former.CreatorTaskID.Id) }
	sib, _ := s.Get(c)
	if sib.CreatorTaskID != taskIDOf(a) { t.Fatal("sibling c moved; must stay under a") }
	// error paths
	if _, _, err := s.SwapWithParent(p); err != ErrSwapNoParent { t.Fatalf("root swap: %v", err) }
	if _, _, err := s.SwapWithParent("00000000000000000000000000000000"); err != ErrSwapNotFound { t.Fatalf("missing: %v", err) }
}

func TestWALReplayRestoresParentChange(t *testing.T) {
	// create a→b, SetParent(b, zero) [detach], SwapWithParent path too;
	// reopen the WAL into a fresh store; assert links match the final state,
	// including the detach round-tripping through an ABSENT creator_task_id key.
}
```

(`mustCreate`/`taskIDOf` — reuse the file's existing creation helpers; `TestWALReplayRestoresAttribution` at :895 shows the WAL reopen pattern to copy for the third test.)

- [ ] **Step 2: Run to verify failure** — `go test ./server/ -run 'SetParent|SwapWithParent|ParentChange' -v` → compile error (methods undefined).
- [ ] **Step 3: Implement** in `server/taskstore.go` after `SetCaps`:

```go
// writeParentChangedLocked emits one task_parent_changed record carrying the
// task's post-change parent. Caller holds s.mu. An all-zero parent marshals
// with the creator_task_id key absent (omitempty); for THIS event type absence
// means "set to zero" — the type itself carries the intent, unlike
// task_created where an absent key means the link was never set.
func (s *TaskStore) writeParentChangedLocked(id string, parent protocol.TaskID) {
	if s.wal == nil {
		return
	}
	creatorHex := ""
	if parent.Id != ([16]byte{}) {
		creatorHex = hex.EncodeToString(parent.Id[:])
	}
	if err := s.wal.Write(WALEvent{Type: "task_parent_changed", TaskID: id, CreatorTaskID: creatorHex}); err != nil {
		slog.Error("WAL write failed", "op", "task_parent_changed", "task_id", id, "err", err)
	}
}

// SetParent re-points a live task's parent link. Operator-only at the handler;
// the store itself only refuses a missing task. Cycle prevention is the
// HANDLER's job (descendantsOf) — by the time this runs the request is valid.
func (s *TaskStore) SetParent(id string, parent protocol.TaskID) (TaskEntry, bool) {
	s.mu.Lock()
	e, ok := s.tasks[id]
	if !ok {
		s.mu.Unlock()
		return TaskEntry{}, false
	}
	e.CreatorTaskID = parent
	s.writeParentChangedLocked(id, parent)
	snapshot := *e
	s.mu.Unlock()
	return snapshot, true
}

var (
	ErrSwapNotFound      = errors.New("set_parent: task not found")
	ErrSwapNoParent      = errors.New("set_parent: task has no parent to swap with")
	ErrSwapParentMissing = errors.New("set_parent: parent record is gone")
)

// SwapWithParent inverts id with its current parent A: id takes A's own parent
// (possibly the root) and A becomes id's child. Both links are rewritten under
// one hold of s.mu, so the two-cycle the equivalent two-call sequence forms
// transiently is never observable — which is why this path needs no cycle
// check. A's grandparent link is deliberately not validated: a dangling
// creator (pruned) is a state the store already reaches today.
func (s *TaskStore) SwapWithParent(id string) (target, former TaskEntry, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return TaskEntry{}, TaskEntry{}, ErrSwapNotFound
	}
	if t.CreatorTaskID.Id == ([16]byte{}) {
		return TaskEntry{}, TaskEntry{}, ErrSwapNoParent
	}
	pHex := hex.EncodeToString(t.CreatorTaskID.Id[:])
	p, ok := s.tasks[pHex]
	if !ok {
		return TaskEntry{}, TaskEntry{}, ErrSwapParentMissing
	}
	var tid protocol.TaskID
	raw, derr := hex.DecodeString(id)
	if derr != nil || len(raw) != len(tid.Id) {
		return TaskEntry{}, TaskEntry{}, ErrSwapNotFound // store keys are valid hex by construction
	}
	copy(tid.Id[:], raw)
	t.CreatorTaskID, p.CreatorTaskID = p.CreatorTaskID, tid
	s.writeParentChangedLocked(id, t.CreatorTaskID)
	s.writeParentChangedLocked(pHex, tid)
	return *t, *p, nil
}
```

WAL replay case (in the switch at ~:769, after `task_caps_changed`):

```go
		case "task_parent_changed":
			if t, ok := s.tasks[ev.TaskID]; ok {
				var p protocol.TaskID
				if ev.CreatorTaskID != "" {
					if raw, err := hex.DecodeString(ev.CreatorTaskID); err == nil && len(raw) == len(p.Id) {
						copy(p.Id[:], raw)
					}
				}
				t.CreatorTaskID = p
			}
```

Comment corrections (spec "Comment and test corrections"): at :315 and :746 change *"records the original creator and must not change on resume"* to note resume still never touches it and `SetParent`/`SwapWithParent` are the only writers after Create.

- [ ] **Step 4: Run tests** — `go test ./server/ -run 'SetParent|SwapWithParent|ParentChange|Attribution|CreatorTaskID' -v` → PASS (including the two pre-existing attribution tests).
- [ ] **Step 5: Commit** — `feat(server): TaskStore.SetParent/SwapWithParent + task_parent_changed WAL`

### Task 3: Server handler

**Files:**
- Create: `server/set_parent_handler.go`
- Modify: `server/task_handler.go` (dispatch case after `SetCaps` ~:592; redaction comment ~:1785-1791)
- Test: `server/set_parent_handler_test.go` (model on the existing set_caps handler tests — grep `SetCapsStatus_NotOperator` for the harness pattern), plus a `scopeSet` assertion in `server/scope_attenuation_test.go`

**Interfaces:**
- Consumes: Task 1 protocol types, Task 2 store methods, existing `descendantsOf` / `childIndex` / `lookupPrincipal`.
- Produces: `handleSetParent(conn ConnHandle, requestID uint32, cid string, req *protocol.SetParentRequest)`.

- [ ] **Step 1: Write failing tests** covering the spec's Tests list (server section): not_operator; self-parent → would_cycle; descendant-parent → would_cycle; detach ok with old/new echo; swap on root → no_parent; swap with parent_id set → swap_takes_no_parent; swap with pruned parent → parent_not_found; P→A→B swap ok (links, response triple, sibling preservation, caps/scope byte-identical); swap with P=zero; scope integration (post-swap `scopeSet(B) ∋ A`, `scopeSet(A) ∌ B`).
- [ ] **Step 2: Run to verify failure** — `go test ./server/ -run SetParent -v` → compile error.
- [ ] **Step 3: Implement** `server/set_parent_handler.go`:

```go
package server

import (
	"encoding/hex"
	"log/slog"

	"github.com/on-keyday/agent-harness/appwire"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// handleSetParent re-points a live task's parent link on the operator's
// behalf, or (swap) inverts the task with its current parent.
//
// Gate and rationale mirror handleSetCaps: operator identity (no principal
// task) is the only ungrantable predicate — a capability bit authorising
// "re-point parents" would let its holder adopt any victim task under itself
// and inherit that victim's whole subtree, unrecoverable by intersectCaps.
func (h *TaskHandler) handleSetParent(conn ConnHandle, requestID uint32, cid string, req *protocol.SetParentRequest) {
	respond := func(status protocol.SetParentStatus, oldP, newP, swapped protocol.TaskID) {
		resp := protocol.TaskControlResponse{Kind: protocol.TaskControlKind_SetParent, RequestId: requestID}
		resp.SetSetParent(protocol.SetParentResponse{Status: status, OldParent: oldP, NewParent: newP, SwappedId: swapped})
		out := resp.MustAppend([]byte{byte(appwire.AppKind_TaskControl)})
		conn.SendMessage(out) //nolint:errcheck
	}
	var zero protocol.TaskID

	if h.lookupPrincipal(cid).Id != ([16]byte{}) {
		slog.Warn("set_parent denied: caller is not an operator", "cid", cid)
		respond(protocol.SetParentStatus_NotOperator, zero, zero, zero)
		return
	}
	targetHex := hex.EncodeToString(req.TaskId.Id[:])

	if req.Swap() {
		if req.ParentId.Id != ([16]byte{}) {
			respond(protocol.SetParentStatus_SwapTakesNoParent, zero, zero, zero)
			return
		}
		target, former, err := h.Tasks.SwapWithParent(targetHex)
		switch err {
		case nil:
		case ErrSwapNotFound:
			respond(protocol.SetParentStatus_NotFound, zero, zero, zero)
			return
		case ErrSwapNoParent:
			respond(protocol.SetParentStatus_NoParent, zero, zero, zero)
			return
		case ErrSwapParentMissing:
			respond(protocol.SetParentStatus_ParentNotFound, zero, zero, zero)
			return
		default:
			respond(protocol.SetParentStatus_InternalError, zero, zero, zero)
			return
		}
		formerID := taskIDFromHex(former.ID)
		slog.Info("set_parent swap applied", "task", targetHex, "now_under", hexOrRoot(target.CreatorTaskID), "former_parent", former.ID)
		respond(protocol.SetParentStatus_Ok, formerID, target.CreatorTaskID, formerID)
		if h.OnChange != nil {
			h.OnChange()
		}
		return
	}

	before, ok := h.Tasks.Get(targetHex)
	if !ok {
		respond(protocol.SetParentStatus_NotFound, zero, zero, zero)
		return
	}
	if req.ParentId.Id != ([16]byte{}) {
		parentHex := hex.EncodeToString(req.ParentId.Id[:])
		if _, ok := h.Tasks.Get(parentHex); !ok {
			respond(protocol.SetParentStatus_ParentNotFound, zero, zero, zero)
			return
		}
		// Cycle check. descendantsOf seeds the walk with the root it is asked
		// about (allowed[targetHex] = true), so parent==target is inside this
		// predicate too — no separate self-parent branch.
		descendants := make(map[string]bool)
		descendantsOf(h.childIndex(), targetHex, descendants)
		if descendants[parentHex] {
			respond(protocol.SetParentStatus_WouldCycle, zero, zero, zero)
			return
		}
	}
	after, ok := h.Tasks.SetParent(targetHex, req.ParentId)
	if !ok {
		respond(protocol.SetParentStatus_NotFound, zero, zero, zero)
		return
	}
	slog.Info("set_parent applied", "task", targetHex, "old", hexOrRoot(before.CreatorTaskID), "new", hexOrRoot(after.CreatorTaskID))
	respond(protocol.SetParentStatus_Ok, before.CreatorTaskID, after.CreatorTaskID, zero)
	if h.OnChange != nil {
		h.OnChange()
	}
}

// taskIDFromHex converts a store key back to a wire TaskID; store keys are
// valid 32-hex by construction, so a failed decode yields the zero id.
func taskIDFromHex(idHex string) protocol.TaskID {
	var tid protocol.TaskID
	raw, err := hex.DecodeString(idHex)
	if err == nil && len(raw) == len(tid.Id) {
		copy(tid.Id[:], raw)
	}
	return tid
}

func hexOrRoot(id protocol.TaskID) string {
	if id.Id == ([16]byte{}) {
		return "(root)"
	}
	return hex.EncodeToString(id.Id[:])
}
```

Dispatch case in `task_handler.go` (copy the `SetCaps` case shape at ~:588-592):

```go
	case protocol.TaskControlKind_SetParent:
		sp := req.SetParent()
		if sp == nil {
			slog.Error("TaskHandler: SetParent variant is nil")
			return
		}
		h.handleSetParent(conn, req.RequestId, cid, sp)
```

Rewrite the `redactParentTaskInfo` comment (~:1785-1791): the superset guarantee now holds only for links Create made; disclosure argument unchanged (spec "Comment and test corrections" item 3).

- [ ] **Step 4: Run** — `go test ./server/ -v -run 'SetParent|Scope'` → PASS; then `go test ./server/` full → PASS (re-run once if `TestOpenInteractiveSessionMux` flakes — known pre-existing).
- [ ] **Step 5: Commit** — `feat(server): handleSetParent — operator-only reparent with cycle guard + atomic swap`

### Task 4: Shared client helper

**Files:**
- Create: `cli/set_parent.go`
- Test: `cli/set_parent_test.go` (model on `cli/whoami_test.go` / existing fake `taskControlClient` if present; else round-trip via request construction)

**Interfaces:**
- Produces:

```go
type SetParentOpts struct {
	TaskID   string // hex (full 32)
	ParentID string // hex; "" = detach to root (all-zero on the wire)
	Swap     bool
}
type SetParentResult struct {
	Status    protocol.SetParentStatus
	OldParent string // hex, "" = root
	NewParent string
	SwappedID string
}
func SetParentWith(ctx context.Context, c taskControlClient, opts SetParentOpts) (SetParentResult, error)
func SetParent(ctx context.Context, serverCID objproto.ConnectionID, opts SetParentOpts) (SetParentResult, error)
func SetParentMessage(opts SetParentOpts, res SetParentResult) string
```

- [ ] **Step 1: Write failing tests**: request construction for the three forms (`--parent`, detach, swap — assert `TaskId`/`ParentId`/`Swap()` on the built request via a fake client capturing it); `SetParentMessage` renders the item-29 shapes:
  - `set-parent <B8>: parent=<A8> → <P8>`
  - `set-parent <B8>: parent=<A8> → (root)`
  - `set-parent <B8> --swap: <B8> now under (root), <A8> now under <B8>`
- [ ] **Step 2: Verify failure**, **Step 3: Implement** (mirror `cli/set_caps.go`: parse ids via `parseTaskIDHex`, build `SetParentRequest`, `RoundTripTaskControl`, decode; map statuses in `setParentStatusError` — every non-ok status gets a human message, `would_cycle` explains "the new parent is a descendant of the task"). `SetParentMessage` helper:

```go
// SetParentMessage renders the operator-facing result line (checklist item
// 29): it names the target and the change, never a bare count.
func SetParentMessage(opts SetParentOpts, res SetParentResult) string {
	short := func(hexID string) string {
		if hexID == "" {
			return "(root)"
		}
		return hexID[:8]
	}
	t := short(opts.TaskID)
	if opts.Swap {
		return fmt.Sprintf("set-parent %s --swap: %s now under %s, %s now under %s",
			t, t, short(res.NewParent), short(res.SwappedID), t)
	}
	return fmt.Sprintf("set-parent %s: parent=%s → %s", t, short(res.OldParent), short(res.NewParent))
}
```

- [ ] **Step 4: Run** — `go test ./cli/ -run SetParent -v` → PASS.
- [ ] **Step 5: Commit** — `feat(cli): SetParent/SetParentWith + shared result message`

### Task 5: CLI verb + caps catalog line

**Files:**
- Modify: `cmd/harness-cli/main.go` (`case "caps"` at :191 — add `set-parent` dispatch; new `runCapsSetParent` beside `runCapsSet` at :1145)
- Modify: `cli/caps.go` (`WriteCaps` SCOPE section: one line — subtree membership is computed from the parent link, which `caps set-parent` re-points)

**Interfaces:** Consumes Task 4.

- [ ] **Step 1: Implement dispatch** — in `case "caps"`, before the catalog fallthrough:

```go
		if len(args) > 0 && args[0] == "set-parent" {
			runCapsSetParent(ctx, parseCID(), args[1:])
			return
		}
```

`runCapsSetParent` copies `runCapsSet`'s interspersed-parse loop verbatim (flags stay order-free), with flags `--parent <id>`, `--none`, `--swap`; exactly one positional task id; exactly one of the three flags (else usage to stderr, exit 2); calls `cli.SetParent`, prints `cli.SetParentMessage` to stdout, `die(err)` on error.

- [ ] **Step 2: Manual check** — `go run ./cmd/harness-cli caps set-parent` (no server needed for the usage error path) → usage line, exit 2. `go vet ./cmd/harness-cli`.
- [ ] **Step 3: Commit** — `feat(harness-cli): caps set-parent verb`

### Task 6: TUI — cmdline verb, keybinding, parent picker mode

**Files:**
- Modify: `tui/cmdline.go` (dispatch under `case "caps"` at :357; new `SetParentAction` + `parseSetParent` beside `parseSetCaps` :1063)
- Modify: `tui/client.go` (`DoSetParent` + `SetParentResultMsg` beside `DoSetCaps` :164)
- Modify: `tui/keys.go` (`SetParent: "A"` in mainKeyMap + mainKeyBindings row "set parent")
- Modify: `tui/app.go` (key dispatch beside ReGrant :1813; picker Enter branch :1377; `SetParentResultMsg` handling beside `SetCapsResultMsg`)
- Modify: `tui/authoritypicker.go` (`PickerModeParent`, `rowParentRoot`/`rowParentSwap` kinds, `OpenParent`, `ParentChoice`)
- Test: `tui/cmdline_test.go` (parser: three forms + mutual-exclusion error), `tui/keys_test.go` (auto via reflect), `tui/app_noclient_test.go` (add `{"set-parent", SetParentAction{...}}` row)

**Interfaces:**
- Consumes: `cli.SetParentOpts/SetParentWith/SetParentMessage`.
- Produces: `SetParentAction{TaskID, ParentID string, Detach, Swap bool}` (isAction); `DoSetParent(c *cli.Client, opts cli.SetParentOpts) tea.Cmd`; picker `OpenParent(target protocol.TaskInfo, tasks []protocol.TaskInfo)` and `ParentChoice() (parentHex string, detach, swap bool)`.

- [ ] **Step 1: Failing parser tests** — `caps set-parent <id> --parent <id>` / `--none` / `--swap`; two of the three flags → error; zero of the three → error.
- [ ] **Step 2: Implement cmdline** — `parseSetParent` scans args like `parseSetCaps` (manual loop, `--parent` takes a value, positional = target id).
- [ ] **Step 3: Implement DoSetParent** —

```go
func DoSetParent(c *cli.Client, opts cli.SetParentOpts) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		res, err := cli.SetParentWith(ctx, c, opts)
		return SetParentResultMsg{Opts: opts, Res: res, Err: err}
	}
}
```

Msg handling appends `ErrorStyle.Render(err)` or `OKStyle.Render(cli.SetParentMessage(msg.Opts, msg.Res))` to `a.cmdresult` (grep `SetCapsResultMsg` in app.go and mirror its shape exactly).

- [ ] **Step 4: Action dispatch in app.go** — beside `SetCapsAction` :2450: resolve `v.TaskID` (and `v.ParentID` when non-empty) via `a.resolveTaskIDPrefix`, then `DoSetParent`.
- [ ] **Step 5: Keybinding + picker** — `A` opens `a.authorityPicker.OpenParent(*t, a.tasks.Rows())` (guard `focus == focusTasks`, mirror the ReGrant block :1813). Picker parent mode: `OpenParent` builds `[{rowParentRoot, "(root — detach)"}]`, plus `{rowParentSwap, "(swap with <P8>)"}` when the target has a parent, then task rows (excluding the target itself); `Toggle` in parent mode moves selection (single-select: selecting a row clears others); Enter in app.go: `if mode == PickerModeParent { parentHex, detach, swap := picker.ParentChoice(); ... DoSetParent }`. `SetAllCaps` is a no-op in parent mode. Check `View()` renders label-only rows correctly for the two pseudo-kinds (adapt the row renderer's checkbox prefix to a `▸` cursor-only prefix for parent mode).
- [ ] **Step 6: Run** — `go test ./tui/ -v -run 'SetParent|Keys'` → PASS; `go test ./tui/` → PASS.
- [ ] **Step 7: Commit** — `feat(tui): set-parent cmdline verb + A keybinding + parent picker mode`

### Task 7: wasm bridge + raw snapshot field

**Files:**
- Modify: `cmd/harness-webui-wasm/main.go` (register `"setParent"` in the map at :112; new `harnessSetParent` beside `harnessSetCaps` :352; add `"createdById"` to the task map at :626)

**Interfaces:**
- Consumes: `cli.SetParentWith`.
- Produces: JS `harness.setParent({taskId, parentId?, swap?}) -> Promise<{oldParent, newParent, swappedId}>` (hex strings, `""` = root); snapshot task field `createdById` (full 32-hex, `""` when zero).

- [ ] **Step 1: Implement** `harnessSetParent` mirroring `harnessSetCaps`'s promise/executor shape: read `taskId` (required), `parentId` (optional string), `swap` (optional bool) — key names exactly as the JS sends them (checklist item 8); call `cli.SetParentWith(rootCtx, c, ...)`; resolve `{"oldParent": res.OldParent, "newParent": res.NewParent, "swappedId": res.SwappedID}`.
- [ ] **Step 2: Snapshot field** — beside `"createdBy"`:

```go
					// createdById is the RAW full id beside the short label:
					// the parent-picker dialog highlights the current parent
					// by exact id (a truncated prefix could match the wrong
					// row), same label+raw pattern as capsBits/scopeBase.
					"createdById": func() string {
						if t.CreatorTaskId.Id == ([16]byte{}) {
							return ""
						}
						return hex.EncodeToString(t.CreatorTaskId.Id[:])
					}(),
```

- [ ] **Step 3: Run** — `make wasm-check` → PASS.
- [ ] **Step 4: Commit** — `feat(wasm): harness.setParent bridge + createdById raw snapshot field`

### Task 8: WebUI — task-sheet action, parent dialog, runCmd verb

**Files:**
- Modify: `webui/index.html` (new `<dialog id="parent-modal" class="picker-modal">` beside the regrant modal markup)
- Modify: `webui/static/main.js` (`openParentDialog(t)` beside `openRegrantDialog` :2736; `addItem("⇄ 親タスク変更", ...)` beside the 再付与 item :3809; `case "set-parent"` in `runCmd`; help text)

**Interfaces:** Consumes `window.harness.setParent`, snapshot `createdById`.

- [ ] **Step 1: Dialog** — markup mirrors `regrant-modal` (title, scrollable row list, apply/cancel buttons; width from the viewport per `.picker-modal` CSS, item 32). `openParentDialog(t)` fills the list: `(root — detach)` row; `(swap with <P8>)` row when `t.createdById`; then one row per snapshot task except `t.id`, highlighting the row whose id `=== t.createdById`. Single-select radio behaviour; apply calls `harness.setParent` with `{taskId: t.id}` + (`{}` for detach — omit parentId — / `{swap:true}` / `{parentId: pickedId}`), then `appendCmdOutput` the same message shapes as `cli.SetParentMessage` (JS-side copy, like the `caps set` message at :2782 — dark-theme + 390px check at verification).
- [ ] **Step 2: runCmd case** —

```js
        case "set-parent": {
          // set-parent <task-id> (--parent <id> | --none | --swap)
          let parent = null, none = false, swap = false, target = null;
          for (let i = 1; i < tokens.length; i++) {
            const t = tokens[i];
            if (t === "--none") none = true;
            else if (t === "--swap") swap = true;
            else if (t === "--parent") { i++; parent = tokens[i]; }
            else if (t.startsWith("--parent=")) parent = t.slice("--parent=".length);
            else if (t.startsWith("-")) throw new Error(`set-parent: unknown flag ${t}`);
            else if (!target) target = t;
            else throw new Error(`set-parent: unexpected arg ${t}`);
          }
          if (!target) throw new Error("set-parent: missing task id");
          const picked = [parent !== null, none, swap].filter(Boolean).length;
          if (picked !== 1) throw new Error("set-parent: pass exactly one of --parent <id>, --none, --swap");
          const req = { taskId: target };
          if (swap) req.swap = true;
          else if (parent !== null) req.parentId = parent;
          const r = await window.harness.setParent(req);
          const s8 = (h) => h ? h.slice(0, 8) : "(root)";
          out = swap
            ? `set-parent ${target.slice(0, 8)} --swap: ${target.slice(0, 8)} now under ${s8(r.newParent)}, ${s8(r.swappedId)} now under ${target.slice(0, 8)}`
            : `set-parent ${target.slice(0, 8)}: parent=${s8(r.oldParent)} → ${s8(r.newParent)}`;
          break;
        }
```

- [ ] **Step 3: Verify** — `make check` (bundles webui-build) → PASS. Visual check happens in the dummy-harness task.
- [ ] **Step 4: Commit** — `feat(webui): set-parent — task-sheet dialog + command verb`

### Task 9: Docs

**Files:**
- Modify: `README.md` ("Capabilities and scope" section: parent-link paragraph — what subtree is computed from, `caps set-parent`/`--swap`; TUI cmdline verb list + `A` key)
- Modify: `docs/superpowers/specs/2026-08-13-task-scoped-caps-design.md` (append `## Amendment (2026-08-14): the creator link is operator-mutable` — two sentences pointing at the reparent spec)

- [ ] **Step 1: Write both**, **Step 2: Commit** — `docs: parent link — README + task-scoped-caps amendment`

### Task 10: Full verification + wire-skew

- [ ] **Step 1:** `make check && make wasm-check && make vet && make test` → all PASS (worktree stays clean: `git status --porcelain` empty apart from intended files).
- [ ] **Step 2:** `scripts/wire-skew-check.sh` → PASS (asserts NEW runner × OLD server skew is recoverable).
- [ ] **Step 3: Commit** anything outstanding.

### Task 11: Dummy-harness E2E (per the dummy-harness skill)

- [ ] **Step 1:** Read `.claude/skills/dummy-harness/SKILL.md`; bring up an instance (`scripts/dummy-harness.sh up --detach --agent fake --name reparent`), eval its env.
- [ ] **Step 2:** Create a parent-child pair (agent-credentialed spawn per the skill's recipe), then from the operator env: `harness-cli ls` shows `by=<A8>` on B; `caps set-parent <B> --swap`; `ls` now shows `by=<B8>` on A and no `by=` (or `by=<P8>`) on B. Exercise `--none`, a `would_cycle` rejection, and `not_operator` from the agent side.
- [ ] **Step 3:** WebUI visual check via Playwright (dark theme, desktop AND 390px): task sheet → 親タスク変更 dialog, apply, result in cmd output. Screenshots are deliverables — name them `webui-setparent-*.png`, report paths, do not delete.
- [ ] **Step 4:** `scripts/dummy-harness.sh down --name reparent` (prune probe tasks first).

### Task 12: Land to main

- [ ] Invoke the `landing-to-main` skill: rebase onto current trunk, FF-push to origin/main (Mode A), then `make build` in the main checkout (memory: build is part of landing). Server-first restart note applies at deploy.

## Self-Review

- Spec coverage: schema→T1, store/WAL→T2, handler/comments→T3, funnel→T4, CLI+catalog→T5, TUI (cmdline/keys/picker)→T6, wasm+createdById→T7, WebUI (dialog/runCmd)→T8, README+amendment→T9, skew→T10, E2E→T11. All spec sections mapped.
- Checklist-16 omission (no TUI table column) needs no task — it is a decided non-change recorded in the spec.
- Type consistency: `SetParentOpts{TaskID, ParentID, Swap}` and result field names used identically in Tasks 4-8.
