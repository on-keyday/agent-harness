# Grant Conditions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A scope override can carry a condition — `ActionSet` on `exec_run`, `PortRange` on `forward_local` / `forward_remote` — that bounds which object the bit reaches, attenuated by containment, stored with the task, and enforced fail-closed.

**Architecture:** The condition lives on `authority.ScopeOverride` (`Cond *Condition`), so every place that already carries an override — the task store, the WAL, the schedule store, the authority algebra — carries it by carrying the override. On the wire an override gets one marker bit; bodies travel on a client-created stream into the five grant-writing requests and on the existing streamed reads (`TaskInfo`, `ScheduleDetail`). The server's per-cap gate (`scopeSetWith`) reaches nothing for a conditioned bit; `authorizeObject` is the only way past it.

**Tech Stack:** Go, `.bgn` → `make protoregen`, wasm (`cmd/harness-webui-wasm`), WebUI JS.

**Spec:** `docs/superpowers/specs/2026-11-26-grant-conditions-and-bound-enrollment-design.md` — C4–C7, C9, C14, § Grant conditions. Second of three plans. The actions plan comes later and adds the `exec_run` object path; until then an `ActionSet`-conditioned `exec_run` reaches nothing, which is C7's fail-closed state, not a bug.

## Global Constraints

- **Naming:** the wire calls these *grant* conditions everywhere (`grant_conditions_stream_id`, `GrantCondition`, `--grant-condition`), because `OpenInteractiveRequest` already has `conditions_present` / `conditions :BackgroundConditions` for a background run's end conditions. The spec's `conditions_stream_id` is renamed accordingly (Task 1 edits the spec).
- Limits (C6): at most 8 conditions per request, 32 action ids per `ActionSet`, 8 spans per `PortRange`.
- Applicability (C9, C14): `action_set` only on a mask ⊆ `exec_run`; `port_range` only on a mask ⊆ `forward_local|forward_remote`. Anything else is refused at write.
- A condition attaches to an override, never to the base clause. A client that names a condition for a bit must also name a `--scope-for` whose mask is exactly the condition's.
- Wire changes appended only; run `scripts/wire-skew-check.sh`. Use `make test`. Stage by path. Do not land.

## Review Focus

- **A conditioned grant splits.** `Reach.BoundRanks` (`authority/reach.go:48-98`) rebuilds overrides piece by piece; a piece must keep its source's condition, or a clamp silently drops it and the bit becomes unconditioned (anti-goal 1). Pinned in Task 2 (`TestBoundRanksKeepsConditionOnEveryPiece`).
- **A parent's condition reaches a child that named none, including through the base clause.** A child asking `exec_run` in its base scope under a parent whose `exec_run` is conditioned must come out with an override carrying the parent's condition. Pinned in Task 2 (`TestAttenuateInheritsConditionIntoBaseBits`).
- **An unreadable body is "reaches nothing", everywhere.** A marker with no body from the wire, a WAL record whose `cond` has an unknown kind: both become `Condition{Kind: 0}`, which allows nothing and contains nothing. Pinned in Tasks 2 and 3.
- **A request whose stream fails is refused whole** — no task, credential, schedule or caps change is written. Pinned per handler in Task 5.
- **The second bit of a forward:** a `PortRange` on `forward_local` alone leaves `forward_remote` unconditioned in the same scope; the label must make that visible (Task 6 display test).

---

### Task 1: Wire

**Files:**
- Modify: `runner/protocol/message.bgn`; regenerate `runner/protocol/message.go`
- Modify: `docs/superpowers/specs/2026-11-26-grant-conditions-and-bound-enrollment-design.md` (rename `conditions_stream_id` → `grant_conditions_stream_id`, one sentence why)
- Test: `runner/protocol/grant_conditions_wire_test.go`

**Interfaces — Produces:**

```
format ActionID:            id :[16]u8            # used by the actions plan too
enum ConditionKind:         :u8  action_set = 1, port_range = 2
format PortSpan:            lo :u16, hi :u16       # inclusive, lo <= hi
format GrantCondition:
    caps :Capability                               # the override's exact mask
    kind :ConditionKind
    match kind:
        ConditionKind.action_set => actions_len :u8, actions :[actions_len]ActionID
        ConditionKind.port_range => spans_len :u8,   spans :[spans_len]PortSpan
format GrantConditions:     conditions_len :u8, conditions :[conditions_len]GrantCondition
```

- `ScopeOverride`: `exclude_self :u1, conditioned :u1, reserved :u6` (was `reserved :u7`).
- `CredentialIssueRequest`, `CredentialRekeyRequest`, `SetCapsRequest`, `ScheduleSpec`: append `grant_conditions_stream_id :u64` (0 = none).
- `OpenInteractiveExt`: `grant_conditions_present :u1` from `reserved :u3` (→ `reserved :u2`), and `if grant_conditions_present == 1: grant_conditions_stream_id :u64` at its end.
- `TaskInfo`, `ScheduleDetail`: append `grant_conditions_len :u8, grant_conditions :[grant_conditions_len]GrantCondition`.
- Status values appended: `OpenInteractiveStatus.bad_grant_conditions`, `CredentialStatus.bad_grant_conditions`, `SetCapsStatus.bad_grant_conditions`, `ScheduleStatus.bad_grant_conditions` — the body could not be read or decoded, exceeds a limit, names a mask that is not a conditioned override of the request, leaves a conditioned override without a body, or puts a kind on an inapplicable bit.

- [ ] **Step 1:** Write `TestGrantConditionsRoundTrip` (both kinds, limits' worst case), `TestScopeOverrideConditionedBitRoundTrip` (and that `exclude_self` is unaffected), `TestOpenInteractiveExtCarriesGrantConditionsStream`.
- [ ] **Step 2:** Run; expected FAIL (undefined types).
- [ ] **Step 3:** Edit the schema as above, with comments naming C6/C9; `make protoregen`; edit the spec's name.
- [ ] **Step 4:** Run the three tests and `go vet ./...`; expected PASS (callers that build `ScopeOverride` still compile: the new bit defaults to 0).
- [ ] **Step 5:** Commit `runner/protocol/message.bgn runner/protocol/message.go runner/protocol/grant_conditions_wire_test.go` and the spec — "protocol: grant conditions on the wire (C6, C9)".

---

### Task 2: `authority` — the condition and its algebra

**Files:**
- Create: `authority/condition.go`, `authority/condition_test.go`
- Modify: `authority/scope.go` (`ScopeOverride`, `Validate`, new `CondFor`), `authority/reach.go` (`BoundRanks`), `authority/attenuate.go` (`Attenuate`, `ClampUnder`, `IsNarrowing`), `authority/delegate.go` (`ceiling`), `authority/whatif.go` (`sameScope`), `authority/wire.go`

**Interfaces — Produces:**

```go
type PortSpan struct{ Lo, Hi uint16 }
type Condition struct {
    Kind    protocol.ConditionKind // 0: an unreadable body — allows nothing, contains nothing (C7)
    Actions [][16]byte             // action_set: sorted, unique
    Spans   []PortSpan             // port_range: sorted, merged
}
type Object struct {
    Kind   protocol.ConditionKind
    Action [16]byte
    Port   uint16
}
func (c *Condition) Allows(o Object) bool          // nil: true
func (c *Condition) Contains(d *Condition) bool    // nil c: true; non-nil c, nil d: false
func (c *Condition) Intersect(d *Condition) *Condition
func (c *Condition) Clone() *Condition
func ApplicableBits(k protocol.ConditionKind) protocol.Capability
func (s Scope) CondFor(bit protocol.Capability) *Condition   // the covering override's, or nil
func ConditionsFromWire(body protocol.GrantConditions, s *Scope) error  // attach bodies; C7 marker rules
func (s Scope) ConditionsToWire() []protocol.GrantCondition
```

`ScopeOverride` gains `Cond *Condition`. `ScopeFromWire` sets `Cond = &Condition{}` (kind 0) on an override whose `conditioned` bit is set; `ConditionsFromWire` then replaces it with the body, and refuses a body with no matching conditioned override or a conditioned override left at kind 0. `OverridesToWire` sets `conditioned` when `Cond != nil`.

- [ ] **Step 1: Write the failing tests** in `authority/condition_test.go`:
  - `TestConditionContains`: `{a,b} ⊇ {a}`; `{a} ⊉ {a,b}`; spans `8000-8100 ⊇ 8050-8060`, `⊉ 7999-8000`; nil contains everything; non-nil does not contain nil; kind 0 contains nothing and is contained only by nil; different kinds never contain each other.
  - `TestConditionIntersect`: actions → set intersection; spans → overlap; empty result keeps the kind with no members (allows nothing).
  - `TestConditionAllows`: kind 0 allows nothing; a port at a span edge is allowed.
  - `TestValidateConditionApplicability`: `action_set` on `exec_run|cancel` refused; `port_range` on `forward_link` refused; over-limit lists refused.
  - `TestBoundRanksKeepsConditionOnEveryPiece`: an override `{forward_local|forward_remote, global, Cond}` under a creator whose `forward_remote` is subtree splits into two pieces, both with `Cond`.
  - `TestAttenuateRefusesWiderCondition`: creator `exec_run` conditioned `{a}`, child asks `{a,b}` → `ok == false`, offender names `exec_run`.
  - `TestAttenuateInheritsConditionIntoOverride` and `TestAttenuateInheritsConditionIntoBaseBits`: child names no condition → its override (or a new override carved from its base clause, as `BoundRanks` already carves) carries the creator's condition.
  - `TestClampUnderIntersectsConditions`: a descendant's `{a,b}` under a narrowed parent `{a}` becomes `{a}`.
  - `TestIsNarrowingSeesACondition`: same scope, condition added or narrowed → narrowing; widened or removed → not.
  - `TestConditionsWireRoundTrip`: `ScopeFromWire` + `ConditionsFromWire` ∘ `OverridesToWire` + `ConditionsToWire` is the identity; a marker with no body is refused by `ConditionsFromWire`.
- [ ] **Step 2:** Run `go test ./authority/`; expected FAIL.
- [ ] **Step 3: Implement.**
  - `condition.go`: the type and methods above; normalisation in a constructor used by `ConditionsFromWire`.
  - `Validate`: applicability and limits per override with `Cond != nil`.
  - `BoundRanks`: the kept piece and every moved piece copy the source override's `Cond` (`withEntry` gains a `cond *Condition` parameter, nil for pieces carved from the base clause).
  - `Attenuate` (after `BoundRanks`, before the id checks): for each exercised bit whose creator `CondFor` is non-nil, the child's covering piece must have a `Cond` the creator's contains — refuse otherwise; a child piece with nil `Cond` takes a clone of the creator's; a piece whose bits map to different creator conditions is split by condition; base-clause bits under a conditioned creator become an override with the creator's condition. The rebuild at `attenuate.go:47-51` copies `Cond`.
  - `ClampUnder`: the same walk, intersecting instead of refusing.
  - `IsNarrowing`: per exercised bit, also compare `CondFor` (narrowed when `before` contains `after` and they differ, or `before` nil and `after` non-nil).
  - `ceiling` (`delegate.go:222-226`) and `sameScope` (`whatif.go:375-383`) carry / compare `Cond`.
- [ ] **Step 4:** Run `go test ./authority/` and `TestNoSecondScopeImplementation`; expected PASS.
- [ ] **Step 5:** Commit the authority files — "authority: grant conditions, attenuated by containment (C5)".

---

### Task 3: Server storage

**Files:**
- Modify: `server/wal.go` (`WALScopeOverride`, both JSON paths), `server/scope.go` (`applyScopeToWAL`, `scopeFromWAL`), `server/schedule_store.go` (`scopeOverrideJSON`, `cloneScope`, `scopeToJSON`, `scopeFromJSON`), `server/task_handler.go` (`toTaskInfo`, whoami mapper), `server/schedule_handler.go` (`scheduleDetail`)
- Test: `server/wal_test.go`, `server/schedule_store_test.go`, `server/mapper_completeness_test.go`

**Interfaces — Consumes:** Task 2. **Produces:** `WALCondition{Kind uint8; Actions []string (hex); Spans [][2]uint16}` as `cond,omitempty` on `WALScopeOverride`, and the same shape on `scopeOverrideJSON`.

- [ ] **Step 1: Failing tests:** `TestWALEventJSONRoundTripCopiesEveryField` extended with a conditioned override; `TestReplayKeepsConditions` (`task_created` then `task_caps_changed`); `TestWALUnknownConditionKindReplaysAsReachingNothing`; schedule store round trip with a condition, and `schedulesFixtureV1` still loading (no `cond` = nil); `TestToTaskInfoMapsEveryField` / `TestWhoamiResponseMapsEveryField` updated for the bodies and the marker; `cloneScope` deep-copies `Cond` (mutating the clone leaves the source).
- [ ] **Step 2:** Run; expected FAIL.
- [ ] **Step 3:** Implement the JSON shapes and mappers; `toTaskInfo` and `scheduleDetail` set the bodies from `ConditionsToWire`; whoami sends the marker only (C6).
- [ ] **Step 4:** Run `go test ./server/ -run 'WAL|Replay|Schedule.*Store|MapsEveryField|Clone'`; expected PASS.
- [ ] **Step 5:** Commit — "server: persist grant conditions with the override".

---

### Task 4: Server gates

**Files:**
- Modify: `server/capabilities.go` (`scopeSetWith`, new `authorizeObject`), `server/task_handler.go` (the `OpenPortForward`, `RegisterPortForward` and `KillPortForward` cases, `:836-904`)
- Test: `server/grant_condition_gate_test.go`

**Interfaces — Produces:** `func (h *TaskHandler) authorizeObject(cid objproto.ConnectionID, want protocol.Capability, targetHex string, obj authority.Object) bool` — `hasCap`, then the scope test ignoring the condition, then `CondFor(want).Allows(obj)`.

- [ ] **Step 1: Failing tests:**
  - `TestConditionedBitReachesNothingThroughEveryGate`: a principal whose `exec_run` and `forward_local` are conditioned gets false from `inScope`, `authorize` and `scopeSet` for both, on a target inside its scope.
  - `TestAuthorizeObjectPassesAnAllowedPort` / `…RefusesAnotherPort`.
  - `TestPortRangeHoldsOnEachForwardOpen`: register with an allowed `target_port` succeeds; a per-connection `OpenPortForward` naming another `RemotePort` through that registration is refused; one naming an allowed port succeeds.
  - `TestKillPortForwardUsesTheRegistrationPort`.
  - `TestUnconditionedForwardsUnchanged`: the same three paths for an unconditioned holder behave as before.
- [ ] **Step 2:** Run; expected FAIL.
- [ ] **Step 3:** Implement. In `scopeSetWith`, after resolving `scope := t.Scope` (`:242`), return the empty set when `scope.CondFor(want) != nil`. The three forward cases call `authorizeObject` with `Object{Kind: port_range, Port: …}`: register → `target_port` (local) / `bind_port` (remote); open → the request's `RemotePort`; kill → the registration's port. `exec_run`, exec kill and shared transcripts need no edit: they now deny a conditioned holder through `scopeSetWith` (the actions plan adds their object).
- [ ] **Step 4:** Run the new tests and `go test ./server/ -run 'Scope|Forward|Capab'`; expected PASS.
- [ ] **Step 5:** Commit — "server: conditioned bits reach nothing but through authorizeObject (C7)".

---

### Task 5: Server intake — the five grant-writing requests

**Files:**
- Create: `server/grant_conditions_read.go`
- Modify: `server/task_handler.go` (the open prompt-stream goroutine `:660-690`, `openGrantRequest`, `grantRequest` `:1421-1430`), `server/background_open.go`, `server/credential_handler.go` (`issueGrantRequest`, `rekeyGrantRequest`), `server/set_caps_handler.go` (`operatorCapsUpdate`, `setCapsGrantRequest`), `server/schedule_handler.go` (`withSpecPrompt` `:220-250`, `specGrantRequest`)
- Test: `server/grant_conditions_intake_test.go`

**Interfaces — Produces:** `func readGrantConditions(conn ConnHandle, streamID uint64) (protocol.GrantConditions, error)`, modelled on `readExecBody` (`server/exec_run.go:39`): bounded read, `DecodeExact`, limits. `grantRequest` gains `Conditions protocol.GrantConditions`; each `…GrantRequest(req)` builder applies them with `authority.ConditionsFromWire` to the scope it converts.

- [ ] **Step 1: Failing tests**, one table per request kind (open fresh foreground, open fresh background, open resume, credential issue, credential rekey, set caps by operator, set caps by task, schedule create, schedule edit):
  - a valid body → the stored grant carries the condition;
  - unreadable stream, undecodable body, over-limit body, body for a mask that is not a conditioned override, conditioned override with no body, `port_range` on `exec_run` → `bad_grant_conditions`, and nothing written (no task / credential / schedule; set_caps leaves the old grant);
  - a condition wider than the caller's → the request's existing containment refusal (`scope_not_permitted`; set_caps: what its task path answers for a `grantFor` refusal today).
- [ ] **Step 2:** Run; expected FAIL.
- [ ] **Step 3:** Implement. Open: read the grant-conditions stream in the goroutine that already reads the prompt, before `handleOpenInteractive`. Credential, set_caps, schedule: read it in a goroutine before the handler body, as `withSpecPrompt` does for a schedule's prompt; on a denied request, discard the stream as `discardPayloadStream` does (`task_handler.go:530`).
- [ ] **Step 4:** Run; expected PASS.
- [ ] **Step 5:** Commit — "server: grant conditions arrive on a stream in every grant-writing request".

---

### Task 6: Clients — flag, request, display

**Files:**
- Modify: `cli/verb/scope.go` (new `ParseGrantCondition`, `MergeGrantCondition`, `ConditionsLabel`), `cli/verb/spawn.go` (flag on the spawn grammar), `cli/verb/table.go` (`caps set` row), `cli/verb/gen` (regenerate `actions_gen.go`), `cli/payload_stream.go` users: `cli/runmode.go` `roundTripOpenRequest`, `cli/set_caps.go`, `cli/credential.go`, `cli/schedule.go`; display in `cli/list.go`, `cli/whoami.go`, `cli/schedule.go` `ScheduleDetailParts`, `cli/caps_audit*.go`
- Test: `cli/verb/scope_test.go`, `cli/grant_conditions_send_test.go`, `cli/list_test.go`

**Interfaces — Produces:** `--grant-condition CAPS=actions:<32hex>[,<32hex>…]` and `--grant-condition CAPS=ports:<p>[-<q>][,…]`, repeatable, on `session new`, `schedule new/edit`, `credential new/rekey`, `caps set`. Not on `caps what-if` / `caps set-defaults` (documented omission). `SessionOpts`, `SetCapsOpts`, `ScheduleSpecOpts` and the credential opts gain `GrantConditions []protocol.GrantCondition`; when non-empty the builder sets `conditioned` on the override whose mask equals each condition's `caps` and sends the body on a send stream through `TaskControlWithPayload` (`cli/payload_stream.go:38`) — for the open, next to the prompt stream in `roundTripOpenRequest`.

- [ ] **Step 1: Failing tests:** `TestParseGrantCondition` (both forms; a port above 65535, `lo > hi`, an unknown kind, an action id that is not 32 hex refused); `TestGrantConditionNeedsMatchingScopeFor` (no `--scope-for` with that exact mask → error naming the flag to add); `TestOpenSendsGrantConditionsOnAStream` (fake transport sees the request with `grant_conditions_present` and then the body); `TestConditionsLabel` and the `ls` line (`forward_local=ids:<T> if ports:8000-8100`; a conditioned override with no body renders `if (condition unknown)`); `ls --json` / `whoami --json` gain `conditions_by_cap`.
- [ ] **Step 2:** Run; expected FAIL.
- [ ] **Step 3:** Implement; regenerate the verb actions with the generator in `cli/verb/gen`.
- [ ] **Step 4:** Run `go test ./cli/...`; expected PASS.
- [ ] **Step 5:** Commit — "cli: --grant-condition, sent on a stream, shown beside scope_by_cap".

---

### Task 7: TUI and WebUI

**Files:**
- Modify: `tui/client.go` (`Authority` gains `GrantConditions`; `opts()`), `tui/app.go` (`spawnAuthority`), `tui/overlays.go` / `tui/authoritypicker.go` (carry them), `tui/detail.go` (`formatTaskDetail` line); `cmd/harness-webui-wasm/main.go` (`overridesFromOpts` sibling for conditions, `harnessSetCaps`, `harnessStartInteractive`, `harnessParseAuthority`, snapshot fields `conditionsByCap`); `webui/static/main.js` (override-row UI `:4849-4935` gains a condition field; task row label), `webui/static/task_models.js` (`sessionScopeFields`, `resumeRegrantFlags`, `composeScheduleTokens`)
- Test: `tui/*_test.go` for the picker carrying conditions and the detail line; `webui/static/*.test.mjs` for the override row and the label

- [ ] **Step 1:** Walk `surface-parity-checklist` items 1–39 for `--grant-condition` and the condition display; write the verdict per number into that skill's `firing-log.md`. Every surface either implements it or records why not.
- [ ] **Step 2:** Failing tests for each "implemented" verdict.
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** `go test ./tui/ ./cmd/harness-webui-wasm/...`, `make js-test`, `make wasm-check`; expected PASS.
- [ ] **Step 5:** Run the TUI in a dummy PTY and the WebUI through Playwright against `scripts/dummy-harness.sh`: set a `PortRange` with `caps set`, see it on the task row in both, open a session with one from each surface's form.
- [ ] **Step 6:** Commit — "tui, webui: grant conditions".

---

### Task 8: Whole-suite and end-to-end

- [ ] **Step 1:** `make test` — exit 0, no `FAIL`.
- [ ] **Step 2:** `scripts/wire-skew-check.sh` — `PASS`.
- [ ] **Step 3:** Dummy-harness E2E, in the exact spelling `--help` prints: a bash task T; `caps set --caps forward_local --scope subtree --scope-for forward_local=ids:<T> --grant-condition forward_local=ports:8000-8100 <child>`; from the child, `forward` to T on 8050 works and on 9000 is refused; `ls` shows the condition; restart the dummy server and see it survive (WAL).
