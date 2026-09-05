//go:build !js

package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/on-keyday/agent-harness/cli"
	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// classifyForLocalPrune dials the server, snapshots the task list, and
// returns the subset of taskIDs that are safe to remove locally. A task is
// safe when its server status is terminal (Succeeded/Failed/Cancelled) or
// when it is no longer in the snapshot at all (pruned/typo). Tasks the
// server still considers active (Queued/Running/Detached) are skipped with
// a warning unless force is set.
func classifyForLocalPrune(ctx context.Context, peerCID objproto.ConnectionID, taskIDs []string, force bool, out io.Writer) ([]string, error) {
	c, err := cli.Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	snap, err := c.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	statusByID := make(map[string]protocol.TaskStatus, len(snap.Tasks))
	for i := range snap.Tasks {
		statusByID[hex.EncodeToString(snap.Tasks[i].Id.Id[:])] = snap.Tasks[i].Status
	}
	safe := make([]string, 0, len(taskIDs))
	for _, id := range taskIDs {
		st, known := statusByID[id]
		if !known {
			safe = append(safe, id)
			continue
		}
		switch st {
		case protocol.TaskStatus_Succeeded,
			protocol.TaskStatus_Failed,
			protocol.TaskStatus_Cancelled:
			safe = append(safe, id)
		default:
			if force {
				fmt.Fprintf(out, "force-removing %s (status=%s on server)\n", id, st.String())
				safe = append(safe, id)
			} else {
				fmt.Fprintf(out, "skip %s: still active on server (status=%s); pass --force to override\n", id, st.String())
			}
		}
	}
	return safe, nil
}
