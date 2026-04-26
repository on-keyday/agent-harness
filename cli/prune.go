package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// Prune asks the server to forget terminal tasks older than `before`.
// This used to also walk local worktrees; that step is now in PruneLocal.
func Prune(ctx context.Context, peerCID objproto.ConnectionID, before time.Duration, out io.Writer) error {
	cutoff := time.Now().Add(-before)
	removed, err := PruneTasks(ctx, peerCID, cutoff)
	if err != nil {
		return err
	}
	if removed > 0 {
		fmt.Fprintf(out, "server forgot %d task(s)\n", removed)
	}
	return nil
}

// PruneLocal walks <repo>/.harness-worktrees/ and `git worktree remove --force`
// the entries whose ModTime is older than `before`. No server interaction.
func PruneLocal(ctx context.Context, repo string, before time.Duration, out io.Writer) error {
	cutoff := time.Now().Add(-before)
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

// PruneTasks asks the server to forget terminal tasks whose EndedAt is before
// cutoff. Internal helper used by Prune; exposed for callers that want the
// raw count (e.g. tui).
func PruneTasks(ctx context.Context, peerCID objproto.ConnectionID, cutoff time.Time) (uint32, error) {
	c, err := Dial(ctx, peerCID)
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
