package cli

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/on-keyday/agent-harness/runner/protocol"
	"github.com/on-keyday/objtrsf/objproto"
)

// RestoreResult counts what a restore did, per id.
type RestoreResult struct {
	Restored       uint32
	AlreadyPresent uint32
	NotInWAL       uint32
}

// RestorableTask is one task a prune forgot, as the server reports it.
type RestorableTask struct {
	TaskID    string
	PrunedAt  time.Time
	CreatedAt time.Time
	RepoPath  string
	Prompt    string
}

// Restorable lists what a restore could put back, newest prune first.
//
// This is the half that makes `restore` usable at all: the verb takes ids, and
// the ids of forgotten tasks are in a file on the server host that no listing
// reads. An operator who did not write the id down before pruning -- which is
// everyone who pruned by accident -- had no way to learn it.
func (c *Client) Restorable(ctx context.Context) ([]RestorableTask, protocol.RestoreWALStatus, error) {
	rr := protocol.RestoreTasksRequest{ListOnly: 1}
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_RestoreTasks}
	req.SetRestoreTasks(rr)
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return nil, 0, err
	}
	body := resp.RestoreTasks()
	if body == nil {
		return nil, 0, fmt.Errorf("empty restore response")
	}
	out := make([]RestorableTask, 0, len(body.Candidates))
	for i := range body.Candidates {
		cd := &body.Candidates[i]
		out = append(out, RestorableTask{
			TaskID:    hex.EncodeToString(cd.TaskId.Id[:]),
			PrunedAt:  time.Unix(0, int64(cd.PrunedAt)),
			CreatedAt: time.Unix(0, int64(cd.CreatedAt)),
			RepoPath:  string(cd.RepoPath),
			Prompt:    string(cd.Prompt),
		})
	}
	return out, body.WalStatus, nil
}

// WriteRestorable renders the listing.
//
// An empty listing says WHICH of the four it is. They looked identical --
// "nothing to put back" -- and three of them are failures that measured
// nothing: the only record of two was a line in the server's log, which the
// operator asking the question cannot see. Zero is a measurement; absence is
// not.
func WriteRestorable(rows []RestorableTask, status protocol.RestoreWALStatus, out io.Writer) {
	if len(rows) == 0 {
		switch status {
		case protocol.RestoreWALStatus_NoDataDir:
			fmt.Fprintln(out, "restore: this server runs without --data-dir, so it writes no WAL — "+
				"nothing it ever pruned can be put back, and nothing here is a measurement of what was pruned")
		case protocol.RestoreWALStatus_Missing:
			fmt.Fprintln(out, "restore: the server's events.log is not there (never written, or removed) — "+
				"this is not \"no prunes\": it is no history to read")
		case protocol.RestoreWALStatus_Unreadable:
			fmt.Fprintln(out, "restore: the server could not parse its WAL; the reason is in the server's log "+
				"(`restorable: WAL read failed`). One bad line makes the whole file unreadable, so this "+
				"says nothing about what was pruned")
		default:
			fmt.Fprintln(out, "restore: nothing to put back — the WAL was read, and no prune in it is still standing")
		}
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "TASK ID\tPRUNED\tCREATED\tREPO\tPROMPT")
	for _, r := range rows {
		prompt := strings.ReplaceAll(r.Prompt, "\n", " ")
		if len(prompt) > 48 {
			prompt = prompt[:48] + "…"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.TaskID,
			r.PrunedAt.Format("2006-01-02 15:04:05"), r.CreatedAt.Format("2006-01-02 15:04"),
			r.RepoPath, prompt)
	}
	tw.Flush() //nolint:errcheck
}

// RestoreTasks puts back task records a prune forgot, rebuilt from the
// server's WAL. Requires the `prune` capability and the same scope: what a
// caller could forget, it can un-forget.
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

// RestoreWith renders a restore through an existing client. With no ids it
// LISTS what could be put back, which is the only way to learn them.
func RestoreWith(ctx context.Context, c *Client, taskIDs []string, out io.Writer) error {
	if len(taskIDs) == 0 {
		rows, status, err := c.Restorable(ctx)
		if err != nil {
			return err
		}
		WriteRestorable(rows, status, out)
		return nil
	}
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
