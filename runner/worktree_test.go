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

// TestWorktreeManagerSerializesSameRepo verifies that concurrent Create/Remove
// calls on the same WorktreeManager do not corrupt the git worktree list.
// The -race flag + -count=10 catches any mutex regression.
func TestWorktreeManagerSerializesSameRepo(t *testing.T) {
	repo := initRepo(t)
	wm := &WorktreeManager{Repo: repo}

	const n = 5
	var wg sync.WaitGroup
	errs := make([]error, n)

	for i := 0; i < n; i++ {
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
