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

// PruneLocal walks <repo>/.harness-worktrees/ and `git worktree remove --force`
// the entries whose ModTime is older than `before`. No server interaction.
//
// Native-only: requires os/exec to drive the git binary. The wasm build
// excludes this file via build tag; the browser UI does not expose
// prune-local functionality.
func PruneLocal(ctx context.Context, repo string, before time.Duration, out io.Writer) error {
	cutoff := time.Now().Add(-before)
	dir := filepath.Join(repo, ".harness-worktrees")
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
