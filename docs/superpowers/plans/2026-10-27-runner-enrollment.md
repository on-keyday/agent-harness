# Runner Enrollment Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Runners authenticate with an enrolled Ed25519 key, agents and the data plane prove their ticket / grant with a transcript-bound binder, and the connect PSK is gone — while old binaries keep connecting through `_legacy` roles until a follow-up change.

**Architecture:** One schema task renames today's handshake roles to `client_legacy` / `runner_legacy` (numbers unchanged, behaviour unchanged) and adds the new shapes beside them. Later tasks switch each sender to its new role and teach each receiver to accept both, then remove the connect PSK, then add the `runner` verbs and the displays.

**Tech Stack:** Go, brgen `.bgn` schemas (`make protoregen`), `crypto/ed25519`, the existing verb table (`cli/verb`), bubbletea TUI, WASM WebUI.

**Spec:** `docs/superpowers/specs/2026-10-27-runner-enrollment-design.md`

## Global Constraints

- Read `.claude/skills/implementation-pitfalls/SKILL.md` in full before writing code.
- Work in the harness worktree this plan lives in; commit on its branch. Never land on `main` before Task 11 passes.
- Role numbers: `client_legacy = 0`, `runner_legacy = 1`, `client = 2`, `runner = 3`. `ClientKind` values are NOT changed (the WAL stores them).
- `PskAuthStatus.not_enrolled = 6`; retryable, like `no_identity`.
- `Capability.runner_enroll = 0x800000`; `all = 0xffffff`; audit tier strong, scope none.
- Runner signature: Ed25519 over `"agent-harness runner-auth v1" || 0x00 || transcript`.
- Agent binder: `cli.ComputePSKBinder(ticket[:], transcript)`; data-plane binder: `cli.ComputePSKBinder(grantID[:], transcript)`.
- Runner key default path: `filepath.Join(os.UserHomeDir(), ".config", "harness", "runner-key")`; hex seed, mode 0600; override `--runner-key-file` / `HARNESS_RUNNER_KEY_FILE`. Never `os.UserConfigDir()`.
- Enrollment store: `<data-dir>/runner-keys.json`, mode 0600, temp file + rename.
- Verbs: `runner enroll <pubkey-hex> [--label L]`, `runner keys`, `runner unenroll <key-prefix> [--force]` (prefix ≥ 8 hex).
- Every list in a response goes on a body stream (`respondWithBody` / `cli.ReadResponseBody`); `runner/protocol/response_rows_test.go` enforces it.
- Tests that run the CLI inside a task: `env -u HARNESS_SERVER_PUBKEY -u HARNESS_SERVER_CID -u HARNESS_AUTH_TICKET -u HARNESS_TASK_ID make test`.
- No test may read or write the real `~/.config/harness/runner-key`.

## Review Focus

1. **The migrating agent loses harness access mid-migration** — a new `harness-cli` against an old server answers `no_identity`. Expected: the order in the spec (server first) works end to end; pinned by Task 11 step 3 (old server + new CLI fails loudly, new server + old runner + new CLI works).
2. **A runner restarted before its key is enrolled** must retry, then connect without manual action once enrolled. Pinned in Task 4 (`TestNotEnrolledRunnerConnectsAfterEnroll`).
3. **`runner unenroll` of the key every slot uses** — refused without `--force`, and the printed undo restores every slot on its next retry. Pinned in Task 8 (`TestUnenrollInUseNeedsForceAndUndoRestores`).
4. **A key file resolved differently by two processes** (systemd unit vs. an agent shell with `XDG_CONFIG_HOME` set). Expected: same path regardless of `XDG_CONFIG_HOME`. Pinned in Task 2 (`TestDefaultRunnerKeyPathIgnoresXDG`).
5. **A legacy agent and a new agent create tasks that read differently** in `ls` / the WAL. Expected: identical `OriginKind`. Pinned in Task 5 (`TestLegacyAndNewAgentRecordSameOriginKind`).

---

### Task 1: The whole schema change, behaviour unchanged

Every wire change of the feature is in this task (pitfalls: one schema task). Afterwards the code compiles and behaves exactly as before: senders and receivers use the renamed `_legacy` roles.

**Files:**
- Modify: `runner/protocol/message.bgn`, regenerated `runner/protocol/message.go`
- Modify (mechanical renames only): `server/psk.go`, `server/pubsub_authz.go`, `server/server.go`, `server/agent_handler.go`, `cli/psk.go`, `cli/psk_js.go`, `cli/dataplane_dial.go`, `runner/connect.go`, `runner/dataplane_accept.go`, and every test the compiler names
- Modify: `cli/verb/caps.go` (`capAuditTable` entry, `CapDescription` for `runner_enroll`)
- Test: `runner/protocol/control_budget_test.go`

**Interfaces — Produces (schema, exact):**

```
enum AuthRole:  client_legacy = 0, runner_legacy = 1, client = 2, runner = 3

format PskAuthRequest:
    binder_len :u16
    binder :[binder_len]u8
    role :AuthRole
    match role:
        AuthRole.client_legacy => client_hello_legacy :ClientHelloLegacy
        AuthRole.runner_legacy => runner_hello :RunnerHello
        AuthRole.client        => client_hello :ClientHello
        AuthRole.runner        => runner_auth :RunnerAuth

format ClientHelloLegacy:   kind :ClientKind; if agent: agent_info :AgentInfoLegacy; if data_plane: data_plane_info :DataPlaneInfoLegacy
format AgentInfoLegacy:     today's AgentInfo, byte for byte (runner_id, task_id, auth_ticket, hostname)
format DataPlaneInfoLegacy: today's DataPlaneInfo (grant_id, task_id)
format ClientHello:         kind :ClientKind; if agent: agent_info :AgentInfo; if data_plane: data_plane_info :DataPlaneInfo
format AgentInfo:           runner_id :RunnerID; task_id :TaskID; hostname_len :u8; hostname
format DataPlaneInfo:       task_id :TaskID
format RunnerAuth:          key :[32]u8; sig :[64]u8; hello :RunnerHello

PskAuthStatus:   + not_enrolled = 6
Capability:      + runner_enroll = 0x800000, "runner_enroll";  all = 0xffffff
TaskControlKind: + runner_enroll, runner_unenroll, runner_keys   (appended)

format RunnerEnrollRequest:    key :[32]u8; label_len :u8; label :[label_len]u8
format RunnerEnrollResponse:   status :RunnerKeyStatus
format RunnerUnenrollRequest:  prefix_len :u8; prefix :[prefix_len]u8 (hex text); force :u1; reserved :u7
format RunnerUnenrollResponse: request_id :u32; status :RunnerKeyStatus; stream_id :u64
format RunnerUnenrollBody:     key :[32]u8; label_len :u8; label; runners_len :u16; runners :[runners_len]RunnerKeyUser
format RunnerKeysRequest:      reserved :u8
format RunnerKeysResponse:     request_id :u32; stream_id :u64
format RunnerKeysBody:         keys_len :u16; keys :[keys_len]RunnerKeyRow; legacy_len :u16; legacy :[legacy_len]RunnerKeyUser
format RunnerKeyRow:           key :[32]u8; label_len :u8; label; enrolled_at_unix_ms :u64; enrolled_by_len :u8; enrolled_by; runners_len :u16; runners :[runners_len]RunnerKeyUser
format RunnerKeyUser:          id :RunnerID; hostname_len :u8; hostname
enum RunnerKeyStatus:          ok, already_enrolled, not_found, ambiguous_prefix, in_use, invalid_key

RunnerInfo: skills_injected :u1; legacy_auth :u1; reserved :u6   (was reserved :u7)
            + key :[32]u8 (zero for a legacy runner); + key_label_len :u8; key_label :[key_label_len]u8   (appended)
ConnInfo:   identified :u1; legacy_auth :u1; reserved :u6        (was reserved :u7)
```

Both union arms of `TaskControlRequest` / `TaskControlResponse` gain the three kinds, with field names `runner_enroll`, `runner_unenroll`, `runner_keys`. In `RunnerUnenrollBody`, `runners` lists the runners registered with the key — closed, or blocking the unenroll when the status is `in_use`.

- [ ] **Step 1: Write the failing budget test** — `TestPskAuthRequestRunnerAuthFitsDatagram` in `control_budget_test.go`: encode a `PskAuthRequest{role: runner}` whose `RunnerAuth.Hello` is the widest hello that test file already builds; assert `len ≤` the budget constant the file already uses.
- [ ] **Step 2: Run** `go test ./runner/protocol/ -run TestPskAuthRequestRunnerAuthFitsDatagram` — FAIL (`RunnerAuth` undefined).
- [ ] **Step 3: Edit `message.bgn`** as above; `make protoregen`.
- [ ] **Step 4: Rename call sites** so they keep today's behaviour: `AuthRole_Client`→`AuthRole_ClientLegacy`, `AuthRole_Runner`→`AuthRole_RunnerLegacy`, `ClientHello()`→`ClientHelloLegacy()` on `PskAuthRequest`, `AgentInfo`→`AgentInfoLegacy` and `DataPlaneInfo`→`DataPlaneInfoLegacy` wherever they are reached from a `ClientHelloLegacy`. `TaskControlKind.client_hello`'s request arm keeps its type `ClientHello` (the new shape); the gate validates a legacy agent's ticket as today, then converts its `ClientHelloLegacy` to a `ClientHello` (ticket dropped) before `pskDispatchIdentity`, so `RecordClientIdentity` and everything after it take the new type. The ticket re-check at `server/agent_handler.go:69` becomes a presence check (`Board.Registry().Ticket(rid, tid)` found): the gate has already proved the ticket. Add `runner_enroll` to `capAuditTable` (`{tier: TierStrong, scope: ScopeNone}`) and to `CapDescription` naming the three verbs.
- [ ] **Step 5: Verify** `make check && make wasm-check && make vet && env -u HARNESS_SERVER_PUBKEY -u HARNESS_SERVER_CID -u HARNESS_AUTH_TICKET -u HARNESS_TASK_ID make test` — all PASS, including the new budget test and `TestResponseRowsAreNotInline`.
- [ ] **Step 6: Verify the wire is unchanged for today's roles**: `scripts/wire-skew-check.sh` — every phase reports connected (handshake-compatible skew).
- [ ] **Step 7: Commit** `protocol: enrollment schema — legacy roles renamed, new roles, runner keys verbs, runner_enroll`.

### Task 2: The runner's key

**Files:**
- Create: `runner/runnerkey.go`, `runner/runnerkey_test.go`, `cli/runner_auth.go`, `cli/runner_auth_test.go`
- Modify: `cmd/agent-runner/main.go`, `runner/connect.go` (`Config`)

**Interfaces — Produces:**
- `runner.DefaultRunnerKeyPath() (string, error)`
- `runner.LoadOrCreateRunnerKey(path string) (ed25519.PrivateKey, error)` — creates the parent dir 0700 and the file 0600 when absent; an unparsable file is an error, never overwritten.
- `runner.RotateRunnerKey(path string) (ed25519.PrivateKey, error)` — renames the file to `path+".prev"` (replacing an older `.prev`), then creates a new key.
- `runner.Config.RunnerKey ed25519.PrivateKey`
- `cli.SignRunnerAuth(priv ed25519.PrivateKey, transcript []byte) []byte`, `cli.VerifyRunnerAuth(pub ed25519.PublicKey, transcript, sig []byte) bool` — beside `cli.SignServerAuth`, same domain-separation shape.
- `cli.RunnerKeyFileEnv = "HARNESS_RUNNER_KEY_FILE"`

- [ ] **Step 1: Failing tests** in `runner/runnerkey_test.go`: `TestLoadOrCreateRunnerKeyCreatesThenReuses` (two calls → same public key; file mode 0600), `TestLoadOrCreateRunnerKeyRefusesCorruptFile` (file holding `"zz"` → error, file unchanged), `TestRotateRunnerKeyKeepsPrev` (new key differs; `.prev` decodes to the old one), `TestDefaultRunnerKeyPathIgnoresXDG` (`t.Setenv("XDG_CONFIG_HOME", t.TempDir())` and `t.Setenv("HOME", h)` → `h/.config/harness/runner-key`). In `cli/runner_auth_test.go`: `TestRunnerAuthSignVerify` (verifies; fails for another transcript and another key).
- [ ] **Step 2: Run** `go test ./runner/ ./cli/ -run 'RunnerKey|RunnerAuth'` — FAIL (undefined).
- [ ] **Step 3: Implement** the functions above.
- [ ] **Step 4: Wire `cmd/agent-runner`**: flags `--runner-key-file` (default from env, then `DefaultRunnerKeyPath`), `--print-pubkey`, `--rotate-key`. The two actions run before any other flag validation (no `--server-cid` needed), print the hex public key alone on stdout and `runner key: <path>` on stderr, exit 0. Normal startup loads the key into `runner.Config.RunnerKey`, logs `slog.Info("runner key", "path", p, "pubkey", hex)`, and unsets `HARNESS_RUNNER_KEY_FILE` beside the operator-PSK names (`cmd/agent-runner/main.go:272`). A flag test in `cmd/agent-runner` asserts `--print-pubkey --runner-key-file <tmp>` prints the same key twice and needs no other flag.
- [ ] **Step 5: Run** the tests above plus `go test ./cmd/agent-runner/` — PASS.
- [ ] **Step 6: Commit** `runner: a persistent Ed25519 runner key; --print-pubkey, --rotate-key`.

### Task 3: The enrollment store

**Files:**
- Create: `server/runnerkeys.go`, `server/runnerkeys_test.go`
- Modify: `server/server.go` (`Config`), `cmd/harness-server/main.go`

**Interfaces — Produces:**
- `type RunnerKeyRecord struct { Key ed25519.PublicKey; Label string; EnrolledAt time.Time; EnrolledBy string }` (JSON: `key` hex, `label`, `enrolled_at` RFC 3339, `enrolled_by`)
- `LoadRunnerKeyStore(path string, initial []RunnerKeyRecord) (*RunnerKeyStore, error)` — `path == ""` = memory only.
- `(*RunnerKeyStore).Lookup(pub ed25519.PublicKey) (RunnerKeyRecord, bool)`
- `(*RunnerKeyStore).Enroll(rec RunnerKeyRecord) (added bool, err error)` — existing key: keeps the record, replaces `Label` only when `rec.Label != ""`.
- `(*RunnerKeyStore).Resolve(prefixHex string) (RunnerKeyRecord, error)` — `ErrKeyNotFound`, `ErrAmbiguousPrefix`, `ErrPrefixTooShort` (< 8).
- `(*RunnerKeyStore).Remove(pub ed25519.PublicKey) error`
- `(*RunnerKeyStore).List() []RunnerKeyRecord` (sorted by `EnrolledAt`)
- `server.Config.RunnerKeysPath string`, `server.Config.InitialRunnerKeys []RunnerKeyRecord`

- [ ] **Step 1: Failing tests**: `TestRunnerKeyStorePersistsAcrossLoad`, `TestRunnerKeyStoreCorruptFileIsAnError`, `TestRunnerKeyStoreReenrollKeepsRecordReplacesLabel`, `TestRunnerKeyStoreResolvePrefix` (unique / ambiguous / too short / missing), `TestRunnerKeyStoreFileMode0600`.
- [ ] **Step 2: Run** `go test ./server/ -run RunnerKeyStore` — FAIL.
- [ ] **Step 3: Implement**; writes go to `path+".tmp"` then `os.Rename`.
- [ ] **Step 4: Wire `cmd/harness-server`**: `RunnerKeysPath = <data-dir>/runner-keys.json`; with no `--data-dir`, `""` and `slog.Warn("no --data-dir: runner enrollments will not survive a restart")`. A corrupt file is a startup error. `server.New` holds the store as `s.runnerKeys`.
- [ ] **Step 5: Run** — PASS. **Step 6: Commit** `server: the runner enrollment store`.

### Task 4: Runners authenticate with their key

**Files:**
- Modify: `runner/connect.go` (`sendRunnerMergedHandshake`, `driveAfterConn`), `cli/persist.go` (`Retryable`), `server/psk.go`, `server/pubsub_authz.go`, `server/server.go` (gate wiring, `streamingConn`, `connInfoFor`), `server/registry.go` (`RunnerEntry.Key`, `Add`), `server/runner_handler.go`, `server/task_handler.go` (`toRunnerInfo`)
- Test: `server/runner_enroll_e2e_test.go` (new), `cli/psk_retryable_test.go`, `server/registry_test.go`

**Interfaces:**
- Consumes: Task 2's `RunnerKey`, `SignRunnerAuth` / `VerifyRunnerAuth`; Task 3's store.
- Produces: `pskGate.Role() protocol.AuthRole` returns the normalised role (`AuthRole_Client` or `AuthRole_Runner`) for both legacy and new; `pskGate.Legacy() bool`; `pskGate.RunnerKey() ed25519.PublicKey`. `streamingConn.legacyAuth bool`, `streamingConn.authKey ed25519.PublicKey`, set when the gate accepts. `RunnerHandler.AuthOf func(cid objproto.ConnectionID) (key ed25519.PublicKey, legacy bool)`. `RunnerEntry.Key ed25519.PublicKey` (nil = legacy). `Registry.Add(e *RunnerEntry) (displaced objproto.ConnectionID, err error)` with `ErrIdentityKeyMismatch`. `runner.Config.NotEnrolledOnce *sync.Once` (set by `cmd/agent-runner`).

- [ ] **Step 1: Failing negative control** `TestRunnerWithoutEnrolledKeyIsRefused` (real server, a runner `Config` with a fresh key not in the store): expect `cli.PskRejectedError` with `Code == PskAuthStatus_NotEnrolled` and no registry entry. On today's code the runner registers. Also `TestNotEnrolledRunnerConnectsAfterEnroll` (same runner under `PersistLoop` with a short backoff; `Enroll` the key; the registry gains it), `TestEnrolledRunnerRegistersWithKey` (`RunnerInfo.Key` equals the key, `LegacyAuth` false, `KeyLabel` the label), `TestLegacyRunnerRoleStillRegisters` (hand-built `runner_legacy` request → registered, `RunnerInfo.LegacyAuth` true, its `ConnInfo.LegacyAuth` true), `TestRegistryRefusesTakeoverByDifferentKey` (same `runner_id`, different key → `ErrIdentityKeyMismatch`, original entry still current) and `TestRegistrySameKeyTakeoverStillWorks`. In `psk_retryable_test.go` add the `not_enrolled → true` row.
- [ ] **Step 2: Run** them — FAIL.
- [ ] **Step 3: Runner side**: `sendRunnerMergedHandshake` sends `role=runner`, `RunnerAuth{key: pub, sig: cli.SignRunnerAuth(priv, transcript), hello: buildRunnerHello(cfg)}`, `binder_len = 0`. On a `not_enrolled` rejection, `NotEnrolledOnce.Do` logs one WARN: `runner key <hex> is not enrolled; run: harness-cli runner enroll <hex> --label <hostname>`. `Retryable()` returns true for `NoIdentity` and `NotEnrolled`.
- [ ] **Step 4: Server gate**, `role=runner`: verify sig → `bad_psk`; `Lookup` → `not_enrolled` + `slog.Warn("runner key not enrolled", "key", hex, "hostname", …, "cid", …)`; zero `runner_id` → `no_identity`; accept. `role=runner_legacy`: today's path plus `slog.Info("legacy runner handshake", "hostname", …, "cid", …)`. `AllowsMessage` and `authorizeJoin` read the normalised role. `pskDispatchIdentity` takes the hello from either arm. The gate's accept stores `legacyAuth` / `authKey` on the `streamingConn`.
- [ ] **Step 5: Registry and rows**: the runner handler sets `RunnerEntry.Key` via `AuthOf`; `Add` refuses a different non-nil key on a live identity (caller closes the new connection, `slog.Warn` with both key prefixes). `toRunnerInfo` fills `Key`, `KeyLabel` (store lookup), `LegacyAuth`; `connInfoFor` sets `LegacyAuth`.
- [ ] **Step 6: Run** the Step 1 tests and `go test ./server/ ./runner/ ./cli/` — PASS.
- [ ] **Step 7: Commit** `runner, server: runners prove an enrolled key; legacy runners still accepted and marked`.

### Task 5: Agents prove their ticket with a binder

**Files:**
- Modify: `cli/psk.go` (`buildMergedClientHello`, `resolveBinderPSK`, `SendMergedHandshake`), `cli/psk_js.go`, `server/psk.go`, `server/server.go` (gate wiring), `server/agent_handler.go`
- Test: `server/agent_binder_e2e_test.go` (new)

**Interfaces:**
- Produces: every new client sends `role=client`. `pskGate.TicketFor func(rid protocol.RunnerID, tid protocol.TaskID) ([16]byte, bool)`, wired to `s.Board.Registry().Ticket`. After acceptance the gate hands identity code a `*protocol.ClientHello` for both roles (a legacy hello is converted, ticket dropped), so `RecordClientIdentity` and everything after it sees one type.

- [ ] **Step 1: Failing tests**: `TestAgentBinderFromAnotherConnectionIsRefused` (compute an agent binder over connection A's transcript, present it on connection B → `bad_ticket`); `TestAgentRoleClientCarriesNoTicket` (encode the request `buildMergedClientHello` produces for an agent env; assert the 16 ticket bytes do not occur in it); `TestLegacyAgentHelloStillAccepted`; `TestLegacyAndNewAgentRecordSameOriginKind` (each creates a task via `submit`; the two `TaskEntry.OriginKind` values are equal); `TestOperatorKindsUnderRoleClientProveOperatorPSK`.
- [ ] **Step 2: Run** — FAIL.
- [ ] **Step 3: Client**: `role=client`; agent → `AgentInfo{RunnerId, TaskId, Hostname}` and `binder = ComputePSKBinder(ticket[:], transcript)`; operator kinds → binder from the operator PSK (falling back to the connect PSK only until Task 7). WASM: `role=client`.
- [ ] **Step 4: Server**: `binderKey` for `role=client, kind=agent` = `TicketFor(rid, tid)`; missing → `bad_ticket`. The constant-time binder compare is the ticket check; `ValidateTicket` remains for `client_legacy` only.
- [ ] **Step 5: Run** — PASS. **Step 6: Commit** `cli, server: agents prove the ticket with a transcript binder instead of sending it`.

### Task 6: The data plane proves its grant with a binder

**Files:**
- Modify: `cli/dataplane_dial.go` (`sendDataPlaneHello`), `runner/dataplane_accept.go` (`checkDataPlaneHello`), `runner/dataplane_grants.go`
- Test: `runner/dataplane_accept_test.go`, `cli/dataplane_dial_test.go`

**Interfaces — Produces:** `(*grantStore).MatchBinder(taskID protocol.TaskID, transcript, binder []byte) (protocol.DataPlaneGrant, bool)` — constant-time compare against `ComputePSKBinder(g.GrantId[:], transcript)` for each live grant of the task; the matched grant then goes through the existing `Validate` (kind, direction, TTL).

- [ ] **Step 1: Failing tests**: `TestDataPlaneBinderFromAnotherConnectionIsRefused` (→ `bad_ticket`), `TestDataPlaneBinderPicksTheMatchingGrant` (two live grants on one task; the hello bound to the second is accepted as the second), `TestLegacyDataPlaneHelloStillAccepted`, `TestDataPlaneHelloCarriesNoGrantID`.
- [ ] **Step 2: Run** — FAIL. **Step 3: Implement** (client: `role=client`, `DataPlaneInfo{TaskId}`, grant binder; runner: `role=client` → `MatchBinder`, `role=client_legacy` → today's path). **Step 4: Run** — PASS.
- [ ] **Step 5: Commit** `cli, runner: the data plane proves its grant with a transcript binder`.

### Task 7: Remove the connect PSK

**Files:**
- Modify: `cmd/harness-server/main.go` (`--psk`, `--psk-file`, `resolvePSK`, the `--dangerously-permit-no-operator-psk` help), `server/server.go` (`Config.PSK`), `server/psk.go` (`psk`, `binderKey` fallback), `cmd/agent-runner/main.go` (`--psk`, `--psk-file`), `runner/connect.go` (`Config.PSK`), `runner/agentenv.go` (`AgentEnvSpec.PSK`, `HARNESS_PSK`), `runner/dataplane_accept.go`, `cli/psk.go` (`GetPSK`), `cli/psk_js.go` (`GetPSK` → `GetOperatorPSK`, callers), `cli/client.go`
- Modify scripts: `scripts/wire-skew-check.sh`, `scripts/dummy-harness.py`, `scripts/netem-lab/netem-lab.py`, `scripts/netem-lab/test_netem_lab.py`, `scripts/e2e-oneshot-progress.sh`, `scripts/restart.py`, `scripts/server.py`, `scripts/test_daemon.py`
- Modify tests: every file `grep -rlnE 'HARNESS_PSK\b|GetPSK\(|\.PSK\b|--psk\b' --include=*_test.go .` lists, plus `integration/psk_e2e_test.go`

- [ ] **Step 1: Delete** the flags, fields, env and functions listed. `client_legacy` / `runner_legacy` keep working: with no connect PSK their binder was already skipped, which is today's behaviour on this fleet. Operator kinds prove the operator PSK only; `--dangerously-permit-no-operator-psk` now means they prove nothing — update its help text to say so.
- [ ] **Step 2: Scripts.** Seed keys instead of passing `--psk`: a script that starts a NEW runner first runs `agent-runner --print-pubkey --runner-key-file "$TMP/runner-key"`, writes `$DATA/runner-keys.json` (`[{"key":"<hex>","label":"<name>","enrolled_at":"<now>","enrolled_by":"operator"}]`) before the server starts, and passes `--runner-key-file` to the runner. In `wire-skew-check.sh`, OLD binaries keep `--psk` off as well (their flag is optional), and phase 1's self-heal step seeds the new runner's key into the NEW server's data dir. Drop `HARNESS_PSK` from `dummy-harness.py`'s unset list.
- [ ] **Step 3: Verify** `make check && make wasm-check && make vet && env -u … make test && make test-integration` — PASS; `grep -rnE 'HARNESS_PSK\b|GetPSK\(|--psk\b|PSKFile' --include=*.go --include=*.py --include=*.sh .` returns nothing outside `docs/`.
- [ ] **Step 4: Commit** `remove the connect PSK: runners use keys, agents tickets, the data plane grants`.

### Task 8: The `runner` verbs

**Files:**
- Create: `server/runner_keys_handler.go`, `server/runner_keys_handler_test.go`, `cli/runner_keys.go` (`RunnerEnroll` / `RunnerEnrollWith`, `RunnerKeys` / `RunnerKeysWith`, `RunnerUnenroll` / `RunnerUnenrollWith`), `tui/runner_keys.go` (`DoRunnerEnroll`, `DoRunnerKeys`, `DoRunnerUnenroll`)
- Modify: `server/capabilities.go` (three kinds → `Capability_RunnerEnroll`), `server/task_handler.go` (dispatch), `cli/verb/table.go` (three `VerbSpec`s, `WebUIDispatch` `runnerEnroll` / `runnerKeys` / `runnerUnenroll`, `CmdlineSurfaces: CLI | TUI | WebUI`, `NoModalSurface` with the `server dial-runner` reason), `cmd/harness-cli/dispatch.go`, `tui/dispatch.go`, `cmd/harness-webui-wasm/main.go`, `webui/static/main.js`; `go generate ./cli/verb/`

**Interfaces:**
- `cli.RunnerEnrollWith(ctx, c taskControlClient, key ed25519.PublicKey, label string) (protocol.RunnerKeyStatus, error)`; `cli.RunnerKeysWith(ctx, c *Client) ([]RunnerKeyRow, []RunnerKeyUser, error)`; `cli.RunnerUnenrollWith(ctx, c *Client, prefix string, force bool) (protocol.RunnerKeyStatus, RunnerUnenrollResult, error)` — TUI and WebUI call the `With` forms on their long-lived client (pitfall 3).
- `cli.RunnerKeyRow{Key ed25519.PublicKey; Label string; EnrolledAt time.Time; EnrolledBy string; Runners []RunnerKeyUser}`, `cli.RunnerKeyUser{ID protocol.RunnerID; Hostname string}`, `cli.RunnerUnenrollResult{Key ed25519.PublicKey; Label string; Runners []RunnerKeyUser}` — decoded from the protocol bodies.
- `enrolled_by` = `operator` for a connection with no principal task, else `task:<hex id>`.
- `Build` validation: enroll needs exactly 64 hex characters; unenroll's prefix 8–64 hex.

- [ ] **Step 1: Failing tests** (server): `TestRunnerVerbsNeedRunnerEnroll` (a `--caps none` agent gets `permission_denied` on all three), `TestRunnerEnrollRecordsEnrolledBy` (operator → `operator`; an agent with the bit → `task:<id>`), `TestRunnerKeysListsUsersAndLegacy`, `TestUnenrollInUseNeedsForceAndUndoRestores` (a registered runner on the key: without `force` → `in_use` with that runner in the body and the key still enrolled; with `force` → runner connection closed, body carries the full key; re-enrolling that key lets the runner reconnect on its next retry), `TestUnenrollUnusedKeyNeedsNoForce`. Verb table: `go test ./cli/verb/` stays green (`TestEveryWebUIVerbDeclaresDispatch`, `TestNoHandWrittenVerbFlagSetsRemain`).
- [ ] **Step 2: Run** — FAIL. **Step 3: Implement** the handler (bodies via `respondWithBody`), client functions (`ReadResponseBody`), and the three surfaces.
- [ ] **Step 4: Output** (same text on all three surfaces): `enrolled <key16> label=<label>` / `already enrolled <key16> label=<label>`; `runner keys` one block per key — `<key16> label=<label> enrolled=<RFC3339> by=<enrolled_by> runners=<n>` then one indented line per runner `<id8> <hostname>` — then `legacy runners=<n>` and its lines (zero printed, never elided); unenroll refused: `refused: <n> runner(s) use <key16> (pass --force to cut them off):` + lines; unenroll done: `unenrolled <key16> label=<label>, closed <n> runner(s)` + `undo: harness-cli runner enroll <64 hex> --label <label>`. TUI writes `a.cmdresult`, WebUI `appendCmdOutput`. CLI exits non-zero on any status other than `ok` / `already_enrolled`.
- [ ] **Step 5: Run** — PASS; `make js-test` PASS. **Step 6: Commit** `runner enroll / keys / unenroll on every surface`.

### Task 9: Key label and legacy on runner and connection rows

**Files:**
- Modify: `cli/list.go` (row: `key=<label or key16>` or `key=legacy`; `runnerJSON`: `key` hex, `key_label`, `legacy_auth`), `tui/runners.go` (`runnerColsFor` + `rebuild`: a `Key` column shown when the width allows, both changed together per checklist item 34), `tui/detail.go` (`formatRunnerDetail`: `key:` line), `cmd/harness-webui-wasm/main.go` (runner map `key`, `keyLabel`, `legacyAuth`; conn map `legacyAuth`), `webui/static/main.js` (`renderRunnerRows` line, connections view badge), `cli/conns.go` (text `legacy` marker, JSON `legacy_auth`), `tui/conns.go`
- Test: `cli/list_test.go`, `cli/conns_test.go`, `tui/runners_test.go`

- [ ] **Step 1: Failing tests**: `TestRunnerRowShowsKeyLabelOrLegacy` (label → `key=<label>`; empty label → `key=<key16>`; legacy → `key=legacy`), `TestRunnerJSONCarriesKeyFields`, `TestConnRowMarksLegacy`, `TestRunnerTableKeyColumnSwapsWithRows` (resize across the threshold does not panic and keeps the cursor).
- [ ] **Step 2: Run** — FAIL. **Step 3: Implement.** **Step 4: Run** — PASS, `make js-test` PASS.
- [ ] **Step 5: Commit** `ls, conns, TUI, WebUI: runner key label and legacy handshakes`.

### Task 10: Docs, the parity walk, the backlog

**Files:** `README.md`, `.claude/commands/runner-up.md`, `.claude/skills/dummy-harness/SKILL.md`, `scripts/kvm-runner/README.md`, `scripts/runner-autostart.py` (docstring), `scripts/sandbox/README.md` (only if a probe or the security model names `HARNESS_PSK` or `~/.config/harness`), `.claude/skills/surface-parity-checklist/firing-log.md`, `docs/backlog.md`, the spec (Amendment section)

- [ ] **Step 1: README** — Quick start without `--psk`; how a runner is enrolled; the three verbs; `runner_enroll` in Capabilities; key rotation.
- [ ] **Step 2: `runner-up.md` and the autostart docstring** — the agent's three commands (`agent-runner --print-pubkey`, `harness-cli runner enroll <key> --label <host>`, then `runner.sh up` / `register`), and that the agent needs `runner_enroll`.
- [ ] **Step 3: kvm-runner README** — create and enroll the guest key before taking a snapshot.
- [ ] **Step 4: Spec Amendment** — `wire-skew-check.sh` phase 3 already runs OLD runner × NEW server (the spec's Testing section says it does not); record that, and any other deviation the tasks made.
- [ ] **Step 5: Backlog** — as the spec's "When this lands" says.
- [ ] **Step 6: Firing log** — one entry for this feature's walk (the verdicts below), `done` / `omitted` only.
- [ ] **Step 7: Commit** `docs: runner enrollment`.

### Task 11: End-to-end verification

- [ ] **Step 1:** `make check && make wasm-check && make vet && env -u HARNESS_SERVER_PUBKEY -u HARNESS_SERVER_CID -u HARNESS_AUTH_TICKET -u HARNESS_TASK_ID make test && make test-integration` — PASS.
- [ ] **Step 2:** `scripts/wire-skew-check.sh` — phase 1 rejected-then-self-healed, phase 3 connected.
- [ ] **Step 3: Dummy harness, by the documented command lines** (pitfall 13), new server:
  - an OLD-binary runner (`git worktree add` at `main`, `go build -o`) registers; `harness-cli ls` shows `key=legacy`; `conns` marks it legacy;
  - a NEW runner with a fresh key logs the enroll hint and retries; `harness-cli runner enroll <printed key> --label t` → it registers on the next retry; `ls` shows `key=t`;
  - `runner unenroll <prefix>` refuses and names it; `--force` closes it and prints `undo:`; running the undo line brings it back;
  - from inside a task on the new runner: `harness-cli ls` works and `file push` / `file pull` round-trip;
  - an OLD `harness-cli` against the NEW server works (`client_legacy`); the NEW `harness-cli` against an OLD server fails with an error naming `no_identity`.
- [ ] **Step 4:** Tear the dummy harness down; report the screenshots/log paths if any were taken.

After Task 11 the branch is landed (FF to `main`, `make build` in the main
checkout). The fleet migration in the spec's "Version skew and the migration"
is an operation, not a task here: the operator restarts the server first. The
follow-up change that deletes the `_legacy` roles gets its own plan once no
runner or connection row reads `legacy`.

## Surface-parity walk (1–39, S1–S6)

| # | Verdict |
| --- | --- |
| 1 | done — three `VerbSpec`s (Task 8) |
| 2 | done — hex length checks in `Build` (Task 8) |
| 3 | n/a — note item |
| 4 | omitted — no keybinding: rare administrative verbs, reached from the TUI command line like `server dial-runner` |
| 5 | n/a — no picker |
| 6 | omitted — no WebUI button, same reason as 4 |
| 7 | done — `WebUIDispatch` per verb + `main.js` routing (Task 8) |
| 8 | n/a — `Bound` crosses generically |
| 9 | n/a — no session default |
| 10 | n/a — note item |
| 11 | done — `ls` rows (Task 9) |
| 12 | done — `ls --json` (Task 9) |
| 13 | n/a — `whoami` prints cap names from the enum, so `runner_enroll` appears with no edit |
| 14 | n/a — not a session field |
| 15 | done — `CapDescription` for `runner_enroll` (Task 1) |
| 16, 17 | n/a — task surfaces |
| 18 | done — runner detail `key:` (Task 9) |
| 18a | done — runner table `Key` column (Task 9) |
| 19–22 | n/a — task surfaces |
| 23 | done — wasm runner and conn maps (Task 9) |
| 24 | done — `--label` on an enrolled key replaces the label; `--force` exists only on unenroll |
| 25 | done — an empty `--label` means "keep", written in the verb notes; there is no way to clear a label, by decision |
| 26–28a | n/a — no spawn field, no WAL field |
| 29 | done — result lines name the key and the change (Task 8) |
| 30 | done — `a.cmdresult` / `appendCmdOutput` (Task 8) |
| 31 | done — `runners=0` and `legacy runners=0` printed; `key=` always present (Tasks 8, 9) |
| 32 | n/a — no new grammar |
| 33 | done — unenroll of a key in use errors without `--force` |
| 34 | done — `Key` column built through `runnerColsFor` + `rebuild` together (Task 9) |
| 34a | n/a — no form field |
| 35 | done — README (Task 10) |
| 36 | n/a — no embedded agent skill names the connect PSK (`grep -rn HARNESS_PSK runner/agentskills` is empty); `runner-up.md` is repo-local (Task 10) |
| 37 | done — spec Amendment (Task 10) |
| 38 | n/a — no screen rendering |
| 39 | done at Task 11 — the spec's Operator surfaces section checked row by row |
| S1–S4 | n/a — no preset, wrapper dispatch, agent table or firewall field changes |
| S5 | done — the env handed to agents loses `HARNESS_PSK`; the wrapper forwards `HARNESS_*` by prefix, so nothing to remove there; `HARNESS_RUNNER_KEY_FILE` is unset by the runner before spawning (Task 2) |
| S6 | done — `scripts/sandbox/README.md` / `probe.sh` checked for `HARNESS_PSK` and `~/.config/harness` (Task 10) |
