# Selectable caps/scope pickers Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace typed caps-name/scope-grammar entry in the TUI and WebUI
(re-grant and spawn) with selection UIs — chips/checkboxes/radios — that
serialize back to the existing `--scope` grammar string.

**Architecture:** One selection model (caps bitmask + ScopeBase + selected
task-id set) per surface, serialized on apply to the grammar string and fed
into the unchanged `cli.ParseScope` funnel. TUI gets a single-list
`AuthorityPickerModel` popup with two openers (`a` re-grant, `caps`/`scope`
no-arg session defaults); WebUI gets a `<dialog>` for re-grant and an inline
base-radio + task-checklist for Compose.

**Tech Stack:** Go (bubbletea TUI), vanilla JS + native `<dialog>` (WebUI),
existing wasm bridge (`window.harness.setCaps`, unchanged).

**Spec:** `docs/superpowers/specs/2026-08-13-selectable-authority-ui-design.md`
— read it before Task 1. Its Problem section defines the acceptance bar.

## Global Constraints

- **No wire, bridge, or CLI changes.** Scope reaches the server as the same
  grammar string through `cli.ParseScope`; caps as the same bitmask.
- **Re-grant applies send BOTH fields explicitly** (scope includes literal
  `subtree`); spawn pickers serialize base-subtree-no-ids to `""`.
- **`base = global` disables task-id selection** in every picker.
- **Task lists include terminal tasks, task-table order; re-grant omits the
  target task itself.**
- **Prefill**: re-grant from the target's `Capabilities`/`Scope`; spawn from
  session defaults.
- **Dark theme** (`#1e1e1e`/`#d4d4d4`) and 390px width for every WebUI change.
- **Verify with make targets**: `make check`, `make wasm-check`, `make test`.
- WebUI hot-reloads from `--webui-dir` in the dummy harness — no server
  rebuild for JS-only iterations, but `make wasm-check` still gates.

---

### Task 1: TUI `AuthorityPickerModel` — pure model + serialization

**Files:**
- Create: `tui/authoritypicker.go`
- Test: `tui/authoritypicker_test.go` (create)

**Interfaces produced (Task 2 depends on these exact names):**

```go
type AuthorityPickerMode int
const (
    PickerModeRegrant AuthorityPickerMode = iota // caps+scope+cascade+keep-conns → set_caps
    PickerModeSession                            // caps+scope → session defaults
)

type AuthorityPickerModel struct{ /* unexported fields */ }

// OpenRegrant opens on a target task, prefilled from its stored authority.
// tasks is the full task-table snapshot; the target row is filtered out.
func (m *AuthorityPickerModel) OpenRegrant(target protocol.TaskInfo, tasks []protocol.TaskInfo)
// OpenSession opens prefilled with the session defaults.
func (m *AuthorityPickerModel) OpenSession(caps protocol.Capability, scope protocol.TaskScope, tasks []protocol.TaskInfo)
func (m *AuthorityPickerModel) IsOpen() bool
func (m *AuthorityPickerModel) Close()
func (m *AuthorityPickerModel) Mode() AuthorityPickerMode
func (m *AuthorityPickerModel) TargetID() string // regrant target hex, "" in session mode

// Move moves the cursor by delta, skipping disabled rows (task rows while
// base==global; wraps at both ends).
func (m *AuthorityPickerModel) Move(delta int)
// Toggle flips the current row (cap/task/cascade/keep-conns) or cycles the
// base row subtree→none→global→subtree.
func (m *AuthorityPickerModel) Toggle()

// Result returns the current selection, serialized.
//   caps      — the edited bitmask
//   scopeSpec — grammar string; PickerModeRegrant always explicit
//               ("subtree"|"none"|"global"|"ids:…"|"subtree+ids:…"),
//               PickerModeSession returns "" for base-subtree-no-ids
//   cascade, keepConns — false in session mode
func (m *AuthorityPickerModel) Result() (caps protocol.Capability, scopeSpec string, cascade, keepConns bool)

func (m *AuthorityPickerModel) SetSize(w, h int)
func (m *AuthorityPickerModel) View() string // includes the grammar echo footer
```

Internal row model: one flat `[]pickerRow` — 12 cap rows (from
`cli.CapsCatalog()` filtered to real bits: drop `Bit == 0` and
`Bit == uint32(protocol.Capability_All)`), 1 base row, N task rows
(short id, status, agent, prompt head — reuse `FormatTaskID` and the
task-table's status word), then cascade + keep-conns rows only in
regrant mode. Scroll window follows the cursor within the popup height.

- [ ] **Step 1: Write the failing tests**

```go
func pickerTask(b byte, prompt string) protocol.TaskInfo {
	var ti protocol.TaskInfo
	ti.Id.Id[0] = b
	ti.Prompt = []byte(prompt)
	return ti
}

// Serialization table: every base × ids combination, both modes.
func TestAuthorityPickerScopeSerialization(t *testing.T) { /* table:
    regrant  subtree no-ids  -> "subtree"
    regrant  none    no-ids  -> "none"
    regrant  global  (ids selected before cycling to global) -> "global"
    regrant  none    ids{X}  -> "ids:<x>"
    regrant  subtree ids{X,Y}-> "subtree+ids:<x>,<y>"
    session  subtree no-ids  -> ""
    session  none    no-ids  -> "none"
    drive via OpenRegrant/OpenSession + Move/Toggle on real rows, then
    assert Result(); every produced non-"" spec must round-trip through
    cli.ParseScope without error */ }

// Prefill: caps bits lit, scope ids pre-checked, target row absent.
func TestAuthorityPickerRegrantPrefill(t *testing.T) { /* target with
    Capabilities=cancel|notify, Scope{Base:None, IDs:[sibling]}; tasks =
    [target, sibling, other]. Assert cap rows for cancel+notify start on,
    sibling task row starts checked, target row not present. */ }

// base=global disables task rows: Move skips them, Result ignores them.
func TestAuthorityPickerGlobalSkipsTaskRows(t *testing.T) {}

// Toggle on the base row cycles subtree→none→global→subtree.
func TestAuthorityPickerBaseCycle(t *testing.T) {}

// Session mode hides cascade/keep-conns rows and returns false for both.
func TestAuthorityPickerSessionModeRows(t *testing.T) {}
```

- [ ] **Step 2: Run to see them fail**

`go test ./tui/ -run TestAuthorityPicker -v` — expect undefined symbols.

- [ ] **Step 3: Implement the model**

Core serialization (the one non-obvious function):

```go
func scopeSpecFor(base protocol.ScopeBase, ids []string, sessionMode bool) string {
	sort.Strings(ids)
	switch base {
	case protocol.ScopeBase_Global:
		return "global"
	case protocol.ScopeBase_None:
		if len(ids) == 0 {
			return "none"
		}
		return "ids:" + strings.Join(ids, ",")
	default: // subtree
		if len(ids) == 0 {
			if sessionMode {
				return "" // the spawn default
			}
			return "subtree"
		}
		return "subtree+ids:" + strings.Join(ids, ",")
	}
}
```

View: bordered popup like `RunnerPickerModel`, cursor row inverted, `[x]`
checkbox glyphs, base row `base: subtree`, footer =
`space toggle · enter apply · esc cancel` + the grammar echo (truncated to
width with `runewidth`).

- [ ] **Step 4: Run tests to green**

`go test ./tui/ -run TestAuthorityPicker` — PASS.

- [ ] **Step 5: Commit**

```bash
git add tui/authoritypicker.go tui/authoritypicker_test.go
git commit -m "feat(tui): AuthorityPickerModel — selectable caps/scope, grammar-string output"
```

---

### Task 2: TUI wiring — `a` opens the picker; `caps`/`scope` no-arg too

**Files:**
- Modify: `tui/app.go` (`authorityPicker AuthorityPickerModel` field; `a` key
  handler; picker key dispatch while open; `CapsAction`/`ScopeAction` Show
  paths), `tui/tasks.go` (add `Rows()`), `tui/keys.go` (help text wording only)
- Test: `tui/app_regrant_test.go` (rewrite), `tui/app_noclient_test.go`
  (unchanged — must keep passing)

**Interfaces:**
- Consumes: everything Task 1 produces; existing `DoSetCaps`,
  `cli.ParseScope`, `cli.SetCapsOpts`.
- Produces: `func (m *TasksModel) Rows() []protocol.TaskInfo` (snapshot copy
  of the current rows).

- [ ] **Step 1: Write the failing tests** (replace `app_regrant_test.go`)

```go
// `a` on a selected task opens the picker in regrant mode on that task.
func TestReGrantKeyOpensPicker(t *testing.T) { /* SetRows one task, send
    'a', assert authorityPicker.IsOpen() && Mode()==PickerModeRegrant &&
    TargetID()==<hex>; cmdline stays empty */ }

// No selection still warns and does not open.
func TestReGrantKeyNoSelection(t *testing.T) {}

// Esc closes without dispatching.
func TestPickerEscCloses(t *testing.T) {}

// Enter in regrant mode dispatches a cmd (DoSetCaps closure) when a client
// is bound, and warns "not connected" with a nil client.
func TestPickerEnterDispatchesSetCaps(t *testing.T) {}
func TestPickerEnterNilClient(t *testing.T) {}

// `caps` / `scope` with no argument open the picker in session mode;
// applying writes sessionCaps+sessionScope.
func TestCapsNoArgOpensPicker(t *testing.T) {}
func TestScopeNoArgOpensPicker(t *testing.T) {}
func TestPickerSessionApplyWritesDefaults(t *testing.T) {}
```

`CapsAction{Show:true}` / `ScopeAction{Show:true}` now open the picker
instead of printing — they stay in the nil-client allowlist (opening needs
no client; only a regrant APPLY needs one).

- [ ] **Step 2: Run to see them fail**

`go test ./tui/ -run 'TestReGrant|TestPicker|TestCapsNoArg|TestScopeNoArg' -v`

- [ ] **Step 3: Wire it**

In the `tea.KeyMsg` dispatch, BEFORE the pane switches (same position class
as `runnerPicker`): while `a.authorityPicker.IsOpen()`, route
j/k/up/down → `Move(±1)`, space → `Toggle()`, esc → `Close()`, enter →
apply-and-close:

```go
case "enter":
    caps, spec, cascade, keep := a.authorityPicker.Result()
    mode, target := a.authorityPicker.Mode(), a.authorityPicker.TargetID()
    a.authorityPicker.Close()
    if mode == PickerModeSession {
        a.sessionCaps = caps
        if spec == "" {
            a.sessionScope = protocol.TaskScope{Base: protocol.ScopeBase_Subtree}
        } else {
            sc, err := cli.ParseScope(spec) // cannot fail: picker-built
            if err != nil { a.cmdresult.Append(ErrorStyle.Render("scope: " + err.Error())); return a, nil }
            a.sessionScope = sc
        }
        a.cmdresult.Append(OKStyle.Render("defaults set: ") + capsLabel(a.sessionCaps) + "  scope=" + cli.ScopeLabel(a.sessionScope))
        return a, nil
    }
    if a.client == nil {
        a.cmdresult.Append(WarnStyle.Render("not connected — wait for the connection or check the server"))
        return a, nil
    }
    sc, err := cli.ParseScope(spec)
    if err != nil { a.cmdresult.Append(ErrorStyle.Render("scope: " + err.Error())); return a, nil }
    return a, DoSetCaps(a.client, cli.SetCapsOpts{
        TaskID: target, Caps: &caps, Scope: &sc, Cascade: cascade, KeepConns: keep,
    })
```

The `a` key handler replaces its SetValue/CursorEnd body with
`a.authorityPicker.OpenRegrant(*t, a.tasks.Rows())` (warn path unchanged).
`View()` overlays the picker like `runnerPicker` does. Update the `a
re-grant` help Long text to "open the re-grant picker for the selected
task's caps/scope (operator-only)".

- [ ] **Step 4: Full TUI tests green**

`go test ./tui/` — PASS (including untouched noclient + cmdline tests).

- [ ] **Step 5: Commit**

```bash
git add tui/
git commit -m "feat(tui): a / caps / scope open the authority picker instead of typed grammar"
```

---

### Task 3: WebUI — re-grant dialog and Compose scope selector

**Files:**
- Modify: `webui/static/main.js` (factor `buildCapChips`; replace
  `promptSetCaps`; replace `#scope-input` handling), `webui/static/index.html`
  (the `<dialog id="regrant-modal">`, Compose scope block), `webui/static/style.css`
  (only if the existing `cap-chip` / dialog styles don't cover the new rows),
  `cmd/harness-webui-wasm/main.go` (snapshot row gains raw prefill fields)

**Interfaces:**
- Consumes: `window.harness.setCaps({taskId, caps, scope, cascade, keepConns})`
  (unchanged), `window.harness.capList()`, snapshot task rows already held in
  `lastTasks`/equivalent render source.
- Produces: `buildCapChips(container, getBits, setBits)` used by both Compose
  and the dialog; `openRegrantDialog(task)` replacing `promptSetCaps(taskId)`
  (the caller passes the full task row, which carries caps + scope).

- [ ] **Step 1: Read the anchors**

`renderCaps()` (`main.js:2555`), `promptSetCaps` (`main.js:2631`), the
`filePreviewModal` `showModal()` pattern, and how the task detail sheet
builds its buttons (`addItem("🔑 caps/scope 再付与", …)`).

The snapshot row carries only LABEL strings (`caps: cli.CapsLabel(…)` at
`cmd/harness-webui-wasm/main.go:613`) — label forms like `all,-spawn` make
JS back-parsing fragile, so the wasm snapshot conversion gains three raw
fields alongside the labels: `capsBits` (number),
`scopeBase` (`"subtree"|"none"|"global"`), `scopeIds` (array of hex
strings). Display keeps using the labels; prefill uses the raw fields.

- [ ] **Step 2: Factor the chips + build the dialog**

`buildCapChips(container, getBits, setBits)`: exactly today's chip loop with
the closure variables replaced by the two accessors; Compose calls it with
`() => spawnCaps` / `(v) => { spawnCaps = v; }`. The dialog markup:

```html
<dialog id="regrant-modal">
  <h3>caps/scope 再付与 <span id="regrant-task"></span></h3>
  <div id="regrant-chips"></div>
  <div id="regrant-base">
    <label><input type="radio" name="regrant-base" value="subtree" checked>subtree</label>
    <label><input type="radio" name="regrant-base" value="none">none</label>
    <label><input type="radio" name="regrant-base" value="global">global</label>
  </div>
  <div id="regrant-tasks" class="task-checklist"></div>
  <label><input type="checkbox" id="regrant-cascade">--cascade（子孫も締める）</label>
  <label><input type="checkbox" id="regrant-keep-conns">--keep-conns（接続を切らない）</label>
  <code id="regrant-echo"></code>
  <div><button id="regrant-apply">適用</button><button id="regrant-cancel">キャンセル</button></div>
</dialog>
```

`openRegrantDialog(t)`: prefill chips from `t.capsBits`, radio from
`t.scopeBase`, pre-checked task checkboxes from `t.scopeIds`,
rebuild `#regrant-tasks` from the latest snapshot minus `t.id`, disable the
checklist when the `global` radio is on, live-update `#regrant-echo` on
every change with the same serialization as Task 1's `scopeSpecFor`
(re-implemented in JS: `scopeSpecJS(base, ids)` — always explicit, this is
the regrant path). 適用 →

```js
const req = { taskId: t.id, caps: bits, scope: scopeSpecJS(base, ids),
              cascade: cascadeBox.checked, keepConns: keepBox.checked };
await window.harness.setCaps(req);  // then setStatus + refreshSnapshot as today
```

- [ ] **Step 3: Compose scope selector**

Replace `#scope-input` + `initScope()` with: the same three radios
(`spawn-base`), `<details id="spawn-scope-tasks"><summary>対象タスクを選択 (0)</summary>…</details>`
checklist rebuilt on snapshot refresh (preserving checked state by id), and
a `<code id="spawn-scope-echo">`. `spawnScope` is now assigned
`scopeSpecJS(base, ids)` with the session rule (subtree+no-ids → `""`).
Summary count updates on toggle. Everything else (`sessionReq`) untouched.

- [ ] **Step 4: Gate + hot-reload check**

`make wasm-check` (wasm unchanged but the gate is cheap) and a dummy-harness
`--webui-dir` visual pass at desktop + 390px: dialog opens prefilled, echo
updates, cancel/Esc send nothing.

- [ ] **Step 5: Commit**

```bash
git add webui/
git commit -m "feat(webui): selection-based re-grant dialog and Compose scope picker"
```

---

### Task 4: Dummy-harness E2E — both surfaces, happy + deviation paths

**Files:** none (verification; screenshots kept at worktree root)

- [ ] **Step 1: Stand up** `scripts/dummy-harness.sh up --detach --agent fake --name PICK`,
  spawn 2–3 fake tasks so the checklists have rows.
- [ ] **Step 2: TUI, real keystrokes** (session + `stty rows 40 cols 160` +
  scrubbed env, as in the previous round): `a` → space-toggle a cap → j to
  the base row → space ×1 (none) → j to a task row → space → enter. Assert
  `caps set: 1 task(s) changed` and `ls` shows `scope=ids:…`. Then Esc
  mid-picker: snapshot shows the picker gone, `ls` unchanged.
- [ ] **Step 3: TUI session mode**: cmdline `scope` (no arg) opens the
  picker; select `none`; a subsequent `submit` via cmdline spawns with
  `scope=none` (verify in `ls`).
- [ ] **Step 4: WebUI, Playwright**: 再付与 → dialog prefilled (chips + ids
  checked) → change base to `none`, check one task, 適用 → status line +
  `ls` reflect it. キャンセル and Esc → no change. Compose: pick
  `subtree+ids` via the details checklist, spawn, `ls` shows it.
  Screenshots: `webui-authpicker-desktop.png`, `webui-authpicker-390.png`
  (dialog open), same pair for Compose if the layout changed visibly.
- [ ] **Step 5: Tear down** (`down --name PICK`), prune nothing (dummy dies
  whole).

---

### Task 5: Land

- [ ] **Step 1: Gates**: `make check && make wasm-check && make test`
  (wire-skew-check is a no-op — no `.bgn` touched — safe to skip).
- [ ] **Step 2: Mode A landing** (per `landing-policy-remote-agent-harness`):
  FF-safe check, `git push origin HEAD:main`, FF local main, then
  `make build` in the main checkout (mandatory part of landing).

## Self-review notes

- Spec coverage: §1 model+serialization → Task 1 (+ JS twin in Task 3);
  §2 WebUI dialog + Compose → Task 3; §3 TUI picker + two openers → Tasks
  1–2; §4 no-change list → Global Constraints; Testing → Tasks 1, 2, 4.
- The JS serializer is a deliberate twin of the Go one (two runtimes); both
  are pinned by tests/E2E to the same grammar, and the string round-trips
  through the single `cli.ParseScope` on every path, so drift fails loudly.
- WebUI prefill reads raw `capsBits`/`scopeBase`/`scopeIds` added to the
  wasm snapshot rows — an output-side addition; the input funnel
  (grammar string through `cli.ParseScope`) is untouched.
