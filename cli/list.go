package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/on-keyday/agent-harness/objproto"
	"github.com/on-keyday/agent-harness/runner/protocol"
)

// List queries the server for all runners + recent tasks and writes a human-
// readable summary to out. Method form: callable repeatedly without re-dialing.
func (c *Client) List(ctx context.Context, out io.Writer) error {
	req := &protocol.TaskControlRequest{Kind: protocol.TaskControlKind_List}
	req.SetList(protocol.ListQuery{Query: nil})
	resp, err := c.RoundTripTaskControl(ctx, req)
	if err != nil {
		return err
	}
	lr := resp.List()
	if lr == nil {
		return fmt.Errorf("empty list response")
	}

	fmt.Fprintln(out, "RUNNERS")
	if len(lr.Runners) == 0 {
		fmt.Fprintln(out, "  (none)")
	}
	for _, r := range lr.Runners {
		fmt.Fprintf(out, "  %s  repo=%s  current=%s\n",
			runnerStatusStr(r.Status),
			string(r.RepoPath),
			shortHex(r.CurrentTask.Id[:]),
		)
	}

	fmt.Fprintln(out, "TASKS")
	if len(lr.Tasks) == 0 {
		fmt.Fprintln(out, "  (none)")
	}
	for _, t := range lr.Tasks {
		fmt.Fprintf(out, "  %s  %s  repo=%s  prompt=%q\n",
			shortHex(t.Id.Id[:]),
			taskStatusStr(t.Status),
			string(t.RepoPath),
			string(t.Prompt),
		)
	}
	return nil
}

// List (package-level) is a thin wrapper that opens a fresh Client per call.
// Suitable for short-lived CLI processes (harness-cli). Long-lived consumers
// should hold a *Client and call (*Client).List instead.
func List(ctx context.Context, peerCID objproto.ConnectionID, out io.Writer) error {
	c, err := Dial(ctx, peerCID)
	if err != nil {
		return err
	}
	defer c.Close()
	return c.List(ctx, out)
}

func runnerStatusStr(s protocol.RunnerStatus) string {
	switch s {
	case protocol.RunnerStatus_Idle:
		return "Idle   "
	case protocol.RunnerStatus_Busy:
		return "Busy   "
	default:
		return "Offline"
	}
}

func taskStatusStr(s protocol.TaskStatus) string {
	switch s {
	case protocol.TaskStatus_Queued:
		return "Queued   "
	case protocol.TaskStatus_Running:
		return "Running  "
	case protocol.TaskStatus_Succeeded:
		return "Succeeded"
	case protocol.TaskStatus_Failed:
		return "Failed   "
	case protocol.TaskStatus_Cancelled:
		return "Cancelled"
	}
	return "?"
}

// shortHex returns a 12-char hex prefix; if the slice is all zero, returns "-".
func shortHex(b []byte) string {
	allZero := true
	for _, v := range b {
		if v != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		return "-"
	}
	const tab = "0123456789abcdef"
	out := make([]byte, 0, 12)
	for i := 0; i < 6 && i < len(b); i++ {
		out = append(out, tab[b[i]>>4], tab[b[i]&0xf])
	}
	return string(out)
}
