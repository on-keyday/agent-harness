# Delegated Re-grant and Task Rename Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans (operator's chosen method: native) to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A task may `caps set` its descendants live, bounded exactly as a re-granting resume; `session rename` sets a task's display name.

**Architecture:** `handleSetCaps` gains a task-caller branch that gates by R1 and builds the update with the existing `grantFor` (resume semantics). Rename is a new TaskControl kind writing a `task_renamed` WAL event.

**Tech Stack:** Go, brgen `.bgn` → `go generate`, Bubble Tea TUI, Go wasm WebUI.

**Spec:** `docs/superpowers/specs/2026-11-22-delegated-regrant-and-rename-design.md` (R1–R10).

## Global Constraints

- R1 gate (task caller): `resumeTargetAllowed(cid, false, T)` AND `T` a strict descendant of the caller in the live creator index (`h.childIndex()` + `authority.DescendantsOf`); `T == caller` fails.
- Every refusal of R1 → `not_found`. `grantFor` error → `invalid_scope` + message.
- Name rules = `credentialNameRefusal` (≤ 64 bytes, UTF-8, no C0/DEL/C1); empty name clears.
- Operator paths unchanged; existing tests stay green.
- Tests via `make test`; focused runs with `env -u HARNESS_TASK_ID -u HARNESS_RUNNER_ID -u HARNESS_AUTH_TICKET -u HARNESS_SERVER_PUBKEY -u HARNESS_SERVER_CID -u HARNESS_OPERATOR_PSK go test …`.
- Prose edits: Read → Edit only.

## Review Focus

1. A task re-granting its own ancestor's descendant that is its sibling (not its descendant) while holding `resume` with global scope → `not_found` (R1 is descendant-only even when resume scope is global). Test in Task 2.
2. A task caller widening a child's caps past its own `delegate_caps` → clamped exactly as at resume (not an error). Test in Task 2.
3. A task caller passing `--delegate-scope` wider than its own bound → refused with the "outside what you may delegate" text. Test in Task 2.
4. Rename with a 65-byte name / a newline → `bad_name`; empty → clears. Test in Task 3.
5. Rename survives a server restart (WAL replay). Test in Task 3.

---

### Task 1: Wire schema (whole schema, one task)

**Files:** `runner/protocol/message.bgn` → regenerate; `server/cap_completeness_test.go` (`task_rename: capInHandler`), `server/scope_completeness_test.go`, the last-kind guard test; round-trip test `runner/protocol/rename_wire_test.go`.

**Produces (all appended):** `SetCapsResponse.message_len :u16, message :[message_len]u8`; `AuthoritySource.task_caps_set`; `TaskControlKind.task_rename`; `format TaskRenameRequest: task_id :TaskID; name_len :u8; name :[name_len]u8`; `enum TaskRenameStatus: ok, not_found, bad_name`; `format TaskRenameResponse: status :TaskRenameStatus`.

- [ ] Write round-trip tests (both new formats; SetCapsResponse with message) → RED (undefined).
- [ ] Edit `.bgn`, `go generate ./runner/protocol/`, classify the new kind in completeness tests → GREEN; `make test`.
- [ ] Commit `protocol: task_rename and set_caps message (delegated re-grant)`.

### Task 2: `caps set` for task callers (R1, R2, R4–R8)

**Files:** `server/set_caps_handler.go`; test `server/set_caps_task_test.go`.

**Interfaces:** `func (h *TaskHandler) regrantReaches(cid, targetHex string) bool` (R1); `func grantRequestFromSetCaps(req *protocol.SetCapsRequest) grantRequest`.

- [ ] Tests: `TestTaskSetCapsMatchesResumeRegrant` (table: caps/scope/bound/limits; expected = `grantFor` on the same inputs); `TestTaskSetCapsRefusesSelfAncestorSiblingNonDescendant` (each `not_found`, incl. Review Focus 1); `TestTaskSetCapsNeedsResumeOverTarget`; `TestTaskSetCapsClampsPastDelegate` (RF2); `TestTaskSetCapsRefusesBoundPastOwn` (RF3, message text); `TestTaskSetCapsCredentialMaskedCap`; `TestTaskSetCapsProvenance` (`task_caps_set`, actor = caller); `TestTaskSetCapsCascadeAndTeardown` → RED.
- [ ] Implement: principal-zero branch unchanged; task branch per spec § Design steps 1–6; `respond` carries `message`.
- [ ] GREEN; `make test`; commit `server: caps set re-grants a descendant within the caller's bound`.

### Task 3: `session rename` server side (R3, R10)

**Files:** `server/task_rename_handler.go`; `server/taskstore.go` / `server/credential_store.go` (`SetName`, `task_renamed` replay; move `credentialNameRefusal` to a shared `taskNameRefusal`); `server/task_handler.go` dispatch; test `server/task_rename_test.go`.

**Interfaces:** `func (s *TaskStore) SetName(id, name string) bool` (writes `task_renamed{TaskID, Name}`, fires the task-event publication outside the store lock).

- [ ] Tests: operator renames any; task renames self; task renames descendant under R1; non-descendant → `not_found`; 65 bytes / control byte → `bad_name` (RF4); empty clears; `TestRenameSurvivesReplay` (RF5); WAL JSON round-trip test still green → RED.
- [ ] Implement; GREEN; `make test`; commit `server: session rename`.

### Task 4: what-if agrees (R9)

**Files:** `authority/whatif.go` (`apply`, `Regrant`); `authority/whatif_test.go`.

- [ ] Test `TestWhatIfTaskRegrantMatchesServerRule` (descendant ok, non-descendant/self refused, clamps as grantFor) → RED; implement; GREEN; commit `authority: what-if predicts a task's live re-grant`.

### Task 5: CLI

**Files:** `cli/` set_caps client (render `message`; an agent identity no longer special-cases `not_operator`), `cli/rename.go` (three-layer: `*Client.TaskRename`, `TaskRenameWith`, `TaskRenameOut`), `cli/verb/table.go` (`session rename <task-id> <name>`, CLI|TUI|WebUI, notes, example), `cmd/harness-cli/dispatch.go`; tests `cli/rename_test.go`, verb-table suite.

- [ ] Tests (parse through `DispatchCLILine`; refusal texts; `caps set` from an agent identity against the in-process server) → RED; implement; GREEN.
- [ ] Dummy-harness once: agent identity `caps set <worker> --caps …` then `session rename <worker> name`; `ls` shows it.
- [ ] Commit `cli: session rename; caps set from a task`.

### Task 6: TUI + WebUI rename

**Files:** `tui/dispatch.go` (handler via `a.client`), `tui/detail.go` / keys (`n` in the detail popup prefills `session rename <id> `); `cmd/harness-webui-wasm/` export `taskRename`, `webui/static/cmdline.js`, `main.js` task-sheet "rename" action; tests `tui/rename_test.go`, `webui/static/rename_test.mjs`.

- [ ] Tests → RED; implement; GREEN; `make test`.
- [ ] Live check: TUI PTY (cmdline + `n`), WebUI Playwright (task sheet rename); save `rename-*.png/.txt` (untracked).
- [ ] Commit `tui, webui: session rename`.

### Task 7: Docs

**Files:** `README.md` (`caps set` who-may-call; verb list), `runner/agentskills/supervising-workers/SKILL.md` + mirrors (live re-grant of a worker, descendants only, bounded like a re-granting resume; `session rename`), `.claude/skills/surface-parity-checklist/firing-log.md` (walk items touched), spec status.

- [ ] Edit (Read → Edit); keep mirrors byte-identical; `make test`; `scripts/wire-skew-check.sh` PASS; commit `docs: delegated re-grant and session rename`.

### Final

- [ ] One opus whole-branch review; fix its findings in one wave; no second wave.
