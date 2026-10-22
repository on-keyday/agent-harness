# Delegation bound Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every task carries a delegation bound (`delegate_caps`, `delegate_scope`) that limits the authority it may write onto the tasks it spawns or re-grants; the default delegates no bits; the operator edits it live with `caps set`.

**Architecture:** The rule lives in the leaf package `authority` as one type, `Granter` — a granting task measured twice, by its own scope (the existing `Reach`) and by its ceiling read as a scope around it (D21). The server resolves a `Granter` per request and calls it at every grant site and in the bound-writing cascade; the store carries the bound beside caps and scope in one `authority.Grant` value and persists it on the two WAL events that already carry authority. Clients get the bound on `TaskInfo` / `WhoAmIResponse` and set it through the existing verb table, `cli.SessionOpts`, `SetCapsOpts` and `tui.Authority`.

**Tech Stack:** Go 1.26; `.bgn` schema regenerated with `make protoregen` (brgen kit cached under `~/.cache/brgen-kit`; a transient `curl 52` under host load is retried, not debugged); `cli/verb` codegen (`go generate ./cli/verb/`); the wasm WebUI bridge; `make` targets.

**Spec:** `docs/superpowers/specs/2026-10-22-delegation-bound-design.md` — D1–D23, § The rule, § Wire, § Persistence, § Surfaces, § Testing. The spec's server line numbers predate the `authority` package; the mapping is in § Spec names → current code below.

## Global Constraints

- Default bound = zero value: `delegate_caps = none`, `delegate_scope = subtree`, no ids, `exclude_self = 0` (D2, D23). A request that omits the fields, a zero Go struct and a legacy WAL record all mean it.
- `child.caps = P.caps ∩ P.delegate_caps ∩ requested` (§ Caps). Bits and ranks above the bound are CLAMPED silently; ids outside it are REFUSED (`scope_not_permitted`) on spawn/resume and DROPPED on a cascade (D12).
- An operator connection (zero principal) is unbounded: no ceiling check runs; values are written after `authority.Validate` alone (D17).
- No stored invariant `delegate_caps ⊆ caps` (D13). The child's `delegate_caps = requested ∩ P.delegate_caps` — not intersected with `P.caps`.
- `delegate_scope` never carries overrides (D10): the wire type is a bare `TaskScope`; a Go value with `Overrides` is an invalid scope.
- A change of the bound alone is not a narrowing (D14): it closes no connection.
- `--cascade` clamps descendants to the target's bound ONLY when that `caps set` writes a bound half (D19); otherwise today's cascade, unchanged.
- Flags `--delegate-caps` (caps grammar, `parseCapsFlag`) and `--delegate-scope` (the `--scope` grammar, `parseScopeFlag`, `descendants` and `-self` forms included); display keys `delegate=` and `delegate-scope=` (D15).
- Ceiling refusal text, verbatim: `scope id <id> is outside what you may delegate (see whoami: delegate-scope)`.
- Rollout server first, then every client, no skew guard (D18). Run `scripts/wire-skew-check.sh` before landing, with `TMPDIR` under the worktree's `.superpowers/` (the shared `/tmp` is near quota).
- Work in `/home/kforfk/workspace/remote-agent-harness/.harness-worktrees/70fbad4a6eb6f1e992be8a669f1bcefd`. Run `make test` and `go test ./cli/...` with `env -u HARNESS_SERVER_PUBKEY -u HARNESS_SERVER_CID -u HARNESS_AUTH_TICKET -u HARNESS_TASK_ID`.

## Spec names → current code

| Spec says | Now |
| --- | --- |
| `intersectCaps` at four grant sites (`server/task_handler.go`) | `Granter.ChildCaps`; `intersectCaps` is deleted |
| `attenuateScope` (`server/capabilities.go`) | `authority.Attenuate`, wrapped by `TaskHandler.attenuateScope`; both callers move to `Granter.ChildScope` |
| `newGrantReach` / `grantReach.reaches` / `boundRanks` | `authority.NewReach` / `Reach.Reaches` / `Reach.BoundRanks` |
| `clampScopeUnder` | `authority.ClampUnder` |
| `isNarrowing` | `authority.IsNarrowing` |
| `scopeCoversNewChild` | `authority.CoversNewChild` |
| `validateScope` | `authority.Validate` |
| `minScopeBase` / `scopeBaseRank` | `authority.MinBase` / `authority.BaseRank` |
| `writeCapsChangedLocked(…)` "takes the bound" | `writeCapsChangedLocked(e *TaskEntry)` already writes a snapshot of `e`; it writes `e`'s bound too — no signature change |
| `Scope.applyToWAL` / `scopeFromWAL` | `applyScopeToWAL` / `scopeFromWAL` in `server/scope.go` |

## Decisions this plan adds (spec silent)

- **One `authority.Grant` value** (`Caps`, `Scope`, `DelegateCaps`, `DelegateScope`) is what `TaskStore.Create` takes and what `GrantUpdate` carries, replacing the separate `caps, scope` parameters. Adding two more positional parameters to `Create` (11 → 13) and two presence/value pairs to `Resume` (13 → 17) and `SetCaps` would make transpositions compile. The ~190 test call sites are rewritten mechanically with `gofmt -r` (Task 2).
- **Refusals are typed errors**, not prefixed strings: `*authority.InvalidScopeError`, `*authority.OutOfReachError`, `*authority.OutOfDelegationError`. `scopeRejectionMsg` maps them with `errors.As`. The existing `Attenuate` keeps its `offender` return (the `"invalid scope: "` prefix is converted once, inside `Granter`).
- **A refused id in the child's own bound** reads `delegate-scope id <id> is outside what you may delegate (see whoami: delegate-scope)` — the spec fixes the text for scope ids only; the field name tells the agent which flag to fix.
- **The WAL key list gains `delegate_scope_exclude_self`.** The spec's list omits it, but the bound has `exclude_self` (D22, and it is forced on under `C = none`); without the key a forced `exclude_self` is lost at the next restart.
- **`WhoAmIResponse` for an operator carries `delegate_caps = all`, `delegate_scope = global`** (D17), so both `whoami` branches print what the wire says.
- **`actD` membership asks `Reach.Reaches(id, protocol.Capability_FileRead)`** on the ceiling's `Reach`. Any single non-spawn bit gives the same answer because `C` has no overrides (spec § The rule: "the bit passed is immaterial"); naming one constant keeps the call sites identical.
- **The WebUI delegation sections use one new builder**, `buildScopeControls`, for the base radios, exclude-self box, visibility radios and the two task checklists; the existing compose and re-grant scope controls are not refactored onto it in this change.

## Review Focus

1. **A worker spawned through `session new` by an agent** (the path the skill documents) gets the ceiling refusal TEXT, not the fixed "scope not permitted" line — native and wasm. Pinned in Task 4 (server sets `error_msg`) and Task 6 (both clients print it).
2. **A `caps set <supervisor> --caps … --cascade` after landing** leaves every worker's caps as they were, although every supervisor replays with the empty bound (D19). Pinned in Task 5.
3. **Restart keeps a bound written by `caps set` of caps only, by a resume, and a forced `exclude_self`.** Pinned in Task 2's replay test.
4. **The TUI and WebUI resume paths never send the session's default bound** (checklist item 26): `r`/`R`/`u`/`U` keep the recorded authority; an explicit resume re-grants the bound only behind its own presence bit. Pinned in Task 8 (`tui/session_opts_test.go`) and Task 9.
5. **An agent with the default bound that spawns with `--caps all`** gets a child with `caps=none` and `delegate=none`, and a grandchild of that child holds none — `all` means "everything you may delegate". Pinned in Task 1 and live step 1.

---

## File Structure

| File | Change |
| --- | --- |
| `authority/delegate.go` (new) + `authority/delegate_test.go` | `Grant`, `Granter`, the three error types, the ceiling |
| `authority/no_copies_test.go` | `intersectCaps` joins the forbidden names |
| `server/taskstore.go`, `server/wal.go`, `server/scope.go` | `TaskEntry` fields; `Grant()`; `GrantUpdate`; `Create`/`SetCaps`/`Resume` signatures; WAL keys + replay |
| `server/capabilities.go`, `server/task_handler.go` | `granter`; grant sites; `exactFields`; `scopeRejectionMsg`; `toTaskInfo`; whoami |
| `server/set_caps_handler.go` | bound halves; D19 cascade |
| `runner/protocol/message.bgn` + regenerated `message.go` | § Wire table |
| `cli/verb/spawn.go`, `cli/verb/table.go`, `cli/verb/caps.go`, regenerated `cli/verb/actions_gen.go` | flags; exact conflicts; `AtLeastOne`; DELEGATION catalog section |
| `cli/session_opts.go`, `cli/submit.go`, `cli/set_caps.go`, `cli/open_interactive_native.go`, `cli/open_interactive_wasm.go` | funnel fields; request builds; `error_msg` |
| `cli/list.go`, `cli/whoami.go`, `cli/caps_audit.go`, `cli/caps_audit_render.go` | display |
| `tui/client.go`, `tui/app.go`, `tui/dispatch.go`, `tui/overlays.go`, `tui/authoritypicker.go`, `tui/detail.go`, `tui/capsauditmodal.go` | defaults, picker section, detail lines |
| `cmd/harness-webui-wasm/main.go`, `webui/index.html`, `webui/static/main.js` | snapshot fields; compose + re-grant sections; row meta, sheet, audit |
| `README.md`, `runner/agentskills/supervising-workers/SKILL.md`, `.claude/skills/supervising-workers/SKILL.md`, `.agents/skills/supervising-workers/SKILL.md` | docs |

---

### Task 1: the rule, in `authority`

**Files:** Create `authority/delegate.go`, `authority/delegate_test.go`

**Interfaces:**
- Consumes: `NewReach`, `Reach.Reaches`, `Reach.BoundRanks`, `Attenuate`, `ClampUnder`, `CoversNewChild`, `Validate`, `MinBase`, `NormalizeIDs` (all existing).
- Produces:
  - `type Grant struct { Caps protocol.Capability; Scope Scope; DelegateCaps protocol.Capability; DelegateScope Scope }`
  - `type Granter struct` (unexported fields: hex, held `Grant`, own `*Reach` over `held.Scope`, ceil `*Reach` over `held.DelegateScope`)
  - `func NewGranter(hex string, held Grant, children map[string][]string) *Granter`
  - `func (g *Granter) ChildCaps(requested protocol.Capability) protocol.Capability` — nil `g` (operator) returns `requested`
  - `func (g *Granter) ChildScope(req Scope, held protocol.Capability) (Scope, error)` — `Attenuate(req, held, g.own)` then the ceiling; nil `g` → `Attenuate(req, held, nil)` only
  - `func (g *Granter) ChildBound(reqCaps protocol.Capability, req Scope, childHex string) (protocol.Capability, Scope, error)` — the child's own bound; `childHex == ""` is a task not yet created; nil `g` → `Validate` + `NormalizeIDs` only, `reqCaps` unchanged
  - `func (g *Granter) ClampDescendant(d Grant, dHex string) Grant` — the D19 cascade; `g` must not be nil
  - `type InvalidScopeError struct{ Err error }`, `type OutOfReachError struct{ ID string }`, `type OutOfDelegationError struct{ ID string; Field string }` (`Field` is `"scope"` or `"delegate-scope"`), each with `Error()`

The ceiling, applied to a scope `s` (spec § Scope table; used by `ChildScope` with refuse, by `ClampDescendant` with drop — ONE unexported function taking the mode, not two copies):

- `s.Base = MinBase(s.Base, C.Base)`; every override's `Base = MinBase(o.Base, C.Base)`.
- `vis := MinBase(s.VisRank(), C.VisRank())`; if `vis != s.Base` → `VisBasePresent = true, VisBase = vis`, else `VisBasePresent = false, VisBase = 0` (canonical, `Validate`'s rule).
- each id of `s.IDs` and of every override's `IDs` must satisfy `g.ceil.Reaches(id, protocol.Capability_FileRead)` (`actD`); each of `s.VisIDs` must satisfy `g.ceil.Reaches(id, protocol.Capability_None)` (`visD`). Refuse → `&OutOfDelegationError{ID, Field}`; drop → leave it out.
- `ExcludeSelf` untouched.

`ChildBound` applies the same ceiling to `req` with `Field = "delegate-scope"`, after: `req.Overrides` non-empty → `&InvalidScopeError{…"a delegation bound carries no per-capability overrides"}`; `Validate(req)` failing → `&InvalidScopeError`. Then `ExcludeSelf` is forced on when the child is not in `actD`: `childHex == ""` → `!CoversNewChild(C, protocol.Capability_FileRead)`, else `!g.ceil.Reaches(childHex, protocol.Capability_FileRead)`. Caps = `reqCaps & held.DelegateCaps`.

`ClampDescendant`: caps = `d.Caps & held.Caps & held.DelegateCaps`; scope = `ClampUnder(d.Scope, caps, g.own)` then the ceiling (drop); delegate caps = `d.DelegateCaps & held.DelegateCaps`; delegate scope = the `ChildBound` ceiling (drop, no error path) with `ExcludeSelf` forced by `dHex`.

- [ ] **Step 1: Write the tests** in `authority/delegate_test.go` over a hand-built creator index (`P` with children `S` (sibling-to-be, an existing child), and an unrelated task `X`; `P.Scope = subtree+ids:X`):
  - `TestChildCapsTable`: granter held caps `spawn,file_read,cancel` × bound {`none`, `file_read`} × requested {`none`, `file_read`, `file_read,cancel`, `all`} → `none,none,none,none` / `none,file_read,file_read,file_read`; nil granter × `all` → `all`.
  - `TestChildCapsBoundNamesBitHolderLacks` (D13): bound `cancel`, held caps without `cancel` → child gets no `cancel`; same bound with held caps gaining `cancel` → child gets it.
  - `TestChildScopeVisClampedToCeiling`: held scope with global visibility, `Scope{Base: subtree, VisBase: global, VisBasePresent: true}`, ceiling zero value (`subtree`), request `subtree` with visibility `global` → result `subtree` with `VisBasePresent == false`.
  - `TestChildScopeDefaultCeilingIDs`: ceiling zero value; `ids:<P>` ok, `ids:<S>` ok, `ids:X` → `*OutOfDelegationError{ID: X, Field: "scope"}` (X is in `P`'s own ids, outside its subtree).
  - `TestChildScopeDescendantsCeiling` (D22): ceiling `{Base: subtree, ExcludeSelf: true}`; `ids:<P>` refused, `vis-ids:<P>` ok, `ids:<S>` ok.
  - `TestChildBoundNoneCeilingForcesExcludeSelf`: ceiling `{Base: none, IDs: [X]}`; child scope `ids:<P>` ok, `ids:X` ok, `ids:<S>` refused; `ChildBound(0, Scope{}, "")` returns `ExcludeSelf == true`.
  - `TestChildScopeCeilingIDLists`: ceiling `subtree+ids:X` → `ids:X` ok; ceiling `subtree+vis-ids:X` → `vis-ids:X` ok, `ids:X` refused; ceiling `global` → `ids:X` ok; an override `file_read:none+ids:X` under ceiling `subtree` → refused.
  - `TestCeilingSetsAreReachOverCeiling`: over a three-level index, for every id in the index, `actD` membership equals `ActionSet(P, C, FileRead)` ∩ `VisibleSet(P, C)` and `visD` membership equals `VisibleSet(P, C)` — for `C` in {zero, descendants, none+ids, global}.
  - `TestChildBoundClampsToGranters`: held bound `file_read` / `subtree`; request `file_read,cancel` / `global+vis-ids:<S>` → `file_read` / `subtree+vis-ids:<S>`; a grandchild granted by that child (`NewGranter(child, …)` with the child's bound = default) gets `ChildCaps(all) == none` (Review Focus 5).
  - `TestChildBoundRefusesOverrides`: request bound with one override → `*InvalidScopeError`.
  - `TestClampDescendantDropsInsteadOfRefusing`: `d.Scope = subtree+ids:X`, `d.DelegateScope = subtree+vis-ids:X`, target bound default → both ids dropped, `d.Caps` and `d.DelegateCaps` intersected; a `dHex` outside `T`'s `actD` gets `DelegateScope.ExcludeSelf == true`.
  - Error texts: `(&OutOfDelegationError{ID: "ab…", Field: "scope"}).Error() == "scope id ab… is outside what you may delegate (see whoami: delegate-scope)"`; `OutOfReachError` → `"scope id ab… is outside your own scope"`.
- [ ] **Step 2: Run** `go test ./authority/ -run 'TestChild|TestCeiling|TestClampDescendant'` — Expected: FAIL to compile (`Granter` undefined).
- [ ] **Step 3: Implement** `authority/delegate.go` per the Interfaces and the ceiling above. In `ChildScope`, an `offender` from `Attenuate` with the `"invalid scope: "` prefix becomes `&InvalidScopeError`, any other offender `&OutOfReachError{ID}`.
- [ ] **Step 4: Run** `go test ./authority/ -count=1` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(authority): delegation bound — Granter and the ceiling`.

---

### Task 2: store, WAL and replay

**Files:** Modify `server/taskstore.go`, `server/wal.go`, `server/scope.go`, every `server/*_test.go` calling the changed signatures; Test `server/delegation_store_test.go` (new)

**Interfaces:**
- Consumes: `authority.Grant` (Task 1).
- Produces:
  - `TaskEntry` fields `DelegateCaps protocol.Capability`, `DelegateScope authority.Scope`; `func (e TaskEntry) Grant() authority.Grant`.
  - `type GrantUpdate struct { Caps, Scope, DelegateCaps, DelegateScope bool; To authority.Grant }` and `func (u GrantUpdate) Any() bool`.
  - `Create(repo, prompt string, kind protocol.TaskKind, origin protocol.ClientKind, creatorTaskID protocol.TaskID, boundRunnerID string, selector protocol.RunnerSelector, extraArgs []string, g authority.Grant, agentProfile string) string`
  - `SetCaps(id string, u GrantUpdate, by AuthorityWrite) (TaskEntry, bool)`
  - `Resume(id, prompt string, extraArgs []string, selector protocol.RunnerSelector, boundRunnerID string, resumerKind protocol.ClientKind, u GrantUpdate, newKind protocol.TaskKind, agentProfile string, resumer protocol.TaskID) (TaskEntry, error)` — writes `task_caps_changed` iff `u.Any()`.
  - `func applyDelegationToWAL(caps protocol.Capability, s authority.Scope, ev *WALEvent)` / `func delegationFromWAL(ev WALEvent) (protocol.Capability, authority.Scope)` in `server/scope.go`, beside the scope pair; NO `board_observe` migration.
  - WAL keys (`WALEvent` + `walEventJSON`, all `omitempty`): `delegate_caps`, `delegate_scope_base`, `delegate_scope_vis_base`, `delegate_scope_vis_base_present`, `delegate_scope_exclude_self`, `delegate_scope_ids`, `delegate_scope_vis_ids`; written on `task_created` and every `task_caps_changed`; replay of both assigns the bound.

- [ ] **Step 1: Rewrite the call sites mechanically** before any test is written, so the package compiles against the new shapes:
  ```bash
  gofmt -r 'x.Create(a, b, c, d, e, f, g, h, i, j, k) -> x.Create(a, b, c, d, e, f, g, h, authority.Grant{Caps: i, Scope: j}, k)' -w server/
  gofmt -r 'x.SetCaps(a, b, c, d, e, f) -> x.SetCaps(a, GrantUpdate{Caps: b, Scope: d, To: authority.Grant{Caps: c, Scope: e}}, f)' -w server/
  gofmt -r 'x.Resume(a, b, c, d, e, f, g, h, i, j, k, l, m) -> x.Resume(a, b, c, d, e, f, GrantUpdate{Caps: g, Scope: i, To: authority.Grant{Caps: h, Scope: j}}, k, l, m)' -w server/
  ```
  then change the three signatures and add missing `authority` imports. Expected: `go build ./server/ && go vet ./server/` clean.
- [ ] **Step 2: Write the tests** in `server/delegation_store_test.go` on a store with a WAL in `t.TempDir()`:
  - `TestCreateStoresBound`: `Create(…, Grant{DelegateCaps: file_read, DelegateScope: {Base: none, IDs: [X]}}, …)` → `Get` returns them.
  - `TestSetCapsEachHalfAlone`: `GrantUpdate{DelegateCaps: true}` changes only `DelegateCaps`; `{DelegateScope: true}` only `DelegateScope`; `{Caps: true}` leaves the bound.
  - `TestBoundSurvivesReplay` (Review Focus 3): create with a bound; `SetCaps` caps only; `Resume` with `GrantUpdate{DelegateScope: true, To: {DelegateScope: {Base: none, ExcludeSelf: true}}}`; reopen a store on the same WAL → bound equals the last written, `ExcludeSelf` included.
  - `TestLegacyRecordReplaysDefaultBound`: a hand-written `task_created` line without `delegate_*` keys but with `capabilities` holding `board_observe` → `DelegateCaps == 0`, `DelegateScope` is the zero `Scope` (no global-visibility migration).
- [ ] **Step 3: Run** `go test ./server/ -run 'TestCreateStoresBound|TestSetCapsEachHalfAlone|TestBoundSurvivesReplay|TestLegacyRecordReplaysDefaultBound'` — Expected: FAIL (fields not stored / not replayed).
- [ ] **Step 4: Implement** the fields, `Grant()`, the projection pair and the replay. `writeCapsChangedLocked(e)` and `Create` call `applyDelegationToWAL(e.DelegateCaps, e.DelegateScope, &ev)`.
- [ ] **Step 5: Run** `go test ./server/ -count=1` — Expected: PASS, including `TestWALEventJSONRoundTripCopiesEveryField` (the shadow struct carries the seven keys).
- [ ] **Step 6: Commit** `feat(server): store and persist the delegation bound`.

---

### Task 3: wire schema and the mappers

**Files:** Modify `runner/protocol/message.bgn`; regenerate `runner/protocol/message.go`; Modify `server/task_handler.go` (`toTaskInfo`, whoami at the `protocol.WhoAmIResponse{` literal); Test `runner/protocol/delegation_wire_test.go` (new), `server/mapper_completeness_test.go`

**Interfaces:**
- Consumes: Task 2's `TaskEntry` fields.
- Produces (spec § Wire, exact placement):
  - `SubmitRequest`: `reserved :u4` → `delegate_caps_present :u1`, `delegate_scope_present :u1`, `reserved :u2`; appended after `overrides`: `delegate_caps :Capability`, `delegate_scope :TaskScope`. `requested_caps`' comment: `all` = "everything the creator may delegate".
  - `OpenInteractiveRequest`: `reserved :u1` → `delegate_caps_present :u1`; after `overrides`, before `if x11_enabled == 1`: `delegate_caps :Capability`, `delegate_scope :TaskScope`, `delegate_scope_present :u1`, `reserved2 :u7`.
  - `OpenInteractiveResponse`: after the `if attached == 1` block, before `if status == …ambiguous_runner`: `error_msg_len :u16`, `error_msg :[error_msg_len]u8`.
  - `SetCapsRequest`: `reserved :u4` → `delegate_caps_present :u1`, `delegate_scope_present :u1`, `reserved :u2`; appended: `delegate_caps :Capability`, `delegate_scope :TaskScope`.
  - `TaskInfo`: appended after `authority_age_ms`: `delegate_caps :Capability`, `delegate_scope :TaskScope`.
  - `WhoAmIResponse`: appended after `reserved3`: `delegate_caps :Capability`, `delegate_scope :TaskScope`.
  - Each presence bit's comment states: resume / `caps set` only; on create the values always apply and their zero is the default bound.

- [ ] **Step 1: Write `TestDelegationWireRoundTrip`**: one value of each of the six formats with every new field non-zero (`OpenInteractiveRequest` with `x11_enabled = 1` and a populated `X11Forward`, so the placement before the conditional block is exercised; `OpenInteractiveResponse` with `attached = 1` and `status = ambiguous_runner`); `MustAppend` then decode; deep-equal.
- [ ] **Step 2: Run** `go test ./runner/protocol/ -run TestDelegationWireRoundTrip` — Expected: FAIL to compile.
- [ ] **Step 3: Edit `message.bgn`** as above; `make protoregen`.
- [ ] **Step 4: Run** `go test ./runner/protocol/ -count=1 && go vet ./...` — Expected: PASS. Then `go test ./server/ -run 'MapsEveryField'` — Expected: FAIL (`TestToTaskInfoMapsEveryField`, `TestWhoamiResponseMapsEveryField` name the new fields).
- [ ] **Step 5: Map** in `toTaskInfo` (`DelegateCaps`, `DelegateScope` via `ToWire` with no overrides) and in the whoami literal (an agent: its record's bound; an operator: `Capability_All` and `TaskScope{Base: ScopeBase_Global}`). Extend the two completeness tests' fixtures with non-zero bounds.
- [ ] **Step 6: Run** `go test ./server/ -count=1` — Expected: PASS.
- [ ] **Step 7: Commit** `feat(protocol): delegation bound on the wire`.

---

### Task 4: spawn and resume enforce the bound

**Files:** Modify `server/capabilities.go`, `server/task_handler.go`, `authority/no_copies_test.go` (`"intersectCaps"` joins the forbidden names once it is deleted here); Test `server/delegation_spawn_test.go` (new)

**Interfaces:**
- Consumes: `Granter` (Task 1), `GrantUpdate` / `Create` (Task 2), wire fields (Task 3).
- Produces:
  - `func (h *TaskHandler) granter(cid string, callerCaps protocol.Capability) *authority.Granter` — nil for an operator; an agent whose record is gone → `Grant{Caps: callerCaps, Scope: {Base: none}, DelegateScope: {Base: none}}`. Replaces `attenuateScope`.
  - `resumedCaps(idHex string, override bool, g *authority.Granter, requested protocol.Capability) protocol.Capability` — `g.ChildCaps(requested)` when overriding.
  - `scopeRejectionMsg(err error) string` — `InvalidScopeError` → `"invalid scope: " + inner`; the other two → their `Error()`.
  - `exactFields` gains `DelegateCapsPresent`, `DelegateScopePresent`; `exactMismatch` returns `"--delegate-caps"` / `"--delegate-scope"` after `"--scope"`.
  - `intersectCaps` deleted.

At each site (`handleSubmit`, `handleSubmitResume`, `handleOpenInteractive` fresh and resume): `g := h.granter(cid, creatorCaps)`; caps `g.ChildCaps(req.RequestedCaps)` (resume: `resumedCaps`); scope `g.ChildScope(…, caps)` refused when fresh or `ScopePresent`; bound `g.ChildBound(req.DelegateCaps, authority.ScopeFromWire(req.DelegateScope, nil), childHex)` (`""` fresh, the task id on resume) refused when fresh or `DelegateScopePresent()`. Fresh: `Create(…, authority.Grant{…all four…}, …)`. Resume: `GrantUpdate{Caps: ResumeCapsOverride(), Scope: ScopePresent(), DelegateCaps: DelegateCapsPresent(), DelegateScope: DelegateScopePresent(), To: …}`. `handleOpenInteractive` refuses through a variant of `errResp` that sets `error_msg` to `scopeRejectionMsg(err)` (D20).

- [ ] **Step 1: Write the tests** (handler-level, the existing `scope_attenuation_test.go` harness style — a store, a principal mapping per cid, `handleSubmit` / `handleOpenInteractive` called directly):
  - `TestSpawnCapsThroughBound`: the spec's caps table at all four sites — {operator, agent bound none, agent bound `file_read`} × requested {none, `file_read`, `file_read,cancel`, `all`}, agent holding `spawn,file_read,cancel`. Expected: operator = requested; bound none → none; bound `file_read` → `none, file_read, file_read, file_read`.
  - `TestSpawnScopeCeilingRefusalText`: agent P with scope `subtree+ids:X`, default bound; submit `--scope ids:X` → `scope_not_permitted`, `error_msg == "scope id <X> is outside what you may delegate (see whoami: delegate-scope)"`; the same through `handleOpenInteractive` → `OpenInteractiveResponse.ErrorMsg` equal (Review Focus 1).
  - `TestSpawnScopeOutsideOwnScopeText`: an id outside `P`'s own scope → `"scope id … is outside your own scope"` (existing text kept).
  - `TestSpawnWritesChildBound`: P bound `file_read` / `subtree`; request bound `file_read,cancel` / `none` → child record `file_read` / `none` with `ExcludeSelf` false (a new child is inside a `subtree` ceiling); P bound `{Base: none}` → child's `DelegateScope.ExcludeSelf == true`.
  - `TestResumePresenceBitsWriteOneHalf`: resume with only `delegate_caps_present` → only `DelegateCaps` changes; only `delegate_scope_present` → only `DelegateScope`; neither → both kept.
  - `TestExactResumeRefusesBoundHalves`: exact resume with either new bit → `resume_exact_mismatch`, `error_msg` naming `--delegate-caps` / `--delegate-scope`.
- [ ] **Step 2: Run** `go test ./server/ -run 'TestSpawnCapsThroughBound|TestSpawnScope|TestSpawnWritesChildBound|TestResumePresenceBitsWriteOneHalf|TestExactResumeRefusesBoundHalves'` — Expected: FAIL (children get `creator & requested`; no ceiling).
- [ ] **Step 3: Implement** the Interfaces above.
- [ ] **Step 4: Run** `go test ./server/ ./authority/ -count=1` — Expected: PASS, `TestNoSecondScopeImplementation` included. Existing spawn tests that relied on an agent creator passing bits through with the default bound now fail by design; give those fixtures a bound (`DelegateCaps: Capability_All, DelegateScope: {Base: global}`) and ledger which tests changed and why.
- [ ] **Step 5: Commit** `feat(server): spawn and resume are bounded by the granter's delegation bound`.

---

### Task 5: `caps set` writes the bound; the D19 cascade

**Files:** Modify `server/set_caps_handler.go`; Test `server/set_caps_delegation_test.go` (new)

**Interfaces:**
- Consumes: `GrantUpdate`, `SetCaps` (Task 2); `Granter.ClampDescendant` (Task 1); `SetCapsRequest` fields (Task 3).
- Produces: `handleSetCaps` writes `DelegateCaps` / `DelegateScope` behind their presence bits after `authority.Validate(authority.ScopeFromWire(req.DelegateScope, nil))` (refusal `invalid_scope`); with `Cascade()` and a bound bit set, each descendant `d` becomes `authority.NewGranter(after.ID, after.Grant(), children).ClampDescendant(d.Grant(), dHex)` written with all four presence bits and `AuthorityWrite{cascade, target}`; with `Cascade()` and no bound bit, today's loop unchanged. `narrowed` stays `IsNarrowing` over caps/scope only.

- [ ] **Step 1: Write the tests**:
  - `TestSetCapsBoundHalvesAlone`: `delegate_caps_present` only → only the bound's caps change; `delegate_scope_present` only → only its scope.
  - `TestSetCapsBoundInvalidScope`: `delegate_scope` with `vis_base` set and `vis_base_present = 0` → `invalid_scope`, record unchanged.
  - `TestCascadeWithBoundClampsTwoLevels`: T → C → G, C and G with caps `file_read,cancel`, scope `subtree+vis-ids:X`, bound `file_read,cancel` / `subtree`; `caps set T --delegate-caps file_read --delegate-scope subtree --cascade` → C and G: caps `file_read`, `vis-ids:X` dropped, bound caps `file_read`.
  - `TestCascadeCapsOnlyLeavesBoundUntouched` (D19, Review Focus 2): T with the default bound, child C holding `file_read`; `caps set T --caps spawn,file_read --cascade` → C keeps `file_read` and its bound.
  - `TestBoundOnlyChangeClosesNoConns` (D14): `DropConnsForPrincipal` counted by a stub; a bound-only `caps set` → 0 calls, `conns_closed = 0`; a bound `--cascade` that clamps C's caps → C's conns dropped.
- [ ] **Step 2: Run** `go test ./server/ -run 'TestSetCapsBound|TestCascadeWithBound|TestCascadeCapsOnly|TestBoundOnlyChange'` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./server/ -count=1` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(server): caps set edits the delegation bound; --cascade clamps to it when it does`.

---

### Task 6: CLI flags, the shared funnel, the refusal text

**Files:** Modify `cli/verb/spawn.go`, `cli/verb/table.go`, regenerated `cli/verb/actions_gen.go`, `cli/session_opts.go`, `cli/submit.go`, `cli/set_caps.go`, `cli/open_interactive_native.go`, `cli/open_interactive_wasm.go`, and every copy of `SpawnAction` into `SessionOpts` (`grep -rn '\.Overrides' cli tui cmd/harness-webui-wasm` lists them); Test `cli/verb/delegation_flags_test.go` (new), `cli/session_opts_delegation_test.go` (new)

**Interfaces:**
- Consumes: wire fields (Task 3).
- Produces:
  - `spawnFlags`: `{Name: "delegate-caps", FieldType: "*protocol.Capability", Convert: "parseCapsFlag", PresenceField: "DelegateCapsPresent"}`, `{Name: "delegate-scope", FieldType: "*protocol.TaskScope", Convert: "parseScopeFlag", PresenceField: "DelegateScopePresent"}`; help: "capabilities the new task may put into the tasks IT creates (default none)" / "the most scope it may give them, in --scope's grammar (default subtree)". `exactConflicts` gains both.
  - `caps set`: both flags (pointer fields `DelegateCaps`, `DelegateScope`, no presence field, as `--caps`); `AtLeastOne` → `{"caps", "scope", "delegate-caps", "delegate-scope"}`. `caps set-defaults` and `scope`: both flags, help "… future spawns carry by default".
  - `cli.SessionOpts`: `DelegateCaps protocol.Capability`, `DelegateScope protocol.TaskScope`, `DelegateCapsPresent`, `DelegateScopePresent bool` (doc: resume only, like `ScopePresent`); `buildSubmitRequest` / `buildOpenInteractiveRequest` set the values and bits.
  - `cli.SetCapsOpts`: `DelegateCaps *protocol.Capability`, `DelegateScope *protocol.TaskScope`; nil = keep.
  - Both `OpenInteractive` clients print `ErrorMsg` when non-empty for `scope_not_permitted`, else today's fixed text.

- [ ] **Step 1: Write the tests**:
  - `TestDelegateFlagsOnEverySpawnVerb`: for `submit`, `session new` on CLI, TUI and WebUI surfaces: `--delegate-caps file_read --delegate-scope descendants` → action fields equal `file_read`, `{Base: subtree, ExcludeSelf: true}`, both presence true.
  - `TestDelegateFlagsConflictWithExact`: `submit --resume <id> --exact --delegate-caps none` → error naming both flags.
  - `TestCapsSetAcceptsBoundAlone`: `caps set <id> --delegate-scope none` parses (AtLeastOne satisfied); `caps set <id>` alone still refused.
  - `TestSetDefaultsDelegateFlags`: `caps set-defaults --delegate-caps file_read` and `scope --delegate-scope subtree` parse.
  - `TestSessionOptsCarryBound`: `buildSubmitRequest` and `buildOpenInteractiveRequest` from a `SessionOpts` with a non-zero bound and both bits → request fields and `DelegateCapsPresent()` / `DelegateScopePresent()` set.
  - `TestScopeSpecRoundTripsBound`: `ScopeSpec` of `descendants`, `none+ids:<id>`, `global/subtree` re-parses via `ParseScope` to the same `TaskScope`.
- [ ] **Step 2: Run** `go test ./cli/... -run 'TestDelegate|TestCapsSetAcceptsBoundAlone|TestSetDefaultsDelegateFlags|TestSessionOptsCarryBound|TestScopeSpecRoundTripsBound'` (env scrub per Global Constraints) — Expected: FAIL.
- [ ] **Step 3: Implement**; regenerate with `go generate ./cli/verb/`.
- [ ] **Step 4: Run** `go test ./cli/... -count=1` — Expected: PASS, including `TestNoHandWrittenVerbFlagSetsRemain` and the verb invariant tests.
- [ ] **Step 5: Commit** `feat(cli): --delegate-caps / --delegate-scope on spawn, caps set and the session defaults`.

---

### Task 7: CLI display — `ls`, JSON, `whoami`, `caps` catalog, `caps audit`

**Files:** Modify `cli/list.go`, `cli/whoami.go`, `cli/verb/caps.go`, `cli/caps_audit.go`, `cli/caps_audit_render.go`; Test `cli/delegation_display_test.go` (new)

**Interfaces:**
- Consumes: `TaskInfo` / `WhoAmIResponse` fields (Task 3).
- Produces:
  - `ls` row tokens `delegate=<CapsLabel> delegate-scope=<ScopeLabel>` on EVERY row, default included (`delegate=none delegate-scope=subtree`), placed after `scope=`.
  - `ls --json` / `session ls` keys `delegate_caps`, `delegate_scope`, in the forms `caps` / `scope` use.
  - `whoami`: both lines on both branches; operator prints `delegate=all delegate-scope=global` from the response.
  - `WriteCaps`: a `DELEGATION` section after `SCOPE`: the rule `child = your caps ∩ your delegate ∩ requested`, the default (`delegate=none delegate-scope=subtree`: your children hold nothing; you may name your own and your descendants' ids), that `--delegate-scope` takes the `--scope` grammar, and that an id "outside what you may delegate" is the operator's to widen with `caps set --delegate-scope`.
  - `AuditHolder.Delegates bool` — holds the bit AND `delegate_caps` contains it; rendered as a ` deleg` mark on the holder line (CLI, TUI modal, WebUI read the same field).

- [ ] **Step 1: Write the tests**: `TestLsRowShowsDefaultBound` (zero bound → `delegate=none delegate-scope=subtree` present); `TestLsJSONCarriesBound`; `TestWhoamiOperatorBound`; `TestCapsCatalogHasDelegation` (`WriteCaps` output contains `DELEGATION` and `--delegate-scope`); `TestAuditMarksDelegators` (holder with bit in both → `Delegates`, rendered line contains `deleg`; holder with the bit only in caps → no mark).
- [ ] **Step 2: Run** `go test ./cli/... -run 'TestLsRowShowsDefaultBound|TestLsJSONCarriesBound|TestWhoamiOperatorBound|TestCapsCatalogHasDelegation|TestAuditMarksDelegators'` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./cli/... -count=1` — Expected: PASS (update golden/listing tests whose rows gain the tokens; ledger them).
- [ ] **Step 5: Commit** `feat(cli): show the delegation bound in ls, whoami, caps and caps audit`.

---

### Task 8: TUI

**Files:** Modify `tui/client.go`, `tui/app.go`, `tui/dispatch.go`, `tui/overlays.go`, `tui/authoritypicker.go`, `tui/detail.go`, `tui/capsauditmodal.go`; Test `tui/session_opts_test.go`, `tui/authoritypicker_delegation_test.go` (new), `tui/detail_test.go`

**Interfaces:**
- Consumes: `SessionOpts` fields (Task 6), `TaskInfo` fields (Task 3), `AuditHolder.Delegates` (Task 7).
- Produces:
  - `tui.Authority` gains `DelegateCaps protocol.Capability`, `DelegateScope protocol.TaskScope`, `DelegateCapsPresent`, `DelegateScopePresent bool`; `Authority.opts` copies them (the one builder; `tui/session_opts_test.go` already fails on a second `cli.SessionOpts{` literal).
  - `App.sessionDelegateCaps`, `App.sessionDelegateScope` beside `sessionCaps` / `sessionScope`; fresh spawns read them; `SetDefaultsAction`'s new fields write them; resume builds never read them (item 26).
  - Picker: in spawn (session defaults), re-grant and set-defaults modes, a `delegation` section after the scope rows built from the same row kinds — caps chips, base radio row with exclude-self, visibility radio row, the task checklist for action ids and for view-only ids — each row marked as belonging to the section. `Result()` additionally returns `delegateCaps protocol.Capability, delegateScopeSpec string` (spec through `cli.ScopeLabel`). Re-grant prefills from the target's `DelegateCaps` / `DelegateScope`; applying sends both bound halves (`SetCapsOpts.DelegateCaps/DelegateScope` non-nil) only when the operator changed them.
  - `formatTaskDetail`: lines `delegate: <CapsLabel>` and `delegate-scope: <ScopeLabel>` after the scope line. The task table is not changed (spec: omitted).
  - Caps audit modal renders `deleg`.

- [ ] **Step 1: Write the tests**:
  - `TestAuthorityOptsCarriesBound`: `Authority{…bound…}.opts(sessionRequest{})` → all four fields.
  - `TestResumeNeverSendsSessionDefaultBound` (Review Focus 4): with `sessionDelegateCaps = file_read`, the `r` and `u` request builds carry `DelegateCapsPresent == false` and zero bound.
  - `TestPickerDelegationSection`: open re-grant on a task with bound `file_read` / `descendants`; the section's chips and radios show it; toggling `cancel` in the section changes `delegateCaps` and not `caps`; `Result()` returns `"descendants"` until changed.
  - `TestDetailShowsBound`: default bound → `delegate: none` and `delegate-scope: subtree` lines.
- [ ] **Step 2: Run** `go test ./tui/ -run 'TestAuthorityOptsCarriesBound|TestResumeNeverSendsSessionDefaultBound|TestPickerDelegationSection|TestDetailShowsBound'` — Expected: FAIL.
- [ ] **Step 3: Implement.** Batched `KeyRunes` (checklist item 5) apply to the new rows as to the existing ones.
- [ ] **Step 4: Run** `go test ./tui/ -count=1` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(tui): delegation bound in the picker, session defaults and the detail popup`.

---

### Task 9: WebUI and the wasm bridge

**Files:** Modify `cmd/harness-webui-wasm/main.go`, `webui/index.html`, `webui/static/main.js`; Test `webui/static/delegation_test.mjs` (new, `node --test` against the real wasm bridge as `cmd_test.mjs` does — add it to the `js-test` target), Playwright check for what only the page shows

**Interfaces:**
- Consumes: `TaskInfo` fields (Task 3), `SessionOpts` / `SetCapsOpts` fields (Task 6), `AuditHolder.Delegates` (Task 7).
- Produces:
  - Snapshot task fields: `delegateCaps` (label), `delegateCapsBits`, `delegateScope` (label), `delegateScopeBase`, `delegateScopeVisBase`, `delegateScopeIds`, `delegateScopeVisIds` (raw, as `capsBits` / `scopeBase` / `scopeIds`).
  - Bridge: submit / open-interactive opts accept `delegateCaps` (bits) and `delegateScope` (spec string, parsed with `ParseScope`) plus `delegateCapsPresent` / `delegateScopePresent`; set-caps opts accept `delegateCaps` / `delegateScope` (absent = keep).
  - `main.js`: `spawnDelegateCaps`, `spawnDelegateScope` beside `spawnCaps` / `spawnScope`, fed to fresh spawns only; `buildScopeControls(root, state, onChange)` for the delegation sections; a delegation `<details>` in the compose panel and the re-grant dialog (`#regrant-modal`) built from `buildCapChips` + `buildScopeControls`, serialised through `harness.scopeSpec`, re-grant prefilled from the raw snapshot fields and sending a bound half only when changed; the task row meta and task sheet show `delegate=… delegate-scope=…`; the caps audit holder line shows `deleg`; `caps set-defaults --delegate-*` typed in the command input updates the compose state.

- [ ] **Step 1: Write `webui/static/delegation_test.mjs`**: through the bridge, `submit --delegate-caps file_read --delegate-scope descendants x` parses with both flags set; `caps set-defaults --delegate-scope none` parses; `harness.scopeSpec({base: "subtree", excludeSelf: true, ids: [], visBase: "", visIds: []})` returns `"descendants"`.
- [ ] **Step 2: Run** `make js-test` with the file listed in the target — Expected: PASS already, because the parse comes from Task 6's table. These assertions pin the bridge against regression; they are not this task's RED. This task's new behaviour (snapshot fields, bridge opts, page controls) has no node-reachable seam and is verified in Step 4's Playwright pass.
- [ ] **Step 3: Implement** the bridge and `main.js`; `make webui-build`.
- [ ] **Step 4: Run** `make js-test` — Expected: PASS. Then against a dummy harness (`scripts/dummy-harness.sh up --detach --agent fake --name deleg-webui`), in Playwright at 1280px and 390px: the snapshot's task objects carry the seven fields (read with `browser_evaluate`); set a delegation bound in compose, spawn, the new row shows it; open re-grant on that task, the section is prefilled; resume with the default bound set sends no bound (the task's bound in `ls` unchanged afterwards). Screenshots under the worktree's `.superpowers/`, reported by path (they are deliverables).
- [ ] **Step 5: Commit** `feat(webui): delegation bound in compose, re-grant and the task views`.

---

### Task 10: docs, parity walk, live verification, land

**Files:** Modify `README.md` ("Capabilities and scope"), `runner/agentskills/supervising-workers/SKILL.md` and its two mirrors (`.claude/skills/…`, `.agents/skills/…` — kept byte-identical), `.claude/skills/surface-parity-checklist/firing-log.md`

- [ ] **Step 1: README**: the bound, its default, `--delegate-caps` / `--delegate-scope`, `caps set` editing it, the D19 cascade rule, `delegate=` on `ls`.
- [ ] **Step 2: Skill** per the spec's Agent-skill row: `creator_caps ∩ delegate_caps ∩ requested`; default delegates no bits, check `whoami` before promising a worker a bit; "outside what you may delegate" is the operator's to widen; the `--caps all` sentence becomes "everything you may delegate"; the own-id and sibling recipes gain "the bit they grant must be in your `delegate`, or it is dropped" and the `descendants` note. Read each file in full before editing it (user rule); `cmp` the three copies after.
- [ ] **Step 3: Walk `surface-parity-checklist` items 1–39 and S1–S6** against the spec's Surfaces table, one verdict each, logged in `firing-log.md`; any `done` without a file is a gap to fix now.
- [ ] **Step 4: Full suite**: `env -u HARNESS_SERVER_PUBKEY -u HARNESS_SERVER_CID -u HARNESS_AUTH_TICKET -u HARNESS_TASK_ID make test` — Expected: PASS (the known server flake family may need one package re-run; report it by name if it fires).
- [ ] **Step 5: Live**, against `scripts/dummy-harness.sh up --detach --agent fake --name deleg` (eval its `env`, then `unset HARNESS_SERVER_PUBKEY`), the spec's six steps in the spelling `--help` prints; agent-side commands through the `bash` profile. Record each command and its output line in the ledger. `down` afterwards.
- [ ] **Step 6: `scripts/wire-skew-check.sh`** with `TMPDIR` under `.superpowers/` — Expected: no unexpected skew beyond the formats in § Wire.
- [ ] **Step 7: Commit** docs; then land per the landing policy (Mode A: code commits before the unlanded doc commits, FF-push by SHA, `git -C <main> merge --ff-only <SHA>`, `make build` in the main checkout), and tell the operator to restart the server right away (wire change, server first).
