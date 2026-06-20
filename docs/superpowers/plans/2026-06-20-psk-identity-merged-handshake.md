# Merged PSK + identity handshake Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: superpowers:subagent-driven-development. Steps use `- [ ]`.

**Goal:** Make every connection establish its principal at the PSK gate via a single brgen-schematized `PskAuthRequest` (binder + identity union), in one round-trip — closing the task-control fail-open-to-operator hole and schematizing the PSK wire format.

**Architecture:** First message becomes `[0x45] + PskAuthRequest{binder, role, ClientHello|RunnerHello}`. The server `pskGate` verifies the binder (unchanged HMAC over the ECDH transcript), requires identity (fail-closed), validates the agent ticket, then re-dispatches the embedded hello to the existing handler. Clients/runner send this instead of separate PSK + hello messages.

**Tech Stack:** Go, brgen (`.bgn` → `runner/protocol/message.go`), objproto/peer transport, server/psk + handlers, cli, runner.

**Spec:** `docs/superpowers/specs/2026-06-20-psk-identity-merged-handshake-design.md`

## Global Constraints

- **ATOMIC wire bump — not incrementally landable.** Server, every CLI (incl. the harness-cli bridged into runner-host sandboxes), TUI, WebUI (wasm), and the runner all speak the new first message. Between Task 2 and Task 5 the tree is internally inconsistent; **do not land until ALL tasks are done, the whole tree builds (`make check`+`wasm-check`), and tests pass.** Deploy server+runner+all CLIs together.
- **Binder crypto unchanged:** `ComputePSKBinder(psk, transcript)` (HMAC-SHA512 over the objproto transcript) stays byte-identical. Do NOT change what the binder covers (transcript binding must be preserved).
- **Processing order:** decode → verify binder → (only then) validate ticket / record identity / register runner. No identity action before binder verification.
- **Fail-closed:** a `PskAuthRequest` with no identity union → reject (`no_identity` + close). Identity is mandatory in BOTH PSK and no-PSK (binder_len 0) modes.
- **Generated code not hand-edited** (`message.go`); `.bgn` + `make protoregen`. Build hygiene: `make check`/`wasm-check` + focused `go test`. NEVER bare `go build ./cmd/<x>/`. Commit only intended files (no `git add -A`; ignore pre-existing untracked noise).
- Single-user dogfood: no migration shim; old↔new mismatch fails loud (desired).

## File Structure
- `runner/protocol/message.bgn` → `AuthRole`, `PskAuthStatus`, `PskAuthRequest`, `PskAuthResponse` (regenerates message.go).
- `appwire/psk.go` → remove the hand-written `PskAuthStatus` (now in protocol); keep `AppKind_PskAuth`.
- `cli/psk.go`, `cli/psk_binder.go` → keep `ComputePSKBinder`; replace `SendAndWaitPSK` with a merged builder that also carries identity.
- `cli/client.go` → `Dial(ctx, peerCID, kind)`; build+send `PskAuthRequest`, await `PskAuthResponse`; drop the separate hello.
- `cli/hello.go` → fold `SayHelloAuto`'s ClientHello construction into the merged builder.
- `server/psk.go` + `server/server.go` → gate decodes/verifies/requires/validates, re-dispatches identity; no-PSK still requires the handshake.
- `cli/agent/conn.go` → merged request (role=client, agent).
- `runner/connect.go` → merged request (role=runner).
- callers of `cli.Dial` (cli/*, cmd/harness-cli/*, cmd/harness-tui, cmd/harness-webui-wasm) → pass kind.

---

## Task 1: schema — PskAuthRequest/Response/AuthRole/PskAuthStatus

**Files:** Modify `runner/protocol/message.bgn`; regenerate `message.go`. Test: `runner/protocol/psk_handshake_test.go` (new).

**Interfaces:** Produces `protocol.AuthRole{Client,Runner}`, `protocol.PskAuthStatus{Ok,BadPsk,BadTicket,NoIdentity}`, `protocol.PskAuthRequest{BinderLen,Binder,Role, ClientHello()/RunnerHello() union accessors}`, `protocol.PskAuthResponse{Status}`.

- [ ] **Step 1:** Add to `runner/protocol/message.bgn` (near ClientHello/RunnerHello):
```
enum AuthRole:
    :u8
    client = 0
    runner = 1

enum PskAuthStatus:
    :u8
    ok          = 0
    bad_psk     = 1
    bad_ticket  = 2
    no_identity = 3

format PskAuthRequest:
    binder_len :u16
    binder :[binder_len]u8
    role :AuthRole
    role == AuthRole.client => client_hello :ClientHello
    role == AuthRole.runner => runner_hello :RunnerHello

format PskAuthResponse:
    status :PskAuthStatus
```
- [ ] **Step 2:** `make protoregen ARGS='runner/protocol/message.bgn'`. (BLOCKED on env failure; do not hand-edit message.go.)
- [ ] **Step 3:** Round-trip test (new `runner/protocol/psk_handshake_test.go`): encode a client-role request (binder 64 bytes + a ClientHello kind=agent) → decode → assert binder + role + the ClientHello fields survive; a runner-role request (binder + RunnerHello) round-trips; binder_len 0 (no-PSK) round-trips; `PskAuthResponse{status}` round-trips. Mirror existing `runner/protocol/*_test.go` encode/decode helpers.
- [ ] **Step 4:** `go test ./runner/protocol/ -run PskAuth -v && make check` → PASS (existing `appwire.PskAuthStatus` still compiles — not removed until Task 6).
- [ ] **Step 5:** Commit. `git add runner/protocol/message.bgn runner/protocol/message.go runner/protocol/psk_handshake_test.go && git commit -m "feat(protocol): schematize PSK handshake (PskAuthRequest/Response with identity union)"`

---

## Task 2: server gate — verify binder + require/validate/record identity

**Files:** Modify `server/psk.go` (`pskGate`), `server/server.go` (handleConnection wiring). Test: `server/psk_test.go` (extend).

**Interfaces:** Consumes Task 1 types, `ComputePSKBinder`, `Registry.Validate`, the existing dispatch (`s.dispatcher.Dispatch`) + the ClientHello/RunnerHello handlers.

- [ ] **Step 1: Write gate tests** (`server/psk_test.go`): build a `[0x45]+PskAuthRequest` and drive the gate. Cases: valid binder + client ClientHello(operator kind) → `ok`, authed, identity dispatched; valid binder + agent ClientHello + valid ticket → ok + principal set; valid binder + agent + BAD ticket → `bad_ticket` + close; bad binder → `bad_psk` + close; binder ok + no identity union → `no_identity` + close; runner role + valid → ok + registered. (Use a fake/registered ticket via `board.Registry().Register` and a stub dispatcher to observe re-dispatch.)
- [ ] **Step 2:** Run → FAIL.
- [ ] **Step 3: Rewrite `pskGate.Check`** to: decode `PskAuthRequest` from `data[1:]`; if `g.psk != nil` recompute binder over `transcript` and constant-time compare `req.Binder` → mismatch `bad_psk`+close; require a present identity union → else `no_identity`+close; if role=client && ClientHello.Kind==Agent → `Registry.Validate(...)` → invalid `bad_ticket`+close; on accept send `[0x45]+PskAuthResponse{ok}`, set `authed`, and RETURN the embedded hello bytes (re-encoded as the normal `[appkind]+hello` message) so handleConnection dispatches it. Keep `ComputePSKBinder` usage identical. (Adjust the `Check` signature/return to surface the to-redispatch bytes; or expose a helper the caller invokes.)
- [ ] **Step 4: handleConnection wiring** (`server/server.go:769`): when no PSK is configured, the gate must STILL require the first message to be a `PskAuthRequest` (identity mandatory) — set `newPSKGate` so the no-PSK path expects the handshake (binder skipped) rather than `authed=true`-from-start. On gate accept, dispatch the returned embedded hello via `s.dispatcher.Dispatch(wrapped, helloBytes)` (and the gate already sent the PskAuthResponse).
- [ ] **Step 5:** Run gate tests + `make check` → PASS. (Other components still send the OLD handshake → server-only tests pass, but the live system is inconsistent until Tasks 3-5; that's expected per the atomic-bump constraint.)
- [ ] **Step 6:** Commit. `git add server/psk.go server/server.go server/psk_test.go && git commit -m "feat(server): PSK gate decodes merged handshake; require+validate+record identity (fail-closed)"`

---

## Task 3: cli task-control — Dial(kind) sends the merged handshake

**Files:** Modify `cli/client.go` (`Dial`), `cli/psk.go` (merged builder), `cli/hello.go` (reuse ClientHello construction). Update all `cli.Dial` callers. Test: `cli/psk_test.go`.

**Interfaces:** Produces `Dial(ctx, peerCID, kind protocol.ClientKind)`. Consumes Task 1 types.

- [ ] **Step 1:** Add a merged builder in `cli/psk.go` (or client.go): given `psk, transcript, kind`, build `PskAuthRequest{ binder: ComputePSKBinder(psk,transcript) or empty if psk==nil, role: client, client_hello: <ClientHello: kind=agent+AgentInfo when inAgentContext else kind> }`; send `[0x45]+encode`; await one `[0x45]+PskAuthResponse`; error unless `status==ok`. Reuse `SayHelloAuto`'s agent-detection (`cliopts.Resolve*`) for the ClientHello.
- [ ] **Step 2:** Change `Dial(ctx, peerCID)` → `Dial(ctx, peerCID, kind protocol.ClientKind)`; after the objproto handshake, call the merged builder with `kind`; remove the old `SendAndWaitPSK`-only path. 
- [ ] **Step 3:** Update every `cli.Dial` caller to pass kind: `cli/*.go` (cancel/list/prune/logs/get_log/notify/notify_watch/watch/submit/open_interactive_native/server_dial_runner) → `protocol.ClientKind_Cli`; `cmd/harness-cli/*` → `Cli`; `cmd/harness-tui/main.go` → `Tui`; `cmd/harness-webui-wasm/main.go` → `Webui`. Remove the now-redundant explicit `SayHelloAuto`/`SayHello` calls in the 6 task-control commands + TUI/WebUI (identity now rides Dial). (`make check` lists every caller.)
- [ ] **Step 4:** Test (`cli/psk_test.go`): the merged builder produces a well-formed `[0x45]+PskAuthRequest` with the binder and a ClientHello; agent-env → kind=agent + AgentInfo; operator → the passed kind, no AgentInfo. (Decode the built bytes with the protocol types.)
- [ ] **Step 5:** `go test ./cli/ ./cmd/harness-cli/ -run 'Psk|Hello|Caps' -v && make check && make wasm-check` → PASS.
- [ ] **Step 6:** Commit. `git add cli/ cmd/harness-cli/ cmd/harness-tui/ cmd/harness-webui-wasm/ && git commit -m "feat(cli): Dial(kind) sends merged PSK+identity handshake; drop per-command hello"`

---

## Task 4: cli agentboard — merged handshake

**Files:** Modify `cli/agent/conn.go` (its PSK + AgentBridge/ClientHello sequence). Test: existing `cli/agent/*_e2e_test.go` setup.

**Interfaces:** Consumes the merged builder / Task 1 types.

- [ ] **Step 1:** Replace the agentboard connection's PSK + hello with the same merged `PskAuthRequest{ role=client, client_hello kind=agent }` send + `PskAuthResponse` await. Reuse the Task 3 builder (export it from `cli` if needed).
- [ ] **Step 2:** Update the agentboard E2E test harness (`startServerE2E` path already injects tasks for caps; ensure the merged handshake is what the test client sends). `go test ./cli/agent/ -count=1` → PASS.
- [ ] **Step 3:** `make check && make wasm-check` → PASS.
- [ ] **Step 4:** Commit. `git add cli/agent/ && git commit -m "feat(cli/agent): agentboard uses merged PSK+identity handshake"`

---

## Task 5: runner — merged handshake (role=runner)

**Files:** Modify `runner/connect.go`. Test: existing runner connect tests.

**Interfaces:** Consumes Task 1 types + `ComputePSKBinder`.

- [ ] **Step 1:** In `runner/connect.go`, replace `cli.SendAndWaitPSK(...)` + the separate `RunnerHello` send (`:212`, `:239`) with one merged `PskAuthRequest{ binder, role=runner, runner_hello: RunnerHello{...} }` send + `PskAuthResponse` await; on `ok` proceed (the server registers via the re-dispatched RunnerHello, replies RunnerHelloResponse as today).
- [ ] **Step 2:** Update runner connect tests to the merged handshake. `go test ./runner/ -count=1` → PASS.
- [ ] **Step 3:** `make check` → PASS.
- [ ] **Step 4:** Commit. `git add runner/ && git commit -m "feat(runner): connect via merged PSK+identity handshake (role=runner)"`

---

## Task 6: cleanup + whole-tree verify + E2E

**Files:** Remove `appwire/psk.go`'s `PskAuthStatus` (migrate refs to `protocol.PskAuthStatus`); remove dead `SendAndWaitPSK`/old hello helpers if fully unused. Test: whole suite + live E2E.

- [ ] **Step 1:** Migrate any remaining `appwire.PskAuthStatus` references to `protocol.PskAuthStatus`; delete the hand-written enum from `appwire/psk.go` (keep the file only if it has other content, else `git rm`). Remove `SendAndWaitPSK` / per-command `SayHelloAuto` if now unused (grep to confirm zero refs before deleting).
- [ ] **Step 2:** `make check && make wasm-check && go test ./... -skip 'TestHandleOpenPortForward_RemoteRegisters'` → all PASS (the whole tree now consistently speaks the new handshake).
- [ ] **Step 3: Live E2E** (the original goal — after the user rebuilds+restarts server+runner and the runner-host harness-cli): spawn a `--caps none` sandbox task; assert `harness-cli ls` from it shows only its subtree (NOT all tasks), and `submit`/`cancel`/`file pull` return permission-denied. (This is the regression the probe surfaced; document the result.)
- [ ] **Step 4:** Commit. `git add -A -- appwire/ cli/ && git commit -m "chore: remove pre-merge PSK/hello helpers; PskAuthStatus now schema-defined"` (verify nothing unintended staged).

---

## Self-Review
**Spec coverage:** schema (T1); gate verify+require+validate+record (T2); cli task-control Dial(kind)+merged (T3); agentboard (T4); runner (T5); cleanup+E2E (T6). Fail-closed + no-PSK-still-requires-identity (T2 step 4). Binder unchanged / transcript binding (T2 step 3 keeps ComputePSKBinder). Atomic-bump landing (Global Constraints).
**Type consistency:** `PskAuthRequest`/`PskAuthResponse`/`AuthRole`/`PskAuthStatus`, `Dial(ctx,peerCID,kind)`, merged builder — consistent across tasks.
**Placeholder scan:** gate Check signature/return for the re-dispatch is described ("return the embedded hello bytes / expose a helper") — the implementer picks the exact shape; no TBD values. The agentboard builder export is noted.
