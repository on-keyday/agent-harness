//go:build !js

package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// PruneLocal removes worktrees under <repo>/.harness-worktrees/ via
// `git worktree remove --force`. Two modes:
//
//   - taskIDs == nil: scan the whole directory and remove entries whose
//     ModTime is older than `before`. The original time-based behavior.
//   - taskIDs != nil: remove ONLY the listed task ids (resolved as the dir
//     name <repo>/.harness-worktrees/<task-id>/). `before` is ignored.
//     Caller is expected to have already gated against tasks that the
//     server considers active.
//
// Native-only: requires os/exec to drive the git binary. The wasm build
// excludes this file via build tag; the browser UI does not expose
// prune-local functionality.
func PruneLocal(ctx context.Context, repo string, before time.Duration, taskIDs []string, out io.Writer) error {
	dir := filepath.Join(repo, ".harness-worktrees")

	if taskIDs != nil {
		fmt.Fprintf(out, "prune-local: removing %d task id(s) under %s\n", len(taskIDs), dir)
		var removed, missing, skippedError int
		for _, id := range taskIDs {
			path := filepath.Join(dir, id)
			if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
				fmt.Fprintf(out, "skip %s: no worktree at %s\n", id, path)
				missing++
				continue
			}
			cmd := exec.Command("git", "worktree", "remove", "--force", path)
			cmd.Dir = repo
			if out2, cerr := cmd.CombinedOutput(); cerr != nil {
				fmt.Fprintf(out, "skip %s: %s\n", id, out2)
				skippedError++
				continue
			}
			fmt.Fprintf(out, "removed %s\n", id)
			removed++
		}
		fmt.Fprintf(out, "prune-local: removed %d, skipped %d (missing=%d, error=%d)\n",
			removed, missing+skippedError, missing, skippedError)
		return nil
	}

	cutoff := time.Now().Add(-before)
	fmt.Fprintf(out, "prune-local: cutoff = %s; scanning %s\n", FormatPruneCutoff(before), dir)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		fmt.Fprintf(out, "prune-local: no worktrees directory; nothing to do\n")
		return nil
	}
	if err != nil {
		return err
	}
	var removed, skippedNewer, skippedError int
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			skippedNewer++
			continue
		}

		path := filepath.Join(dir, e.Name())
		cmd := exec.Command("git", "worktree", "remove", "--force", path)
		cmd.Dir = repo
		if out2, cerr := cmd.CombinedOutput(); cerr != nil {
			fmt.Fprintf(out, "skip %s: %s\n", e.Name(), out2)
			skippedError++
			continue
		}
		fmt.Fprintf(out, "removed %s\n", e.Name())
		removed++
	}
	fmt.Fprintf(out, "prune-local: removed %d, skipped %d (newer=%d, error=%d)\n",
		removed, skippedNewer+skippedError, skippedNewer, skippedError)
	return nil
}
