//go:build !js

package cli

import (
	"bytes"
	"context"
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
	if err := PruneLocal(context.Background(), repo, 7*24*time.Hour, nil, &out); err != nil {
		t.Fatalf("PruneLocal: %v", err)
	}
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Errorf("worktree should be removed, got err=%v\nlog: %s", err, out.String())
	}
}

func TestPruneNoDir(t *testing.T) {
	var out bytes.Buffer
	err := PruneLocal(context.Background(), t.TempDir(), 7*24*time.Hour, nil, &out)
	if err != nil {
		t.Fatalf("PruneLocal on empty repo should not error: %v", err)
	}
}

// TestPruneByTaskID verifies the task-id-targeted branch:
//   - listed task ids are removed regardless of mtime
//   - unlisted worktrees are left alone
//   - non-existent task ids report missing but don't error
func TestPruneByTaskID(t *testing.T) {
	repo := t.TempDir()
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

	targetID := "deadbeefdeadbeefdeadbeefdeadbeef"
	keepID := "cafebabecafebabecafebabecafebabe"
	targetDir := filepath.Join(repo, ".harness-worktrees", targetID)
	keepDir := filepath.Join(repo, ".harness-worktrees", keepID)
	run("worktree", "add", "-b", "harness/"+targetID, targetDir)
	run("worktree", "add", "-b", "harness/"+keepID, keepDir)

	// Both worktrees are recent — time-based prune (--before=24h) would
	// keep both. Targeted prune should remove only targetID.
	var out bytes.Buffer
	err := PruneLocal(context.Background(), repo, 24*time.Hour,
		[]string{targetID, "nonexistent00000000000000000000ff"}, &out)
	if err != nil {
		t.Fatalf("PruneLocal: %v", err)
	}
	if _, err := os.Stat(targetDir); !os.IsNotExist(err) {
		t.Errorf("target worktree should be removed, got err=%v\nlog: %s", err, out.String())
	}
	if _, err := os.Stat(keepDir); err != nil {
		t.Errorf("keep worktree should still exist, got err=%v\nlog: %s", err, out.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("removed "+targetID)) {
		t.Errorf("expected 'removed %s' in log, got: %s", targetID, out.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("no worktree")) {
		t.Errorf("expected missing-worktree skip in log, got: %s", out.String())
	}
}
