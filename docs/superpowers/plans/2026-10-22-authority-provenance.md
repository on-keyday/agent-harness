# Authority provenance (visualizer Phase 2) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every task records who last wrote its authority (caps / scope / parent link), and `ls`, `session ls`, the TUI detail popup, the WebUI task sheet and `caps audit` show it.

**Architecture:** Three values on `TaskEntry` — a source kind, the acting task, a timestamp — written by each store method that changes authority, persisted on the WAL events those methods already write, and carried to clients on `TaskInfo`. `handleList` hides the actor from a caller that cannot see it. One Go label function renders the value for every surface.

**Tech Stack:** Go 1.26; `.bgn` schema regenerated with `make protoregen` (brgen kit already cached under `~/.cache/brgen-kit`); `make` targets; the wasm WebUI bridge.

**Spec:** `docs/superpowers/specs/2026-10-22-authority-visualizer-design.md` — § Phase 2 — provenance, decisions V6, V13, V17, V18; Wire and Surfaces tables.

## Global Constraints

- `AuthoritySource` values, in this order (spec § Phase 2 Values, V17): `unknown = 0`, `spawn`, `operator_spawn`, `operator_caps_set`, `cascade`, `resume`, `reparent`.
- A resume that keeps the recorded authority writes nothing (spec: "the authority did not change").
- A legacy WAL record replays as `unknown`; WAL keys are `omitempty`.
- `authority_actor` is zeroed for a caller whose visible set does not contain it (V18); the UI then says the actor is hidden. Operators see every actor.
- `.bgn` changes: server first, then every client (V/D18 style rollout); run `scripts/wire-skew-check.sh` before landing.
- Work in `/home/kforfk/workspace/remote-agent-harness/.harness-worktrees/70fbad4a6eb6f1e992be8a669f1bcefd`. Run `make test` and `go test ./cli/...` with `env -u HARNESS_SERVER_PUBKEY -u HARNESS_SERVER_CID -u HARNESS_AUTH_TICKET -u HARNESS_TASK_ID`.

## Decisions this plan adds (spec silent)

- **`authority_age_ms`** rides beside `authority_at`, computed on the server clock in `handleList`, exactly as `progress_age_ms` beside `progress_at` — clients render ages from it (`cli.AgeStr`), never from `now - authority_at`, so cross-host clock skew cannot distort them.
- **`authority_actor_hidden :u1`** takes one bit of `TaskInfo`'s `reserved :u5` (→ `u4`). A zero actor means "operator" for `operator_*` and an operator `resume`, so hiding needs its own bit or the two read the same.
- **"Visible" for the actor** is the caller's list set: `allowed ∪ {parentHex}` from `listVisibleToCaller` — the one-hop parent's id is already disclosed by its redacted row and `whoami`'s `creator_task_id`.
- **One label for every surface** (`cli.AuthorityLabel`), the same string in the CLI row, TUI popup, WebUI sheet and caps audit (checklist item 32: one serializer per grammar). Form: `<source>@<actor> (<age>)` — `spawn@3f2a9c1d (2h ago)`, `operator_caps_set@operator (5m ago)`, `cascade@hidden (1m ago)`, `unknown`.
- **`whoami` is not a surface** of this phase (the spec's Surfaces table does not list it).

## Review Focus

1. **A cascade writes provenance on every clamped descendant, and only those** — the target gets `operator_caps_set`, each descendant `cascade` with the target as actor; a descendant the cascade left unchanged still gets `cascade` (it was written). Pinned in Task 3.
2. **Swap marks both tasks** `reparent` (spec V17: "both tasks of a swap"). Pinned in Task 2.
3. **Restart survives** — provenance written, server restarted (WAL replay), `ls` shows the same source/actor/time. Pinned in Task 2's replay test.
4. **A confined agent never learns an unseen actor's id** — including through `ls --json` and the WebUI snapshot, which both read `TaskInfo`. Pinned in Task 3 at the wire, so every client inherits it.
5. **The `ls` row of a pre-change task** reads `auth=unknown`, not an empty or zero-time field (checklist item 31: zero is shown). Pinned in Task 4.

---

## File Structure

| File | Change |
| --- | --- |
| `runner/protocol/message.bgn` + regenerated `message.go` | `AuthoritySource` enum; four `TaskInfo` fields + one bit |
| `server/taskstore.go` | `TaskEntry` fields; `AuthorityWrite`; provenance written by `Create`, `SetCaps`, `Resume`, `SetParent`, `SwapWithParent`; replay |
| `server/wal.go` | `WALEvent` / `walEventJSON` keys |
| `server/set_caps_handler.go`, `server/task_handler.go`, `server/set_parent_handler.go` | pass the writer; `toTaskInfo` maps; `handleList` ages and hides |
| `cli/authority_label.go` (new) | `AuthorityLabel` |
| `cli/list.go` | `auth=` row token; JSON fields (shared by `session ls`) |
| `tui/detail.go` | `authority:` line |
| `cli/caps_audit.go`, `cli/caps_audit_render.go`, `tui/capsauditmodal.go` | holder carries and prints the label |
| `cmd/harness-webui-wasm/main.go`, `webui/static/main.js` | snapshot field `authority`; task sheet line; audit holder line |
| `README.md` | "Capabilities and scope": the `auth=` token |

---

### Task 1: wire schema

**Files:** Modify `runner/protocol/message.bgn`; regenerate `runner/protocol/message.go`; Test `runner/protocol/authority_wire_test.go` (new)

**Interfaces:**
- Produces: `protocol.AuthoritySource` with constants `AuthoritySource_Unknown`, `_Spawn`, `_OperatorSpawn`, `_OperatorCapsSet`, `_Cascade`, `_Resume`, `_Reparent` (`String()` = the snake names); `TaskInfo` fields `AuthoritySource`, `AuthorityActor protocol.TaskID`, `AuthorityAt uint64`, `AuthorityAgeMs uint64`, getter/setter `AuthorityActorHidden()` / `SetAuthorityActorHidden(bool)`.

- [ ] **Step 1: Write `TestTaskInfoAuthorityRoundTrip`**: build a `TaskInfo` with `AuthoritySource_Cascade`, a non-zero actor, `AuthorityAt = 1`, `AuthorityAgeMs = 2`, hidden = true; `MustAppend` then decode; every field equal. Also `AuthoritySource_Reparent.String() == "reparent"`.
- [ ] **Step 2: Run** `go test ./runner/protocol/ -run TestTaskInfoAuthorityRoundTrip` — Expected: FAIL to compile.
- [ ] **Step 3: Edit `message.bgn`**: `enum AuthoritySource : u8` with the Global Constraints values and a comment per value (spec § Values table, verbatim meaning); in `TaskInfo` replace `reserved :u5` with `authority_actor_hidden :u1` + `reserved :u4`, and append after `title` (the format's last field): `authority_source :AuthoritySource`, `authority_actor :TaskID`, `authority_at :u64` (unix nanos, server clock, of the last write; 0 = unknown), `authority_age_ms :u64` (server-computed, like `progress_age_ms`). Run `make protoregen`.
- [ ] **Step 4: Run** `go test ./runner/protocol/ -count=1 && go vet ./...` — Expected: PASS (the regen churn is expected; check build/vet/test and the new symbols, per the bgn memory).
- [ ] **Step 5: Commit** `feat(protocol): authority provenance on TaskInfo`.

---

### Task 2: store, WAL and replay

**Files:** Modify `server/taskstore.go`, `server/wal.go`; Test `server/authority_provenance_test.go` (new)

**Interfaces:**
- Consumes: Task 1's enum.
- Produces:
  - `TaskEntry` fields `AuthoritySource protocol.AuthoritySource`, `AuthorityActor protocol.TaskID`, `AuthorityAt time.Time`.
  - `type AuthorityWrite struct{ Source protocol.AuthoritySource; Actor protocol.TaskID }`.
  - `SetCaps(id string, setCaps bool, caps protocol.Capability, setScope bool, scope authority.Scope, by AuthorityWrite) (TaskEntry, bool)` — new last parameter.
  - `Resume(..., agentProfile string, resumer protocol.TaskID)` — new last parameter; writes `{resume, resumer}` only when it writes a `task_caps_changed`.
  - `Create` derives `spawn` (actor = creator) or `operator_spawn` (zero creator) itself; `SetParent` and `SwapWithParent` write `{reparent, zero}` on every task whose parent changed.
  - WAL keys `authority_source` (uint8), `authority_actor` (hex, "" for zero), `authority_at` (unix nanos), all `omitempty`, on `task_created`, `task_caps_changed`, `task_parent_changed`; replay of each event assigns all three.

- [ ] **Step 1: Write the tests** in `server/authority_provenance_test.go`, on a `NewTaskStore()` with a WAL in `t.TempDir()`:
  - `TestProvenanceCreate`: operator-created → `operator_spawn`, zero actor, non-zero `AuthorityAt`; created by task P → `spawn`, actor P.
  - `TestProvenanceSetCaps`: `SetCaps(..., AuthorityWrite{operator_caps_set, zero})` → those values, `AuthorityAt` advanced.
  - `TestProvenanceResume`: resume with caps override by resumer R → `resume`, actor R; resume with neither override → source/actor/time unchanged.
  - `TestProvenanceReparent`: `SetParent` → `reparent`; `SwapWithParent` → BOTH tasks `reparent` (Review Focus 2).
  - `TestProvenanceSurvivesReplay` (Review Focus 3): perform one write of each kind, reopen a store on the same WAL, every task's three fields equal; a hand-written legacy `task_created` line with no authority keys replays as `unknown`, zero actor, zero time.
- [ ] **Step 2: Run** `go test ./server/ -run TestProvenance` — Expected: FAIL to compile.
- [ ] **Step 3: Implement** the Interfaces above. `writeCapsChangedLocked` and `writeParentChangedLocked` take the entry's provenance and write it; `AuthorityAt = time.Now()` at the write. Update every caller of the changed signatures (the compiler lists them; existing tests pass a zero `AuthorityWrite{}` / zero resumer).
- [ ] **Step 4: Run** `go test ./server/ -count=1` — Expected: PASS, including `TestWALEventJSONRoundTripCopiesEveryField`.
- [ ] **Step 5: Commit** `feat(server): record who last wrote a task's authority`.

---

### Task 3: handlers, `TaskInfo` mapping, actor hiding

**Files:** Modify `server/set_caps_handler.go`, `server/task_handler.go`, `server/set_parent_handler.go`; Test `server/authority_provenance_test.go`

**Interfaces:**
- Consumes: Task 2's `AuthorityWrite`, signatures.
- Produces: `toTaskInfo` sets `AuthoritySource`, `AuthorityActor`, `AuthorityAt` (unix nanos, 0 when the time is zero); `handleList` sets `AuthorityAgeMs` (0 when `AuthorityAt` is 0) and, for a non-`all` caller, zeroes the actor and sets `AuthorityActorHidden` when the actor is non-zero and not in `allowed ∪ {parentHex}`.

- [ ] **Step 1: Write the tests** (through `h.Handle`, as `set_caps_test.go` does):
  - `TestSetCapsWritesProvenance` (Review Focus 1): operator `caps set <c> --cascade` → c `operator_caps_set`/zero; g `cascade`/actor c.
  - `TestResumeWritesResumer`: an agent resumer with `resume_caps_override` → `resume`/actor = resumer's task id; an operator resumer → `resume`/zero.
  - `TestListHidesUnseenActor` (Review Focus 4): on `scopeFixture` (p → c → g, each scope `subtree`), set g's scope to `none`, then the operator cascades from p, so g's actor is p. In g's own `List`, g's row has a zero actor and hidden = 1: p is g's grandparent, neither in g's allowed set nor its one-hop parent. In c's `List`, g's row shows actor p with hidden = 0, because p is c's one-hop parent. The operator's `List` shows actor p, hidden = 0.
  - `TestListAuthorityAge`: right after a `SetCaps`, the row has `AuthorityAt > 0` and `AuthorityAgeMs < 60000`; a legacy-unknown row has both 0.
- [ ] **Step 2: Run** `go test ./server/ -run 'TestSetCapsWritesProvenance|TestResumeWritesResumer|TestListHidesUnseenActor|TestListAuthorityAge'` — Expected: FAIL.
- [ ] **Step 3: Implement.** `handleSetCaps` passes `{operator_caps_set, zero}` for the target and `{cascade, target id}` per descendant; `handleSubmitResume` / `handleOpenInteractive` pass `h.lookupPrincipal(cid)` as the resumer; `server/mapper_completeness_test.go` will name any `TaskInfo` field left unmapped.
- [ ] **Step 4: Run** `go test ./server/ -count=1` — Expected: PASS.
- [ ] **Step 5: Commit** `feat(server): carry authority provenance to clients, hiding unseen actors`.

---

### Task 4: the label, `ls` and `session ls`

**Files:** Create `cli/authority_label.go`, `cli/authority_label_test.go`; Modify `cli/list.go`, its tests

**Interfaces:**
- Produces: `func AuthorityLabel(t protocol.TaskInfo) string`; `ls` row token `auth=<AuthorityLabel>`; `taskJSON` fields `authority_source` (string), `authority_actor` (32-hex, "" for zero), `authority_actor_hidden` (bool), `authority_at` (uint64), `authority_age_ms` (uint64).

- [ ] **Step 1: Write `TestAuthorityLabel`** — table, exact strings:
  - unknown, all zero → `unknown`
  - `spawn`, actor `3f2a9c1d…`, age 2h → `spawn@3f2a9c1d (2h ago)`
  - `operator_caps_set`, zero actor, 5m → `operator_caps_set@operator (5m ago)`
  - `cascade`, hidden → `cascade@hidden (1m ago)`
  - `resume`, zero actor → `resume@operator (…)`; `reparent` → `reparent@operator (…)`
- [ ] **Step 2: Write `TestListRowCarriesAuthority`** (Review Focus 5): the row of a legacy task contains `auth=unknown`; of a spawned one `auth=spawn@<id8> (`; the JSON of each carries all five fields, zeros included.
- [ ] **Step 3: Run** `go test ./cli/ -run 'TestAuthorityLabel|TestListRowCarriesAuthority'` — Expected: FAIL.
- [ ] **Step 4: Implement**: the age is `AgeStr(t.AuthorityAgeMs)`; the id is the first 8 hex. The row token sits after `scope=`. `session ls` gains the fields through the shared `taskJSON`.
- [ ] **Step 5: Run** `env -u HARNESS_SERVER_PUBKEY -u HARNESS_SERVER_CID -u HARNESS_AUTH_TICKET -u HARNESS_TASK_ID go test ./cli/... -count=1` — Expected: PASS.
- [ ] **Step 6: Commit** `feat(cli): auth= in ls and session ls`.

---

### Task 5: TUI, WebUI, caps audit

**Files:** Modify `tui/detail.go`, `tui/capsauditmodal.go`, `cli/caps_audit.go`, `cli/caps_audit_render.go`, `cmd/harness-webui-wasm/main.go`, `webui/static/main.js`; tests beside each

**Interfaces:**
- Consumes: `cli.AuthorityLabel`.
- Produces: `AuditHolder.Authority string` (= `AuthorityLabel` of the holder) and `AuditHolderJSON.Authority` (`json:"authority"`); wasm snapshot task field `authority` (the label string); the WebUI renders it as given (no JS formatting).

- [ ] **Step 1: Write the tests**:
  - `tui`: `TestFormatTaskDetailShowsAuthority` — `formatTaskDetail` output contains `authority: spawn@3f2a9c1d (2h ago)` for such a task and `authority: unknown` for a legacy one.
  - `cli`: `TestCapsAuditHolderCarriesAuthority` — `CapsAudit` holder and its JSON carry the label; the text renderer prints `auth=<label>` on the holder line.
  - `tui`: the caps-audit modal line for a holder contains `auth=`.
- [ ] **Step 2: Run** `go test ./tui/ ./cli/ -run 'Authority'` — Expected: FAIL.
- [ ] **Step 3: Implement** the Go side; in the wasm `ls` snapshot map add `"authority": cli.AuthorityLabel(t)`; in `main.js` add a line 「権限の記録: <authority>」 to the task sheet and append ` · <authority>` to the caps-audit holder line (`h.authority`).
- [ ] **Step 4: Run** `go test ./tui/ ./cli/... -count=1 && make wasm-check && make js-test` (env scrubbed) — Expected: PASS.
- [ ] **Step 5: Commit** `feat(tui,webui): show authority provenance in detail, sheet and audit`.

---

### Task 6: docs, verification, land

- [ ] **Step 1: README** "Capabilities and scope": one paragraph after the `caps set` block — `ls` shows `auth=<source>@<actor> (<age>)`, the seven sources, `@hidden` for an actor the caller cannot see, `unknown` for tasks written before this change.
- [ ] **Step 2: Walk `surface-parity-checklist` items 1–39** for the new field, one verdict per number, and record the walk in its `firing-log.md`. Expected `done`: 11, 12, 14, 17, 21, 23, 31, 35. Expected `omitted` with reasons: 13 (`whoami` is not a Phase 2 surface), 16 (the TUI task table is width-bound; the popup carries it), 20 (the WebUI task row meta is crowded; the sheet carries it). Any other verdict is decided and logged at the walk.
- [ ] **Step 3: Run** `make vet && make test && make wasm-check && scripts/wire-skew-check.sh` (env scrubbed) — Expected: all exit 0.
- [ ] **Step 4: Live check** on `scripts/dummy-harness.sh up --detach --agent fake` (unset `HARNESS_SERVER_PUBKEY` after `eval`): an operator spawn reads `auth=operator_spawn@operator`; that agent's child `auth=spawn@<parent8>`; `caps set <child> --caps none` → `auth=operator_caps_set@operator`; restart the dummy server (`down` / `up` keeps nothing — instead stop the server with its shutdown file and relaunch with the same `--data-dir`, per the dummy-harness skill's restart notes) and see the same values. WebUI task sheet shows the line (screenshot kept).
- [ ] **Step 5: Land** per `landing-to-main` Mode A: code commits first, FF-push by SHA, FF the main checkout by SHA, `make build`. Rollout: the operator restarts the server before clients (wire change).
