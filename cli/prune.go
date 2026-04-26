package cli

import (
	"context"
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

// Prune asks the server to forget terminal tasks older than `before`.
// This used to also walk local worktrees; that step is now in PruneLocal.
func Prune(ctx context.Context, peerCID objproto.ConnectionID, before time.Duration, out io.Writer) error {
	cutoff := time.Now().Add(-before)
	fmt.Fprintf(out, "prune: cutoff = %s; asking server to forget terminal tasks\n", FormatPruneCutoff(before))
	removed, err := PruneTasks(ctx, peerCID, cutoff)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "prune: server forgot %d task(s)\n", removed)
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
