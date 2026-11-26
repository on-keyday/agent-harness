# Bound Enrollment Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `runner_enroll` becomes a scoped bit, and an enrolled key can be bound to a set of tasks, so that a runner authenticated with that key takes only those tasks.

**Architecture:** The binding is stored on the key record (`RunnerKeyRecord.Bound`, the key store's JSON file). It is read once, when a runner registers, into `RunnerEntry.Bound`. `Registry.Candidates` takes the task id and skips a bound runner whose binding does not name it. `readoptHeldTasks` applies the same rule to held tasks. The four key verbs (enroll, unenroll, kick, keys) resolve the caller's `runner_enroll` scope through `scopeSet`. An unbound key needs global scope. A key bound to S needs every task in S to be in scope.

**Tech Stack:** Go, `.bgn` → `make protoregen`, wasm (`cmd/harness-webui-wasm`), WebUI JS.

**Spec:** `docs/superpowers/specs/2026-11-26-grant-conditions-and-bound-enrollment-design.md`: C1–C3, C18, § Bound enrollment, and the C1/C2 lines of § Testing. This is the second of three plans. Grant conditions is already on this branch; actions comes next.

## Global Constraints

- At most 16 bound tasks per key (`bound_len :u8`, enforced in Go: refused over 16).
- An all-zero task id is never a binding member. Zero passed to `Candidates` ("no task yet") matches no binding.
- The binding is fixed at enroll (C2). Changing it means unenroll + enroll. An unenroll of an in-use key kicks its runners, so `RunnerEntry.Bound` cannot go stale while a runner holds it.
- An existing key-store file (no `bound_tasks`) loads as unbound (§ Bound enrollment).
- Out of reach on unenroll, kick and keys answers exactly as an unknown key or runner does: `not_found`, or absent from the listing. It never answers `permission_denied` (§ Bound enrollment, no existence oracle).
- C18: no server rule reserves T for its bound key. Keeping T off other runners is the controller's selector.
- Runner-decoded formats are unchanged. `RunnerHello` and registration are not touched, so no transition window is needed (§ Wire and disk).
- Wire changes are appended only. Run `scripts/wire-skew-check.sh` after committing the wire. Use `make test` (a bare `go test` inherits the task's live `HARNESS_*`). Stage by path. Do not land.

## Review Focus

- **A prefix that reveals a key or runner outside reach.** `RunnerKeyStore.Resolve` and the kick matcher run over every key or runner. A scoped caller whose prefix matches one reachable and one unreachable key would get `ambiguous_prefix`, which tells it the other key exists. Resolution must run over reachable keys and runners only. Pinned in Task 3: `TestUnenrollPrefixResolvesAmongReachableOnly` and `TestKickPrefixResolvesAmongReachableOnly`.
- **Re-enrolling an existing key with a different binding.** Today `Enroll` keeps the record and answers `already_enrolled`. Kept as is, the caller would believe its binding applied. Refused as `binding_differs`, with the record (label included) unchanged. Pinned in Task 2 (`TestEnrollKeepsRecordWhenBindingDiffers`) and Task 3 (`TestEnrollBindingDiffers`).
- **The restore line printed by unenroll.** `runner unenroll` prints the `runner enroll` that restores the key. Without `--bind`, running it restores the key unbound, which is wider than what was removed. Pinned in Task 5 (`TestFormatRunnerUnenrollRestoresBinding`).
- **A new foreground open, or a schedule create, when the only matching runner is bound.** Zero matches no binding, so the open answers `no_runner_for_repo`. It is not placed on the bound runner, and it does not come back as an ambiguous pick that includes it. Pinned in Task 4 (`TestForegroundOpenSkipsBoundRunner`).
- **A held task reported by a bound runner whose binding does not name it.** The readopt is refused with its own log line, and the task stays held until the hold expires. Pinned in Task 4 (`TestReadoptRefusesTaskOutsideBinding`).

---

### Task 1: Wire

**Files:**
- Modify: `runner/protocol/message.bgn`; regenerate `runner/protocol/message.go`
- Modify: the spec's § Wire and disk (list the formats below beside `RunnerEnrollRequest.bound`, "added while planning")
- Test: `runner/protocol/bound_enrollment_wire_test.go`

**Interfaces — Produces** (all appended):

- `RunnerEnrollRequest`: `bound_len :u8, bound :[bound_len]TaskID`. Empty means unbound.
- `RunnerKeyRow`, `RunnerUnenrollBody`, `RunnerInfo`: `bound_len :u8, bound :[bound_len]TaskID`.
- `RunnerKeyStatus` gains three values:
  - `out_of_reach = "out_of_reach"`: enroll; a bound task is outside the caller's `runner_enroll` scope, or the key is unbound and that scope is not global.
  - `binding_differs = "binding_differs"`: enroll; the key is enrolled with another binding. Unenroll + enroll to change it.
  - `bad_binding = "bad_binding"`: enroll; more than 16 tasks, or a zero task id.

- [ ] **Step 1:** Write the tests:
  - `TestRunnerEnrollRequestBoundRoundTrip`: 0 and 16 ids.
  - `TestRunnerKeyRowBoundRoundTrip`.
  - `TestRunnerUnenrollBodyBoundRoundTrip`.
  - `TestRunnerInfoBoundRoundTrip`.
- [ ] **Step 2:** Run `go test ./runner/protocol/ -run Bound`. Expected: FAIL (unknown fields).
- [ ] **Step 3:** Edit the schema with comments naming C1/C2, run `make protoregen`, and edit the spec's list.
- [ ] **Step 4:** Run the tests and `go vet ./...`. Expected: PASS.
- [ ] **Step 5:** Commit `runner/protocol/message.bgn`, `runner/protocol/message.go`, the test file and the spec: "protocol: bound enrollment on the wire (C1, C2)".

---

### Task 2: Key store binding

**Files:**
- Modify: `server/runnerkeys.go`
- Test: `server/runnerkeys_test.go`

**Interfaces — Produces:**

```go
// RunnerKeyRecord gains:
Bound []protocol.TaskID // sorted by hex, unique; nil = unbound

var ErrBindingDiffers = errors.New("the key is enrolled with another binding")

func normalizeBinding(ids []protocol.TaskID) ([]protocol.TaskID, error) // sort+dedupe; error on zero id or > 16
func (st *RunnerKeyStore) ResolveIn(prefixHex string, keep func(RunnerKeyRecord) bool) (RunnerKeyRecord, error)
// Resolve(p) == ResolveIn(p, nil)
```

`runnerKeyJSON` gains `BoundTasks []string \`json:"bound_tasks,omitempty"\``, holding 32-hex ids. On load, a malformed id is an error, like a malformed key. `Enroll` compares the existing record's binding with the request's binding as sets. If they differ, it returns `(false, ErrBindingDiffers)` and changes nothing; the label is replaced only when the bindings are equal.

- [ ] **Step 1:** Write the failing tests:
  - `TestRunnerKeysPersistBinding`: save, load, equal.
  - `TestRunnerKeysFileWithoutBindingLoadsUnbound`: a fixture JSON with no `bound_tasks`.
  - `TestRunnerKeysRejectsMalformedBoundID`.
  - `TestEnrollKeepsRecordWhenBindingDiffers`: the label is not replaced.
  - `TestResolveInSkipsUnkept`: two keys share a prefix and `keep` drops one; the result is the other, not `ErrAmbiguousPrefix`.
  - `TestNormalizeBinding`: dedupe and sort; zero id refused; 17 refused.
- [ ] **Step 2:** Run `go test ./server/ -run 'RunnerKeys|ResolveIn|Binding'`. Expected: FAIL.
- [ ] **Step 3:** Implement as above.
- [ ] **Step 4:** Run. Expected: PASS.
- [ ] **Step 5:** Commit: "server: a runner key records the tasks it is bound to (C2)".

---

### Task 3: Scoped `runner_enroll` (C1)

**Files:**
- Modify:
  - `server/runner_keys_handler.go`
  - `server/runner_kick_handler.go`
  - `server/scope_completeness_test.go` (the four kinds become `targetGated`; rewrite the comment at :84-85)
  - `server/scope_percap_completeness_test.go` (`RunnerEnroll` becomes `capResolvedLiteral`)
  - `cli/verb/caps.go` (`capAuditTable`: drop `scope: ScopeNone`; also the `CapDescription` text at :151, which must say the scope bounds the tasks a key may be bound to and that global is needed for an unbound key)
- Test: `server/runner_keys_scope_test.go` (new), `server/runner_keys_handler_test.go`

**Interfaces — Consumes:** Task 1 statuses and fields; Task 2 `Bound`, `normalizeBinding`, `ResolveIn`, `ErrBindingDiffers`.

**Produces:**

```go
// enrollReach is the caller's runner_enroll target set (scopeSet).
func (h *TaskHandler) enrollReach(cid objproto.ConnectionID) (all bool, allowed map[string]bool)
// reachesBinding: all, or bound non-empty and every id in allowed. Unbound needs all.
func reachesBinding(all bool, allowed map[string]bool, bound []protocol.TaskID) bool
```

**Behaviour of each verb:**
- **enroll**: check, in this order:
  1. `normalizeBinding` fails → `bad_binding`;
  2. `!reachesBinding` → `out_of_reach`;
  3. `ErrBindingDiffers` → `binding_differs`.

  `slog` logs the binding.
- **unenroll**: `ResolveIn(prefix, reach)`.
- **kick**: the prefix match runs over runners whose `RunnerEntry.Bound` is reachable. Task 4 adds that field; until then, read the key record through `h.RunnerKeys.Lookup(e.Key)`. A runner whose key is not in the store counts as unbound.
- **keys**: rows filtered by `reachesBinding`, each carrying `bound`.
- **unenroll body**: carries `bound`.

**Ruling:** a scoped caller can learn that a public key is enrolled, through `binding_differs` on a key it cannot reach. This is accepted: a fresh key answers `ok`, so the difference between enrolled and not enrolled cannot be hidden on enroll anyway, and the key is a public key.

- [ ] **Step 1:** Write the failing tests in `runner_keys_scope_test.go`. Each caller is a task principal whose `runner_enroll` grant has the named scope; T and U are tasks.
  - `TestEnrollScopedToT`: scope `ids:[T]`.
    - Bound `[T]` → `ok`.
    - Unbound → `out_of_reach`.
    - Bound `[U]` → `out_of_reach`.
    - Bound `[T,U]` → `out_of_reach`.
  - `TestEnrollGlobalMayEnrollUnbound`.
  - `TestEnrollBadBinding`: 17 ids; a zero id.
  - `TestEnrollBindingDiffers`.
  - `TestUnenrollOutOfReachIsNotFound`.
  - `TestUnenrollPrefixResolvesAmongReachableOnly`.
  - `TestKickOutOfReachIsNotFound` and `TestKickPrefixResolvesAmongReachableOnly`: registry entries carrying keys from the store.
  - `TestKeysListsOnlyReachable`: an unbound key is hidden from an `ids:[T]` caller; a key bound to `[T]` is shown, with `bound`.
  - `TestUnenrollBodyCarriesBinding`.
  - The existing `TestRunnerVerbsNeedRunnerEnroll` still passes: the bit gate is unchanged.
- [ ] **Step 2:** Run `go test ./server/ -run 'Enroll|Unenroll|Kick|Keys|Classified|Resolved|Agreement'`. Expected: FAIL.
- [ ] **Step 3:** Implement the handlers and flip the three classification tables together. `TestCapsAuditAgreesWithServer` (`server/caps_audit_agreement_test.go`) holds the audit table and the server table to each other.
- [ ] **Step 4:** Run the same tests, then `make test`. Expected: PASS.
- [ ] **Step 5:** Commit: "server: runner_enroll is scoped by the tasks a key is bound to (C1)".

---

### Task 4: Placement and readopt (C2)

**Files:**
- Modify:
  - `server/registry.go`: `RunnerEntry.Bound map[string]struct{}`, nil = unbound; `Candidates`.
  - `server/runner_handler.go`: `BindingOf func(ed25519.PublicKey) []protocol.TaskID`, the sibling of `AuthOf`, set right after `entry.Key` at :160.
  - `server/server.go`: wire `BindingOf` to `RunnerKeys.Lookup`, beside `AuthOf` at :761.
  - The five `Candidates` callers:

    | Call site | Task id passed |
    | --- | --- |
    | `background_open.go:166` | the resume hex, else `""` |
    | `background_open.go:212` | `t.ID` |
    | `scheduler.go:81` | `task.ID` |
    | `task_handler.go:1959` | `existingTaskIDHex` |
    | `schedule_handler.go:694` | `""` |

  - `server/readopt.go`: refuse a held task outside the reporting runner's `Bound`, after the `AssignedTo` case, with log `readopt: refused, outside the runner key's binding`.
  - `server/runner_kick_handler.go`: read `e.Bound` instead of the Task 3 lookup.
  - Every test call of `Candidates`: 14 found by `grep -n 'Candidates(' server/*_test.go`.
- Test: `server/registry_test.go`, `server/scheduler_test.go`, `server/readopt_test.go`, `server/open_bound_test.go` (new)

**Interfaces — Produces:**

```go
func (r *Registry) Candidates(repo string, sel protocol.RunnerSelector, taskID string) []RunnerEntry // "" = no task yet; matches no binding
func (e RunnerEntry) takes(taskID string) bool // e.Bound == nil || (taskID != "" && e.Bound has taskID)
```

The task id is a hex string, as `BindTask` / `ActiveTasks` key it: the registry's own convention.

- [ ] **Step 1:** Write the failing tests:
  - `TestCandidatesSkipsBoundRunnerForOtherTask`.
  - `TestCandidatesBoundRunnerTakesItsTask`.
  - `TestCandidatesNoTaskMatchesNoBinding`.
  - `TestTickPlacesOtherTaskOnUnboundRunner`: an unbound and a bound runner on the same repo; queued U goes to the unbound runner, queued T may go to the bound one.
  - `TestForegroundOpenSkipsBoundRunner`: the only runner is bound → `no_runner_for_repo`.
  - `TestRegistrationReadsBinding`: `BindingOf` fills `RunnerEntry.Bound`.
  - `TestReadoptRefusesTaskOutsideBinding`: the runner keeps T and is refused U.
- [ ] **Step 2:** Run `go test ./server/ -run 'Candidates|Tick|ForegroundOpen|Registration|Readopt'`. Expected: FAIL (compile error on the signature, then assertions).
- [ ] **Step 3:** Implement.
- [ ] **Step 4:** Run, then `make test`. Expected: PASS.
- [ ] **Step 5:** Commit: "server: a bound runner takes only its tasks (C2)".

---

### Task 5: Surfaces

Walk `surface-parity-checklist` items 1–39 and record a verdict for each in the ledger; S1–S6 do not fire.

**Files:**
- **CLI verb and parsing:**
  - `cli/verb/table.go`, the `runner enroll` row:
    - add flag `bind`, Type `FlagString`, Field `Bind`, FieldType `[]protocol.TaskID`, Convert `parseBindFlag`;
    - help: "the tasks a runner with this key may take, as ID[,ID…] (full 32-hex ids, at most 16); omitted = unbound, which needs runner_enroll global";
    - a Notes line on the scope rule;
    - an Example.
  - `cli/verb/scope.go`: export `ParseTaskIDList(list string) ([]protocol.TaskID, error)`, factored out of `parseScopeIDs` (same validation; its errors say "task id"). `parseBindFlag` calls it and refuses more than 16.
- **CLI client and formatting**, `cli/runner_keys.go`:
  - `RunnerEnrollWith(ctx, c, key, label string, bound []protocol.TaskID)`, and `RunnerEnroll` likewise;
  - `RunnerKeyRow.Bound []string`, `RunnerUnenrollResult.Bound`;
  - `FormatRunnerEnroll` gives each new status a sentence;
  - `FormatRunnerKeys` prints `binding=unbound` or `binding=<id8>,…`, always (item 31);
  - `FormatRunnerKeysJSON` emits `"bound_tasks":[…]`, `[]` when unbound;
  - `FormatRunnerUnenroll`'s restore line carries `--bind <ids>`.
- **Runner rows**, `cli/list.go`: the JSON row gets `bound_tasks` and the human row gets `binding=…`. `server/task_handler.go` `toRunnerInfo` sets `bound` from `RunnerEntry.Bound`.
- **Dispatch on each surface:**
  - `cmd/harness-cli/dispatch.go` `RunnerEnroll`;
  - `tui/dispatch.go` and the TUI's `DoRunnerEnroll` gain `bound`;
  - `tui/detail.go` `formatRunnerDetail` gets a `binding:` line with full ids;
  - `cmd/harness-webui-wasm/main.go`: `harnessRunnerEnroll` takes a third argument, the raw `--bind` text, parsed with `verb.ParseTaskIDList` (the parse stays in Go); the runner map gets `binding`;
  - `webui/static/cmdline.js` passes `b.flags.bind || ""`;
  - `webui/static/task_models.js` puts `binding` on the runner model and the row shows it.
- **Docs and golden:**
  - The MCP golden (`cmd/harness-cli`, as `e4283716` regenerated it).
  - `README.md` § runner enrollment (:1005-1020): `--bind`, and the scope rule.
- **Tests:** `cli/runner_keys_test.go`, `cli/verb/scope_test.go`, `tui` and `webui/static` tests beside the existing runner-key ones.

**Ruling: no binding column in the TUI runners table** (item 18a): the table is over-subscribed. The binding is on the `ls` row, in the TUI runner detail and in the WebUI row.

- [ ] **Step 1:** Write the failing tests:
  - `TestParseBindFlag`: 16 ok; 17, short id, non-hex refused; dedupe.
  - `TestFormatRunnerKeysShowsBinding`: unbound prints `binding=unbound`.
  - `TestFormatRunnerKeysJSONBoundTasks`.
  - `TestFormatRunnerUnenrollRestoresBinding`.
  - `TestFormatRunnerEnrollNewStatuses`.
  - `TestRunnerRowShowsBinding`: human row and JSON.
  - TUI: `formatRunnerDetail` binding line.
  - JS: the runner row model carries `binding`.
- [ ] **Step 2:** Run `make test` and `make js-test`. Expected: FAIL.
- [ ] **Step 3:** Implement. Then run `make wasm-check`.
- [ ] **Step 4:** Run `make test`, `make js-test` and `make wasm-check`. Expected: PASS.
- [ ] **Step 5:** Commit: "cli, tui, webui: runner enroll --bind, and the binding wherever a key or runner is shown".

---

### Task 6: Live check (dummy harness)

No code. Its result goes in the ledger. This covers the C1/C2 part of the spec's dummy E2E; the action part belongs to the actions plan.

- [ ] **Step 1:** `scripts/dummy-harness.sh up --detach --agent fake --name bound`, then `eval` its `env`.
- [ ] **Step 2:** Make a second git repo R2 that the dummy's own runner does not serve. As operator, open two background tasks on R2, T and U (`session new -d --background` with its deadline flags); both stay queued. Then make a credential holding `runner_enroll` with `--scope ids:<T>`:
  - `credential keygen --out <seed file>`;
  - `credential new --pubkey … --caps runner_enroll --scope ids:<T>`;
  - `credential bind`.

  The secret is used only through that file. A hostname pin cannot separate the two runners here, because both report this host's name (C18's accepted limit).
- [ ] **Step 3:** Under that credential:
  - `runner enroll <new key> --bind <T>` → ok;
  - `runner enroll <another key>` without `--bind` → `out_of_reach`;
  - `runner keys` shows only the bound key.
- [ ] **Step 4:** Start a second `agent-runner` with `--runner-key-file` set to the new key and `--roots R2`. Expect:
  - T is placed on it;
  - U stays queued with a wait reason;
  - `ls` runner rows show `binding=`.
- [ ] **Step 5:** `scripts/dummy-harness.sh down --name bound`, stop the second runner, and record the observations.
