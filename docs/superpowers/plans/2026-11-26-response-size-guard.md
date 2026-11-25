# Response Size Guard Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** No task-control response is dropped silently over UDP: one that does not fit the connection's datagram budget is replaced by a `response_too_large` answer the client turns into an error.

**Architecture:** One server function, `sendTaskControl`, encodes every task-control response, checks it with the existing `protocol.CheckControlMessage`, and substitutes a small `ResponseTooLargeResponse` over budget. A line-scan test keeps every send going through it, as `TestRunnerRequestsAreSentOnlyThroughSendRunnerRequest` does for runner requests. The client's single decode point, `(*Client).dispatchControl`, turns the substitute into a typed error.

**Tech Stack:** Go, `.bgn` schema → `make protoregen` (ebm2go), `scripts/netem-lab` for the UDP reproduction.

**Spec:** `docs/superpowers/specs/2026-11-26-grant-conditions-and-bound-enrollment-design.md` — P4, C15, § Response size guard, Testing (C15). This is the first of four plans for that spec; it does not depend on the other three.

## Global Constraints

- Every control message is one objproto message, and over UDP one datagram; ws/wss are not checked (`CheckControlMessage` returns nil for any transport but `udp`).
- Wire changes are appended only (new enum value at the END of `enum TaskControlKind`, new format, new arm). Run `scripts/wire-skew-check.sh`.
- Use `make test` for the full suite (a bare `go test` in a task inherits the live `HARNESS_*` identity); a single package may be run as `env -u HARNESS_SERVER_CID -u HARNESS_SERVER_PUBKEY -u HARNESS_TASK_ID -u HARNESS_AUTH_TICKET go test ./<pkg>/ -run <Name>`.
- Stage by explicit path; never `git add -A` / a directory.
- Do not land: this plan's commits wait on the task branch with the spec until the operator says otherwise.

## Review Focus

- **The substitute itself must fit.** A `ResponseTooLargeResponse` is 1 + 4 + 1 + 4 + 4 bytes plus the app-kind byte; if a future field made it variable, the guard would recurse into a silent drop. Pinned in Task 3 (`TestResponseTooLargeAlwaysFits`).
- **A response sent long after the request** (await-idle firing, idle watchers, `exec_run_list`) still names the right request id. Pinned by the substitute copying `RequestId` (Task 3 test asserts it).
- **ws connection, same oversized response:** must go out unchanged. Pinned in Task 3 (`TestSendTaskControlLeavesWSAlone`).
- **A request whose own kind is `response_too_large`:** nothing sends one; the client must not loop or mis-translate. `need.go` classifies the kind `NeedNotRequest` (Task 2), so a client-sent one is answered as any not-a-request kind is.
- **Old client, new server:** an old client cannot decode the new arm, so its waiter gets `ErrResponseUndecodable` from the existing path (`cli/client_skew_test.go` pins that path) instead of hanging — strictly better than today's silent drop. No test needed beyond the existing one.

---

### Task 1: Reproduce the silent drop (red, before any code)

**Files:** none changed.

**Interfaces:** none.

- [ ] **Step 1: Build the current tree and start a lab**

```bash
make build
scripts/netem-lab/netem-lab.py --name rsg up
eval "$(scripts/netem-lab/netem-lab.py --name rsg env)"
```
Expected: the lab's runner is listed by `scripts/netem-lab/netem-lab.py --name rsg exec cli -- harness-cli --server-cid "$HARNESS_SERVER_CID" ls`.

- [ ] **Step 2: Give a bash task a whoami larger than one datagram**

Open a bash session on the lab runner (`session new -d --agent bash` through `exec cli`), then, as the operator, `caps set --caps exec_view --scope subtree --scope-for exec_view=ids:<80 ids> <task>` with 80 random 32-hex ids, comma-separated (ids are not checked to exist, `authority.Validate`; `--scope-for` is written together with `--scope`, per `caps set --help`). 80 × 16 bytes puts `WhoAmIResponse` above the 1169-byte floor.

- [ ] **Step 3: Observe the hang**

Run `harness-cli whoami` inside the task (`harness-cli exec <task> -- harness-cli whoami`, through `exec cli`) with a 20 s timeout.
Expected: no answer until the timeout, and nothing in either log. Record the command and the observed result in the Task 5 commit message.

- [ ] **Step 4: Leave the lab up for Task 5.**

---

### Task 2: Wire — `response_too_large`

**Files:**
- Modify: `runner/protocol/message.bgn` (end of `enum TaskControlKind`; new format beside `PermissionDeniedResponse`; one arm in `TaskControlResponse`)
- Regenerate: `runner/protocol/message.go`
- Modify: `authority/need.go`, `authority/need_test.go`, `server/scope_completeness_test.go`
- Test: `runner/protocol/response_too_large_test.go`

**Interfaces:**
- Produces: `protocol.TaskControlKind_ResponseTooLarge`; `protocol.ResponseTooLargeResponse{RequestedKind TaskControlKind; Size uint32; Budget uint32}`; `(*TaskControlResponse).SetResponseTooLarge(ResponseTooLargeResponse)` / `.ResponseTooLarge() *ResponseTooLargeResponse` (generated).

- [ ] **Step 1: Write the failing round-trip test** `TestResponseTooLargeRoundTrip`: a `TaskControlResponse{Kind: TaskControlKind_ResponseTooLarge, RequestId: 9}` with `{RequestedKind: TaskControlKind_Whoami, Size: 2000, Budget: 1169}` encodes and `DecodeExact`s back to the same four values.

- [ ] **Step 2: Run it; expected FAIL** (undefined `TaskControlKind_ResponseTooLarge`).

- [ ] **Step 3: Schema.** Append `response_too_large` as the last value of `enum TaskControlKind`, commented: response-only, as `permission_denied`; the server sends it in place of a response that would not fit one UDP datagram (C15). Add

```
format ResponseTooLargeResponse:
    requested_kind :TaskControlKind   # the kind of the response that was too large
    size           :u32               # its encoded size, app-kind byte included
    budget         :u32               # what one datagram held on that connection
```

and the arm `TaskControlKind.response_too_large => response_too_large :ResponseTooLargeResponse` in `TaskControlResponse` only. Then `make protoregen`.

- [ ] **Step 4: Classify the kind.** `authority/need.go`: `TaskControlKind_ResponseTooLarge: NeedNotRequest` beside `PermissionDenied`; add it to the `NeedNotRequest` list in `authority/need_test.go`; `server/scope_completeness_test.go`: `noTarget` beside `PermissionDenied`.

- [ ] **Step 5: Run** `go vet ./...` and the protocol, authority and server packages' tests; expected PASS.

- [ ] **Step 6: Commit** `runner/protocol/message.bgn runner/protocol/message.go runner/protocol/response_too_large_test.go authority/need.go authority/need_test.go server/scope_completeness_test.go` — "protocol: response_too_large (C15)".

---

### Task 3: `sendTaskControl`

**Files:**
- Create: `server/taskcontrol_send.go`
- Test: `server/taskcontrol_send_test.go`

**Interfaces:**
- Consumes: Task 2's types; `protocol.CheckControlMessage(transport string, budget, n int) error`; `ConnHandle.ConnectionID().Transport`, `ConnHandle.MaxDatagramSize()`.
- Produces: `func sendTaskControl(conn ConnHandle, resp protocol.TaskControlResponse)`.

- [ ] **Step 1: Write the failing tests**
  - `TestSendTaskControlReplacesOversizeOverUDP`: `fakeConn` with id `objproto.MustParseConnectionID("udp:127.0.0.1:9000-1")` (`MaxDatagramSize` is 1169). Send a `Whoami` response with `RequestId: 42` whose single override names 80 ids. `lastTaskControlResponse` decodes to `Kind == ResponseTooLarge`, `RequestId == 42`, `RequestedKind == Whoami`, `Size ==` the original encoded length (app-kind byte included), `Budget == 1169`.
  - `TestSendTaskControlLeavesWSAlone`: same response on a `ws:127.0.0.1:9001-4` fakeConn arrives as `Whoami` with its 80 ids.
  - `TestSendTaskControlSendsSmallResponsesAsIs`: a `Cancel` response over udp arrives unchanged.
  - `TestResponseTooLargeAlwaysFits`: the encoded substitute with max field values is ≤ 64 bytes.

- [ ] **Step 2: Run; expected FAIL** (undefined `sendTaskControl`).

- [ ] **Step 3: Implement `sendTaskControl(conn ConnHandle, resp protocol.TaskControlResponse)` in `server/taskcontrol_send.go`.** Encode with the `AppKind_TaskControl` prefix; on a `CheckControlMessage` error, `slog.Warn` with kind, request id, size and budget, and send the substitute instead. Doc comment states why (C15, the silent UDP drop) and names the guard test of Task 4.

- [ ] **Step 4: Run; expected PASS.**

- [ ] **Step 5: Commit** both files — "server: sendTaskControl guards a response's size (C15)".

---

### Task 4: Every response goes through `sendTaskControl`

**Files:**
- Modify: every non-test file in `server/` that appends `AppKind_TaskControl` to a response (48 sites plus `respondAgent` at `server/agent_taskcontrol.go:61` and `denyTaskControl` in `server/task_handler.go`; `grep -rn 'AppKind_TaskControl)' server --include=*.go | grep -v _test`)
- Test: `server/taskcontrol_send_test.go`

**Interfaces:**
- Consumes: `sendTaskControl` (Task 3).
- Removes: `respondAgent` — its 31 callers call `sendTaskControl`.

- [ ] **Step 1: Write the failing guard** `TestTaskControlResponsesAreSentOnlyThroughSendTaskControl`, a line scan over `server/*.go` shaped like `TestRunnerRequestsAreSentOnlyThroughSendRunnerRequest` (`server/runner_send_test.go:50`): fail on any non-test line containing `AppKind_TaskControl` except in `taskcontrol_send.go`, a line that is exactly `case appwire.AppKind_TaskControl:` (receiving), and the `tcReq.Append(` line in `psk.go` (it encodes a REQUEST dispatched locally, not a response). Do not exclude whole files.

- [ ] **Step 2: Run; expected FAIL**, listing every current site (≈48 lines). This is the negative control; keep its output for the commit message count.

- [ ] **Step 3: Replace each site.** `out := resp.MustAppend(...)` + `conn.SendMessage(out)` (and the one-line form) become `sendTaskControl(conn, resp)`; delete `respondAgent` and rename its callers. Where a site sends a pointer or a differently named variable, pass the value. No other behaviour changes.

- [ ] **Step 4: Run** the guard and `env -u … go test ./server/`; expected PASS.

- [ ] **Step 5: Commit** the modified server files by path — "server: every task-control response goes through sendTaskControl".

---

### Task 5: The client turns `response_too_large` into an error

**Files:**
- Create: `cli/response_too_large.go`
- Modify: `cli/client.go` (`(*Client).dispatchControl`)
- Test: `cli/client_skew_test.go`

**Interfaces:**
- Consumes: Task 2's types.
- Produces: `type ResponseTooLargeError struct { RequestedKind protocol.TaskControlKind; Size, Budget uint32 }` with `Error() string`.

- [ ] **Step 1: Write the failing test** `TestResponseTooLargeFailsTheWaiter` beside the two existing tests in `cli/client_skew_test.go`: pending request 7; dispatch a `ResponseTooLarge` response for 7 (`RequestedKind: Whoami, Size: 2000, Budget: 1169`); the waiter receives `r.err` with `errors.As(r.err, &*ResponseTooLargeError)` true and the three fields as sent, and `r.resp == nil`.

- [ ] **Step 2: Run; expected FAIL.**

- [ ] **Step 3: Implement.** In `dispatchControl`, after a successful decode and before `deliver`, a `ResponseTooLarge` response is delivered as `taskControlResult{err: &ResponseTooLargeError{…}}`. Here rather than in `RoundTripTaskControl` / `BeginTaskControl`: it is the one point both pass through, and no request kind can legitimately expect this response. `Error()` reads: `the server's <kind> response is <size> bytes; one udp datagram holds <budget> on this path, so the server did not send it — connect over ws, or narrow what the request returns`.

- [ ] **Step 4: Run** `env -u … go test ./cli/ -run 'Response|Undecodable|Garbage'`; expected PASS.

- [ ] **Step 5: Verify against the lab (green).** `make build`, restart the lab (`netem-lab.py --name rsg down && … up`, then redo Task 1 Step 2), rerun Task 1 Step 3. Expected: `whoami` fails at once with the `ResponseTooLargeError` text naming `Whoami`, and the server log has the `slog.Warn` line. Bring the lab down.

- [ ] **Step 6: Commit** `cli/response_too_large.go cli/client.go cli/client_skew_test.go` — the message records Task 1's red observation and this green one, each with its command.

---

### Task 6: Whole-suite verification

- [ ] **Step 1:** `make test` — expected exit 0, no `FAIL`.
- [ ] **Step 2:** `scripts/wire-skew-check.sh` — expected `PASS`.
- [ ] **Step 3:** `make wasm-check` — expected success (the WebUI reaches the server through the same `cli.Client`, so it inherits Task 5 with no change of its own; TUI and CLI likewise).
