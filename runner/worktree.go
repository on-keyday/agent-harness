package runner

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
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
// Resume-idempotent: if <dir> is already a registered worktree for
// `harness/<taskID>` (because the previous run's wm.Remove failed or the
// runner crashed before cleanup), the existing dir is reused as-is —
// re-running `git worktree add` would fail with "already exists", and
// destructively wiping the dir would also drop any uncommitted work claude
// left behind. Reuse preserves that work across the resume.
//
// Concurrent calls on the same WorktreeManager are serialized by an internal mutex.
func (wm *WorktreeManager) Create(taskID string) (string, error) {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	dir := filepath.Join(wm.Repo, ".harness-worktrees", taskID)
	branch := "harness/" + taskID

	if wm.worktreeRegisteredLocked(dir, branch) {
		return dir, nil
	}

	// If a previous registration is stale (dir gone but `.git/worktrees/<id>`
	// still around), `git worktree add` would refuse with "missing but
	// already registered". Prune is a no-op when nothing is stale.
	pruneCmd := exec.Command("git", "worktree", "prune")
	pruneCmd.Dir = wm.Repo
	_ = pruneCmd.Run()

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

// worktreeRegisteredLocked reports whether <dir> is currently a non-prunable
// worktree of wm.Repo checked out at refs/heads/<branch>. Caller must hold
// wm.mu.
//
// Parses `git worktree list --porcelain`, whose record format is one block
// per worktree (blocks separated by a blank line); each block starts with
// "worktree <abs-path>" and may include "branch refs/heads/<name>",
// "detached", and "prunable <reason>" lines.
func (wm *WorktreeManager) worktreeRegisteredLocked(dir, branch string) bool {
	cmd := exec.Command("git", "worktree", "list", "--porcelain")
	cmd.Dir = wm.Repo
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	wantRef := "refs/heads/" + branch
	var curWT, curBranch string
	var curPrunable bool
	matches := func() bool {
		return curWT == dir && curBranch == wantRef && !curPrunable
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if line == "" {
			if matches() {
				return true
			}
			curWT, curBranch, curPrunable = "", "", false
			continue
		}
		switch {
		case strings.HasPrefix(line, "worktree "):
			curWT = strings.TrimPrefix(line, "worktree ")
		case strings.HasPrefix(line, "branch "):
			curBranch = strings.TrimPrefix(line, "branch ")
		case line == "prunable" || strings.HasPrefix(line, "prunable "):
			curPrunable = true
		}
	}
	return matches()
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
