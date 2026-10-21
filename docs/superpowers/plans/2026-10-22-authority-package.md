# Authority package (visualizer Phase 1) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Move the scope semantics into one package, `authority`, that the server enforces with and the CLI / TUI / wasm clients read with, so there is one implementation of "what a scope covers".

**Architecture:** A new leaf package `github.com/on-keyday/agent-harness/authority` takes the pure scope code out of `server/scope.go`, `server/capabilities.go` and `server/set_caps_handler.go`. The server keeps everything that resolves a CONNECTION to a task and the WAL projection, and calls the package for the rest. The clients' restatements (`cli/verb.scopeForCap`, `cli.reachOfDescendant`, the per-clause logic in `cli.BuildTaskGrants`) are rebuilt on it, pinned by characterization tests written before they change. No behaviour changes.

**Tech Stack:** Go 1.26; the `.bgn`-generated `runner/protocol` types; `make` targets; `GOOS=js GOARCH=wasm` for the WebUI bridge.

**Spec:** `docs/superpowers/specs/2026-10-22-authority-visualizer-design.md` (§ Phase 1, decisions V7–V9, V14). Read the Problem section (P3) as well as Phase 1.

## Global Constraints

- `authority` imports only `runner/protocol` and the standard library, and builds under `GOOS=js GOARCH=wasm` (`make wasm-check`).
- The types MOVE (V9): `server.Scope` / `server.ScopeOverride` become `authority.Scope` / `authority.ScopeOverride` at every use. No type alias, no wrapper type left in `server`.
- No behaviour change, on any surface: same decisions on every gated request, same WAL bytes, same `ls` / `whoami` / tree / caps-audit output. A changed output is a defect of this plan, not a fix.
- The WAL projection (`applyToWAL`, `scopeFromWAL`, including the legacy `board_observe` → global-visibility migration) stays in `server`.
- Work in the worktree `/home/kforfk/workspace/remote-agent-harness/.harness-worktrees/70fbad4a6eb6f1e992be8a669f1bcefd`; absolute paths under the bare repo root route to the parent checkout.
- Compile checks with `go build ./...` / `go vet ./...` (no binaries in the tree). Run `make test` with the task env scrubbed: `env -u HARNESS_SERVER_PUBKEY -u HARNESS_SERVER_CID -u HARNESS_AUTH_TICKET -u HARNESS_TASK_ID make test` — two integration tests fail otherwise for reasons unrelated to any diff.

## Review Focus

1. **Legacy WAL records** — a record with no scope keys, and one whose caps carry `board_observe` without `scope_vis_base_present`, must replay to the same scope as before (subtree; global visibility). Pinned in Task 1.
2. **Id normalisation on the client side** — clients now pass wire ids through `authority.ScopeFromWire`, which lower-cases, sorts and dedupes. Labels must not change order or content. Pinned by Task 4's characterization test.
3. **Tasks the listing does not contain** — a creator missing from the listing (orphan) and a scope id naming an unlisted task (`Offscreen`) must keep their current tree treatment. Pinned in Task 5's golden tree.
4. **Operator connections** — an operator creator still bypasses attenuation (any id, any base) and an operator caller still gets `all` / global. Pinned in Task 2.
5. **The exercised attach bits** (`0313d08c`) — moving `exercisedBits` must keep an `exec_control` holder's `exec_view` / `exec_cowrite` scopes bounded on spawn and cascade. Pinned by the existing `TestAttenuateScopeBoundsTheScopesAStrongerBitExercises` / `TestSetCapsCascadeKeepsOverridesOfExercisedBits`, which must still pass unchanged in `server`.

---

## File Structure

| File | Responsibility |
| --- | --- |
| `authority/scope.go` | `Scope`, `ScopeOverride`, `VisRank`, `ForCap`, `String`, `Validate`, `BaseRank`, `MinBase`, `NormalizeIDs`, `DefaultScope`, `CapsLabel`, `TaskIDHexLen` |
| `authority/wire.go` | `ScopeFromWire`, `(Scope) ToWire`, `(Scope) OverridesToWire`, the two id converters |
| `authority/sets.go` | `DescendantsOf`, `ActionSet`, `VisibleSet`, `CoversNewChild` |
| `authority/attach.go` | `AttachModeCap`, `AttachModeScopeCap`, `ExercisedBits` |
| `authority/reach.go` | `Reach`, `NewReach`, `(*Reach) Reaches`, `(*Reach) BoundRanks` |
| `authority/attenuate.go` | `Attenuate`, `ClampUnder`, `IsNarrowing` |
| `authority/*_test.go` | the moved pure tests, plus `no_copies_test.go` |
| `server/scope.go` | shrinks to the WAL projection (`applyScopeToWAL`, `scopeFromWAL`) |
| `server/capabilities.go` | connection resolution and the `attenuateScope` wrapper |
| `server/set_caps_handler.go` | `handleSetCaps`, calling `authority.ClampUnder` / `IsNarrowing` |
| `cli/verb/scope.go` | `scopeForCap` deleted; `ResolvedScopeByCap` on `authority` |
| `cli/tasktree.go` | `reachOfDescendant`, `BuildTaskGrants` on `authority` |
| `cli/testdata/task_grants_golden.json` | Task 5's characterization record |

---

### Task 1: `authority` scope types and wire conversion; server moves onto them

**Files:**
- Create: `authority/scope.go`, `authority/wire.go`, `authority/scope_test.go`
- Modify: `server/scope.go` (keeps only the WAL projection), every `server/*.go` and `server/*_test.go` that names `Scope` / `ScopeOverride` / the moved helpers, `server/scope_test.go`

**Interfaces:**
- Produces (exact names; bodies are the current server ones, moved):
  - `type Scope struct{ Base protocol.ScopeBase; IDs []string; VisBase protocol.ScopeBase; VisBasePresent, ExcludeSelf bool; VisIDs []string; Overrides []ScopeOverride }`
  - `type ScopeOverride struct{ Caps protocol.Capability; Base protocol.ScopeBase; ExcludeSelf bool; IDs []string }`
  - `func (s Scope) VisRank() protocol.ScopeBase`, `func (s Scope) ForCap(c protocol.Capability) (protocol.ScopeBase, bool, []string)`, `func (s Scope) String() string`
  - `func (s Scope) ToWire() protocol.TaskScope`, `func (s Scope) OverridesToWire() []protocol.ScopeOverride`, `func ScopeFromWire(w protocol.TaskScope, ov []protocol.ScopeOverride) Scope`
  - `func Validate(s Scope) error`, `func BaseRank(b protocol.ScopeBase) int`, `func MinBase(a, b protocol.ScopeBase) protocol.ScopeBase`, `func NormalizeIDs(ids []string) []string`, `func DefaultScope() Scope`, `func CapsLabel(m protocol.Capability) string`, `const TaskIDHexLen = 32`
- In `server`: `func applyScopeToWAL(s authority.Scope, ev *WALEvent)` (was the method `applyToWAL`) and `scopeFromWAL(ev WALEvent) authority.Scope`, unchanged otherwise.

- [ ] **Step 1: Move the pure tests first.** Move `TestMinScopeBaseRanksByPermissiveness`, `TestScopeWireRoundTripNormalises`, `TestScopeFromWireDropsMalformedIDs`, `TestScopeString`, `TestDefaultScopeIsTheZeroValue`, `TestVisRankFollowsBaseWhenAbsent`, `TestForCapPrefersTheOverride`, `TestValidateScopeRejections`, `TestValidateScopeAccepts` from `server/scope_test.go` into `authority/scope_test.go` (package `authority`), renaming the calls to the exported names above. `TestSetCapsRewritesRunningTaskAndPersistsBoth` stays in `server` (it needs the store).
- [ ] **Step 2: Add the legacy-replay pin** in `server/scope_test.go` (Review Focus 1), if not already covered:

```go
func TestScopeFromWALLegacyRecords(t *testing.T) {
	if got := scopeFromWAL(WALEvent{}); got.Base != protocol.ScopeBase_Subtree || got.VisBasePresent {
		t.Errorf("empty record = %+v, want the subtree default", got)
	}
	got := scopeFromWAL(WALEvent{Capabilities: uint32(protocol.Capability_BoardObserve)})
	if got.VisRank() != protocol.ScopeBase_Global {
		t.Errorf("legacy board_observe record vis = %v, want global", got.VisRank())
	}
}
```

- [ ] **Step 3: Run** `go test ./authority/` — Expected: FAIL to compile (package missing).
- [ ] **Step 4: Create `authority/scope.go` and `authority/wire.go`** by moving the declarations listed under Interfaces out of `server/scope.go`, exporting them, and keeping their doc comments. `server/scope.go` keeps the WAL pair only.
- [ ] **Step 5: Move every server reference.** Delete nothing else. Let the compiler list the sites (`go vet ./server 2>&1`): type uses become `authority.Scope` / `authority.ScopeOverride`; `s.toWire()` → `s.ToWire()`; `s.overridesToWire()` → `s.OverridesToWire()`; `s.applyToWAL(&ev)` → `applyScopeToWAL(s, &ev)`; `validateScope` → `authority.Validate`; and so on for every name in Interfaces. A struct-field KEY named `Scope:` is not a type use and stays.
- [ ] **Step 6: Run** `go vet ./server ./authority && go test ./authority/ ./server/ -count=1` — Expected: PASS.
- [ ] **Step 7: Run** `make wasm-check` — Expected: exit 0.
- [ ] **Step 8: Commit**

```bash
git add authority server
git commit -m "refactor: move the scope types into package authority"
```

---

### Task 2: target sets, attach ranks, reach and attenuation into `authority`

**Files:**
- Create: `authority/sets.go`, `authority/attach.go`, `authority/reach.go`, `authority/attenuate.go`, `authority/attenuate_test.go`
- Modify: `server/capabilities.go`, `server/set_caps_handler.go`, `server/taskstore_reach.go`, `server/taskstore.go`, `server/set_parent_handler.go`, `server/task_handler.go`, and the server tests calling the moved names (`server/capabilities_test.go`, `server/set_caps_test.go`, `server/open_seat_test.go`, `server/scope_percap_completeness_test.go`, `server/scope_attenuation_test.go`, `server/set_parent_test.go`)

**Interfaces:**
- Consumes: Task 1's types.
- Produces:
  - `func DescendantsOf(children map[string][]string, selfHex string, allowed map[string]bool)`
  - `func ActionSet(callerHex string, scope Scope, want protocol.Capability, children map[string][]string) (all bool, allowed map[string]bool)`
  - `func VisibleSet(callerHex string, scope Scope, children map[string][]string) (all bool, allowed map[string]bool)` (was `visibleSetOf`)
  - `func CoversNewChild(s Scope, c protocol.Capability) bool`
  - `func AttachModeCap(mode protocol.AttachMode) protocol.Capability`, `func AttachModeScopeCap(mode protocol.AttachMode) protocol.Capability`, `func ExercisedBits(caps protocol.Capability) protocol.Capability`
  - `type Reach struct{ /* unexported: hex, scope, children, vis sets, per-bit cache */ }`, `func NewReach(hex string, scope Scope, children map[string][]string) *Reach`, `func (r *Reach) Reaches(id string, bits protocol.Capability) bool`, `func (r *Reach) BoundRanks(s Scope, held protocol.Capability) Scope`
  - `func Attenuate(req Scope, childCaps protocol.Capability, creator *Reach) (out Scope, offender string, ok bool)` — `creator == nil` is an operator: normalise and `Validate` only. Otherwise the current body after the creator is resolved, with `creator.scope` in place of the looked-up scope.
  - `func ClampUnder(child Scope, childCaps protocol.Capability, parent *Reach) Scope` (was `clampScopeUnder(child, childCaps, parent TaskEntry, children)`; `parent.ID` / `parent.Scope` come from the `Reach`)
  - `func IsNarrowing(beforeCaps protocol.Capability, before Scope, afterCaps protocol.Capability, after Scope) bool`
- `server` keeps: `attenuateScope(cid, req, childCaps)` as the wrapper that resolves the principal (operator → `Attenuate(req, caps, nil)`; vanished creator → `NewReach(hex, Scope{Base: None}, …)`), `scopeSet`, `scopeSetWith`, `inScope`, `authorize`, `resumeTargetAllowed`, `controlSeatAllowed`, `resizeAllowed`, `resumedCaps`, `visibilitySet` (now `authority.VisibleSet(callerHex, scope, h.childIndex())`).

- [ ] **Step 1: Move the pure tests.** `TestClampScopeUnderCarriesEveryAxis`, `TestClampScopeUnderDropsOverridesForLostBits` (`server/capabilities_test.go`), `TestScopeOnlyNarrowingCountsAsNarrowing`, `TestPerBitAndVisibilityNarrowingCountsAsNarrowing` (`server/set_caps_test.go`) go to `authority/attenuate_test.go`, calling `ClampUnder(child, caps, NewReach(parent.ID, parent.Scope, nil))` and `IsNarrowing(...)`. Tests that need a `TaskHandler` or the store stay in `server`.
- [ ] **Step 2: Add the operator pin** to `authority/attenuate_test.go` (Review Focus 4):

```go
func TestAttenuateOperatorIsUnbounded(t *testing.T) {
	req := Scope{Base: protocol.ScopeBase_Global, IDs: []string{strings.Repeat("ab", 16)}}
	got, _, ok := Attenuate(req, protocol.Capability_All, nil)
	if !ok || got.Base != protocol.ScopeBase_Global || len(got.IDs) != 1 {
		t.Errorf("operator = %+v ok=%v, want the request unchanged", got, ok)
	}
	if _, off, ok := Attenuate(Scope{Overrides: []ScopeOverride{{Caps: protocol.Capability_Spawn}}}, protocol.Capability_All, nil); ok || !strings.HasPrefix(off, "invalid scope: ") {
		t.Errorf("operator incoherent scope: ok=%v offender=%q, want refused", ok, off)
	}
}
```

- [ ] **Step 3: Run** `go test ./authority/` — Expected: FAIL to compile.
- [ ] **Step 4: Move the functions** listed under Produces into the four files, exporting them with their doc comments; `server` calls the package. `grantReach` becomes `Reach`; its comments' references to `attenuateScope` / `clampScopeUnder` name `Attenuate` / `ClampUnder`.
- [ ] **Step 5: Run** `go vet ./... && go test ./authority/ ./server/ -count=1` — Expected: PASS, including every test named in Review Focus 5.
- [ ] **Step 6: Run** `make wasm-check` — Expected: exit 0.
- [ ] **Step 7: Commit**

```bash
git add authority server
git commit -m "refactor: target sets, reach and attenuation into package authority"
```

---

### Task 3: the single-implementation guard

**Files:**
- Create: `authority/no_copies_test.go`

**Interfaces:**
- Consumes: the exported names of Tasks 1–2.

- [ ] **Step 1: Write `TestNoSecondScopeImplementation`** in the manner of `cli/verb`'s `TestNoHandWrittenVerbFlagSetsRemain`: walk `../server`, `../cli` (recursively), `../tui`, `../cmd` for non-test `.go` files and fail when one declares a function named any of `scopeForCap`, `reachOfDescendant`, `actionSet`, `visibleSetOf`, `descendantsOf`, `clampScopeUnder`, `isNarrowing`, `minScopeBase`, `scopeBaseRank`, `validateScope`, `exercisedBits`, `attachModeScopeCap` (match `func <name>(` and `) <name>(`). The message names the file and says to call `authority` instead.
- [ ] **Step 2: Run** `go test ./authority/ -run TestNoSecondScopeImplementation -v` — Expected: FAIL naming `cli/verb/scope.go` (`scopeForCap`) and `cli/tasktree.go` (`reachOfDescendant`) — the two copies Tasks 4–5 remove. (Every server name is gone after Task 2.)
- [ ] **Step 3: Leave it uncommitted.** It is committed with Task 5, the commit that removes the last copy, so every commit on the branch stays green.

---

### Task 4: `cli/verb` on `authority`

**Files:**
- Modify: `cli/verb/scope.go` (delete `scopeForCap`; `ResolvedScopeByCap` resolves through `authority`), `cli/verb/scope_test.go` (or the file holding `ResolvedScopeByCap`'s tests)

**Interfaces:**
- Consumes: `authority.ScopeFromWire`, `(authority.Scope).ForCap`, `(authority.Scope).ToWire`.
- Produces: `ResolvedScopeByCap(caps, base, overrides) map[string]string` with an unchanged signature and output.

- [ ] **Step 1: Write the characterization test `TestResolvedScopeByCapUnchanged`** BEFORE touching the code: for a table of (caps, scope, overrides) — no overrides; one override with ids; two disjoint overrides; `exclude_self` on base and on an override; a `global/subtree` pair; ids given in unsorted order — record today's output as literal expected maps (run the current code once to fill them in, then paste).
- [ ] **Step 2: Run** `go test ./cli/verb/ -run TestResolvedScopeByCapUnchanged -v` — Expected: PASS on the current code.
- [ ] **Step 3: Replace `scopeForCap`** with `s := authority.ScopeFromWire(base, overrides); b, ex, ids := s.ForCap(bit)` and render `ScopeLabel(authority.Scope{Base: b, ExcludeSelf: ex, IDs: ids}.ToWire())`.
- [ ] **Step 4: Run** `go test ./cli/... -count=1 && make wasm-check` — Expected: PASS, the characterization test included.

---

### Task 5: `cli/tasktree` on `authority`; guard goes green

**Files:**
- Modify: `cli/tasktree.go` (`reachOfDescendant`, `BuildTaskGrants`), `cli/tasktree_test.go`
- Create: `cli/testdata/task_grants_golden.json`

**Interfaces:**
- Consumes: `authority.ScopeFromWire`, `authority.ActionSet`, `authority.VisibleSet`, `(authority.Scope).ForCap`.
- Produces: `BuildTaskGrants(tasks []protocol.TaskInfo) TaskGrants` — signature and output unchanged.

- [ ] **Step 1: Write `TestBuildTaskGrantsGolden`** BEFORE touching the code. A fixed tree of ≥ 8 tasks covering: subtree / none / global bases; `global/subtree` and `none/global` visibility pairs; overrides with and without ids; `exclude_self`; an id naming a sibling, an ancestor, the task itself, and an unlisted task (Offscreen); an orphan whose creator is not listed; an `exec_control`-only holder. The test marshals `BuildTaskGrants(tree)` to JSON and compares with `cli/testdata/task_grants_golden.json`; with `-update` it writes the file.
- [ ] **Step 2: Run** `go test ./cli/ -run TestBuildTaskGrantsGolden -update` on the CURRENT code, then without `-update` — Expected: PASS. Commit the golden file with the test now, so the record predates the change:

```bash
git add cli/tasktree_test.go cli/testdata/task_grants_golden.json
git commit -m "test(cli): record BuildTaskGrants output before it moves onto authority"
```

- [ ] **Step 3: Rebuild `reachOfDescendant(t, target)`** on `authority`: convert `t` with `ScopeFromWire`, build a one-edge creator index `{t: [target]}`, and for each LITERALLY held bit (Phase 1 keeps today's literal reading; V14's exercised reading is Phase 3) test `ActionSet(tHex, s, bit, idx)` for `target`; `Visible` from `VisibleSet` with the same index.
- [ ] **Step 4: Rebuild the per-target clause resolution in `BuildTaskGrants`** on `(authority.Scope).ForCap`: for each held bit, the clause `ForCap` returns decides whether a named id is an `action` (base clause) or `override` (override clause) target; view-only ids from `VisIDs`; global from the resolved bases. Edge kinds, the `globalCaps` suppression and `Offscreen` counting keep their current rules.
- [ ] **Step 5: Run** `go test ./cli/ ./authority/ -count=1` — Expected: PASS: the golden test unchanged, the existing `TestBuildTaskGrants_*` tests, and Task 3's `TestNoSecondScopeImplementation` now green.
- [ ] **Step 6: Commit**

```bash
git add cli authority/no_copies_test.go
git commit -m "refactor(cli): task tree grants and per-bit scopes on package authority"
```

---

### Task 6: verify and land

- [ ] **Step 1: Run** `make vet && env -u HARNESS_SERVER_PUBKEY -u HARNESS_SERVER_CID -u HARNESS_AUTH_TICKET -u HARNESS_TASK_ID make test && make wasm-check` — Expected: all exit 0.
- [ ] **Step 2: Live check** against `scripts/dummy-harness.sh up --detach --agent fake` (unset `HARNESS_SERVER_PUBKEY` after `eval "$(scripts/dummy-harness.sh env)"`): spawn an agent with `--caps spawn,cancel --scope subtree --scope-for cancel=none` that spawns a `--caps cancel,file_read` child; `ls` shows the child as `subtree +cancel:none` (same as before the move). Open the WebUI 🌳 tree on the same instance and confirm the creator-edge label and arrows render for the pair (screenshot kept, per the UI-screenshot rule). `down` afterwards.
- [ ] **Step 3: Land** per `landing-to-main` Mode A: rebase onto local `main`, FF-push the code commits (not the unlanded spec commits), FF the main checkout to the pushed SHA by SHA (not `HEAD~n`, which resolves in the main checkout's own context), then `make build` there.
