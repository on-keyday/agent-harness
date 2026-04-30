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

// Create creates (or re-attaches) a worktree at <Repo>/.harness-worktrees/<taskID>.
//
// First-run: branch `harness/<taskID>` does not exist → `git worktree add -b
// harness/<taskID> <dir>` creates it from the current HEAD.
//
// Resume: branch `harness/<taskID>` already exists from a previous run (the
// runner intentionally retains it after Remove so the work is reachable) →
// `git worktree add <dir> harness/<taskID>` attaches a new worktree to that
// existing branch. The dir path is identical to the previous run, which is
// what makes claude's project key (~/.claude/projects/<cwd-hash>/) match —
// the user can then `--resume <session-uuid>` and have claude find its
// stored conversation.
//
// Concurrent calls on the same WorktreeManager are serialized by an internal mutex.
func (wm *WorktreeManager) Create(taskID string) (string, error) {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	dir := filepath.Join(wm.Repo, ".harness-worktrees", taskID)
	branch := "harness/" + taskID

	args := []string{"worktree", "add", "-b", branch, dir}
	if wm.branchExistsLocked(branch) {
		// Existing branch — drop -b so git attaches to the ref instead of
		// trying to create a new one (which would fail with "already exists").
		args = []string{"worktree", "add", dir, branch}
	}
	cmd := exec.Command("git", args...)
	cmd.Dir = wm.Repo
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("worktree add: %w (%s)", err, out)
	}
	return dir, nil
}

// branchExistsLocked reports whether the given branch ref exists in wm.Repo.
// Caller must hold wm.mu — this is only invoked from Create which already
// owns the mutex.
func (wm *WorktreeManager) branchExistsLocked(branch string) bool {
	cmd := exec.Command("git", "show-ref", "--verify", "--quiet", "refs/heads/"+branch)
	cmd.Dir = wm.Repo
	return cmd.Run() == nil
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
