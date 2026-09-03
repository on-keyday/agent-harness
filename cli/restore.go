package cli

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// RestoreResult counts what a restore did, per id.
type RestoreResult struct {
	Restored       uint32
	AlreadyPresent uint32
	NotInWAL       uint32
}

// RestoreTasks puts back task records a prune forgot, rebuilt from the
// server's WAL. Operator-only.
//
// The ids are required. There is no sweep-back form: the WAL holds every task
// the server has ever seen, and restoring "everything" would resurrect years
// of them. That asymmetry with prune is deliberate -- the destructive verb had
// a bare form and this is what it cost.
func (c *Client) RestoreTasks(ctx context.Context, taskIDs []string) (RestoreResult, error) {
	if len(taskIDs) == 0 {
		return RestoreResult{}, fmt.Errorf("restore: at least one task id")
	}
	ids := make([]protocol.TaskID, 0, len(taskIDs))
	for _, hexID := range taskIDs {
		raw, err := hex.DecodeString(hexID)
		if err != nil || len(raw) != 16 {
			return RestoreResult{}, fmt.Errorf("invalid task id %q (need 32 hex chars)", hexID)
		}
		var tid protocol.TaskID
		copy(tid.Id[:], raw)
		ids = append(ids, tid)
	}
	rr := protocol.RestoreTasksRequest{}
	if !rr.SetTaskIds(ids) {
		return RestoreResult{}, fmt.Errorf("too many task ids: %d (max 65535)", len(ids))
	}
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_RestoreTasks}
	req.SetRestoreTasks(rr)
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return RestoreResult{}, err
	}
	if resp.Kind != protocol.TaskControlKind_RestoreTasks {
		return RestoreResult{}, fmt.Errorf("unexpected response kind: %v", resp.Kind)
	}
	body := resp.RestoreTasks()
	if body == nil {
		return RestoreResult{}, fmt.Errorf("empty restore response")
	}
	return RestoreResult{
		Restored: body.Restored, AlreadyPresent: body.AlreadyPresent, NotInWAL: body.NotInWal,
	}, nil
}

// RestoreWith renders a restore through an existing client.
func RestoreWith(ctx context.Context, c *Client, taskIDs []string, out io.Writer) error {
	res, err := c.RestoreTasks(ctx, taskIDs)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "restore: %d restored, %d already present, %d not in the WAL\n",
		res.Restored, res.AlreadyPresent, res.NotInWAL)
	if res.NotInWAL > 0 {
		fmt.Fprintln(out, "restore: an id with no task_created cannot be rebuilt — nothing invents a task the server never wrote")
	}
	if res.Restored > 0 {
		fmt.Fprintln(out, "restore: the RECORD is back; the task log is not (prune removed the file) and the worktree was never touched")
	}
	return nil
}

// Restore opens a fresh Client per call, for short-lived CLI processes.
func Restore(ctx context.Context, peerCID objproto.ConnectionID, taskIDs []string, out io.Writer) error {
	c, err := Dial(ctx, peerCID, protocol.ClientKind_Cli)
	if err != nil {
		return err
	}
	defer c.Close()
	return RestoreWith(ctx, c, taskIDs, out)
}
