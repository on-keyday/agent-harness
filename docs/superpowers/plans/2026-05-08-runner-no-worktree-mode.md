# Runner `--no-worktree` Mode Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a runner-level `--no-worktree` mode (with optional `--force-inject-harness-settings` companion) that runs each task directly in the bound `repoPath` instead of creating a per-task git worktree.

**Architecture:** Two new boolean fields on `runner.Session` and `runner.Config`, plumbed from two new CLI flags on `cmd/agent-runner`. The `handleAssign` and `handleOpenExec` flows branch on `s.NoWorktree` at three points: worktree create (skip), settings/skills injection (skip — or run if `ForceInjectHarnessSettings=true`), and `wm.RemoveIfClean` cleanup (skip — always, even with force-inject). HARNESS_* env injection is unchanged. No protocol/server/client changes.

**Tech Stack:** Go 1.22+, standard `flag` package, `log/slog`, `os/exec` (for git probes in tests). Reference spec: `docs/superpowers/specs/2026-05-08-runner-no-worktree-mode-design.md`.

---

## File Structure

| File | Change |
|------|--------|
| `runner/session.go` | Add `NoWorktree bool` and `ForceInjectHarnessSettings bool` fields on `Session`. Branch in `handleAssign` (steps 2/4/7) and `handleOpenExec` (steps 2/4/7). |
| `runner/connect.go` | Add `NoWorktree bool` and `ForceInjectHarnessSettings bool` fields on `Config`; propagate to `Session` in `Run`. |
| `cmd/agent-runner/main.go` | Register `--no-worktree` and `--force-inject-harness-settings` flags; pass to `runner.Config`. |
| `runner/session_test.go` | Six new tests covering both modes and concurrency / resume / force-inject. |
| `integration/e2e_test.go` | One new scenario: e2e with `runner.Config{NoWorktree: true}` over a non-git dir. |
| `README.md` | Two-paragraph addition to the operating-modes section. |

---

## Task 1: Skip worktree create / injection / cleanup in handleAssign

**Files:**
- Test: `runner/session_test.go` (append at end)
- Modify: `runner/session.go` (Session struct + `handleAssign`)

- [ ] **Step 1: Write the failing test**

Append at the end of `runner/session_test.go`:

```go
// TestHandleAssign_NoWorktree_NoGitDir verifies that with Session.NoWorktree=true
// the runner executes claude with cwd=repoPath, does not create a git worktree,
// does not inject .claude/settings.json or .claude/skills/, and does not delete
// the repo dir on cleanup. The repoPath in this test is a non-git tempdir, which
// would otherwise fail the worktree create step.
func TestHandleAssign_NoWorktree_NoGitDir(t *testing.T) {
	repo := t.TempDir() // non-git on purpose
	fake := writeFakeClaude(t, `pwd`)
	ms := &mockSender{}
	s := &Session{
		AllowedRoots: []string{repo},
		ClaudeBin:    fake,
		Timeout:      5 * time.Second,
		Sender:       ms,
		Now:          time.Now,
		NoWorktree:   true,
	}
	var taskIDBytes [16]byte
	taskIDBytes[0] = 0xAB
	req := &protocol.AssignTask{
		TaskId: protocol.TaskID{Id: taskIDBytes},
		Prompt: []byte("nw-no-git"),
	}
	req.SetRepoPath([]byte(repo))
	s.handleAssign(context.Background(), req)

	// (1) Last message is TaskFinished, ExitCode 0.
	if len(ms.sent) < 3 {
		t.Fatalf("expected ≥3 messages, got %d", len(ms.sent))
	}
	last := decodeRunnerMsg(t, ms.sent[len(ms.sent)-1])
	if last.Kind != protocol.RunnerMessageType_TaskFinished {
		t.Fatalf("last msg kind: %v", last.Kind)
	}
	if tf := last.TaskFinished(); tf == nil || tf.ExitCode != 0 {
		t.Fatalf("finished: %+v", tf)
	}

	// (2) TaskStarted carries WorktreeDir == repoPath.
	var startedDir string
	for _, raw := range ms.sent {
		m := decodeRunnerMsg(t, raw)
		if m.Kind == protocol.RunnerMessageType_TaskStarted {
			ts := m.TaskStarted()
			if ts != nil {
				startedDir = string(ts.WorktreeDir())
			}
		}
	}
	if startedDir != repo {
		t.Fatalf("TaskStarted.WorktreeDir: got %q, want %q", startedDir, repo)
	}

	// (3) claude saw cwd=repoPath via `pwd`.
	var combined []byte
	ms.mu.Lock()
	for _, p := range ms.publishes {
		combined = append(combined, p.data...)
	}
	ms.mu.Unlock()
	if !strings.Contains(string(combined), repo) {
		t.Fatalf("expected pwd output to contain %q; got %q", repo, combined)
	}

	// (4) No worktree dir created.
	if _, err := os.Stat(filepath.Join(repo, ".harness-worktrees")); !os.IsNotExist(err) {
		t.Fatalf(".harness-worktrees should not exist; stat err=%v", err)
	}

	// (5) No .claude/ injection happened.
	if _, err := os.Stat(filepath.Join(repo, ".claude")); !os.IsNotExist(err) {
		t.Fatalf(".claude/ should not exist in no-worktree mode without force-inject; stat err=%v", err)
	}

	// (6) repo dir itself survives the run.
	if _, err := os.Stat(repo); err != nil {
		t.Fatalf("repo dir disappeared: %v", err)
	}
}
```

The test uses `os` and `filepath` packages — verify the existing file imports them (it does, via `path/filepath`; add `os` if not already present).

- [ ] **Step 2: Add the imports if missing**

Check `runner/session_test.go` imports. Required for the new test: `os`. If not present in the import block at the top of the file, add it. Run:

```
go vet ./runner/...
```

Expected: imports complain (or test wouldn't compile).

- [ ] **Step 3: Run test to verify it fails**

```
go test ./runner/ -run TestHandleAssign_NoWorktree_NoGitDir -v
```

Expected: build error first ("`s.NoWorktree` undefined" or similar). After the field is added in Step 4 the test will fail because `handleAssign` does not branch on it.

- [ ] **Step 4: Add `NoWorktree` field to Session**

In `runner/session.go`, in the `Session` struct (search for `type Session struct {`), add at the bottom of the field block (just before `mu sync.Mutex`):

```go
	// NoWorktree, when true, makes handleAssign / handleOpenExec skip the
	// worktree create / branch / cleanup steps and run the agent process
	// directly in the request's RepoPath. Settings/skills injection is also
	// skipped by default (use ForceInjectHarnessSettings to override).
	// HARNESS_* env vars are still injected. Set from runner.Config.NoWorktree.
	NoWorktree bool
```

- [ ] **Step 5: Branch handleAssign on NoWorktree**

In `runner/session.go`, in `handleAssign`, replace the existing Step 2 (worktree create) block:

```go
	// Step 2: Create worktree.
	wm := s.getWorktreeManager(repoPath)
	dir, err := wm.Create(taskIDHex)
	if err != nil {
		finishWithError(-1, "worktree_error: "+err.Error())
		return
	}
```

with:

```go
	// Step 2: Create worktree (skipped in NoWorktree mode — agent runs in repoPath directly).
	var dir string
	var wm *WorktreeManager
	if s.NoWorktree {
		dir = repoPath
		s.logger().Info("no-worktree mode: using repo path as cwd", "task_id", taskIDHex, "repo", repoPath)
	} else {
		wm = s.getWorktreeManager(repoPath)
		d, err := wm.Create(taskIDHex)
		if err != nil {
			finishWithError(-1, "worktree_error: "+err.Error())
			return
		}
		dir = d
	}
```

Then wrap the Step 4 settings/skills injection. Find the existing block:

```go
	// Write .claude/settings.json into the worktree so the inbox hook fires.
	// Non-fatal: task continues even if settings file can't be written.
	if err := WriteAgentSettings(dir); err != nil {
		s.logger().Warn("write agent settings failed", "task_id", taskIDHex, "err", err)
	}
	if err := WriteAgentSkills(dir); err != nil {
		s.logger().Warn("write agent skills failed", "task_id", taskIDHex, "err", err)
	}
```

Replace with:

```go
	// Write .claude/settings.json into the worktree so the inbox hook fires.
	// Non-fatal: task continues even if settings file can't be written.
	// In NoWorktree mode this is skipped by default to avoid polluting the
	// user's repo; ForceInjectHarnessSettings re-enables it (Task 6).
	if !s.NoWorktree {
		if err := WriteAgentSettings(dir); err != nil {
			s.logger().Warn("write agent settings failed", "task_id", taskIDHex, "err", err)
		}
		if err := WriteAgentSkills(dir); err != nil {
			s.logger().Warn("write agent skills failed", "task_id", taskIDHex, "err", err)
		}
	}
```

(The `ForceInjectHarnessSettings` term is added in Task 6; for now this condition is a single-flag guard.)

Then wrap the Step 6 cleanup. Find:

```go
	switch r := wm.RemoveIfClean(taskIDHex, HarnessInjectedPaths); {
	case r.StatusErr != nil:
		s.logger().Warn("worktree cleanup skipped", "task_id", taskIDHex, "err", r.StatusErr)
	case !r.Removed:
		s.logger().Info("worktree retained — uncommitted work present", "task_id", taskIDHex, "dirty", r.DirtyPaths)
	}
```

Replace with:

```go
	if !s.NoWorktree {
		switch r := wm.RemoveIfClean(taskIDHex, HarnessInjectedPaths); {
		case r.StatusErr != nil:
			s.logger().Warn("worktree cleanup skipped", "task_id", taskIDHex, "err", r.StatusErr)
		case !r.Removed:
			s.logger().Info("worktree retained — uncommitted work present", "task_id", taskIDHex, "dirty", r.DirtyPaths)
		}
	}
```

- [ ] **Step 6: Run test to verify it passes**

```
go test ./runner/ -run TestHandleAssign_NoWorktree_NoGitDir -v
```

Expected: PASS.

Also run the full `runner` suite to confirm no regression:

```
go test ./runner/ -v
```

Expected: all pre-existing tests still pass.

- [ ] **Step 7: Commit**

```
git add runner/session.go runner/session_test.go
git commit -m "runner: NoWorktree field skips worktree/settings/cleanup in handleAssign"
```

---

## Task 2: Skip worktree in handleOpenExec

**Files:**
- Test: `runner/session_test.go` (append)
- Modify: `runner/session.go` (`handleOpenExec`)

We mirror the three branches into `handleOpenExec` and add a unit test that mirrors the existing `TestHandleOpenExecGateFailureClosesStream` pattern (noopBidiStream + `ClaudeBin = /bin/true`) but on the success path — non-git tempdir + `NoWorktree=true` should reach `TaskFinished` without a worktree create error.

- [ ] **Step 1: Write the failing test**

Append to `runner/session_test.go`:

```go
// TestHandleOpenExec_NoWorktree_NoGitDir mirrors TestHandleAssign_NoWorktree_NoGitDir
// but for the interactive PTY path. Uses /bin/true as ClaudeBin and the existing
// noopBidiStream so the exec terminates immediately; we only verify the
// runner-side wiring (no worktree create error, WorktreeDir == repoPath, no
// .harness-worktrees/, no .claude/).
func TestHandleOpenExec_NoWorktree_NoGitDir(t *testing.T) {
	repo := t.TempDir() // non-git on purpose
	ms := &mockSender{}
	const streamID trsf.StreamID = 100
	stream := &noopBidiStream{streamID: streamID}
	lookup := &fakeBidiLookup{streams: map[trsf.StreamID]trsf.BidirectionalStream{streamID: stream}}

	s := &Session{
		AllowedRoots: []string{repo},
		ClaudeBin:    "/bin/true",
		Sender:       ms,
		Streams:      lookup,
		Now:          time.Now,
		NoWorktree:   true,
	}
	var taskIDBytes [16]byte
	taskIDBytes[0] = 0xBE
	oer := &protocol.OpenExecRunnerRequest{
		TaskId:   protocol.TaskID{Id: taskIDBytes},
		StreamId: uint64(streamID),
	}
	oer.SetRepoPath([]byte(repo))

	s.handleOpenExec(context.Background(), oer)

	// (1) Last message is TaskFinished (success or non-zero — both mean "we got past worktree create").
	if len(ms.sent) < 3 {
		t.Fatalf("expected ≥3 messages, got %d", len(ms.sent))
	}
	last := decodeRunnerMsg(t, ms.sent[len(ms.sent)-1])
	if last.Kind != protocol.RunnerMessageType_TaskFinished {
		t.Fatalf("last msg kind: %v", last.Kind)
	}
	tf := last.TaskFinished()
	if tf == nil {
		t.Fatal("TaskFinished missing payload")
	}
	if bytes.Contains(tf.ErrorMessage, []byte("worktree_error")) {
		t.Errorf("got worktree_error in NoWorktree mode: %q", tf.ErrorMessage)
	}

	// (2) TaskStarted.WorktreeDir == repoPath.
	var startedDir string
	for _, raw := range ms.sent {
		m := decodeRunnerMsg(t, raw)
		if m.Kind == protocol.RunnerMessageType_TaskStarted {
			ts := m.TaskStarted()
			if ts != nil {
				startedDir = string(ts.WorktreeDir())
			}
		}
	}
	if startedDir != repo {
		t.Errorf("TaskStarted.WorktreeDir: got %q, want %q", startedDir, repo)
	}

	// (3) No worktree dir created.
	if _, err := os.Stat(filepath.Join(repo, ".harness-worktrees")); !os.IsNotExist(err) {
		t.Errorf(".harness-worktrees should not exist; stat err=%v", err)
	}

	// (4) No .claude/ injection.
	if _, err := os.Stat(filepath.Join(repo, ".claude")); !os.IsNotExist(err) {
		t.Errorf(".claude/ should not exist in NoWorktree without force-inject; stat err=%v", err)
	}
}
```

The test references `fakeBidiLookup` — confirm it exists in `session_test.go` (it does, used by `TestHandleOpenExecGateFailureClosesStream`). If the file does not yet import `bytes`, add it.

- [ ] **Step 2: Run test to verify it fails**

```
go test ./runner/ -run TestHandleOpenExec_NoWorktree_NoGitDir -v
```

Expected: FAIL — `handleOpenExec` does not branch on `s.NoWorktree` yet, so `wm.Create` will fail on the non-git tempdir and `TaskFinished` will carry `worktree_error: ...`.

- [ ] **Step 3: Branch handleOpenExec Step 2**

In `runner/session.go`, in `handleOpenExec`, replace:

```go
	// Step 2: worktree.
	wm := s.getWorktreeManager(repoPath)
	dir, err := wm.Create(taskIDHex)
	if err != nil {
		_ = stream.CloseBoth()
		finishWithError(-1, "worktree_error: "+err.Error())
		return
	}
```

with:

```go
	// Step 2: worktree (skipped in NoWorktree mode — agent runs in repoPath directly).
	var dir string
	var wm *WorktreeManager
	if s.NoWorktree {
		dir = repoPath
		log.Info("no-worktree mode: using repo path as cwd", "task_id", taskIDHex, "repo", repoPath)
	} else {
		wm = s.getWorktreeManager(repoPath)
		d, err := wm.Create(taskIDHex)
		if err != nil {
			_ = stream.CloseBoth()
			finishWithError(-1, "worktree_error: "+err.Error())
			return
		}
		dir = d
	}
```

- [ ] **Step 4: Wrap Step 4 settings/skills injection**

Find:

```go
	// Write .claude/settings.json into the worktree so the inbox hook fires.
	// Non-fatal: task continues even if settings file can't be written.
	if err := WriteAgentSettings(dir); err != nil {
		log.Warn("write agent settings failed", "task_id", taskIDHex, "err", err)
	}
	if err := WriteAgentSkills(dir); err != nil {
		log.Warn("write agent skills failed", "task_id", taskIDHex, "err", err)
	}
```

Replace with:

```go
	// Write .claude/settings.json into the worktree so the inbox hook fires.
	// Non-fatal: task continues even if settings file can't be written.
	// Skipped in NoWorktree mode by default; ForceInjectHarnessSettings overrides (Task 6).
	if !s.NoWorktree {
		if err := WriteAgentSettings(dir); err != nil {
			log.Warn("write agent settings failed", "task_id", taskIDHex, "err", err)
		}
		if err := WriteAgentSkills(dir); err != nil {
			log.Warn("write agent skills failed", "task_id", taskIDHex, "err", err)
		}
	}
```

- [ ] **Step 5: Wrap Step 6 cleanup**

Find:

```go
	switch r := wm.RemoveIfClean(taskIDHex, HarnessInjectedPaths); {
	case r.StatusErr != nil:
		log.Warn("worktree cleanup skipped", "task_id", taskIDHex, "err", r.StatusErr)
	case !r.Removed:
		log.Info("worktree retained — uncommitted work present", "task_id", taskIDHex, "dirty", r.DirtyPaths)
	}
```

Replace with:

```go
	if !s.NoWorktree {
		switch r := wm.RemoveIfClean(taskIDHex, HarnessInjectedPaths); {
		case r.StatusErr != nil:
			log.Warn("worktree cleanup skipped", "task_id", taskIDHex, "err", r.StatusErr)
		case !r.Removed:
			log.Info("worktree retained — uncommitted work present", "task_id", taskIDHex, "dirty", r.DirtyPaths)
		}
	}
```

- [ ] **Step 6: Run test to verify it passes**

```
go test ./runner/ -run TestHandleOpenExec_NoWorktree_NoGitDir -v
```

Expected: PASS.

- [ ] **Step 7: Verify no regression**

```
go test ./runner/ -v
```

Expected: all pre-existing tests still pass (notably `TestHandleOpenExecGateFailureClosesStream`).

- [ ] **Step 8: Commit**

```
git add runner/session.go runner/session_test.go
git commit -m "runner: NoWorktree field also skips worktree/settings/cleanup in handleOpenExec"
```

---

## Task 3: Verify HEAD untouched on existing git repo

**Files:**
- Test: `runner/session_test.go` (append)

This task is test-only — Task 1's implementation already covers the behavior; we add a guard test to ensure no regression around the git-repo case (which the non-git test in Task 1 cannot cover).

- [ ] **Step 1: Write the test**

Append to `runner/session_test.go`:

```go
// TestHandleAssign_NoWorktree_GitDir_HEADUntouched verifies that running a task
// in NoWorktree mode against a real git repo does not modify the user's HEAD,
// does not create the harness/<id> branch, and does not create a worktree dir.
func TestHandleAssign_NoWorktree_GitDir_HEADUntouched(t *testing.T) {
	repo := initRepo(t) // git repo on branch "main"
	headBefore := readHEADRef(t, repo)

	fake := writeFakeClaude(t, `echo nw-git-test`)
	ms := &mockSender{}
	s := &Session{
		AllowedRoots: []string{repo},
		ClaudeBin:    fake,
		Timeout:      5 * time.Second,
		Sender:       ms,
		Now:          time.Now,
		NoWorktree:   true,
	}
	var taskIDBytes [16]byte
	taskIDBytes[0] = 0xCD
	req := &protocol.AssignTask{
		TaskId: protocol.TaskID{Id: taskIDBytes},
		Prompt: []byte("nw-git"),
	}
	req.SetRepoPath([]byte(repo))
	s.handleAssign(context.Background(), req)

	// HEAD ref unchanged.
	if got := readHEADRef(t, repo); got != headBefore {
		t.Errorf("HEAD changed: before=%q after=%q", headBefore, got)
	}

	// No harness/<id> branch.
	taskHex := hex.EncodeToString(taskIDBytes[:])
	if branchExists(t, repo, "harness/"+taskHex) {
		t.Errorf("branch harness/%s should not exist in NoWorktree mode", taskHex)
	}

	// No worktree dir.
	if _, err := os.Stat(filepath.Join(repo, ".harness-worktrees", taskHex)); !os.IsNotExist(err) {
		t.Errorf(".harness-worktrees/%s should not exist; stat err=%v", taskHex, err)
	}
}

// readHEADRef returns the current symbolic HEAD value of repo (e.g. "refs/heads/main").
func readHEADRef(t *testing.T, repo string) string {
	t.Helper()
	cmd := exec.Command("git", "symbolic-ref", "HEAD")
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("symbolic-ref HEAD: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// branchExists reports whether refs/heads/<name> is a valid ref in repo.
func branchExists(t *testing.T, repo, name string) bool {
	t.Helper()
	cmd := exec.Command("git", "show-ref", "--verify", "--quiet", "refs/heads/"+name)
	cmd.Dir = repo
	return cmd.Run() == nil
}
```

The test imports `os/exec`. Verify the test file has it imported; if not, add it.

- [ ] **Step 2: Run the test**

```
go test ./runner/ -run TestHandleAssign_NoWorktree_GitDir_HEADUntouched -v
```

Expected: PASS (Task 1's implementation already handles this).

- [ ] **Step 3: Commit**

```
git add runner/session_test.go
git commit -m "test: NoWorktree leaves git HEAD and refs untouched"
```

---

## Task 4: Concurrent tasks coexist in NoWorktree mode

**Files:**
- Test: `runner/session_test.go` (append)

- [ ] **Step 1: Write the test**

Append to `runner/session_test.go`:

```go
// TestHandleAssign_NoWorktree_ConcurrentTasks verifies that two tasks assigned
// concurrently to the same Session in NoWorktree mode both reach TaskFinished
// without serializing on each other (no shared worktree mutex).
func TestHandleAssign_NoWorktree_ConcurrentTasks(t *testing.T) {
	repo := t.TempDir()
	fake := writeFakeClaude(t, `sleep 0.1; echo done`)
	ms := &mockSender{}
	s := &Session{
		AllowedRoots: []string{repo},
		ClaudeBin:    fake,
		Timeout:      5 * time.Second,
		Sender:       ms,
		Now:          time.Now,
		NoWorktree:   true,
	}

	var wg sync.WaitGroup
	wg.Add(2)
	for i := byte(1); i <= 2; i++ {
		i := i
		go func() {
			defer wg.Done()
			req := &protocol.AssignTask{
				TaskId: protocol.TaskID{Id: [16]byte{i}},
				Prompt: []byte("nw-concurrent"),
			}
			req.SetRepoPath([]byte(repo))
			s.handleAssign(context.Background(), req)
		}()
	}
	wg.Wait()

	ms.mu.Lock()
	sentCopy := append([][]byte{}, ms.sent...)
	ms.mu.Unlock()
	finished := collectTaskFinished(t, sentCopy)

	for _, id := range []byte{1, 2} {
		key := [16]byte{id}
		tf, ok := finished[key]
		if !ok {
			t.Errorf("no TaskFinished for task %d", id)
			continue
		}
		if tf.ExitCode != 0 {
			t.Errorf("task %d: ExitCode=%d (want 0); err=%q", id, tf.ExitCode, tf.ErrorMessage)
		}
	}
}
```

- [ ] **Step 2: Run the test**

```
go test ./runner/ -run TestHandleAssign_NoWorktree_ConcurrentTasks -v
```

Expected: PASS.

- [ ] **Step 3: Commit**

```
git add runner/session_test.go
git commit -m "test: NoWorktree allows concurrent tasks on the same repo"
```

---

## Task 5: Resume (same task_id twice) succeeds in NoWorktree mode

**Files:**
- Test: `runner/session_test.go` (append)

- [ ] **Step 1: Write the test**

Append to `runner/session_test.go`:

```go
// TestHandleAssign_NoWorktree_Resume verifies that running the same task_id
// twice in NoWorktree mode does not break (no worktree state to reuse, no
// branch to re-attach — both runs simply use cwd=repoPath).
func TestHandleAssign_NoWorktree_Resume(t *testing.T) {
	repo := t.TempDir()
	fake := writeFakeClaude(t, `echo run`)
	ms := &mockSender{}
	s := &Session{
		AllowedRoots: []string{repo},
		ClaudeBin:    fake,
		Timeout:      5 * time.Second,
		Sender:       ms,
		Now:          time.Now,
		NoWorktree:   true,
	}
	taskID := protocol.TaskID{Id: [16]byte{0x99}}
	for i := 0; i < 2; i++ {
		req := &protocol.AssignTask{
			TaskId: taskID,
			Prompt: []byte("nw-resume"),
		}
		req.SetRepoPath([]byte(repo))
		s.handleAssign(context.Background(), req)
	}

	ms.mu.Lock()
	sentCopy := append([][]byte{}, ms.sent...)
	ms.mu.Unlock()

	// Count TaskFinished messages for this task — expect exactly 2, both ExitCode 0.
	var finishedCount, okCount int
	for _, raw := range sentCopy {
		m := decodeRunnerMsg(t, raw)
		if m.Kind != protocol.RunnerMessageType_TaskFinished {
			continue
		}
		tf := m.TaskFinished()
		if tf == nil || tf.TaskId.Id != taskID.Id {
			continue
		}
		finishedCount++
		if tf.ExitCode == 0 {
			okCount++
		}
	}
	if finishedCount != 2 || okCount != 2 {
		t.Fatalf("expected 2 successful TaskFinished, got finished=%d ok=%d", finishedCount, okCount)
	}
}
```

- [ ] **Step 2: Run the test**

```
go test ./runner/ -run TestHandleAssign_NoWorktree_Resume -v
```

Expected: PASS.

- [ ] **Step 3: Commit**

```
git add runner/session_test.go
git commit -m "test: NoWorktree allows the same task_id to run twice (resume path)"
```

---

## Task 6: Add ForceInjectHarnessSettings field + behavior

**Files:**
- Test: `runner/session_test.go` (append)
- Modify: `runner/session.go` (Session struct + two condition flips)

- [ ] **Step 1: Write the failing test**

Append to `runner/session_test.go`:

```go
// TestHandleAssign_NoWorktree_ForceInject verifies that with both
// NoWorktree=true and ForceInjectHarnessSettings=true, the runner writes
// .claude/settings.json (with harness-cli hooks) and .claude/skills/
// directly into repoPath. Cleanup is still skipped — the injected files
// persist past task end.
func TestHandleAssign_NoWorktree_ForceInject(t *testing.T) {
	repo := t.TempDir()
	fake := writeFakeClaude(t, `echo force-inject`)
	ms := &mockSender{}
	s := &Session{
		AllowedRoots:               []string{repo},
		ClaudeBin:                  fake,
		Timeout:                    5 * time.Second,
		Sender:                     ms,
		Now:                        time.Now,
		NoWorktree:                 true,
		ForceInjectHarnessSettings: true,
	}
	var taskIDBytes [16]byte
	taskIDBytes[0] = 0xEE
	req := &protocol.AssignTask{
		TaskId: protocol.TaskID{Id: taskIDBytes},
		Prompt: []byte("nw-force-inject"),
	}
	req.SetRepoPath([]byte(repo))
	s.handleAssign(context.Background(), req)

	// (1) settings.json exists in repoPath.
	settingsPath := filepath.Join(repo, ".claude", "settings.json")
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("expected settings.json at %s, got err=%v", settingsPath, err)
	}
	// settings.json carries the harness-cli command prefix (any matching hook is fine).
	if !strings.Contains(string(data), "harness-cli ") {
		t.Errorf("settings.json missing harness-cli hook commands; content=%s", data)
	}

	// (2) skills dir exists with at least one entry.
	skillsDir := filepath.Join(repo, ".claude", "skills")
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		t.Fatalf("expected skills dir at %s, got err=%v", skillsDir, err)
	}
	if len(entries) == 0 {
		t.Errorf("skills dir is empty; expected at least one harness skill")
	}

	// (3) No worktree dir despite force-inject.
	if _, err := os.Stat(filepath.Join(repo, ".harness-worktrees")); !os.IsNotExist(err) {
		t.Errorf(".harness-worktrees should not exist in NoWorktree mode; stat err=%v", err)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

```
go test ./runner/ -run TestHandleAssign_NoWorktree_ForceInject -v
```

Expected: build error first ("`ForceInjectHarnessSettings` undefined"). After Step 3 the test fails because the condition still skips injection.

- [ ] **Step 3: Add `ForceInjectHarnessSettings` field**

In `runner/session.go`, in the `Session` struct, add immediately after the `NoWorktree` field:

```go
	// ForceInjectHarnessSettings, when true, causes WriteAgentSettings /
	// WriteAgentSkills to run even in NoWorktree mode (target = repoPath).
	// No-op when NoWorktree=false (worktree mode always injects regardless).
	// Set from runner.Config.ForceInjectHarnessSettings.
	ForceInjectHarnessSettings bool
```

- [ ] **Step 4: Flip the injection condition in handleAssign**

In `handleAssign`, find the block added in Task 1:

```go
	if !s.NoWorktree {
		if err := WriteAgentSettings(dir); err != nil {
			s.logger().Warn("write agent settings failed", "task_id", taskIDHex, "err", err)
		}
		if err := WriteAgentSkills(dir); err != nil {
			s.logger().Warn("write agent skills failed", "task_id", taskIDHex, "err", err)
		}
	}
```

Replace the condition with:

```go
	if !s.NoWorktree || s.ForceInjectHarnessSettings {
```

(Body unchanged.)

- [ ] **Step 5: Flip the same condition in handleOpenExec**

In `handleOpenExec`, find the analogous block from Task 2 and apply the identical condition change:

```go
	if !s.NoWorktree || s.ForceInjectHarnessSettings {
```

The cleanup (`RemoveIfClean`) condition is **NOT** changed — it stays gated only by `!s.NoWorktree`. Calling git-worktree-remove on the user's main checkout would be destructive, regardless of force-inject.

- [ ] **Step 6: Run test to verify it passes**

```
go test ./runner/ -run TestHandleAssign_NoWorktree_ForceInject -v
```

Expected: PASS.

Run the full runner suite to confirm no regression:

```
go test ./runner/ -v
```

Expected: all tests pass.

- [ ] **Step 7: Commit**

```
git add runner/session.go runner/session_test.go
git commit -m "runner: ForceInjectHarnessSettings re-enables .claude/* injection in NoWorktree mode"
```

---

## Task 7: Wire CLI flags + Config plumbing

**Files:**
- Modify: `runner/connect.go` (Config struct + Run plumbing)
- Modify: `cmd/agent-runner/main.go` (flags + Config)

No new test — flag wiring is exercised by the integration test in Task 8 and by manual smoke.

- [ ] **Step 1: Add fields to `runner.Config`**

In `runner/connect.go`, in the `Config` struct (after `PSK`), add:

```go
	// NoWorktree disables the per-task git worktree creation. Tasks run with
	// cwd = AssignTask.RepoPath. Settings/skills injection and worktree
	// cleanup are skipped by default. See spec
	// docs/superpowers/specs/2026-05-08-runner-no-worktree-mode-design.md.
	NoWorktree bool

	// ForceInjectHarnessSettings is only meaningful with NoWorktree=true:
	// it re-enables WriteAgentSettings / WriteAgentSkills (target = RepoPath).
	// Worktree cleanup remains disabled in NoWorktree mode regardless.
	ForceInjectHarnessSettings bool
```

- [ ] **Step 2: Propagate to `Session` in `runner.Run`**

In `runner/connect.go`, in `Run`, find the `session := &Session{...}` literal and add the two fields. Look for the existing field block (with `AllowedRoots`, `ClaudeBin`, etc.) and add at the bottom:

```go
		NoWorktree:                 cfg.NoWorktree,
		ForceInjectHarnessSettings: cfg.ForceInjectHarnessSettings,
```

- [ ] **Step 3: Add startup log line**

Still in `runner/connect.go`, immediately before the `peer.Dial` call (at the top of `Run`, after `cfg.Logger` is defaulted), add:

```go
	cfg.Logger.Info("runner config",
		"no_worktree", cfg.NoWorktree,
		"force_inject_harness_settings", cfg.ForceInjectHarnessSettings)
```

- [ ] **Step 4: Register flags in `cmd/agent-runner/main.go`**

In `cmd/agent-runner/main.go`, in the `var ( ... )` block (near the existing flags), add:

```go
	noWorktree                 = flag.Bool("no-worktree", false, "skip per-task git worktree creation; run agent processes directly in the bound repo path. Disables .claude/settings.json and .claude/skills/ injection by default (see --force-inject-harness-settings).")
	forceInjectHarnessSettings = flag.Bool("force-inject-harness-settings", false, "only meaningful with --no-worktree: re-enable .claude/settings.json and .claude/skills/ injection at the bound repo path.")
```

- [ ] **Step 5: Pass flags into `runner.Config`**

In `cmd/agent-runner/main.go`, in the `runner.Run(ctx, runner.Config{...})` literal, add at the bottom of the field list:

```go
		NoWorktree:                 *noWorktree,
		ForceInjectHarnessSettings: *forceInjectHarnessSettings,
```

- [ ] **Step 6: Build and check `--help`**

```
go build ./cmd/agent-runner
./agent-runner --help 2>&1 | grep -E '(no-worktree|force-inject)'
```

Expected output (formatting may vary):

```
  -force-inject-harness-settings
        only meaningful with --no-worktree: ...
  -no-worktree
        skip per-task git worktree creation; ...
```

Then clean up the local binary:

```
rm -f agent-runner
```

- [ ] **Step 7: Run all unit tests**

```
go test ./runner/ ./cmd/... -v
```

Expected: all pass.

- [ ] **Step 8: Commit**

```
git add runner/connect.go cmd/agent-runner/main.go
git commit -m "agent-runner: --no-worktree and --force-inject-harness-settings flags

Plumbed through runner.Config -> runner.Session. Startup log records
both values for visibility."
```

---

## Task 8: Integration test — runner with --no-worktree end-to-end

**Files:**
- Modify: `integration/e2e_test.go` (append a new test function)

- [ ] **Step 1: Write the integration test**

Append to `integration/e2e_test.go`, after `TestSubmitFakeClaudeE2E`:

```go
// TestSubmitFakeClaudeE2E_NoWorktree mirrors TestSubmitFakeClaudeE2E but starts
// the runner with NoWorktree=true over a non-git tempdir. Verifies the full
// server↔runner↔CLI flow works end-to-end without git worktree machinery.
func TestSubmitFakeClaudeE2E_NoWorktree(t *testing.T) {
	if testing.Short() {
		t.Skip("E2E test skipped in -short mode")
	}

	// Plain tempdir — no git init.
	repo := t.TempDir()
	fakeClaude, err := filepath.Abs("../testdata/fake-claude.sh")
	if err != nil {
		t.Fatal(err)
	}

	addr := "127.0.0.1:18540" // distinct port from TestSubmitFakeClaudeE2E
	peerCID, err := objproto.ParseConnectionID("ws:"+addr+"-*",
		objproto.ParseOption_AllowRandomID|objproto.ParseOption_ResolveAddr)
	if err != nil {
		t.Fatalf("parse server cid: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := server.New(server.Config{
		Addr:    addr,
		DataDir: t.TempDir(),
	})
	serverDone := make(chan error, 1)
	go func() { serverDone <- s.Run(ctx) }()
	time.Sleep(300 * time.Millisecond)

	runnerDone := make(chan error, 1)
	go func() {
		runnerDone <- runner.Run(ctx, runner.Config{
			ServerCID:    peerCID,
			AllowedRoots: []string{repo},
			ClaudeBin:    fakeClaude,
			NoWorktree:   true,
		})
	}()
	time.Sleep(500 * time.Millisecond)

	taskID, err := cli.Submit(ctx, peerCID, repo, "nw-e2e")
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	t.Logf("submitted task %s", taskID)

	// fake-claude.sh creates hello.txt in cwd. With NoWorktree, cwd == repo.
	helloPath := filepath.Join(repo, "hello.txt")
	if !waitForFile(t, helloPath, 15*time.Second) {
		t.Fatalf("hello.txt did not appear at %s within timeout", helloPath)
	}
	t.Logf("artifact present at %s", helloPath)

	// Verify task reached Succeeded.
	deadline := time.Now().Add(10 * time.Second)
	var lastOutput string
	for time.Now().Before(deadline) {
		var buf bytes.Buffer
		if err := cli.List(ctx, peerCID, &buf); err != nil {
			t.Fatalf("list: %v", err)
		}
		lastOutput = buf.String()
		if strings.Contains(lastOutput, "Succeeded") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !strings.Contains(lastOutput, "Succeeded") {
		t.Fatalf("task never reached Succeeded; last list output:\n%s", lastOutput)
	}

	// .harness-worktrees must not exist.
	if _, err := os.Stat(filepath.Join(repo, ".harness-worktrees")); !os.IsNotExist(err) {
		t.Errorf(".harness-worktrees should not exist in NoWorktree mode; stat err=%v", err)
	}

	cancel()
	select {
	case <-serverDone:
	case <-time.After(2 * time.Second):
		t.Log("server did not exit within 2s of cancel")
	}
	select {
	case <-runnerDone:
	case <-time.After(2 * time.Second):
		t.Log("runner did not exit within 2s of cancel")
	}
}
```

- [ ] **Step 2: Run the integration test**

```
go test ./integration/ -run TestSubmitFakeClaudeE2E_NoWorktree -v
```

Expected: PASS. (The existing `TestSubmitFakeClaudeE2E` should also still pass — run both:)

```
go test ./integration/ -v
```

Expected: all PASS.

- [ ] **Step 3: Commit**

```
git add integration/e2e_test.go
git commit -m "integration: e2e flow with --no-worktree over a non-git dir"
```

---

## Task 9: README update

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Locate the right section**

Open `README.md`. Search for the section describing runner CLI flags / operating modes — around the existing `--max-tasks`, `--claude-bin`, `--roots` discussion (the README has a runner-introduction section near `bin/agent-runner` examples).

- [ ] **Step 2: Add the new modes paragraph**

In the runner-introduction / flags section, add a new subsection (or extend the existing flag list):

```markdown
### Operating modes

By default the runner creates a `git worktree` per task under
`<repo>/.harness-worktrees/<task-id>/` and runs the agent in that isolated
checkout. Two flags adjust this:

- `--no-worktree`: skip worktree creation and run each task directly in the
  bound repo path (`AllowedRoots[0]` or the request's `RepoPath`). Intended
  for generic-process workloads (`--claude-bin bash`, etc.). Disables
  `.claude/settings.json` and `.claude/skills/` injection by default —
  agentboard hooks are not auto-installed in this mode. The user's repo is
  left untouched on task end (no `git worktree remove` is ever called).
  `HARNESS_*` environment variables are still injected into every spawned
  process.

- `--force-inject-harness-settings`: only meaningful with `--no-worktree`.
  Re-enables `.claude/settings.json` / `.claude/skills/` injection at the
  bound repo path, so agentboard hooks fire even without a per-task
  worktree. Files persist after task end (no auto-cleanup); manage them
  manually if desired.
```

If `README.md` does not have a clear "operating modes" subsection, place this near the existing per-flag descriptions and adjust the heading level to match the surrounding structure.

- [ ] **Step 3: Verify rendering**

Visual check only — open the file and confirm the new section reads cleanly in context. No tooling needs to run.

- [ ] **Step 4: Commit**

```
git add README.md
git commit -m "docs: README — runner --no-worktree / --force-inject-harness-settings"
```

---

## Task 10: Final verification

- [ ] **Step 1: Run the entire test suite**

```
go test ./...
```

Expected: all packages pass. Note: the integration tests take longer; if any fail with timing-related issues, increase the small `time.Sleep` in the new e2e test (Task 8) before declaring a real failure.

- [ ] **Step 2: Manual smoke (optional, recommended)**

In one terminal:

```
go run ./cmd/harness-server  # or however the server is launched in your dev workflow
```

In another:

```
go run ./cmd/agent-runner --no-worktree --claude-bin /bin/bash --roots /tmp/scratch
```

In a third:

```
mkdir -p /tmp/scratch
harness-cli submit --repo /tmp/scratch --task 'pwd && ls -la'
```

Expected: task succeeds; the log shows `cwd == /tmp/scratch`; `/tmp/scratch/.harness-worktrees/` does not exist; `/tmp/scratch/.claude/` does not exist; `/tmp/scratch` itself is unchanged after the task ends.

Repeat with `--force-inject-harness-settings` added → expect `/tmp/scratch/.claude/settings.json` and `/tmp/scratch/.claude/skills/` to appear (and persist).

- [ ] **Step 3: Confirm spec coverage**

Open `docs/superpowers/specs/2026-05-08-runner-no-worktree-mode-design.md` and tick every requirement against the commits made. Each item in the spec's "File touch list" should map to a Task in this plan; each test in the spec's "Testing" section should map to one of Tasks 1, 3, 4, 5, 6, 8.
