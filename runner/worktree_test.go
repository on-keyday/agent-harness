package runner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

// initRepo creates a fresh git repo with one commit in a tempdir, returning its absolute path.
func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README")
	run("commit", "-m", "init")
	return dir
}

func TestCreateWorktree(t *testing.T) {
	repo := initRepo(t)
	wm := &WorktreeManager{Repo: repo}
	dir, err := wm.Create("task-abc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("worktree dir missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "README")); err != nil {
		t.Fatalf("README missing in worktree: %v", err)
	}
}

func TestRemoveWorktree(t *testing.T) {
	repo := initRepo(t)
	wm := &WorktreeManager{Repo: repo}
	dir, _ := wm.Create("task-xyz")

	if err := wm.Remove("task-xyz"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("worktree should be removed, got err=%v", err)
	}
}

func TestCreateWorktreeWithDirtyFile(t *testing.T) {
	// Verify Remove --force handles the case where a worktree has uncommitted changes.
	repo := initRepo(t)
	wm := &WorktreeManager{Repo: repo}
	dir, _ := wm.Create("dirty")
	// Write a new file into the worktree without committing
	os.WriteFile(filepath.Join(dir, "new.txt"), []byte("uncommitted"), 0o644)
	if err := wm.Remove("dirty"); err != nil {
		t.Fatalf("Remove should succeed despite dirty changes: %v", err)
	}
}

// TestCreateReusesExistingWorktreeOnResume verifies the resume-idempotent
// path: when a previous run left both the worktree directory and its git
// registration in place (because wm.Remove failed or the runner crashed
// before cleanup), Create re-uses the existing dir instead of re-running
// `git worktree add` (which would fail with "already exists") or wiping
// the dir (which would drop any uncommitted work claude left behind).
func TestCreateReusesExistingWorktreeOnResume(t *testing.T) {
	repo := initRepo(t)
	wm := &WorktreeManager{Repo: repo}

	dir, err := wm.Create("resume-task")
	if err != nil {
		t.Fatal(err)
	}

	// Simulate uncommitted work the user wants preserved across resume.
	uncommitted := filepath.Join(dir, "in-flight.txt")
	if err := os.WriteFile(uncommitted, []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Resume on the same task id — must NOT fail with "already exists" and
	// MUST preserve the uncommitted file.
	dir2, err := wm.Create("resume-task")
	if err != nil {
		t.Fatalf("resume Create failed: %v", err)
	}
	if dir2 != dir {
		t.Errorf("resume returned different dir: got %q want %q", dir2, dir)
	}
	if data, err := os.ReadFile(uncommitted); err != nil || string(data) != "dirty\n" {
		t.Errorf("uncommitted file not preserved: data=%q err=%v", data, err)
	}
}

// TestCreateAfterStaleRegistration verifies that when the worktree dir is
// gone but its git registration lingers (e.g., someone removed the dir
// directly), Create prunes the stale entry and re-attaches successfully.
func TestCreateAfterStaleRegistration(t *testing.T) {
	repo := initRepo(t)
	wm := &WorktreeManager{Repo: repo}

	dir, err := wm.Create("stale-reg")
	if err != nil {
		t.Fatal(err)
	}
	// Drop just the directory; leave git's registration behind.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	dir2, err := wm.Create("stale-reg")
	if err != nil {
		t.Fatalf("Create after stale registration failed: %v", err)
	}
	if _, err := os.Stat(dir2); err != nil {
		t.Fatalf("worktree dir missing after re-create: %v", err)
	}
}

// TestWorktreeRegisteredFromPorcelainNormalizesSeparators is a regression
// guard for the Windows resume bug: git emits worktree paths with forward
// slashes on every platform, but filepath.Join on Windows produces
// backslashes. The parser must compare in slash-normalised form so the
// resume-idempotent reuse path fires on Windows. (On Linux both sides are
// already slash-form, so this also covers that case.)
func TestWorktreeRegisteredFromPorcelainNormalizesSeparators(t *testing.T) {
	porcelain := []byte("worktree C:/repo/main\n" +
		"HEAD abcd1234\n" +
		"branch refs/heads/main\n" +
		"\n" +
		"worktree C:/repo/.harness-worktrees/task-w\n" +
		"HEAD deadbeef\n" +
		"branch refs/heads/harness/task-w\n" +
		"\n")

	// Windows-style dir produced by filepath.Join on a Windows runner.
	winDir := `C:\repo\.harness-worktrees\task-w`
	if !worktreeRegisteredFromPorcelain(porcelain, winDir, "harness/task-w") {
		t.Errorf("Windows-style dir %q failed to match porcelain forward-slash entry", winDir)
	}

	// Linux-style dir on a Linux runner.
	linDir := "/srv/repo/.harness-worktrees/task-l"
	linPorcelain := []byte("worktree /srv/repo/main\n" +
		"HEAD abcd1234\n" +
		"branch refs/heads/main\n" +
		"\n" +
		"worktree /srv/repo/.harness-worktrees/task-l\n" +
		"HEAD deadbeef\n" +
		"branch refs/heads/harness/task-l\n" +
		"\n")
	if !worktreeRegisteredFromPorcelain(linPorcelain, linDir, "harness/task-l") {
		t.Errorf("Linux-style dir %q failed to match its porcelain entry", linDir)
	}

	// Negative: prunable record must not match.
	prunable := []byte("worktree C:/repo/.harness-worktrees/task-p\n" +
		"HEAD deadbeef\n" +
		"branch refs/heads/harness/task-p\n" +
		"prunable gitdir file points to non-existent location\n" +
		"\n")
	winPrunable := `C:\repo\.harness-worktrees\task-p`
	if worktreeRegisteredFromPorcelain(prunable, winPrunable, "harness/task-p") {
		t.Errorf("prunable record should not match")
	}
}

// TestRemoveIfCleanRemovesWhenNothingDirty verifies the basic clean path:
// a fresh worktree with no edits gets removed.
func TestRemoveIfCleanRemovesWhenNothingDirty(t *testing.T) {
	repo := initRepo(t)
	wm := &WorktreeManager{Repo: repo}
	dir, err := wm.Create("clean")
	if err != nil {
		t.Fatal(err)
	}

	r := wm.RemoveIfClean("clean", HarnessInjectedPaths)
	if !r.Removed {
		t.Fatalf("expected Removed=true, got %+v", r)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("worktree dir should be gone, stat err=%v", err)
	}
}

// TestRemoveIfCleanRemovesWhenOnlyInjectedDirty verifies that the runner's
// own injected files (settings.json, skills, minimal CLAUDE.md) do not
// count as user/agent work — the worktree is still removed.
func TestRemoveIfCleanRemovesWhenOnlyInjectedDirty(t *testing.T) {
	repo := initRepo(t)
	wm := &WorktreeManager{Repo: repo}
	dir, err := wm.Create("inj-only")
	if err != nil {
		t.Fatal(err)
	}
	// Simulate the runner's own writes.
	if err := WriteAgentSettings(dir); err != nil {
		t.Fatal(err)
	}
	if err := WriteAgentSkills(dir); err != nil {
		t.Fatal(err)
	}

	r := wm.RemoveIfClean("inj-only", HarnessInjectedPaths)
	if !r.Removed {
		t.Fatalf("expected Removed=true with only injected files dirty, got %+v", r)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("worktree dir should be gone, stat err=%v", err)
	}
}

// TestRemoveIfCleanRetainsWhenRealWorkPresent verifies the safety net:
// any non-injected uncommitted change keeps the worktree intact and is
// reported back as DirtyPaths so the caller can log what was preserved.
func TestRemoveIfCleanRetainsWhenRealWorkPresent(t *testing.T) {
	repo := initRepo(t)
	wm := &WorktreeManager{Repo: repo}
	dir, err := wm.Create("real-work")
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteAgentSettings(dir); err != nil {
		t.Fatal(err)
	}
	// The piece we want preserved.
	if err := os.WriteFile(filepath.Join(dir, "in-flight.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r := wm.RemoveIfClean("real-work", HarnessInjectedPaths)
	if r.Removed {
		t.Fatal("worktree was removed despite uncommitted in-flight.txt")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("worktree dir should survive, stat err=%v", err)
	}
	found := false
	for _, p := range r.DirtyPaths {
		if p == "in-flight.txt" {
			found = true
		}
	}
	if !found {
		t.Errorf("DirtyPaths should report in-flight.txt, got %v", r.DirtyPaths)
	}
}

// TestRemoveIfCleanReturnsStatusErrWhenDirGone covers the runtime edge:
// if the worktree dir vanished between TaskFinished and cleanup, the
// status command fails and the result reports it without panicking.
func TestRemoveIfCleanReturnsStatusErrWhenDirGone(t *testing.T) {
	repo := initRepo(t)
	wm := &WorktreeManager{Repo: repo}
	dir, err := wm.Create("gone")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}

	r := wm.RemoveIfClean("gone", HarnessInjectedPaths)
	if r.StatusErr == nil {
		t.Errorf("expected StatusErr when worktree dir vanished, got %+v", r)
	}
}

// TestCreateWorktree_RecoversOrphanDir verifies the resume-after-restart case
// where a worktree dir is left on disk without a matching git registration
// (e.g., server restart while a worktree was active and runner cleanup never
// ran, then the user resumed). Without recovery, `git worktree add` fails
// with "already exists". Create must succeed and the branch's committed work
// must be preserved. Uncommitted state in the orphan dir is acceptable to
// lose because the user explicitly chose --resume and the branch is the
// authoritative source of preserved work.
func TestCreateWorktree_RecoversOrphanDir(t *testing.T) {
	repo := initRepo(t)
	wm := &WorktreeManager{Repo: repo}

	// First-run to create the branch.
	dir, err := wm.Create("rm-orphan-task")
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}

	// Add and commit a file on the branch so we can verify it's preserved.
	committedPath := filepath.Join(dir, "committed.txt")
	if err := os.WriteFile(committedPath, []byte("committed content\n"), 0o644); err != nil {
		t.Fatalf("write committed: %v", err)
	}
	cmd := exec.Command("git", "add", "committed.txt")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git add: %v (%s)", err, out)
	}
	cmd = exec.Command("git", "commit", "-m", "commit on branch")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git commit: %v (%s)", err, out)
	}

	// Cleanly remove the worktree (registration + dir).
	if err := wm.Remove("rm-orphan-task"); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	// Simulate orphan dir without registration: re-create the dir with
	// non-git junk so `git worktree repair` cannot recover it (no .git
	// pointer). This mimics the post-server-restart scenario where the dir
	// survived but git's metadata was lost.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir orphan: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stale.txt"), []byte("stale\n"), 0o644); err != nil {
		t.Fatalf("write stale: %v", err)
	}

	// Create must fall back to rm + add and succeed.
	got, err := wm.Create("rm-orphan-task")
	if err != nil {
		t.Fatalf("Create after rm-orphan: %v", err)
	}
	if got != dir {
		t.Fatalf("dir drift: %q vs %q", got, dir)
	}
	// Stale (uncommitted) content gone.
	if _, err := os.Stat(filepath.Join(dir, "stale.txt")); !os.IsNotExist(err) {
		t.Fatalf("stale.txt should have been removed, got err=%v", err)
	}
	// Committed content (from the branch) still there.
	if data, err := os.ReadFile(committedPath); err != nil {
		t.Fatalf("committed file missing on resumed branch: %v", err)
	} else if string(data) != "committed content\n" {
		t.Fatalf("committed file corrupted: %q", data)
	}
}

// TestCreateWorktree_RecoversStaleRegistration verifies the inverse orphan
// case: dir is gone from disk but git's `.git/worktrees/<id>` registration
// survives. Without `--expire=now` on prune, git's default 3-month prune
// expiry would let the registration survive, causing `git worktree add` to
// fail with "missing but already registered". This test simulates a recently
// killed session followed by --resume.
func TestCreateWorktree_RecoversStaleRegistration(t *testing.T) {
	repo := initRepo(t)
	wm := &WorktreeManager{Repo: repo}

	// First-run to create the branch + registration.
	dir, err := wm.Create("stale-reg-task")
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}

	// Simulate the dir being deleted out-of-band (e.g. user rm -rf, or a
	// half-completed cleanup) WITHOUT going through wm.Remove so the
	// `.git/worktrees/<id>` registration stays intact.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("rm dir: %v", err)
	}
	// Sanity: registration should still be there.
	regParent := filepath.Join(repo, ".git", "worktrees")
	entries, err := os.ReadDir(regParent)
	if err != nil || len(entries) == 0 {
		t.Fatalf("expected stale registration in %s, entries=%v err=%v", regParent, entries, err)
	}

	// Create must succeed: prune --expire=now clears the stale registration,
	// then `git worktree add` proceeds normally.
	got, err := wm.Create("stale-reg-task")
	if err != nil {
		t.Fatalf("Create with stale registration: %v", err)
	}
	if got != dir {
		t.Fatalf("dir drift: %q vs %q", got, dir)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("worktree dir missing after recovery: %v", err)
	}
}

// TestWorktreeManagerSerializesSameRepo verifies that concurrent Create/Remove
// calls on the same WorktreeManager do not corrupt the git worktree list.
// The -race flag + -count=10 catches any mutex regression.
func TestWorktreeManagerSerializesSameRepo(t *testing.T) {
	repo := initRepo(t)
	wm := &WorktreeManager{Repo: repo}

	const n = 5
	var wg sync.WaitGroup
	errs := make([]error, n)

	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			id := fmt.Sprintf("concurrent-%d", idx)
			dir, err := wm.Create(id)
			if err != nil {
				errs[idx] = fmt.Errorf("Create(%s): %w", id, err)
				return
			}
			if _, err := os.Stat(dir); err != nil {
				errs[idx] = fmt.Errorf("stat after Create(%s): %w", id, err)
				return
			}
			if err := wm.Remove(id); err != nil {
				errs[idx] = fmt.Errorf("Remove(%s): %w", id, err)
			}
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}
}
