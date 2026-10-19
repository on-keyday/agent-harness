# Resume caps split + detached seat — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Split resume out of `spawn` into `resume` / `resume_exact`, add exact (recorded-settings) resume, and make the control seat of an opened session follow `exec_control` through a `detached` open.

**Architecture:** One schema change in `runner/protocol/message.bgn` (regenerated with `make protoregen`). The server moves `submit` / `open_interactive` out of the `requiredCap` map into inline gates (bit → seat → target), the way `AttachSession` is gated. Clients gain `ResumeExact` / `Detached` on `cli.SessionOpts` and an `OpenDetached` call that replaces every open-then-drop path.

**Tech Stack:** Go, `.bgn` schema → Go via ebm2go (`make protoregen`), objtrsf trsf streams, bubbletea TUI, Go→wasm WebUI bridge + `webui/static/main.js`.

**Spec:** `docs/superpowers/specs/2026-10-19-resume-caps-design.md` — read § Problem (P1–P4) and § Decisions taken before any task. Every Dn below refers to that table.

## Global Constraints

- Bit values: `resume = 0x100000`, `resume_exact = 0x200000`, `all = 0x3fffff` (D5, D9).
- `spawn` authorizes creating only; it implies neither resume bit. `resume` implies `resume_exact` (D20).
- Resume target rule: allowed iff some HELD bit among `resumeRequestBits(exact)` has the target in its own scope (D19).
- A scope override whose mask contains `spawn` is refused (D21).
- No migration of stored caps or scopes (D22, D17).
- Under exact, the request carries no settings; anything else → `resume_exact_mismatch`, never ignored (D10).
- An attached open (`detached = 0`) requires `exec_control` over the target, operators included (D7).
- `stream_id` of `OpenInteractiveResponse` is present only when `attached == 1`; 0 is a valid stream id and never a sentinel there.
- Work in THIS worktree (`/home/kforfk/workspace/remote-agent-harness/.harness-worktrees/70fbad4a6eb6f1e992be8a669f1bcefd`), on branch `harness/70fbad4a6eb6f1e992be8a669f1bcefd`. Verify with `git rev-parse --abbrev-ref HEAD` before the first edit. Do not use absolute paths under the parent checkout.
- Read `.claude/skills/implementation-pitfalls/SKILL.md` in full before the first edit.
- Compile-check with `make check` / `go build ./...`; never a bare `go build ./cmd/<x>/` (it drops a binary in the worktree).
- Every commit ends with `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

1. **An operator's attached `session new` / TUI `s` / WebUI "open detachable"** must keep working: operator connections have no principal and get `Capability_All` + global scope, so the seat check must pass for them. Test in Task 4.
2. **An exact resume whose recorded profile no runner advertises any more** must answer `profile_unavailable`, not fall back to a runner default. Test in Task 3.
3. **A `--rows`/`--cols` pair where only one is non-zero** on a detached open must send no size (same rule as `applyInitialWindowSize`). Test in Task 4.
4. **A resume_exact holder whose target is outside both scopes** must get `resume_not_found`, not `PermissionDenied` (existence oracle). Test in Task 2.
5. **`submit --resume X --exact` with no prompt** must pass the CLI's own validation (today `submit` refuses an empty prompt). Test in Task 5.

---

### Task 1: Schema

**Files:**
- Modify: `runner/protocol/message.bgn` (enum `Capability`; formats `SubmitRequest`, `OpenInteractiveRequest`, `OpenInteractiveResponse`; enums `SubmitStatus`, `OpenInteractiveStatus`)
- Regenerate: `runner/protocol/message.go` via `make protoregen`
- Modify (compile fix-ups only): `server/task_handler.go` (where `OpenInteractiveResponse.StreamId` is set), `cli/open_interactive_native.go`, `cli/open_interactive_wasm.go` (where it is read)
- Test: `runner/protocol/` round-trip test file beside `TestSubmitRequestResumeRoundTrip` (`server/resume_test.go:131` is the existing pattern)

**Interfaces:**
- Produces: `protocol.Capability_Resume`, `protocol.Capability_ResumeExact`; `SubmitRequest.ResumeExact()/SetResumeExact`; `OpenInteractiveRequest.ResumeExact()/SetResumeExact`, `.Detached()/SetDetached`, the conditional `initial_rows`/`initial_cols` accessors (same generated shape as the `x11` block); `OpenInteractiveResponse.Attached()/SetAttached` and the conditional `stream_id` accessor; `SubmitStatus_ResumeExactMismatch`, `OpenInteractiveStatus_ResumeExactMismatch`, `OpenInteractiveStatus_DetachRequired`.

- [ ] **Step 1: Edit the schema** exactly as spec § Wire lists it, including the new comment on `spawn`. Also fix the stale `Capability.all (0xfff)` in the `SubmitRequest.requested_caps` comment to `0x3fffff`.

- [ ] **Step 2: Regenerate**

Run: `make protoregen`
Expected: `runner/protocol/message.go` changes; the diff is large (constant of ebm2go, accept it).

- [ ] **Step 3: Write the round-trip tests**

`TestOpenInteractiveRequestDetachedRoundTrip`: set `detached=1`, `initial_rows=40`, `initial_cols=150`, `resume_exact=1`; encode → decode; all four read back. With `detached=0` the encoded length is exactly 4 bytes shorter than with `detached=1`.
`TestOpenInteractiveResponseAttachedStreamZero`: `status=ok, attached=1, stream_id=0` round-trips with `Attached()==true` and stream id 0; `attached=0` encodes no stream id (length 8 shorter).

- [ ] **Step 4: Compile fix-ups.** Server: wherever an OK `OpenInteractiveResponse` is built, `SetAttached(true)` and set the stream id through the conditional setter (behaviour unchanged until Task 4). Clients: read the stream id through the conditional accessor; if `!Attached()` on an OK status, return an error `"server opened the session detached; no stream"` (Task 5 adds the real detached path).

- [ ] **Step 5: Verify**

Run: `make check && go test ./runner/... ./cli/... -count=1`
Expected: build PASS, tests PASS.
Run: `go test ./server/ -count=1`
Expected: exactly `TestEveryCapabilityDeclaresHowItsTargetIsResolved` fails, naming `resume` and `resume_exact` (they joined `Capability_All`). Task 2 turns it green. A `cli/verb` test asserting that `GrantableCaps` covers every bit of `Capability_All` may fail the same way and is also Task 2's. Any other failure is a fix-up you missed.

- [ ] **Step 6: Commit** — `protocol: resume / resume_exact bits, exact + detached open flags, attached bit on the open response`

---

### Task 2: The capability gate — split, target rule, catalog

**Files:**
- Modify: `server/capabilities.go` (`requiredCap`, new helpers), `server/task_handler.go` (Submit / OpenInteractive dispatch cases; the two resume target gates at `:1148` and `:1392`), `server/scope.go` (`validateScope`)
- Modify: `server/cap_completeness_test.go` (`kindCapClass`), `server/scope_percap_completeness_test.go` (`capTargetClasses`)
- Modify: `cli/verb/caps.go` (`GrantableCaps`, `CapDescription`)
- Create: `server/resume_caps_test.go`

**Interfaces:**
- Consumes: Task 1's constants and `ResumeExact()` accessors.
- Produces (in `server/capabilities.go`):
  - `func resumeRequestCap(resumeID protocol.TaskID, exact bool) protocol.Capability` — `Spawn` when `resumeID` is zero; `Resume` for a plain resume; `Resume|ResumeExact` for exact.
  - `func resumeRequestBits(exact bool) []protocol.Capability` — `{Resume}` or `{Resume, ResumeExact}`.
  - `func (h *TaskHandler) resumeTargetAllowed(cid string, exact bool, targetHex string) bool` — for each bit in `resumeRequestBits(exact)` the caller holds, `h.inScope(cid, b, targetHex)`; true on the first hit.

- [ ] **Step 1: Write the failing tests** in `server/resume_caps_test.go`, dispatching through `h.Handle(conn, encodeTaskControlRequest(t, req))` with a principal in `h.principals` (fixture: `server/attach_mode_caps_test.go:97-125`) and a registered runner (fixture: `TestSpawnAttenuation`, `server/capabilities_test.go:92`):

```go
// TestResumeGateMatrix: {fresh, resume, exact} × held {none, spawn, resume_exact, resume}
// fresh:  allowed only for spawn; else PermissionDenied with RequiredCap == Capability_Spawn
// resume: allowed only for resume; spawn-only → PermissionDenied, RequiredCap == Capability_Resume
// exact:  allowed for resume and resume_exact; else RequiredCap == Capability_Resume|Capability_ResumeExact
// "allowed" = response kind is Submit / OpenInteractive (any status), not PermissionDenied.
// Use detached=1 on open requests so Task 4's seat check cannot interfere later.

// TestResumeTargetRuleD19:
//   resume-only, --scope-for resume=ids:A (base subtree with child C) → resumes A; C → resume_not_found
//   resume_exact-only, --scope-for resume_exact=ids:X → exact X ok; exact other → resume_not_found
//   resume(ids:A) + resume_exact(ids:B) → exact A ok, exact B ok, plain B → resume_not_found
// TestResumeOutOfBothScopesIsNotFound (Review Focus 4): resume_exact holder, target outside → resume_not_found, never PermissionDenied
// TestScopeOverrideNamingSpawnRefused: validateScope(Scope{Overrides: [{Caps: Spawn|FileRead, ...}]}) returns an error containing "spawn names no task"
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test ./server/ -run 'TestResumeGateMatrix|TestResumeTargetRuleD19|TestResumeOutOfBothScopesIsNotFound|TestScopeOverrideNamingSpawnRefused' -count=1`
Expected: FAIL (spawn-only resumes are still allowed; resume bits deny).

- [ ] **Step 3: Implement.**
  - Remove `Submit` and `OpenInteractive` from `requiredCap`, and rewrite its doc comment's list of absences.
  - In both dispatch cases, deny with `h.denyTaskControl(conn, req.Kind, req.RequestId, want)` when `!hasAnyCap(h.callerCaps(cid), want)`, where `want = resumeRequestCap(req.ResumeTaskId, req.ResumeExact())`.
  - Replace the two `inScope(cid, protocol.Capability_Spawn, idHex)` target gates with `h.resumeTargetAllowed(cid, exact, idHex)`.
  - In `validateScope`, return `fmt.Errorf("scope override names spawn: spawn names no task; narrow resume / resume_exact instead")` for any override mask containing `Capability_Spawn`.
  - Classifications:
    - `kindCapClass`: `Submit` and `OpenInteractive` become `capInHandler`, with a comment saying "the bit depends on resume_task_id and resume_exact".
    - `capTargetClasses`: `Spawn` becomes `capNoTargetResolution` with the reason "creating a task names no existing one". `Resume` and `ResumeExact` become `{kind: capResolvedVia, via: "resumeRequestBits"}`, so the `inScope` call inside `resumeTargetAllowed` must loop over `resumeRequestBits(...)` on the same line the scan reads.
  - Catalog: insert `Capability_ResumeExact, Capability_Resume` right after `Capability_Spawn` in `GrantableCaps`, with a comment noting they are weakest-first within their rank and independent of spawn. `CapDescription` texts, copied verbatim:
    - spawn: `"create tasks (submit, open sessions); does NOT resume — see resume. Opening a session without -d also needs exec_control"`
    - resume: `"resume a finished task under its own id, free to change prompt, args, agent, kind, caps and scope; creates no new task; keeps the task's recorded caps unless --caps re-grants them; implies resume_exact"`
    - resume_exact: `"resume a finished task exactly as recorded (--resume <id> --exact): only the runner, --resume-conversation, x11, -d and the initial size may be chosen"`
    - exec_control: append `"; opening a session without -d needs it"` to the existing text.

- [ ] **Step 4: Verify**

Run: `go test ./server/ ./cli/... -count=1`
Expected: PASS, including Task 1's `TestEveryCapabilityDeclaresHowItsTargetIsResolved` and `TestCapabilityTargetClassesMatchTheSource`. If `cli/verb` has a catalog/golden test, update its expected output to the new rows.

- [ ] **Step 5: Commit** — `server: split resume from spawn; resume / resume_exact gate with per-bit target scopes`

---

### Task 3: Exact resume

**Files:**
- Modify: `server/task_handler.go` (`handleSubmitResume`; the resume branch and the `OpenExecRunnerRequest` build in `handleOpenInteractive`)
- Create: `server/resume_exact_test.go`

**Interfaces:**
- Consumes: Task 1 accessors, Task 2's gate.
- Produces: `func exactMismatch(req exactFields, rec TaskEntry, path protocol.TaskKind) string`. It returns "" when the request is a valid exact resume, else the field name for the error message. `exactFields` is a small struct the two handlers fill from their request: `Prompt string; ExtraArgs []string; AgentProfile string; CapsOverride, ScopePresent bool; Kind protocol.TaskKind` (the kind the path opens: Oneshot for submit, `interactiveKind(req)` for open). A zero `resume_task_id` with exact set is checked by the callers before lookup.

- [ ] **Step 1: Write the failing tests** (fixture: `TestHandleOpenInteractiveOkSetsRepoPathOnOpenExec`, `server/task_handler_test.go:731`, which captures the runner's `OpenExec`):

```go
// TestExactMismatchEachField: for each of prompt / extra_args / agent_profile / caps_override / scope_present,
//   a submit with only that field set + resume_exact → SubmitStatus_ResumeExactMismatch, error_msg names the field
// TestExactKindMismatch: recorded Interactive, submit exact → mismatch; recorded Stream, open exact with event_stream=0 → OpenInteractiveStatus_ResumeExactMismatch
// TestExactWithoutResumeID: resume_exact=1, zero resume_task_id → mismatch on both paths
// TestExactKeepsRecordedSettings: record {Prompt:"p", ExtraArgs:["--x"], AgentProfile:"bash"}; exact submit →
//   entry after resume has the same three; plain resume with no args → ExtraArgs empty (today's behaviour, pinned)
// TestExactOpenSendsRecordedArgsToRunner: exact open → captured OpenExecRunnerRequest.ExtraArgs == ["--x"], AgentProfile == "bash"
// TestExactPinsLastRunner (D18): two runners both advertising "bash", task AssignedTo = runner B →
//   exact resume with Any binds B; with B unregistered and one candidate left → binds A; two left → ambiguous_runner
// TestExactProfileGone (Review Focus 2): recorded profile advertised by no runner → profile_unavailable
```

- [ ] **Step 2: Run to see them fail**

Run: `go test ./server/ -run 'TestExact' -count=1`
Expected: FAIL.

- [ ] **Step 3: Implement.** In both resume branches, when `exact`:
  1. Read the entry (`h.Tasks.Get`) BEFORE candidate resolution and keep `AssignedTo`, because `Tasks.Resume` zeroes it (`server/taskstore.go:441`).
  2. Run `exactMismatch`; on a mismatch, answer the new status. Submit fills `error_msg` with `"resume_exact: <field> is a setting; an exact resume takes the task's recorded one"`.
  3. Fix the profile to the recorded one, for pinned and Any selectors alike, so the picker never re-fires.
  4. If the selector is Any and a runner with `ID == AssignedTo` is among the candidates, narrow to it. Otherwise continue with Any.
  5. Pass the recorded `Prompt` (submit) / `""` (open), the recorded `ExtraArgs` and the recorded profile to `Tasks.Resume`, with both override flags false.
  6. On the open path, also put the recorded args into `OpenExecRunnerRequest.ExtraArgs` instead of `req.ExtraArgs`.

- [ ] **Step 4: Verify** — `go test ./server/ -count=1` → PASS.

- [ ] **Step 5: Commit** — `server: exact resume takes every setting from the record and defaults to the last runner`

---

### Task 4: The seat and the detached open

**Files:**
- Modify: `server/capabilities.go` (new helper), `server/task_handler.go` (`handleOpenInteractive`, its dispatch case)
- Create: `server/open_seat_test.go`

**Interfaces:**
- Produces:
  - `func scopeCoversNewChild(s Scope, c protocol.Capability) bool`. It is true iff `s.ForCap(c)`'s base is `Subtree` or `Global`, or it is `descendants`, i.e. a `Subtree` base with `excludeSelf`. A `None` base, with or without ids, is false.
  - In the open dispatch, after the bit gate: if `!oi.Detached()`, require `hasAnyCap(caps, attachModeCap(AttachMode_Control))` (else deny naming `Capability_ExecControl`). Then require the target to be in the control scope: on a resume, `h.inScope(cid, attachModeScopeCap(AttachMode_Control), idHex)`; on a fresh open, `scopeCoversNewChild(callerScope, Capability_ExecControl)`, or the caller is an operator. If the target is out of that scope, answer `OpenInteractiveStatus_DetachRequired`.

- [ ] **Step 1: Write the failing tests:**

```go
// TestAttachedOpenNeedsExecControl: spawn-only principal, fresh open detached=0 → PermissionDenied naming exec_control; detached=1 → not denied
// TestAttachedOpenOutOfControlScope: spawn + exec_control with --scope-for exec_control=none → fresh attached open → DetachRequired
// TestScopeCoversNewChildAgreesWithActionSet: for each base (subtree, descendants, global, none, ids:<x>):
//   create a real child of the caller, then compare scopeCoversNewChild with actionSet(...)[childHex] (or all)
// TestOperatorAttachedOpenStillWorks (Review Focus 1): no principal, detached=0 → Ok with Attached()==true
// TestDetachedOpenNoClientStream: detached=1 → Ok, Attached()==false, no CreateBidirectionalStream on the client conn,
//   task status Detached (SetDetached called), runner OpenExec still sent
// TestDetachedOpenAppliesInitialSize: rows=40 cols=150 → the runner stream receives one TerminalWindowSize frame 40x150;
//   rows=40 cols=0 (Review Focus 3) → no frame
// TestDetachedX11Refused: detached=1 + x11 → InternalError
// TestAttachedOpenFirstStreamIdZero: first server bidi stream on the client conn (id 0) → Attached()==true and stream id 0
```

- [ ] **Step 2: Run to see them fail** — `go test ./server/ -run 'TestAttachedOpen|TestScopeCoversNewChild|TestOperatorAttachedOpen|TestDetachedOpen|TestDetachedX11' -count=1` → FAIL.

- [ ] **Step 3: Implement.** When `Detached()` is set, `handleOpenInteractive`:
  - skips `tuiConn.CreateBidirectionalStream` and the initial `Attach`;
  - still registers the session mux with its hooks;
  - calls `h.Tasks.SetDetached(taskIDHex)`;
  - if both sizes are non-zero, builds one `TerminalWindowSize` control frame and applies it with `SessionMux.applyWinSizeFrame`;
  - responds `attached=0`.

  Reject `X11Enabled() && Detached()` with `finishWithError` → `InternalError`, beside the existing x11 + event_stream check.

- [ ] **Step 4: Verify** — `go test ./server/ -count=1` → PASS. Then rerun the whole package twice more: `go test ./server/ -count=3`. A pre-existing flaky family is known (`project_flaky_test_open_interactive_sessionmux`); a failure outside the new tests must be compared against `main` before it is blamed on this task.

- [ ] **Step 5: Commit** — `server: an attached open needs exec_control; detached open attaches nobody and carries its size`

---

### Task 5: Client library and the CLI

**Files:**
- Modify: `cli/session_opts.go` (`SessionOpts`, `buildOpenInteractiveRequest`), `cli/submit.go` (`buildSubmitRequest`, the SubmitStatus error switch), `cli/open_interactive_native.go`, `cli/verb/spawn.go`, `cli/verb/table.go` (submit's `Validate`), `cmd/harness-cli/session.go` (`runSessionNewWith`, `spawnOpts`)
- Regenerate: `cli/verb/actions_gen.go` via `go generate ./cli/verb/`
- Test: `cli/session_opts_test.go` (or the existing builder test file), `cli/verb/` spawn rule tests

**Interfaces:**
- Produces:
  - `SessionOpts.ResumeExact bool`, `SessionOpts.Detached bool`, both copied by both builders. When `Detached` is set, the builder also sets the request's initial size; it does NOT do so for an attached open.
  - `func (c *Client) OpenDetached(ctx context.Context, repoPath string, opts SessionOpts) (string, error)`, native only. It forces `Detached = true` and returns the task id. It reads no stream. Ambiguity and status errors are mapped exactly as `openInteractive` maps them.
  - `SpawnAction.Exact bool`, from the new flag.

- [ ] **Step 1: Write the failing tests:**

```go
// TestBuildOpenInteractiveRequestDetached: Detached + rows/cols → request Detached()==true and both sizes set;
//   attached → Detached()==false and no size block
// TestBuildRequestsCarryResumeExact: both builders set ResumeExact
// TestExactRules (cli/verb): `session new --resume X --exact -d` parses; `--exact` without --resume → requirement error;
//   --exact with each of --task / --agent / --agent-arg / --caps / --scope / --scope-for → exclusive error
// TestSubmitExactNeedsNoPrompt (Review Focus 5): `submit --resume X --exact` validates; `submit --resume X --exact do it` is refused
// TestStatusErrorsNameTheFix: resume_exact_mismatch → message contains "--exact"; detach_required → message contains "-d"
```

- [ ] **Step 2: Run to see them fail** — `go test ./cli/... -run 'TestBuildOpenInteractiveRequestDetached|TestBuildRequestsCarryResumeExact|TestExactRules|TestSubmitExactNeedsNoPrompt|TestStatusErrorsNameTheFix' -count=1` → FAIL.

- [ ] **Step 3: Implement.**
  - Flag in `spawnFlags`, for all three kinds: `{Name: "exact", Type: FlagBool, Default: false, Field: "Exact", Help: "with --resume: resume exactly as recorded — no new prompt, args, agent, caps or scope (needs only resume_exact)"}`.
  - Rules: a `Requirement{Flags: {"exact"}, Needs: "resume"}`, and one exclusive `Rule` per conflicting flag, each with the reason `"an exact resume takes the task's recorded settings"`.
  - Submit's `Validate`: when `exact` is set, a prompt (`--task` or trailing text) is refused; otherwise the existing "prompt is required" rule applies.
  - Regenerate with `go generate ./cli/verb/`.
  - In `cmd/harness-cli/session.go`: `spawnOpts` sets `ResumeExact`. Both the `detach` branch and the `a.Stream` branch call `c.OpenDetached`, which deletes `waitStreamCompleted` and the stream close there. The stream branch then calls `SessionStreamAttach` as before.
  - Status errors:
    - `resume_exact_mismatch` → `"resume_exact_mismatch: an exact resume takes the task's recorded settings; drop the setting flags or drop --exact"`. On submit, append the server's `error_msg`.
    - `detach_required` → `"detach_required: you may open this task but not hold its seat; add -d"`.

- [ ] **Step 4: Verify**

Run: `make check && go test ./cli/... ./cmd/... -count=1`
Expected: PASS. `TestEverySurfaceReadsEveryActionField` will FAIL for the TUI and the WebUI until Task 6 reads `Exact`. If it does, note the names here and leave them for Task 6. Nothing else may fail.

- [ ] **Step 5: Commit** — `cli: --exact, SessionOpts.ResumeExact/Detached, OpenDetached replaces open-then-drop`

---

### Task 6: TUI and WebUI

**Files:**
- Modify: `tui/client.go` (`sessionRequest`, `Authority.opts`, `DoSubmitWithOpts`), `tui/interactive.go` (`DoOpenInteractiveWithOpts`, `DoOpenDetachableSession`, `DoStartDetachedSession`, `DoOpenX11Session`), `tui/app.go:2195-2215` (spawn dispatch)
- Modify: `cli/open_interactive_wasm.go` (`Interactive`), `cmd/harness-webui-wasm/main.go` (the submit bridge near `:1030` and the interactive bridge near `:2706`), `webui/static/main.js` (the command-input submit request near `:7440`)
- Test: `tui/` dispatch test beside the existing spawn tests; `webui/static/cmd_test.mjs`

**Interfaces:**
- Consumes: `SessionOpts.ResumeExact` / `Detached`, `Client.OpenDetached` (Task 5).
- Produces: `sessionRequest.ResumeExact bool`, `sessionRequest.Detached bool`, copied in `Authority.opts`. Each `Do*` above gains an `exact bool` parameter right after `resumeConversation`. The TUI keys `r`/`R`/`u`/`U` and every popup pass `false` (spec § Surfaces: omitted, the operator holds `all`).

- [ ] **Step 1: Write the failing tests:**

```go
// TestSpawnDispatchCarriesExact (tui): cmdline `submit --resume <id> --exact` and `session new --resume <id> --exact -d`
//   reach the request build with ResumeExact == true (assert on the cli.SessionOpts the stub client records)
// TestStartDetachedUsesDetachedOpen (tui): DoStartDetachedSession calls OpenDetached, never OpenInteractive
```
```js
// cmd_test.mjs: `submit --resume <id> --exact` → the request object passed to harness.submit has resumeExact === true
```

- [ ] **Step 2: Run to see them fail**

Run: `go test ./tui/ -run 'TestSpawnDispatchCarriesExact|TestStartDetachedUsesDetachedOpen' -count=1 && make js-test`
Expected: FAIL.

- [ ] **Step 3: Implement.**
  - TUI: thread `v.Exact` from `tui/app.go` into the `Do*` calls. `DoStartDetachedSession` calls `c.OpenDetached` and no longer closes a stream.
  - wasm: when `opts.EventStream` is set, `Interactive` builds the request with `Detached = true` and returns the task id without waiting for a stream. Both bridges read `opts.Get("resumeExact")` as a boolean and set `ResumeExact`.
  - `main.js`: the command-input submit request adds `resumeExact: !!b.flags["exact"]`.

- [ ] **Step 4: Verify**

Run: `make check && make wasm-check && make test`
Expected: PASS, including `TestEverySurfaceReadsEveryActionField` and `TestSessionOptsIsBuiltInOnePlace`.

- [ ] **Step 5: Commit** — `tui, webui: carry --exact; detached starts use the detached open`

---

### Task 7: Docs and agent skill

**Files:**
- Modify: `README.md` (the capability list; the session section's `-d` paragraph; the example at `:889` and any other example that grants `spawn` to a supervisor expected to resume)
- Modify: `runner/agentskills/supervising-workers/SKILL.md`, then copy it byte-for-byte to `.claude/skills/supervising-workers/SKILL.md` and `.agents/skills/supervising-workers/SKILL.md`
- Modify: `docs/superpowers/specs/2026-10-19-resume-caps-design.md` — append `## Amendment — what shipped` (checklist item 37) only after Task 8.

- [ ] **Step 1: Edit the skill.**
  - At `:173`, the advice to resume rather than spawn now says that resuming needs `resume`, not `spawn`.
  - The `--caps` examples at `:343` gain `resume` wherever the child is expected to resume its workers.
  - Add a short paragraph on recovering a session you only cowrite: `harness-cli session new --resume <id> --exact -d`, then `session send`. Say that `resume_exact` is the bit needed, and that opening without `-d` needs `exec_control`.

- [ ] **Step 2: Edit the README** to the same facts, and add `resume` / `resume_exact` to its capability list with the `CapDescription` wording from Task 2.

- [ ] **Step 3: Verify**

Run: `go test ./runner/agentskills/ -count=1`
Expected: PASS (`TestMirrorsMatchEmbeddedSkills`, `TestAgentsMirrorHasNoExtraSkills`).

- [ ] **Step 4: Commit** — `docs: resume / resume_exact / exec_control-for-attached in README and supervising-workers`

---

### Task 8: Whole-feature verification

**Files:**
- Modify: `.claude/skills/surface-parity-checklist/firing-log.md` (one entry for this walk)
- Modify: the spec's Amendment (Task 7, Step final)

- [ ] **Step 1: Walk `surface-parity-checklist` items 1–39 by number**, with a verdict per item, against the diff of Tasks 1–7. Item 39 = the spec's § Surfaces table, row by row against the code. Fix gaps in the owning task's files, commit each fix separately.

- [ ] **Step 2: `make test`, `make check`, `make wasm-check`, `make vet`** → all PASS.

- [ ] **Step 3: `scripts/wire-skew-check.sh`** → exit 0. Record what it reports.

- [ ] **Step 4: Live run on the dummy harness** (read the `dummy-harness` skill first). Use two runner slots serving the same root with the same presets, and run the five steps of spec § Testing "Live" in the exact spellings the help text prints. Expected results:
  - Step 3: `session new --resume X --exact -d` prints X; `ls` shows X Running/Detached with its recorded args; `session send X 'echo ok'` works.
  - Step 4: the three refusals read "requires capability resume", "requires capability spawn", and "requires capability exec_control".
  - Step 5: the TUI's and the WebUI's sessions are sized; a full-screen program draws.

  Bring the dummy instance down afterwards.

- [ ] **Step 5: Amendment** — append to the spec what shipped, any divergence, and the commands and outputs of Steps 2–4 (`feedback_numbers_carry_their_command`). Commit: `docs(spec): resume caps — what shipped`.
