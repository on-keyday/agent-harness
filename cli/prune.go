package cli

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

// Prune removes harness worktrees in <repo>/.harness-worktrees/ that haven't been
// modified within `before`. Operates locally — does NOT contact the server.
func Prune(repo string, before time.Duration, out io.Writer) error {
	dir := filepath.Join(repo, ".harness-worktrees")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	cutoff := time.Now().Add(-before)
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue
		}

		path := filepath.Join(dir, e.Name())
		cmd := exec.Command("git", "worktree", "remove", "--force", path)
		cmd.Dir = repo
		if out2, cerr := cmd.CombinedOutput(); cerr != nil {
			fmt.Fprintf(out, "skip %s: %s\n", e.Name(), out2)
			continue
		}
		fmt.Fprintf(out, "removed %s\n", e.Name())
	}
	return nil
}
