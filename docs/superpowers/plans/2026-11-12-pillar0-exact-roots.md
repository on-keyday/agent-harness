# Pillar 0 — exact roots and one placement function — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A task's repo must equal one of a runner's roots, on the server and on
the runner, and submit, open and queue assignment all answer "who may take this
task" from `Registry.Candidates`.

**Architecture:** One exact predicate, `protocol.RootMatches`, replaces
`IsUnderRoot`/`MatchLen` everywhere. `Registry.Candidates` loses its
longest-prefix ranking. `Scheduler.Tick` stops matching roots itself: it walks
queued tasks in creation order and asks `Candidates` for each. The unused third
placement copy, `Dispatcher.TryDispatch`, is deleted.

**Tech Stack:** Go (server, runner, runner/protocol). No `.bgn` change.

**Spec:** `docs/superpowers/specs/2026-11-12-run-modes-and-records-grand-design.md`
— rows G9 and G17, § Pillar 0, and problem P3.

## Global Constraints

- Read `.claude/skills/implementation-pitfalls/SKILL.md` before starting.
- **No wire change.** `RunnerCandidate.matched_root` stays. It now carries the
  runner's root string that equals the repo.
- Path rule (unchanged from `IsUnderRoot`'s contract): wire paths are
  slash-separated. They are either POSIX-absolute (`/a`) or drive-letter-absolute
  (`C:/a`). Both sides go through `path.Clean`. Drive letters are NOT
  case-normalised.
- A runner with no roots (`len(s.AllowedRoots) == 0`) still accepts every repo
  on the runner gates. That is unchanged.
- Foreground and submit keep `NoRunner` / `NoRunnerForRepo` /
  `PinnedNotFound`. Background enqueue-only admission (G26) is pillar A, not
  this plan.
- Deploy order: a new server with an old runner is safe, because the server now
  sends a subset of what the old runner accepts. An old server with a new runner
  can assign a subdirectory task that the new runner refuses. Restart the server
  first, or use `scripts/build_and_restart_all.py`, which does that.
- Verify with `make` targets, run with `env -u HARNESS_SERVER_PUBKEY -u
  HARNESS_SERVER_CID -u HARNESS_AUTH_TICKET -u HARNESS_TASK_ID`, because a task
  shell's inherited `HARNESS_*` fails two UDP-forward tests otherwise.
- The live store was checked before writing this plan. One saved task has a repo
  below a root, and its agent is not offered by that root's runner, so it was
  already unresumable. Nothing in this plan migrates it. Do not write paths or
  ids from that check into the repo, which is public.

## Review Focus

1. **Slash noise:** root `/a/b/` and repo `/a/b`, or repo `/a//b`, must match,
   because both sides are cleaned (Task 1 test).
2. **POSIX root `/`** now serves only the repo `/`. It used to cover every
   absolute path, and that coverage is gone on purpose (Task 1 test).
3. **Drive-letter case:** `c:/w` against root `C:/w` does not match. This is
   pinned so the behaviour is visible, not changed (Task 1 test).
4. **Parent-rooted runner while the repo's own runner is disconnected:** a
   queued task stays `Queued` and is not taken (Task 3 test). This is the
   silent-fallback case G9 removes.
5. **A runner with free slots and several queued tasks** takes them in creation
   order in one `Tick`. A task pinned to an absent runner does not block later
   tasks (Task 3 tests).

---

### Task 1: The exact predicate, and the runner gates

**Files:**
- Modify: `runner/protocol/pathmatch.go` (add `RootMatches`. Keep
  `IsUnderRoot`/`MatchLen` until Task 2, which removes their last callers)
- Modify: `runner/session.go:533-542` (`repoAllowed`)
- Test: `runner/protocol/pathmatch_test.go`, `runner/session_test.go:287-310`

**Interfaces:**
- Produces: `func RootMatches(root, repo string) bool` in package `protocol`.
  It returns true iff both are absolute (`isAbsCrossOS`) and
  `path.Clean(root) == path.Clean(repo)`.

- [ ] **Step 1: Write the failing test** `TestRootMatches` in `pathmatch_test.go`. Use a table with these cases:

```go
{"equal", "/home/u/ws", "/home/u/ws", true},
{"child refused", "/home/u/ws", "/home/u/ws/foo", false},
{"parent refused", "/home/u/ws", "/home/u", false},
{"sibling lookalike", "/home/u/ws", "/home/u/ws-evil", false},
{"trailing slash root", "/a/b/", "/a/b", true},
{"trailing slash repo", "/a/b", "/a/b/", true},
{"redundant slashes", "/a/b", "/a//b", true},
{"relative repo refused", "/a/b", "a/b", false},
{"relative root refused", "a/b", "/a/b", false},
{"posix root serves only itself", "/", "/anything", false},
{"posix root vs itself", "/", "/", true},
{"win equal", "C:/Users/foo", "C:/Users/foo", true},
{"win child refused", "C:/Users/foo", "C:/Users/foo/bar", false},
{"win drive case not normalised", "C:/w", "c:/w", false},
{"win different drive", "C:/w", "D:/w", false},
{"win drive without slash refused", "C:w", "C:w", false},
{"cross-form mismatch", "/Users/foo", "C:/Users/foo", false},
```

- [ ] **Step 2: Run it and see it fail.** Run `go test ./runner/protocol/ -run TestRootMatches`. Expected: build failure, `undefined: RootMatches`.

- [ ] **Step 3: Implement `RootMatches`.** Its doc comment takes over the wire-format and cross-OS paragraph from `IsUnderRoot`, and states that it is the single predicate shared by `Registry.Candidates` and `Session.repoAllowed` (G9).

- [ ] **Step 4: Switch `repoAllowed` to `protocol.RootMatches`.** Rewrite its doc to say "equals one of AllowedRoots". Rewrite `TestSessionRepoAllowedDelegatesToProtocol` with root `/home/user/repos` and these cases: `/home/user/repos` true, `/home/user/repos/` true, `/home/user/repos/project` false, `/home/user/other` false, `/home/user` false, `relative/path` false.

- [ ] **Step 5: Run the tests.** Run `go test ./runner/protocol/ ./runner/`. Expected: PASS. If another runner test fails because its fixture's repo is below its root, edit the fixture so the repo equals the root. Do not edit a test whose subject is containment; there are none in `runner/` today.

- [ ] **Step 6: Commit.** Message: `runner: roots match exactly (G9)`.

### Task 2: `Candidates` matches exactly; the scoring goes

**Files:**
- Modify: `server/registry.go:378-444` (`Candidates`; delete `bestRootScore`)
- Modify: `server/task_handler.go:2873-2884` (`matchedRoot`)
- Modify: `runner/protocol/pathmatch.go` (delete `IsUnderRoot`, `MatchLen`)
- Modify: `cmd/agent-runner/main.go:114` (`--roots` help text)
- Test: `server/registry_test.go`, `server/task_handler_test.go`, `runner/protocol/pathmatch_test.go`

**Interfaces:**
- Consumes: `protocol.RootMatches` (Task 1).
- Produces: `Registry.Candidates(repo string, sel protocol.RunnerSelector) []RunnerEntry`, with the same signature as today. Every selected runner that has a root `RootMatches`-equal to repo is returned, sorted by `ConnectedAt` then ID as today. The result is capacity-agnostic, as today.

- [ ] **Step 1: Write the failing tests.**
  - In `registry_test.go`, rewrite `TestRegistryCandidatesPrefixMatch` as `TestRegistryCandidatesExactMatch`. Runner A has roots `/home/u/ws` and `/home/u/ws/repo1`; runner B has root `/srv/ws`. `Candidates("/home/u/ws/repo1")` returns only A. `Candidates("/home/u/ws/other")` returns none. `Candidates("/srv/ws/foo")` returns none.
  - Add `TestRegistryCandidatesParentRootDoesNotServe`. Runner `broad` has root `/a` and is connected; no other runner. `Candidates("/a/b")` is empty.
  - In `task_handler_test.go`, add `TestHandleSubmitSubdirOfRootIsNoRunner`. A runner with root `/x` is registered; submitting `/x/repo` answers `SubmitStatus_NoRunner`, and `h.Tasks` holds no task afterwards. This is P3, which used to be answered `ok` and then stay `Queued`.
  - Add `TestHandleOpenInteractiveSubdirOfRootIsNoRunner`. It is the same setup through `handleOpenInteractive`, and the answer is `OpenInteractiveStatus_NoRunnerForRepo`.

- [ ] **Step 2: Run them and see them fail.** Run `go test ./server/ -run 'Candidates|Subdir'`. Expected: the new tests FAIL, because prefix matching still admits the subdirectory.

- [ ] **Step 3: Implement.**
  - `Candidates` keeps the runners for which `r.selects(sel, e)` holds and some root satisfies `RootMatches(root, repo)`. The `scored` type and the max-tier filter are removed. Rewrite its doc comment to describe exact matching and to cite G9.
  - `matchedRoot(roots, repo)` returns the first root with `RootMatches(root, repo)`. Its doc says it is display metadata: the runner's own spelling of the matching root.
  - Delete `IsUnderRoot`, `MatchLen`, `TestIsUnderRoot` and `TestMatchLen`.
  - The `--roots` help becomes `comma-separated list of absolute repo paths this runner serves; a task's repo must equal one of them (no subdirectories)`.

- [ ] **Step 4: Remove the ranking tests.** Delete `TestRegistryCandidatesLongestPrefixMatch`, `TestRegistryCandidatesLongestPrefixTieAmbiguous`, `TestRegistryCandidatesLongestPrefixPerRunnerMaxRoot` and `TestRegistryCandidatesLongestPrefixScopedToSelector`. Their subject, ranking among containing roots, no longer exists. Before deleting the last one, read it: if it also asserts the selector filter on its own, keep that assertion as `TestRegistryCandidatesSelectorFilters` with exact roots.

- [ ] **Step 5: Run the server and runner suites, and fix fixtures by one rule.** Run `go test ./server/ ./runner/...`. Some tests will fail because a fixture registers root `/x` and uses repo `/x/repo` (for example `TestRegistryCandidatesAmbiguous`, `…CapacityAgnostic`, `…SelectorByHostname`, `scheduler_test.go:454`). For these, set the repo equal to the root. Do not weaken an assertion.

- [ ] **Step 6: Check that nothing else uses the old predicate.** Run `grep -rn 'IsUnderRoot\|MatchLen\|bestRootScore' --include=*.go .`. Expected: no output.

- [ ] **Step 7: Commit.** Message: `server: Candidates matches roots exactly; drop longest-prefix ranking (G9, G17)`.

### Task 3: `Tick` places through `Candidates`; `TryDispatch` goes

**Files:**
- Modify: `server/scheduler.go` (`Tick` and its doc)
- Modify: `server/taskstore.go:1249-1279` (replace `NextQueuedForRepo`/`NextQueuedForRepoFunc`)
- Modify: `server/dispatch.go:182-269` (delete `TryDispatch`), and fix the field comment at `:74` ("used by TryDispatch and OnCancel") to say OnCancel only
- Delete: `server/trydispatch_test.go`
- Test: `server/scheduler_test.go`, `server/taskstore_test.go:270-296`, `server/server_test.go`

**Interfaces:**
- Consumes: `Registry.Candidates` (Task 2), `RunnerEntry.Status()`, `RunnerEntry.HasProfile`, `Registry.BindTask`/`UnbindTask`, `TaskStore.Assign`.
- Produces: `func (s *TaskStore) QueuedTasks() []TaskEntry`, which returns snapshots of every `Queued` task in creation order (`s.order`).

- [ ] **Step 1: Write the failing tests.**
  - `taskstore_test.go`: replace `TestTaskStoreNextQueuedForRepo` with `TestTaskStoreQueuedTasksInCreationOrder`. Create three tasks on two repos and assign the middle one. `QueuedTasks()` returns the first and third, in that order.
  - `scheduler_test.go` `TestSchedulerParentRootDoesNotTakeTask`: runner `broad` has root `/a` and is idle. A task is queued for `/a/b`. After `Tick` the task is still `Queued` and `assign` was never called. This is Review Focus 4.
  - `TestSchedulerFillsFreeSlotsInOrder`: one runner with root `/x` and `MaxTasks: 2`, and three tasks queued on `/x`. After one `Tick` the first two are assigned, in creation order, and the third is still `Queued`. This is Review Focus 5.
  - Existing tests that must stay green unchanged: `TestSchedulerHonorsSelector`, `…HonorsAgentProfile`, `…IdentityPinSurvivesReconnect`, `…PinnedTaskDoesNotBlockQueue`, `…AssignErrorLeavesQueued`, `…BindsBeforeSending`, `…MultipleRunnersFIFO`.

- [ ] **Step 2: Run them and see them fail.** Run `go test ./server/ -run 'Scheduler|QueuedTasks'`. Expected: `QueuedTasks` is undefined.

- [ ] **Step 3: Implement.**
  - `QueuedTasks` lives beside `List`. Delete `NextQueuedForRepo` and `NextQueuedForRepoFunc`. Move the comment explaining why an ineligible task is skipped rather than stopping the walk ("one task pinned to an offline runner stall[s] every later task") into `Tick`.
  - `Tick`, under `s.mu`, works as follows:

```go
for _, task := range s.store.QueuedTasks() {
    for _, c := range s.reg.Candidates(task.RepoPath, task.Selector) {
        if c.Status() != protocol.RunnerStatus_Idle { continue }
        if task.AgentProfile != "" && !c.HasProfile(task.AgentProfile) { continue }
        if !s.reg.BindTask(c.ID, task.ID) { continue }
        if err := s.assign(c.ID, task.ID); err != nil {
            s.reg.UnbindTask(c.ID, task.ID)
            slog.Error("assign failed", "runner", c.ID, "task", task.ID, "repo", task.RepoPath, "error", err)
            continue
        }
        s.store.Assign(task.ID, c.Identity, "", c.SkillsInjected)
        break
    }
}
```

  - `Candidates` returns snapshots, so a runner filled earlier in the same pass can still read `Idle`. In that case `BindTask`'s capacity check refuses it, and that refusal is the guard.
  - Keep the existing `BoundRunnerID is deliberately NOT enforced` paragraph in `Tick`'s doc. Rewrite the numbered steps to describe the walk over queued tasks.

- [ ] **Step 4: Port the ticket test before deleting the file.** `TestTryDispatch_RegistersTicket` is the only test asserting that a dispatched task's agentboard ticket is in the board and is revoked on `TaskFinished`. The production path shares that code: `sendAssign` calls `boardRegisterTask`. Move the test to `server_test.go` as `TestSendAssignRegistersTicket`:
  - Build a `Server` as `TestSendAssignReachesRunner` does, with `s.Board` set to an `agentboard.New(...)`.
  - Call `s.sendAssign`, then make the same four assertions.
  - Move the helpers it needs (`boardRunnerID`, `boardTaskID`) into `server_test.go`.
  - Then delete `server/trydispatch_test.go`: its HappyPath, NoCapacity and SendError cases are the scheduler tests' AssignsOnePair, SkipsBusy and AssignErrorLeavesQueued.
  - Delete `Dispatcher.TryDispatch`, and remove imports that become unused.

- [ ] **Step 5: Run the tests.** Run `go test ./server/` and then `grep -rn 'TryDispatch\|NextQueuedForRepo' --include=*.go .`. Expected: PASS. The only grep hits are prose comments in `agentboard/registry.go:37`, `agentboard/board.go:97`, `server/agent_wake.go:12`, `server/oncancel_test.go:59,62` and `server/scheduler_test.go:220`. Reword those to "the scheduler's assign" or "sendAssign".

- [ ] **Step 6: Commit.** Message: `server: Tick places through Candidates; delete TryDispatch (G17)`.

### Task 4: Whole-suite and live check

**Files:** none changed unless a check fails.

- [ ] **Step 1: Run the suite.** Run `env -u HARNESS_SERVER_PUBKEY -u HARNESS_SERVER_CID -u HARNESS_AUTH_TICKET -u HARNESS_TASK_ID make test`, then `make build`. Expected: PASS. Note that a server-package flake is known to occur in roughly 1 of 6 package runs. If a failure is unrelated to roots, re-run that package once before investigating.

- [ ] **Step 2: Live check against a dummy harness.** Run `eval "$(scripts/dummy-harness.sh env)"` after `scripts/dummy-harness.sh up --agent fake --detach`. Read the runner's root from `harness-cli ls` (`roots=`); call it `$REPO`.
  - `harness-cli submit --repo "$REPO/sub" --agent bash --task true` must be refused with the NoRunner status, and `harness-cli ls` must show no new task. Before this change it answered ok and stayed `Queued`.
  - `harness-cli session new -d --repo "$REPO/sub" --agent bash` must be refused with NoRunnerForRepo.
  - `harness-cli submit --repo "$REPO" --agent bash --task 'echo ok'` must run to `Succeeded`.
  - Run `scripts/dummy-harness.sh down`.

- [ ] **Step 3: Report.** State that the branch is ready to land, and list the commits. Landing and `make build` in the main checkout follow the repo's landing policy (local main fast-forward, then push).
