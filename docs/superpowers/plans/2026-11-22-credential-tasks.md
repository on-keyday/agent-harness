# Credential Tasks Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A task kind with no runner and no process whose Ed25519 key lets an external script connect to the harness with that task's caps and scope, with expiry, revoke, re-key, and visibility on CLI/TUI/WebUI.

**Architecture:** The credential is a `TaskEntry` (`TaskKind.credential`, `TaskStatus.active`) carrying a public key and an expiry, persisted through the existing JSON WAL. A new handshake arm `AuthRole.credential` proves the private key by signing the objproto transcript; the server records the connection's principal as that task, so every existing cap/scope check applies unchanged. Issue and re-key are TaskControl kinds gated by a new `credential_issue` bit through the spawn grant path (`grantFor`).

**Tech Stack:** Go, brgen `.bgn` schema → `ebm2go` (`go generate`), Go wasm WebUI + vanilla JS, Bubble Tea TUI.

**Spec:** `docs/superpowers/specs/2026-11-21-credential-tasks-design.md` (cited as D1…D20). Task 1 also amends `docs/superpowers/specs/2026-11-15-pillar-s-schedules-design.md`.

## Global Constraints

- Every subagent: read `.claude/skills/implementation-pitfalls/SKILL.md` in full first; read the spec's **Why** and **Goals** sections verbatim and report which bullets the diff addresses.
- Work in this worktree; commit on its `harness/<dir>` branch; stage by explicit path; `git show --stat` after each commit. Do not land, push or `make build` — the lander does that after the whole plan.
- Verify with `make test` (never bare `go test` inside a task: inherited `HARNESS_*` identity breaks signature tests). Compile checks: `go build ./...` or `go vet ./<pkg>` — never a bare `go build ./cmd/x`.
- Exact values: `credential_issue = 0x80000000`; `Capability.all = 0xffffffff`; `TaskStatus.active` appended after `Held` (value 7); `TaskKind.credential` appended (value 3); `EndReason.expired = 11`; `AuthRole.credential = 4`; `PskAuthStatus.revoked = 7`; max TTL **90 days** (`CredentialMaxTTL = 90 * 24 * time.Hour`), no default TTL.
- Signature domain string: `"agent-harness credential-auth v1\x00"`, message = domain ‖ transcript (same shape as `cli/runner_auth.go:12`).
- Credential string: `<32 hex task id>:<64 hex Ed25519 seed>`. Credential file: that string plus a trailing newline, mode `0600`. Public key on the CLI: 64 hex.
- Client credential sources, first match wins: `--credential-file PATH` (global flag), `HARNESS_CREDENTIAL_FILE`, `HARNESS_CREDENTIAL`. When one is present the client sends `AuthRole.credential` — never an operator binder and never an agent hello, even if `HARNESS_OPERATOR_PSK` or `HARNESS_TASK_ID`/`HARNESS_AUTH_TICKET` are also set.
- Masked bits on a credential task (D14): `credential_issue | schedule_edit`. `spawn` is allowed.
- A seed is never logged, never written to browser storage, never put in `ls`/`whoami` output.
- No LAN addresses, hostnames or private paths in committed files.

## Review Focus

1. **A credential whose task record is gone** (pruned after revoke while a connection stayed open): expected `callerCaps` = None and an empty visible set — today `scopeSetWith` falls back to `DefaultScope()` (Subtree) for a missing record (`server/capabilities.go:305`). Test in Task 4.
2. **Server restart with a live credential**: expected it is still `active` and authenticates; one whose expiry passed during downtime is ended `expired` at start, not left active until the next request. Test in Task 3.
3. **A shell carrying both operator PSK and `HARNESS_CREDENTIAL`** (an operator testing a script from their own terminal): expected the connection is the credential (whoami shows the credential task, `schedule_edit` refused), never silently the operator. Test in Task 7.
4. **An agent task's shell that sets `HARNESS_CREDENTIAL`** (`HARNESS_TASK_ID` + ticket also present): expected the credential identity, not the agent. Test in Task 7.
5. **TTL edges**: `--ttl 0`, negative, unparseable, `91d`: expected refusal naming the 90-day bound; expiry computed on the server clock from a duration, never a client-supplied absolute time. Tests in Task 5 (server) and Task 7 (flag parsing).

---

### Task 1: Pillar S amendment — `schedule run` accepts `resume_exact` (D16)

Independent of the rest; lands first.

**Files:**
- Modify: `server/schedule_handler.go:678-722` (`schedulePower` doc + run path), `:1023-1037` (`scheduleRunCheck`)
- Modify: `cli/verb/caps.go:197-200` (ScheduleRun description), `cli/verb/table.go:~3301` (run Notes), `README.md:~1908-1923`
- Modify: `docs/superpowers/specs/2026-11-15-pillar-s-schedules-design.md` (new `## Amendment — 2026-11-22: manual firing accepts resume_exact` section after the 2026-11-21 one, superseding S-D17a for `run` only; Decided-by: operator, 2026-11-22)
- Test: `server/schedule_power_test.go`

**Interfaces:**
- Produces: `func (h *TaskHandler) scheduleRunPower(cid string, sc *Schedule) (protocol.Capability, *protocol.ScheduleResponse)` — used only by `scheduleRunCheck`.

- [ ] **Step 1: Update the failing tests.** In `TestSchedulePowerResumeExactRefused` (`:265`) flip the "run with resume_exact only" case to allowed and rename the test `TestSchedulePowerResumeExactRunsButDoesNotEdit`; keep its edit/bind cases denied `Capability_Resume`. In `TestSchedulePowerRun` (`:126`), `TestSchedulePowerDenialsOverTheWire` (`:281`), `TestSchedulePowerRunChecksStateAtFiring` (`:322`) the deny cap for a bound run with neither bit becomes `protocol.Capability_Resume | protocol.Capability_ResumeExact`. Add `TestScheduleRunExactOutOfScopeIsNotReachable`: caller holds `ScheduleRun|ResumeExact` with `resume_exact` scoped to task A; running a schedule bound to B answers `ScheduleStatus_TaskNotReachable`.
- [ ] **Step 2:** `make test` restricted to `./server/ -run 'SchedulePower|ScheduleRun'` → FAIL on the flipped cases.
- [ ] **Step 3: Implement `scheduleRunPower`.** pending_create → needs `spawn` (unchanged); unbound → nothing; bound to T → `!hasAnyCap(callerCaps, resumeRequestCap(tid, true))` denies with that mask, else `!h.resumeTargetAllowed(cid, true, T)` → `TaskNotReachable`. `scheduleRunCheck` calls it instead of `schedulePower(cid, &sc, "")`. Rewrite the `schedulePower` doc comment: it now serves new/edit only; the "resume_exact is not enough" sentence moves to edit/bind, with the reason run differs (firer picks timing only; bind `:724` and edit `:856` already required full resume over T).
- [ ] **Step 4:** same test command → PASS; then full `make test`.
- [ ] **Step 5:** update the description/notes/README wording ("…or resume or resume_exact over its bound task") and write the pillar-S amendment section. Commit: `schedule: manual run accepts resume_exact over the bound task (S-D17a amendment)`.

---

### Task 2: Wire schema and per-bit tables (the whole schema, one task)

**Files:**
- Modify: `runner/protocol/message.bgn` → regenerate `runner/protocol/message.go`
- Modify: `runner/protocol/conn_role.go`, `runner/protocol/task_kind.go`, `runner/protocol/capability_test.go`
- Modify: `cli/verb/caps.go` (`GrantableCaps`, `CapDescription`, `capAuditTable` with `{tier: TierStrong, scope: ScopeNone}`), `cli/verb/caps_audit_table_test.go` (count 31 → 32, TierStrong list), `server/scope_percap_completeness_test.go` (`capNoTargetResolution`, reason "issuing names no existing task; the new task's bound is the grant path")
- Test: `runner/protocol/credential_wire_test.go` (new)

**Interfaces — Produces (all appended; numbers per Global Constraints):**
- `Capability.credential_issue = 0x80000000, "credential_issue"`; `all = 0xffffffff`; extend the comment at `:4945-4951` with the 0x7fffffff clause.
- `TaskStatus.active`, `TaskKind.credential`, `EndReason.expired = 11`, `ClientKind.credential`, `ConnRole.credential`; `ConnRoleForClientKind(ClientKind_Credential) == ConnRole_Credential`; `IsSessionKind` / `IsPTYKind` false for credential.
- `AuthRole.credential = 4` with arm `credential_auth :CredentialAuth`; `format CredentialAuth: task_id :TaskID; sig :[64]u8`.
- `PskAuthStatus.revoked = 7` (comment: signature verified, credential cancelled).
- `TaskControlKind.credential_issue`, `TaskControlKind.credential_rekey` (appended), with request/response arms:
  - `CredentialIssueRequest`: `pubkey :[32]u8`, `ttl_ms :u64`, `name_len :u8`, `name :[name_len]u8`, then the same grant fields `OpenInteractiveRequest` carries for spawn (caps, scope, overrides, delegate caps/scope, spawn limits) — copy their exact field shapes so `openGrantRequest`'s logic can be shared.
  - `CredentialRekeyRequest`: `task_id :TaskID`, `pubkey :[32]u8`, `ttl_ms :u64`, `caps_present :u1` + the same optional grant fields as issue (narrowing only).
  - `CredentialResponse`: `status :CredentialStatus`, `task_id :TaskID`, `expires_at :u64` (unix nanos, server clock), `message_len :u16`, `message :[message_len]u8`.
  - `enum CredentialStatus`: `ok`, `ttl_invalid`, `ttl_too_long`, `bad_pubkey`, `scope_not_permitted`, `spawn_limit`, `not_credential`, `no_such_task`, `masked_cap` (requested `credential_issue`/`schedule_edit`).
- `TaskInfo` appended: `credential_expires_at :u64`, `credential_last_used_at :u64` (0 = unknown since server start), `credential_name_len :u8`, `credential_name :[credential_name_len]u8`.
- `WhoAmIResponse` appended: `principal_kind :ClientKind`, `credential_expires_at :u64`.
- `SetCapsStatus` (or the set_caps response enum): appended `masked_cap`.

- [ ] **Step 1: Write the failing tests.** `TestCapabilityBits` expects `Capability_All == 0xffffffff` and the OR list includes `Capability_CredentialIssue`; `TestCredentialIssueBitNotInOldAll`: `0x7fffffff & uint64(Capability_CredentialIssue) == 0`. In `credential_wire_test.go`: round-trip `PskAuthRequest{Role: AuthRole_Credential}` with a `CredentialAuth`; round-trip both TaskControl request kinds and `CredentialResponse`; `TaskInfo` and `WhoAmIResponse` round-trip with the new trailing fields set.
- [ ] **Step 2:** `make test` scoped to `./runner/protocol/` → FAIL (undefined symbols).
- [ ] **Step 3:** edit `message.bgn`, run `go generate ./runner/protocol/` (accept the regen churn; check build/vet/test and that the new symbols exist), update `conn_role.go`, `task_kind.go`, and the per-bit tables listed above.
- [ ] **Step 4:** `make test` → PASS (includes `TestCapabilityStaysExactInTheWebUI`, `TestGrantableCapsCoversEveryBitOfAll`, `TestEveryCapabilityDeclaresHowItsTargetIsResolved`, `TestAuditScopeKindAgreesWithTargetClasses`).
- [ ] **Step 5:** Commit `protocol: credential tasks schema (credential_issue bit, active status, credential auth arm)`.

---

### Task 3: TaskStore — credential rows, re-key, expiry, mask, replay

**Files:**
- Create: `server/credential_store.go`
- Modify: `server/taskstore.go` (`TaskEntry` fields; `ReplayEvents` cases; `SetCaps` mask; `BackgroundDeadlines` → see below), `server/wal.go` (`WALEvent` + `walEventJSON` fields), `server/deadline_watch.go`
- Test: `server/credential_store_test.go`

**Interfaces:**
- Consumes: Task 2 enums.
- Produces:
  - `TaskEntry` fields: `CredentialPubKey [32]byte`, `ExpiresAt time.Time`, `LastUsedAt time.Time`, `CredentialName string`.
  - `WALEvent` fields: `CredentialPubKey string \`json:"credential_pubkey,omitempty"\`` (hex), `ExpiresAtNs int64 \`json:"expires_at_ns,omitempty"\``, `CredentialName string \`json:"credential_name,omitempty"\`` — on `task_created`, and the new event `task_rekeyed` (TaskID, CredentialPubKey, ExpiresAtNs, Ts, plus caps fields when narrowed).
  - `const CredentialMaxTTL = 90 * 24 * time.Hour`
  - `func (s *TaskStore) CreateCredential(creator protocol.TaskID, origin protocol.ClientKind, g authority.Grant, pub [32]byte, expiresAt time.Time, name string) (string, error)` — through `createWithinLimits` (spawn limits apply), `Kind = TaskKind_Credential`, `Status = TaskStatus_Active`, `StartedAt = now`, no repo/prompt/runner; `g.Caps &^= CredentialMaskedCaps` defensively; WAL `task_created` with the three fields; fires `OnCreate`. `OnCreate` publication must not publish `TaskQueued` for this kind — publish the status the row holds.
  - `const CredentialMaskedCaps = protocol.Capability_CredentialIssue | protocol.Capability_ScheduleEdit`
  - `func (s *TaskStore) RekeyCredential(id string, pub [32]byte, expiresAt time.Time, u *GrantUpdate, by AuthorityWrite) (TaskEntry, error)` — errors `ErrNotCredential` / `ErrNoSuchTask`; works from `active`, `cancelled`, or any terminal status; sets `Status = active`, clears `EndedAt`, `EndReason`, `CancelledBy`; replaces key and expiry; applies `u` (narrowing) through the same code `SetCaps` uses; WAL `task_rekeyed`.
  - `func (s *TaskStore) TouchCredential(id string, at time.Time)` — memory only, no WAL (D20).
  - `func (s *TaskStore) CredentialLive(id string, now time.Time) (pub [32]byte, state CredentialState, ok bool)` with `type CredentialState uint8` = `CredentialLive | CredentialExpired | CredentialRevoked`; `ok=false` for a missing or non-credential task. "Expired" = `active` with `ExpiresAt <= now`, or `Cancelled` with `EndReason_Expired`; "Revoked" = any other ended status.
  - Expiry: `BackgroundDeadlines(now)` also yields `DeadlineHit{ID, Reason: EndReason_Expired}` for `Kind == credential && Status == active` at `ExpiresAt` (the scan currently skips non-background — add the credential branch before that `continue`). `runDeadlineWatcher`'s `end` callback routes `EndReason_Expired` to `s.Cancel(id, EndReason_Expired, zero)` (not `CancelBackgroundRun`). `CreateCredential` and `RekeyCredential` call `s.deadlineSet(true)`.
  - `SetCaps`: when `e.Kind == credential`, `u.To.Caps &^= CredentialMaskedCaps` and `u.To.DelegateCaps &^= CredentialMaskedCaps` before writing (covers set_caps and its cascade).
  - Replay: `task_created` with kind credential restores the three fields and `Status = active` (not Queued); `task_rekeyed` replays as above; the post-replay sweeps (`:1598-1627`) must leave `active` rows alone (they do by status; pin with a test). After replay, any `active` credential with `ExpiresAt <= now` is cancelled `expired` once at start.

- [ ] **Step 1: Write the failing tests** (`credential_store_test.go`):
  - `TestCreateCredentialIsActiveAndNotQueued`: status `TaskStatus_Active`, kind credential, absent from `QueuedTasks()`, `Tick` assigns nothing.
  - `TestCreateCredentialMasksIssueAndScheduleEdit`: grant with `All` → stored caps lack both masked bits, keep `Spawn`.
  - `TestSetCapsCannotAddMaskedBitsToCredential` (direct and via cascade from a parent).
  - `TestCredentialExpiresThroughDeadlineScan`: `BackgroundDeadlines(expiresAt)` returns the hit with `EndReason_Expired`; after the end callback: `Cancelled`, `EndReason_Expired`.
  - `TestCredentialSurvivesReplay`: write WAL, `ReplayEvents` on a fresh store → `active`, same pubkey/expiry/name, `LastUsedAt` zero.
  - `TestCredentialExpiredDuringDowntimeEndsAtStart` (Review Focus 2).
  - `TestRekeyRevivesRevokedAndExpired` and `TestRekeyReplacesKeyOnLiveCredential`: `CredentialLive` reports the new key, state Live; old key bytes no longer returned.
  - `TestRekeyRefusesNonCredential`.
  - `TestWALEventJSONRoundTripCopiesEveryField` stays green with the new fields.
- [ ] **Step 2:** `make test` scoped `./server/ -run 'Credential|WALEventJSON'` → FAIL.
- [ ] **Step 3:** implement per Interfaces.
- [ ] **Step 4:** scoped → PASS; full `make test` → PASS.
- [ ] **Step 5:** Commit `server: credential task store (active status, expiry, re-key, cap mask)`.

---

### Task 4: Server authentication and liveness

**Files:**
- Create: `cli/credential_auth.go` (no build tag; used by server and client)
- Modify: `server/psk.go` (gate), `server/server.go` (gate wiring ~`:1521`, `OnCancel` wiring `:772`, `RecordCredentialIdentity` wiring ~`:492`), `server/dispatch.go:71` (dispatcher field), `server/task_handler.go` (`RecordClientIdentity` refuses `ClientKind_Credential`; new `RecordCredentialIdentity`), `server/capabilities.go` (`callerCaps`, `scopeSetWith`), `server/pubsub_authz.go` (comment only: credential role falls under the non-Client refusal)
- Test: `server/credential_auth_test.go`, `cli/credential_auth_test.go`

**Interfaces:**
- Consumes: Task 3 `CredentialLive`, `TouchCredential`.
- Produces:
  - `func SignCredentialAuth(key ed25519.PrivateKey, transcript []byte) []byte`, `func VerifyCredentialAuth(pub ed25519.PublicKey, transcript, sig []byte) bool` (domain string per Global Constraints).
  - `pskGate.CredentialFor func(tid protocol.TaskID, now time.Time) ([32]byte, CredentialState, bool)`; wired to `s.tasks.CredentialLive`.
  - Gate step for `AuthRole_Credential`: missing arm → `no_identity`; `!ok` or bad signature → `bad_ticket`; then `Expired` → `expired`, `Revoked` → `revoked`; else accept. Order: look up key, verify, then state (so states are only revealed to the key holder, D5).
  - `AllowsMessage`: credential role admits TaskControl (except a ClientHello) and Telemetry; Pubsub refused.
  - `pskDispatchIdentity` case `AuthRole_Credential` → `d.RecordCredentialIdentity(cid, conn, tid) bool`.
  - `func (h *TaskHandler) RecordCredentialIdentity(cid string, conn ConnHandle, tid protocol.TaskID) protocol.ClientHelloStatus` — sets `clientKinds[cid] = ClientKind_Credential`, `principals[cid] = tid` (refuses a zero `tid` → close), calls `OnConnIdentified`, calls `Tasks.TouchCredential`.
  - `callerCaps`: if the principal's task is a credential and `CredentialLive` state ≠ Live → `Capability_None`. Missing record already → None.
  - `scopeSetWith`: a principal whose record is missing, or a credential not Live → empty set (not `DefaultScope()`).
  - `OnCancel` wiring: for a credential task, `s.dropConnsForPrincipal(id)` (send a wire close first, as `kickConn` does — reuse `kickConn` per cid inside the helper rather than bare `sc.Close()`).

- [ ] **Step 1: Write the failing tests** (`server/credential_auth_test.go`, using the existing in-process server harness the `psk_test.go` / `whoami` tests use):
  - `TestCredentialHandshakeAccepts`; `TestCredentialHandshakeWrongKeyIsBadTicket`; `TestCredentialHandshakeUnknownTaskIsBadTicket`; `TestCredentialHandshakeNonCredentialTaskIsBadTicket`; `TestCredentialHandshakeExpired`; `TestCredentialHandshakeRevoked`.
  - `TestCredentialPrincipalIsNeverZero` (anti-goal): after accept, `lookupPrincipal(cid)` equals the task id; a request needing `schedule_edit` is denied; `callerCaps` ≠ `Capability_All`.
  - `TestCredentialConnRefusesPubsubJoin`.
  - `TestClientHelloClaimingCredentialKindRefused`.
  - `TestCredentialDeadBeforeTimerSeesNothing` (D8): expiry in the past, watcher not running → first request denied, `visibleToCaller` empty.
  - `TestMissingPrincipalRecordSeesNothing` (Review Focus 1).
  - `TestRevokeClosesLiveCredentialConn` (D9).
  - D6 classification: every `ClientKind_Agent` site (`server/psk.go:187,264`, `server/server.go:1731`, `server/task_handler.go:318,334`, `server/taskstore.go:264`, `cli/psk.go`) and every `AuthRole_Client` site (`server/psk.go` `AllowsMessage`/`binderKey`, `server/pubsub_authz.go:21`) gets a one-line verdict in the commit message: "principal task → also credential" or "runner process → agent only". `server/server.go:1731` (conn listing principal) is the former: credential conns show their principal task.
  - `cli/credential_auth_test.go`: sign/verify round trip; flipped transcript byte fails; runner-auth signature over the same transcript does not verify as credential-auth.
- [ ] **Step 2:** scoped `make test` → FAIL.
- [ ] **Step 3:** implement per Interfaces.
- [ ] **Step 4:** scoped → PASS; full `make test` → PASS.
- [ ] **Step 5:** Commit `server: credential handshake, principal and liveness checks`.

---

### Task 5: Server verbs — issue, re-key, refusals, whoami, list

**Files:**
- Create: `server/credential_handler.go`
- Modify: `server/capabilities.go:43` (`requiredCap`: both new kinds → `Capability_CredentialIssue`), `server/task_handler.go` (dispatch the two kinds; resume refusal in the OpenInteractive resume branch `:1577` and `openBackground` `:62`; `toTaskInfo` `:2413` fills the credential fields; whoami `:976` fills `principal_kind` and `credential_expires_at`), `server/schedule_handler.go` (bind/new `--task T` refuses a credential T), `server/set_caps_handler.go` (map the store mask to `masked_cap` when the request asked for a masked bit)
- Test: `server/credential_handler_test.go`

**Interfaces:**
- Consumes: Task 3 store API, Task 2 wire types, `grantFor` / `granter` (`server/task_handler.go:1308`, `server/capabilities.go:364`).
- Produces:
  - `func (h *TaskHandler) handleCredentialIssue(conn ConnHandle, requestID uint32, cid string, req *protocol.CredentialIssueRequest)`: validate `ttl_ms` (0 → `ttl_invalid`; `> CredentialMaxTTL` → `ttl_too_long`), pubkey must be a valid 32-byte Ed25519 point (`bad_pubkey`); requested caps containing a masked bit → `masked_cap`; grant via `h.grantFor(h.granter(cid, h.callerCaps(cid)), <grantRequest from req>, newID)` → `scope_not_permitted` on error; `Tasks.CreateCredential(h.lookupPrincipal(cid), h.lookupClientKind(cid), grant.To, pub, now+ttl, name)` → `spawn_limit` on a limit error; reply `ok` with id and expiry.
  - `func (h *TaskHandler) handleCredentialRekey(...)`: target must be a credential in the caller's `credential_issue` scope (else `no_such_task`); issue-equivalence check: `g := h.granter(cid, callerCaps)`; recorded (or narrowed) caps `c` must satisfy `g.ChildCaps(c) == c` and `g.ChildScope(recordedScope, c)` must succeed without narrowing (compare equal) → else `scope_not_permitted`; then `Tasks.RekeyCredential`; then drop live conns of that principal (old key) via `DropConnsForPrincipal`.
  - Resume refusal message: `"a credential task is re-keyed, not resumed: credential rekey <id>"` (OpenInteractive status: reuse the existing not-resumable status if one exists, else `ResumeNotFound` with this message).

- [ ] **Step 1: Write the failing tests:**
  - `TestCredentialIssueNeedsIssueBit`; `TestCredentialIssueAttenuatesToIssuer` (agent with `ScheduleRun|ResumeExact|CredentialIssue` scoped to T issues `{ScheduleRun, ResumeExact}` → ok; asking `Spawn` it lacks → stored caps exclude it); `TestCredentialIssueTTLBounds` (0, `CredentialMaxTTL + 1ms` refused; `CredentialMaxTTL` ok) (Review Focus 5); `TestCredentialIssueRefusesMaskedCaps`; `TestCredentialIssueBadPubkey`.
  - `TestCredentialRekeyGatedAsIssue`: caller with `Resume` but no `CredentialIssue` → denied; with `CredentialIssue` but narrower caps than the credential → `scope_not_permitted`; operator → ok; old key's live connection closed.
  - `TestResumePathsRefuseCredential` (foreground open, background open, schedule bind of a credential).
  - `TestWhoamiReportsCredential`: `principal_kind == ClientKind_Credential`, expiry set.
  - `TestListCarriesCredentialFields`.
  - End-to-end over the in-process server: issue `{ScheduleRun, ResumeExact}` scoped to T, connect with the key, `schedule run` of a schedule bound to T → fired; `schedule show` history entry `resumed_by` kind `credential`.
- [ ] **Step 2:** scoped `make test` → FAIL.
- [ ] **Step 3:** implement.
- [ ] **Step 4:** scoped → PASS; full `make test` → PASS.
- [ ] **Step 5:** Commit `server: credential issue / rekey verbs and refusals`.

---

### Task 6: Agentboard for credential connections (D10)

**Files:**
- Modify: `server/agent_handler.go` (new `establishCredentialBoard`), `server/task_handler.go` (`RecordCredentialIdentity` calls it through a new `OnCredentialHello` hook, wired in `server/server.go` beside `OnAgentHello` `:351`), `server/server.go` (`OnCancel` wiring: `Board.Revoke(zero, tid)` for a credential), `server/credential_handler.go` (re-key also revokes the board state), the board sender rendering (where a retained message's sender is formatted for `agent read` / `board read` — find it from `from_task` rendering in `cli/`), the wake path (skip for the zero runner identity)
- Test: `server/credential_board_test.go`

**Interfaces:**
- Consumes: Task 4 `RecordCredentialIdentity`.
- Produces: `func (s *Server) establishCredentialBoard(conn ConnHandle, tid protocol.TaskID)` — `ac := s.getOrCreateAgentConn(conn); ac.helloed = true; ac.state = s.Board.Attach(protocol.RunnerID{}, tid, "", "")`. No ticket is registered and `boardRegisterTask`'s zero-runner guard is not touched.

- [ ] **Step 1: Write the failing tests:**
  - `TestCredentialBoardSendWithinPeerScope`: credential with `board_peer` scoped to agent A sends to `chat.<A8>` → ok; to `chat.<B8>` → refused as an agent would be.
  - `TestCredentialBoardReadAndWait`: a message published to the credential's own `chat.<id8>` is returned by read and unblocks a wait.
  - `TestCredentialBoardPublishWakesNothing`: publishing to a credential's inbox makes no runner call and logs no error.
  - `TestCredentialBoardStateDroppedOnRevokeAndRekey`.
  - `TestCredentialSenderRendersAsCredential`: a message sent by a credential named `ci` renders sender `credential ci` (id prefix when unnamed).
- [ ] **Step 2:** scoped `make test` → FAIL.
- [ ] **Step 3:** implement.
- [ ] **Step 4:** scoped → PASS; full `make test` → PASS.
- [ ] **Step 5:** Commit `server: agentboard for credential connections (no wake)`.

---

### Task 7: Client — handshake, CLI verbs, rendering

**Files:**
- Create: `cli/credential.go` (client methods + `…With` / `…Out` forms + renderers), `cli/credential_source.go` (`//go:build !js`; loading the credential), `cli/verb/credential_test.go`
- Modify: `cli/client.go` (`dialOn` ~`:135`: credential branch), `cli/psk.go` (`SendCredentialHandshake`), `cli/persist.go:127` (`Retryable`: `revoked` stays fatal — no change in expression, add the comment and a test), `cmd/harness-cli/main.go:34/93` (global `--credential-file`), `cli/verb/table.go` (new `// --- credentials (docs/superpowers/specs/2026-11-21-credential-tasks-design.md) ---` block), `cmd/harness-cli/dispatch.go` (handlers), `cli/whoami.go:91` (render `kind=credential expires=…`), `cli/list.go` (`taskKindStr` `:1082` gains `credential`; `taskLine` `:276` appends `  expires=<RFC3339>  name=<n>  last_used=<…|unknown>` after the `schedule=` pattern `:310-316`; `--json` fields `credential_expires_at`, `credential_last_used_at`, `credential_name`), `cmd/harness-cli` usage text
- Test: `cli/credential_test.go`, `cmd/harness-cli/credential_cli_test.go`

**Interfaces:**
- Consumes: Task 2 wire types, Task 4 `SignCredentialAuth`.
- Produces:
  - `type Credential struct { TaskID protocol.TaskID; Key ed25519.PrivateKey }`; `func ParseCredential(s string) (Credential, error)`; `func (c Credential) String() string`; `func LoadCredential(flagPath string) (*Credential, error)` (sources and order per Global Constraints; nil,nil when none; a file readable by group/other → error naming the mode).
  - `func SendCredentialHandshake(ctx context.Context, sendFn func([]byte) error, transcript []byte, cred Credential, serverKey ed25519.PublicKey, respCh <-chan PskAuthReply) error` (model: `runner/connect.go:617-657`).
  - `dialOn`: `if cred != nil` → `SendCredentialHandshake`, `Client.kind = ClientKind_Credential`; otherwise unchanged.
  - Verb table rows (`CmdlineSurfaces: CLI|TUI|WebUI` except keygen = CLI only, with a `TestSurfaceNarrowingHasAReason` note "keygen writes a private key file on the host that will use it; TUI/WebUI run elsewhere"):
    - `credential keygen --out FILE` → prints the public key hex.
    - `credential new --ttl DUR [--name N] [--caps …] [--scope …] [--out FILE | --pubkey HEX]` (`Exclusive` out|pubkey; caps/scope flags reuse the spawn flag declarations).
    - `credential rekey <id> --ttl DUR [--caps …] [--out FILE | --pubkey HEX]`.
    - `credential ls [--json]` (filters `ls` rows by kind credential, in reach).
    - `credential revoke <id>` (sends the existing Cancel).
    - DUR accepts Go durations plus a `d` suffix (`30d`).
  - D3a: when `EffectiveClientKind(ClientKind_Cli)` is Agent and neither `--out` nor `--pubkey` is given → error `"an agent identity may not print a private key: use --out FILE or --pubkey HEX"`.
  - Output: issue/rekey print `credential <id8>: issued, expires <RFC3339>` plus either `secret: <credential string>` (print case) or `secret written to <path>`.

- [ ] **Step 1: Write the failing tests:** `TestParseCredentialRoundTrip`, `TestParseCredentialRejects` (wrong lengths, missing colon, non-hex); `TestLoadCredentialOrder` (flag > file env > value env); `TestLoadCredentialRefusesGroupReadableFile`; `TestCredentialBeatsOperatorPSK` and `TestCredentialBeatsAgentEnv` (Review Focus 3, 4: dial against the in-process server, whoami shows the credential task); `TestRevokedIsFatal`; `TestDurationWithDaySuffix` (`30d`, `90d`, `91d` parses but server refuses); `TestAgentIdentityRefusesPrintedSeed`; verb-table suite stays green (`TestEveryReachableVerbHasANote`, `TestEveryOperatorListingTakesJSON`, `TestExamplesParse`, `TestUsageDescribesEveryDeclaredCliVerb`, `TestEverySurfaceReadsEveryActionField` — add the credential actions to `verbConsumers`).
- [ ] **Step 2:** scoped `make test` → FAIL.
- [ ] **Step 3:** implement; `go generate ./cli/verb/` for `actions_gen.go`.
- [ ] **Step 4:** scoped → PASS; full `make test` → PASS.
- [ ] **Step 4b:** the `agent` verbs (`send`, `read`, `wait`, `subscribe`) work under a credential identity: wherever they require an agent context (`EffectiveClientKind` == Agent / `HARNESS_TASK_ID`), accept a loaded credential too. Test `TestAgentVerbsRunUnderCredential` (send to a peer, read own inbox) against the in-process server.
- [ ] **Step 5:** run once against `scripts/dummy-harness.sh` in the exact spelling the usage prints (pitfall 13): keygen → new --pubkey → `HARNESS_CREDENTIAL_FILE=… harness-cli whoami` → `agent send` to a live agent's chat topic → `credential revoke` → whoami refused `revoked`. Commit `cli: credential handshake and credential verbs`.

---

### Task 8: TUI and WebUI surfaces

**Files:**
- Modify: `tui/dispatch.go` (credential handlers via `DoRecord(a.client, …)` — the long-lived client, never a fresh dial), `tui/detail.go:389` + `:545` (kind label; expires / last used / name lines), `tui/tasks.go` (no new column: status column shows `active`; `About` shows `credential <name> expires <date>`), `tui/cmdline_help` text
- Create: `cmd/harness-webui-wasm/credential.go` (exports `credentialNew`, `credentialRekey`, `credentialList`, `credentialRevoke`, all via `currentClient()` and the `…With` forms; generation of a key pair in wasm for the generate path)
- Modify: `cmd/harness-webui-wasm/main.go:95` (register exports), `:1753`/`:1891` (snapshot fields `credentialExpiresAt`, `credentialLastUsedAt`, `credentialName`), `webui/static/cmdline.js` (`case "credential":` like `schedule` `:324-342`), `webui/index.html` (a `credentials-modal` dialog + header button, like `#schedules-modal` `:739`), `webui/static/main.js` (dialog: list with revoke/rekey buttons; issue form with a pubkey field OR "generate" → result shown once with copy and a download through `downloadText(name, "text/plain", credentialString)` `:8578`; nothing stored in `localStorage`), `webui/static/task_models.js` (`credentialRowModel`), `renderTaskList` meta `:6867` (`expires=… name=…` like the schedule append `:6873`)
- Test: `webui/static/credential_test.mjs`, `tui/credential_test.go`, `cli/verb` modal-surface entries (`ModalSurfaces: {WebUI, "webui/index.html#credentials-modal"}`; TUI: `NoModalSurface: "looked for: a TUI credentials table; issue/rekey/revoke are by the command line, rows appear in the task table"`)

**Interfaces:**
- Consumes: Task 7 `cli` credential methods and verb rows.

- [ ] **Step 1: Write the failing tests:** `credential_test.mjs` — `credentialRowModel` renders expiry/name/last-used (`unknown` for 0); the generate path's result string matches `/^[0-9a-f]{32}:[0-9a-f]{64}$/` and is not written to `localStorage`; `cmd_test.mjs` gains `credential ls`. `tui/credential_test.go` — detail popup shows `kind: credential` and `expires:`; `credential revoke <id>` from the cmdline goes through `a.client`.
- [ ] **Step 2:** run `make test` (includes the node tests) → FAIL.
- [ ] **Step 3:** implement.
- [ ] **Step 4:** `make test` → PASS.
- [ ] **Step 5:** live check every surface (memory: Playwright does not cover the TUI): WebUI via Playwright against the dummy harness — issue with generate, see it once, download, revoke; TUI in a dummy PTY — cmdline issue with `--pubkey`, detail popup, revoke. Keep the screenshots and report their paths. Commit `tui, webui: credential surfaces`.

---

### Task 9: Docs, parity walk, skew check

**Files:**
- Modify: `README.md` (credential section; per-binary summaries; TUI cmdline verb list), `.claude/skills/harness-cli-from-a-tool/SKILL.md` §7–§8 + `runner/agentskills` source and mirrors (a tool may run under a credential instead of an inherited ticket; `revoked` row in the rejection table: "the credential was revoked: re-key or re-issue; a restart will not help"), `.claude/skills/surface-parity-checklist/firing-log.md`, the spec (Status: implemented, with the surface matrix as shipped)

- [ ] **Step 1:** walk `surface-parity-checklist` items 1–39 for the new kind, fields and verb family, writing a verdict per number into `firing-log.md`.
- [ ] **Step 2:** `scripts/wire-skew-check.sh` → PASS (new runner × old server recovers).
- [ ] **Step 3:** dummy-harness E2E from the spec's Testing section: issue `schedule_run,resume_exact` scoped to T from the CLI, fire a schedule bound to T from a shell holding only `HARNESS_CREDENTIAL`, `schedule show` attributes `resumed_by` to a credential; revoke; next call refused `revoked`; rekey; works again with the new key, old key refused.
- [ ] **Step 4:** `make test` → PASS. Commit `docs: credential tasks (README, skills, parity walk)`. Report ready to land (do not land).
