package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestPruneRemovesOldWorktrees(t *testing.T) {
	repo := t.TempDir()
	// init a git repo with one commit
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
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
	os.WriteFile(filepath.Join(repo, "README"), []byte("x\n"), 0o644)
	run("add", "README")
	run("commit", "-m", "init")

	// Create a worktree using git directly (so it's a real worktree)
	wtDir := filepath.Join(repo, ".harness-worktrees", "old-task")
	run("worktree", "add", "-b", "harness/old-task", wtDir)

	// Make the directory look old
	oldTime := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(wtDir, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := Prune(repo, 7*24*time.Hour, &out); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Errorf("worktree should be removed, got err=%v\nlog: %s", err, out.String())
	}
}

func TestPruneNoDir(t *testing.T) {
	var out bytes.Buffer
	err := Prune(t.TempDir(), 7*24*time.Hour, &out)
	if err != nil {
		t.Fatalf("Prune on empty repo should not error: %v", err)
	}
}
