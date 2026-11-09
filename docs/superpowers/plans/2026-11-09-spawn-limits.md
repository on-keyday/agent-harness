# Spawn limits Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every task carries a `spawn_depth` (generations that may still be created below it, decremented per spawn) and a `max_descendants` (records its subtree may hold), both enforced by the server when a task is created, editable with `caps set`, and shown on every surface.

**Architecture:** One wire format, `SpawnLimit` (value + `limited` bit), carries both limits everywhere. The two values join `authority.Grant`, so they ride the existing `Create` / `GrantUpdate` / `SetCaps` / `Resume` / WAL paths beside caps, scope and the delegation bound. The creation check runs inside `TaskStore`'s lock through one new entry point, `CreateWithinLimits`, which the two production create sites call; a guard test keeps them on it.

**Tech Stack:** Go 1.26; `.bgn` regenerated with `make protoregen` (brgen kit cached under `~/.cache/brgen-kit`); `cli/verb` codegen (`go generate ./cli/verb/`); the wasm WebUI bridge; `make` targets.

**Spec:** `docs/superpowers/specs/2026-11-09-spawn-limits-design.md` — D1–D19, § The rule, § Wire, § Persistence, § Surfaces, § Testing.

## Global Constraints

- Both limits default to unbounded: the zero value of every new field, an omitted flag, and a legacy WAL record all mean unbounded (D8).
- `desc(A)` counts every record in `A`'s subtree under the current `creator_task_id` links, whatever its status, `A` excluded (D2, D3, D4).
- Creation: refuse if the creator's `spawn_depth` is limited and 0; then refuse if, for any ancestor `A` (creator first) with a limited `max_descendants`, `desc(A) + 1 > A.max_descendants`. Otherwise the child gets `spawn_depth = min(requested, creator.spawn_depth − 1)` and `max_descendants = requested` (§ The rule). Check and insert in ONE `TaskStore.mu` critical section (D11).
- No branch on operator identity (D7). An operator-created task has an all-zero creator, so no ancestor and an unbounded creator depth.
- A resume is never checked (D9); a resume behind `spawn_depth_present` writes `min(requested, resumer.spawn_depth − 1)` floored at 0; behind `max_descendants_present` writes the request.
- `caps set` writes either value on the target only; `--cascade` never changes a limit (D13). A limit-only change closes no connection.
- Refusal status `spawn_limit` with `error_msg` (D12), text verbatim from Task 3.
- Flags `--spawn-depth N|unbounded`, `--max-descendants N|unbounded` (D15).
- Prune is not touched (D10).
- Work in `/home/kforfk/workspace/remote-agent-harness/.harness-worktrees/70fbad4a6eb6f1e992be8a669f1bcefd`. Run tests with `env -u HARNESS_SERVER_PUBKEY -u HARNESS_SERVER_CID -u HARNESS_AUTH_TICKET -u HARNESS_TASK_ID`.

## Decisions this plan adds (spec silent or amended)

- **One wire format `SpawnLimit { limited :u1, reserved :u7, value :u16 }`** replaces the spec's separate value fields and `*_limited` bits. Both limits use it, so the CLI converter, the generated verb fields (`*protocol.SpawnLimit` — `cli/verb/gen` imports only `protocol`) and every mapper handle one type. **Amends the spec:** `spawn_depth`'s range is 0–65535 like `max_descendants`; 0–255 was an arbitrary width.
- **Presence bits take existing reserved bits:** `SubmitRequest` `reserved :u2` → `spawn_depth_present :u1`, `max_descendants_present :u1`; `SetCapsRequest` the same from its `reserved :u2`; `OpenInteractiveRequest` `reserved2 :u7` → the two bits + `reserved2 :u5`.
- **`TaskStore.Create` keeps its signature and stays unchecked** (210 call sites, almost all tests). Production calls `CreateWithinLimits` with the same parameters; `TestOnlyCreateWithinLimitsInProduction` fails on any non-test `server/*.go` calling `.Create(` on a `TaskStore`.
- **`toTaskInfo(t TaskEntry, descendants uint32)`**; `handleList` takes one `Tasks.DescendantCounts()` per call.
- **The counts and the room for the operator's `whoami`:** every `limited` bit 0, values 0.

## Review Focus

1. **Two creations racing for an ancestor's last place** must not both succeed. Pinned in Task 3 (`TestCreateWithinLimitsRace`, `-race`).
2. **A restart keeps limits** written at spawn, by a caps-only `caps set` afterwards, and by a re-granting resume. Pinned in Task 3 (`TestLimitsSurviveReplay`).
3. **A `caps set <supervisor> --caps … --cascade`** leaves every descendant's limits as they were — `ClampDescendant` builds a fresh `Grant` and would zero them. Pinned in Task 2 and Task 5.
4. **An interactive open refused by a limit** leaves no task row and opens nothing on the runner. Pinned in Task 4 (`TestOpenInteractiveRefusedLeavesNoTask`).
5. **TUI/WebUI resume paths never send the session's default limits** (checklist item 26). Pinned in Task 8 and Task 9.

---

## File Structure

| File | Change |
| --- | --- |
| `runner/protocol/message.bgn` + regenerated `message.go`; `runner/protocol/spawn_limit_wire_test.go` (new) | `SpawnLimit`; request / response / status fields |
| `authority/delegate.go`, `authority/spawn_limit.go` (new) + tests | `Grant` fields; limit arithmetic; `ChildSpawnDepth`; `ClampDescendant` keeps limits |
| `server/taskstore.go`, `server/wal.go`, `server/scope.go`, `server/spawn_limit.go` (new) + tests | entry fields; `GrantUpdate`; WAL keys; `CreateWithinLimits`; `SpawnLimitError`; `DescendantCounts`; `SpawnRoom` |
| `server/task_handler.go` | create sites; resume re-grant; `exactFields`; `toTaskInfo`; whoami |
| `server/set_caps_handler.go` | limit halves |
| `cli/verb/spawn.go`, `cli/verb/table.go`, `cli/verb/authority.go`, `cli/verb/caps.go`, regenerated `actions_gen.go` | flags, converter, catalog |
| `cli/session_opts.go`, `cli/submit.go`, `cli/set_caps.go`, `cli/open_interactive_native.go`, `cli/open_interactive_wasm.go`, `cmd/harness-cli/spawn.go` | funnel; refusal text |
| `cli/list.go`, `cli/whoami.go`, `cli/whatif.go` | display; prediction |
| `tui/client.go`, `tui/app.go`, `tui/dispatch.go`, `tui/overlays.go`, `tui/authoritypicker.go`, `tui/detail.go` | defaults, picker, detail |
| `cmd/harness-webui-wasm/main.go`, `webui/index.html`, `webui/static/main.js` | snapshot, compose, re-grant, row meta |
| `README.md`, `runner/agentskills/supervising-workers/SKILL.md` + two mirrors, `.claude/skills/surface-parity-checklist/firing-log.md`, the spec (amendment) | docs |

---

### Task 1: wire schema

**Files:** Modify `runner/protocol/message.bgn`; regenerate `runner/protocol/message.go`; Test `runner/protocol/spawn_limit_wire_test.go` (new)

**Interfaces:**
- Produces:
  - `format SpawnLimit: limited :u1, reserved :u7, value :u16` — doc: `limited = 0` is unbounded and `value` is then 0.
  - `SubmitRequest`: `spawn_depth_present :u1`, `max_descendants_present :u1` in place of `reserved :u2`; appended after `delegate_scope`: `spawn_depth :SpawnLimit`, `max_descendants :SpawnLimit`. The presence bits are resume-only.
  - `OpenInteractiveRequest`: after `delegate_scope_present`: `spawn_depth_present :u1`, `max_descendants_present :u1`, `reserved2 :u5`; then `spawn_depth :SpawnLimit`, `max_descendants :SpawnLimit`, before `if x11_enabled == 1`.
  - `SetCapsRequest`: the two presence bits from `reserved :u2`; appended `spawn_depth`, `max_descendants`.
  - `SubmitStatus`, `OpenInteractiveStatus`: append `spawn_limit = "spawn_limit"`.
  - `TaskInfo`: append `spawn_depth :SpawnLimit`, `max_descendants :SpawnLimit`, `descendants :u32`.
  - `WhoAmIResponse`: append the same three and `spawn_room :SpawnLimit`.

- [ ] **Step 1: Write `TestSpawnLimitWireRoundTrip`**: one value of each of the six request/response formats with every new field non-zero (`OpenInteractiveRequest` with `x11_enabled = 1` and a populated X11 block, so placement before the conditional block is exercised); `MustAppend` then `DecodeExact`; deep-equal.
- [ ] **Step 2: Run** `go test ./runner/protocol/ -run TestSpawnLimitWireRoundTrip` — Expected: FAIL to compile.
- [ ] **Step 3: Edit `message.bgn`; `make protoregen`.**
- [ ] **Step 4: Run** `go test ./runner/protocol/ -count=1 && go build ./...` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(protocol): spawn limits on the wire`.

---

### Task 2: the arithmetic, in `authority`

**Files:** Create `authority/spawn_limit.go`, `authority/spawn_limit_test.go`; Modify `authority/delegate.go`

**Interfaces:**
- Consumes: `protocol.SpawnLimit` (Task 1).
- Produces:
  - `func Unbounded() protocol.SpawnLimit`, `func LimitOf(n uint16) protocol.SpawnLimit`
  - `func DecLimit(l protocol.SpawnLimit) protocol.SpawnLimit` — unbounded stays; `n > 0` → `n − 1`; 0 stays 0
  - `func MinLimit(a, b protocol.SpawnLimit) protocol.SpawnLimit` — unbounded is the identity
  - `func LimitLabel(l protocol.SpawnLimit) string` — `"unbounded"` or the decimal
  - `Grant` gains `SpawnDepth, MaxDescendants protocol.SpawnLimit`
  - `func (g *Granter) ChildSpawnDepth(requested protocol.SpawnLimit) protocol.SpawnLimit` — `MinLimit(requested, DecLimit(held.SpawnDepth))`; nil `g` → `requested`
  - `ClampDescendant` returns `d.SpawnDepth`, `d.MaxDescendants` unchanged

- [ ] **Step 1: Write the tests**: `TestDecLimit` (unbounded, 0, 1, 3); `TestMinLimit` (both orders, both unbounded); `TestChildSpawnDepth` — held {unbounded, 0, 1, 3} × requested {unbounded, 0, 5} → {unbounded,0,5; 0,0,0; 0,0,0; 2,0,2}; nil granter returns the request; `TestClampDescendantKeepsLimits` (Review Focus 3): `d` with `SpawnDepth = LimitOf(2)`, `MaxDescendants = LimitOf(4)` → unchanged in the result.
- [ ] **Step 2: Run** `go test ./authority/ -run 'TestDecLimit|TestMinLimit|TestChildSpawnDepth|TestClampDescendantKeepsLimits'` — Expected: FAIL to compile.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./authority/ -count=1` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(authority): spawn-limit arithmetic and the granted depth`.

---

### Task 3: store — fields, WAL, the checked creation

**Files:** Modify `server/taskstore.go`, `server/wal.go`, `server/scope.go`; Create `server/spawn_limit.go`; Test `server/spawn_limit_store_test.go` (new), `server/spawn_limit_guard_test.go` (new)

**Interfaces:**
- Consumes: `Grant` fields (Task 2).
- Produces:
  - `TaskEntry` fields `SpawnDepth`, `MaxDescendants protocol.SpawnLimit`; `Grant()` includes them; `Create` stores them from `g`.
  - `GrantUpdate` gains `SpawnDepth, MaxDescendants bool`; `Any()` includes them; `SetCaps` / `Resume` write them behind the bits.
  - WAL keys (`WALEvent` + `walEventJSON`, `omitempty`): `spawn_depth`, `spawn_depth_limited`, `max_descendants`, `max_descendants_limited`; `func applyLimitsToWAL(depth, max protocol.SpawnLimit, ev *WALEvent)` / `func limitsFromWAL(ev WALEvent) (depth, max protocol.SpawnLimit)` in `server/scope.go`; written on `task_created` and in `writeCapsChangedLocked`; replay of both assigns them.
  - `func (s *TaskStore) CreateWithinLimits(<Create's parameters>) (string, error)` — under `s.mu`: depth check on the creator's record, then the ancestor walk, then the insert `Create` does. Refactor the insert into `createLocked` shared by both.
  - `type SpawnLimitError struct { Depth bool; Distance int; Count, Max uint32 }`, `Error()`:
    - `Depth`: `spawn limit: your spawn depth is 0, so this task may create no tasks`
    - otherwise: `spawn limit: <whose> descendant limit is reached (<Count>/<Max>); resume an existing task, or ask the operator to prune or raise the limit`, `<whose>` = `your own` (0), `your parent's` (1), `an ancestor's (<Distance> levels up)` (≥2)
  - `func (s *TaskStore) DescendantCounts() map[string]uint32` — one pass over the store
  - `func (s *TaskStore) SpawnRoom(id string) protocol.SpawnLimit` — min over `id` and its ancestors with a limited `max_descendants` of `max − desc`, floored at 0; unbounded if none

- [ ] **Step 1: Write the tests**:
  - `TestCreateWithinLimitsDepth`: creator depth limited 0 → `*SpawnLimitError{Depth: true}`, no row added; unbounded / 1 → ok.
  - `TestCreateWithinLimitsDescendants`: A `max = 2`; two children ok; third → `{Distance: 0, Count: 2, Max: 2}`; one child `Finish`ed → third still refused; that child `PruneByIDs`'d → ok; a child C of A creating → refused with `Distance: 1`; a great-grandchild path → `Distance: 3` text `an ancestor's (3 levels up)`.
  - `TestCreateWithinLimitsChildLimitDoesNotWiden` (D6): child given `max = 10` under A `max = 2` still stops at A's.
  - `TestCreateWithinLimitsOperatorRoot`: all-zero creator with a full subtree elsewhere → ok.
  - `TestCreateWithinLimitsReparentOverLimit` (D4): `SetParent` a task into a full A → ok; next creation under A → refused.
  - `TestCreateWithinLimitsRace` (Review Focus 1): A `max = 1`, 32 goroutines call `CreateWithinLimits` under A → exactly 1 succeeds; run with `-race`.
  - `TestLimitsSurviveReplay` (Review Focus 2): create with limits; `SetCaps` caps only; `Resume` with `GrantUpdate{SpawnDepth: true, …}`; reopen on the same WAL → last-written values.
  - `TestLegacyRecordReplaysUnbounded`: a `task_created` line without the keys → both unbounded.
  - `TestSpawnRoom`: A `max = 3` with one child, the child `max = 10` → the child's room is 2.
  - `TestOnlyCreateWithinLimitsInProduction`: parse every non-test `server/*.go`; any call `X.Create(` where `X` is `h.Tasks`, `s.tasks` or a `*TaskStore` receiver outside `taskstore.go`/`spawn_limit.go` fails, naming file:line.
- [ ] **Step 2: Run** `go test ./server/ -run 'TestCreateWithinLimits|TestLimitsSurviveReplay|TestLegacyRecordReplaysUnbounded|TestSpawnRoom|TestOnlyCreateWithinLimitsInProduction'` — Expected: FAIL to compile.
- [ ] **Step 3: Implement.** The guard test fails until Task 4 moves the two create sites; mark it `t.Skip("until Task 4")` here and remove the skip in Task 4.
- [ ] **Step 4: Run** `go test ./server/ -count=1` — Expected: PASS (`TestWALEventJSONRoundTripCopiesEveryField` included).
- [ ] **Step 5: Commit** `feat(server): store, persist and check spawn limits`.

---

### Task 4: handlers — creation, resume, `ls`, `whoami`

**Files:** Modify `server/task_handler.go`, `server/spawn_limit_guard_test.go` (drop the skip), `server/mapper_completeness_test.go`; Test `server/spawn_limit_handler_test.go` (new)

**Interfaces:**
- Consumes: Tasks 1–3.
- Produces:
  - `grantRequest` gains `SpawnDepth, MaxDescendants protocol.SpawnLimit` and their presence bits (from both request formats); `grantFor` writes `To.SpawnDepth = g.ChildSpawnDepth(r.SpawnDepth)`, `To.MaxDescendants = r.MaxDescendants`, with `u.SpawnDepth = fresh || r.SpawnDepthPresent` (likewise max).
  - Both fresh sites (`handleSubmit`, `handleOpenInteractive`) call `CreateWithinLimits`; a `*SpawnLimitError` → `SubmitStatus_SpawnLimit` / `OpenInteractiveStatus_SpawnLimit` with `error_msg = err.Error()`, returned before anything else is done for the task.
  - `exactFields` gains both presence bits; `exactMismatch` names `--spawn-depth` / `--max-descendants`.
  - `toTaskInfo(t TaskEntry, descendants uint32)`; `handleList` passes `counts[t.ID]` from one `DescendantCounts()`.
  - whoami literal: the record's two values, `Descendants` from `DescendantCounts()`, `SpawnRoom` from `Tasks.SpawnRoom(hex)`; operator: zero values.

- [ ] **Step 1: Write the tests** (handler level, as `server/delegation_spawn_test.go` does):
  - `TestSubmitRefusedBySpawnLimit`: agent A `max = 1` with one child; submit → `spawn_limit`, `error_msg` equals the Task 3 text with `your own` and `1/1`.
  - `TestOpenInteractiveRefusedLeavesNoTask` (Review Focus 4): same through `handleOpenInteractive` → `spawn_limit`, `ErrorMsg` set, `Tasks.List(0)` length unchanged, no open-exec sent to the fake runner.
  - `TestSpawnWritesChildLimits`: A depth 3; child requests depth 5, max 4 → child depth 2, max 4; omitted → depth 2, max unbounded.
  - `TestResumeNotCheckedAndRegrants` (D9): A full; an ended child resumed → ok, count unchanged; resume with `spawn_depth_present` writes `min(req, resumer − 1)`.
  - `TestExactResumeRefusesLimitBits`: either bit with `resume_exact` → `resume_exact_mismatch` naming the flag.
  - `TestWhoamiSpawnRoom`, `TestListCarriesDescendants`.
- [ ] **Step 2: Run** `go test ./server/ -run 'TestSubmitRefusedBySpawnLimit|TestOpenInteractiveRefusedLeavesNoTask|TestSpawnWritesChildLimits|TestResumeNotCheckedAndRegrants|TestExactResumeRefusesLimitBits|TestWhoamiSpawnRoom|TestListCarriesDescendants|MapsEveryField'` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./server/ -count=1` — Expected: PASS, guard test included.
- [ ] **Step 5: Commit** `feat(server): creation enforces spawn limits; ls and whoami carry them`.

---

### Task 5: `caps set`

**Files:** Modify `server/set_caps_handler.go`; Test `server/set_caps_spawn_limit_test.go` (new)

**Interfaces:**
- Consumes: `GrantUpdate` bits (Task 3), `SetCapsRequest` fields (Task 1).
- Produces: `handleSetCaps` writes each value behind its presence bit on the target only; the cascade's `GrantUpdate` never sets the limit bits; `narrowed` unchanged (caps/scope only).

- [ ] **Step 1: Write the tests**: `TestSetCapsLimitsAlone` (each bit alone changes only its value); `TestCascadeLeavesLimits` (Review Focus 3): T → C with C `depth 2, max 4`; `caps set T --caps file_read --cascade` and `caps set T --spawn-depth 0 --cascade` → C's limits unchanged, C's caps clamped as today; `TestLimitOnlyChangeClosesNoConns` (stub `DropConnsForPrincipal` → 0 calls).
- [ ] **Step 2: Run** `go test ./server/ -run 'TestSetCapsLimitsAlone|TestCascadeLeavesLimits|TestLimitOnlyChangeClosesNoConns'` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./server/ -count=1` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(server): caps set edits spawn limits`.

---

### Task 6: CLI flags and the shared funnel

**Files:** Modify `cli/verb/spawn.go`, `cli/verb/table.go`, `cli/verb/authority.go`, regenerated `cli/verb/actions_gen.go`, `cli/session_opts.go`, `cli/submit.go`, `cli/set_caps.go`, `cli/open_interactive_native.go`, `cli/open_interactive_wasm.go`, `cmd/harness-cli/spawn.go`, every `SpawnAction` → `SessionOpts` copy (`grep -rn 'DelegateScopePresent' cli tui cmd/harness-webui-wasm` lists them); Test `cli/verb/spawn_limit_flags_test.go`, `cli/session_opts_spawn_limit_test.go` (new)

**Interfaces:**
- Consumes: wire fields (Task 1).
- Produces:
  - `func parseSpawnLimitFlag(s string) (*protocol.SpawnLimit, error)` — `unbounded` or a decimal 0–65535; anything else an error naming the flag's grammar.
  - `spawnFlags`: `{Name: "spawn-depth", FieldType: "*protocol.SpawnLimit", Convert: "parseSpawnLimitFlag", PresenceField: "SpawnDepthPresent", Help: "generations that may still be created below the new task: N or unbounded (default: one less than yours)"}`, `{Name: "max-descendants", …, PresenceField: "MaxDescendantsPresent", Help: "tasks the new task's subtree may hold, ended ones included until pruned: N or unbounded (default unbounded)"}`; `exactConflicts` gains both. Same two on `caps set` (pointer, no presence; `AtLeastOne` widens) and `caps set-defaults`.
  - `SessionOpts` / `SetCapsOpts` gain the values and bits; both request builders set them.
  - Both `OpenInteractive` clients and `submit` print `ErrorMsg` for `spawn_limit` and exit non-zero.

- [ ] **Step 1: Write the tests**: `TestSpawnLimitFlagsOnEverySpawnVerb` (CLI, TUI, WebUI surfaces: `--spawn-depth 2 --max-descendants unbounded` → fields and presence); `TestSpawnLimitFlagGrammar` (`-1`, `65536`, `abc` refused; `0`, `unbounded` ok); `TestSpawnLimitFlagsConflictWithExact`; `TestCapsSetAcceptsLimitAlone`; `TestSessionOptsCarryLimits`.
- [ ] **Step 2: Run** `go test ./cli/... -run 'TestSpawnLimit|TestCapsSetAcceptsLimitAlone|TestSessionOptsCarryLimits'` — Expected: FAIL.
- [ ] **Step 3: Implement**; `go generate ./cli/verb/`.
- [ ] **Step 4: Run** `go test ./cli/... -count=1` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(cli): --spawn-depth / --max-descendants on spawn, caps set and the defaults`.

---

### Task 7: CLI display and the catalog

**Files:** Modify `cli/list.go`, `cli/whoami.go`, `cli/verb/caps.go`, `cli/whatif.go`; Test `cli/spawn_limit_display_test.go` (new)

**Interfaces:**
- Produces:
  - `ls` row tokens after `delegate-scope=`: `spawn-depth=<LimitLabel> max-descendants=<LimitLabel> descendants=<n>` on every row, unbounded included.
  - `ls --json` / `session ls`: `spawn_depth`, `max_descendants` (`null` = unbounded, else the number), `descendants`.
  - `whoami`: a line `spawn-depth=… max-descendants=… room=…`.
  - `WriteCaps`: a `SPAWN LIMITS` section after `DELEGATION` — the two rules; defaults; a place frees only on prune, so reuse a task by resuming it; D19's caution, verbatim: `Grant only what the work needs: an agent given spawn with limits is bounded only while it lacks prune over its subtree.` The `prune` line gains: `over a limited subtree it frees places, and pruning an ended task in the middle lifts the limits for the tasks below it`.
  - `caps what-if --spawn`: the predicted child's `spawn-depth`.

- [ ] **Step 1: Write the tests**: `TestLsRowShowsUnboundedLimits`; `TestLsJSONLimitsNullWhenUnbounded`; `TestWhoamiShowsRoom`; `TestCapsCatalogHasSpawnLimits` (section header, the caution sentence, the prune sentence); `TestWhatIfPredictsSpawnDepth`.
- [ ] **Step 2: Run** them — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./cli/... -count=1` — Expected: PASS (ledger listing goldens that gained the tokens).
- [ ] **Step 5: Commit** `feat(cli): show spawn limits in ls, whoami, caps and what-if`.

---

### Task 8: TUI

**Files:** Modify `tui/client.go`, `tui/app.go`, `tui/dispatch.go`, `tui/overlays.go`, `tui/authoritypicker.go`, `tui/detail.go`; Test `tui/spawn_limit_test.go` (new)

**Interfaces:**
- Produces: `tui.Authority` gains the two values and bits, copied by `Authority.opts`; `App.sessionSpawnDepth`, `App.sessionMaxDescendants` beside `sessionDelegateCaps`, read by fresh spawns only; the picker's spawn, re-grant and set-defaults modes gain a `limits` section with two rows, each a numeric field plus an `unbounded` toggle, re-grant prefilled from the task and sending a value only when changed; `formatTaskDetail` line `spawn limits: depth=… max-descendants=… descendants=…`.

- [ ] **Step 1: Write the tests**: `TestAuthorityOptsCarriesLimits`; `TestResumeNeverSendsSessionDefaultLimits` (Review Focus 5: `r`/`u` builds carry both bits false); `TestPickerLimitsSection` (prefill, typing digits, toggling unbounded); `TestDetailShowsLimits`.
- [ ] **Step 2: Run** `go test ./tui/ -run 'Limits'` — Expected: FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** `go test ./tui/ -count=1` — Expected: PASS. Then drive `harness-tui` against a dummy harness in a PTY (`session-debugging`): open the picker, set both limits, spawn, read the detail popup.
- [ ] **Step 5: Commit** `feat(tui): spawn limits in the picker, session defaults and the detail popup`.

---

### Task 9: WebUI and the wasm bridge

**Files:** Modify `cmd/harness-webui-wasm/main.go`, `webui/index.html`, `webui/static/main.js`; Test `webui/static/spawn_limit_test.mjs` (new, added to `js-test`)

**Interfaces:**
- Produces: snapshot task fields `spawnDepth` / `maxDescendants` (label) and `spawnDepthLimited`, `spawnDepthValue`, `maxDescendantsLimited`, `maxDescendantsValue`, `descendants` (raw); bridge opts `spawnDepth` / `maxDescendants` as `{limited, value}` plus presence on submit / open / set-caps; `main.js` `spawnSpawnDepth`, `spawnMaxDescendants` beside `spawnDelegateCaps`, fresh spawns only; a `limits` group (number input + `unbounded` checkbox, two rows) in compose and `#regrant-modal`, prefilled from raw fields, sending a value only when changed; row meta shows the `ls` tokens.

- [ ] **Step 1: Write `spawn_limit_test.mjs`**: through the bridge, `submit --spawn-depth 2 --max-descendants unbounded x` parses with both set; `caps set-defaults --max-descendants 3` parses. (Pins the bridge; the parse comes from Task 6, so this is not this task's RED — the page behaviour is verified in Step 4.)
- [ ] **Step 2: Run** `make js-test` — Expected: PASS.
- [ ] **Step 3: Implement**; `make webui-build`.
- [ ] **Step 4: Verify in Playwright** against `scripts/dummy-harness.sh up --detach --agent fake --name limits-webui`, at 1280px and 390px: the snapshot's task objects carry the raw fields (`browser_evaluate`); set limits in compose, spawn, the row shows them; re-grant is prefilled; a resume with session-default limits set leaves the task's limits unchanged in `ls` (Review Focus 5). Screenshots under the worktree's `.superpowers/`, reported by path.
- [ ] **Step 5: Commit** `feat(webui): spawn limits in compose, re-grant and the task row`.

---

### Task 10: docs, parity walk, live verification

**Files:** Modify `README.md`, `runner/agentskills/supervising-workers/SKILL.md` and its two byte-identical mirrors, `.claude/skills/surface-parity-checklist/firing-log.md`, `docs/superpowers/specs/2026-11-09-spawn-limits-design.md` (Amendment)

- [ ] **Step 1: README** "Capabilities and scope": the two limits, defaults, flags, `caps set`, D10's prune note, D19's caution.
- [ ] **Step 2: Skill**, per the spec's Agent-skill row and D18, verbatim order: check `whoami`'s `room`; ending or cancelling does not free a place; on `spawn_limit` resume an existing worker first, else ask the operator to prune or raise the limit; holding `prune` is not a licence — ask and prune only once approved; an unapproved prune is restorable but not the expected path; never retry the spawn as is. Read the file in full before editing; `cmp` the three copies after.
- [ ] **Step 3: Spec amendment**: `SpawnLimit` format and the 0–65535 depth range (this plan's first decision), `CreateWithinLimits` and the guard.
- [ ] **Step 4: Walk `surface-parity-checklist` items 1–39 and S1–S6** against the spec's Surfaces table, one verdict each, logged in `firing-log.md`.
- [ ] **Step 5: Full suite** `make vet && make test` (env scrub) — Expected: PASS.
- [ ] **Step 6: Live**, the spec's six steps against `scripts/dummy-harness.sh up --detach --agent fake --name limits` (eval `env`, `unset HARNESS_SERVER_PUBKEY`), agent-side commands through the `bash` profile, in the spelling `--help` prints; record each command and its output line. `down` afterwards.
- [ ] **Step 7: `scripts/wire-skew-check.sh <sha before Task 1>`** — Expected: PASS.
- [ ] **Step 8: Commit** docs; land per the landing policy (Mode A FF-push, `merge --ff-only` in the main checkout, `make build` there); tell the operator the server must be restarted first (wire change).
