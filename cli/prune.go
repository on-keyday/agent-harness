package cli

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"time"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// FormatPruneCutoff renders a "before" duration as a human-readable cutoff
// description for prune UI: "7d ago (2026-04-19 21:30:42 +0900)" for positive
// durations, or "all (everything)" for zero/negative durations.
func FormatPruneCutoff(before time.Duration) string {
	if before <= 0 {
		return "all (everything)"
	}
	cutoff := time.Now().Add(-before)
	return fmt.Sprintf("%s ago (%s)", formatBefore(before), cutoff.Format("2006-01-02 15:04:05 -0700"))
}

// formatBefore prefers Nd / Nh / Nm units over time.Duration's "168h0m0s"
// rendering, falling back to the default for non-round inputs.
func formatBefore(d time.Duration) string {
	switch {
	case d%(24*time.Hour) == 0:
		return fmt.Sprintf("%dd", d/(24*time.Hour))
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	default:
		return d.String()
	}
}

// PruneResult is the breakdown returned by a server-side prune. For
// time-based prunes SkippedActive / SkippedMissing are always zero (the
// server only considers terminal tasks). For id-based prunes they tell the
// caller why a requested id was not pruned.
type PruneResult struct {
	Removed        uint32
	SkippedActive  uint32
	SkippedMissing uint32
}

// Prune asks the server to forget tasks. With taskIDs empty: terminal tasks
// older than `before` are removed (the original behavior). With taskIDs
// non-empty: only those ids are considered, `before` is ignored, and tasks
// that are still active are skipped unless force is true.
//
// This used to also walk local worktrees; that step is now in PruneLocal.
func (c *Client) Prune(ctx context.Context, before time.Duration, taskIDs []string, force bool, out io.Writer) error {
	if len(taskIDs) == 0 {
		cutoff := time.Now().Add(-before)
		fmt.Fprintf(out, "prune: cutoff = %s; asking server to forget terminal tasks\n", FormatPruneCutoff(before))
		res, err := c.PruneTasks(ctx, cutoff, nil, false)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "prune: server forgot %d task(s)\n", res.Removed)
		return nil
	}
	fmt.Fprintf(out, "prune: asking server to forget %d task id(s) (force=%t)\n", len(taskIDs), force)
	res, err := c.PruneTasks(ctx, time.Time{}, taskIDs, force)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "prune: server forgot %d, skipped %d (active=%d, missing=%d)\n",
		res.Removed, res.SkippedActive+res.SkippedMissing, res.SkippedActive, res.SkippedMissing)
	if res.SkippedActive > 0 && !force {
		fmt.Fprintln(out, "prune: pass --force to also drop active (Queued/Running/Detached) tasks")
	}
	return nil
}

// PruneTasks asks the server to forget tasks. If taskIDs is empty the server
// runs in time mode (terminal tasks with EndedAt < cutoff are removed). If
// taskIDs is non-empty the server runs in id mode (cutoff is ignored).
// Method form: callable on an existing *Client without re-dialing.
func (c *Client) PruneTasks(ctx context.Context, cutoff time.Time, taskIDs []string, force bool) (PruneResult, error) {
	pr := protocol.PruneTasksRequest{BeforeTs: uint64(cutoff.UnixNano())}
	if len(taskIDs) > 0 {
		ids := make([]protocol.TaskID, 0, len(taskIDs))
		for _, hexID := range taskIDs {
			raw, err := hex.DecodeString(hexID)
			if err != nil || len(raw) != 16 {
				return PruneResult{}, fmt.Errorf("invalid task id %q (need 32 hex chars)", hexID)
			}
			var tid protocol.TaskID
			copy(tid.Id[:], raw)
			ids = append(ids, tid)
		}
		if !pr.SetTaskIds(ids) {
			return PruneResult{}, fmt.Errorf("too many task ids: %d (max 65535)", len(ids))
		}
	}
	if force {
		pr.Force = 1
	}
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_PruneTasks}
	req.SetPrune(pr)
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return PruneResult{}, err
	}
	if resp.Kind != protocol.TaskControlKind_PruneTasks {
		return PruneResult{}, fmt.Errorf("unexpected response kind: %v", resp.Kind)
	}
	rp := resp.Prune()
	if rp == nil {
		return PruneResult{}, fmt.Errorf("empty prune response")
	}
	return PruneResult{
		Removed:        rp.Removed,
		SkippedActive:  rp.SkippedActive,
		SkippedMissing: rp.SkippedMissing,
	}, nil
}

// Prune (package-level) is a thin wrapper that opens a fresh Client per call.
// Suitable for short-lived CLI processes (harness-cli). Long-lived consumers
// should hold a *Client and call (*Client).Prune instead.
func Prune(ctx context.Context, peerCID objproto.ConnectionID, before time.Duration, taskIDs []string, force bool, out io.Writer) error {
	c, err := Dial(ctx, peerCID)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.Prune(ctx, before, taskIDs, force, out)
}

// PruneTasks (package-level) is a thin wrapper that opens a fresh Client per
// call. Suitable for short-lived CLI processes. Long-lived consumers should
// hold a *Client and call (*Client).PruneTasks instead.
func PruneTasks(ctx context.Context, peerCID objproto.ConnectionID, cutoff time.Time, taskIDs []string, force bool) (PruneResult, error) {
	c, err := Dial(ctx, peerCID)
	if err != nil {
		return PruneResult{}, err
	}
	defer c.Close()
	return c.PruneTasks(ctx, cutoff, taskIDs, force)
}
