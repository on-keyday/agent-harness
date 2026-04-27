package runner

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"sync"
)

// WorktreeManager creates and removes git worktrees for tasks under <Repo>/.harness-worktrees/<taskID>.
// All operations shell out to the `git` binary; assumes git ≥ 2.30 is on PATH.
//
// A per-repo mutex serializes concurrent `git worktree` operations on the same
// repository so that parallel tasks on the same repo do not corrupt the worktree
// list (git worktree is not concurrency-safe for add/remove on the same repo).
type WorktreeManager struct {
	Repo string // absolute path to the main repo (the runner's bound repo)
	mu   sync.Mutex
}

// Create creates a new worktree at <Repo>/.harness-worktrees/<taskID> on a fresh branch
// `harness/<taskID>` based on the current HEAD of the main repo. Returns the absolute path
// of the new worktree.
//
// Concurrent calls on the same WorktreeManager are serialized by an internal mutex.
func (wm *WorktreeManager) Create(taskID string) (string, error) {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	dir := filepath.Join(wm.Repo, ".harness-worktrees", taskID)
	branch := "harness/" + taskID
	cmd := exec.Command("git", "worktree", "add", "-b", branch, dir)
	cmd.Dir = wm.Repo
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("worktree add: %w (%s)", err, out)
	}
	return dir, nil
}

// Remove deletes a previously-created worktree. Uses --force to drop dirty changes.
// Safe to call on a non-existent worktree (returns the error message but doesn't panic).
//
// Concurrent calls on the same WorktreeManager are serialized by an internal mutex.
func (wm *WorktreeManager) Remove(taskID string) error {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	dir := filepath.Join(wm.Repo, ".harness-worktrees", taskID)
	cmd := exec.Command("git", "worktree", "remove", "--force", dir)
	cmd.Dir = wm.Repo
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("worktree remove: %w (%s)", err, out)
	}
	return nil
}
