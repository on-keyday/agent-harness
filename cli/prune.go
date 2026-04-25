package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Prune removes old harness worktrees in <repo>/.harness-worktrees/ AND asks the server
// to forget terminal tasks (and their log files) older than `before`. Both steps run on
// each call: the worktree pass walks the local filesystem; the server pass speaks
// TaskControl. If `addr` is empty, the server step is skipped. If the server is
// unreachable, the local pass still runs and a warning is printed to out.
func Prune(ctx context.Context, addr, repo string, before time.Duration, out io.Writer) error {
	cutoff := time.Now().Add(-before)

	// Step 1: ask the server to forget terminal tasks older than cutoff.
	// We do this BEFORE the worktree sweep so a freshly-pruned task's
	// worktree (still on disk) still appears for cleanup below.
	if addr != "" {
		if removed, err := PruneTasks(ctx, addr, cutoff); err != nil {
			fmt.Fprintf(out, "warning: server prune skipped: %v\n", err)
		} else if removed > 0 {
			fmt.Fprintf(out, "server forgot %d task(s)\n", removed)
		}
	}

	// Step 2: walk local worktrees and `git worktree remove --force` the old ones.
	dir := filepath.Join(repo, ".harness-worktrees")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
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

// PruneTasks asks the server to forget terminal tasks whose EndedAt is before cutoff.
// Returns the count of tasks removed by the server. The server also deletes the
// per-task log files at <data-dir>/logs/<id>.log.
func PruneTasks(ctx context.Context, addr string, cutoff time.Time) (uint32, error) {
	c, err := Dial(ctx, addr)
	if err != nil {
		return 0, err
	}
	defer c.Close()

	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_PruneTasks}
	req.SetPrune(protocol.PruneTasksRequest{BeforeTs: uint64(cutoff.UnixNano())})
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return 0, err
	}
	if resp.Kind != protocol.TaskControlKind_PruneTasks {
		return 0, fmt.Errorf("unexpected response kind: %v", resp.Kind)
	}
	pr := resp.Prune()
	if pr == nil {
		return 0, fmt.Errorf("empty prune response")
	}
	return pr.Removed, nil
}
