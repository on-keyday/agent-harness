# harness-cli mcp — third pass Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every CLI verb is an MCP tool unless it structurally cannot be one; the tool list is filtered by a capability rule derived from `authority`, never written in the table.

**Architecture:** The table declares, per verb, MCP or MCPNotATool and the request kinds it SENDS. `authority.MaySatisfy` decides from a kind and a held set whether some request of that kind could pass. A guard in `cli.Client` refuses a kind a verb did not declare. `cmd/harness-cli` bodies stop writing fd 1, reading fd 0 and calling `os.Exit`, so one process can serve many calls.

**Tech Stack:** Go; JSON-RPC 2.0 over stdio; `go/ast` for the body-hygiene guard.

**Spec:** `docs/superpowers/specs/2026-11-22-harness-cli-mcp-surface-design.md` (third pass, D13–D16). The landed first pass is `0b5a069d` (`cmd/harness-cli/mcp.go`, `mcp_test.go`, golden).

## Global Constraints

- The CLI's behaviour does not change: same stdout, stderr, exit code for every verb (an `os.Exit(n)` becomes a returned exit-code error that `die()` turns into the same exit).
- No bit is written in `cli/verb/table.go`; capability knowledge comes from `authority` only.
- stdout of `harness-cli mcp` is JSON-RPC only (D11).
- `make test` green at every commit; never bare `go test` (use the Makefile's `GO_TEST_ENV` prefix for targeted runs).
- No `.bgn` change.

## Review Focus

1. **A wrongly hidden tool** — agent send (reach-only, no bit) or file ls (file_write alone) must stay listed for a caller lacking board_peer / file_read. Pinned in Task 1 and Task 6.
2. **A body that still writes fd 1 or reads fd 0 under MCP** — would corrupt the JSON-RPC stream or swallow it. Pinned by Task 4's go/ast guard and Task 6's stdio-purity test.
3. **A `Sends` list missing a kind** — breaks that verb on the operator's CLI at first use (by design). Pinned by Task 7's every-tool live sweep.
4. **A positional value starting with `-`** (a topic named `-x`, a text `--force`) parsed as a flag. Pinned in Task 6.
5. **`session new` through MCP without `-d`** would take the server's terminal. Pinned in Task 6 (Fixed `--detach`).

---

### Task 1: `authority.MaySatisfy`

**Files:** Modify `authority/need.go`; Test `authority/need_branches_test.go`.

**Produces:** `func MaySatisfy(kind protocol.TaskControlKind, held protocol.Capability) bool` — true when some request in `variants(kind)` has every clause intersecting `held` (implications via the existing `ExercisedBits`-style widening: a holder of exec_control satisfies exec_view clauses — clauses already list alternatives, so plain intersection suffices); `Later` requirements count as satisfiable; a class other than `NeedBits` is satisfiable.

- [ ] Step 1: test `TestMaySatisfy` rows: AgentSend held=0 → true (reach-only); ListFiles held=file_write → true; OpenFileTransfer held=file_read → true (pull); OpenFileTransfer held=0 → false; Cancel held=0 → false, held=cancel → true; List held=0 → true; OpenInteractive held=spawn → true (detached spawn); OpenInteractive held=0 → false; SetCaps held=0 → true (identity).
- [ ] Step 2: run → FAIL (undefined). Step 3: implement. Step 4: PASS. Step 5: commit `authority: MaySatisfy`.

### Task 2: declaration types and the `mcp` verb

**Files:** Modify `cli/verb/verb.go` (MCPTool: drop `Name`, add `Omit`, `Fixed`, `Why`, `WriteFlags` (spec D17); VerbSpec: add `MCPNotATool string`, `Sends []protocol.TaskControlKind`), `cli/verb/table.go` (`mcp` verb: drop `--stdio`, add `--caps` with `FieldType: "*protocol.Capability", Convert: "parseCapsFlag"`, Notes per spec §5.1), regenerate `cli/verb/actions_gen.go` (`go generate ./cli/verb`), `cmd/harness-cli/mcp.go` (drop `MCP.Name` use; `Mcp(a)` passes `a.ReadOnly`, `a.Caps`).

- [ ] Step 1: `go generate ./cli/verb` then `make test` → PASS (`TestGeneratedFileIsCurrent` included). Commit `cli/verb: MCPNotATool, Sends, Omit/Fixed; mcp --caps`.

### Task 3: every CLI verb declared

**Files:** Modify `cli/verb/table.go` (and `membershipVerb`); Create `cli/verb/mcp_declaration_test.go`.

- [ ] Step 1: write `TestEveryCLIVerbHasAnMCPVerdict` (exactly one of MCP / MCPNotATool per CLI verb; `mcp` is MCPNotATool), `TestToolsDeclareSends` (every MCP verb has non-nil Sends), `TestMCPOmitNamesRealFlags` (each Omit and WriteFlags entry names a flag of the verb; Omit/Fixed non-empty ⇒ Why non-empty; WriteFlags only on a `read` verb), `TestToolNamesAreUnique`. Run → FAIL (undeclared verbs).
- [ ] Step 2: declare every verb. MCPNotATool (with the spec §6 reason): watch, notify-watch, exec tap, forward tap, session stream attach, forward, ssh-gateway, agent wait, agent dispatch, session await-idle, await-progress, session exec, session attach, file edit, file new, exec, lifecycle, mcp. Omit/Fixed: conns (Omit follow, watch), session new (Fixed `--detach`, Omit detach, x11, x11-display). SideEffect `write` for anything that always can write server state or a local file (file pull, credential keygen/bind, workspace save/rm/forward add/rm, prune-local, restore …); `read` otherwise, with WriteFlags where a flag makes it write: record get (`o`), transcript get (`o`), skill (`extract`, `force`). Sends: from the body's calls (survey: `grep -n TaskControlKind_ cli/*.go cli/agent/*.go` per function; `c.Snapshot` sends List). Run → PASS.
- [ ] Step 3: `make test` → PASS. Commit `cli/verb: every CLI verb has an MCP verdict and its Sends`.

### Task 4: bodies safe for a long-lived process

**Files:** Modify `cmd/harness-cli/dispatch.go`, `session.go`, `workspace.go`, `main.go`; Create `cmd/harness-cli/body_hygiene_test.go`.

**Produces:** `cliVerbs.in io.Reader` + `stdin()`; `type exitCodeError struct{ code int; msg string }` with `die()` exiting `code` after printing `msg` if non-empty.

- [ ] Step 1: write `TestToolBodiesDoNotTouchProcessStreams` — go/ast over non-test files of `cmd/harness-cli`: inside every function reachable as a tool body (the cliVerbs method set plus the helpers they call by name in this package: run*With, printSessionScreen, printDetectReport, runWorkspaceAction, editWorkspaceForwards, printWatchers…), refuse `os.Exit`, `os.Stdin`, `os.Stdout`, `fmt.Print`, `fmt.Printf`, `fmt.Println`; allowlist the not-a-tool bodies (Exec/runExecAction, FileEdit/FileNew/runFileEdit/runFileNew/editViaExternalEditor, Forward/forwardIPLink/readFlagBody, SessionAttach/runSessionAttachWith, SessionStreamAttach, SessionExec/runSessionExecWith, SessionAwaitIdle/runSessionAwaitIdleWith, AwaitProgress, Lifecycle, SshGateway, Watch, NotifyWatch, ExecTap, ForwardTap, AgentWait, AgentDispatch, Mcp) and main/die/helpWidth with reasons. Run → FAIL listing sites.
- [ ] Step 2: convert: every `fmt.Print*` / `os.Stdout` in tool bodies → `h.stdout()` (session.go functions gain an `out io.Writer` parameter); `os.Exit(n)` in ServerDialRunner, RunnerEnroll, RunnerUnenroll, RunnerDialAdd, RunnerDialRm, RunnerKick → `return exitCodeError{code: 1}`; `exitOnAmbiguous` → `exitCodeError{code: 3, msg: candidates}`; `runSessionResizeWith` rejected → `exitCodeError{code: 3, msg: "session resize: " + hint}`; BoardSend/AgentSend read `h.stdin()`. `die()` handles exitCodeError. Run → PASS.
- [ ] Step 3: `make test` → PASS (CLI behaviour tests unchanged). Commit `cmd/harness-cli: tool bodies write their sink, read their stdin, return exit codes`.

### Task 5: the `Sends` guard

**Files:** Create `cli/expect_kinds.go`, `cli/expect_kinds_test.go`; Modify `cli/client.go` (RoundTripTaskControl, BeginTaskControl), `cmd/harness-cli/main.go` and the MCP call path.

**Produces:** `func ExpectKinds(verb string, kinds []protocol.TaskControlKind) (restore func())` — process-wide; nil kinds = unchecked. `RoundTripTaskControl` / `BeginTaskControl` return `fmt.Errorf("%s: sent %v, which its declaration (VerbSpec.Sends) does not list", verb, kind)` before sending when a set is installed and the kind is outside it.

- [ ] Step 1: tests: with `ExpectKinds("x", [List])`, a `RoundTripTaskControl` of Cancel fails with that text and sends nothing (fake conn records no frame); List passes; after `restore()` nothing is checked. → FAIL, implement, PASS.
- [ ] Step 2: `main.go` installs the dispatched verb's `Sends` (via `verb.ParseCLICommand`'s spec lookup) before dispatch; the MCP `callTool` installs and restores per call. `make test` → PASS. Commit `cli: refuse a kind the verb did not declare`.

### Task 6: `mcp.go` — filters, errors, schema completeness

**Files:** Modify `cmd/harness-cli/mcp.go`, `mcp_test.go`, `testdata/mcp_tools_golden.json` (regenerate with `-update-mcp`).

- [ ] Step 1: tests: `TestMCPHoldFilter` (held=0: agent_send listed, cancel not; held=cancel: cancel listed); `TestMCPCapsFlagNarrows` (`--caps file_read` + held=all: file_pull listed, cancel not, version listed); `TestMCPWithheldToolIsRPCError` (read-only + write tool, caps-filtered tool, unknown name → JSON-RPC `-32602`, no handler ran); `TestMCPTrailingAndDashValues` (agent_send text `--force`, board read topic `-x`: argv keeps them as values; a Pathspec verb with a `-`-leading positional is refused); `TestMCPSessionNewIsDetached` (argv starts with `--detach`; `x11` refused as unknown); `TestMCPWriteFlags` (`--read-only`: record_get listed with `readOnlyHint: true`, no `o` property, a call naming `o` refused as unknown; without `--read-only`: `o` present and `readOnlyHint: false`); `TestMCPArgvRoundTripEveryTool` (for every tool, minimal arguments from the schema's required set → argv → Action equals the canonical argv's); stdio purity over the whole list. → FAIL.
- [ ] Step 2: implement: held from `cli.WhoAmI` (operator → all; failure → unfiltered + stderr line), `--caps` intersection, `MaySatisfy` over `Sends` (any; empty → true); JSON-RPC errors for withheld; Trailing property; Omit/Fixed; `--` / refusal for `-` values; stdin reader that errors; exit-code errors → `isError` with captured output. Regenerate golden. → PASS. `make test` → PASS. Commit `mcp: the whole table, filtered by authority`.

### Task 7: live

- [ ] Step 1: `scripts/dummy-harness.sh up --detach --agent fake`; from a `--caps all` bash task, drive `harness-cli mcp` with a JSON-RPC script that calls EVERY listed tool once with minimal valid arguments (a running target task id where one is needed); assert no response text contains "does not list" (the Sends guard) and no response is a transport failure. Fix Sends gaps found (each a Ruling).
- [ ] Step 2: V2: `tools/call version --json` and one write tool (`progress`) give the same bytes as the CLI. V8 live: a `--caps file_write` task's `tools/list` contains `file_ls` and `agent_send`, not `cancel`.
- [ ] Step 3: real client: `pi mcp list` (or `claude mcp` if pi is absent) against `harness-cli mcp` → connected, N tools. Down the dummy.
- [ ] Step 4: final review (one fresh reviewer, most capable model); fix Critical/Important; land with `scripts/land.py`.
