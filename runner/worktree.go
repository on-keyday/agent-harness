package runner

import (
	"fmt"
	"log/slog"
	"os"
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
// Resume-idempotent: if <dir> is already a registered worktree of this repo
// (because the previous run's wm.Remove failed or the runner crashed before
// cleanup), the existing dir is reused as-is — re-running `git worktree add`
// would fail with "already exists", and destructively wiping the dir would
// also drop any uncommitted work claude left behind. Reuse preserves that
// work across the resume.
//
// Reuse deliberately does NOT require HEAD to be on `harness/<taskID>`: the
// user sometimes checks out a different branch (or ends up detached, e.g. a
// session killed mid-rebase) inside the task worktree, and the session ends
// with that checkout in place. Requiring the branch to match turned every
// such resume into a wipe-and-fail: the dir was rm -rf'ed (destroying
// uncommitted work without the RemoveIfClean dirty-guard) while its live
// registration survived, so the same call's `git worktree add` died with
// "missing but already registered" and only a second attempt — after the
// wipe — succeeded (incident 2026-07-06, agent-runner-sisaku.log). HEAD is
// left wherever the user put it.
//
// Concurrent calls on the same WorktreeManager are serialized by an internal mutex.
func (wm *WorktreeManager) Create(taskID string) (string, error) {
	wm.mu.Lock()
	defer wm.mu.Unlock()
	dir := filepath.Join(wm.Repo, ".harness-worktrees", taskID)
	branch := "harness/" + taskID

	if wm.worktreeAttachedLocked(dir) {
		return dir, nil
	}

	// If a previous registration is stale (dir gone but `.git/worktrees/<id>`
	// still around), `git worktree add` would refuse with "missing but
	// already registered". `--expire=now` is required because the default
	// prune expiry (`gc.worktreePruneExpire`, 3 months by default) means a
	// recently-removed dir's registration would survive plain `prune`. The
	// per-repo mutex (wm.mu) ensures no concurrent task is racing us, so a
	// repo-wide prune is safe.
	pruneCmd := exec.Command("git", "worktree", "prune", "--expire=now")
	pruneCmd.Dir = wm.Repo
	_ = pruneCmd.Run()

	// Orphan-dir recovery: dir exists on disk but is not registered as a
	// worktree (e.g., server restarted while a worktree was active and the
	// runner cleanup never ran, or the registration was pruned but the dir
	// remained). `git worktree add` would fail with "already exists". Try
	// `git worktree repair` first — if the .git pointer is intact it will
	// re-establish registration without losing uncommitted work. If <dir>
	// still isn't an attached worktree after that, fall back to rm -rf so
	// the subsequent add can succeed (the user explicitly resumed so they
	// have accepted that uncommitted state in this dir is gone; committed
	// work is preserved on the branch). A second prune follows the rm: if a
	// live registration for <dir> survived (it wasn't prunable while the
	// dir existed, so the earlier prune skipped it), it just became stale
	// and would make the add below fail with "missing but already
	// registered".
	if _, err := os.Stat(dir); err == nil {
		repairCmd := exec.Command("git", "worktree", "repair", dir)
		repairCmd.Dir = wm.Repo
		_ = repairCmd.Run()
		if wm.worktreeAttachedLocked(dir) {
			return dir, nil
		}
		slog.Warn("worktree dir present but not attachable as a worktree; removing for re-add", "dir", dir, "branch", branch)
		if rmErr := os.RemoveAll(dir); rmErr != nil {
			return "", fmt.Errorf("worktree orphan dir removal: %w", rmErr)
		}
		pruneCmd := exec.Command("git", "worktree", "prune", "--expire=now")
		pruneCmd.Dir = wm.Repo
		_ = pruneCmd.Run()
	}

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

// worktreeAttachedLocked reports whether <dir> is currently a non-prunable
// worktree of wm.Repo, regardless of which branch (or detached commit) it
// has checked out. Caller must hold wm.mu.
func (wm *WorktreeManager) worktreeAttachedLocked(dir string) bool {
	cmd := exec.Command("git", "worktree", "list", "--porcelain")
	cmd.Dir = wm.Repo
	out, err := cmd.Output()
	if err != nil {
		return false
	}
	return worktreeAttachedFromPorcelain(out, dir)
}

// worktreeAttachedFromPorcelain parses `git worktree list --porcelain` and
// reports whether <dir> matches a non-prunable record. The checked-out ref
// is deliberately not part of the match — see Create's doc comment for why
// a branch requirement here caused resume-time data loss. Split out from
// worktreeAttachedLocked so tests can exercise the parser against
// handcrafted output (in particular, the Windows separator case documented
// below).
//
// Format: one block per worktree, blocks separated by a blank line. Each
// block starts with "worktree <abs-path>" and may include "branch
// refs/heads/<name>", "detached", and "prunable <reason>" lines.
//
// Cross-OS quirk: git emits worktree paths with forward slashes on every
// platform (e.g. `C:/repo/.harness-worktrees/abc` on Windows), while
// filepath.Join on Windows produces backslashes. Normalise both sides via
// `strings.ReplaceAll(..., "\\", "/")` so the resume-idempotent reuse path
// triggers on Windows runners — without this, the previous-run worktree
// was never recognised as already-registered and re-add would fail with
// "already exists". `filepath.ToSlash` is not used because it is OS-aware
// (no-op on Linux) and would silently regress on a Linux build that
// happens to inspect Windows-style paths in tests or fixtures.
func worktreeAttachedFromPorcelain(out []byte, dir string) bool {
	wantWT := slashPath(dir)
	var curWT string
	var curPrunable bool
	matches := func() bool {
		return curWT == wantWT && !curPrunable
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if line == "" {
			if matches() {
				return true
			}
			curWT, curPrunable = "", false
			continue
		}
		switch {
		case strings.HasPrefix(line, "worktree "):
			curWT = slashPath(strings.TrimPrefix(line, "worktree "))
		case line == "prunable" || strings.HasPrefix(line, "prunable "):
			curPrunable = true
		}
	}
	return matches()
}

// slashPath converts every backslash in p to a forward slash. Used to
// normalise paths for cross-OS comparison; see the comment on
// worktreeAttachedFromPorcelain.
func slashPath(p string) string { return strings.ReplaceAll(p, `\`, "/") }

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
	return wm.removeLocked(taskID)
}

func (wm *WorktreeManager) removeLocked(taskID string) error {
	dir := filepath.Join(wm.Repo, ".harness-worktrees", taskID)
	cmd := exec.Command("git", "worktree", "remove", "--force", dir)
	cmd.Dir = wm.Repo
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("worktree remove: %w (%s)", err, out)
	}
	return nil
}

// RemoveCleanResult describes what RemoveIfClean decided.
type RemoveCleanResult struct {
	Removed       bool     // true if the worktree was actually deleted
	DirtyPaths    []string // worktree-relative paths that prevented removal (empty when Removed)
	StatusErr     error    // error from `git status --porcelain` (treated as "skip removal")
}

// RemoveIfClean removes the worktree only when `git status --porcelain`
// inside it shows no entries outside `ignoredPaths`. Entries below an
// `ignoredPaths` entry that ends with "/" are treated as a directory match.
//
// Use this from runner task-cleanup paths where a runner crash or wm.Remove
// failure would otherwise destroy uncommitted in-flight work the user wants
// preserved across resume — RemoveIfClean leaves the dir alone (returning
// the dirty paths for logging) so the next OpenExec can re-attach via
// Create's resume-idempotent path.
//
// A non-nil StatusErr (e.g. dir vanished, git not on PATH) is treated as
// "skip removal" rather than escalated, since the cleanup path's caller
// has already sent TaskFinished and cannot meaningfully react.
//
// Concurrent calls on the same WorktreeManager are serialized by the same
// mutex as Create/Remove.
func (wm *WorktreeManager) RemoveIfClean(taskID string, ignoredPaths []string) RemoveCleanResult {
	wm.mu.Lock()
	defer wm.mu.Unlock()

	dir := filepath.Join(wm.Repo, ".harness-worktrees", taskID)
	// --untracked-files=all expands a wholly-untracked directory entry like
	// ".claude/" into its individual files, so prefix matching against
	// ignoredPaths (e.g. ".claude/skills/") catches them. Default ("normal")
	// would collapse the dir and our exclusion list would see only ".claude/".
	cmd := exec.Command("git", "status", "--porcelain", "--untracked-files=all", "-z")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return RemoveCleanResult{StatusErr: err}
	}

	dirty := filterDirtyPaths(out, ignoredPaths)
	if len(dirty) > 0 {
		return RemoveCleanResult{DirtyPaths: dirty}
	}
	if err := wm.removeLocked(taskID); err != nil {
		return RemoveCleanResult{StatusErr: err}
	}
	return RemoveCleanResult{Removed: true}
}

// filterDirtyPaths parses `git status --porcelain -z` output and returns
// the worktree-relative paths whose entries are NOT covered by ignoredPaths.
//
// Format note: `--porcelain -z` produces NUL-terminated records of the
// form "XY <path>" (XY = 2-byte status). Renames carry an extra
// NUL-terminated field for the original path; we read it and discard.
//
// ignoredPaths semantics: an entry that ends with "/" matches the prefix
// of any path under that directory; otherwise the match is exact. This
// lets the caller pass entries like ".claude/skills/" without enumerating
// every file beneath it.
func filterDirtyPaths(porcelainZ []byte, ignoredPaths []string) []string {
	var dirty []string
	records := strings.Split(string(porcelainZ), "\x00")
	for i := 0; i < len(records); i++ {
		rec := records[i]
		if len(rec) < 4 {
			// Either trailing empty record or malformed; skip.
			continue
		}
		status := rec[:2]
		path := rec[3:]
		// Rename entries (status starts with R or C) carry the source path
		// in the next NUL-terminated record. Consume it so it isn't parsed
		// as its own status entry.
		if status[0] == 'R' || status[0] == 'C' {
			i++
		}
		if pathIgnored(path, ignoredPaths) {
			continue
		}
		dirty = append(dirty, path)
	}
	return dirty
}

func pathIgnored(path string, ignoredPaths []string) bool {
	for _, ig := range ignoredPaths {
		if strings.HasSuffix(ig, "/") {
			if strings.HasPrefix(path, ig) {
				return true
			}
			continue
		}
		if path == ig {
			return true
		}
	}
	return false
}
