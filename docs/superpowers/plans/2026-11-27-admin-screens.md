# Screens for actions and runner administration — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Put actions (define / edit / delete / list / run / pick in a grant condition) and runner administration (enroll / keys / unenroll / kick / dial records / dial once) on the TUI's and the WebUI's own controls, give the TUI per-capability override editing, and make "typable on a UI's command input ⇒ has a screen there" a test.

**Architecture:** One set of plain view structs in `cli` (JSON-tagged) is the data both screens render and the wasm bridge marshals; every write still goes through the existing `cli` funnels (`RunActionPut`, `RunnerEnrollWith`, …), so no wire or server change except one `actiondef` validation rule. WebUI screens are `<dialog>`s following `#credentials-modal`, with their pure logic in a new `webui/static/admin_models.js` tested under `node --test`. TUI screens are overlays following `ExecsModal`, each named by a `ModalSurfaces` row.

**Tech Stack:** Go (bubbletea / bubbles: table, textinput), Go→wasm bridge (`cmd/harness-webui-wasm`), vanilla JS, `node --test` with the real wasm (`webui/static/harness_env.mjs`).

**Spec:** `docs/superpowers/specs/2026-11-27-admin-screens-actions-runner-keys-design.md`

## Global Constraints

- Do not land, push or `make build`. Stage by explicit path only; `git show --stat` after each commit.
- Tests: `make test` (a bare `go test` needs the `env -u HARNESS_*` prefix in `.superpowers/sdd/2026-11-27-actions/gotestenv`); `make js-test` rebuilds `main.wasm` first.
- Results go to the result surface: WebUI `appendCmdOutput`, TUI `cmdresult`; never the connection badge. A result names its target and change; a zero is printed, not elided.
- List rows and result lines are the CLI's text (`cli.Format*`, `cli.ActionTemplateString`); a screen never writes a second wording.
- WebUI capability bit operations only through `capHas` / `capOr` / `capXor`. Dialog width derives from the viewport (`.picker-modal` rules); usable at 390 px with no horizontal page scroll.
- A destructive row action asks first. Force is offered only after a plain request was refused `in_use`, and the second confirmation lists what the refusal named (the runners on the key for unenroll, the runner's tasks for kick).
- The server refuses what the screens must make unbuildable: a template whose element 0 is a param, a declared param no element references, a choice param with no choices, a choice containing `,` (Task 1), a dial record without a key, override rows whose bits overlap.
- No `NoModalSurface` reason may be added for a verb typable on the TUI or WebUI command input (Task 2's rule enforces it).

## Review Focus

1. An edit saved after someone else edited the same action: `stale_revision` must show the CLI's text and keep the form open with what was typed (Tasks 4, 7).
2. A regrant through the TUI picker on a task with a conditioned override the operator did not touch: the override and its condition body arrive unchanged (Task 8).
3. A grant condition naming an action that has since been deleted: the checklist shows "deleted <id8>", keeps it checked, and serializes it unchanged until unchecked (Tasks 5, 8).
4. No actions defined: the run-action form and the condition checklist say so ("no actions — define one in Actions") instead of an empty select (Tasks 4, 5, 7, 8).
5. Unenroll / kick of a busy key / runner: the first click never forces; force appears only after `in_use`, listing the runners / tasks (Tasks 6, 9).

---

### Task 1: `actiondef` comma rule, quoted choices, structured template input

**Files:**
- Modify: `actiondef/actiondef.go` (choice validation in `Validate`), `actiondef/actiondef_test.go`
- Modify: `cli/actions.go` (`FormatActions` choice display; `ActionPutInput.Elements`; `RunActionPut`), `cli/actions_test.go`
- Modify: `docs/superpowers/specs/2026-11-26-grant-conditions-and-bound-enrollment-design.md` (C12 row: choices may not contain `,`)

**Interfaces:**
- Produces: `ActionPutInput.Elements []protocol.ActionElement` — when non-nil, `RunActionPut` uses it as the template and `Template` must be empty (both set → error `action <verb>: give the command as words or as elements, not both`).

- [ ] **Step 1: Failing tests**

```go
func TestValidateRefusesCommaInChoice(t *testing.T) {
	def := choiceDef(t, "env", "staging", "a,b") // helper: one choice param, template {"run", {env}}
	if err := Validate(&def); err == nil || !strings.Contains(err.Error(), ",") {
		t.Fatalf("a choice containing ',' must be refused naming it: %v", err)
	}
}
func TestFormatActionsQuotesChoices(t *testing.T) {
	got := FormatActions([]protocol.ActionRecord{recWithChoices(t, "a|b", "c")})
	if !strings.Contains(got, `"a|b" "c"`) {
		t.Fatalf("choices must be quoted, space-separated:\n%s", got)
	}
}
func TestRunActionPutTakesElements(t *testing.T) // via the execRunBody-style builder: an ActionPutInput with Elements
                                                 // {literal "{x}"} builds a definition whose element 0 is the literal "{x}"
                                                 // (no ParseTemplate escaping); Template and Elements both set → the error above.
```

For the third test, split the definition-building half of `RunActionPut` into `func buildActionDefinition(in ActionPutInput, name, desc, verb string) (protocol.ActionDefinition, error)` and test that.

- [ ] **Step 2: Run, watch them fail** — `make test` (or the gotestenv prefix with `-run 'TestValidateRefusesCommaInChoice|TestFormatActionsQuotesChoices|TestBuildActionDefinition' ./actiondef/ ./cli/`). Expected: 3 FAIL.
- [ ] **Step 3: Implement** the rule in `Validate` beside the existing choice checks (message: `param %q: choice %q contains ',', which --param cannot write`), `%q` choices joined by a space in `FormatActions`, and `Elements` in `buildActionDefinition`.
- [ ] **Step 4: Run** — the three pass; `make test` green.
- [ ] **Step 5: Commit** `actiondef, cli: refuse ',' in a choice; quote choices in action ls; structured template input`.

### Task 2: The command-line-implies-screen rule

**Files:**
- Modify: `cli/verb/verb.go` (`VerbSpec.LineCommand bool`, documented beside `NoModalSurface`)
- Modify: `cli/verb/table.go` (set `LineCommand: true` on the line's own commands; `preview` drops `NoModalSurface` and declares `{Surface: WebUI, At: "webui/index.html#session-preview-modal"}`)
- Modify: `cli/verb/modal_surface_test.go` (the rule, the list)

**Interfaces:**
- Produces: `var screenGaps = map[string]Surface{ "<verb path joined by space>": <surfaces lacking a screen>, … }` in `modal_surface_test.go`. Tasks 4–9 delete entries (or bits) from it as they add `ModalSurfaces`.

- [ ] **Step 1: Failing test**

```go
// For each S in {TUI, WebUI}: S in CmdlineSurfaces ⇒ a ModalSurfaces row for S,
// unless LineCommand. A verb breaking it must be in screenGaps for exactly the
// surfaces it lacks; an entry for a surface the verb no longer lacks fails too.
func TestTypableOnAUIMeansAScreenThere(t *testing.T)
func TestLineCommandHasNoScreenAndNoReason(t *testing.T) // LineCommand verbs carry neither ModalSurfaces nor NoModalSurface
func TestNoModalSurfaceOnlyForVerbsTypableNowhere(t *testing.T) // NoModalSurface set ⇒ CmdlineSurfaces has neither TUI nor WebUI
```

Build `screenGaps` from the first test's own failure output at this commit: every (verb, surface) it reports, minus the `LineCommand` ones. On `4c4e1d48` that is expected to include `action new/edit/rm/ls`, the eight runner verbs and `exec` (on both surfaces) — this task does not remove them.

- [ ] **Step 2: Run, watch them fail** — `-run 'TestTypableOnAUIMeansAScreenThere|TestLineCommandHasNoScreenAndNoReason|TestNoModalSurfaceOnlyForVerbsTypableNowhere' ./cli/verb/`. Expected: FAIL listing the gaps.
- [ ] **Step 3: Implement** the field, the `LineCommand` marks (`quit`, `exit`, `help`, `refresh`, `clear`, `sync`, `reconnect`, `trsf`, `diag`, `repo`, `display` — each currently says "a command of the TUI's own line"; drop their `NoModalSurface`), the `preview` declaration, and `screenGaps`. Verbs typable on a UI lose their `NoModalSurface` text (the third test); their gap is now the `screenGaps` entry.
- [ ] **Step 4: Run** — green; `make test` green.
- [ ] **Step 5: Commit** `cli/verb: a verb typable on a UI's command line has a screen there (LineCommand, screenGaps)`.

### Task 3: View structs and the bridge

**Files:**
- Create: `cli/admin_views.go`, `cli/admin_views_test.go`
- Modify: `cmd/harness-webui-wasm/main.go` (register the new functions beside `runnerKeys`)

**Interfaces:**
- Produces (in `cli`, JSON field names are the camelCase of the Go names):

```go
type ActionParamView struct{ Name, Type string; Choices []string }          // Type "choice" | "token"
type ActionElementView struct{ Literal string; Param string; IsParam bool }
type ActionView struct {
	ID string; Name, Description string; Revision uint32
	Params []ActionParamView; Template []ActionElementView
	TemplateText, EditedBy string; EditedAtMs uint64
}
func ActionViews(list []protocol.ActionRecord) []ActionView
// ActionForm is what a screen's form submits: Ref/Revision set = edit.
type ActionForm struct{ Ref string; Revision uint32; Name, Description string; Params []ActionParamView; Template []ActionElementView }
func ActionPutInputFromForm(f ActionForm) (ActionPutInput, error) // Params via the same rules as --param; Elements, never Template; DescriptionSet true
type RunnerKeyView struct{ Key, Prefix, Label, EnrolledBy string; EnrolledAtMs int64; Bound, Runners []string }
func RunnerKeyViews(rows []RunnerKeyRow) []RunnerKeyView
type RunnerDialView struct{ ID, Label, KeyPrefix, State, LastError string; Candidates []string }
func RunnerDialViews(rows []RunnerDialRow) []RunnerDialView
// ForceOffer is a refusable destructive result: NeedsForce on in_use, with what is in the way.
type ForceOffer struct{ Text string; OK, NeedsForce bool; InTheWay []string }
func UnenrollOffer(st protocol.RunnerKeyStatus, res RunnerUnenrollResult) ForceOffer // Text = FormatRunnerUnenroll; InTheWay = runner ids
func KickOffer(st protocol.RunnerKickStatus, res RunnerKickResult) ForceOffer         // Text = FormatRunnerKick; InTheWay = task ids
```

- Bridge (each a Promise; `text` is always the CLI's line): `harness.actionViews()` → `{views, text, ok}`; `harness.actionPutForm(formObject)` → `{text, ok}`; `harness.runnerKeyViews()` → `{views, text, ok}`; `harness.runnerDialViews()` → `{views, text, ok}`; `harness.runnerUnenrollOffer(prefix, force)` and `harness.runnerKickOffer(prefix, force)` → `ForceOffer`. The form object crosses as JSON and is decoded into `cli.ActionForm`.

- [ ] **Step 1: Failing tests** — `TestActionViewsRoundTripThroughTheForm` (a record → `ActionViews` → an `ActionForm` built from the view → `ActionPutInputFromForm` → `buildActionDefinition` equals the record's definition, including a literal `"{x}"`); `TestActionPutInputFromFormRefuses` (param-first template, unreferenced param, empty choices, `a,b` choice — each an error naming the rule); `TestUnenrollOfferNeedsForceOnlyOnInUse` and `TestKickOfferNeedsForceOnlyOnInUse` (ok → NeedsForce false; in_use → true with the ids; not_found → false).
- [ ] **Step 2: Run, watch them fail.**
- [ ] **Step 3: Implement** the views and the bridge functions (`runnerKeyPromise` is the existing Promise helper).
- [ ] **Step 4: Run** — `make test` and `make wasm-check` green.
- [ ] **Step 5: Commit** `cli, wasm: view structs and row/form bridge functions for actions and runner keys`.

### Task 4: WebUI — Actions dialog and running an action from a task

**Files:**
- Create: `webui/static/admin_models.js` (loaded by `index.html` before `main.js`), `webui/static/admin_test.mjs`
- Modify: `webui/index.html` (`#actions-btn` beside `#credentials-btn`; `<dialog id="actions-modal">`; `<dialog id="action-run-modal">`), `webui/static/main.js` (`openActions`, `openActionRun`, the task sheet item "▶ action を実行" beside "⌨ コマンド実行"), `webui/static/style.css` if a rule is needed
- Modify: `cli/verb/table.go` (`ModalSurfaces` WebUI rows: `action new/edit/rm/ls` → `webui/index.html#actions-modal`; `exec` → `webui/index.html#action-run-modal`), `cli/verb/modal_surface_test.go` (remove those WebUI bits from `screenGaps`)

**Interfaces:**
- Consumes: Task 3's bridge; `execRunToOutput(taskId, [], {action, valueSpecs})` (existing).
- Produces (in `admin_models.js`): `actionFormModel(view|null)` → editable form state; `actionFormErrors(form)` → `[string]` (empty = submittable; same rules as Task 3's refusals); `actionFormToObject(form)` → the `ActionForm` object; `actionRunModel(views)` → `{empty: bool, actions: [{name, params:[{name, kind:"select"|"text", choices}]}]}`; `actionRunValueSpecs(name, values)` → `["k=v", …]` (token values checked with the token rule; a bad one returns an error string instead).

- [ ] **Step 1: Failing JS tests** in `admin_test.mjs`: form model of a view reproduces it through `actionFormToObject`; `actionFormErrors` lists the param-first, unreferenced-param, empty-choice and comma cases and is empty for a valid form; the template's first row cannot be switched to a param (the model refuses the change); `actionRunModel([])` is `{empty: true}`; `actionRunValueSpecs` refuses `-x` for a token.
- [ ] **Step 2: Run** `make js-test` — FAIL.
- [ ] **Step 3: Implement** the models, then the dialogs: list rows (name, `rev N`, `templateText`, params, edited by/at) with Edit / 🗑 (confirm naming the action); New/Edit view with param rows (name, type select, choice rows with add/remove) and template rows (select "literal" or a param + text box), Submit disabled while `actionFormErrors` is non-empty and the errors shown under it; on `stale_revision` (the CLI text says so) the form stays open with its contents. The run dialog: action select, one control per param, result via `appendCmdOutput` labelled with the action name.
- [ ] **Step 4: Run** `make js-test` and `make test` — green (the modal-surface tests resolve the new ids).
- [ ] **Step 5: Commit** `webui: Actions dialog and running an action from the task sheet`.

### Task 5: WebUI — action checklist in grant conditions

**Files:**
- Modify: `webui/static/main.js` (`buildOverrideRows`: the condition control for an `exec_run`-only row), `webui/static/admin_models.js`, `webui/static/admin_test.mjs`

**Interfaces:**
- Produces: `conditionChecklistModel(views, cond)` → `{empty, rows:[{id, label, checked, deleted}]}` (`cond` is the row's current `actions:…` text or ""); `conditionFromChecklist(rows)` → `"actions:<id>,…"` or `""`.

- [ ] **Step 1: Failing tests**: a cond naming a deleted id yields a row `{deleted: true, checked: true, label: "deleted <id8>"}`; `conditionFromChecklist` of the model of a cond returns it unchanged (order kept); no views and no cond → `{empty: true}`; the `ports` kind is untouched (still a text box: the existing `grant_condition_test.mjs` keeps passing).
- [ ] **Step 2: Run** — FAIL.
- [ ] **Step 3: Implement**: the checklist loads `harness.actionViews()` once per dialog open; Compose and regrant both reach it through `buildOverrideRows`.
- [ ] **Step 4: Run** `make js-test` — green.
- [ ] **Step 5: Commit** `webui: pick actions by name for an exec_run grant condition`.

### Task 6: WebUI — Runner keys dialog and kick

**Files:**
- Modify: `webui/index.html` (`#runner-keys-btn` in the Runners section heading; `<dialog id="runner-keys-modal">`), `webui/static/main.js` (`openRunnerKeys`; a Kick control on each runner row), `webui/static/admin_models.js`, `webui/static/admin_test.mjs`
- Modify: `cli/verb/table.go` (WebUI rows: `runner enroll/keys/unenroll/dial add/dial ls/dial rm`, `server dial-runner` → `webui/index.html#runner-keys-modal`; `runner kick` → the runner row's control, named by its `main.js` function), `screenGaps`

**Interfaces:**
- Consumes: Task 3 (`runnerKeyViews`, `runnerDialViews`, `runnerUnenrollOffer`, `runnerKickOffer`); existing `harness.runnerEnroll(pubkeyHex, label, bind)`, `harness.runnerDialAdd(candidates, keyPrefix, label)`, `harness.runnerDialRemove(id)`, and the bridge function behind `WebUIDispatch{Fn: "serverDialRunner"}`.
- Produces: `forceStep(offer)` → `{done: bool, confirmForce: string|null}` (the second confirmation's text, listing `inTheWay`); `enrollFormErrors({pubkey})` (64 hex), `dialFormErrors({candidates, keyPrefix})` (key required).

- [ ] **Step 1: Failing tests**: `forceStep` of an ok offer is done; of an in_use offer asks with every id in the text; of not_found is done with the CLI text; `dialFormErrors` refuses an empty key; `enrollFormErrors` refuses 63 hex.
- [ ] **Step 2: Run** — FAIL.
- [ ] **Step 3: Implement** the dialog: Keys list (prefix, label, enrolled by, bound id8s or "unbound", Unenroll), Enroll form (public key, label, bind via `buildTaskChecklist`), Dial records list (id, candidates, key, label, state, Remove), Add form (candidates, key select from the key views, label), Dial once (cid, via select of registered runners or "direct"). Kick on each runner row with the same two-step confirmation.
- [ ] **Step 4: Run** `make js-test`, `make test` — green.
- [ ] **Step 5: Commit** `webui: runner keys, dial records and kick`.

### Task 7: TUI — Actions modal and running an action

**Files:**
- Create: `tui/actions_modal.go` (`ActionsModal`: list + delete confirm), `tui/action_form.go` (`ActionFormModel`: new/edit), `tui/action_run_form.go` (`ActionRunFormModel`), with `_test.go` for each
- Modify: `tui/app.go` (fields + constructors), `tui/overlays.go` (key routing like `inExecsModal`), `tui/keys.go` (`mainKeys.Actions` global, `mainKeys.RunAction` on the tasks table — letters free in the key map; `keys_test` holds the map/bindings pair), `cli/verb/table.go` (TUI rows: `action *` → `tui/actions_modal.go:ActionsModal`; `exec` → `tui/action_run_form.go:ActionRunFormModel`), `screenGaps`

**Interfaces:**
- Consumes: `cli.ActionListWith`, `cli.ActionViews`, `cli.ActionPutInputFromForm`, `(*cli.Client).RunActionPut`, `RunActionRemove`, `cli.ActionInvocationFromRecord`, the existing `DoExecRun` path with `ExecRunFlags.Action` / `Values` (`tui/execrun.go`).
- Produces: `ActionResultMsg` is reused for results (`tui/actions_verbs.go`).

- [ ] **Step 1: Failing tests**: the modal opens on its key and lists views (fake client returning two records); `ActionFormModel` refuses to submit a param-first template and an unreferenced param and shows the reason; Edit carries the revision and a `stale_revision` result keeps the form open; `ActionRunFormModel` with no actions shows "no actions — define one in Actions"; a choice param cycles its choices, a token param refuses `-x`; submit produces an exec with the action and values; `TestEveryOverlayIsNamedByTheVerbTable` passes.
- [ ] **Step 2: Run** — FAIL.
- [ ] **Step 3: Implement** with `bubbles/table` for the list and `textinput` for fields, as `ExecsModal` and `BgFormModel` do; sized to fit 80×24.
- [ ] **Step 4: Run** `make test` — green.
- [ ] **Step 5: Commit** `tui: Actions modal and running an action`.

### Task 8: TUI — override editing in the authority picker

**Files:**
- Modify: `tui/authoritypicker.go` (overrides become edited rows; the "CARRIED, not edited" comment is replaced by what is now guaranteed), `tui/authoritypicker_test.go`, `tui/overlays.go` (keys for add / remove override row), `tui/keys.go`

**Interfaces:**
- Consumes: `cli.ActionViews` for the condition checklist (loaded when the picker opens); `cli/verb` grant-condition parsing for the existing `ports:` text.
- Produces: `Overrides()` and `GrantConditions()` keep their signatures and now return the edited rows.

- [ ] **Step 1: Failing tests**:
  - `TestRegrantKeepsUntouchedOverrideAndBody`: open on a task with two overrides, one `ActionSet`-conditioned; change the other's base; `Overrides()`/`GrantConditions()` return the conditioned one byte-identical.
  - `TestRemovingAnOverrideDropsOnlyIt`: remove one row; the other remains; the result line names the removed mask.
  - `TestClaimedBitNotSelectableInAnotherRow`: a bit in row 1 cannot be toggled on in row 2.
  - `TestConditionChecklistForExecRunRow`: a row of exactly `exec_run` shows the action checklist; a deleted id stays checked as "deleted <id8>"; a row of `forward_local` shows the ports text; a mixed row shows none and drops its condition.
  - `TestSessionDefaultsEditOverrides`: the same editing in the session-defaults mode reaches the spawn request (`Authority.opts`).
- [ ] **Step 2: Run** — FAIL.
- [ ] **Step 3: Implement**: each override is a section of rows (cap toggles, base cycle, exclude-self where the base takes it, task ids, condition); add/remove row keys; the existing base/ids controls are reused per section.
- [ ] **Step 4: Run** `make test` — green.
- [ ] **Step 5: Commit** `tui: edit per-capability overrides and their conditions in the authority picker`.

### Task 9: TUI — Runner keys modal and kick

**Files:**
- Create: `tui/runner_keys_modal.go` (`RunnerKeysModal`: keys list, enroll form, dial records list, add form, dial once), `tui/runner_keys_modal_test.go`
- Modify: `tui/app.go`, `tui/overlays.go`, `tui/keys.go` (`mainKeys.RunnerKeys` and `mainKeys.RunnerKick` on the runners table), `cli/verb/table.go` (TUI rows: the seven runner-key/dial verbs and `server dial-runner` → `tui/runner_keys_modal.go:RunnerKeysModal`; `runner kick` → its key handler on `*App`), `screenGaps` (now empty of every verb this plan covers)

**Interfaces:**
- Consumes: `cli.RunnerKeysWith`, `RunnerKeyViews`, `RunnerDialListWith`, `RunnerDialViews`, `RunnerEnrollWith`, `RunnerUnenrollWith` + `UnenrollOffer`, `RunnerKickWith` + `KickOffer`, `RunnerDialAddWith`, `RunnerDialRemoveWith`, `ServerDialRunnerWith`; the existing `Do*` commands in `tui/runner_keys.go` where they fit.

- [ ] **Step 1: Failing tests**: the modal lists keys and dial records; unenroll of an in-use key asks once plainly and offers force only after `in_use`, listing the runners; the dial add form refuses an empty key; kick follows the same two steps listing tasks; `TestEveryOverlayIsNamedByTheVerbTable` passes; `TestTypableOnAUIMeansAScreenThere` passes with no entry left for the twelve verbs or `exec`.
- [ ] **Step 2: Run** — FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `make test` — green.
- [ ] **Step 5: Commit** `tui: runner keys, dial records and kick`.

### Task 10: Live check, backlog, docs

**Files:**
- Modify: `docs/backlog.md` (one entry: the remaining `screenGaps` verbs, with "check for an undeclared equivalent screen first", e.g. `session send` ↔ the session preview)
- Modify: `README.md` § Actions (the screens: where Actions and Runner keys are on each UI)

- [ ] **Step 1:** `scripts/dummy-harness.sh up --detach --known-psk --agent fake`, `eval "$(scripts/dummy-harness.sh env)"`.
- [ ] **Step 2: WebUI** (headless Playwright against `$WEBUI_URL`): define an action with a choice and a token param; edit it; run it from a task's sheet; grant a child `exec_run` conditioned on it through the regrant dialog's checklist; delete it and see the condition row read "deleted <id8>"; enroll a key, list, unenroll; add and remove a dial record; at 390 px width, no horizontal scroll. Screenshots kept (report their paths).
- [ ] **Step 3: TUI** in a dummy PTY sized with `session send --resize`: the same flows, plus override editing (add a row, set its condition, regrant, see `ls` show the condition) and kick refusal → force.
- [ ] **Step 4:** `scripts/dummy-harness.sh down`; commit the backlog and README edits.
- [ ] **Step 5:** `make test`, `make js-test`, `make wasm-check` green; `scripts/wire-skew-check.sh` (no `.bgn` change expected: it exits 0 at once).
