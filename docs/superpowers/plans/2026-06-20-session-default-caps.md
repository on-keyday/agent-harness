# Session-default capabilities (TUI/WebUI) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let TUI and WebUI operators pre-set a per-session default capability set that every spawn from that session uses, without typing `--caps` each time.

**Architecture:** Each client holds an in-memory `sessionCaps` (default `Capability_All`) and passes it as `RequestedCaps` on every spawn via the existing `*WithSelectorArgsAndCaps` builders. Server enforcement is unchanged (it intersects with the operator's full caps). Cap-name parsing/listing is extracted from `cmd/harness-cli` into the shared `cli` package so CLI, TUI, and WebUI use one source.

**Tech Stack:** Go, Bubble Tea (TUI), Go/wasm + vanilla JS/CSS (WebUI), existing `protocol.Capability` + `cli` client.

**Spec:** `docs/superpowers/specs/2026-06-20-session-default-caps-design.md`

## Global Constraints

- **Client-side only; no server/wire/schema change.** Enforcement stays in the landed capability system; this feature only sets `RequestedCaps` from client state.
- **Default semantics, session lifetime.** `sessionCaps` is what spawns use; no per-spawn override UI; in-memory only (not persisted).
- **One source for cap names: `Capability.String()`.** No duplicate name→bit literal maps. The shared `cli` helpers are the only parser/list.
- **`sessionCaps` is passed as a parameter**, never read from global state inside a helper.
- **WebUI matches the dark palette (#1e1e1e / #d4d4d4) and is usable at ≤600px (verify 390px)**; terminal/text stays readable. Verify desktop + 390px in Playwright.
- **Build hygiene:** verify with `make check` (builds TUI + wasm + all) and focused `go test`. NEVER bare `go build ./cmd/<x>/`. Commit only intended files (no `git add -A`; the worktree has pre-existing untracked noise — ignore it).
- Public repo: no local env / IPs / private paths in any new content.

---

## File Structure

- `cli/caps.go` (new, NO build tag — must be wasm-safe so WebUI can use it): `ParseCaps`, `GrantableCaps`, `CapNames` (exported; moved verbatim from `cmd/harness-cli/caps.go`).
- `cli/caps_test.go` (new): the relocated parser test.
- `cmd/harness-cli/caps.go`: deleted; callers use `cli.ParseCaps`.
- `cmd/harness-cli/caps_test.go`: deleted (moved to `cli`).
- `cmd/harness-cli/main.go`, `cmd/harness-cli/session.go`: call `cli.ParseCaps`.
- `tui/app.go`: `App.sessionCaps` field + handle the `caps` Action.
- `tui/cmdline.go`: `CapsAction` + parse `caps` command.
- `tui/client.go`, `tui/interactive.go`: thread `sessionCaps` into spawn helpers.
- `cmd/harness-webui-wasm/main.go`: expose `harnessCapList`; accept `caps` in `harnessSubmit` + `harnessStartInteractive`.
- `webui/static/main.js`, `webui/static/style.css`: chip row + readout + `spawnCaps` state.

---

## Task 1: Extract cap parsing into the shared `cli` package

**Files:**
- Create: `cli/caps.go`, `cli/caps_test.go`
- Delete: `cmd/harness-cli/caps.go`, `cmd/harness-cli/caps_test.go`
- Modify: `cmd/harness-cli/main.go` (calls `parseCaps`), `cmd/harness-cli/session.go` (calls `parseCaps`)

**Interfaces:**
- Produces: `cli.ParseCaps(s string) (protocol.Capability, error)`; `cli.GrantableCaps() []protocol.Capability`; `cli.CapNames(caps []protocol.Capability) []string`.

- [ ] **Step 1: Create `cli/caps.go`** (no build tag) with the three functions moved from `cmd/harness-cli/caps.go`, exported, and `grantableCaps` becoming a function `GrantableCaps()` returning the slice:

```go
package cli

import (
	"fmt"
	"strings"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// GrantableCaps lists the individual capability values a --caps flag (or a UI)
// may name. Names come from Capability.String() — the single source so they
// never drift from the enum.
func GrantableCaps() []protocol.Capability {
	return []protocol.Capability{
		protocol.Capability_None,
		protocol.Capability_Spawn,
		protocol.Capability_Cancel,
		protocol.Capability_ExecAttach,
		protocol.Capability_FileRead,
		protocol.Capability_FileWrite,
		protocol.Capability_ForwardLocal,
		protocol.Capability_ForwardRemote,
		protocol.Capability_Notify,
		protocol.Capability_Prune,
		protocol.Capability_RunnerAdmin,
		protocol.Capability_InfoGlobal,
		protocol.Capability_All,
	}
}

// CapNames returns the string representation of each capability.
func CapNames(caps []protocol.Capability) []string {
	names := make([]string, len(caps))
	for i, c := range caps {
		names[i] = c.String()
	}
	return names
}

// ParseCaps converts a comma-separated list of capability names into a bitmask.
// Empty/whitespace → Capability_All (inherit-all); unknown name → error.
func ParseCaps(s string) (protocol.Capability, error) {
	if strings.TrimSpace(s) == "" {
		return protocol.Capability_All, nil
	}
	grantable := GrantableCaps()
	byName := make(map[string]protocol.Capability, len(grantable))
	for _, c := range grantable {
		byName[c.String()] = c
	}
	var out protocol.Capability
	for _, name := range strings.Split(s, ",") {
		name = strings.TrimSpace(name)
		c, ok := byName[name]
		if !ok {
			return 0, fmt.Errorf("unknown capability %q (valid: %s)",
				name, strings.Join(CapNames(grantable), ", "))
		}
		out |= c
	}
	return out, nil
}
```

- [ ] **Step 2: Create `cli/caps_test.go`** by moving the body of `cmd/harness-cli/caps_test.go`, renaming `parseCaps`→`cli.ParseCaps` (in-package, so just `ParseCaps`), package `cli`:

```go
package cli

import (
	"testing"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

func TestParseCaps(t *testing.T) {
	if got, err := ParseCaps(""); err != nil || got != protocol.Capability_All {
		t.Fatalf("empty = %#x,%v want All", got, err)
	}
	if got, err := ParseCaps("   "); err != nil || got != protocol.Capability_All {
		t.Fatalf("ws = %#x,%v want All", got, err)
	}
	got, err := ParseCaps("spawn,file_read")
	if err != nil || got != (protocol.Capability_Spawn|protocol.Capability_FileRead) {
		t.Fatalf("got %#x,%v", got, err)
	}
	if _, err := ParseCaps("bogus"); err == nil {
		t.Fatal("expected error for unknown cap")
	}
}
```

- [ ] **Step 3: Delete the old files.** `git rm cmd/harness-cli/caps.go cmd/harness-cli/caps_test.go`.

- [ ] **Step 4: Update callers in `cmd/harness-cli`.** In `main.go` and `session.go`, replace each `parseCaps(...)` call with `cli.ParseCaps(...)` (the `cli` package is already imported there; if not, add the import). Compile will list every reference.

Run: `grep -rn "parseCaps(" cmd/harness-cli/` → expect only `cli.ParseCaps(` after edits.

- [ ] **Step 5: Run tests + compile.**

Run: `go test ./cli/ -run TestParseCaps -v && go test ./cmd/harness-cli/ && make check`
Expected: PASS; `cmd/harness-cli` still builds and its `--caps` flag works.

- [ ] **Step 6: Commit.**

```bash
git add cli/caps.go cli/caps_test.go cmd/harness-cli/main.go cmd/harness-cli/session.go
git commit -m "refactor(cli): extract ParseCaps/GrantableCaps to shared cli package"
```

---

## Task 2: TUI `caps` command + session default

**Files:**
- Modify: `tui/cmdline.go` (add `CapsAction` + `case "caps"`), `tui/app.go` (`App.sessionCaps` + handle action + thread into spawns), `tui/client.go` (`DoSubmit*` caps param), `tui/interactive.go` (`DoOpenInteractive*` / `DoOpenDetachableSession` caps param)
- Test: `tui/cmdline_test.go` (or the nearest existing cmdline test file)

**Interfaces:**
- Consumes: `cli.ParseCaps`, `cli.GrantableCaps`, `cli.CapNames`, `protocol.Capability`, and the `*WithSelectorArgsAndCaps` builders.
- Produces: `CapsAction{ Caps protocol.Capability; Show bool }`; `App.sessionCaps protocol.Capability`.

- [ ] **Step 1: Write the command-parse test** in `tui/cmdline_test.go`:

```go
func TestParseCapsCommand(t *testing.T) {
	act, err := ParseCommand("caps spawn,file_read", "repo")
	if err != nil {
		t.Fatal(err)
	}
	ca, ok := act.(CapsAction)
	if !ok {
		t.Fatalf("got %T, want CapsAction", act)
	}
	if ca.Show {
		t.Fatal("with args, Show should be false")
	}
	if ca.Caps != (protocol.Capability_Spawn | protocol.Capability_FileRead) {
		t.Fatalf("caps = %#x", ca.Caps)
	}
	// no args → Show
	act, _ = ParseCommand("caps", "repo")
	if ca, _ := act.(CapsAction); !ca.Show {
		t.Fatal("no args → Show=true")
	}
	// bad name → error
	if _, err := ParseCommand("caps bogus", "repo"); err == nil {
		t.Fatal("expected error for unknown cap")
	}
}
```

- [ ] **Step 2: Run (fails — CapsAction undefined).** `go test ./tui/ -run TestParseCapsCommand -v`

- [ ] **Step 3: Add `CapsAction` + the parse case** in `tui/cmdline.go`. Define the type next to the other `*Action` types (each implements `isAction()`):

```go
type CapsAction struct {
	Caps protocol.Capability
	Show bool // true = display current set (no args), false = set to Caps
}

func (CapsAction) isAction() {}
```

In `ParseCommand`'s switch (`:182`-area), add:

```go
	case "caps":
		if len(tokens) == 1 {
			return CapsAction{Show: true}, nil
		}
		c, err := cli.ParseCaps(strings.Join(tokens[1:], ""))
		if err != nil {
			return nil, err
		}
		return CapsAction{Caps: c}, nil
```

(`tokens[1:]` joined with "" so `caps spawn, file_read` and `caps spawn,file_read` both work; `ParseCaps` trims per-name. Ensure `protocol` and `cli` are imported in cmdline.go.)

- [ ] **Step 4: Add `sessionCaps` to the model + initialize.** In `tui/app.go` `type App struct` (`:30`) add:

```go
	sessionCaps protocol.Capability // default caps applied to spawns from this TUI session
```

In the App constructor (where the struct is initialized — find `App{` literal / `NewApp`), set `sessionCaps: protocol.Capability_All`.

- [ ] **Step 5: Handle `CapsAction`** in the Update type-switch where `ParseCommand` results are dispatched (around `tui/app.go:707`, mirror how an existing state-changing action like the `repo` command sets state + a status line). Add a case:

```go
	case CapsAction:
		if act.Show {
			a.setStatus("caps: " + capsLabel(a.sessionCaps)) // see Step 7 for capsLabel
		} else {
			a.sessionCaps = act.Caps
			a.setStatus("caps set: " + capsLabel(a.sessionCaps))
		}
		return a, nil
```

Use whatever the existing status mechanism is (find how command errors / `repo` confirmations are surfaced — e.g. a `a.setStatus(...)` / status field; match it exactly). Parse errors from `ParseCommand` are already surfaced by the existing error path at `:707`.

- [ ] **Step 6: Thread `sessionCaps` into spawns.** Add a trailing `caps protocol.Capability` parameter to the TUI spawn helpers and pass `a.sessionCaps` at each call site:
  - `tui/client.go`: `DoSubmit`, `DoSubmitWithOpts` → call `c.SubmitWithSelectorArgsAndCaps(ctx, repo, prompt, sel, extraArgs, resumeTaskID, caps)`.
  - `tui/interactive.go`: `DoOpenInteractive`, `DoOpenInteractiveWithHost`, `DoOpenInteractiveWithOpts`, `DoOpenDetachableSession` → call `c.OpenInteractiveWithSelectorArgsAndCaps(..., caps)`; the X11 branch → `c.OpenInteractiveX11(..., caps)` (replace the hardcoded `protocol.Capability_All` at `tui/interactive.go:91`).
  - Call sites in `tui/app.go` (`:496`, `:588`, `:593`, `:651`, `:1007`, and any other `DoSubmit*`/`DoOpenInteractive*`/`DoOpenDetachableSession` call): pass `a.sessionCaps`.

- [ ] **Step 7: Add `capsLabel`** (TUI-local helper, e.g. in `tui/cmdline.go` or `tui/app.go`): comma-join the enabled granular caps, collapse to `all`/`none`:

```go
func capsLabel(c protocol.Capability) string {
	if c == protocol.Capability_All {
		return "all"
	}
	if c == protocol.Capability_None {
		return "none"
	}
	var names []string
	for _, g := range cli.GrantableCaps() {
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

- [ ] **Step 8: Run tests + compile.**

Run: `go test ./tui/ -run 'TestParseCapsCommand' -v && go test ./tui/ && make check`
Expected: PASS; TUI builds.

- [ ] **Step 9: Commit.**

```bash
git add tui/cmdline.go tui/cmdline_test.go tui/app.go tui/client.go tui/interactive.go
git commit -m "feat(tui): caps command sets a session-default capability set for spawns"
```

---

## Task 3: WebUI wasm — expose cap list + accept caps on spawn

**Files:**
- Modify: `cmd/harness-webui-wasm/main.go` (add `harnessCapList`; add `caps` to `harnessSubmit` + `harnessStartInteractive`)
- Test: manual via Task 4 Playwright (wasm has no unit tests; verify with `make check` / `wasm-check`)

**Interfaces:**
- Consumes: `cli.GrantableCaps`, `protocol.Capability`, `*WithSelectorArgsAndCaps` builders.
- Produces: JS globals — `harness.capList()` → array of `{name: string, bit: number}` (granular caps only); `harness.submit(opts)` and `harness.startInteractive(opts)` read `opts.caps` (a number bitmask; absent → all).

- [ ] **Step 1: Add `harnessCapList`** in `cmd/harness-webui-wasm/main.go` and register it in the `js.Global().Set("harness", ...)` map (`:66`):

```go
"capList": js.FuncOf(harnessCapList),
```

```go
// harnessCapList returns the granular caps as [{name, bit}] for the UI chips
// (excludes none/all — those are quick-set buttons). Names from Capability.String().
func harnessCapList(this js.Value, args []js.Value) any {
	var out []any
	for _, c := range cli.GrantableCaps() {
		if c == protocol.Capability_None || c == protocol.Capability_All {
			continue
		}
		out = append(out, map[string]any{"name": c.String(), "bit": float64(uint32(c))})
	}
	return js.ValueOf(out)
}
```

- [ ] **Step 2: Read `opts.caps` in `harnessSubmit`.** In `harnessSubmit` (`:276`), after reading the other opts and before the spawn, compute caps and use the caps-bearing builder:

```go
			caps := protocol.Capability_All
			if cv := opts.Get("caps"); cv.Type() == js.TypeNumber {
				caps = protocol.Capability(uint32(cv.Int()))
			}
			id, err := c.SubmitWithSelectorArgsAndCaps(rootCtx, repo, task, sel, extraArgs, resumeTaskID, caps)
```

(replaces the current `c.SubmitWithSelectorAndArgs(...)` call.)

- [ ] **Step 3: Read `opts.caps` in `harnessStartInteractive`.** Find `harnessStartInteractive` (the handler behind `startInteractive`, near `:624`) and apply the same pattern: read `opts.caps` (number → `protocol.Capability`, absent → All) and call `c.InteractiveWithSelectorArgsAndCaps(..., caps)` (the wasm interactive entry point — match its existing signature, adding the caps arg via the `...AndCaps` variant).

- [ ] **Step 4: Compile-check both builds.**

Run: `make wasm-check && make check`
Expected: wasm + native build clean.

- [ ] **Step 5: Commit.**

```bash
git add cmd/harness-webui-wasm/main.go
git commit -m "feat(webui-wasm): expose capList; accept caps on submit/startInteractive"
```

---

## Task 4: WebUI frontend — cap chips + effective-set readout

**Files:**
- Modify: `webui/static/main.js` (chip row, readout, `spawnCaps` state, pass to spawn calls), `webui/static/style.css` (chip styling)
- Test: Playwright (per WebUI conventions)

**Interfaces:**
- Consumes: `harness.capList()`, `harness.submit({..., caps})`, `harness.startInteractive({..., caps})` (Task 3).

- [ ] **Step 1: Add `spawnCaps` state + render chips.** In `webui/static/main.js`, near the spawn/new-session form, add a module-level `let spawnCaps` initialized to the OR of all `harness.capList()` bits (= all granular bits on, matching `Capability_All`'s granular subset). After wasm is ready (the same place other `harness.*` calls become available), build the chip row:
  - `[all]` button → set `spawnCaps` to OR of all bits; `[none]` → `spawnCaps = 0`.
  - one toggle chip per `harness.capList()` entry: clicking flips that `bit` in `spawnCaps`.
  - re-render chip on/off state + the readout on every change.

```js
let capDefs = [];      // [{name, bit}]
let spawnCaps = 0;     // bitmask
function initCaps() {
  capDefs = harness.capList();
  spawnCaps = capDefs.reduce((m, c) => m | c.bit, 0); // all granular bits on
  renderCaps();
}
function toggleCap(bit) { spawnCaps ^= bit; renderCaps(); }
function setAllCaps(on) { spawnCaps = on ? capDefs.reduce((m,c)=>m|c.bit,0) : 0; renderCaps(); }
function capsLabel() {
  const allBits = capDefs.reduce((m,c)=>m|c.bit,0);
  if (spawnCaps === allBits) return "all";
  if (spawnCaps === 0) return "none";
  return capDefs.filter(c => (spawnCaps & c.bit) === c.bit).map(c => c.name).join(",");
}
function renderCaps() { /* update chip .on classes + readout textContent; see Steps 2-3 */ }
```

- [ ] **Step 2: Markup + readout.** Insert a container (e.g. `<div id="caps-row">`) into the spawn form area. `renderCaps()` populates it: an `[all]`/`[none]` pair, the chips (each a `<button class="cap-chip" data-bit=...>name</button>` with an `.on` class when set), and a readout `<span id="caps-readout">caps: <label></span>` showing `capsLabel()`. Wire chip clicks to `toggleCap(bit)` and the buttons to `setAllCaps(true/false)`.

- [ ] **Step 3: Pass `caps` on spawn.** Where main.js calls `harness.submit({...})` and `harness.startInteractive({...})`, add `caps: spawnCaps` to the opts object.

- [ ] **Step 4: Style (dark + mobile).** In `webui/static/style.css` add `.cap-chip` styling matching the palette: off = muted bg (e.g. `#2a2a2a` text `#888`), on = accent (reuse an existing accent var/color in style.css; e.g. the color used by the active tab/button), small radius, padding, `cursor:pointer`. `#caps-row` uses `display:flex; flex-wrap:wrap; gap:` so chips wrap on narrow screens. `#caps-readout` muted monospace. Confirm usable at 390px (chips wrap, tap targets ≥ ~28px high).

- [ ] **Step 5: Build + Playwright verify.** `make webui-build` (rebuild wasm+static is served; per WebUI conventions a browser refresh suffices, no server restart). Drive via Playwright (URL from `HARNESS_SERVER_CID`):
  - Load the page; assert the chip row renders with all chips `.on` and readout `caps: all`.
  - Click `[none]` → all chips off, readout `caps: none`. Toggle `spawn` + `file_read` → readout `caps: spawn,file_read`.
  - Spawn a task with that set; assert the created task is confined — e.g. via the task's attribution/snapshot, or by attempting a control-plane op from it and seeing denial; at minimum assert the submit call carried `caps: spawnCaps` (network/console check).
  - Verify at desktop width and resized to 390px: chips wrap, readout legible, dark palette intact. Screenshot both.

- [ ] **Step 6: Commit.**

```bash
git add webui/static/main.js webui/static/style.css
git commit -m "feat(webui): cap chips + effective-set readout for session-default caps"
```

---

## Self-Review

**Spec coverage:**
- Shared parser extraction (`cli.ParseCaps`/`GrantableCaps`/`CapNames`) → Task 1. ✓
- TUI `caps` command + `sessionCaps` + spawn threading → Task 2. ✓
- WebUI flat toggle chips + `[all]`/`[none]` + effective-set readout (collapse all/none) → Task 4 (chips/readout) + Task 3 (capList + caps on spawn). ✓
- Default semantics, session lifetime, no per-spawn override, no persistence → respected (in-memory state, no config). ✓
- Server/wire unchanged; client passes RequestedCaps via existing `*AndCaps` builders → Tasks 2-3. ✓
- Names single-sourced from `Capability.String()` → Task 1 (`CapNames`), reused everywhere. ✓
- Dark/mobile WebUI + Playwright → Task 4. ✓

**Type consistency:** `cli.ParseCaps`/`GrantableCaps`/`CapNames`, `CapsAction{Caps,Show}`, `App.sessionCaps`, `capsLabel`, `harness.capList()`/`{name,bit}`, `opts.caps` (number), `*WithSelectorArgsAndCaps` builders — names consistent across tasks. WebUI chip set and TUI `capsLabel` both exclude None/All from the granular list, matching.

**Placeholder scan:** none — every code step has concrete content; the one judgment point (TUI status mechanism / App constructor location) is anchored to "mirror the existing `repo` action / `App{` literal".
